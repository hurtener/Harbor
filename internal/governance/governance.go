// Package governance is Harbor's policy middleware between the runtime
// and the LLM-edge chain. It owns identity-scoped enforcement of cost
// ceilings (Phase 36a), per-call MaxTokens (Phase 36b), and rate limits
// (Phase 36b). The package composes OUTSIDE Phase 36's retry wrapper —
// see D-043 for the full chain order:
//
//	governance(retry(downgrade(corrections(safety(driver)))))
//
// Governance ships LATENT at V1 (per the Wave 7b scoping decision): the
// interface + math + events + persistence all ship and wire, but every
// enforcement path is operator-opt-in. With zero `Governance.IdentityTiers`
// configured (the loader's default), every `PreCall` permits and every
// `PostCall` is an accumulator update only — no ceilings fire, no events
// emit beyond the accumulator's own bookkeeping.
//
// Concurrent reuse (D-025): one Subsystem instance is safe to share
// across N concurrent goroutines. Per-key state lives in a `sync.Map` of
// `*identityState`; each `identityState` uses atomic primitives for its
// counters (lock-free CAS for monetary float64 sums; atomic for integer
// counts). Per-run scope flows via `ctx`; no mutable state on the
// Subsystem struct itself crosses run boundaries beyond the keyed
// per-identity caches.
package governance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// Subsystem is governance's enforcement seam — the interface
// `governance.Wrap` consumes to gate / observe an LLM call. Implementations
// MUST be safe for N concurrent invocations against a single shared
// instance (D-025).
//
// `PreCall` is invoked before the wrapped `LLMClient.Complete`. A non-nil
// return short-circuits the call: the wrapper returns the error directly
// and does NOT invoke the inner client. Implementations emit the
// corresponding `governance.*` event from PreCall on rejection.
//
// `PostCall` is invoked after the wrapped `LLMClient.Complete` returns,
// whether or not `callErr` is nil. It accumulates cost / token / latency
// state from `resp` and emits any observability events. A non-nil return
// from PostCall is an observability signal — it is logged at Warn level by
// the wrapper but does NOT replace the original call's `(resp, callErr)`
// outcome on the way back to the caller.
type Subsystem interface {
	PreCall(ctx context.Context, req llm.CompleteRequest) error
	PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error
}

// Config is the operator-supplied governance shape. Zero-value = latent
// (no enforcement). Operators populate `IdentityTiers` + `DefaultTier`
// (or a custom `Resolver`) to switch on per-policy enforcement.
type Config struct {
	// DefaultTier is the tier name applied to an identity that does
	// not match a custom Resolver mapping. Empty = no default tier =
	// no enforcement for unmatched identities (latent).
	DefaultTier string

	// IdentityTiers maps tier name → policy shape. A nil / empty map
	// disables all enforcement (latent default).
	IdentityTiers map[string]TierConfig

	// Resolver maps identity → tier name. nil = "use DefaultTier for
	// every identity." Operators with claim-based or session-attribute-
	// based tier assignment wire their resolver here.
	Resolver TierResolver

	// Clock is the time source for bucket-refill math. nil =
	// `time.Now` (the production default). Tests inject a fake clock.
	Clock Clock
}

// TierConfig is one tier's policy bundle. Each field zero = latent for
// that policy. An operator who wants to enforce cost only sets
// `BudgetCeilingUSD`; rate-limit + MaxTokens fields stay zero.
type TierConfig struct {
	// BudgetCeilingUSD is the per-identity (per tier) cost ceiling in
	// USD. PreCall blocks when the accumulator's total for the request's
	// identity meets or exceeds this. 0 = no ceiling.
	BudgetCeilingUSD float64

	// RateLimit is the per-(identity, model) token-bucket config.
	// Zero-valued = no rate limit (latent).
	RateLimit RateLimitConfig

	// MaxTokens is the per-call cap. Requests whose `MaxTokens` exceed
	// this fail loudly. 0 = no cap (latent).
	MaxTokens int
}

// RateLimitConfig is the token-bucket shape. Capacity is the bucket
// ceiling (max tokens reservable). RefillTokens fill the bucket every
// RefillInterval. A zero Capacity disables the rate limit even if other
// fields are set.
type RateLimitConfig struct {
	Capacity       int
	RefillTokens   int
	RefillInterval time.Duration
}

