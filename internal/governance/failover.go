package governance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// ErrFailoverExhausted — the failover walk advanced through every entry in
// the ordered chain and the terminal hop STILL returned a retryable provider
// error. Wraps the last hop's error so the operator sees why the chain ran
// out. A PERMANENT error (or a PreCall trip) stops the walk earlier with its
// own error, never this one. Callers compare via errors.Is.
var ErrFailoverExhausted = errors.New("governance: failover chain exhausted")

// ProviderRef names one entry in the ordered failover chain — a
// broker-pulled, zero-URL/zero-secret descriptor. The fallback key is pulled
// via the inference broker-pull source and never persisted; advancing a hop
// activates the ref's live key and re-issues through the one-method
// LLMClient. Provider MAY differ across entries so a cross-provider
// (heterogeneous) chain is expressible.
type ProviderRef struct {
	// Provider is the LLM provider the entry authenticates against (e.g.
	// "openai", "anthropic"). Entries may name DIFFERENT providers.
	Provider string
	// Name is the installed provider-binding name resolved by the
	// KeyActivator (bare-name resolution). It references a zero-URL,
	// broker-pulled binding — never a credential-sink lever on the wire.
	Name string
}

// KeyActivator makes a ProviderRef's broker-pulled key the live key the
// one-method LLMClient reads on its next Complete. It is the seam between
// the governance-orchestrated failover walk and the inference broker-pull
// credential source: production wires it to swap the runtime's atomic
// LiveKey to the ref's binding; a fail-loud activation (broker unreachable,
// binding unknown) stops the walk rather than re-issuing against a stale or
// empty key.
//
// A KeyActivator is a compiled artifact shared across N goroutines; it holds
// no per-run state.
type KeyActivator interface {
	// Activate installs ref's broker-pulled key as the live key. A non-nil
	// return fails the hop loud (the walk stops); it never silently serves a
	// prior provider's key.
	Activate(ctx context.Context, ref ProviderRef) error
}

// CostReader reads the accumulated per-identity cost so the
// governance.failover hop event can carry the running total the re-run
// PreCall gates against. The CostAccumulator satisfies it; a nil reader
// yields a zero AccumCostUSD (the event still emits — the ceiling check
// itself runs inside PreCall, not off this read).
type CostReader interface {
	Snapshot(ctx context.Context, q identity.Quadruple) (float64, map[string]float64, error)
}

// FailoverPolicy walks an ordered chain of broker-pulled provider
// descriptors on a retryable provider error, re-running PreCall before each
// re-issue and emitting a governance.failover hop event. It realizes the
// RFC §6.15 failover seam — Harbor orchestrates the walk at the governance
// layer: the provider SDK's native fallback array is deliberately NOT used, so
// every hop is a Harbor event through audit + bus + the per-identity cost
// accumulator, and a cross-provider chain stays fully expressible.
//
// Immutable after construction; safe to share across N goroutines. Per-run
// state (hop index, per-hop response) lives on the call stack / ctx, never
// on the policy (the concurrent-reuse contract).
type FailoverPolicy interface {
	// Complete issues the request against the primary provider, then walks
	// the ordered chain on a retryable error. Before EACH re-issue it
	// re-runs PreCall for the run's identity; a hop that trips PreCall
	// returns the budget / rate sentinel and STOPS the walk (no silent
	// continue). Each hop emits governance.failover. Returns the first
	// successful response, or the terminal error on chain exhaustion
	// (ErrFailoverExhausted) / a permanent error (returned verbatim).
	Complete(ctx context.Context, ident identity.Identity, req llm.CompleteRequest, chain []ProviderRef) (llm.CompleteResponse, error)
}

