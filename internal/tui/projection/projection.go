// Package projection reduces Harbor Protocol snapshots and events into a
// rendering-independent conversation projection.
package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

const (
	maxSafeTextBytes     = 512
	maxLiveJournalEvents = 4096
)

var (
	// ErrIdentityRequired reports a projection input with an incomplete identity.
	ErrIdentityRequired = errors.New("projection: complete identity required")
	// ErrIdentityMismatch reports an input for a different isolation triple.
	ErrIdentityMismatch = errors.New("projection: identity mismatch")
)

// Reducer is an immutable, concurrent-safe projection compiler.
type Reducer struct{}

// SnapshotBundle is one generation of authoritative Protocol reads. Events
// must be oldest-first. CapturedSequence fences the point represented by the
// snapshots; later live events always win during reconciliation.
type SnapshotBundle struct {
	Generation              uint64
	CapturedSequence        uint64
	Identity                types.IdentityScope
	History                 types.StateHistoryResponse
	Tasks                   types.TaskListResponse
	TaskDetails             []types.TaskDetail
	Session                 *types.SessionsInspectResponse
	Pauses                  types.PauseListResponse
	Health                  *types.RuntimeHealth
	AggregateTruncated      bool
	ToolsAggregatesPartial  bool
	UnavailableCapabilities []string
	ToolAnalyticsBounded    bool
}

// Projection is a self-contained immutable value for one identity triple.
type Projection struct {
	Identity   types.IdentityScope `json:"identity"`
	Generation uint64              `json:"generation"`
	// Usage is the session's accumulated LLM usage, decoded from the canonical
	// llm.cost.recorded stream (the same source the Console reads). It is the
	// only honest place cost/tokens/context come from — never estimated.
	Usage Usage `json:"usage"`
	// RunUsage is the same accounting attributed per run, for the per-turn
	// summary line under each answer.
	RunUsage                map[string]Usage `json:"run_usage,omitempty"`
	LastSequence            uint64           `json:"last_sequence"`
	Cursor                  string           `json:"cursor,omitempty"`
	SessionStatus           string           `json:"session_status,omitempty"`
	SessionErased           bool             `json:"session_erased,omitempty"`
	HistoryTruncated        bool             `json:"history_truncated,omitempty"`
	HistoryHasMore          bool             `json:"history_has_more,omitempty"`
	AggregateTruncated      bool             `json:"aggregate_truncated,omitempty"`
	CountersPartial         bool             `json:"counters_partial,omitempty"`
	ToolsAggregatesPartial  bool             `json:"tools_aggregates_partial,omitempty"`
	ToolAnalyticsBounded    bool             `json:"tool_analytics_bounded,omitempty"`
	Retention               []Retention      `json:"retention,omitempty"`
	UnavailableCapabilities []string         `json:"unavailable_capabilities,omitempty"`
	Blocks                  []Block          `json:"blocks"`
	ReconciliationRequired  bool             `json:"reconciliation_required,omitempty"`
	ReplayGap               *ReplayGap       `json:"replay_gap,omitempty"`
	tombstones              map[string]uint64
	seen                    map[uint64]struct{}
	unsequenced             map[string]struct{}
	liveEvents              []WireEvent
	sessionSequence         uint64
	// streamStep counts completed LLM steps per run, so successive streaming
	// messages/reasoning in one turn key to distinct blocks in arrival order.
	streamStep map[string]int
}

// ReplayGap describes why an authoritative snapshot reconciliation is needed.
type ReplayGap struct {
	Reason       string `json:"reason"`
	LastSequence uint64 `json:"last_sequence"`
	SeenSequence uint64 `json:"seen_sequence"`
}

// Retention preserves the observed scope and optional horizon without
// pretending an identity-scoped absence is a runtime-wide empty.
type Retention struct {
	Surface string     `json:"surface"`
	Scope   string     `json:"scope,omitempty"`
	Oldest  *time.Time `json:"oldest_retained_at,omitempty"`
}

