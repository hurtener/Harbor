// Package llm defines Harbor's LLM-client interface and the
// runtime-wide invariants that guard every `Complete` call.
//
// The interface is **one method**, `Complete(ctx, req) (resp, error)`
// (RFC §6.5). Tool dispatch is the runtime's job (RFC §6.4
// "code-level tool calling"); the LLM client is reduced to a JSON-
// producing chat-completion adapter. Provider-native tool-calling
// shapes (the `tools=` request parameter, the `tool_choice=` mode
// selector, OpenAI's `function_call`, Anthropic's `tool_use` blocks,
// Gemini's function-calling protocol, etc.) never appear in this
// package — the static guard in the package's static smoke guard enforces
// the boundary by greppping for the canonical symbol names.
//
// The message envelope is provider-agnostic: `ChatMessage.Content`
// is a sum-type that carries either `Text *string` (the common case)
// or `Parts []ContentPart` for multimodal input.
// Multimodal parts (`ImagePart`, `AudioPart`, `FilePart`) each carry
// one of three supply forms — `URL`, `DataURL`, or `Artifact` — and
// the runtime auto-materializes inline `DataURL` content above the
// heavy-output threshold into `ArtifactRef`s before persistence and
// emit.
//
// **Context-window safety net.** Every `Complete` call routes
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
// Concurrent-reuse contract: one `LLMClient` is safe to
// share across N concurrent goroutines. Mutable state on the client
// (or the `Driver`) is forbidden; per-call state lives in `ctx` and
// the request value. The package-level `concurrent_test.go` pins
// this with N=128 invocations under `-race`.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// LLMClient is the single contract callers depend on. ONE method.
// Streaming is signalled via `req.Stream` + `req.OnContent` /
// `req.OnReasoning`; cancellation flows through `ctx`. The runtime
// owns prompt construction, tool semantics, parsing, and parallel
// dispatch — see RFC §6.4.
//
// Implementations MUST be safe for N concurrent goroutines against a
// single shared instance.
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
// shaped by the multimodal sum-type and safety-net
// invariants).
//
// `Messages` is the chat thread — role + content only. The system /
// user / assistant roles are the entire vocabulary; tool-result
// rendering happens at the `ObservationRenderer` layer as user-role
// messages (RFC §6.4).
//
// `ResponseFormat` is an optional structured-output hint. `nil` means
// "plain text"; `json_object` requests provider JSON mode;
// `json_schema` carries a caller-supplied JSON Schema. Harbor owns
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
// reasoning controls (bifrost's `ChatReasoning`). `""` normally permits the
// corrections layer to inherit the selected model profile; combine it with
// `ReasoningEffortExplicit=true` to omit provider controls without inheriting.
//
// `Extra` is provider-passthrough sanitized by the correction
// layer. Harbor stores the field but does not interpret it.
type CompleteRequest struct {
	Model string
	// ExternalGrant is an optional coordinator-signed execution grant. The
	// grant wrapper verifies it before this request reaches a provider. It is
	// intentionally not interpreted by individual drivers.
	ExternalGrant   *ExternalGrant
	Messages        []ChatMessage
	ResponseFormat  *ResponseFormat
	Stream          bool
	OnContent       func(delta string, done bool)
	OnReasoning     func(delta string, done bool)
	Temperature     *float32
	MaxTokens       *int
	Stops           []string
	ReasoningEffort ReasoningEffort
	// ReasoningEffortExplicit distinguishes an explicitly omitted provider
	// control from the ordinary empty value that inherits the selected model
	// profile's default in the corrections layer. When true with an empty
	// ReasoningEffort, the request reaches the driver with no reasoning control.
	ReasoningEffortExplicit bool
	Extra                   map[string]any
	// Validator is the caller-supplied post-response
	// validation hook. When non-nil, the retry wrapper invokes it
	// after each successful `Complete`; a non-nil return triggers a
	// corrective re-ask bounded by `ModelProfile.MaxRetries`. The
	// validator is opaque to the wrapper — return any error type;
	// the wrapper truncates and includes its `Error()` in the retry
	// sub-prompt + the `llm.retry_with_feedback` event payload.
	//
	// `nil` Validator (the default) disables the retry loop entirely;
	// the wrapper is a no-op pass-through. Validators MUST be safe for
	// concurrent invocation against the same compiled artifact (the
	// wrapper itself enforces the concurrent-reuse contract; the validator runs once per call).
	Validator func(CompleteResponse) error

	// RebuildMessages reconstructs this request's messages after the runtime
	// installs a context checkpoint. It is used only by request preparation,
	// before provider work, and never serialized or retained by the driver.
	RebuildMessages func() ([]ChatMessage, error) `json:"-"`

	// Tools is the per-turn tool catalog. When
	// nil the driver calls the provider without the tool-calling
	// block (text-only completion — preserves non-React planner
	// behavior).
	Tools []ToolDeclaration
	// ToolChoice is the per-provider tool-choice
	// passthrough. "" means "do not emit a tool_choice field"; "auto"
	// lets the provider decide; "required" forces the model to emit at
	// least one tool call; "none" suppresses tool calls entirely.
	ToolChoice string
	// ParallelToolCalls is the per-turn knob for
	// parallel function-calling (default true for supporting providers;
	// bifrost maps it per provider). The planner sets this per the
	// operator's yaml knob + the runloop executor's capability signal.
	ParallelToolCalls bool

	// modelProfile is a trusted, request-local technical descriptor attached by
	// the provider-route selector. It is deliberately unexported: callers must
	// not be able to smuggle a profile into the model-sensitive wrapper chain.
	// When absent, wrappers use the static ConfigSnapshot profile as the legacy
	// fallback.
	modelProfile      *ModelProfile
	modelProfileModel string
}