// FailoverConfig is the constructor input for the chain-walk FailoverPolicy.
// Inner, Governance, Activator and Bus are mandatory; Cost and Logger are
// optional.
type FailoverConfig struct {
	// Inner is the one-method LLMClient re-issued on every hop (the same
	// client the plain single-provider path uses — the walk is mechanism
	// ABOVE the client, never a new client method).
	Inner llm.LLMClient
	// Governance is the enforcement Subsystem whose PreCall re-gates every
	// hop (budget / rate-limit / MaxTokens) and whose PostCall accounts each
	// hop's cost so the NEXT PreCall sees the accumulated total.
	Governance Subsystem
	// Activator swaps the broker-pulled live key to a hop's provider binding.
	Activator KeyActivator
	// Cost reads the accumulated per-identity cost for the hop event. Optional.
	Cost CostReader
	// Bus publishes the governance.failover hop event. Mandatory.
	Bus events.EventBus
	// Clock is the time source for the hop event's OccurredAt. Nil = DefaultClock.
	Clock Clock
	// Logger receives PostCall observability warnings. Nil = slog.Default().
	Logger *slog.Logger
}

// chainFailoverPolicy is the one shipped FailoverPolicy driver: it walks the
// ordered broker-pulled chain at the governance layer. All fields are
// immutable after construction (the concurrent-reuse contract).
type chainFailoverPolicy struct {
	inner  llm.LLMClient
	gov    Subsystem
	act    KeyActivator
	cost   CostReader
	bus    events.EventBus
	clock  Clock
	logger *slog.Logger
}

// NewFailoverPolicy constructs the chain-walk FailoverPolicy. It validates
// the mandatory collaborators loud (a nil Inner / Governance / Activator /
// Bus is a construction bug, not a runtime degradation) and returns the
// shared, immutable policy.
func NewFailoverPolicy(cfg FailoverConfig) (FailoverPolicy, error) {
	if cfg.Inner == nil {
		return nil, fmt.Errorf("%w: failover Inner LLMClient is required", ErrInvalidConfig)
	}
	if cfg.Governance == nil {
		return nil, fmt.Errorf("%w: failover Governance Subsystem is required", ErrInvalidConfig)
	}
	if cfg.Activator == nil {
		return nil, fmt.Errorf("%w: failover KeyActivator is required", ErrInvalidConfig)
	}
	if cfg.Bus == nil {
		return nil, fmt.Errorf("%w: failover event Bus is required", ErrInvalidConfig)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = DefaultClock
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &chainFailoverPolicy{
		inner:  cfg.Inner,
		gov:    cfg.Governance,
		act:    cfg.Activator,
		cost:   cfg.Cost,
		bus:    cfg.Bus,
		clock:  clock,
		logger: logger,
	}, nil
}

// Complete runs the primary call, then walks the chain on a retryable error.
// The identity flows from ctx (the run quadruple); ident is the caller's
// resolved identity, used as the attribution fallback when ctx carries no
// quadruple.
func (p *chainFailoverPolicy) Complete(ctx context.Context, ident identity.Identity, req llm.CompleteRequest, chain []ProviderRef) (llm.CompleteResponse, error) {
	var err error
	ctx, _, err = llm.EnsureAttemptScope(ctx)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	quad := p.resolveQuad(ctx, ident)

	// Hop 0 — the primary provider. PreCall gates it exactly like every
	// hop; a trip fails loud (identical to the plain single-provider path).
	resp, callErr := p.issue(ctx, req, 0)
	if callErr == nil {
		return resp, nil
	}
	if !isRetryableProviderError(callErr) {
		// Permanent error — the whole chain would waste the same request.
		// Return it verbatim (never a silent success).
		return resp, callErr
	}

	// The primary has no ProviderRef, so the first hop's FromProvider is a
	// best-effort, MODEL-DERIVED label (the request's model name stands in for
	// the primary provider); every subsequent hop's FromProvider is the prior
	// entry's real provider name.
	fromProvider := req.Model
	for i, ref := range chain {
		hopIndex := i + 1

		// Emit the hop event BEFORE the re-issue: the advance decision is
		// made (a retryable primary/prior error), the accumulated cost is the
		// running total the re-run PreCall will gate against.
		accum := p.readAccum(ctx, quad)
		p.emitFailover(ctx, quad, fromProvider, ref.Provider, hopIndex, accum)

		// Activate the hop's broker-pulled key. A fail-loud activation stops
		// the walk — never re-issue against a stale/empty key.
		if err := p.act.Activate(ctx, ref); err != nil {
			return llm.CompleteResponse{}, fmt.Errorf("governance/failover: activate provider %q (hop %d): %w", ref.Provider, hopIndex, err)
		}

		// Re-issue. issue() runs PreCall EXACTLY ONCE before the inner call
		// (never twice — a second PreCall would double-drain the mutating
		// rate limiter), so a hop that trips the budget / rate / MaxTokens
		// ceiling returns the governance sentinel from PreCall without
		// touching the inner client. Those sentinels are all in
		// permanentSentinels(), so the non-retryable branch below STOPS the
		// walk LOUD — the walk does NOT continue down the chain, and an
		// N-provider chain can never push a run past its per-identity ceiling
		// across hops.
		resp, callErr = p.issue(ctx, req, hopIndex)
		if callErr == nil {
			return resp, nil
		}
		if !isRetryableProviderError(callErr) {
			return resp, callErr
		}
		fromProvider = ref.Provider
	}

	// Chain exhausted — every hop returned a retryable error. Surface it loud.
	return resp, fmt.Errorf("%w (%d hops walked): %w", ErrFailoverExhausted, len(chain), callErr)
}

// issue runs PreCall → inner.Complete → PostCall for one hop, installing the
// per-call attempt-cost tap so intermediate retry / downgrade attempts fold
// into accounting (parity with the plain wrap path). PreCall's error
// short-circuits (fail loud); PostCall's error is observability-only.
func (p *chainFailoverPolicy) issue(ctx context.Context, req llm.CompleteRequest, fallbackHop int) (llm.CompleteResponse, error) {
	if err := p.gov.PreCall(ctx, req); err != nil {
		return llm.CompleteResponse{}, err
	}
	ctx, _ = llm.ContextWithAttemptCostTap(llm.WithAttemptCoordinates(ctx, fallbackHop+1, 0, 0, fallbackHop))
	resp, callErr := p.inner.Complete(ctx, req)
	if postErr := p.gov.PostCall(ctx, req, resp, callErr); postErr != nil {
		p.logger.WarnContext(ctx, "governance/failover: PostCall error (observability only)",
			slog.String("error", postErr.Error()),
			slog.String("model", req.Model),
		)
	}
	return resp, callErr
}

// resolveQuad prefers the ctx quadruple (carries RunID) and falls back to the
// caller-supplied identity when ctx has none — so the hop event is always
// attributed even for a ctx that only carried the bare identity.
func (p *chainFailoverPolicy) resolveQuad(ctx context.Context, ident identity.Identity) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	if id, ok := identity.From(ctx); ok {
		return identity.Quadruple{Identity: id}
	}
	return identity.Quadruple{Identity: ident}
}

