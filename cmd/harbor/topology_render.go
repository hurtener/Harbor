// cmd/harbor/topology_render.go — Phase 70 (D-102): the ASCII tree
// renderer that backs `harbor inspect-topology`.
//
// # Source choice — trajectory-synthesised, not event-driven (D-102)
//
// Phase 70's master-plan goal cites `topology.snapshot` events; Phase 74
// (Console topology projection events, RFC §6.13) is the canonical
// producer of those events. Phase 74 has not yet landed, so Phase 70
// SYNTHESISES the topology from the run's existing event stream
// (`tool.invoked` / `tool.completed` / `tool.failed`, `task.spawned`,
// `planner.finish`, `pause.requested`). When Phase 74 ships, this
// renderer gains a "preferred-source" branch that reads `topology.snapshot`
// frames directly — the synthesise path stays as a fallback for runs whose
// snapshot frames pre-date the producer (D-102's source dual-path note).
//
// # Renderer shape — indent-based (D-102)
//
// Two valid ASCII shapes for run graphs: box-drawing (`├──`, `└──`,
// `│`) and indent-based (`+--`, plain spaces). The indent-based shape
// wins on three criteria the prompt asked us to settle:
//
//   - terminal portability — indent + `+--` renders on every terminal,
//     including Windows cmd's CP437, Linux TTYs without ncurses, CI
//     log capture tools that strip ANSI / Unicode;
//   - deterministic byte length — fixed-width ASCII makes the golden
//     comparison trivial under `diff`; box-drawing characters can be
//     multi-byte UTF-8 sequences that bloat the golden surface;
//   - readability — the visual hierarchy is one space per level + the
//     `+--` connector; readers from a wider terminal family see the
//     same shape as readers in a 80-col SSH window.
//
// Sort order is `(Sequence, EventID)` — `Sequence` is per-bus
// monotonic + gap-free (events.Event.Sequence), so two snapshots of
// the same run produce byte-stable ASCII. `EventID` (ULID-shaped) is
// the tie-break if a future driver ever issues parallel sequences.
//
// # Truncation rule
//
// Node labels longer than `width - indent - connector` characters are
// truncated with a trailing `…` (single rune, three bytes) so a wide
// tool name (e.g. `mcp:slack:send_message_to_channel_with_very_long_name`)
// does not bleed past the right margin. Truncation is applied AFTER
// indent so a deeply-nested node loses content first.
//
// The renderer is pure: no I/O, no goroutines, no clock. The cmd body
// (cmd_inspect_topology.go) shells the assembled `Topology` value
// through `Render(t, width)`.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TopologyNodeKind enumerates the node shapes the renderer recognises.
// Each kind maps to one bus-emitted event category; the synthesise pass
// in `BuildTopologyFromEvents` produces the matching kind per event.
type TopologyNodeKind string

const (
	// TopologyNodeTool — a `tool.invoked` event (or its terminal
	// `tool.completed` / `tool.failed` counterpart). The renderer
	// merges paired invoke/complete events into a single node with
	// duration + status decoration.
	TopologyNodeTool TopologyNodeKind = "tool"
	// TopologyNodeTask — a `task.spawned` event. Renders the task's
	// kind (foreground / background) + the assigned TaskID prefix.
	TopologyNodeTask TopologyNodeKind = "task"
	// TopologyNodePause — a `pause.requested` event (unified pause
	// primitive, Phase 50 / D-067). Renders the pause Reason so
	// operators see which kind of pause (`approval_required` /
	// `auth_required` / `input_required`) is blocking the run.
	TopologyNodePause TopologyNodeKind = "pause"
	// TopologyNodeFinish — a `planner.finish` event. Renders the
	// Finish Reason. Always the LAST node in a complete run.
	TopologyNodeFinish TopologyNodeKind = "finish"
	// TopologyNodeApproval — a `tool.approval_requested` event. Shown
	// as a child of the parent tool node (the gate-wrapped tool's
	// approval prompt).
	TopologyNodeApproval TopologyNodeKind = "approval"
	// TopologyNodeAuth — a `tool.auth_required` event. Shown as a
	// child of the parent tool node (the OAuth-wrapped tool's auth
	// prompt).
	TopologyNodeAuth TopologyNodeKind = "auth"
)

