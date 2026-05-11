package llm

import "errors"

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
)