// Block is one normalized, safe conversation/runtime record. Text is bounded;
// unknown payload values are never copied into generic blocks.
// Usage is the canonical per-session LLM usage rollup accumulated from
// llm.cost.recorded. ContextWindow is the active model's input-token window (0
// when the Runtime has not advertised one — the caller must not invent a
// denominator).
type Usage struct {
	Model         string  `json:"model,omitempty"`
	TotalTokens   int64   `json:"total_tokens,omitempty"`
	PromptTokens  int64   `json:"prompt_tokens,omitempty"`
	OutputTokens  int64   `json:"output_tokens,omitempty"`
	USD           float64 `json:"usd,omitempty"`
	ContextWindow int64   `json:"context_window,omitempty"`
	// CacheReadTokens / CacheWriteTokens are a subset of PromptTokens
	// (prompt tokens served from / written to the provider's prompt cache),
	// not additional tokens — never summed into TotalTokens.
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type Block struct {
	ID          string                   `json:"id"`
	Kind        string                   `json:"kind"`
	RunID       string                   `json:"run_id,omitempty"`
	Sequence    uint64                   `json:"sequence,omitempty"`
	At          time.Time                `json:"at,omitempty"`
	Status      string                   `json:"status,omitempty"`
	Text        string                   `json:"text,omitempty"`
	Tool        string                   `json:"tool,omitempty"`
	Token       string                   `json:"token,omitempty"`
	EventType   string                   `json:"event_type,omitempty"`
	PayloadKeys []string                 `json:"payload_keys,omitempty"`
	Artifacts   []types.StateArtifactRef `json:"artifacts,omitempty"`
	Incomplete  bool                     `json:"incomplete,omitempty"`
	// DurationMS is the canonical wall-clock time the Runtime reports for the
	// run (TaskRow.StartedAt → UpdatedAt). It is authoritative: the client never
	// derives elapsed time from local wall-clock reads.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// ErrorCode is the bounded terminal error code the Runtime reports on a
	// task.failed frame. It is read from a field the reducer already received
	// on the wire (no wire change); the foreground turn-failure line renders
	// it so a failed turn shows an explicit code instead of going idle.
	ErrorCode string `json:"error_code,omitempty"`
}

// ChangeSet tells a renderer whether it may coalesce this update. Content and
// reasoning deltas are batchable; lifecycle/intervention changes are immediate.
type ChangeSet struct {
	Changed                bool     `json:"changed"`
	Batchable              bool     `json:"batchable"`
	Immediate              bool     `json:"immediate"`
	BlockIDs               []string `json:"block_ids,omitempty"`
	ReconciliationRequired bool     `json:"reconciliation_required,omitempty"`
	IgnoredTerminal        bool     `json:"ignored_terminal,omitempty"`
}

// WireEvent is the common state-history/SSE projection shape.
type WireEvent = types.StateEvent

// Hydrate constructs a projection from one complete snapshot generation.
func (r *Reducer) Hydrate(bundle SnapshotBundle) (Projection, error) {
	return r.projectSnapshot(bundle)
}

// Reconcile applies a newer snapshot generation without rolling back live
// mutations newer than CapturedSequence.
func Reconcile(current Projection, bundle SnapshotBundle) (Projection, ChangeSet, error) {
	return (&Reducer{}).reconcileWithChanges(current, bundle)
}

func (r *Reducer) reconcileWithChanges(current Projection, bundle SnapshotBundle) (Projection, ChangeSet, error) {
	if err := validateIdentity(bundle.Identity); err != nil {
		return Projection{}, ChangeSet{}, err
	}
	if err := sameIdentity(current.Identity, bundle.Identity); err != nil {
		return Projection{}, ChangeSet{}, err
	}
	if current.SessionErased {
		return cloneProjection(current), ChangeSet{Immediate: true, IgnoredTerminal: true}, nil
	}
	if bundle.Generation <= current.Generation {
		return cloneProjection(current), ChangeSet{}, nil
	}
	next, err := r.projectSnapshot(bundle)
	if err != nil {
		return Projection{}, ChangeSet{}, err
	}
	live := make([]WireEvent, 0, len(current.liveEvents))
	for _, event := range current.liveEvents {
		if event.Sequence == 0 || event.Sequence > bundle.CapturedSequence {
			live = append(live, event)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].Sequence == 0 || live[j].Sequence == 0 {
			return live[i].OccurredAt.Before(live[j].OccurredAt)
		}
		return live[i].Sequence < live[j].Sequence
	})
	for _, event := range live {
		next, _, err = r.apply(next, event, false)
		if err != nil {
			return Projection{}, ChangeSet{}, err
		}
	}
	return next, ChangeSet{Changed: true, Immediate: true}, nil
}

