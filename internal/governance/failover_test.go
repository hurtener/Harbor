package governance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// errRetryable is a generic provider error — it matches NO structural LLM
// sentinel and NO governance sentinel, so the failover classifier treats it
// as retryable (advance the chain). It stands in for a transient 5xx /
// network fault a real provider would surface.
var errRetryable = errors.New("provider: transient upstream 503")

// providerLLM is a fake one-method LLMClient whose success depends on the
// ACTIVE provider (set by the recordingActivator). It returns success only
// when the active provider equals succeedOn; every other provider yields
// errRetryable. It reports a fixed cost on every call so the cost
// accumulator can gate a later hop's PreCall. Immutable but for the atomic
// call counter — safe to share across goroutines.
type providerLLM struct {
	active    *atomic.Pointer[string] // shared with the activator; nil before the first activation (the primary)
	succeedOn string
	cost      float64
	calls     atomic.Int64
}

func (l *providerLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	l.calls.Add(1)
	cur := "primary"
	if l.active != nil {
		if p := l.active.Load(); p != nil {
			cur = *p
		}
	}
	resp := llm.CompleteResponse{Content: "ok:" + cur, Cost: llm.Cost{TotalCost: l.cost, Currency: "USD"}}
	if cur == l.succeedOn {
		return resp, nil
	}
	return resp, errRetryable
}

func (l *providerLLM) Close(context.Context) error { return nil }

// errLLM returns a fixed error on every call (used to prove a PERMANENT
// error stops the walk).
type errLLM struct {
	err   error
	calls atomic.Int64
}

func (l *errLLM) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	l.calls.Add(1)
	return llm.CompleteResponse{}, l.err
}
func (l *errLLM) Close(context.Context) error { return nil }

// recordingActivator records every ProviderRef it is asked to activate and
// (when wired) publishes the active provider into a shared atomic the
// providerLLM reads. failOn forces an activation failure for one provider.
type recordingActivator struct {
	mu     sync.Mutex
	calls  []governance.ProviderRef
	active *atomic.Pointer[string]
	failOn string
}

func (a *recordingActivator) Activate(_ context.Context, ref governance.ProviderRef) error {
	a.mu.Lock()
	a.calls = append(a.calls, ref)
	a.mu.Unlock()
	if a.failOn != "" && ref.Provider == a.failOn {
		return fmt.Errorf("broker unreachable for provider %q", ref.Provider)
	}
	if a.active != nil {
		v := ref.Provider
		a.active.Store(&v)
	}
	return nil
}

func (a *recordingActivator) providers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.calls))
	for i, r := range a.calls {
		out[i] = r.Provider
	}
	return out
}

// latentSub is a governance Subsystem that permits every PreCall and folds
// cost into the supplied accumulator on PostCall. It lets a failover unit
// test run without a ceiling (the latent default) while still driving real
// PostCall accounting when an accumulator is supplied.
type latentSub struct {
	acc *governance.CostAccumulator
}

func (s *latentSub) PreCall(context.Context, llm.CompleteRequest) error { return nil }
func (s *latentSub) PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error {
	if s.acc == nil {
		return nil
	}
	return s.acc.PostCall(ctx, req, resp, callErr)
}

func newFailover(t *testing.T, inner llm.LLMClient, sub governance.Subsystem, act governance.KeyActivator, cost governance.CostReader, bus events.EventBus) governance.FailoverPolicy {
	t.Helper()
	fp, err := governance.NewFailoverPolicy(governance.FailoverConfig{
		Inner: inner, Governance: sub, Activator: act, Cost: cost, Bus: bus,
	})
	if err != nil {
		t.Fatalf("NewFailoverPolicy: %v", err)
	}
	return fp
}

func subscribeFailover(t *testing.T, bus events.EventBus, tenant, user, session string) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: tenant, User: user, Session: session,
		Types: []events.EventType{governance.EventTypeFailover},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

func drainFailoverEvents(sub events.Subscription, d time.Duration) []governance.GovernanceFailoverPayload {
	var out []governance.GovernanceFailoverPayload
	deadline := time.After(d)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == governance.EventTypeFailover {
				if p, ok := ev.Payload.(governance.GovernanceFailoverPayload); ok {
					out = append(out, p)
				}
			}
		case <-deadline:
			return out
		}
	}
}