// TopologyNodeStatus enumerates the terminal-state decorations the
// renderer applies. Drives the trailing `[ok]` / `[fail]` / `[pending]`
// suffix on a tool node. NOT applied to pause / finish / task nodes —
// those have their own kind-specific decoration.
type TopologyNodeStatus string

const (
	// TopologyStatusPending — the node has been observed in
	// invoked-shape but no terminal-shape event has arrived. Renders
	// as `[pending]`.
	TopologyStatusPending TopologyNodeStatus = "pending"
	// TopologyStatusOK — paired invoke + completed event observed.
	// Renders as `[ok]` plus a duration if available.
	TopologyStatusOK TopologyNodeStatus = "ok"
	// TopologyStatusFail — paired invoke + failed event observed.
	// Renders as `[fail]` plus the ErrorClass if available.
	TopologyStatusFail TopologyNodeStatus = "fail"
)

// TopologyNode is one row in the rendered ASCII tree. The renderer
// arranges nodes by depth + sequence; the synthesiser populates Depth
// based on parent inference (a `tool.approval_requested` is a child of
// the preceding `tool.invoked` for the same tool name).
//
// All fields are value-typed and lower-case-only-where-it-matters —
// stable serialisation via encoding/json yields a byte-identical JSON
// snapshot for a given input event order.
type TopologyNode struct {
	// Sequence is the source event's per-bus monotonic Sequence.
	// Primary sort key — guarantees byte-stable ordering.
	Sequence uint64 `json:"sequence"`
	// EventID is the source event's ULID-shaped ID. Tie-break when
	// two events share a Sequence (which today they cannot, but the
	// schema reserves the slot per events.Event docs).
	EventID string `json:"event_id,omitempty"`
	// Kind is the node category. Drives the rendered prefix.
	Kind TopologyNodeKind `json:"kind"`
	// Label is the human-readable identifier — tool name, task ID,
	// finish reason, pause reason. Truncated to width at render time.
	Label string `json:"label"`
	// Status is the terminal-state decoration for tool nodes;
	// empty for pause/finish/task/approval/auth.
	Status TopologyNodeStatus `json:"status,omitempty"`
	// Detail is an optional one-line extra rendered in parentheses
	// after the label — e.g. `(123ms)` for a completed tool, or
	// `(class=transient)` for a failed tool, or `(reason=goal)` for
	// a finish.
	Detail string `json:"detail,omitempty"`
	// Depth is the indent level (0 = root). The synthesiser computes
	// this from event-kind inference rules (tool.approval_requested
	// → child of the open tool.invoked); the renderer respects it.
	Depth int `json:"depth"`
}

// Topology is the assembled run graph. `RunID` + `Identity` decorate
// the rendered header; `Nodes` is the body. A nil or empty Topology
// is renderable — it produces a header + a `(no events)` line.
type Topology struct {
	// RunID identifies the run the topology was synthesised for.
	// Mandatory: the renderer refuses to render an empty RunID
	// (calling code should fail before reaching this).
	RunID string `json:"run_id"`
	// Tenant / User / Session are the identity triple decoration.
	// Optional — empty fields render as `<unknown>`.
	Tenant  string `json:"tenant,omitempty"`
	User    string `json:"user,omitempty"`
	Session string `json:"session,omitempty"`
	// SourceMode records how the topology was assembled — either
	// `"events.synthesised"` (Phase 70's V1 path, this PR) or
	// `"events.topology_snapshot"` (post-Phase 74 path, future). The
	// header line names it so an operator can distinguish the two.
	SourceMode string `json:"source_mode"`
	// Nodes is the rendered body, sorted by (Sequence, EventID) at
	// render time. The synthesiser appends in observation order; the
	// renderer applies the deterministic sort before walking.
	Nodes []TopologyNode `json:"nodes"`
}

