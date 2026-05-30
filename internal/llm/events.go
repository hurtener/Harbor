package llm

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Phase 32 LLM-edge event types. Registered via init() so the
// canonical events registry stays the single source of truth (see
// internal/events/events.go and AGENTS.md §17.6's "wiring gap"
// lesson — register at declaration time, publish at use time).
//
// All payloads are SafePayload (compose events.SafeSealed): they
// carry no secret-shaped data. Identity is the Harbor quadruple;
// content payloads (artifact refs, MIME types, byte counts, model
// names) are operator-visible by design.
const (
	// EventTypeImageMaterialized — emitted when the safety-pass's
	// auto-materialize step rewrites an inline DataURL ≥ heavy-output
	// threshold to an ArtifactRef (D-022). Carries the source
	// CompleteRequest's model name + the new ref's id + size.
	EventTypeImageMaterialized events.EventType = "llm.image.materialized"
	// EventTypeContextLeak — emitted when the safety-pass detects
	// raw heavy content that survived every upstream producer's
	// normalization step (D-026 violation). The bus event lets
	// operators trace the offending producer.
	EventTypeContextLeak events.EventType = "llm.context_leak"
	// EventTypeContextWindowExceeded — emitted when the safety-pass
	// token-budget guard fires (D-026). Payload carries the
	// estimated token count + the model's cap + the reserve
	// fraction so operators can quantify how often planner-side
	// recovery (truncate / summarize) needs to engage.
	EventTypeContextWindowExceeded events.EventType = "llm.context_window_exceeded"
	// EventTypeCostRecorded — emitted by the runtime AFTER a
	// successful Complete. Phase 36a (governance accumulator)
	// subscribes; Phase 32 registers the type + ships the payload
	// shape so Phase 36a's emit site lands clean.
	EventTypeCostRecorded events.EventType = "llm.cost.recorded"
	// EventTypeModeDowngraded — emitted by Phase 35's structured-
	// output downgrade chain (`json_schema → json_object → text`).
	// Phase 32 registers the type as a forward-compat seam; no
	// downgrade logic ships in Phase 32.
	EventTypeModeDowngraded events.EventType = "llm.mode_downgraded"
	// EventTypeRetryWithFeedback (Phase 36) — emitted by the retry
	// wrapper per corrective re-ask. Carries the attempt index and a
	// truncated `Reason` derived from the validator's error.
	EventTypeRetryWithFeedback events.EventType = "llm.retry_with_feedback"
	// EventTypePostureReadAdmin — Phase 72g (D-112). Emitted when an
	// admin-scoped caller reads ANOTHER tenant's LLM posture via the
	// `llm.posture` Protocol method. An own-tenant read does NOT emit.
	// The cross-tenant read is a privileged action and lands on the
	// audit trail per CLAUDE.md §7 + RFC §6.15.
	EventTypePostureReadAdmin events.EventType = "llm.posture_read_admin"
	// EventTypeCompletionChunk — Phase 107 streaming completion event.
	// Emitted per token delta from the LLM provider under the originating
	// run's identity quadruple. The `Done=true` chunk fires exactly once
	// per stream (terminator marker). SafePayload — deltas are per-session
	// operator-visible content.
	EventTypeCompletionChunk events.EventType = "llm.completion.chunk"
)

func init() {
	for _, t := range []events.EventType{
		EventTypeImageMaterialized,
		EventTypeContextLeak,
		EventTypeContextWindowExceeded,
		EventTypeCostRecorded,
		EventTypeModeDowngraded,
		EventTypeRetryWithFeedback,
		EventTypePostureReadAdmin,
		EventTypeCompletionChunk,
	} {
		events.RegisterEventType(t)
	}
}

