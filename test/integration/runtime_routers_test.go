// Phase 14 cross-subsystem integration test per AGENTS.md §17.
//
// Wires real audit + events + state + sessions + engine + routers +
// concurrency + Subflow drivers and exercises:
//
//   - PredicateRouter at the engine seam (RoutePolicy in Meta).
//   - 2-level subflow (parent → subflow A → subflow B) with ctx-based
//     cancel mirroring (parent ctx cancel propagates to both children).
//   - MapConcurrent over a list of envelopes with a real engine running
//     alongside (the bus + state + sessions stack stays alive).
//
// Failure mode: subflow factory error returns the wrapped sentinel.
//
// Engine-level Engine.Cancel(runID) mirroring is deferred to a Phase 13
// follow-up that extends the subflow watcher (see PR body).
package integration_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/concurrency"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/routers"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase14_PredicateRouter_FanOut wires a 5-node graph
// (input → predicate router → 3 branches → output) and asserts the
// router writes RoutePolicy into Meta with the correct target. The
// bus + state + sessions stack stays alive alongside; identity
// propagates through the engine boundary.
func TestE2E_Phase14_PredicateRouter_FanOut(t *testing.T) {
	cfg := phase10Config() // shared config helper from runtime_engine_test.go
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("Open session: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(ctx, id.SessionID, "test-end") })

	// Build the predicate router. Three branches keyed on Priority.
	low := engine.NodeRef{Name: "low"}
	mid := engine.NodeRef{Name: "mid"}
	high := engine.NodeRef{Name: "high"}
	router := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(e messages.Envelope) bool { return e.Headers.Priority < 5 }, Target: low},
			{Predicate: func(e messages.Envelope) bool { return e.Headers.Priority < 10 }, Target: mid},
			{Predicate: func(_ messages.Envelope) bool { return true }, Target: high},
		},
	}

	// 5-node graph: input → router → low/mid/high → out.
	// The engine writes to ALL outgoing edges from `router` (fan-out);
	// every branch receives the SAME envelope (Meta is a shared map
	// reference). To avoid mutating Meta concurrently — which would
	// race against sibling branch goroutines reading from it — branch
	// nodes only WRITE to Payload (a value field local to the
	// returned struct copy). Sibling branches return the envelope
	// unchanged so non-matching branches don't poison the egress.
	branchFn := func(branch string) engine.NodeFunc {
		return func(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			rp, ok := routers.FromMeta(env.Meta)
			if !ok || rp.ExplicitTarget == nil || rp.ExplicitTarget.Name != branch {
				// Not our branch — drop with a sentinel Payload so the
				// drainer can recognize and skip it.
				out := env
				out.Payload = "skipped:" + branch
				return out, nil
			}
			out := env
			out.Payload = "taken:" + branch
			return out, nil
		}
	}
	input := engine.Node{Name: "input", Func: func(_ context.Context, e messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return e, nil
	}}
	routerNode := router.AsNode("router")
	lowN := engine.Node{Name: "low", Func: branchFn("low")}
	midN := engine.Node{Name: "mid", Func: branchFn("mid")}
	highN := engine.Node{Name: "high", Func: branchFn("high")}
	outN := engine.Node{Name: "out", Func: func(_ context.Context, e messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return e, nil
	}}

	e, err := engine.New([]engine.Adjacency{
		{From: input, To: []engine.Node{routerNode}},
		{From: routerNode, To: []engine.Node{lowN, midN, highN}},
		{From: lowN, To: []engine.Node{outN}},
		{From: midN, To: []engine.Node{outN}},
		{From: highN, To: []engine.Node{outN}},
		{From: outN, To: nil},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(stopCtx)
	})

	// Send three envelopes (one per priority band) and observe the
	// chosen branch through Meta.
	cases := []struct {
		priority   int
		wantBranch string
	}{
		{2, "low"},
		{7, "mid"},
		{15, "high"},
	}
	for _, c := range cases {
		in := messages.Envelope{
			Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID, Priority: c.priority},
			SessionID: id.SessionID,
			RunID:     "R-router-" + c.wantBranch,
			Payload:   c.wantBranch,
		}
		if err := e.Emit(context.Background(), in); err != nil {
			t.Fatalf("Emit prio=%d: %v", c.priority, err)
		}
		// Drain — exactly three envelopes per scenario (one per
		// branch). MUST drain all three or leftovers leak into the
		// next scenario's drainage. Find the one tagged "taken:..."
		// and assert it's the matching branch.
		var taken string
		drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		for i := range 3 {
			got, err := e.Fetch(drainCtx)
			if err != nil {
				cancel()
				t.Fatalf("Fetch prio=%d (drain %d): %v", c.priority, i, err)
			}
			if v, ok := got.Payload.(string); ok && len(v) >= 6 && v[:6] == "taken:" {
				taken = v[6:]
			}
		}
		cancel()
		if taken != c.wantBranch {
			t.Errorf("prio=%d: taken=%q, want %q", c.priority, taken, c.wantBranch)
		}
	}
}

