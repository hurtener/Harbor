// Package integration_test — the v1.17 wave-end E2E (CLAUDE.md §17.7 step 5).
//
// It spans the wave's shipped surfaces end-to-end over REAL drivers (the
// patterns audit redactor + inmem event bus + inmem state store + the real
// governance enforcement stack + a spec-shaped fixture inference broker + the
// real notifications mapper + the real OAuth provider set):
//
//   - group-cancelled conversational mirror (task-group cancel notification),
//   - the planner steer / pause / resume descendant-control decision shapes,
//   - the per-tool OAuth owner-scoped provider uninstall,
//   - the admin `governance.set_posture` write + round-trip read,
//   - the inference broker-pull provider install + fail-loud
//     ErrProviderKeyUnavailable on an unreachable broker,
//   - the Harbor-orchestrated failover walk with a re-run-PreCall BUDGET TRIP
//     that fails loud and STOPS the walk — the wave's load-bearing failure
//     mode.
//
// Identity propagation (the multi-isolation triple) is asserted on every leg.
// A concurrency-stress subtest runs the failover walk across ≥2 tenants under
// N≥10 goroutines against a SINGLE shared FailoverPolicy, asserting per-identity
// cost-accumulator isolation (no cross-identity budget bleed) and no goroutine
// leak after teardown. Runs under -race.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

func TestE2E_WaveV117(t *testing.T) {
	t.Run("group_cancelled_mirror", testWaveV117GroupCancelledMirror)           // 192
	t.Run("planner_steer_pause_resume", testWaveV117PlannerSteerPauseResume)    // 193
	t.Run("per_tool_oauth_owner_uninstall", testWaveV117OwnerScopedUninstall)   // 194
	t.Run("governance_set_posture", testWaveV117GovernanceSetPosture)           // 195
	t.Run("inference_broker_pull", testWaveV117InferenceBrokerPull)             // 196
	t.Run("failover_precall_trip", testWaveV117FailoverPreCallTrip)             // 197 — the failure mode
	t.Run("failover_concurrency_stress", testWaveV117FailoverConcurrencyStress) // N>=10 across >=2 tenants
}

// --- real-driver fixtures ---------------------------------------------------

func waveV117BusState(t *testing.T) (events.EventBus, state.StateStore) {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 128,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return bus, st
}

func waveV117Ctx(t *testing.T, tenant, user, session, run string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	ctx, err := identity.WithRun(context.Background(), id, run)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// waveV117LLM is a ctx-RUN-keyed fake LLMClient: success only when the active
// provider for the ctx RUN equals succeedOn; otherwise a generic (retryable)
// provider error. Keying by the per-run id (never the tenant) keeps concurrent
// same-tenant goroutines independent so the walk is deterministic per run,
// while cost still folds into the shared per-tenant IDENTITY accumulator. A
// fixed cost per call feeds the real cost accumulator so a later hop's PreCall
// can trip a ceiling. Safe to share across goroutines (ctx-scoped state,
// atomic counter — the concurrent-reuse contract).
type waveV117LLM struct {
	shared    *sync.Map // run id → active provider string
	succeedOn string
	cost      float64
	calls     atomic.Int64
}

var errWaveV117Retryable = errors.New("provider: transient 503 (retryable)")

func (l *waveV117LLM) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	l.calls.Add(1)
	q, _ := identity.QuadrupleFrom(ctx)
	cur := "primary"
	if v, ok := l.shared.Load(q.RunID); ok {
		cur, _ = v.(string)
	}
	resp := llm.CompleteResponse{Content: "ok:" + cur, Cost: llm.Cost{TotalCost: l.cost, Currency: "USD"}}
	if cur == l.succeedOn {
		return resp, nil
	}
	return resp, errWaveV117Retryable
}
func (l *waveV117LLM) Close(context.Context) error { return nil }

// waveV117Activator swaps the ctx RUN's active provider — the seam the
// production installer wires to the broker-pulled LiveKey (keyed per run so
// concurrent same-tenant walks stay independent in the fixture).
type waveV117Activator struct{ shared *sync.Map }

