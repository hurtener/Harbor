// cmd/harbor/topology_synthesise.go — Phase 70 (D-102): the
// trajectory-synthesised topology builder.
//
// `BuildTopologyFromEvents` walks a sequence of wire-event frames (the
// flat JSON shape `internal/protocol/transports/stream` emits) and
// produces a `Topology` value the renderer consumes.
//
// # Why parse wire-shape JSON instead of internal events.Event
//
// The CLI is a Protocol client (CLAUDE.md §8 + RFC §7) — it consumes
// the canonical wire surface, not the in-process events.Event struct.
// The wire shape is intentionally flat (sequence, run, tenant, user,
// session, type, payload as generic JSON) so a future third-party
// Console implementation can synthesise the same topology from the
// same SSE stream without importing anything from `internal/`.
//
// # Inference rules
//
// The synthesiser applies these rules in observation order:
//
//   - `tool.invoked` → push a TopologyNodeTool with Status=Pending; key
//     it by ToolName so paired terminal events can match.
//   - `tool.completed` → look up the matching pending tool node by
//     ToolName (last unmatched); upgrade its Status to OK + duration.
//   - `tool.failed` → ditto; upgrade Status to Fail + error class.
//   - `tool.invalid_args` → if a pending tool exists with the same
//     name, decorate it; otherwise emit a standalone tool node with
//     Status=Fail (the validation failure pre-dated the invoke event,
//     so there is no pending node to upgrade).
//   - `task.spawned` → push a TopologyNodeTask at depth 0.
//   - `pause.requested` → push a TopologyNodePause at depth 1 IF the
//     preceding visible node is a tool/task (it's a child of the
//     active operation); else depth 0.
//   - `tool.approval_requested` → push a TopologyNodeApproval at depth
//     of (last open tool node's depth + 1).
//   - `tool.auth_required` → ditto, kind=Auth.
//   - `planner.finish` → push a TopologyNodeFinish at depth 0; always
//     the last visible node when present.
//
// Identity is read from the wire-event header (`tenant` / `user` /
// `session` / `run`) and copied onto the Topology struct so the
// rendered header carries it.
//
// # Determinism contract
//
// Two invocations of `BuildTopologyFromEvents` with the same input
// slice produce byte-identical Topology values. The synthesiser does
// not consult the clock, does not read goroutine-local state, and
// does not iterate maps in unsorted order — the pending-tools index
// is a slice append/pop pattern, not a map iteration. The renderer
// applies its own sort defensively (see Render).

package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WireEventFrame is the JSON shape `internal/protocol/transports/stream`
// emits in the SSE `data:` line. Mirrors `stream.wireEvent` byte-for-byte
// (the field tags match) — we re-declare it here rather than imp the
// internal type because cmd/harbor is intentionally a Protocol client
// (CLAUDE.md §8 — the wire shape is the contract, not the Go struct).
type WireEventFrame struct {
	Type       string                 `json:"type"`
	Sequence   uint64                 `json:"sequence"`
	OccurredAt string                 `json:"occurred_at"`
	Tenant     string                 `json:"tenant"`
	User       string                 `json:"user"`
	Session    string                 `json:"session"`
	Run        string                 `json:"run,omitempty"`
	Payload    map[string]interface{} `json:"payload,omitempty"`
	Extra      map[string]string      `json:"extra,omitempty"`
}