func (r *Reducer) projectSnapshot(bundle SnapshotBundle) (Projection, error) {
	if err := validateIdentity(bundle.Identity); err != nil {
		return Projection{}, err
	}
	next := Projection{
		Identity: cloneIdentity(bundle.Identity), Generation: bundle.Generation,
		Blocks: []Block{}, tombstones: map[string]uint64{}, seen: map[uint64]struct{}{},
		unsequenced: map[string]struct{}{}, liveEvents: []WireEvent{},
	}
	next.HistoryTruncated = bundle.History.Truncated
	next.HistoryHasMore = bundle.History.HasMore
	next.AggregateTruncated = bundle.AggregateTruncated
	next.ToolsAggregatesPartial = bundle.ToolsAggregatesPartial
	next.ToolAnalyticsBounded = bundle.ToolAnalyticsBounded
	next.UnavailableCapabilities = sortedUnique(bundle.UnavailableCapabilities)
	if bundle.Health != nil {
		next.Retention = make([]Retention, 0, len(bundle.Health.Retention))
		for _, horizon := range bundle.Health.Retention {
			next.Retention = append(next.Retention, Retention{Surface: horizon.Surface, Scope: horizon.Scope, Oldest: horizon.OldestRetainedAt})
		}
		sort.Slice(next.Retention, func(i, j int) bool { return next.Retention[i].Surface < next.Retention[j].Surface })
	}

	events := append([]types.StateEvent(nil), bundle.History.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	for _, event := range events {
		if bundle.CapturedSequence > 0 && event.Sequence > bundle.CapturedSequence {
			continue
		}
		var err error
		next, _, err = r.apply(next, event, true)
		if err != nil {
			return Projection{}, err
		}
	}

	if bundle.Session != nil && !next.SessionErased {
		next.SessionStatus = string(bundle.Session.Row.Status)
		next.CountersPartial = bundle.Session.Row.CountersPartial
		next.sessionSequence = bundle.CapturedSequence
	}
	if !next.SessionErased {
		mergeTasks(&next, bundle.Tasks.Rows, bundle.TaskDetails, false)
		mergePauses(&next, bundle.Pauses.Snapshots, bundle.CapturedSequence)
	}
	if bundle.CapturedSequence > next.LastSequence {
		next.LastSequence = bundle.CapturedSequence
	}
	return next, nil
}

// Apply reduces one live or replayed event. Duplicate events are idempotent;
// an unseen event behind the applied cursor is retained in the bounded journal
// and makes the need for authoritative reconciliation explicit.
func (r *Reducer) Apply(current Projection, event WireEvent) (Projection, ChangeSet, error) {
	return r.apply(current, event, false)
}

func (r *Reducer) apply(current Projection, event WireEvent, historical bool) (Projection, ChangeSet, error) {
	if err := validateIdentity(current.Identity); err != nil {
		return Projection{}, ChangeSet{}, err
	}
	if err := eventIdentity(current.Identity, event); err != nil {
		return Projection{}, ChangeSet{}, err
	}
	if current.SessionErased {
		return cloneProjection(current), ChangeSet{Immediate: true, IgnoredTerminal: true}, nil
	}
	next := cloneProjection(current)
	if event.Sequence == 0 {
		key := unsequencedKey(event)
		if _, ok := next.unsequenced[key]; ok {
			return cloneProjection(current), ChangeSet{}, nil
		}
		next.unsequenced[key] = struct{}{}
	} else {
		if _, ok := next.seen[event.Sequence]; ok {
			return next, ChangeSet{}, nil
		}
		next.seen[event.Sequence] = struct{}{}
		if !historical && event.Sequence < next.LastSequence {
			appendLiveEvent(&next, event)
			next.ReconciliationRequired = true
			next.ReplayGap = &ReplayGap{Reason: "unseen_out_of_order", LastSequence: next.LastSequence, SeenSequence: event.Sequence}
			return next, ChangeSet{Changed: true, Immediate: true, ReconciliationRequired: true}, nil
		}
		next.LastSequence = event.Sequence
		next.Cursor = fmt.Sprintf("%d", event.Sequence)
	}
	if !historical {
		appendLiveEvent(&next, event)
	}

	payload := object(event.Payload)
	runID := event.Run
	if runID == "" {
		runID = stringField(payload, "TaskID", "task_id")
	}
	immediate := true
	blockIDs := []string{}
	switch event.Type {
	case "llm.completion.chunk":
		var decoded completionChunkPayload
		if !decodePayload(event.Payload, &decoded) || decoded.TaskID == "" || (decoded.Kind != "content" && decoded.Kind != "reasoning") {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		if runID == "" {
			runID = decoded.TaskID
		}
		kind := "text"
		if decoded.Kind == "reasoning" {
			kind = "reasoning"
		}
		// Segment streaming per LLM step AND per kind. The stream keys used to
		// be a bare kind:runID, so a turn's preamble ("let me check…") and its
		// post-tool answer merged into ONE text block anchored where it first
		// appeared — yanking the final answer above the intervening
		// reasoning/tool blocks. Content and reasoning each carry their OWN Done
		// terminator, so each advances its own step counter; a shared counter
		// would let a reasoning terminator bump the content block index (and
		// vice versa), scrambling which delta lands in which block.
		stepKey := kind + ":" + runID
		step := next.streamStep[stepKey]
		id := fmt.Sprintf("%s:%s:%d", kind, runID, step)
		if decoded.Delta != "" {
			appendDelta(&next, id, kind, runID, event, decoded.Delta)
			blockIDs = append(blockIDs, id)
		}
		if decoded.Done {
			if next.streamStep == nil {
				next.streamStep = map[string]int{}
			}
			next.streamStep[stepKey] = step + 1
		}
		immediate = false
	case "llm.cost.recorded":
		// Canonical usage accounting. It is deliberately NOT a transcript block:
		// it feeds the session's cost/token/context readout instead of
		// interleaving accounting noise with the conversation.
		var decoded costPayload
		if !decodePayload(event.Payload, &decoded) {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		if decoded.Model != "" {
			next.Usage.Model = safeText(decoded.Model)
		}
		if decoded.ContextWindowTokens > 0 {
			next.Usage.ContextWindow = decoded.ContextWindowTokens
		}
		next.Usage.TotalTokens += decoded.Usage.TotalTokens
		next.Usage.PromptTokens += decoded.Usage.PromptTokens
		next.Usage.OutputTokens += decoded.Usage.CompletionTokens
		next.Usage.CacheReadTokens += decoded.Usage.CacheReadTokens
		next.Usage.CacheWriteTokens += decoded.Usage.CacheWriteTokens
		next.Usage.USD += decoded.Cost.TotalCost
		if event.Run != "" {
			if next.RunUsage == nil {
				next.RunUsage = map[string]Usage{}
			}
			run := next.RunUsage[event.Run]
			run.TotalTokens += decoded.Usage.TotalTokens
			run.PromptTokens += decoded.Usage.PromptTokens
			run.OutputTokens += decoded.Usage.CompletionTokens
			run.CacheReadTokens += decoded.Usage.CacheReadTokens
			run.CacheWriteTokens += decoded.Usage.CacheWriteTokens
			run.USD += decoded.Cost.TotalCost
			next.RunUsage[event.Run] = run
		}
		return next, ChangeSet{Changed: true, Immediate: false}, nil
	case "task.spawned", "task.started":
		var decoded taskPayload
		if !decodePayload(event.Payload, &decoded) || decoded.TaskID == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		runID = decoded.TaskID
		id := "task:" + runID
		upsertLifecycle(&next, Block{ID: id, Kind: "task", RunID: runID, Sequence: event.Sequence, At: event.OccurredAt, Status: strings.TrimPrefix(event.Type, "task.")})
		blockIDs = append(blockIDs, id)
	case "task.completed", "task.failed", "task.cancelled":
		var decoded taskPayload
		if !decodePayload(event.Payload, &decoded) || decoded.TaskID == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		runID = decoded.TaskID
		id := "task:" + runID
		terminalLifecycle(&next, id, "task", runID, strings.TrimPrefix(event.Type, "task."), event)
		if event.Type == "task.failed" && decoded.ErrorCode != "" {
			if idx := blockIndex(next.Blocks, id); idx >= 0 {
				next.Blocks[idx].ErrorCode = safeText(decoded.ErrorCode)
			}
		}
		blockIDs = append(blockIDs, id)
	case "notification.task_completed", "notification.task_group_resolved", "notification.task_group_cancelled":
		// The conversational mirror of a background terminal transition /
		// group resolution / unprompted group cancellation. Rendered
		// unconditionally as a muted one-line lifecycle notice — these are
		// deliberate operator-facing wake signals, not the event-fallback
		// leakage the generic branch guards against. (An operator-driven
		// cancel is suppressed upstream at the notifications mapper, so a
		// task_group_cancelled notification only ever reaches here for an
		// unprompted cancel worth surfacing.)
		var decoded notificationPayload
		if !decodePayload(event.Payload, &decoded) || decoded.Summary == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		id := notificationID(event)
		upsertNotification(&next, id, decoded.Summary, event)
		blockIDs = append(blockIDs, id)
	case "notification.task_failed":
		// The background-failure mirror. Suppressed when the failing task
		// IS a foreground turn — a "user:"+TaskID block exists, and that
		// block is minted only for foreground-Kind rows, so its presence is
		// the reliable foreground discriminator. That failure is surfaced by
		// the dedicated turn-failure line, so the mirror would duplicate it.
		// Rendered when no such foreground turn is tracked (a genuinely
		// background task failing).
		var decoded notificationPayload
		if !decodePayload(event.Payload, &decoded) || decoded.Summary == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		if decoded.TaskID != "" && blockIndex(next.Blocks, "user:"+decoded.TaskID) >= 0 {
			// Foreground turn — the turn-failure line owns this. Suppress the
			// duplicate mirror (still a typed, classified event, not a drop).
			return next, ChangeSet{Changed: false}, nil
		}
		id := notificationID(event)
		upsertNotification(&next, id, decoded.Summary, event)
		blockIDs = append(blockIDs, id)
	case "tool.invoked":
		var decoded toolPayload
		if !decodePayload(event.Payload, &decoded) || decoded.ToolName == "" || runID == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		tool := decoded.ToolName
		id := toolInvocationID(runID, event.Sequence, event.OccurredAt)
		upsertLifecycle(&next, Block{ID: id, Kind: "tool", RunID: runID, Tool: safeText(tool), Status: "invoked", Sequence: event.Sequence, At: event.OccurredAt})
		blockIDs = append(blockIDs, id)
	case "tool.completed", "tool.failed", "tool.policy_exhausted":
		var decoded toolPayload
		if !decodePayload(event.Payload, &decoded) || decoded.ToolName == "" || runID == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		tool := decoded.ToolName
		id := openToolInvocation(next.Blocks, runID, tool)
		incomplete := false
		if id == "" {
			id = toolInvocationID(runID, event.Sequence, event.OccurredAt)
			incomplete = true
		}
		status := "succeeded"
		if event.Type != "tool.completed" {
			status = "failed"
		}
		if incomplete {
			next.Blocks = append(next.Blocks, Block{ID: id, Kind: "tool", RunID: runID, Tool: safeText(tool), Status: status, Sequence: event.Sequence, At: event.OccurredAt, Incomplete: true})
		} else {
			terminalLifecycle(&next, id, "tool", runID, status, event)
		}
		setTool(&next, id, tool)
		blockIDs = append(blockIDs, id)
	case "pause.requested":
		var decoded pauseRequestedPayload
		if !decodePayload(event.Payload, &decoded) || decoded.Token == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		token := decoded.Token
		id := "intervention:" + token
		if next.tombstones[id] < event.Sequence {
			upsertLifecycle(&next, Block{ID: id, Kind: "intervention", RunID: runID, Token: safeText(token), Status: "pending", Text: safeText(decoded.Reason), Sequence: event.Sequence, At: event.OccurredAt})
			blockIDs = append(blockIDs, id)
		}
	case "tool.approval_requested":
		var decoded approvalRequestedPayload
		if !decodePayload(event.Payload, &decoded) || decoded.PauseToken == "" || decoded.Tool == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		upsertIntervention(&next, decoded.PauseToken, runID, decoded.Reason, event)
		blockIDs = append(blockIDs, "intervention:"+decoded.PauseToken)
	case "tool.auth_required":
		var decoded authRequiredPayload
		if !decodePayload(event.Payload, &decoded) || decoded.PauseToken == "" || decoded.Source == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		label := decoded.SourceName
		if label == "" {
			label = decoded.Source
		}
		upsertIntervention(&next, decoded.PauseToken, runID, "Connect "+label, event)
		blockIDs = append(blockIDs, "intervention:"+decoded.PauseToken)
	case "pause.resumed":
		var decoded pauseResolvedPayload
		if !decodePayload(event.Payload, &decoded) || decoded.Token == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		resolveInterventions(&next, decoded.Token, event.Sequence)
	case "tool.approved", "tool.rejected", "tool.auth_completed":
		var decoded interventionResolvedPayload
		if !decodePayload(event.Payload, &decoded) || decoded.PauseToken == "" {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		resolveInterventions(&next, decoded.PauseToken, event.Sequence)
	case "session.closed":
		var decoded sessionPayload
		if !decodePayload(event.Payload, &decoded) || decoded.SessionID != next.Identity.Session {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		next.SessionStatus = "completed"
		next.sessionSequence = event.Sequence
	case "session.reopened":
		var decoded sessionPayload
		if !decodePayload(event.Payload, &decoded) || decoded.SessionID != next.Identity.Session {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		next.SessionStatus = "running"
		next.sessionSequence = event.Sequence
	case "session.erased":
		var decoded sessionPayload
		if !decodePayload(event.Payload, &decoded) || decoded.SessionID != next.Identity.Session {
			return genericFallback(next, event, payload), ChangeSet{Changed: true, Immediate: true, BlockIDs: []string{genericEventID(event)}}, nil
		}
		markErased(&next, event.Sequence)
	default:
		next = genericFallback(next, event, payload)
		id := genericEventID(event)
		blockIDs = append(blockIDs, id)
	}
	return next, ChangeSet{Changed: true, Batchable: !immediate, Immediate: immediate, BlockIDs: blockIDs}, nil
}

// ApplyProtocolError records typed terminal session erasure. Other Protocol
// errors leave the projection unchanged and remain caller-owned.
func ApplyProtocolError(current Projection, err error) (Projection, ChangeSet) {
	var protocolErr *client.ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != protoerrors.CodeSessionErased {
		return cloneProjection(current), ChangeSet{}
	}
	next := cloneProjection(current)
	markErased(&next, next.LastSequence+1)
	return next, ChangeSet{Changed: true, Immediate: true}
}

// Normalize returns stable JSON suitable for cross-language fixture parity.
func Normalize(p Projection) ([]byte, error) {
	copy := cloneProjection(p)
	copy.tombstones = nil
	if copy.Blocks == nil {
		copy.Blocks = []Block{}
	}
	return json.Marshal(copy)
}

// HydrateClient performs the canonical tail-first snapshot join. Callers open
// their event stream first and pass its captured cursor as capturedSequence;
// events received while these reads run are then applied after Hydrate and win.
func HydrateClient(ctx context.Context, c client.Client, generation, capturedSequence uint64, maxPages int) (SnapshotBundle, error) {
	return HydrateClientWithOptions(ctx, c, generation, capturedSequence, HydrateOptions{MaxHistoryPages: maxPages})
}

// HydrateOptions controls read page sizes. Zero values use Protocol maxima.
type HydrateOptions struct {
	MaxHistoryPages int
	HistoryPageSize int
	TaskPageSize    int
	PausePageSize   int
}

// HydrateClientWithOptions performs the complete paginated snapshot join.
func HydrateClientWithOptions(ctx context.Context, c client.Client, generation, capturedSequence uint64, options HydrateOptions) (SnapshotBundle, error) {
	maxPages := options.MaxHistoryPages
	if maxPages <= 0 {
		maxPages = 8
	}
	historyPageSize := options.HistoryPageSize
	if historyPageSize <= 0 {
		historyPageSize = types.MaxStateHistoryLimit
	}
	taskPageSize := options.TaskPageSize
	if taskPageSize <= 0 {
		taskPageSize = types.MaxTaskListPageSize
	}
	pausePageSize := options.PausePageSize
	if pausePageSize <= 0 {
		pausePageSize = types.MaxPauseListPageSize
	}
	id := c.Identity()
	bundle := SnapshotBundle{Generation: generation, CapturedSequence: capturedSequence, Identity: id}
	info, err := c.RuntimeInfo(ctx)
	if err != nil {
		return SnapshotBundle{}, fmt.Errorf("projection: runtime info: %w", err)
	}
	advertised := make(map[types.Capability]struct{}, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		advertised[capability] = struct{}{}
	}
	for _, capability := range types.Capabilities() {
		if _, ok := advertised[capability]; !ok {
			bundle.UnavailableCapabilities = append(bundle.UnavailableCapabilities, string(capability))
		}
	}
	health, err := c.RuntimeHealth(ctx)
	if err != nil {
		return SnapshotBundle{}, fmt.Errorf("projection: runtime health: %w", err)
	}
	bundle.Health = &health
	before := uint64(0)
	pages := make([][]types.StateEvent, 0, maxPages)
	for range maxPages {
		page, err := c.StateHistory(ctx, types.StateHistoryRequest{SessionID: id.Session, Before: before, Limit: historyPageSize})
		if err != nil {
			return SnapshotBundle{}, fmt.Errorf("projection: state history: %w", err)
		}
		if len(pages) == 0 {
			bundle.History = page
			// The stream is already open. Fence hydration at the authoritative
			// history tail so buffered overlap is deterministically de-duplicated.
			bundle.CapturedSequence = page.TailSequence
		}
		bundle.History.Truncated = bundle.History.Truncated || page.Truncated
		bundle.History.HasMore = page.HasMore
		bundle.History.NextCursor = page.NextCursor
		pages = append(pages, page.Events)
		if !page.HasMore || page.NextCursor == 0 {
			break
		}
		before = page.NextCursor
	}
	bundle.History.Events = nil
	for i := len(pages) - 1; i >= 0; i-- {
		bundle.History.Events = append(bundle.History.Events, pages[i]...)
	}
	var taskCursor types.TaskListCursor
	seenTaskCursors := map[string]struct{}{}
	for {
		page, pageErr := c.TasksList(ctx, types.TaskListRequest{PageSize: taskPageSize, Cursor: taskCursor})
		if pageErr != nil {
			return SnapshotBundle{}, fmt.Errorf("projection: tasks list: %w", pageErr)
		}
		bundle.Tasks.Rows = append(bundle.Tasks.Rows, page.Rows...)
		bundle.Tasks.Aggregates = page.Aggregates
		bundle.Tasks.StatusCounterStrip = page.StatusCounterStrip
		next := page.Cursor.NextPageToken
		if next == "" {
			break
		}
		if _, exists := seenTaskCursors[next]; exists {
			return SnapshotBundle{}, errors.New("projection: tasks list repeated cursor")
		}
		seenTaskCursors[next] = struct{}{}
		taskCursor = page.Cursor
	}
	for _, row := range bundle.Tasks.Rows {
		detail, detailErr := c.TasksGet(ctx, types.TaskGetRequest{ID: row.ID})
		if detailErr != nil {
			return SnapshotBundle{}, fmt.Errorf("projection: task %s: %w", row.ID, detailErr)
		}
		bundle.TaskDetails = append(bundle.TaskDetails, detail)
	}
	session, err := c.SessionsInspect(ctx, types.SessionsInspectRequest{SessionID: id.Session})
	if err != nil {
		return SnapshotBundle{}, fmt.Errorf("projection: session inspect: %w", err)
	}
	bundle.Session = &session
	for pageNumber := 1; ; pageNumber++ {
		page, pageErr := c.PauseList(ctx, types.PauseListRequest{Page: pageNumber, PageSize: pausePageSize})
		if pageErr != nil {
			return SnapshotBundle{}, fmt.Errorf("projection: pause list: %w", pageErr)
		}
		bundle.Pauses.Snapshots = append(bundle.Pauses.Snapshots, page.Snapshots...)
		bundle.Pauses.Page = page.Page
		bundle.Pauses.PageSize = page.PageSize
		bundle.Pauses.PageCount = page.PageCount
		bundle.Pauses.TotalRows = page.TotalRows
		bundle.Pauses.Truncated = bundle.Pauses.Truncated || page.Truncated
		if page.PageCount <= pageNumber {
			break
		}
	}
	return bundle, nil
}

func mergeTasks(p *Projection, rows []types.TaskRow, details []types.TaskDetail, liveAfterSnapshot bool) {
	detailByID := make(map[string]types.TaskDetail, len(details))
	for _, detail := range details {
		detailByID[detail.Task.ID] = detail
	}
	sorted := append([]types.TaskRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt.Before(sorted[j].StartedAt) })
	for _, row := range sorted {
		if row.Identity.Tenant != p.Identity.Tenant || row.Identity.User != p.Identity.User || row.Identity.Session != p.Identity.Session {
			continue
		}
		// A user block is the marker of a FOREGROUND turn (a
		// composer-submitted prompt). Gate its creation on the row's Kind,
		// not on Query presence: a spawned BACKGROUND task also carries a
		// non-empty Query, so keying on Query would fabricate a user block
		// for background rows and misclassify their failures as foreground
		// turn failures. A foreground row always gets its user block —
		// falling back to the description (or empty) when Query is empty —
		// so a foreground turn is never misread as a background notice.
		if row.Kind == types.TaskKindForeground {
			id := "user:" + row.ID
			if blockIndex(p.Blocks, id) < 0 {
				text := row.Query
				if text == "" {
					text = row.Description
				}
				p.Blocks = append(p.Blocks, Block{ID: id, Kind: "user", RunID: row.ID, At: row.StartedAt, Text: text})
			}
		}
		id := "task:" + row.ID
		idx := blockIndex(p.Blocks, id)
		if idx < 0 {
			p.Blocks = append(p.Blocks, Block{ID: id, Kind: "task", RunID: row.ID, At: row.StartedAt, Status: string(row.Status), DurationMS: row.DurationMS})
		} else {
			if row.DurationMS > 0 {
				p.Blocks[idx].DurationMS = row.DurationMS
			}
			if !liveAfterSnapshot && !terminalStatus(p.Blocks[idx].Status) {
				p.Blocks[idx].Status = string(row.Status)
			}
		}
		if detail, ok := detailByID[row.ID]; ok && detail.ResultRef != nil {
			artifact := types.StateArtifactRef{ID: detail.ResultRef.ID, MimeType: detail.ResultRef.MimeType, SizeBytes: detail.ResultRef.SizeBytes, Filename: detail.ResultRef.Filename, SHA256: detail.ResultRef.SHA256}
			resultID := "result:" + row.ID
			if blockIndex(p.Blocks, resultID) < 0 {
				p.Blocks = append(p.Blocks, Block{ID: resultID, Kind: "result", RunID: row.ID, Artifacts: []types.StateArtifactRef{artifact}})
			}
		}
	}
}

func mergePauses(p *Projection, snapshots []types.PauseSnapshot, sequence uint64) {
	sorted := append([]types.PauseSnapshot(nil), snapshots...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].PausedAt.Before(sorted[j].PausedAt) })
	for _, pause := range sorted {
		if pause.Identity.Tenant != p.Identity.Tenant || pause.Identity.User != p.Identity.User || pause.Identity.Session != p.Identity.Session {
			continue
		}
		id := "intervention:" + pause.Token
		if p.tombstones[id] > sequence {
			continue
		}
		if pause.State == types.PauseStateResumed {
			p.tombstones[id] = sequence
			removeBlock(p, id)
			continue
		}
		if blockIndex(p.Blocks, id) < 0 {
			block := Block{ID: id, Kind: "intervention", RunID: pause.Identity.Run, Token: safeText(pause.Token), Status: "pending", Text: safeText(pause.Reason), At: pause.PausedAt}
			if pause.PayloadRef != nil {
				block.Artifacts = []types.StateArtifactRef{{ID: pause.PayloadRef.ID, MimeType: pause.PayloadRef.MimeType, SizeBytes: pause.PayloadRef.SizeBytes, Filename: pause.PayloadRef.Filename, SHA256: pause.PayloadRef.SHA256}}
			}
			p.Blocks = append(p.Blocks, block)
		}
	}
}

func appendDelta(p *Projection, id, kind, runID string, event types.StateEvent, delta string) {
	idx := blockIndex(p.Blocks, id)
	if idx < 0 {
		p.Blocks = append(p.Blocks, Block{ID: id, Kind: kind, RunID: runID, Sequence: event.Sequence, At: event.OccurredAt, Text: delta})
		return
	}
	p.Blocks[idx].Text += delta
	p.Blocks[idx].Sequence = event.Sequence
}

func terminalLifecycle(p *Projection, id, kind, runID, status string, event types.StateEvent) {
	idx := blockIndex(p.Blocks, id)
	if idx < 0 {
		p.Blocks = append(p.Blocks, Block{ID: id, Kind: kind, RunID: runID, Status: status, Sequence: event.Sequence, At: event.OccurredAt, Incomplete: true})
	} else {
		p.Blocks[idx].Status = status
		p.Blocks[idx].Sequence = event.Sequence
	}
	p.tombstones[id] = event.Sequence
}

func upsertLifecycle(p *Projection, block Block) {
	if p.tombstones[block.ID] >= block.Sequence {
		return
	}
	idx := blockIndex(p.Blocks, block.ID)
	if idx < 0 {
		p.Blocks = append(p.Blocks, block)
		return
	}
	if p.Blocks[idx].Sequence <= block.Sequence {
		p.Blocks[idx] = block
	}
}

func resolveInterventions(p *Projection, token string, sequence uint64) {
	for i := len(p.Blocks) - 1; i >= 0; i-- {
		block := p.Blocks[i]
		if block.Kind != "intervention" || block.Token != token {
			continue
		}
		p.tombstones[block.ID] = sequence
		p.Blocks = append(p.Blocks[:i], p.Blocks[i+1:]...)
	}
}

func markErased(p *Projection, sequence uint64) {
	p.SessionErased = true
	p.SessionStatus = "erased"
	p.Blocks = []Block{{ID: "session:erased", Kind: "session", Status: "erased", Sequence: sequence}}
	p.tombstones = map[string]uint64{"session:erased": sequence}
	p.seen = map[uint64]struct{}{sequence: {}}
	p.unsequenced = map[string]struct{}{}
	p.liveEvents = nil
	p.ReconciliationRequired = false
	p.ReplayGap = nil
}

func setTool(p *Projection, id, tool string) {
	if i := blockIndex(p.Blocks, id); i >= 0 {
		p.Blocks[i].Tool = safeText(tool)
	}
}
func toolInvocationID(runID string, sequence uint64, at time.Time) string {
	if sequence != 0 {
		return fmt.Sprintf("tool:%s:%d", runID, sequence)
	}
	return "tool:" + runID + ":unsequenced:" + at.UTC().Format(time.RFC3339Nano)
}
func openToolInvocation(blocks []Block, runID, tool string) string {
	for _, block := range blocks {
		if block.Kind == "tool" && block.RunID == runID && block.Tool == tool && block.Status == "invoked" {
			return block.ID
		}
	}
	return ""
}
func blockIndex(blocks []Block, id string) int {
	for i := range blocks {
		if blocks[i].ID == id {
			return i
		}
	}
	return -1
}
func removeBlock(p *Projection, id string) {
	if i := blockIndex(p.Blocks, id); i >= 0 {
		p.Blocks = append(p.Blocks[:i], p.Blocks[i+1:]...)
	}
}
func terminalStatus(status string) bool {
	return status == "completed" || status == "complete" || status == "failed" || status == "cancelled" || status == "succeeded"
}

func object(value any) map[string]any {
	if v, ok := value.(map[string]any); ok {
		return v
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return map[string]any{}
	}
	return out
}
func stringField(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			return value
		}
	}
	return ""
}
func safeKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, safeText(key))
	}
	sort.Strings(keys)
	if len(keys) > 32 {
		keys = keys[:32]
	}
	return keys
}
func safeText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > maxSafeTextBytes {
		return string(runes[:maxSafeTextBytes]) + "..."
	}
	return value
}