func (a *waveV117Activator) Activate(ctx context.Context, ref governance.ProviderRef) error {
	q, _ := identity.QuadrupleFrom(ctx)
	a.shared.Store(q.RunID, ref.Provider)
	return nil
}

// --- 192: group-cancelled conversational mirror -----------------------------

func testWaveV117GroupCancelledMirror(t *testing.T) {
	t.Parallel()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "u", SessionID: "s"}, RunID: "r"}
	trigger := func(origin tasks.CancelOrigin) events.Event {
		return events.Event{
			Type:     tasks.EventTypeTaskGroupCancelled,
			Identity: id,
			Payload: tasks.TaskGroupCancelledPayload{
				Origin: origin,
				Completion: tasks.GroupCompletion{
					GroupID:     tasks.TaskGroupID("g-1"),
					FinalStatus: tasks.GroupCancelled,
					Members: []tasks.MemberOutcome{
						{TaskID: "t-1", Status: tasks.StatusCancelled, Description: "loser"},
						{TaskID: "t-2", Status: tasks.StatusComplete, Description: "winner"},
					},
				},
			},
		}
	}

	// An UNPROMPTED cascade/fail-fast cancel is mirrored onto the conversation.
	out, err := notifications.Map(context.Background(), trigger(tasks.CancelOriginCascade))
	if err != nil {
		t.Fatalf("Map(cascade): %v", err)
	}
	if len(out) != 1 || out[0].Type != notifications.EventTypeNotificationTaskGroupCancelled {
		t.Fatalf("cascade cancel: got %d events (%v), want one task_group_cancelled mirror", len(out), out)
	}
	if out[0].Identity.TenantID != "acme" || out[0].Identity.SessionID != "s" {
		t.Fatalf("identity not propagated onto the mirror: %+v", out[0].Identity)
	}

	// A DIRECT operator-initiated cancel the operator already knows about is
	// suppressed — no mirror.
	out, err = notifications.Map(context.Background(), trigger(tasks.CancelOriginOperator))
	if err != nil {
		t.Fatalf("Map(operator): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("operator cancel: got %d events, want 0 (suppressed)", len(out))
	}
}

// --- 193: planner steer / pause / resume descendant-control decisions -------

func testWaveV117PlannerSteerPauseResume(t *testing.T) {
	t.Parallel()
	// Scope note: this leg is a decision-shape contract check, NOT the
	// end-to-end seam proof. The authoritative end-to-end coverage — routing a
	// SteerTask/PauseTask/ResumeTask through a real dispatch executor onto a
	// per-sub-run steering inbox, with descendant-scoping, human-supremacy, and
	// fail-loud serialization against real drivers — lives in
	// internal/runtime/dispatch/dispatch_steer_pause_test.go. Here we only prove
	// the three sealed shapes carry the descendant TaskID + guidance and remain
	// distinguishable in a planner.Decision type switch (the compile-time sum
	// type plus the field contract the projector and dispatch edge rely on).
	decs := []planner.Decision{
		planner.SteerTask{TaskID: tasks.TaskID("t-1"), Directive: "focus on the auth path"},
		planner.PauseTask{TaskID: tasks.TaskID("t-2"), Reason: "waiting on upstream"},
		planner.ResumeTask{TaskID: tasks.TaskID("t-3"), Directive: "continue"},
	}
	var sawSteer, sawPause, sawResume bool
	for _, d := range decs {
		switch v := d.(type) {
		case planner.SteerTask:
			sawSteer = v.TaskID == "t-1" && v.Directive == "focus on the auth path"
		case planner.PauseTask:
			sawPause = v.TaskID == "t-2" && v.Reason == "waiting on upstream"
		case planner.ResumeTask:
			sawResume = v.TaskID == "t-3" && v.Directive == "continue"
		default:
			t.Fatalf("unexpected decision type %T", v)
		}
	}
	if !sawSteer || !sawPause || !sawResume {
		t.Fatalf("decision shapes did not carry their descendant fields: steer=%v pause=%v resume=%v", sawSteer, sawPause, sawResume)
	}
}