// CompleteResponse is the LLM-call return shape.
//
// `Content` is the full assembled assistant message — for streaming
// calls the driver concatenates `OnContent` deltas into `Content`
// before returning. The runtime parses `Content` into a
// `PlannerAction`; the LLM never emits provider-native
// tool calls.
//
// `ToolCalls` carries provider-validated
// structured tool-call entries. When non-empty, the planner reads
// ToolCalls as its primary decision discriminator (native tool-calling
// path). Empty for text-only responses and for providers without
// native tool-calling support.
//
// `Reasoning` carries the provider-side thinking trace (Anthropic
// extended thinking, OpenAI o-series, DeepSeek native, Gemini
// `thought:true` parts) normalised by the driver. It is the canonical
// captured trace for BOTH unary and streaming calls — distinct from
// the per-delta `OnReasoning` streaming callback, which exists for
// live UX. Empty when the provider did not surface reasoning, or when
// the driver does not read a reasoning channel. Reasoning is captured
// content, NOT replayed into prompts: the planner persists it on
// `trajectory.Step.ReasoningTrace` and only re-injects it when an
// operator opts into replay (RFC §6.2 + §6.5).
//
// `Cost` + `Usage` propagate the provider's reported figures. The
// mandatory safety wrapper emits an observability-only
// `llm.cost.recorded` event re-stamping these shapes once per
// driver-level completion (governance accounting is in-band synchronous,
// not a bus subscriber).
type CompleteResponse struct {
	// FinishReason is the selected completion's provider-normalized termination
	// reason (for example "stop" or "length"). Empty means not reported, never
	// an inferred successful stop. Maintenance consumers reject known truncation.
	FinishReason string
	Content      string
	ToolCalls    []ToolCallStructured
	Reasoning    string
	Cost         Cost
	Usage        Usage
}

// ToolCallStructured is a provider-validated tool-call entry (
// ). Carries the provider-assigned call ID (round-trips
// on `ChatMessage.ToolCallID` when the result is threaded back into
// the next turn), the tool name (matches `tools.Tool.Name`), and
// provider-validated JSON args.
//
// `Index` is the per-response position of this tool call (0-based) and
// is the load-bearing discriminator for streaming-delta assembly: per
// the OpenAI streaming spec, tool-call args arrive across multiple
// SSE chunks. The first delta carries `ID + Name`; subsequent deltas
// for the SAME tool call carry empty ID + null Name and an args
// FRAGMENT to be concatenated onto the prior args. The drivers key
// on Index to merge fragments correctly; without it, providers like
// Amazon Bedrock (which streams args one short fragment at a time)
// produce a trajectory full of half-built ToolCalls. Defaults to 0
// for non-streaming responses + tests; the driver layer is the
// source of truth.
type ToolCallStructured struct {
	ID    string
	Name  string
	Args  json.RawMessage
	Index uint16
}