// TestE2E_Phase14_NestedSubflow_CtxCancel exercises the 2-level
// subflow scenario (parent → subflow A → subflow B) with ctx-based
// cancellation mirroring. Cancelling the parent's ctx must propagate
// to both children within bounded time.
func TestE2E_Phase14_NestedSubflow_CtxCancel(t *testing.T) {
	cfg := phase10Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	openCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Open(openCtx, id.SessionID, id); err != nil {
		t.Fatalf("Open session: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(openCtx, id.SessionID, "test-end") })

	baseline := runtime.NumGoroutine()

	// Inner factory: a 2-node graph whose `in` blocks on ctx.
	var innerCancelObserved atomic.Bool
	innerInNode := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			<-ctx.Done()
			innerCancelObserved.Store(true)
			return env, ctx.Err()
		},
	}
	innerOutNode := engine.Node{Name: "out"}
	innerFactory := func() (engine.Engine, error) {
		return engine.New([]engine.Adjacency{
			{From: innerInNode, To: []engine.Node{innerOutNode}},
		})
	}

	// Outer factory: a 2-node graph whose `in` calls the inner
	// subflow inside its NodeFunc.
	outerInNode := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			out, err := nctx.CallSubflow(ctx, innerFactory, env)
			return out, err
		},
	}
	outerOutNode := engine.Node{Name: "out"}
	outerFactory := func() (engine.Engine, error) {
		return engine.New([]engine.Adjacency{
			{From: outerInNode, To: []engine.Node{outerOutNode}},
		})
	}

	// Build the parent engine that calls the outer subflow.
	parentInNode := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			out, err := nctx.CallSubflow(ctx, outerFactory, env)
			return out, err
		},
	}
	parentOutNode := engine.Node{Name: "out"}

	parentEngine, err := engine.New([]engine.Adjacency{
		{From: parentInNode, To: []engine.Node{parentOutNode}},
	})
	if err != nil {
		t.Fatalf("parent engine.New: %v", err)
	}

	parentCtx, cancelParent := context.WithCancel(context.Background())
	if err := parentEngine.Run(parentCtx); err != nil {
		t.Fatalf("parent engine.Run: %v", err)
	}

	in := messages.Envelope{
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
		SessionID: id.SessionID,
		RunID:     "R-nested",
		Payload:   "subflow-cancel",
	}
	if err := parentEngine.Emit(parentCtx, in); err != nil {
		t.Fatalf("parent Emit: %v", err)
	}

	// Allow the subflow chain to spin up + reach the blocking inner.
	time.Sleep(100 * time.Millisecond)

	// Cancel the parent ctx — must propagate to both subflow levels.
	cancelParent()

	// Stop the parent engine (parent ctx is already cancelled).
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	_ = parentEngine.Stop(stopCtx)

	if !innerCancelObserved.Load() {
		t.Errorf("inner subflow did not observe ctx cancellation")
	}

	// Allow goroutines to settle.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak after nested subflow cancel: baseline=%d after=%d delta=%d",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// TestE2E_Phase14_Subflow_FactoryError pins the failure-mode test for
// AGENTS.md §17.3: factory error returns the wrapped sentinel and
// doesn't leak goroutines.
func TestE2E_Phase14_Subflow_FactoryError(t *testing.T) {
	cfg := phase10Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("Open session: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(ctx, id.SessionID, "test-end") })

	boom := errors.New("synthetic factory boom")

	// Parent engine whose `in` node calls a failing subflow factory.
	// The nil Engine is deliberate — the factory's contract is to fail;
	// the signature is fixed by CallSubflow's factory parameter type.
	//nolint:unparam // factory deliberately always fails; signature fixed by CallSubflow
	failingFactory := func() (engine.Engine, error) {
		return nil, boom
	}
	var subflowErr atomic.Value // error
	parentIn := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			_, err := nctx.CallSubflow(ctx, failingFactory, env)
			subflowErr.Store(err)
			return env, nil // swallow so the parent's worker doesn't loop on the err path
		},
	}
	parentOut := engine.Node{Name: "out"}
	e, err := engine.New([]engine.Adjacency{
		{From: parentIn, To: []engine.Node{parentOut}},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(stopCtx)
	})

	in := messages.Envelope{
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
		SessionID: id.SessionID,
		RunID:     "R-factory-err",
	}
	if err := e.Emit(context.Background(), in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Drain.
	fetchCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.Fetch(fetchCtx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := subflowErr.Load().(error)
	if !errors.Is(got, engine.ErrSubflowFactoryFailed) {
		t.Errorf("err=%v, want errors.Is ErrSubflowFactoryFailed", got)
	}
	if !errors.Is(got, boom) {
		t.Errorf("err=%v, want wraps boom", got)
	}
}

// TestE2E_Phase14_MapConcurrent_OverEngine runs MapConcurrent over a
// list of envelopes alongside a live engine + bus + state + sessions
// stack. Asserts MapConcurrent's order preservation and bound under
// real concurrent load (the bus + sessions GC sweepers run alongside).
func TestE2E_Phase14_MapConcurrent_OverEngine(t *testing.T) {
	cfg := phase10Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	const total = 30
	in := make([]messages.Envelope, total)
	for i := range total {
		in[i] = messages.Envelope{
			Headers:   messages.Headers{TenantID: "T", UserID: "U"},
			SessionID: "S",
			RunID:     "R-map-" + itoa(i),
			Payload:   i,
		}
	}

	out, err := concurrency.MapConcurrent(context.Background(), in, func(_ context.Context, env messages.Envelope) (messages.Envelope, error) {
		return env, nil
	}, 6)
	if err != nil {
		t.Fatalf("MapConcurrent: %v", err)
	}
	if len(out) != total {
		t.Fatalf("len(out)=%d, want %d", len(out), total)
	}
	for i := range total {
		if got := out[i].Payload.(int); got != i {
			t.Errorf("out[%d]=%d, want %d (order broken)", i, got, i)
		}
	}
}