// ErrEmptyRunID surfaces when Render is called with a zero RunID.
// Renderer refuses to silently emit a header with `<empty>` — the
// caller is doing something wrong (CLAUDE.md §5 fail loudly).
var ErrEmptyRunID = errors.New("topology: RunID is empty")

// DefaultRenderWidth is the column width the renderer truncates labels
// to when the caller does not pass `--width`. 80 is the universal SSH
// terminal default; wider terminals get more room for long tool names
// via `--width 120`.
const DefaultRenderWidth = 80

// MinRenderWidth caps the lower bound on `--width`. The renderer needs
// enough room for the deepest indent + a connector + a one-char label;
// 20 columns is the floor below which the output is unreadable.
const MinRenderWidth = 20

// MaxRenderWidth caps the upper bound on `--width`. 1024 is comfortably
// above any reasonable terminal; rejecting larger values keeps the
// caller's `--width` flag from accepting nonsense (e.g. a typo of
// `--width 80000`).
const MaxRenderWidth = 1024

// indentStep is the per-depth indent (in spaces). Two spaces is the
// readable minimum; box-drawing renderers would use four, but
// indent-based ASCII reads better tight.
const indentStep = 2

// connector is the fixed two-char prefix every non-root node carries
// (`+- `). Root nodes (depth 0) carry no connector — they print at
// column zero with the kind tag in column zero.
const connector = "+- "

// ellipsis is the trailing rune the truncator appends.
const ellipsis = "…"

// Render serialises t as a deterministic ASCII tree at width columns.
// Returns ErrEmptyRunID when t.RunID is empty; otherwise the byte slice
// is the renderable output (caller writes to stdout).
//
// width is clamped to [MinRenderWidth, MaxRenderWidth]; the caller's
// `--width` flag wiring should validate first so the operator sees the
// CLIError, but the renderer is defensive (a programmer-bug-induced
// 0 produces 80, not a panic).
func Render(t Topology, width int) ([]byte, error) {
	if t.RunID == "" {
		return nil, ErrEmptyRunID
	}
	w := normaliseWidth(width)

	// Sort nodes by (Sequence, EventID). The synthesiser appends in
	// observation order; the bus is per-session-Sequence-ordered
	// already, but a defensive sort here pins the determinism
	// contract regardless of how the caller fed nodes in.
	sorted := append([]TopologyNode(nil), t.Nodes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Sequence != sorted[j].Sequence {
			return sorted[i].Sequence < sorted[j].Sequence
		}
		return sorted[i].EventID < sorted[j].EventID
	})

	var b strings.Builder

	// Header: three lines.
	//   run <RunID>
	//   tenant=<t> user=<u> session=<s>
	//   source: <mode>
	fmt.Fprintf(&b, "run %s\n", t.RunID)
	fmt.Fprintf(&b, "tenant=%s user=%s session=%s\n",
		valOrUnknown(t.Tenant),
		valOrUnknown(t.User),
		valOrUnknown(t.Session))
	fmt.Fprintf(&b, "source: %s\n", valOrUnknown(t.SourceMode))
	b.WriteString("\n")

	if len(sorted) == 0 {
		b.WriteString("(no events)\n")
		return []byte(b.String()), nil
	}

	for _, n := range sorted {
		writeNode(&b, n, w)
	}
	return []byte(b.String()), nil
}

