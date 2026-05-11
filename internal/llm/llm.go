// Package llm defines Harbor's LLM-client interface and the
// runtime-wide invariants that guard every `Complete` call.
//
// The interface is **one method**, `Complete(ctx, req) (resp, error)`
// (RFC §6.5). Tool dispatch is the runtime's job (RFC §6.4 + brief 07
// "code-level tool calling"); the LLM client is reduced to a JSON-
// producing chat-completion adapter. Provider-native tool-calling
// shapes (the `tools=` request parameter, the `tool_choice=` mode
// selector, OpenAI's `function_call`, Anthropic's `tool_use` blocks,
// Gemini's function-calling protocol, etc.) never appear in this
// package — the static guard in `scripts/smoke/phase-32.sh` enforces
// the boundary by greppping for the canonical symbol names.
//
// The message envelope is provider-agnostic: `ChatMessage.Content`
// is a sum-type that carries either `Text *string` (the common case)
// or `Parts []ContentPart` for multimodal input (D-021).
// Multimodal parts (`ImagePart`, `AudioPart`, `FilePart`) each carry
// one of three supply forms — `URL`, `DataURL`, or `Artifact` — and
// the runtime auto-materializes inline `DataURL` content above the
// heavy-output threshold into `ArtifactRef`s before persistence and
// emit (D-022).
//
// **Context-window safety net (D-026).** Every `Complete` call routes
// through a catch-all pass at the LLM-client edge that (a) auto-
// materializes oversize `DataURL` content, (b) asserts no raw heavy
// content survived ANY producer's normalization step (else
// `ErrContextLeak`), (c) estimates token usage against the configured
// `ModelProfile.ContextWindowTokens` cap and fails with
// `ErrContextWindowExceeded` when the estimate is within
// `ContextWindowReserve` of the cap. **V1 fails loudly**; auto-cascading
// recovery is post-V1 work.
//
// The safety pass is **mandatory by construction**: `Open` returns a
// wrapped client (`safetyClient`) that runs the pass before
// delegating to the underlying `Driver`. Drivers cannot bypass the
// pass through the registry; a hand-constructed `Driver` would
// likewise have to compose `enforceContextSafety` to maintain the
// runtime invariant.
//
// Concurrent-reuse contract (D-025): one `LLMClient` is safe to
// share across N concurrent goroutines. Mutable state on the client
// (or the `Driver`) is forbidden; per-call state lives in `ctx` and
// the request value. The package-level `concurrent_test.go` pins
// this with N=128 invocations under `-race`.
package llm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// LLMClient is the single contract callers depend on. ONE method.
// Streaming is signalled via `req.Stream` + `req.OnContent` /
// `req.OnReasoning`; cancellation flows through `ctx`. The runtime
// owns prompt construction, tool semantics, parsing, and parallel
// dispatch — see RFC §6.4 + brief 07.
//
// Implementations MUST be safe for N concurrent goroutines against a
// single shared instance (D-025).
type LLMClient interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
	// Close releases driver-held resources (HTTP connection pools,
	// background goroutines). Subsequent calls return ErrClientClosed.
	// Implementations MUST honour ctx during long teardowns.
	Close(ctx context.Context) error
}

// Driver is the unexported-by-naming surface every concrete driver
// implements. Identical shape to `LLMClient` minus the contract that
// the safety net has already run. `Open` wraps a `Driver` in a
// `safetyClient` so the safety pass is mandatory by construction.
//
// Driver authors implement this; callers consume `LLMClient`.
//
//nolint:revive // intentional naming — "Driver" parallels memory/state/artifact drivers.
type Driver interface {
	// Complete receives a `CompleteRequest` whose messages have
	// ALREADY passed the safety net (`enforceContextSafety`): no raw
	// heavy content survived, the token-budget guard fired or
	// passed, oversize `DataURL` content has been materialized to
	// `Artifact` form. The driver translates the request into its
	// provider's wire shape and returns the typed response.
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
	// Close mirrors `LLMClient.Close`. Idempotent; second call is a
	// no-op (returns nil).
	Close(ctx context.Context) error
}