// ToolDeclaration is the per-turn tool declarator the LLM sees (
// ). Carries the tool name, operator-facing description,
// and the args JSON Schema.
type ToolDeclaration struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Role is the chat-message role. Settled at the four canonical
// values; `RoleTool` is the in-Harbor convention for the user-role
// rendering of tool observations (the rendering itself
// happens at `ObservationRenderer`, not here; this constant exists
// so callers that construct an explicit user-message describing a
// tool result can label it for clarity).
type Role string

// The Role values for a chat message.
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
	// ToolCallID is the provider-assigned
	// tool-call identifier carried on RoleTool messages. Rendered
	// as the native tool-result role with matching call ID when
	// the provider supports it; falls back to user-role rendering
	// on providers without native tool-result roles.
	ToolCallID *string
	// ToolCalls is the per-message structured
	// tool-call slice carried on RoleAssistant messages that replay
	// a prior planner step's CallTool emission into the next turn's
	// thread. When non-empty, the bifrost translator emits an
	// assistant message with the provider-native `tool_calls`
	// block (OpenAI / Anthropic / Gemini all consume this shape).
	// The matching tool result is threaded back via a sibling
	// RoleTool message whose `ToolCallID` matches `ToolCalls[i].ID`.
	// Empty for every non-assistant message and for assistant
	// messages whose content is the model's final answer.
	ToolCalls []ToolCallStructured
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

// The PartType values, one per multimodal content shape.
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
// is the canonical Harbor reference.
//
// `MIME` is the image MIME type (`image/jpeg`, `image/png`,
// `image/webp`, ...). `Detail` is a provider hint (`low` / `high` /
// `auto`); empty string means "use provider default."
//
// `ProviderNative` and `ProviderFileID` are additive provider-native
// upload fields (the `provider_native` attachment disposition,
// RFC §6.5): they never replace the URL/DataURL/Artifact sum.
// `ProviderNative` asks the driver to hand the part's content to the
// provider's own understanding by uploading it and rewriting the part
// to an opaque provider file reference; any `CompleteRequest` builder
// may set it directly — no planner, config, or Protocol required.
// `ProviderFileID` carries the resulting opaque reference; a part
// arriving with it pre-set skips the upload. A part whose only
// content is a `ProviderFileID` is legal over-threshold — it carries
// no inline bytes, so the context-leak edge guard does not fire on
// it.
type ImagePart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
	Detail   string
	// ProviderNative requests the driver-internal provider upload for
	// this part. See the struct doc.
	ProviderNative bool
	// ProviderFileID is the opaque provider file reference produced by
	// the upload (or pre-set by the caller to reuse a known file).
	ProviderFileID string
}

// AudioPart is a multimodal audio input. Same supply forms as
// `ImagePart`; `MIME` is the audio MIME type. `ProviderNative` /
// `ProviderFileID` carry the provider-native upload contract — see
// the `ImagePart` doc.
type AudioPart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
	// ProviderNative requests the driver-internal provider upload for
	// this part. See the `ImagePart` doc.
	ProviderNative bool
	// ProviderFileID is the opaque provider file reference produced by
	// the upload (or pre-set by the caller to reuse a known file).
	ProviderFileID string
}