func appendLiveEvent(p *Projection, event WireEvent) {
	p.liveEvents = append(p.liveEvents, cloneEvent(event))
	if len(p.liveEvents) <= maxLiveJournalEvents {
		return
	}
	p.liveEvents = append([]WireEvent(nil), p.liveEvents[len(p.liveEvents)-maxLiveJournalEvents:]...)
	p.ReconciliationRequired = true
	p.ReplayGap = &ReplayGap{Reason: "live_journal_overflow", LastSequence: p.LastSequence}
}

func genericEventID(event WireEvent) string {
	if event.Sequence != 0 {
		return fmt.Sprintf("event:%d", event.Sequence)
	}
	return "event:unsequenced:" + event.Type + ":" + event.OccurredAt.UTC().Format(time.RFC3339Nano)
}

func genericFallback(p Projection, event WireEvent, payload map[string]any) Projection {
	id := genericEventID(event)
	p.Blocks = append(p.Blocks, Block{ID: id, Kind: "event", RunID: event.Run, Sequence: event.Sequence, At: event.OccurredAt, EventType: safeText(event.Type), PayloadKeys: safeKeys(payload), Artifacts: cloneArtifacts(event.Artifacts), Incomplete: true})
	return p
}

// notificationID keys a notification block. A notification.* event is a
// synthesised one-shot with its own bus sequence, so the sequence (or the
// unsequenced fallback) uniquely identifies it.
func notificationID(event WireEvent) string {
	if event.Sequence != 0 {
		return fmt.Sprintf("notification:%d", event.Sequence)
	}
	return "notification:unsequenced:" + event.Type + ":" + event.OccurredAt.UTC().Format(time.RFC3339Nano)
}