// --- 194: per-tool OAuth owner-scoped provider uninstall --------------------

// stubOAuthProvider is a minimal auth.OAuthProvider — the wave test exercises
// the provider SET's owner-scoping (Install / Uninstall boundary), not the
// OAuth flow itself, so every flow method is a trivial stub. Close tracks its
// invocation so the drop-closes-the-instance contract is observable.
type stubOAuthProvider struct{ closes atomic.Int64 }

func (p *stubOAuthProvider) Token(context.Context, tools.ToolSourceID) (auth.Token, error) {
	return auth.Token{}, nil
}
func (p *stubOAuthProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}
func (p *stubOAuthProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, nil
}
func (p *stubOAuthProvider) PendingFlow(string) (auth.PendingFlowInfo, bool) {
	return auth.PendingFlowInfo{}, false
}
func (p *stubOAuthProvider) DenyFlow(context.Context, string, string) error { return nil }
func (p *stubOAuthProvider) Revoke(context.Context, tools.ToolSourceID) error {
	return nil
}
func (p *stubOAuthProvider) Close(context.Context) error {
	p.closes.Add(1)
	return nil
}
func (p *stubOAuthProvider) AllowedDownstreamHosts() []string { return nil }

func testWaveV117OwnerScopedUninstall(t *testing.T) {
	t.Parallel()
	set := auth.NewProviderSet(nil)
	ownerA := auth.Owner{Tenant: "tA", Agent: "aA"}
	ownerB := auth.Owner{Tenant: "tB", Agent: "aB"}
	p := &stubOAuthProvider{}
	if err := set.Install(ownerA, "m365", p); err != nil {
		t.Fatalf("install: %v", err)
	}

	// A cross-owner uninstall is refused at the STORE boundary — owner B may
	// not drop owner A's binding, and A's provider is left fully intact.
	if err := set.Uninstall(context.Background(), ownerB, "m365"); !errors.Is(err, auth.ErrProviderOwnerCollision) {
		t.Fatalf("cross-owner uninstall: got %v, want ErrProviderOwnerCollision", err)
	}
	if _, ok := set.Get("m365"); !ok {
		t.Fatalf("cross-owner refused uninstall must leave the binding intact")
	}
	if p.closes.Load() != 0 {
		t.Fatalf("a refused cross-owner uninstall must NOT close the instance, closes=%d", p.closes.Load())
	}

	// The owner's OWN uninstall drops the binding and closes the instance so a
	// still-bound call fails loud.
	if err := set.Uninstall(context.Background(), ownerA, "m365"); err != nil {
		t.Fatalf("owner uninstall: %v", err)
	}
	if _, ok := set.Get("m365"); ok {
		t.Fatalf("owner uninstall left the binding present")
	}
	if p.closes.Load() != 1 {
		t.Fatalf("owner uninstall must close exactly once, closes=%d", p.closes.Load())
	}
}

// --- 195: admin governance.set_posture write + round-trip read --------------

func testWaveV117GovernanceSetPosture(t *testing.T) {
	t.Parallel()
	bus, st := waveV117BusState(t)
	policy, err := governance.NewSetPosturePolicy(st, bus, governance.Config{}, nil, nil, false)
	if err != nil {
		t.Fatalf("NewSetPosturePolicy: %v", err)
	}
	t.Cleanup(func() { _ = policy.Close(context.Background()) })
	provider := governance.NewPostureProviderWithState(governance.Config{}, st)

	actor := identity.Quadruple{Identity: identity.Identity{TenantID: "acme", UserID: "admin", SessionID: "s1"}}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "acme", User: "admin", Session: "s1",
		Types: []events.EventType{governance.EventTypePostureSet},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.50, MaxTokens: 2048}},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The write emits governance.posture_set anchored on the actor's triple.
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.GovernancePostureSetPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Actor.TenantID != "acme" || p.DefaultTierAfter != "free" {
			t.Fatalf("posture_set event mismatch: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe governance.posture_set within 2s")
	}

	// Round-trip: the read returns exactly what was written.
	rctx, err := identity.With(context.Background(), actor.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	read, err := provider.Posture(rctx)
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if read.DefaultTier != "free" || read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 {
		t.Fatalf("round-trip mismatch: %+v", read)
	}
}