// TestFailoverPolicy_AdvancesOnRetryable_Succeeds — a retryable error on the
// primary advances to the first fallback, which succeeds; exactly one
// governance.failover event is emitted, keyed by the run identity.
func TestFailoverPolicy_AdvancesOnRetryable_Succeeds(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity
	sub := subscribeFailover(t, bus, "acme", "u1", "s1")

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "backup", cost: 0.001}
	act := &recordingActivator{active: active}
	fp := newFailover(t, llmc, &latentSub{}, act, nil, bus)

	resp, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok:backup" {
		t.Fatalf("resp = %q, want the fallback response", resp.Content)
	}
	if llmc.calls.Load() != 2 {
		t.Fatalf("inner called %d times, want 2 (primary + one fallback)", llmc.calls.Load())
	}
	evs := drainFailoverEvents(sub, time.Second)
	if len(evs) != 1 {
		t.Fatalf("emitted %d governance.failover events, want 1", len(evs))
	}
	if evs[0].ToProvider != "backup" || evs[0].HopIndex != 1 {
		t.Fatalf("event = %+v, want to=backup hop=1", evs[0])
	}
	if evs[0].Identity.TenantID != "acme" {
		t.Fatalf("event identity = %+v, want the run tenant", evs[0].Identity)
	}
}

// TestFailoverPolicy_StopsOnPermanentError — a PERMANENT error on the primary
// (a structural LLM sentinel a provider swap cannot fix) stops the walk
// immediately: no advance, the terminal error is returned verbatim, no
// fallback is activated, no governance.failover event fires.
func TestFailoverPolicy_StopsOnPermanentError(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity
	sub := subscribeFailover(t, bus, "acme", "u1", "s1")

	llmc := &errLLM{err: llm.ErrInvalidContent}
	act := &recordingActivator{}
	fp := newFailover(t, llmc, &latentSub{}, act, nil, bus)

	_, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
	if !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err = %v, want ErrInvalidContent returned verbatim", err)
	}
	if llmc.calls.Load() != 1 {
		t.Fatalf("inner called %d times, want 1 (no advance on a permanent error)", llmc.calls.Load())
	}
	if got := act.providers(); len(got) != 0 {
		t.Fatalf("activator called %v, want none (no advance on a permanent error)", got)
	}
	if evs := drainFailoverEvents(sub, 200*time.Millisecond); len(evs) != 0 {
		t.Fatalf("emitted %d events, want 0 on a permanent error", len(evs))
	}
}

// TestFailoverPolicy_ChainExhausted_FailsLoud — every hop returns a retryable
// error; the walk exhausts the chain and returns ErrFailoverExhausted
// wrapping the last error (never a silent success).
func TestFailoverPolicy_ChainExhausted_FailsLoud(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "never", cost: 0}
	act := &recordingActivator{active: active}
	fp := newFailover(t, llmc, &latentSub{}, act, nil, bus)

	_, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{
		{Provider: "b1", Name: "b1"}, {Provider: "b2", Name: "b2"},
	})
	if !errors.Is(err, governance.ErrFailoverExhausted) {
		t.Fatalf("err = %v, want ErrFailoverExhausted", err)
	}
	if !errors.Is(err, errRetryable) {
		t.Fatalf("err = %v, want the last hop's error wrapped", err)
	}
	if llmc.calls.Load() != 3 {
		t.Fatalf("inner called %d times, want 3 (primary + 2 hops)", llmc.calls.Load())
	}
}

// TestFailoverPolicy_CrossProvider_Walks — a chain naming DIFFERENT providers
// (openai → anthropic) walks correctly: each hop re-issues against the next
// provider's activated key, and the last provider succeeds.
func TestFailoverPolicy_CrossProvider_Walks(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "anthropic", cost: 0}
	act := &recordingActivator{active: active}
	fp := newFailover(t, llmc, &latentSub{}, act, nil, bus)

	resp, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{
		{Provider: "openai", Name: "oa"}, {Provider: "anthropic", Name: "an"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok:anthropic" {
		t.Fatalf("resp = %q, want anthropic success", resp.Content)
	}
	if got := act.providers(); len(got) != 2 || got[0] != "openai" || got[1] != "anthropic" {
		t.Fatalf("activation order = %v, want [openai anthropic]", got)
	}
}

