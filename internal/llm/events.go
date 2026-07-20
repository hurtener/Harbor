package llm

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// LLM-edge event types. Registered via init() so the
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
	// threshold to an ArtifactRef. Carries the source
	// CompleteRequest's model name + the new ref's id + size.
	EventTypeImageMaterialized events.EventType = "llm.image.materialized"
	// EventTypeContextLeak — emitted when the safety-pass detects
	// raw heavy content that survived every upstream producer's
	// normalization step (violation). The bus event lets
	// operators trace the offending producer.
	EventTypeContextLeak events.EventType = "llm.context_leak"
	// EventTypeContextWindowExceeded — emitted when the safety-pass
	// token-budget guard fires. Payload carries the
	// estimated token count + the model's cap + the reserve
	// fraction so operators can quantify how often planner-side
	// recovery (truncate / summarize) needs to engage.
	EventTypeContextWindowExceeded events.EventType = "llm.context_window_exceeded"
	// EventTypeCostRecorded — emitted by the runtime AFTER a
	// successful Complete. The governance accumulator
	// subscribes; Harbor registers the type + ships the payload
	// shape so the emit site lands clean.
	EventTypeCostRecorded events.EventType = "llm.cost.recorded"
	// EventTypeModeDowngraded — emitted by the structured-
	// output downgrade chain (`json_schema → json_object → text`).
	// Harbor registers the type as a forward-compat seam; no
	// downgrade logic ships in a later phase.
	EventTypeModeDowngraded events.EventType = "llm.mode_downgraded"
	// EventTypeRetryWithFeedback — emitted by the retry
	// wrapper per corrective re-ask. Carries the attempt index and a
	// truncated `Reason` derived from the validator's error.
	EventTypeRetryWithFeedback events.EventType = "llm.retry_with_feedback"
	// EventTypePostureReadAdmin — Emitted when an
	// admin-scoped caller reads ANOTHER tenant's LLM posture via the
	// `llm.posture` Protocol method. An own-tenant read does NOT emit.
	// The cross-tenant read is a privileged action and lands on the
	// audit trail per CLAUDE.md §7 + RFC §6.15.
	EventTypePostureReadAdmin events.EventType = "llm.posture_read_admin"
	// EventTypeCompletionChunk — streaming completion event.
	// Emitted per token delta from the LLM provider under the originating
	// run's identity quadruple. The `Done=true` chunk fires exactly once
	// per stream (terminator marker). SafePayload — deltas are per-session
	// operator-visible content.
	EventTypeCompletionChunk events.EventType = "llm.completion.chunk"
	// EventTypeProviderCredentialFetched — emitted at every connect /
	// refresh pull of a broker-sourced provider key from the coordinator's
	// inference broker. Runtime-scoped: attributed to the per-runtime
	// SERVICE PRINCIPAL (the runtime service token that authenticates the
	// pull), NOT a (tenant, user, session) request — a provider key is a
	// runtime-level credential, not identity-scoped data, so the event does
	// not widen the isolation tuple. SafePayload: carries a non-reversible
	// key fingerprint (never the key value), the broker name, and the
	// outcome. A pull that cannot be audited fails loud (the pull is not
	// served without its audit trail).
	EventTypeProviderCredentialFetched events.EventType = "llm.provider_credential_fetched" //nolint:gosec // event type name, not a credential value
	// EventTypeProviderFileUploaded — emitted by the LLM driver when a
	// `provider_native`-flagged content part is uploaded to the
	// provider's file surface and rewritten to an opaque `file_id`
	// reference (the `provider_native` attachment disposition,
	// RFC §6.5). The event is the observability surface for
	// provider-native uploads — the Protocol/Console read it from the
	// event stream; no task field carries the `file_id`. Cache hits
	// (an already-uploaded artifact reused within the same identity
	// scope) do NOT re-emit.
	EventTypeProviderFileUploaded events.EventType = "llm.provider_file.uploaded"
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
		EventTypeProviderFileUploaded,
		EventTypeProviderCredentialFetched,
	} {
		events.RegisterEventType(t)
	}
}