// readAccum reads the accumulated per-identity cost for the hop event. A nil
// reader or a read error yields 0 — the event still emits (the authoritative
// ceiling check is inside PreCall, not this best-effort read).
func (p *chainFailoverPolicy) readAccum(ctx context.Context, quad identity.Quadruple) float64 {
	if p.cost == nil {
		return 0
	}
	total, _, err := p.cost.Snapshot(ctx, quad)
	if err != nil {
		return 0
	}
	return total
}

// emitFailover publishes the governance.failover hop event. Best-effort — a
// bus failure does NOT change the walk (the failover happened regardless of
// whether an observer saw the event), mirroring the accumulator's
// emit-best-effort posture.
func (p *chainFailoverPolicy) emitFailover(ctx context.Context, quad identity.Quadruple, from, to string, hopIndex int, accum float64) {
	now := p.clock.Now()
	_ = p.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort emit; bus failure doesn't change the walk (see doc above)
		Type:       EventTypeFailover,
		Identity:   quad,
		OccurredAt: now,
		Payload: GovernanceFailoverPayload{
			Identity:     quad,
			FromProvider: from,
			ToProvider:   to,
			HopIndex:     hopIndex,
			AccumCostUSD: accum,
			Reason:       failoverReason,
		},
	})
}

// failoverReason is the bounded, non-secret class label the hop event
// carries. The walk advances ONLY on a retryable provider error, so the
// class is fixed; the raw provider error string is never placed on the bus
// (it may carry provider-specific detail — the event stays SafePayload).
const failoverReason = "retryable_provider_error"