// FilePart is a multimodal file input. Same supply forms as
// `ImagePart`; `MIME` is the document MIME type. `Filename` is a
// hint shown to the model when the provider supports it.
// `ProviderNative` / `ProviderFileID` carry the provider-native
// upload contract — see the `ImagePart` doc.
type FilePart struct {
	URL      string
	DataURL  string
	Artifact *ArtifactStub
	MIME     string
	Filename string
	// ProviderNative requests the driver-internal provider upload for
	// this part. See the `ImagePart` doc.
	ProviderNative bool
	// ProviderFileID is the opaque provider file reference produced by
	// the upload (or pre-set by the caller to reuse a known file).
	ProviderFileID string
	// DocumentType disambiguates a structured document for providers
	// with native document understanding — a short lowercase token
	// derived from the MIME subtype (`pdf`, `csv`, ...). Optional;
	// empty for non-document content.
	DocumentType string
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
// Harbor owns the per-provider downgrade chain
// `json_schema → json_object → text` on `invalid_json_schema` errors;
// Harbor stores the field and the safety-net pass treats the JSON
// schema bytes as opaque metadata (no token-estimate contribution).
type ResponseFormat struct {
	Kind       ResponseFormatKind
	JSONSchema json.RawMessage
}

// ReasoningEffort hints at provider-side thinking budget. Empty normally lets
// the corrections layer apply the selected profile; with
// CompleteRequest.ReasoningEffortExplicit it instead omits provider controls.
type ReasoningEffort string

// The ReasoningEffort levels, ascending. The empty string (not listed
// here) is resolved according to CompleteRequest.ReasoningEffortExplicit.
const (
	ReasoningOff    ReasoningEffort = "off"
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// EffectiveReasoningEffort returns the canonical reasoning control carried by
// a request. The corrections layer may move a validated effort into the
// reserved Extra key for provider-specific routing; drivers must still honor
// that value rather than silently dropping it. An invalid reserved value is a
// malformed request and fails closed.
func EffectiveReasoningEffort(req CompleteRequest) (ReasoningEffort, error) {
	effort := req.ReasoningEffort
	if effort == "" {
		raw, ok := req.Extra["reasoning_effort"]
		if !ok {
			return "", nil
		}
		var stringValue bool
		effortString, stringOK := raw.(string)
		if stringOK {
			effort = ReasoningEffort(effortString)
			stringValue = true
		}
		if !stringValue {
			return "", fmt.Errorf("%w: Extra[reasoning_effort] must be a string", ErrProviderRouteInvalid)
		}
	}
	if !validProviderReasoningEffort(effort) || effort == "" {
		return "", fmt.Errorf("%w: reasoning_effort=%q is unknown", ErrProviderRouteInvalid, effort)
	}
	return effort, nil
}

// OutputMode selects the request-shaping strategy for structured
// output (RFC §6.5). Three modes:
//
//   - `OutputModeNative` — pass `FormatJSONSchema` through unchanged.
//     The provider validates against the schema natively. Default for
//     OpenAI / Anthropic / Google.
//
//   - `OutputModeTools` — encode the schema as a *Harbor-side prompted*
//     envelope where the LLM is asked to emit
//     `{"name":"respond_with","arguments":{...}}` as plain output. The
//     runtime parses that locally. Used as a fallback for providers
//     without native `json_schema` support.
//
//     IMPORTANT: this is NOT a passthrough to provider-native
//     tool-calling APIs (`tools=` / `tool_choice=` / `function_call` /
//     `tool_use`). Harbor's runtime owns tool dispatch (RFC §6.4 /
//     by design); `OutputModeTools` is purely a prompted-output
//     technique. The package's static smoke guard enforces this
//     boundary.
//
//   - `OutputModePrompted` — coerce `FormatJSONObject` and inline the
//     schema as a system-prompt instruction. The LLM-side parse is
//     "produce a JSON object matching this schema." Default for NIM
//     / custom OpenAI-compatible / deepseek-reasoner.
//
// The downgrade chain runs `current → next` on `IsInvalidJSONSchemaError`
// failures, bounded at 3 total attempts (initial + 2 downgrades).
type OutputMode string

const (
	// OutputModeUnset is the zero value — operator did not declare the
	// mode. The downgrade wrapper applies the per-model-prefix default
	// (see `internal/llm/corrections.DefaultOutputModeFor`).
	OutputModeUnset OutputMode = ""
	// OutputModeNative — pass `FormatJSONSchema` through. Provider
	// enforces strict schema mode.
	OutputModeNative OutputMode = "native"
	// OutputModeTools — Harbor-side prompted envelope. NOT provider
	// tool-calling APIs.
	OutputModeTools OutputMode = "tools"
	// OutputModePrompted — `FormatJSONObject` + schema in system prompt.
	OutputModePrompted OutputMode = "prompted"
)

// Cost is the provider-reported cost breakdown. Values are USD.
// Fields are zero when the provider doesn't report a category.
//
// The `llm.cost.recorded` event re-stamps these fields for observability;
// governance accounting is in-band (it folds each call's Cost — and every
// intermediate retry / downgrade attempt via the per-call attempt-cost
// tap — synchronously), not a subscriber of that event.
type Cost struct {
	// ReportPresent means the driver received a normalized cost object, including
	// an explicitly zero object. False on legacy records means unknown, not free.
	// An SDK-calculated cost is not proof of a provider's final invoice.
	ReportPresent bool `json:",omitempty"`
	// Estimated identifies Harbor's configured price-table backfill.
	Estimated           bool `json:",omitempty"`
	InputTokensCost     float64
	OutputTokensCost    float64
	ReasoningTokensCost float64
	TotalCost           float64
	Currency            string // "USD" canonical; reserved for future multi-currency
}

// Usage carries normalized driver token counts and optional availability flags.
// Older drivers/records without flags have unknown provenance. Estimates never
// become provider reports merely because their counts are nonzero.
type Usage struct {
	// ReportPresent means the driver received a normalized usage object. It does
	// not assert that every optional per-token-category field was supplied.
	ReportPresent bool `json:",omitempty"`
	// Estimated marks Harbor's usage backfill rather than a driver report.
	Estimated bool `json:",omitempty"`
	// PromptDetailsPresent and CompletionDetailsPresent record normalized detail
	// object presence. The pinned SDK erases individual cache-field presence:
	// a zero cache count still does not prove a measured miss, even when the
	// prompt-details object exists (it may contain only audio/text counts).
	PromptDetailsPresent     bool `json:",omitempty"`
	CompletionDetailsPresent bool `json:",omitempty"`
	PromptTokens             int
	CompletionTokens         int
	ReasoningTokens          int
	TotalTokens              int
	LatencyMS                int64
	// CacheReadTokens is the count of PromptTokens served from the
	// provider's prompt cache — a subset of PromptTokens, not additional
	// tokens. Zero when the provider/response reports no cache data.
	CacheReadTokens int
	// CacheWriteTokens is the count of PromptTokens newly written to the
	// provider's prompt cache on this call — a subset of PromptTokens, not
	// additional tokens. Zero when the provider/response reports no cache
	// data.
	CacheWriteTokens int
	// ProviderExtras — opaque provider-specific bag (e.g. cache
	// hit/miss). Harbor does not interpret these fields; later phases
	// may read them for correction-layer decisions.
	ProviderExtras map[string]string
}

// ArtifactStub is the model-agnostic JSON shape the LLM sees in
// place of heavy content during prompt assembly (RFC §6.5).
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
//	 "fetch":{"tool":"artifact_fetch","id":"ref-abc-def"}}
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
// name in `LLMConfig.ModelProfiles`. Harbor ships the shape +
// `ContextWindowTokens` + `TokenEstimator` consumers; later phases
// consume the rest.
type ModelProfile struct {
	// ContextWindowTokens is the model's hard input-token cap.
	// Required (> 0); the safety net's token-budget guard uses it.
	ContextWindowTokens int
	// TokenEstimator selects the estimator the safety net runs.
	// "" / "chars_div_4" — default chars/4 + role-overhead.
	// Later phases may register tiktoken-equivalent estimators by name.
	TokenEstimator string
	// JSONSchemaMode — legacy placeholder; the config loader
	// normalises this string into `OutputMode` at snapshot time.
	// Direct callers SHOULD set `OutputMode`; this field
	// is read only when `OutputMode` is `OutputModeUnset`.
	JSONSchemaMode string
	// OutputMode — Harbor-side structured-output strategy.
	// Drives the request-shaping in `internal/llm/output` and the
	// downgrade chain. See `OutputMode` constants for semantics.
	// Zero value (`OutputModeUnset`) falls back to the per-known-
	// provider default (see `corrections.DefaultOutputModeFor`).
	OutputMode OutputMode
	// DefaultMaxTokens — the identity-tier override target.
	DefaultMaxTokens *int
	// ReasoningEffort — request-level default applied by the
	// corrections layer (`corrections.Complete`) when the caller left
	// `CompleteRequest.ReasoningEffort` empty and did not mark that empty
	// value explicit; an explicit value or explicit omission overrides it.
	// Maps to the provider reasoning param (bifrost
	// `ChatReasoning.Effort`, or `Extra["reasoning_effort"]` under
	// `ReasoningRouteThinking`).
	ReasoningEffort ReasoningEffort
	// CostOverrides — per-1M-token rates when the provider doesn't
	// report cost (some OpenRouter routes don't). Harbor reads.
	CostOverrides *CostTable
	// Corrections — per-provider quirk flags consumed by the
	// `internal/llm/corrections` layer. Zero-valued struct means
	// "no corrections needed for this model"; the corrections layer
	// runs a no-op pass for default-shaped profiles.
	Corrections CorrectionsProfile
	// MaxRetries — caps the validator-driven corrective
	// re-asks performed by the retry wrapper. Zero (default) maps to
	// `DefaultMaxRetries` (1). A negative value is rejected at config
	// validation.
	MaxRetries int
}

// ProviderModelProfile is the bounded technical capability descriptor a
// provider-route resolver may return for its selected model. It is deliberately
// smaller than ModelProfile: route selection supplies model facts, while
// Harbor's request and operator policy remain authoritative for sampling,
// output limits, corrections, and retries.
//
// A nil ReasoningEffortLevels value means the resolver did not describe the
// supported levels. A non-nil empty slice explicitly means that the provider
// supports no reasoning levels; ReasoningOff remains universally valid.
type ProviderModelProfile struct {
	ContextWindowTokens   int
	MaxOutputTokens       int
	ReasoningEffort       ReasoningEffort
	ReasoningEffortLevels []ReasoningEffort
}

const (
	maxProviderModelContextTokens = 1 << 30
	maxProviderModelOutputTokens  = 1 << 30
)

// ValidateProviderModelProfile validates a route-supplied technical
// descriptor before it can enter any model-sensitive wrapper.
func ValidateProviderModelProfile(profile ProviderModelProfile) error {
	if profile.ContextWindowTokens <= 0 || profile.ContextWindowTokens > maxProviderModelContextTokens {
		return fmt.Errorf("%w: context_window_tokens=%d is outside the allowed range", ErrProviderRouteInvalid, profile.ContextWindowTokens)
	}
	if profile.MaxOutputTokens <= 0 || profile.MaxOutputTokens > maxProviderModelOutputTokens {
		return fmt.Errorf("%w: max_output_tokens=%d is outside the allowed range", ErrProviderRouteInvalid, profile.MaxOutputTokens)
	}
	if profile.MaxOutputTokens > profile.ContextWindowTokens {
		return fmt.Errorf("%w: max_output_tokens=%d exceeds context_window_tokens=%d", ErrProviderRouteInvalid, profile.MaxOutputTokens, profile.ContextWindowTokens)
	}
	if !validProviderReasoningEffort(profile.ReasoningEffort) {
		return fmt.Errorf("%w: reasoning_effort=%q is unknown", ErrProviderRouteInvalid, profile.ReasoningEffort)
	}
	if profile.ReasoningEffort != "" && profile.ReasoningEffort != ReasoningOff && profile.ReasoningEffortLevels == nil {
		return fmt.Errorf("%w: default reasoning effort %q requires an explicit supported-level list", ErrProviderRouteInvalid, profile.ReasoningEffort)
	}
	if len(profile.ReasoningEffortLevels) > 4 {
		return fmt.Errorf("%w: reasoning_effort_levels has too many entries", ErrProviderRouteInvalid)
	}
	seen := make(map[ReasoningEffort]struct{}, len(profile.ReasoningEffortLevels))
	for _, level := range profile.ReasoningEffortLevels {
		if !validProviderReasoningEffort(level) || level == "" {
			return fmt.Errorf("%w: reasoning_effort_levels contains unknown level %q", ErrProviderRouteInvalid, level)
		}
		if _, ok := seen[level]; ok {
			return fmt.Errorf("%w: reasoning_effort_levels contains duplicate level %q", ErrProviderRouteInvalid, level)
		}
		seen[level] = struct{}{}
	}
	if profile.ReasoningEffort != "" && profile.ReasoningEffort != ReasoningOff && profile.ReasoningEffortLevels != nil {
		if _, ok := seen[profile.ReasoningEffort]; !ok {
			return fmt.Errorf("%w: default reasoning effort %q is not supported", ErrProviderRouteInvalid, profile.ReasoningEffort)
		}
	}
	return nil
}

// SupportsReasoningEffort reports whether a route descriptor accepts one
// explicit request-level reasoning value. The off value is a universal
// disable control, including when the provider advertises no reasoning list.
func (profile ProviderModelProfile) SupportsReasoningEffort(effort ReasoningEffort) bool {
	if effort == ReasoningOff {
		return true
	}
	if !validProviderReasoningEffort(effort) || profile.ReasoningEffortLevels == nil {
		return false
	}
	for _, supported := range profile.ReasoningEffortLevels {
		if supported == effort {
			return true
		}
	}
	return false
}

func validProviderReasoningEffort(effort ReasoningEffort) bool {
	switch effort {
	case "", ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh:
		return true
	default:
		return false
	}
}

func (profile ProviderModelProfile) modelProfile() ModelProfile {
	maxTokens := profile.MaxOutputTokens
	return ModelProfile{
		ContextWindowTokens: profile.ContextWindowTokens,
		DefaultMaxTokens:    &maxTokens,
		ReasoningEffort:     profile.ReasoningEffort,
	}
}

func cloneModelProfile(profile ModelProfile) ModelProfile {
	clone := profile
	if profile.DefaultMaxTokens != nil {
		maxTokens := *profile.DefaultMaxTokens
		clone.DefaultMaxTokens = &maxTokens
	}
	if profile.CostOverrides != nil {
		costs := *profile.CostOverrides
		clone.CostOverrides = &costs
	}
	return clone
}

func withTrustedModelProfile(req CompleteRequest, model string, profile ModelProfile) CompleteRequest {
	clone := cloneModelProfile(profile)
	req.modelProfile = &clone
	req.modelProfileModel = model
	return req
}

// EffectiveModelProfile returns the trusted per-request route descriptor when
// present, otherwise the configured static profile for req.Model. The route
// descriptor is request-local and cannot mutate the shared configuration.
func EffectiveModelProfile(req CompleteRequest, cfg ConfigSnapshot) (ModelProfile, bool) {
	if req.modelProfile != nil && req.modelProfileModel == req.Model {
		profile, ok := cfg.ModelProfiles[req.Model]
		if !ok {
			profile = ModelProfile{}
		}
		// The route descriptor owns technical context facts. Existing operator
		// policy on a configured model remains intact; a route default reasoning
		// value is used only when no static policy value exists.
		profile.ContextWindowTokens = req.modelProfile.ContextWindowTokens
		if routeMaxTokens := req.modelProfile.DefaultMaxTokens; routeMaxTokens != nil &&
			(profile.DefaultMaxTokens == nil || *profile.DefaultMaxTokens <= 0 || *profile.DefaultMaxTokens > *routeMaxTokens) {
			maxTokens := *routeMaxTokens
			profile.DefaultMaxTokens = &maxTokens
		}
		if profile.ReasoningEffort == "" {
			profile.ReasoningEffort = req.modelProfile.ReasoningEffort
		}
		return cloneModelProfile(profile), true
	}
	profile, ok := cfg.ModelProfiles[req.Model]
	if !ok {
		return ModelProfile{}, false
	}
	return cloneModelProfile(profile), true
}

// HasTrustedModelProfile reports whether req carries a route-bound profile for
// its current model. It is used by policy wrappers to avoid replacing the
// resolver's native request controls with a generic grant default.
func HasTrustedModelProfile(req CompleteRequest) bool {
	return req.modelProfile != nil && req.modelProfileModel == req.Model
}

// CorrectionsProfile carries the per-model quirk flags the
// `internal/llm/corrections` layer dispatches on. The types live in
// the `llm` package so the corrections sub-package can consume them
// without an import cycle (logic lives in `internal/llm/corrections`).
//
// Zero-valued struct means "no quirks declared for this model"; the
// corrections pass treats each field's zero value as the Harbor-default
// behaviour (no reorder, no schema mutation, OpenAI-style envelopes,
// usage backfill off).
//
// Per RFC §6.5: this is the operator-controlled surface
// for adapting Harbor's neutral `CompleteRequest` shape to per-provider
// expectations. The corrections layer is the ONLY consumer.
type CorrectionsProfile struct {
	// MessageOrdering controls how the request's chat-message slice
	// is reordered before reaching the driver. Default (zero value)
	// passes the slice through unchanged.
	MessageOrdering MessageOrderingPolicy
	// SchemaMode controls how the request's `ResponseFormat.JSONSchema`
	// bytes are mutated before reaching the driver. Default passes
	// the schema through unchanged.
	SchemaMode SchemaSanitizationMode
	// ReasoningEffortRouting controls whether `req.ReasoningEffort` is
	// translated to a provider-specific `Extra` key (thinking-class
	// models) or passed through as the top-level field (default).
	ReasoningEffortRouting ReasoningRouting
	// ResponseFormatShape controls the wire-shape translation of
	// `req.ResponseFormat`. Default emits the OpenAI envelope; other
	// values translate to per-provider envelopes (Anthropic tool-
	// schema, `json_only` for providers that reject `json_schema`).
	ResponseFormatShape ResponseFormatProfile
	// UsageBackfillEnabled, when true, makes the corrections layer
	// compute synthetic token counts (and, if `CostOverrides` is set,
	// synthetic costs) when the driver returns an all-zeros `Usage`.
	// Default false — the response surfaces zeros verbatim.
	UsageBackfillEnabled bool
}

// MessageOrderingPolicy enumerates the message-reordering modes the
// corrections layer supports. Operator-set in
// `ModelProfile.Corrections.MessageOrdering`.
type MessageOrderingPolicy string

const (
	// OrderingDefault passes the message slice through unchanged.
	OrderingDefault MessageOrderingPolicy = ""
	// OrderingSystemFirstStrict collapses all system-role messages
	// to the front of the slice and emits an alternating
	// user/assistant tail. Required by NIM and some OpenAI-compatible
	// proxies that reject mid-thread `system` messages.
	OrderingSystemFirstStrict MessageOrderingPolicy = "system_first_strict"
)

// SchemaSanitizationMode enumerates the JSON-Schema-mutation modes the
// `SchemaSanitizer` supports. Operator-set in
// `ModelProfile.Corrections.SchemaMode`.
type SchemaSanitizationMode string

const (
	// SchemaDefault passes the operator-supplied schema through
	// unchanged.
	SchemaDefault SchemaSanitizationMode = ""
	// SchemaOpenAIStrict adds `additionalProperties:false` and
	// `strict:true` at every nested object schema. OpenAI's
	// structured-output mode requires both fields; most schemas
	// produced by `tools.RegisterFunc[I, O]` omit them.
	SchemaOpenAIStrict SchemaSanitizationMode = "openai_strict"
	// SchemaPermissive strips `additionalProperties` and `strict`
	// fields wherever they appear. Some providers reject those keys.
	SchemaPermissive SchemaSanitizationMode = "permissive"
)

// ReasoningRouting enumerates the `ReasoningEffort` routing modes the
// corrections layer supports. Operator-set in
// `ModelProfile.Corrections.ReasoningEffortRouting`.
type ReasoningRouting string

const (
	// ReasoningRouteDefault passes the top-level
	// `req.ReasoningEffort` through to the driver unchanged.
	// Bifrost's `ChatReasoning.Effort` field consumes it.
	ReasoningRouteDefault ReasoningRouting = ""
	// ReasoningRouteThinking moves the effort hint from the
	// top-level field into `req.Extra["reasoning_effort"]`.
	// Thinking-class models (`o1`, `o3`, `deepseek-reasoner`)
	// interpret the hint via a provider-specific path that bifrost
	// passes through opaquely. The top-level field is cleared so the
	// regular reasoning channel is not used.
	ReasoningRouteThinking ReasoningRouting = "thinking_model"
)

// ResponseFormatProfile enumerates the `response_format` envelope
// shapes the corrections layer can emit. Operator-set in
// `ModelProfile.Corrections.ResponseFormatShape`.
type ResponseFormatProfile string

const (
	// ResponseFormatOpenAI emits the OpenAI envelope —
	// `{"type":"json_object"}` for `FormatJSONObject` and
	// `{"type":"json_schema","json_schema":{...}}` for
	// `FormatJSONSchema`. This is the default; bifrost's
	// `translateResponseFormat` already produces this shape, so a
	// `default`-profile model is a no-op in the corrections layer.
	ResponseFormatOpenAI ResponseFormatProfile = ""
	// ResponseFormatJSONOnly downgrades `FormatJSONSchema` to
	// `FormatJSONObject`. Used for providers that don't support
	// `json_schema` natively (e.g. some OpenRouter routes); the
	// schema is preserved as `Extra["schema_hint"]` so a prompted
	// fallback can reference it.
	ResponseFormatJSONOnly ResponseFormatProfile = "json_only"
	// ResponseFormatAnthropic packages the schema into Anthropic's
	// tool-schema-style envelope, surfaced in
	// `req.Extra["anthropic_tool_schema"]`. the bifrost
	// driver passes `Extra` opaquely; the Anthropic provider
	// converter consumes the key (or future logic does).
	ResponseFormatAnthropic ResponseFormatProfile = "anthropic"
)

// CostTable carries fallback per-1M-token rates. Used when the
// provider's response doesn't include cost. Harbor consumes.
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