// LLMProviderCredentialFetchedPayload is the typed payload for
// EventTypeProviderCredentialFetched. SafePayload — the runtime principal
// id, broker name, provider, non-reversible key fingerprint, and outcome
// are operator-visible audit metadata; the key VALUE is never carried.
//
// Attribution is RUNTIME-scoped (the flagged open item, resolved): the
// event is keyed by the per-runtime service principal — the runtime service
// token that authenticates the coordinator pull — NOT a synthesized
// (tenant, user, session). The principal is infrastructure identity and
// does not widen the isolation tuple (§4): storage / memory / event-filter
// scoping never treats it as an isolation principal. The reserved runtime
// scope on the carrying Event.Identity satisfies the bus's structural
// triple requirement only; the authoritative attribution is
// RuntimePrincipal.
type LLMProviderCredentialFetchedPayload struct {
	events.SafeSealed
	// RuntimePrincipal is the per-runtime service-principal id (a
	// non-secret identifier for the runtime service token that authenticated
	// the pull). The authoritative audit attribution key.
	RuntimePrincipal string
	// Broker is the boot-declared inference-broker name the key was pulled
	// from (non-secret).
	Broker string
	// Provider is the LLM provider the key authenticates (e.g. "openai").
	Provider string
	// KeyFingerprint is the non-reversible digest of the pulled key
	// (`sha256:<first 12 hex>`) for rotation correlation. NEVER the key.
	KeyFingerprint string
	// Phase is the pull occasion: "connect" (first pull) or "refresh".
	Phase string
	// Outcome is "ok" on a served pull; the failure event is emitted
	// separately by the source's fail-loud path.
	Outcome string
	// OccurredAt is the pull wall-clock instant.
	OccurredAt time.Time
}

// PostureReadAdminPayload is the typed payload for
// EventTypePostureReadAdmin. SafePayload — the actor's
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
// SafePayload — cost / token counts are operator-visible. The event is
// observability-only: the safety wrapper emits it once per driver-level
// completion for dashboards, replay tooling, and session-reopen
// reconstruction. Governance does NOT subscribe against it — cost
// accounting is in-band synchronous in the governance PostCall.
//
// The embedded Usage carries the provider's cache accounting
// (Usage.CacheReadTokens / Usage.CacheWriteTokens) for free — those two
// counts ride the same field and reach every consumer that already decodes
// Usage, no additional payload field required.
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
// EventTypeModeDowngraded. Harbor fills the From/To/Reason fields.
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

// RetryWithFeedbackPayload is the typed payload for
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

// ProviderFileUploadedPayload is the typed payload for
// EventTypeProviderFileUploaded. SafePayload — the artifact ref,
// provider name, modality, opaque `file_id`, MIME, and byte count are
// operator-visible upload metadata, not secret-shaped; the content
// bytes themselves never ride the event.
type ProviderFileUploadedPayload struct {
	events.SafeSealed
	// Identity is the uploading call's identity quadruple — the same
	// scope that keys the driver's file_id cache.
	Identity identity.Quadruple
	// Provider is the LLM provider the file was uploaded to.
	Provider string
	// Model is the request's model name.
	Model string
	// ArtifactRef is the Harbor artifact id the bytes came from.
	// Empty when the part carried inline sub-threshold bytes instead
	// of an artifact reference.
	ArtifactRef string
	// MIME is the uploaded content's media type.
	MIME string
	// Modality is the content's modality family derived from the MIME
	// (`image` / `audio` / `video` / `pdf` / `file`).
	Modality string
	// FileID is the opaque provider file reference the upload returned.
	FileID string
	// SizeBytes is the uploaded payload's byte length.
	SizeBytes int64
	// OccurredAt is the upload wall-clock instant.
	OccurredAt time.Time
}

// CompletionChunkPayload is the typed payload for
// EventTypeCompletionChunk. SafePayload — the delta is
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