// CompleteRequest is the LLM-call payload. Settled in RFC §6.5;
// shaped by D-021 (multimodal sum-type), D-026 (safety-net
// invariants).
//
// `Messages` is the chat thread — role + content only. The system /
// user / assistant roles are the entire vocabulary; tool-result
// rendering happens at the `ObservationRenderer` layer as user-role
// messages (RFC §6.4 + brief 07 §5).
//
// `ResponseFormat` is an optional structured-output hint. `nil` means
// "plain text"; `json_object` requests provider JSON mode;
// `json_schema` carries a caller-supplied JSON Schema. Phase 35 owns
// the per-provider downgrade chain `json_schema → json_object → text`.
//
// `Stream` + `OnContent` / `OnReasoning` cooperate: when `Stream` is
// true, the driver invokes the callbacks for each delta. `OnReasoning`
// fires only for thinking-class providers that expose a separate
// reasoning channel (`o1`, `o3`, `deepseek-reasoner`, etc.).
//
// `Temperature` / `MaxTokens` / `Stops` map directly onto provider
// sampler controls. Pointer types (`*float32`, `*int`) distinguish
// "unset (use provider default)" from "set to zero".
//
// `ReasoningEffort` is a request-level hint mapped to per-provider
// reasoning controls (bifrost's `ChatReasoning`). `""` means "do not
// touch the provider default."
//
// `Extra` is provider-passthrough sanitized by Phase 34's correction
// layer. Phase 32 stores the field but does not interpret it.
type CompleteRequest struct {
	Model           string
	Messages        []ChatMessage
	ResponseFormat  *ResponseFormat
	Stream          bool
	OnContent       func(delta string, done bool)
	OnReasoning     func(delta string, done bool)
	Temperature     *float32
	MaxTokens       *int
	Stops           []string
	ReasoningEffort ReasoningEffort
	Extra           map[string]any
}

// CompleteResponse is the LLM-call return shape.
//
// `Content` is the full assembled assistant message — for streaming
// calls the driver concatenates `OnContent` deltas into `Content`
// before returning. The runtime parses `Content` into a
// `PlannerAction` per brief 07; the LLM never emits provider-native
// tool calls.
//
// `Cost` + `Usage` propagate the provider's reported figures.
// Governance (Phase 36a/36b) subscribes to `llm.cost.recorded` events
// emitted by the runtime when a `Complete` returns; the event payload
// re-stamps these shapes.
type CompleteResponse struct {
	Content string
	Cost    Cost
	Usage   Usage
}

// Role is the chat-message role. Settled at the four canonical
// values; `RoleTool` is the in-Harbor convention for the user-role
// rendering of tool observations (brief 07 §5 — the rendering itself
// happens at `ObservationRenderer`, not here; this constant exists
// so callers that construct an explicit user-message describing a
// tool result can label it for clarity).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool — semantically a user-role observation; reserved so
	// downstream tooling (Console traces, audit logs) can distinguish.
	RoleTool Role = "tool"
)

// ChatMessage is one entry in the chat thread.
//
// `Content` is a sum-type: exactly one of `Text` or `Parts` is set.
// `Text` is the common case (text-only conversation). `Parts` is set
// when the message carries multimodal content. `Name` is optional —
// used by some providers for participant naming.
type ChatMessage struct {
	Role    Role
	Content Content
	Name    *string
}

// Content is the multimodal sum-type. Exactly one of `Text` or
// `Parts` must be set; both-set and both-nil are invalid and rejected
// by the safety net with `ErrInvalidContent`.
type Content struct {
	Text  *string
	Parts []ContentPart
}

// PartType discriminates a `ContentPart`.
type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	PartAudio PartType = "audio"
	PartFile  PartType = "file"
)

