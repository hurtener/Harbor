package llm

import (
	"errors"
	"strings"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrUnknownDriver — Open was asked for a driver name no
	// registered factory handles. The error's message names the
	// registered drivers so misconfigurations are obvious (§4.4).
	ErrUnknownDriver = errors.New("llm: unknown driver")
	// ErrClientClosed — Complete called after Close. The wrapped
	// driver returns this; the safetyClient propagates it verbatim.
	ErrClientClosed = errors.New("llm: client is closed")
	// ErrIdentityMissing — Complete called with a ctx that does not
	// carry an `identity.Identity` (or `identity.Quadruple`).
	// AGENTS.md §6 rule 9 — identity is mandatory at every Harbor
	// boundary; the runtime fails closed.
	ErrIdentityMissing = errors.New("llm: identity missing from ctx")
	// ErrInvalidContent — a `ChatMessage.Content` is malformed: both
	// `Text` and `Parts` set, or neither, or a `ContentPart` whose
	// `Type` discriminator doesn't match its payload (e.g. Type=image
	// with `Image == nil`). The safety pass rejects loudly rather than
	// papering over the inconsistency.
	ErrInvalidContent = errors.New("llm: invalid message content")
	// ErrContextLeak — runtime-wide invariant violation (D-026). A
	// raw byte / string / DataURL ≥ heavy-output threshold survived
	// every producer's normalization step and reached the LLM-client
	// edge. The safety pass fails the request; the bus emits
	// `llm.context_leak` so operators can find the offending
	// producer.
	ErrContextLeak = errors.New("llm: raw heavy content reached LLM-client edge — D-026 violation")
	// ErrContextWindowExceeded — the token-budget guard fired (D-026).
	// The assembled `CompleteRequest`'s estimated token count is
	// within `ContextWindowReserve` of the model's configured
	// `ContextWindowTokens` cap. V1 fails loudly; auto-cascade is
	// post-V1 work — the planner is responsible for recovery (drop
	// older turns, summarize, etc.).
	ErrContextWindowExceeded = errors.New("llm: estimated tokens within reserve of model context window")
	// ErrInvalidConfig — `Open` called with a `ConfigSnapshot` that
	// fails structural validation (driver name empty, model profile
	// missing for the request's model, etc.). Distinct from
	// ErrUnknownDriver — that's a registry miss, this is a
	// configuration miss.
	ErrInvalidConfig = errors.New("llm: invalid configuration")
	// ErrUnsupportedModel — the safety net or driver hit a model
	// name with no matching `ModelProfile`. Required because the
	// token-budget guard depends on a profile's context-window cap.
	ErrUnsupportedModel = errors.New("llm: model has no configured ModelProfile")
	// ErrInvalidJSONSchema (Phase 35) — the provider returned a
	// `Complete` whose JSON output did not validate against the
	// requested schema (or rejected the schema itself at the wire
	// layer). The downgrade wrapper observes this via
	// `IsInvalidJSONSchemaError` and steps the request down the chain.
	// Drivers MAY wrap their provider-specific schema errors with this
	// sentinel; the classifier also matches a small allowlist of error
	// substrings to handle providers that surface only a free-form
	// `error` string.
	ErrInvalidJSONSchema = errors.New("llm: response failed JSON-schema validation")
	// ErrDowngradeExhausted (Phase 35) — the downgrade wrapper ran
	// every step in the chain and the inner call STILL produced
	// `ErrInvalidJSONSchema`. Surfaces with the wrapped chain history
	// so operators can correlate against `llm.mode_downgraded` events.
	ErrDowngradeExhausted = errors.New("llm: structured-output downgrade chain exhausted")
	// ErrRetryExhausted (Phase 36) — the retry wrapper exceeded the
	// per-model `MaxRetries` bound. Wraps the chain of validator
	// failures so operators can see why each attempt failed.
	ErrRetryExhausted = errors.New("llm: retry-with-feedback budget exhausted")
	// ErrValidationFailed (Phase 36) — surfaces when the validator
	// returns non-nil AND the retry wrapper is NOT registered (the
	// caller asked for validation without a wrapper to retry). The
	// wrapper-registered path uses the validator's own error verbatim
	// in `RetryWithFeedbackPayload.Reason`.
	ErrValidationFailed = errors.New("llm: response validator rejected output")
	// ErrOrphanToolCall — an assistant message with `ToolCalls` is
	// not followed by the corresponding `RoleTool` messages whose
	// `ToolCallID` matches each `ToolCalls[i].ID`. OpenAI's wire
	// spec requires the pairing; the safety pass rejects loudly so
	// the producer is forced to fix the upstream omission rather
	// than silently shipping an invalid wire shape.
	ErrOrphanToolCall = errors.New("llm: assistant message with ToolCalls is not followed by matching RoleTool messages")
)

// invalidJSONSchemaErrorMarkers is the small substring allowlist
// `IsInvalidJSONSchemaError` matches against. Providers surface
// schema-related failures inconsistently — some wrap a sentinel,
// others surface a free-form `error` field. The markers are
// case-insensitive and intentionally narrow so non-schema errors
// (timeouts, 5xx, auth) don't trigger false-positive downgrades.
var invalidJSONSchemaErrorMarkers = []string{
	"json_schema",
	"json schema",
	"invalid schema",
	"schema validation",
	"response_format",
	"response format",
	"structured output",
	"json mode",
	"json_object",
}

// IsInvalidJSONSchemaError reports whether `err` represents a
// schema-class failure that the Phase 35 downgrade chain should treat
// as a signal to step the request down to the next `OutputMode`.
//
// The classifier checks two paths:
//
//  1. `errors.Is(err, ErrInvalidJSONSchema)` — drivers / wrappers
//     that classify upstream errors and wrap with the sentinel.
//  2. A small case-insensitive substring scan against
//     `invalidJSONSchemaErrorMarkers`. This handles providers that
//     surface only a free-form error string.
//
// The substring allowlist is deliberately narrow to avoid false
// positives on transient / IO / auth failures. Returns false for nil.
func IsInvalidJSONSchemaError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInvalidJSONSchema) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range invalidJSONSchemaErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