// RenderJSON serialises t as canonical JSON. The JSON form is what
// `--json` mode emits; it is byte-stable for a given input because all
// fields are deterministically ordered (map types are avoided) and the
// nodes slice is sorted by (Sequence, EventID) like the ASCII path.
func RenderJSON(t Topology) ([]byte, error) {
	if t.RunID == "" {
		return nil, ErrEmptyRunID
	}
	sorted := append([]TopologyNode(nil), t.Nodes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Sequence != sorted[j].Sequence {
			return sorted[i].Sequence < sorted[j].Sequence
		}
		return sorted[i].EventID < sorted[j].EventID
	})
	out := Topology{
		RunID:      t.RunID,
		Tenant:     t.Tenant,
		User:       t.User,
		Session:    t.Session,
		SourceMode: t.SourceMode,
		Nodes:      sorted,
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("topology: marshal json: %w", err)
	}
	buf = append(buf, '\n')
	return buf, nil
}

// writeNode renders one node to b at the configured width.
func writeNode(b *strings.Builder, n TopologyNode, width int) {
	indent := strings.Repeat(" ", n.Depth*indentStep)
	var prefix string
	if n.Depth == 0 {
		prefix = ""
	} else {
		prefix = connector
	}
	tag := nodeTag(n.Kind)
	body := buildBody(n)
	full := indent + prefix + tag + " " + body
	if len(full) > width {
		// Truncate the body, not the prefix — the prefix is the
		// structural backbone.
		keep := width - len(indent) - len(prefix) - len(tag) - 1
		if keep < 4 {
			// At minimal width the body collapses to just the
			// ellipsis. The structural prefix stays.
			keep = 4
		}
		body = truncateLabel(body, keep)
		full = indent + prefix + tag + " " + body
	}
	b.WriteString(full)
	b.WriteString("\n")
}

// nodeTag is the leading kind decoration (uppercase, fixed-width-ish so
// columns align). The renderer uses the longest tag (`approval`) as
// the de-facto alignment baseline; shorter tags get trailing-space
// padding so the body column lines up.
func nodeTag(k TopologyNodeKind) string {
	switch k {
	case TopologyNodeTool:
		return "[tool    ]"
	case TopologyNodeTask:
		return "[task    ]"
	case TopologyNodePause:
		return "[pause   ]"
	case TopologyNodeFinish:
		return "[finish  ]"
	case TopologyNodeApproval:
		return "[approval]"
	case TopologyNodeAuth:
		return "[auth    ]"
	default:
		return "[?       ]"
	}
}

// buildBody constructs the human-readable body of a node — label + an
// optional status and detail in trailing parentheses.
func buildBody(n TopologyNode) string {
	var b strings.Builder
	b.WriteString(n.Label)
	switch {
	case n.Status != "" && n.Detail != "":
		fmt.Fprintf(&b, " [%s] (%s)", n.Status, n.Detail)
	case n.Status != "":
		fmt.Fprintf(&b, " [%s]", n.Status)
	case n.Detail != "":
		fmt.Fprintf(&b, " (%s)", n.Detail)
	}
	return b.String()
}

// truncateLabel applies the trailing ellipsis. Operates on byte length;
// labels are ASCII in practice (tool names) so this is safe — a
// future Unicode-labelled tool would need a rune-aware variant.
func truncateLabel(s string, keep int) string {
	if keep <= 0 {
		return ellipsis
	}
	if len(s) <= keep {
		return s
	}
	if keep <= 1 {
		return ellipsis
	}
	return s[:keep-1] + ellipsis
}

// valOrUnknown returns s, or `<unknown>` if s is empty.
func valOrUnknown(s string) string {
	if s == "" {
		return "<unknown>"
	}
	return s
}

// normaliseWidth clamps width to [MinRenderWidth, MaxRenderWidth],
// returning DefaultRenderWidth when width is 0 (the cobra flag default
// for an unset int).
func normaliseWidth(width int) int {
	if width <= 0 {
		return DefaultRenderWidth
	}
	if width < MinRenderWidth {
		return MinRenderWidth
	}
	if width > MaxRenderWidth {
		return MaxRenderWidth
	}
	return width
}
