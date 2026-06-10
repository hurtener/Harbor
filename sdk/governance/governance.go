// Package governance is the public SDK facade over Harbor's
// internal/governance package — per-identity cost ceilings, rate
// limits, and MaxTokens caps composed as an LLM-client wrapper
// (RFC §3.6, §6.15; D-204/D-206). Alias-based re-exports only: no
// behavior lives here. Added in Phase 112b: the headless-embedding
// recipe's multi-stack governance path (D-198) is consumer-facing and
// flushed the names out. The process-global SetFactory hook, the
// per-policy enforcer constructors, and the event payload structs are
// deliberately private — embedders compose per stack via
// NewSubsystemFromConfig + Wrap; the single-stack path is config-driven
// through sdk/assemble.
package governance

import (
	internal "github.com/hurtener/Harbor/internal/governance"
)

// Enforcement vocabulary — aliases of the internal types.
type (
	// Subsystem is the PreCall/PostCall enforcement pair the wrapper
	// drives around every LLM completion.
	Subsystem = internal.Subsystem
	// Config is the operator-supplied governance shape; zero-value =
	// latent (no enforcement).
	Config = internal.Config
	// TierConfig is one tier's policy bundle (cost ceiling, rate
	// limit, MaxTokens cap); each zero field stays latent.
	TierConfig = internal.TierConfig
	// RateLimitConfig is the per-(identity, model) token-bucket shape.
	RateLimitConfig = internal.RateLimitConfig
	// TierResolver maps an identity to a tier name (pure +
	// deterministic, per its contract).
	TierResolver = internal.TierResolver
	// Clock is governance's injectable time source.
	Clock = internal.Clock
	// RealClock is the production time source.
	RealClock = internal.RealClock
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrBudgetExceeded — PreCall blocked on the cost ceiling.
	ErrBudgetExceeded = internal.ErrBudgetExceeded
	// ErrRateLimited — PreCall blocked on the rate limit.
	ErrRateLimited = internal.ErrRateLimited
	// ErrMaxTokensExceeded — PreCall blocked on the per-call cap.
	ErrMaxTokensExceeded = internal.ErrMaxTokensExceeded
	// ErrIdentityRequired — the request reached governance without a
	// complete identity (fails closed).
	ErrIdentityRequired = internal.ErrIdentityRequired
)

// NewSubsystemFromConfig composes the enforcement subsystem (MaxTokens
// → rate limit → cost ceiling) for ONE stack; returns (nil, nil) when
// the tiers are empty (the sanctioned latent default) and fails loudly
// on tiers without store/bus.
var NewSubsystemFromConfig = internal.NewSubsystemFromConfig

// ConfigFromOperator projects the operator config block
// (config.GovernanceConfig) onto the governance Config shape.
var ConfigFromOperator = internal.ConfigFromOperator

// Wrap composes governance around an LLM client; governance stays the
// outermost wrapper (D-043).
var Wrap = internal.Wrap
