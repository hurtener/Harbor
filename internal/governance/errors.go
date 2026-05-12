package governance

import "errors"

// Sentinel errors. Callers compare via errors.Is.
//
// Each sentinel maps 1:1 to a `governance.*` event type — code that
// catches one of these knows which policy fired and can route the
// outcome (e.g. ErrBudgetExceeded → pause/resume primitive in Wave 9+).
var (
	// ErrBudgetExceeded — Phase 36a. PreCall blocked because the
	// (identity, tier) accumulator hit or exceeded the configured
	// BudgetCeilingUSD. Wraps with the identity + total + ceiling so
	// log handlers can render the failure deterministically.
	ErrBudgetExceeded = errors.New("governance: budget ceiling exceeded")

	// ErrRateLimited — Phase 36b. PreCall blocked because the per-
	// (identity, model) token bucket underflowed the requested drain.
	ErrRateLimited = errors.New("governance: rate-limited")

	// ErrMaxTokensExceeded — Phase 36b. PreCall blocked because the
	// request's MaxTokens exceeded the identity tier's cap.
	ErrMaxTokensExceeded = errors.New("governance: per-call MaxTokens exceeded")

	// ErrIdentityRequired — the request reached governance with a
	// missing or incomplete identity. AGENTS.md §6 rule 9 mandates a
	// fail-closed; governance enforces here.
	ErrIdentityRequired = errors.New("governance: identity required")

	// ErrStateUnavailable — a StateStore read failed during PreCall.
	// AGENTS.md §13 forbids silent permits on read failure; the
	// wrapper returns this error rather than fall through to a
	// "permit by default."
	ErrStateUnavailable = errors.New("governance: state store unavailable")

	// ErrClosed — the Subsystem (or its underlying StateStore) was
	// closed. Subsequent calls fail loud.
	ErrClosed = errors.New("governance: subsystem closed")

	// ErrInvalidConfig — operator config did not validate at
	// construction. Used by `NewCostAccumulator` / `NewRateLimiter` /
	// `NewMaxTokensEnforcer`.
	ErrInvalidConfig = errors.New("governance: invalid config")
)