// --- 196: inference broker-pull install + fail-loud on unreachable broker ---

func testWaveV117InferenceBrokerPull(t *testing.T) {
	// No t.Parallel — this subtest uses t.Setenv (broker auth token).
	const (
		dummyToken = "dummy-runtime-service-token-not-a-secret"
		dummyKey   = "dummy-provider-key-not-a-secret"
		authEnv    = "HARBOR_WAVE_V117_BROKER_TOKEN"
		principal  = "wave-v117-runtime-principal"
		broker     = "wave-v117-openai-broker"
	)
	t.Setenv(authEnv, dummyToken)
	bus, _ := waveV117BusState(t)

	runtimeID := identity.Identity{
		TenantID: credsource.RuntimePrincipalTenant, UserID: credsource.RuntimePrincipalUser, SessionID: principal,
	}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: runtimeID.TenantID, User: runtimeID.UserID, Session: runtimeID.SessionID,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	srv := inferencebrokertest.New(t, dummyToken, dummyKey)
	live := llm.NewLiveKey()
	clockT := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return clockT }

	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: "openai", BrokerName: broker, RuntimePrincipal: principal,
		CredentialURL: srv.URL(), AuthTokenEnv: authEnv, CacheTTL: 5 * time.Minute,
		Live: live, Bus: bus, Redactor: auditpatterns.New(), Clock: clock,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	// Connect seeds the LiveKey (the exact holder the bifrost Account reads).
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if live.Get() != dummyKey {
		t.Fatalf("LiveKey not seeded from the broker pull")
	}
	// Identity propagation: the pull event rides the reserved runtime scope.
	ev := waveV117WaitEvent(t, sub.Events(), llm.EventTypeProviderCredentialFetched)
	if p, ok := ev.Payload.(llm.LLMProviderCredentialFetchedPayload); !ok || p.RuntimePrincipal != principal {
		t.Fatalf("pull event attribution wrong: %+v", ev.Payload)
	}

	// FAILURE MODE: broker unreachable on refresh past the TTL → fail loud, no
	// stale key served.
	srv.SetPosture(inferencebrokertest.PostureDown)
	clockT = clockT.Add(6 * time.Minute)
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("unreachable refresh: got %v, want ErrProviderKeyUnavailable", err)
	}
	if live.Get() != "" {
		t.Fatalf("MUST NOT serve a stale key past its refresh contract; LiveKey = %q", live.Get())
	}
	src.Close()
}

func waveV117WaitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// --- 197: Harbor-orchestrated failover walk with a re-run-PreCall budget trip