// BuildTopologyFromEvents synthesises a Topology from a list of wire
// frames filtered to a single run (caller's responsibility — the
// synthesiser does not re-filter). The resulting Topology carries the
// identity triple from the first frame (or empty if frames is empty),
// and the source mode is always `"events.synthesised"` for the V1 path.
//
// The function does not mutate frames. It returns a zero Topology
// (with RunID = runID, empty Nodes) when frames is empty — the caller
// can still Render that to produce a `(no events)` body.
//
// runID is the caller-supplied filter target; it overrides whatever
// Run field is present on the frames (which may be empty for older
// events that pre-date the X-Harbor-Run header convention). The
// renderer's header always shows the caller-supplied runID so an
// operator who asks for a specific run sees that exact ID back.
func BuildTopologyFromEvents(runID string, frames []WireEventFrame) Topology {
	t := Topology{
		RunID:      runID,
		SourceMode: "events.synthesised",
	}
	if len(frames) == 0 {
		return t
	}
	// Identity triple from the first frame. The bus is identity-scoped
	// per CLAUDE.md §6 rule 5 + Phase 60 SSE filter: every frame for
	// a run shares the (tenant, user, session) triple; we pick frame 0
	// rather than walk the whole slice for the trivial case.
	t.Tenant = frames[0].Tenant
	t.User = frames[0].User
	t.Session = frames[0].Session

	// pendingTools maps tool name → index in t.Nodes for tool nodes
	// awaiting a terminal-shape event. A slice-as-deque would also
	// work but the map keyed by name is what the inference rules
	// actually want (last-unmatched-by-name).
	pendingTools := make(map[string][]int)
	// lastOpenToolIdx tracks the index of the most-recently-pushed
	// tool node for depth inference of approval / auth children.
	lastOpenToolIdx := -1

	for _, f := range frames {
		switch f.Type {
		case "tool.invoked":
			name := stringField(f.Payload, "ToolName")
			node := TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodeTool,
				Label:    name,
				Status:   TopologyStatusPending,
				Depth:    0,
			}
			t.Nodes = append(t.Nodes, node)
			idx := len(t.Nodes) - 1
			pendingTools[name] = append(pendingTools[name], idx)
			lastOpenToolIdx = idx
		case "tool.completed":
			name := stringField(f.Payload, "ToolName")
			if idx, ok := popPending(pendingTools, name); ok {
				t.Nodes[idx].Status = TopologyStatusOK
				if durMs, found := numericField(f.Payload, "DurationMS"); found {
					t.Nodes[idx].Detail = fmt.Sprintf("%dms", int64(durMs))
				}
			}
		case "tool.failed":
			name := stringField(f.Payload, "ToolName")
			if idx, ok := popPending(pendingTools, name); ok {
				t.Nodes[idx].Status = TopologyStatusFail
				if cls := stringField(f.Payload, "ErrorClass"); cls != "" {
					t.Nodes[idx].Detail = "class=" + cls
				}
			}
		case "tool.invalid_args":
			name := stringField(f.Payload, "ToolName")
			if idx, ok := popPending(pendingTools, name); ok {
				t.Nodes[idx].Status = TopologyStatusFail
				t.Nodes[idx].Detail = "invalid_args"
			} else {
				t.Nodes = append(t.Nodes, TopologyNode{
					Sequence: f.Sequence,
					Kind:     TopologyNodeTool,
					Label:    name,
					Status:   TopologyStatusFail,
					Detail:   "invalid_args",
				})
			}
		case "tool.approval_requested":
			depth := 1
			if lastOpenToolIdx >= 0 {
				depth = t.Nodes[lastOpenToolIdx].Depth + 1
			}
			label := stringField(f.Payload, "ToolName")
			if label == "" {
				label = "approval"
			}
			detail := ""
			if reason := stringField(f.Payload, "Reason"); reason != "" {
				detail = "reason=" + reason
			}
			t.Nodes = append(t.Nodes, TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodeApproval,
				Label:    label,
				Detail:   detail,
				Depth:    depth,
			})
		case "tool.auth_required":
			depth := 1
			if lastOpenToolIdx >= 0 {
				depth = t.Nodes[lastOpenToolIdx].Depth + 1
			}
			label := stringField(f.Payload, "Source")
			if label == "" {
				label = "oauth"
			}
			detail := ""
			if scope := stringField(f.Payload, "BindingScope"); scope != "" {
				detail = "scope=" + scope
			}
			t.Nodes = append(t.Nodes, TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodeAuth,
				Label:    label,
				Detail:   detail,
				Depth:    depth,
			})
		case "task.spawned":
			label := stringField(f.Payload, "TaskID")
			if label == "" {
				label = "task"
			}
			detail := stringField(f.Payload, "Kind")
			t.Nodes = append(t.Nodes, TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodeTask,
				Label:    label,
				Detail:   detail,
				Depth:    0,
			})
		case "pause.requested":
			depth := 0
			if lastOpenToolIdx >= 0 {
				depth = t.Nodes[lastOpenToolIdx].Depth + 1
			}
			label := stringField(f.Payload, "Reason")
			if label == "" {
				label = "paused"
			}
			detail := stringField(f.Payload, "Token")
			t.Nodes = append(t.Nodes, TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodePause,
				Label:    label,
				Detail:   detail,
				Depth:    depth,
			})
		case "planner.finish":
			label := stringField(f.Payload, "Reason")
			if label == "" {
				label = "finished"
			}
			t.Nodes = append(t.Nodes, TopologyNode{
				Sequence: f.Sequence,
				Kind:     TopologyNodeFinish,
				Label:    label,
				Depth:    0,
			})
			lastOpenToolIdx = -1
		default:
			// Other event types are ignored — the renderer's surface
			// is intentionally scoped to topology-shaping events. A
			// future Phase 74 `topology.snapshot` event would also
			// land here as a new case branch.
		}
	}
	return t
}