// IsZero reports whether the RateLimitConfig is fully zero-valued (no
// rate-limit enforcement configured).
func (r RateLimitConfig) IsZero() bool {
	return r.Capacity == 0 && r.RefillTokens == 0 && r.RefillInterval == 0
}

// IsEnabled reports whether the RateLimitConfig has the minimum
// fields set to drive enforcement (Capacity > 0; refill knobs may be
// zero for non-refilling buckets, but Capacity is the gate).
func (r RateLimitConfig) IsEnabled() bool {
	return r.Capacity > 0
}

// TierResolver maps an `identity.Identity` to a tier name. Resolvers
// MUST be pure and deterministic — the same identity always maps to the
// same tier. Non-deterministic resolvers race against the per-identity
// state caches.
type TierResolver func(identity.Identity) string

// Clock is governance's time source. The standard `time` package
// implements this implicitly via the default `realClock` value; tests
// inject a fake clock with controllable Now().
type Clock interface {
	Now() time.Time
}

// RealClock wraps `time.Now` and is the production default. Exported so
// callers that construct a Config with an explicit Clock for one
// dimension can keep the default for the rest.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// DefaultClock is the production clock. Used when `Config.Clock` is nil.
var DefaultClock Clock = RealClock{}

// resolveTier picks the tier name for an identity using the configured
// resolver, falling back to DefaultTier. An empty result means "no tier
// resolved" — every governance policy is a no-op in that case.
func (c Config) resolveTier(id identity.Identity) string {
	if c.Resolver != nil {
		if name := c.Resolver(id); name != "" {
			return name
		}
	}
	return c.DefaultTier
}

// tierConfig returns the TierConfig for `id` (after resolution), and an
// ok flag. An unresolved identity OR a resolver-named tier that's absent
// from the map returns ok=false — callers permit unconditionally in that
// case. Latent default flows through this branch.
func (c Config) tierConfig(id identity.Identity) (TierConfig, bool) {
	name := c.resolveTier(id)
	if name == "" {
		return TierConfig{}, false
	}
	tc, ok := c.IdentityTiers[name]
	if !ok {
		return TierConfig{}, false
	}
	return tc, true
}

// clock returns the configured Clock or DefaultClock.
func (c Config) clock() Clock {
	if c.Clock == nil {
		return DefaultClock
	}
	return c.Clock
}

// identityFromCtx is the canonical ctx → identity.Identity gate. Missing
// identity returns wrapped `ErrIdentityRequired`. Governance is
// identity-mandatory (AGENTS.md §6 rule 9): the wrapper fails closed.
func identityFromCtx(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.From(ctx)
	if !ok {
		// Try the quadruple key too — some upstream callers attach
		// only the quadruple. Either path is acceptable.
		if q, ok := identity.QuadrupleFrom(ctx); ok {
			return q.Identity, nil
		}
		return identity.Identity{}, fmt.Errorf("%w: no identity in ctx", ErrIdentityRequired)
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return id, nil
}

// quadrupleFromCtx returns the full quadruple for state persistence
// keying. Empty RunID is acceptable for session-scoped state (matches
// `state.ValidateIdentity`).
func quadrupleFromCtx(ctx context.Context) (identity.Quadruple, error) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if err := identity.Validate(q.Identity); err != nil {
			return identity.Quadruple{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		}
		return q, nil
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return identity.Quadruple{}, err
	}
	return identity.Quadruple{Identity: id}, nil
}

// quadKey is the comparable map key shape for an identity quadruple.
type quadKey struct {
	Tenant  string
	User    string
	Session string
	Run     string
}

func quadKeyFor(q identity.Quadruple) quadKey {
	return quadKey{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Run:     q.RunID,
	}
}

// ErrIs reports whether `err` wraps `target` and one of governance's
// sentinels. Useful in callers that branch on policy outcome (e.g. the
// runtime can route an ErrBudgetExceeded to the unified pause/resume
// primitive while letting other errors propagate).
//
// Returns false on nil err.
func ErrIs(err, target error) bool {
	return err != nil && errors.Is(err, target)
}