// upsertNotification appends (or idempotently refreshes) a muted
// conversational notification block. The text is the bounded, redacted
// Summary the runtime already composed; the reducer never fabricates one.
func upsertNotification(p *Projection, id, summary string, event WireEvent) {
	block := Block{ID: id, Kind: "notification", RunID: event.Run, Sequence: event.Sequence, At: event.OccurredAt, Text: safeText(summary)}
	idx := blockIndex(p.Blocks, id)
	if idx < 0 {
		p.Blocks = append(p.Blocks, block)
		return
	}
	p.Blocks[idx] = block
}

func upsertIntervention(p *Projection, token, runID, label string, event WireEvent) {
	id := "intervention:" + token
	if p.tombstones[id] >= event.Sequence && event.Sequence != 0 {
		return
	}
	upsertLifecycle(p, Block{ID: id, Kind: "intervention", RunID: runID, Token: safeText(token), Status: "pending", Text: safeText(label), Sequence: event.Sequence, At: event.OccurredAt})
}

type completionChunkPayload struct {
	TaskID string
	Delta  string
	Kind   string
	// Done marks the terminator chunk of one LLM completion stream (one ReAct
	// step). It is the boundary between a run's successive assistant messages
	// and reasoning segments, so streaming blocks segment on it.
	Done bool
}
type taskPayload struct {
	TaskID string
	// ErrorCode is populated on task.failed frames (empty on other task.*
	// lifecycle types). The reducer already received it on the wire and
	// previously discarded it; the turn-failure line now reads it.
	ErrorCode string
}

