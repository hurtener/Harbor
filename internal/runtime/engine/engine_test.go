package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// --- helpers ---

func passthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

func ident(t, u, s string) identity.Identity {
	return identity.Identity{TenantID: t, UserID: u, SessionID: s}
}

func envFor(id identity.Identity, runID string) messages.Envelope {
	return messages.Envelope{
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
		SessionID: id.SessionID,
		RunID:     runID,
	}
}

// linearGraph builds adj A -> B -> C for tests that need a real
// graph topology with an inlet, intermediate, and outlet.
func linearGraph() []engine.Adjacency {
	a := engine.Node{Name: "A", Func: passthrough}
	b := engine.Node{Name: "B", Func: passthrough}
	c := engine.Node{Name: "C", Func: passthrough}
	return []engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: []engine.Node{c}},
		{From: c, To: nil}, // outlet
	}
}

// --- unit: New / construction validation ---

func TestEngine_New_HappyPath_LinearGraph(t *testing.T) {
	t.Parallel()
	e, err := engine.New(linearGraph())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e == nil {
		t.Fatal("New returned nil engine")
	}
}

func TestEngine_New_RejectsEmptyAdjacencies(t *testing.T) {
	t.Parallel()
	_, err := engine.New(nil)
	if !errors.Is(err, engine.ErrEmptyAdjacencies) {
		t.Fatalf("err=%v, want ErrEmptyAdjacencies", err)
	}
}

func TestEngine_New_RejectsCycle_WithoutAllowCycle(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	b := engine.Node{Name: "B", Func: passthrough}
	// A -> B -> A
	_, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: []engine.Node{a}},
	})
	if !errors.Is(err, engine.ErrCycleDetected) {
		t.Fatalf("err=%v, want ErrCycleDetected", err)
	}
}

func TestEngine_New_AcceptsCycle_WithAllowCycle(t *testing.T) {
	t.Parallel()
	in := engine.Node{Name: "Inlet", Func: passthrough}
	a := engine.Node{Name: "A", Func: passthrough, AllowCycle: true}
	b := engine.Node{Name: "B", Func: passthrough, AllowCycle: true}
	c := engine.Node{Name: "C", Func: passthrough}
	// Inlet -> A; A <-> B (legitimate cycle); A -> C (outlet).
	_, err := engine.New([]engine.Adjacency{
		{From: in, To: []engine.Node{a}},
		{From: a, To: []engine.Node{b, c}},
		{From: b, To: []engine.Node{a}},
		{From: c, To: nil},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestEngine_New_RejectsCycle_ListsCyclePath(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	b := engine.Node{Name: "B", Func: passthrough}
	c := engine.Node{Name: "C", Func: passthrough}
	// A -> B -> C -> A
	_, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: []engine.Node{c}},
		{From: c, To: []engine.Node{a}},
	})
	if !errors.Is(err, engine.ErrCycleDetected) {
		t.Fatalf("err=%v, want ErrCycleDetected", err)
	}
	for _, name := range []string{"A", "B", "C"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error message missing %q: %s", name, err.Error())
		}
	}
}

func TestEngine_New_RejectsDuplicateNodeName(t *testing.T) {
	t.Parallel()
	a1 := engine.Node{Name: "A", Func: passthrough}
	a2 := engine.Node{Name: "A", Func: passthrough, AllowCycle: true}
	b := engine.Node{Name: "B", Func: passthrough}
	_, err := engine.New([]engine.Adjacency{
		{From: a1, To: []engine.Node{b}},
		{From: a2, To: []engine.Node{b}},
	})
	if !errors.Is(err, engine.ErrDuplicateNodeName) {
		t.Fatalf("err=%v, want ErrDuplicateNodeName", err)
	}
}

func TestEngine_New_RejectsInvalidQueueSize(t *testing.T) {
	t.Parallel()
	_, err := engine.New(linearGraph(), engine.WithQueueSize(0))
	if !errors.Is(err, engine.ErrInvalidQueueSize) {
		t.Fatalf("err=%v, want ErrInvalidQueueSize", err)
	}
	_, err = engine.New(linearGraph(), engine.WithQueueSize(-3))
	if !errors.Is(err, engine.ErrInvalidQueueSize) {
		t.Fatalf("err=%v, want ErrInvalidQueueSize", err)
	}
}