// PostureReadAdminPayload is the typed payload for
// EventTypePostureReadAdmin (Phase 72g). SafePayload — the actor's
// identity and the requested tenant are operator-visible audit
// metadata, not secret-shaped. NEVER carries provider API keys — the
// posture surface reports provider/model/region only. The payload runs
// through the audit Redactor before the bus publish (CLAUDE.md §7).
type PostureReadAdminPayload struct {
	events.SafeSealed
	// Actor is the identity of the admin-scoped caller that performed
	// the cross-tenant read.
	Actor identity.Quadruple
	// RequestedTenant is the tenant_id the caller asked to read — a
	// tenant other than the caller's own.
	RequestedTenant string
}

// ImageMaterializedPayload is the typed payload for
// EventTypeImageMaterialized. SafePayload — the artifact ref, MIME
// type, and size are operator-visible content metadata, not secrets.
type ImageMaterializedPayload struct {
	events.SafeSealed
	Identity    identity.Quadruple
	Model       string
	ArtifactRef string
	MIME        string
	SizeBytes   int64
	OccurredAt  time.Time
}

// ContextLeakPayload is the typed payload for EventTypeContextLeak.
// SafePayload — the leak-site identifier (a short structural
// fingerprint like "Messages[2].Content.Text") is operator-visible
// debug data, not secret-shaped.
//
// `SizeBytes` is the size of the offending payload; `Threshold` is
// the runtime's configured heavy-output threshold at the time of the
// emit, so an operator can correlate config-change-time drift.
type ContextLeakPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Model      string
	LeakSite   string
	SizeBytes  int64
	Threshold  int
	OccurredAt time.Time
}

// ContextWindowExceededPayload is the typed payload for
// EventTypeContextWindowExceeded. SafePayload — token counts +
// configured cap are operator-visible.
type ContextWindowExceededPayload struct {
	events.SafeSealed
	Identity             identity.Quadruple
	Model                string
	EstimatedTokens      int
	ContextWindowTokens  int
	ContextWindowReserve float64
	OccurredAt           time.Time
}

// CostRecordedPayload is the typed payload for EventTypeCostRecorded.
// SafePayload — cost / token counts are operator-visible. Phase 36a
// subscribes for per-identity accumulator updates.
type CostRecordedPayload struct {
	events.SafeSealed
	Identity identity.Quadruple
	Model    string
	Cost     Cost
	Usage    Usage
	// ContextWindowTokens is the model's input-token window (from the
	// model profile), stamped so the Console can render context-used vs
	// window (%). Zero when the model has no profile / configured window.
	ContextWindowTokens int
	OccurredAt          time.Time
}

// ModeDowngradedPayload is the typed payload for
// EventTypeModeDowngraded. Phase 35 fills the From/To/Reason fields.
// `FromMode` / `ToMode` carry the Harbor-side `OutputMode` (Native /
// Tools / Prompted / text); `From` / `To` carry the resolved
// `ResponseFormatKind` for backward visibility.
type ModeDowngradedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Model      string
	FromMode   OutputMode
	ToMode     OutputMode
	From       ResponseFormatKind
	To         ResponseFormatKind
	Reason     string
	OccurredAt time.Time
}

// RetryWithFeedbackPayload (Phase 36) is the typed payload for
// EventTypeRetryWithFeedback. SafePayload — `Attempt` is the 1-based
// retry index (1 = first re-ask after the original); `Reason` is the
// validator's truncated `Error()` string. The wrapper truncates
// Reason at 256 characters to keep audit payloads bounded.
type RetryWithFeedbackPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Model      string
	Attempt    int
	MaxRetries int
	Reason     string
	OccurredAt time.Time
}

// CompletionChunkPayload is the typed payload for
// EventTypeCompletionChunk (Phase 107). SafePayload — the delta is
// per-session operator-visible content (the LLM's own output), not a
// secret. Kind is "content" or "reasoning".
type CompletionChunkPayload struct {
	events.SafePayload
	Identity   identity.Quadruple
	TaskID     string
	RunID      string
	Delta      string
	Done       bool
	Kind       string
	OccurredAt time.Time
}