// notificationPayload decodes the redacted notification.* wire shape.
// The bus redacts NotificationPayload on Publish, so its wire keys are
// lowercase (the audit redactor lowercases field names); encoding/json
// matches these to the PascalCase fields case-insensitively.
type notificationPayload struct {
	Summary string
	TaskID  string
}

// costPayload mirrors the canonical llm.cost.recorded frame.
type costPayload struct {
	Model               string
	ContextWindowTokens int64
	Usage               struct {
		TotalTokens      int64
		PromptTokens     int64
		CompletionTokens int64
		CacheReadTokens  int64
		CacheWriteTokens int64
	}
	Cost struct{ TotalCost float64 }
}
type toolPayload struct{ ToolName string }
type pauseRequestedPayload struct {
	Token  string
	Reason string
}
type pauseResolvedPayload struct {
	Token    string
	Decision string
}
type approvalRequestedPayload struct {
	Tool       string
	PauseToken string
	Reason     string
}
type authRequiredPayload struct {
	Source     string
	SourceName string
	PauseToken string
}
type interventionResolvedPayload struct{ PauseToken string }
type sessionPayload struct{ SessionID string }

func decodePayload(value any, out any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, out) == nil
}

// EventClassification reports whether an event has a typed projection family
// or deliberately uses the safe generic fallback.
type EventClassification string