// isRetryableProviderError classifies a provider error as retryable (advance
// the chain) vs permanent (stop the walk). Permanent covers every structural
// LLM sentinel (a malformed request / unsupported model / schema failure that
// switching providers cannot fix), every governance sentinel (a budget / rate
// / identity gate — never bypassed by failover), and ctx cancellation /
// deadline (the caller gave up). Everything else — a transient network fault,
// a 5xx, a timeout the provider surfaced as a generic error — is retryable.
func isRetryableProviderError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	for _, permanent := range permanentSentinels() {
		if errors.Is(err, permanent) {
			return false
		}
	}
	return true
}

// permanentSentinels lists the structural LLM + governance errors that STOP
// the walk. Kept as a function (not a package var) so no mutable slice state
// lives at package scope (the concurrent-reuse contract).
func permanentSentinels() []error {
	return []error{
		// Structural LLM sentinels — a provider swap cannot fix these.
		llm.ErrInvalidContent,
		llm.ErrInvalidConfig,
		llm.ErrUnsupportedModel,
		llm.ErrContextLeak,
		llm.ErrContextWindowExceeded,
		llm.ErrInvalidJSONSchema,
		llm.ErrDowngradeExhausted,
		llm.ErrOrphanToolCall,
		llm.ErrIdentityMissing,
		llm.ErrClientClosed,
		llm.ErrUnknownDriver,
		llm.ErrRetryExhausted,
		llm.ErrValidationFailed,
		// Governance sentinels — a gate is NEVER bypassed by failover.
		ErrBudgetExceeded,
		ErrRateLimited,
		ErrMaxTokensExceeded,
		ErrIdentityRequired,
		ErrStateUnavailable,
		ErrClosed,
	}
}

// --- §4.4 seam registry -----------------------------------------------------

// FailoverDriverFactory builds a FailoverPolicy from a FailoverConfig. The
// §4.4 seam records driver name → factory so an alternate walk strategy (a
// future health-aware variant) can register without touching callers.
type FailoverDriverFactory func(cfg FailoverConfig) (FailoverPolicy, error)

// DefaultFailoverDriver is the name of the shipped chain-walk driver — the
// ordered broker-pulled chain, Harbor-orchestrated at the governance layer.
const DefaultFailoverDriver = "chain"

var (
	failoverDriversMu sync.RWMutex
	failoverDrivers   = map[string]FailoverDriverFactory{}
)

// RegisterFailoverDriver installs a named FailoverPolicy factory. Called from
// init(); the write-once-read-many registry is the §4.4 seam pattern (the one
// sanctioned package-level mutable state — a driver registry, the
// concurrent-reuse contract's write-once-read-many carve-out).
func RegisterFailoverDriver(name string, f FailoverDriverFactory) {
	failoverDriversMu.Lock()
	defer failoverDriversMu.Unlock()
	failoverDrivers[name] = f
}

// NewFailoverPolicyByName builds the named FailoverPolicy driver. An unknown
// name fails loud naming the registered drivers so a misconfiguration is
// obvious (§4.4).
func NewFailoverPolicyByName(name string, cfg FailoverConfig) (FailoverPolicy, error) {
	failoverDriversMu.RLock()
	f, ok := failoverDrivers[name]
	registered := make([]string, 0, len(failoverDrivers))
	for n := range failoverDrivers {
		registered = append(registered, n)
	}
	failoverDriversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: unknown failover driver %q (registered: %v)", ErrInvalidConfig, name, registered)
	}
	return f(cfg)
}

func init() {
	RegisterFailoverDriver(DefaultFailoverDriver, NewFailoverPolicy)
}

var _ FailoverPolicy = (*chainFailoverPolicy)(nil)