// TestFailoverPolicy_PreCallTripFailsLoudAndStops — the D-018 cost-control
// hole: a hop whose re-run PreCall trips the per-identity budget ceiling
// returns ErrBudgetExceeded and STOPS the walk — the next entry is NEVER
// tried, so an N-provider chain cannot push a run past its ceiling across
// hops. Uses the REAL CostAccumulator.
func TestFailoverPolicy_PreCallTripFailsLoudAndStops(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity

	// A ceiling below the primary's reported cost: the primary passes PreCall
	// (total 0) then reports 0.01, tipping the accumulator over 0.001 so the
	// FIRST fallback hop's PreCall trips.
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{
		DefaultTier:   "t",
		IdentityTiers: map[string]governance.TierConfig{"t": {BudgetCeilingUSD: 0.001}},
	})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "backup", cost: 0.01}
	act := &recordingActivator{active: active}
	// The compound wraps only the accumulator so PreCall enforces the ceiling
	// and PostCall folds the cost.
	sub := governance.NewCompound(acc)
	fp := newFailover(t, llmc, sub, act, acc, bus)

	_, err = fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"},
		[]governance.ProviderRef{{Provider: "backup", Name: "b1"}, {Provider: "backup2", Name: "b2"}})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded (the walk must stop on a PreCall trip)", err)
	}
	if llmc.calls.Load() != 1 {
		t.Fatalf("inner called %d times, want 1 (only the primary; the budget-tripped hop never re-issues)", llmc.calls.Load())
	}
	// The second fallback must NEVER be reached — the trip stops the walk.
	if got := act.providers(); len(got) != 1 || got[0] != "backup" {
		t.Fatalf("activation = %v, want only [backup] (the walk stops at the first budget trip)", got)
	}
}

// TestFailoverPolicy_RateLimiter_NoDoubleDrain guards the regression where the
// walk ran PreCall TWICE per fallback hop (an explicit hop-level PreCall PLUS
// issue()'s own PreCall), double-draining the MUTATING RateLimiter so a
// fallback spuriously tripped ErrRateLimited even though the bucket held
// exactly enough tokens for the real inner calls. It wires a REAL RateLimiter
// (via NewCompound, the production shape) and asserts:
//
//   - a bucket with EXACTLY enough tokens for the real calls (primary +
//     fallback = 2 drains) lets the fallback SUCCEED — one PreCall drain per
//     inner call, never two. (This subtest FAILS under the double-drain: the
//     fallback's second PreCall underflows and stops the walk at inner=1.)
//   - a bucket too small for the two calls trips ErrRateLimited on the
//     fallback and STOPS the walk loud (the limiter is never bypassed by
//     failover — the symmetric guard).
func TestFailoverPolicy_RateLimiter_NoDoubleDrain(t *testing.T) {
	t.Parallel()

	// requestedTokens defaults to 1 when req.MaxTokens is unset, so each PreCall
	// drains exactly 1 token from the per-(identity, model) bucket. The re-issue
	// keeps the same req.Model, so primary + fallback share one bucket.
	newRLFailover := func(t *testing.T, capacity int) (governance.FailoverPolicy, *providerLLM, *recordingActivator) {
		t.Helper()
		bus, st, cleanup := busAndState(t)
		t.Cleanup(cleanup)
		rl, err := governance.NewRateLimiter(st, bus, governance.Config{
			DefaultTier: "t",
			IdentityTiers: map[string]governance.TierConfig{
				"t": {RateLimit: governance.RateLimitConfig{Capacity: capacity}},
			},
		})
		if err != nil {
			t.Fatalf("NewRateLimiter: %v", err)
		}
		t.Cleanup(func() { _ = rl.Close(context.Background()) })
		active := &atomic.Pointer[string]{}
		llmc := &providerLLM{active: active, succeedOn: "backup", cost: 0}
		act := &recordingActivator{active: active}
		fp := newFailover(t, llmc, governance.NewCompound(rl), act, nil, bus)
		return fp, llmc, act
	}

	t.Run("exactly_enough_tokens_fallback_succeeds", func(t *testing.T) {
		t.Parallel()
		ctx := ctxWith(t, "acme", "u1", "s1", "r1")
		id := identity.MustQuadrupleFrom(ctx).Identity
		// Capacity 2 == exactly the real inner-call count (primary + fallback).
		fp, llmc, _ := newRLFailover(t, 2)
		resp, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
		if err != nil {
			t.Fatalf("Complete: %v (a bucket sized for the real calls must NOT trip — double-drain regression)", err)
		}
		if resp.Content != "ok:backup" {
			t.Fatalf("resp = %q, want the fallback response", resp.Content)
		}
		if llmc.calls.Load() != 2 {
			t.Fatalf("inner called %d times, want 2 (primary + fallback, one PreCall drain each)", llmc.calls.Load())
		}
	})

	t.Run("bucket_too_small_trips_loud_and_stops", func(t *testing.T) {
		t.Parallel()
		ctx := ctxWith(t, "beta", "u1", "s1", "r1")
		id := identity.MustQuadrupleFrom(ctx).Identity
		// Capacity 1 is too small for two real calls: the primary drains it, the
		// fallback's PreCall underflows → ErrRateLimited stops the walk loud.
		fp, llmc, act := newRLFailover(t, 1)
		_, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
		if !errors.Is(err, governance.ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited (the limiter is never bypassed by failover)", err)
		}
		if llmc.calls.Load() != 1 {
			t.Fatalf("inner called %d times, want 1 (the rate-limited fallback never re-issues)", llmc.calls.Load())
		}
		// The fallback WAS activated (the advance was attempted) but the re-issue
		// was gated by the single PreCall — no double drain, no second hop.
		if got := act.providers(); len(got) != 1 || got[0] != "backup" {
			t.Fatalf("activation = %v, want only [backup]", got)
		}
	})
}