func TestEngine_New_RejectsChannelOverrideForUnknownNode(t *testing.T) {
	t.Parallel()
	_, err := engine.New(linearGraph(),
		engine.WithChannelOverride(engine.NodeRef{Name: "ghost"}, engine.NodeRef{Name: "B"}, 8),
	)
	if !errors.Is(err, engine.ErrNodeNotFound) {
		t.Fatalf("err=%v, want ErrNodeNotFound", err)
	}
}

// --- unit: Emit / Fetch / EmitTo ---

func TestEngine_Emit_RejectsEmptyIdentity(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	cases := []messages.Envelope{
		{}, // entirely empty
		{Headers: messages.Headers{TenantID: "T"}, SessionID: "S"}, // missing user
		{Headers: messages.Headers{TenantID: "T", UserID: "U"}},     // missing session
	}
	for i, env := range cases {
		err := e.Emit(context.Background(), env)
		if !errors.Is(err, engine.ErrIdentityRequired) {
			t.Errorf("case %d: err=%v, want ErrIdentityRequired", i, err)
		}
		if !errors.Is(err, identity.ErrIdentityIncomplete) {
			t.Errorf("case %d: err should also satisfy errors.Is(identity.ErrIdentityIncomplete); got %v", i, err)
		}
	}
}

func TestEngine_EmitFetch_RoundTrip(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	in := envFor(id, "R-1")
	in.Payload = "hello"
	if err := e.Emit(context.Background(), in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := e.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Payload != "hello" {
		t.Errorf("Payload=%v, want %q", got.Payload, "hello")
	}
	if got.RunID != "R-1" {
		t.Errorf("RunID=%q, want %q", got.RunID, "R-1")
	}
	if got.Identity().TenantID != "T" {
		t.Errorf("Tenant=%q, want T", got.Identity().TenantID)
	}
}

func TestEngine_EmitTo_RoutesToInletByName(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	b := engine.Node{Name: "B", Func: passthrough}
	c := engine.Node{Name: "C", Func: passthrough}
	// Two inlets: A and B both feed C.
	e, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{c}},
		{From: b, To: []engine.Node{c}},
		{From: c, To: nil},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	in := envFor(id, "R-1")
	in.Payload = "via-B"
	if err := e.EmitTo(context.Background(), in, engine.NodeRef{Name: "B"}); err != nil {
		t.Fatalf("EmitTo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := e.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Payload != "via-B" {
		t.Errorf("Payload=%v", got.Payload)
	}
}

func TestEngine_EmitTo_RejectsNonInlet(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()
	id := ident("T", "U", "S")
	in := envFor(id, "R-1")
	err := e.EmitTo(context.Background(), in, engine.NodeRef{Name: "B"})
	if !errors.Is(err, engine.ErrNodeNotFound) {
		t.Fatalf("err=%v, want ErrNodeNotFound (B is not an inlet)", err)
	}
}

func TestNode_Ref_ReturnsNodeRef(t *testing.T) {
	t.Parallel()
	n := engine.Node{Name: "X", Func: passthrough}
	ref := n.Ref()
	if ref.Name != "X" {
		t.Errorf("Ref().Name=%q, want X", ref.Name)
	}
}

// TestEngine_NodeContext_EmitFanOut covers NodeContext.Emit (the
// blocking path the worker uses to fan out an envelope on multiple
// outgoing edges). Constructs a 1-to-2 fan-out graph and asserts
// both downstream nodes process the envelope.
func TestEngine_NodeContext_EmitFanOut(t *testing.T) {
	t.Parallel()
	// A fans out to B and C; each node uses NodeContext.Emit
	// implicitly via the worker's emitFromNode path.
	a := engine.Node{Name: "A", Func: passthrough}
	b := engine.Node{Name: "B", Func: passthrough}
	c := engine.Node{Name: "C", Func: passthrough}
	e, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{b, c}},
		{From: b, To: nil},
		{From: c, To: nil},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()
	id := ident("T", "U", "S")
	in := envFor(id, "R")
	in.Payload = "fan"
	if err := e.Emit(context.Background(), in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Two egress envelopes (one per branch).
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		got, err := e.Fetch(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
		if got.Payload != "fan" {
			t.Errorf("Fetch %d: payload=%v", i, got.Payload)
		}
	}
}

// TestEngine_NodeContext_EmitNoWait covers EmitNoWait via a custom
// NodeFunc that calls it directly. With a deliberately tiny
// downstream queue + a blocked consumer, EmitNoWait should return
// ErrChannelFull after the queue fills.
//
// The sink is gated on a release channel so the queue cannot drain
// while src is mid-emit. Without that gate, the Go runtime schedules
// the sink worker between src's emits (especially on macOS), the
// queue keeps draining, and all 3 EmitNoWait calls return nil — a
// race-shaped flake. Closing the release channel after the assertions
// lets the test shut down cleanly.
func TestEngine_NodeContext_EmitNoWait(t *testing.T) {
	t.Parallel()
	const queueSize = 1
	emitErr := make(chan error, 4)
	release := make(chan struct{})
	source := engine.Node{Name: "src", Func: func(_ context.Context, in messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		// Three EmitNoWait calls; only the first slips into the
		// 1-slot downstream queue (sink is blocked on `release`),
		// the next two MUST return ErrChannelFull.
		for i := 0; i < 3; i++ {
			emitErr <- nctx.EmitNoWait(in)
		}
		// Return zero envelope so the worker doesn't ALSO emit;
		// otherwise we'd race the no-wait emits with the regular emit.
		return messages.Envelope{}, nil
	}}
	sink := engine.Node{Name: "sink", Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		// Block until the test releases us OR the engine shuts down.
		// Observing ctx is what lets Stop join the worker cleanly when
		// the test exits before close(release) — Engine.Stop cancels
		// the worker ctx on shutdown.
		select {
		case <-release:
			return in, nil
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		}
	}}
	// Release the sink before the engine's Stop runs so the queue
	// drains and Stop's worker-join completes cleanly. defer + t.Cleanup
	// LIFO would otherwise have Stop fire while sink is still parked.
	defer close(release)
	e, err := engine.New([]engine.Adjacency{
		{From: source, To: []engine.Node{sink}},
		{From: sink, To: nil},
	}, engine.WithChannelOverride(source.Ref(), sink.Ref(), queueSize))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	if err := e.Emit(context.Background(), envFor(id, "R")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Wait for the source NodeFunc to run.
	gotFull := 0
	gotNil := 0
	for i := 0; i < 3; i++ {
		select {
		case err := <-emitErr:
			if errors.Is(err, engine.ErrChannelFull) {
				gotFull++
			} else if err == nil {
				gotNil++
			} else {
				t.Errorf("unexpected EmitNoWait err: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("source NodeFunc didn't fire")
		}
	}
	if gotFull == 0 {
		t.Errorf("expected at least one ErrChannelFull, got %d nil / %d full", gotNil, gotFull)
	}
}

// TestEngine_OutletOnlyNode_PassThrough covers the isOutletOnly
// branch in New: a node that appears only as a To-target (no From
// adjacency, no children) is allowed and pass-through-emits to the
// outlet via the worker's nil-Func branch.
func TestEngine_OutletOnlyNode_PassThrough(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	// "Out" appears ONLY as a child of A — no Func required, no
	// outgoing edges; the worker treats it as outlet-only.
	out := engine.Node{Name: "Out"}
	e, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{out}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()
	id := ident("T", "U", "S")
	if err := e.Emit(context.Background(), envFor(id, "R")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// TestEngine_WithErrorEmissionToEgress covers the option's getter
// path. Phase 10 ships the toggle but no behavior diverges yet (the
// worker still logs to slog regardless); the test pins the option
// constructs successfully.
func TestEngine_WithErrorEmissionToEgress(t *testing.T) {
	t.Parallel()
	e, err := engine.New(linearGraph(), engine.WithErrorEmissionToEgress(true))
	if err != nil {
		t.Fatalf("New with error-to-egress: %v", err)
	}
	if e == nil {
		t.Fatal("nil engine")
	}
}

// --- unit: Stop / lifecycle ---

func TestEngine_Stop_JoinsWorkers(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	id := ident("T", "U", "S")
	for i := 0; i < 5; i++ {
		_ = e.Emit(context.Background(), envFor(id, "R"))
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop, every operation returns ErrEngineStopped.
	if err := e.Emit(context.Background(), envFor(id, "R")); !errors.Is(err, engine.ErrEngineStopped) {
		t.Errorf("post-Stop Emit err=%v, want ErrEngineStopped", err)
	}
	if _, err := e.Fetch(context.Background()); !errors.Is(err, engine.ErrEngineStopped) {
		t.Errorf("post-Stop Fetch err=%v, want ErrEngineStopped", err)
	}
}

func TestEngine_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Stop(stopCtx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := e.Stop(stopCtx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestEngine_Run_RejectsDoubleRun(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()
	if err := e.Run(context.Background()); err == nil {
		t.Error("second Run accepted; expected error")
	}
}

// --- unit: deadline ---

func TestEngine_DeadlineExceeded_DropsAndContinues(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	past := time.Now().Add(-time.Hour)
	in := envFor(id, "R-1")
	in.Payload = "expired"
	in.DeadlineAt = &past

	if err := e.Emit(context.Background(), in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// No envelope reaches Fetch because the worker sees the expired
	// DeadlineAt and drops. Send a follow-up envelope and assert
	// that one passes through.
	in2 := envFor(id, "R-2")
	in2.Payload = "fresh"
	if err := e.Emit(context.Background(), in2); err != nil {
		t.Fatalf("Emit2: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := e.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Payload != "fresh" {
		t.Errorf("Payload=%v, want fresh (expired one was dropped)", got.Payload)
	}
}

// --- goroutine leak ---

func TestEngine_NoGoroutineLeak_AfterStop(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, e engine.Engine)
	}{
		{"idle", func(_ *testing.T, _ engine.Engine) {}},
		{"mid-run", func(t *testing.T, e engine.Engine) {
			id := ident("T", "U", "S")
			for i := 0; i < 5; i++ {
				_ = e.Emit(context.Background(), envFor(id, "R"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for i := 0; i < 5; i++ {
				_, _ = e.Fetch(ctx)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			e, err := engine.New(linearGraph())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := e.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			tc.run(t, e)
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.Stop(stopCtx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			// Allow short settling; tolerance of +3 absorbs Go's
			// parked-goroutine retirement latency.
			deadline := time.Now().Add(2 * time.Second)
			for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
				runtime.Gosched()
				time.Sleep(10 * time.Millisecond)
			}
			if delta := runtime.NumGoroutine() - baseline; delta > 3 {
				t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
					baseline, runtime.NumGoroutine(), delta)
			}
		})
	}
}

// --- cross-tenant isolation (passes by construction in Phase 10;
// pinned so a future regression is caught) ---

func TestEngine_CrossTenant_NoBleed(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	const tenants = 4
	const perTenant = 16

	type observed struct {
		fromTenant string
		runID      string
	}
	results := make(chan observed, tenants*perTenant)

	var wg sync.WaitGroup
	for i := 0; i < tenants; i++ {
		wg.Add(1)
		go func(t int) {
			defer wg.Done()
			id := ident(
				fmt.Sprintf("t-%d", t),
				fmt.Sprintf("u-%d", t),
				fmt.Sprintf("s-%d", t),
			)
			for j := 0; j < perTenant; j++ {
				env := envFor(id, fmt.Sprintf("r-%d-%d", t, j))
				env.Payload = fmt.Sprintf("t=%d j=%d", t, j)
				_ = e.Emit(context.Background(), env)
			}
		}(i)
	}
	wg.Wait()

	// Drain.
	for i := 0; i < tenants*perTenant; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		got, err := e.Fetch(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
		results <- observed{
			fromTenant: got.Identity().TenantID,
			runID:      got.RunID,
		}
	}
	close(results)

	// Every result should have a tenant prefix consistent with
	// its run ID. The engine itself doesn't bleed because each
	// envelope is keyed end-to-end by its own quadruple — but the
	// test pins this so a future regression (e.g. a worker that
	// stamps identity from itself) is caught.
	seen := make(map[string]int)
	for r := range results {
		// runID form is r-T-J — extract T.
		parts := strings.Split(r.runID, "-")
		if len(parts) != 3 {
			t.Errorf("runID %q malformed", r.runID)
			continue
		}
		expectedTenant := "t-" + parts[1]
		if r.fromTenant != expectedTenant {
			t.Errorf("envelope's tenant=%q, expected=%q for runID=%q",
				r.fromTenant, expectedTenant, r.runID)
		}
		seen[r.fromTenant]++
	}
	for i := 0; i < tenants; i++ {
		key := fmt.Sprintf("t-%d", i)
		if seen[key] != perTenant {
			t.Errorf("tenant %s saw %d envelopes, want %d", key, seen[key], perTenant)
		}
	}
}