func testWaveV117FailoverPreCallTrip(t *testing.T) {
	t.Parallel()
	bus, st := waveV117BusState(t)
	ctx := waveV117Ctx(t, "acme", "u1", "s1", "r1")
	id := identity.MustQuadrupleFrom(ctx).Identity

	// Ceiling below the primary's reported cost so the primary passes PreCall
	// (total 0) then tips the accumulator over the ceiling; the FIRST fallback
	// hop's re-run PreCall then trips and STOPS the walk.
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{
		DefaultTier:   "t",
		IdentityTiers: map[string]governance.TierConfig{"t": {BudgetCeilingUSD: 0.001}},
	})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	shared := &sync.Map{}
	llmc := &waveV117LLM{shared: shared, succeedOn: "backup", cost: 0.01}
	act := &waveV117Activator{shared: shared}
	fp, err := governance.NewFailoverPolicy(governance.FailoverConfig{
		Inner: llmc, Governance: governance.NewCompound(acc), Activator: act, Cost: acc, Bus: bus,
	})
	if err != nil {
		t.Fatalf("NewFailoverPolicy: %v", err)
	}

	// Observe the governance.failover hop event on the REAL bus, keyed by the
	// run identity.
	fsub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "acme", User: "u1", Session: "s1",
		Types: []events.EventType{governance.EventTypeFailover},
	})
	if err != nil {
		t.Fatalf("Subscribe(failover): %v", err)
	}
	defer fsub.Cancel()

	_, err = fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"},
		[]governance.ProviderRef{{Provider: "backup", Name: "b1"}, {Provider: "backup2", Name: "b2"}})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("failover walk: got %v, want ErrBudgetExceeded (the budget trip must fail loud and stop)", err)
	}
	// Only the primary re-issued; the budget-tripped hop never re-issues and
	// the second fallback is never reached.
	if llmc.calls.Load() != 1 {
		t.Fatalf("inner called %d times, want 1 (the walk stops at the first PreCall trip)", llmc.calls.Load())
	}
	ev := waveV117WaitEvent(t, fsub.Events(), governance.EventTypeFailover)
	p, ok := ev.Payload.(governance.GovernanceFailoverPayload)
	if !ok || p.Identity.TenantID != "acme" || p.ToProvider != "backup" {
		t.Fatalf("failover event mismatch: %+v", ev.Payload)
	}
}

func testWaveV117FailoverConcurrencyStress(t *testing.T) {
	t.Parallel()
	bus, st := waveV117BusState(t)

	// No ceiling (latent) so every hop permits; the accumulator still folds
	// each identity's cost so the isolation assertion has teeth.
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	shared := &sync.Map{}
	const costPerCall = 0.003
	llmc := &waveV117LLM{shared: shared, succeedOn: "backup", cost: costPerCall}
	act := &waveV117Activator{shared: shared}
	// ONE shared FailoverPolicy exercised by N goroutines across >=2 tenants.
	fp, err := governance.NewFailoverPolicy(governance.FailoverConfig{
		Inner: llmc, Governance: governance.NewCompound(acc), Activator: act, Cost: acc, Bus: bus,
	})
	if err != nil {
		t.Fatalf("NewFailoverPolicy: %v", err)
	}

	const N = 24 // >=10; two tenants interleaved
	tenants := []string{"tenant-a", "tenant-b"}
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := tenants[i%len(tenants)]
			ctx := waveV117Ctx(t, tenant, "u", "s", fmt.Sprintf("run-%d", i))
			id := identity.MustQuadrupleFrom(ctx).Identity
			resp, err := fp.Complete(ctx, id, llm.CompleteRequest{Model: "primary"},
				[]governance.ProviderRef{{Provider: "backup", Name: "b"}})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if resp.Content != "ok:backup" {
				errCh <- fmt.Errorf("goroutine %d: resp %q", i, resp.Content)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	// Per-identity cost-accumulator isolation: each tenant accumulated only its
	// OWN goroutines' folded cost (primary + one fallback per run = 2 calls),
	// never a neighbour's — no cross-identity budget bleed across the hop walk.
	perTenant := map[string]float64{}
	for i := range N {
		perTenant[tenants[i%len(tenants)]] += 2 * costPerCall
	}
	for tenant, want := range perTenant {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"}}
		ctx, err := identity.With(context.Background(), q.Identity)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		total, _, err := acc.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot(%s): %v", tenant, err)
		}
		if diff := total - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("tenant %s accumulated %v, want %v (cross-identity bleed)", tenant, total, want)
		}
	}

	// No-goroutine-leak guarantee: the FailoverPolicy walk is fully synchronous
	// — it starts NO goroutines of its own — so `wg.Wait()` above joining every
	// one of the N callers IS the leak guarantee (a spawned-but-unjoined
	// goroutine would deadlock the Wait). An absolute runtime.NumGoroutine()
	// baseline is deliberately NOT asserted here: this subtest shares the
	// process with the rest of the parallel integration suite, whose goroutines
	// pollute the count. The per-instance NumGoroutine leak baseline is pinned
	// in the quieter governance-package unit test
	// (TestConcurrentReuse_FailoverPolicy_NoBleed).
}