// popPending removes and returns the most-recent index for name from
// the pending map. Returns (0, false) when name has no pending entry —
// the caller decides whether to treat that as "drop the event" or
// "synthesise a standalone node".
func popPending(m map[string][]int, name string) (int, bool) {
	stk := m[name]
	if len(stk) == 0 {
		return 0, false
	}
	idx := stk[len(stk)-1]
	stk = stk[:len(stk)-1]
	if len(stk) == 0 {
		delete(m, name)
	} else {
		m[name] = stk
	}
	return idx, true
}

// stringField extracts a string-valued payload field. Returns "" when
// the field is absent, nil, or not string-shaped. Defensive: the wire
// payload is a generic map; a producer that ships a numeric or boolean
// where a string is expected does not crash the synthesiser.
func stringField(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	// json.Number from the decoder if it was configured with
	// UseNumber — string-conversion attempt for safety.
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return ""
}

// numericField extracts a numeric payload field. Returns (0, false)
// when the field is absent or not numeric. Handles json.Number (when
// the decoder was configured with UseNumber) AND float64 (the default
// shape encoding/json produces for JSON numbers).
func numericField(payload map[string]interface{}, key string) (float64, bool) {
	if payload == nil {
		return 0, false
	}
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		// Some producers stringify numerics — best-effort parse.
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// ParseSSEFrames parses an SSE stream body (the bytes a GET /v1/events
// response yields) into a slice of WireEventFrame. Skips comment lines
// (`:` prefix), keepalives, retry directives. The SSE grammar is
// permissive — `data:` lines can be split across multiple lines and
// MUST be re-joined with `\n` per the spec. The `id:` and `event:`
// header lines are diagnostic only; the JSON payload is the source of
// truth (it duplicates type + sequence).
//
// Filters frames to those whose `Run` field equals runFilter. When
// runFilter is empty, every frame is included (used when the CLI is
// driven against an admin-scoped subscription that already filtered
// server-side via the X-Harbor-Run header).
//
// keepaliveBytes counts the number of `: keepalive` comment lines
// observed — the caller uses this to decide whether to back off the
// idle timer (a stream that emits keepalives is alive, just empty).
func ParseSSEFrames(body []byte, runFilter string) (frames []WireEventFrame, keepaliveCount int, err error) {
	// Split on the SSE event boundary: two consecutive newlines.
	rawEvents := strings.Split(string(body), "\n\n")
	for _, raw := range rawEvents {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Comment / keepalive lines start with `:`. Whole-event
		// comments are counted; otherwise we walk lines.
		var dataLines []string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, ":") {
				if strings.Contains(line, "keepalive") {
					keepaliveCount++
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "data:")), " "))
			}
			// event: and id: lines are diagnostic; ignored.
			// retry: directives are ignored.
		}
		if len(dataLines) == 0 {
			continue
		}
		// Re-join multi-line data with `\n` per SSE spec (defensive —
		// our encoder always single-line JSON, but the parser is
		// agnostic).
		payload := strings.Join(dataLines, "\n")
		var frame WireEventFrame
		if jsonErr := json.Unmarshal([]byte(payload), &frame); jsonErr != nil {
			// One malformed frame is not fatal — log to the caller
			// via the surface error and continue. Returning
			// (frames, err) lets the caller decide whether to
			// surface or swallow.
			return frames, keepaliveCount, fmt.Errorf("topology: parse SSE frame: %w", jsonErr)
		}
		if runFilter != "" && frame.Run != runFilter {
			continue
		}
		frames = append(frames, frame)
	}
	return frames, keepaliveCount, nil
}

// FrameOccurredAt parses the wire `occurred_at` field; returns a zero
// time on parse failure. Used by tests asserting deterministic ordering.
func FrameOccurredAt(f WireEventFrame) time.Time {
	if f.OccurredAt == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", f.OccurredAt)
	if err != nil {
		return time.Time{}
	}
	return t
}