// ContentPart is one element of a multimodal `Content.Parts` slice.
// Exactly one of `Text` / `Image` / `Audio` / `File` is set per the
// `Type` discriminator.
type ContentPart struct {
	Type  PartType
	Text  string     // when Type == PartText
	Image *ImagePart // when Type == PartImage
	Audio *AudioPart // when Type == PartAudio
	File  *FilePart  // when Type == PartFile
}

// ImagePart is a multimodal image input.
//
// Exactly one of `URL` / `DataURL` / `Artifact` is set. `URL` is a
// provider-fetchable remote URL. `DataURL` is an inline
// `data:image/...;base64,...` payload — above the heavy-output
// threshold the runtime materializes it to `Artifact`. `Artifact`
// is the canonical Harbor reference (D-022).
//
// `MIME` is the image MIME type (`image/jpeg`, `image/png`,
// `image/webp`, ...). `Detail` is a provider hint (`low` / `high` /
// `auto`); empty string means "use provider default."
type ImagePart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
	Detail   string
}

// AudioPart is a multimodal audio input. Same supply forms as
// `ImagePart`; `MIME` is the audio MIME type.
type AudioPart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
}

// FilePart is a multimodal file input. Same supply forms as
// `ImagePart`; `MIME` is the document MIME type. `Filename` is a
// hint shown to the model when the provider supports it.
type FilePart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
	Filename string
}

// ResponseFormatKind discriminates a `ResponseFormat`.
type ResponseFormatKind string

