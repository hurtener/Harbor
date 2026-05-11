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
)

func init() {
	for _, t := range []events.EventType{
		EventTypeImageMaterialized,
		EventTypeContextLeak,
		EventTypeContextWindowExceeded,
		EventTypeCostRecorded,
		EventTypeModeDowngraded,
	} {
		events.RegisterEventType(t)
	}
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
	Identity   identity.Quadruple
	Model      string
	Cost       Cost
	Usage      Usage
	OccurredAt time.Time
}

// ModeDowngradedPayload is the typed payload for
// EventTypeModeDowngraded. Phase 35 fills the From/To fields; Phase
// 32 registers the type only.
type ModeDowngradedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Model      string
	From       ResponseFormatKind
	To         ResponseFormatKind
	Reason     string
	OccurredAt time.Time
}