const (
	// EventTyped is a typed conversation/session projection family.
	EventTyped EventClassification = "typed"
	// EventGeneric is retained through the redacted generic fallback.
	EventGeneric EventClassification = "generic"
)

// ClassifyEventType mechanically classifies every canonical event name.
func ClassifyEventType(eventType string) EventClassification {
	switch eventType {
	case "llm.completion.chunk", "task.spawned", "task.started", "task.completed", "task.failed", "task.cancelled",
		"tool.invoked", "tool.completed", "tool.failed", "tool.policy_exhausted", "pause.requested", "pause.resumed",
		"tool.approval_requested", "tool.approved", "tool.rejected", "tool.auth_required", "tool.auth_completed",
		"session.closed", "session.reopened", "session.erased",
		"notification.task_completed", "notification.task_group_resolved", "notification.task_group_cancelled", "notification.task_failed":
		return EventTyped
	default:
		return EventGeneric
	}
}
func sortedUnique(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func unsequencedKey(event types.StateEvent) string {
	return "unsequenced:" + event.Type + ":" + event.Run + ":" + event.OccurredAt.UTC().Format(time.RFC3339Nano)
}

func validateIdentity(id types.IdentityScope) error {
	if strings.TrimSpace(id.Tenant) == "" || strings.TrimSpace(id.User) == "" || strings.TrimSpace(id.Session) == "" {
		return ErrIdentityRequired
	}
	return nil
}
func sameIdentity(left, right types.IdentityScope) error {
	if left.Tenant != right.Tenant || left.User != right.User || left.Session != right.Session {
		return fmt.Errorf("%w: got %s/%s/%s, want %s/%s/%s", ErrIdentityMismatch, right.Tenant, right.User, right.Session, left.Tenant, left.User, left.Session)
	}
	return nil
}
func eventIdentity(id types.IdentityScope, event types.StateEvent) error {
	return sameIdentity(id, types.IdentityScope{Tenant: event.Tenant, User: event.User, Session: event.Session})
}
func cloneIdentity(id types.IdentityScope) types.IdentityScope {
	return types.IdentityScope{Tenant: id.Tenant, User: id.User, Session: id.Session, Run: id.Run, Scope: id.Scope}
}
func cloneArtifacts(values []types.StateArtifactRef) []types.StateArtifactRef {
	return append([]types.StateArtifactRef(nil), values...)
}
func cloneEvent(event WireEvent) WireEvent {
	out := event
	out.Extra = make(map[string]string, len(event.Extra))
	for key, value := range event.Extra {
		out.Extra[key] = value
	}
	out.Artifacts = cloneArtifacts(event.Artifacts)
	if data, err := json.Marshal(event.Payload); err == nil {
		var payload any
		if json.Unmarshal(data, &payload) == nil {
			out.Payload = payload
		}
	}
	return out
}
func cloneProjection(p Projection) Projection {
	out := p
	out.Identity = cloneIdentity(p.Identity)
	out.Blocks = append([]Block(nil), p.Blocks...)
	for i := range out.Blocks {
		out.Blocks[i].PayloadKeys = append([]string(nil), p.Blocks[i].PayloadKeys...)
		out.Blocks[i].Artifacts = cloneArtifacts(p.Blocks[i].Artifacts)
	}
	out.Retention = append([]Retention(nil), p.Retention...)
	out.UnavailableCapabilities = append([]string(nil), p.UnavailableCapabilities...)
	out.tombstones = make(map[string]uint64, len(p.tombstones))
	for key, value := range p.tombstones {
		out.tombstones[key] = value
	}
	out.seen = make(map[uint64]struct{}, len(p.seen))
	for sequence := range p.seen {
		out.seen[sequence] = struct{}{}
	}
	out.unsequenced = make(map[string]struct{}, len(p.unsequenced))
	for key := range p.unsequenced {
		out.unsequenced[key] = struct{}{}
	}
	if p.streamStep != nil {
		out.streamStep = make(map[string]int, len(p.streamStep))
		for key, value := range p.streamStep {
			out.streamStep[key] = value
		}
	}
	out.liveEvents = make([]WireEvent, len(p.liveEvents))
	for i, event := range p.liveEvents {
		out.liveEvents[i] = cloneEvent(event)
	}
	if p.ReplayGap != nil {
		gap := *p.ReplayGap
		out.ReplayGap = &gap
	}
	return out
}