const (
	// FormatText — no structured-output constraint. Default when
	// `CompleteRequest.ResponseFormat` is nil.
	FormatText ResponseFormatKind = "text"
	// FormatJSONObject — provider's "JSON mode" (free-form JSON).
	FormatJSONObject ResponseFormatKind = "json_object"
	// FormatJSONSchema — caller-supplied JSON Schema (strict mode
	// when the provider exposes it).
	FormatJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat is the optional structured-output hint on
// `CompleteRequest`. `nil` means "plain text" (equivalent to
// `Kind: FormatText`).
//
// Phase 35 owns the per-provider downgrade chain
// `json_schema → json_object → text` on `invalid_json_schema` errors;
// Phase 32 stores the field and the safety-net pass treats the JSON
// schema bytes as opaque metadata (no token-estimate contribution).
type ResponseFormat struct {
	Kind       ResponseFormatKind
	JSONSchema json.RawMessage
}

// ReasoningEffort hints at provider-side thinking budget. Empty
// string means "use provider default" (DO NOT touch the request).
type ReasoningEffort string

const (
	ReasoningOff    ReasoningEffort = "off"
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// Cost is the provider-reported cost breakdown. Values are USD.
// Fields are zero when the provider doesn't report a category.
//
// Governance (Phase 36a) subscribes to `llm.cost.recorded` events to
// drive per-identity accumulators; Phase 36a's payload re-stamps
// these fields.
type Cost struct {
	InputTokensCost     float64
	OutputTokensCost    float64
	ReasoningTokensCost float64
	TotalCost           float64
	Currency            string // "USD" canonical; reserved for future multi-currency
}

// Usage is the provider-reported token usage.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	TotalTokens      int
	LatencyMS        int64
	// ProviderExtras — opaque provider-specific bag (e.g. cache
	// hit/miss). Phase 32 does not interpret these fields; Phase 34+
	// may read them for correction-layer decisions.
	ProviderExtras map[string]string
}

// ArtifactStub is the model-agnostic JSON shape the LLM sees in
// place of heavy content during prompt assembly (RFC §6.5, D-026).
// The same shape is used whether the substituted content originated
// from a tool result, a memory turn, or a multimodal input.
//
// Operators can override `Summary` per-producer; the rest is
// runtime-stamped at materialization time. The stub's JSON
// rendering is byte-stable across providers — no per-provider
// swapping.
//
// JSON shape (omitempty on optional fields, no extra fields):
//
//	{"artifact_ref":"ref-abc-def","mime":"image/png","size_bytes":65536,
//	 "hash":"sha256:...","summary":"User-uploaded screenshot at turn 3",
//	 "fetch":{"tool":"artifact.fetch","id":"ref-abc-def"}}
type ArtifactStub struct {
	Ref       string     `json:"artifact_ref"`
	MIME      string     `json:"mime"`
	SizeBytes int64      `json:"size_bytes"`
	Hash      string     `json:"hash,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Fetch     *StubFetch `json:"fetch,omitempty"`
}

// StubFetch is the optional pointer-to-tool hint on an
// `ArtifactStub`. When set, an LLM that wants the bytes knows which
// Harbor tool to call (and with which artifact ID).
type StubFetch struct {
	Tool string `json:"tool"`
	ID   string `json:"id"`
}

// MarshalJSON ensures the canonical render of an `ArtifactStub` —
// stable field order, `omitempty` honored, no extra fields. The
// runtime's `ObservationRenderer` and the safety-net materialization
// both go through this method, so producers and the LLM-side audit
// see byte-identical output.
//
// Implemented explicitly (rather than relying on Go's default struct
// marshaling) so the contract is stable across Go version field-
// ordering changes.
func (s ArtifactStub) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"artifact_ref": s.Ref,
		"mime":         s.MIME,
		"size_bytes":   s.SizeBytes,
	}
	if s.Hash != "" {
		out["hash"] = s.Hash
	}
	if s.Summary != "" {
		out["summary"] = s.Summary
	}
	if s.Fetch != nil {
		out["fetch"] = s.Fetch
	}
	return json.Marshal(out)
}

// ModelProfile carries per-model knobs. Keyed by canonical model
// name in `LLMConfig.ModelProfiles`. Phase 32 ships the shape +
// `ContextWindowTokens` + `TokenEstimator` consumers; Phase 33+
// consume the rest.
type ModelProfile struct {
	// ContextWindowTokens is the model's hard input-token cap.
	// Required (> 0); the safety net's token-budget guard uses it.
	ContextWindowTokens int
	// TokenEstimator selects the estimator the safety net runs.
	// "" / "chars_div_4" — default chars/4 + role-overhead.
	// Phase 33+ may register tiktoken-equivalent estimators by name.
	TokenEstimator string
	// JSONSchemaMode — Phase 35 reads ("native" / "tools" /
	// "prompted"); Phase 32 stores opaque.
	JSONSchemaMode string
	// DefaultMaxTokens — Phase 36b's identity-tier override target.
	DefaultMaxTokens *int
	// ReasoningEffort — request-level default; req.ReasoningEffort
	// overrides per call.
	ReasoningEffort ReasoningEffort
	// CostOverrides — per-1M-token rates when the provider doesn't
	// report cost (some OpenRouter routes don't). Phase 36a reads.
	CostOverrides *CostTable
}

// CostTable carries fallback per-1M-token rates. Used when the
// provider's response doesn't include cost. Phase 36a consumes.
type CostTable struct {
	InputPer1M     float64
	OutputPer1M    float64
	ReasoningPer1M float64
	Currency       string // "USD" canonical
}

// HasIdentity reports whether `ctx` carries a complete Harbor
// identity. The LLM-client edge MUST validate this before invoking
// any driver — the runtime fails closed on missing identity
// (AGENTS.md §6 rule 9, AGENTS.md §13 forbidden-practices).
//
// Used by `safetyClient.Complete`; exposed so test helpers can pin
// the check at the call site.
func HasIdentity(ctx context.Context) bool {
	_, ok := identity.From(ctx)
	if ok {
		return true
	}
	_, ok = identity.QuadrupleFrom(ctx)
	return ok
}

// defaultRequestTimeout is the safety-net's fallback per-call
// timeout when the caller's ctx has no deadline and no per-call
// timeout configured. Conservative (5 min) — high enough for long
// streaming generations but bounded so a runaway never wedges the
// runtime.
const defaultRequestTimeout = 5 * time.Minute