// TestFailoverPolicy_ActivationFailureStops — a broker-unreachable activation
// fails the hop loud and stops the walk (never re-issues against a stale key).
func TestFailoverPolicy_ActivationFailureStops(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "backup", cost: 0}
	act := &recordingActivator{active: active, failOn: "backup"}
	fp := newFailover(t, llmc, &latentSub{}, act, nil, bus)

	_, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
	if err == nil || !strings.Contains(err.Error(), "activate provider") {
		t.Fatalf("err = %v, want a fail-loud activation error", err)
	}
	if llmc.calls.Load() != 1 {
		t.Fatalf("inner called %d times, want 1 (activation failed before re-issue)", llmc.calls.Load())
	}
}

// TestFailover_NeverConstructsBifrostFallback is the D-018 static guard: the
// failover source walks the chain at the governance layer and MUST NEVER
// construct or reference the provider SDK's native fallback array. A source
// scan pins that the mechanism stays Harbor-orchestrated.
func TestFailover_NeverConstructsBifrostFallback(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("failover.go")
	if err != nil {
		t.Fatalf("read failover.go: %v", err)
	}
	for _, banned := range []string{"Fallback", "schemas.", "bifrost"} {
		if strings.Contains(string(src), banned) {
			t.Fatalf("failover.go references %q — the walk must stay Harbor-orchestrated (D-018), never delegate to the SDK fallback array", banned)
		}
	}
}

// TestWrapWithFailover_EmptyChainIsSingleProviderPath — with NO chain the
// wrap is byte-for-byte the plain single-provider path: PreCall → inner →
// PostCall once, no failover machinery (one path, chain length 0 == single
// provider — no two parallel implementations).
func TestWrapWithFailover_EmptyChainIsSingleProviderPath(t *testing.T) {
	t.Parallel()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")

	llmc := &providerLLM{succeedOn: "primary", cost: 0}
	client, err := governance.WrapWithFailover(llmc, &latentSub{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("WrapWithFailover: %v", err)
	}
	resp, err := client.Complete(ctx, llm.CompleteRequest{Model: "primary"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok:primary" {
		t.Fatalf("resp = %q, want the single-provider response", resp.Content)
	}
	if llmc.calls.Load() != 1 {
		t.Fatalf("inner called %d times, want 1 (single-provider path)", llmc.calls.Load())
	}
}

// TestWrapWithFailover_RoutesThroughChain — with a chain configured the wrap
// delegates to the failover walk: a retryable primary advances to the
// fallback which succeeds.
func TestWrapWithFailover_RoutesThroughChain(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "acme", "u1", "s1", "r1")

	active := &atomic.Pointer[string]{}
	llmc := &providerLLM{active: active, succeedOn: "backup", cost: 0}
	act := &recordingActivator{active: active}
	client, err := governance.WrapWithFailover(llmc, &latentSub{}, []governance.ProviderRef{{Provider: "backup", Name: "b"}}, act, nil, bus)
	if err != nil {
		t.Fatalf("WrapWithFailover: %v", err)
	}
	resp, err := client.Complete(ctx, llm.CompleteRequest{Model: "primary"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok:backup" {
		t.Fatalf("resp = %q, want the fallback response (routed through the chain)", resp.Content)
	}
	if llmc.calls.Load() != 2 {
		t.Fatalf("inner called %d times, want 2 (primary + fallback via the chain)", llmc.calls.Load())
	}
}

// ctxScopedActivator + ctxScopedLLM share ONE sync.Map keyed by the ctx
// tenant so a SINGLE shared FailoverPolicy can be exercised by N concurrent
// goroutines with distinct identities and a deterministic per-goroutine walk
// — the per-run "which provider is active" state lives keyed by ctx identity,
// never on the policy (D-025).
type ctxScopedState struct {
	active sync.Map // tenant → active provider string
}

type ctxScopedLLM struct {
	shared    *ctxScopedState
	succeedOn string
	cost      float64
	calls     atomic.Int64
}

func (l *ctxScopedLLM) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	l.calls.Add(1)
	q, _ := identity.QuadrupleFrom(ctx)
	cur := "primary"
	if v, ok := l.shared.active.Load(q.TenantID); ok {
		cur, _ = v.(string)
	}
	resp := llm.CompleteResponse{Content: "ok:" + cur, Cost: llm.Cost{TotalCost: l.cost, Currency: "USD"}}
	if cur == l.succeedOn {
		return resp, nil
	}
	return resp, errRetryable
}
func (l *ctxScopedLLM) Close(context.Context) error { return nil }

type ctxScopedActivator struct{ shared *ctxScopedState }

func (a *ctxScopedActivator) Activate(ctx context.Context, ref governance.ProviderRef) error {
	q, _ := identity.QuadrupleFrom(ctx)
	a.shared.active.Store(q.TenantID, ref.Provider)
	return nil
}

// TestConcurrentReuse_FailoverPolicy_NoBleed runs N≥100 concurrent Complete
// walks against a SINGLE shared FailoverPolicy instance under -race, each with
// a distinct identity. It asserts no data race, no cross-identity cost bleed
// (each identity's accumulated cost is its own), and no goroutine leak after
// teardown (D-025).
func TestConcurrentReuse_FailoverPolicy_NoBleed(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	// No ceiling (latent) so every hop's PreCall permits; the accumulator
	// still folds each identity's cost so the isolation assertion has teeth.
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	const N = 128
	const costPerCall = 0.002 // primary (retryable) + one fallback = 2 calls folded

	// ONE shared policy over ONE shared LLM + activator + accumulator.
	shared := &ctxScopedState{}
	llmc := &ctxScopedLLM{shared: shared, succeedOn: "backup", cost: costPerCall}
	act := &ctxScopedActivator{shared: shared}
	fp := newFailover(t, llmc, governance.NewCompound(acc), act, acc, bus)

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := ctxWith(t, fmt.Sprintf("tenant-%d", i), "u", "s", "run")
			id := identity.MustQuadrupleFrom(ctx).Identity
			resp, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"}, []governance.ProviderRef{{Provider: "backup", Name: "b"}})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if resp.Content != "ok:backup" {
				errs <- fmt.Errorf("goroutine %d: resp %q", i, resp.Content)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// Per-identity cost isolation: each identity accumulated exactly its own
	// two folded calls, never a neighbour's.
	for i := range N {
		ctx := ctxWith(t, fmt.Sprintf("tenant-%d", i), "u", "s", "run")
		q := identity.MustQuadrupleFrom(ctx)
		total, _, err := acc.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot[%d]: %v", i, err)
		}
		if !floatNear(total, 2*costPerCall) {
			t.Fatalf("tenant-%d accumulated %v, want %v (cross-identity bleed)", i, total, 2*costPerCall)
		}
	}

	// Goroutine-leak guard: the walk joins every goroutine it starts (it starts
	// none) — the count returns to baseline after teardown.
	for range 50 {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine count %d did not return to baseline %d", runtime.NumGoroutine(), baseline)
}
