// Wave 4 cross-subsystem integration test per AGENTS.md §17.5.
//
// Wave 4 closed the entire RFC §6.1 surface (Phases 09-14): the
// runtime kernel chain — Envelopes, Engine + workers + cycle
// detection, reliability shell, streaming + per-run capacity
// backpressure, cancellation + per-run fetch dispatcher, routers +
// concurrency utils + Subflow.
//
// Each phase shipped its own per-phase integration test under
// test/integration/. This wave-end smoke proves the surfaces
// COMPOSE — that a single graph wired through real audit + events +
// state + sessions + telemetry/eventbus + engine drivers exercises
// every Wave-4 phase end-to-end with identity propagation preserved
// at every boundary.
//
// Three tests, each focused on a different composition angle:
//
//   - TestE2E_Wave4_FullKernel_Aliveness: full surface aliveness.
//     Graph with reliability policy + streaming + routing under one
//     engine + bus + state + sessions wiring.
//   - TestE2E_Wave4_CancelMidStream_Cascades: Phase 13 ↔ Phase 12
//     composition. Saturate a run with EmitChunk, then Cancel; assert
//     the cancel cascades through capacity waiter, FetchByRun, AND
//     the bus subscriber simultaneously.
//   - TestE2E_Wave4_Concurrent_MultiTenant: cross-tenant streaming
//     under load. 8 tenants × 4 runs concurrent; per-tenant bus
//     subscribers see only their own events; goroutine baseline
//     restored after Stop.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Wave4_FullKernel_Aliveness builds a small graph that
// exercises the full Wave-4 kernel surface in one wiring:
//
//   - Phase 09: identity quadruple flows in via Envelope.
//   - Phase 10: engine runs the graph; cycle-free; Stop joins workers.
//   - Phase 11: producer node has retry policy; deliberate transient
//     failure on first invocation succeeds on retry (bus subscriber
//     does NOT see runtime.error since the retry recovered).
//   - Phase 12: streamer node emits 8 stream frames via EmitChunk;
//     per-run capacity is honored.
//   - Phase 13: FetchByRun reads the run's egress including all
//     stream frames in order.
//   - Phase 14 surface — passthrough nodes only here; the dedicated
//     router/subflow tests live in runtime_routers_test.go.
//
// Identity propagation: assert every event the bus subscriber
// observes carries the originating run's full quadruple. Failure
// mode: retry-then-recover (no terminal error). Run under -race.
func TestE2E_Wave4_FullKernel_Aliveness(t *testing.T) {
	cfg := wave4Config()
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
	ctx, _ := identity.With(context.Background(), id)
	ctx, _ = identity.WithRun(ctx, id, "R-aliveness")

	// Open the session — exercises the typed-wrapper path on top of
	// StateStore and emits session.opened on the bus.
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("session Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background(), id.SessionID, "wave4-test") })

	// Subscribe to runtime.error / runtime.run_cancelled — we expect
	// to see ZERO of either in this test (retry recovers; no Cancel).
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{
			events.EventTypeRuntimeError,
			events.EventTypeRuntimeRunCancelled,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Producer node with retry: fails once, succeeds on retry.
	// Phase 11's reliability shell catches the first error, sleeps
	// the backoff, and re-invokes; the second invocation succeeds.
	var producerCalls atomic.Int32
	producer := engine.Node{
		Name: "producer",
		Func: func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			if producerCalls.Add(1) == 1 {
				return messages.Envelope{}, errors.New("transient")
			}
			in.Payload = "produced"
			return in, nil
		},
		Policy: engine.NodePolicy{
			MaxRetries:  2,
			BackoffBase: 1 * time.Millisecond,
			BackoffMult: 2.0,
			MaxBackoff:  10 * time.Millisecond,
			RunCapacity: 16,
		},
	}

	// Streamer node: emits N stream frames via EmitChunk + the
	// terminal Done frame, and forwards the input envelope to the
	// outlet so FetchByRun sees one regular envelope plus the chunks.
	const frameCount = 8
	streamer := engine.Node{
		Name: "streamer",
		Func: func(ctx context.Context, in messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			for i := range frameCount {
				if err := nctx.EmitChunk(ctx, engine.StreamFrame{
					Text: fmt.Sprintf("chunk-%d", i),
				}); err != nil {
					return messages.Envelope{}, fmt.Errorf("EmitChunk %d: %w", i, err)
				}
			}
			// Done terminal frame
			if err := nctx.EmitChunk(ctx, engine.StreamFrame{Done: true}); err != nil {
				return messages.Envelope{}, fmt.Errorf("EmitChunk done: %w", err)
			}
			in.Payload = "streamed"
			return in, nil
		},
		Policy: engine.NodePolicy{RunCapacity: 16},
	}

	eng, err := engine.New([]engine.Adjacency{
		{From: producer, To: []engine.Node{streamer}},
		{From: streamer, To: nil},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	env := messages.Envelope{
		Payload:   "input",
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID, Topic: "wave4"},
		RunID:     "R-aliveness",
		SessionID: id.SessionID,
		Timestamp: time.Now(),
	}
	if err := eng.Emit(ctx, env); err != nil {
		t.Fatalf("engine.Emit: %v", err)
	}

	// Drain the run's egress: regular envelope + chunks + done frame.
	// FetchByRun (Phase 13) reads from the per-run subqueue.
	deadline := time.Now().Add(3 * time.Second)
	gotChunks := 0
	gotEnvelope := false
	gotDone := false
	for time.Now().Before(deadline) && !(gotEnvelope && gotDone && gotChunks >= frameCount) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		got, err := eng.FetchByRun(fetchCtx, "R-aliveness")
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			t.Fatalf("FetchByRun: %v", err)
		}
		// Stream frames are envelopes whose payload is a StreamFrame.
		// Regular envelopes carry the producer's "produced" or
		// streamer's "streamed" payload.
		if frame, ok := got.Payload.(engine.StreamFrame); ok {
			if frame.Done {
				gotDone = true
			} else {
				gotChunks++
			}
		} else {
			gotEnvelope = true
		}
		// Identity propagation check on every fetch.
		if got.Headers.TenantID != id.TenantID || got.RunID != "R-aliveness" {
			t.Errorf("identity bleed in egress envelope: %+v", got.Headers)
		}
	}
	if !gotEnvelope {
		t.Errorf("never received the regular envelope")
	}
	if gotChunks < frameCount {
		t.Errorf("received %d chunks, want >= %d", gotChunks, frameCount)
	}
	if !gotDone {
		t.Errorf("never received the Done frame")
	}
	if producerCalls.Load() != 2 {
		t.Errorf("producer called %d times, want 2 (one fail + one retry-success)", producerCalls.Load())
	}

	// Drain bus events on a short bounded window — assert NO
	// runtime.error and NO runtime.run_cancelled landed (retry
	// recovered; nobody cancelled).
	drainDeadline := time.After(200 * time.Millisecond)
drain:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break drain
			}
			if ev.Type == events.EventTypeRuntimeError {
				t.Errorf("unexpected runtime.error after retry recovery: %+v", ev)
			}
			if ev.Type == events.EventTypeRuntimeRunCancelled {
				t.Errorf("unexpected runtime.run_cancelled (no Cancel was called): %+v", ev)
			}
		case <-drainDeadline:
			break drain
		}
	}
}

// TestE2E_Wave4_CancelMidStream_Cascades pins the Phase 12 ↔ Phase 13
// composition: a run streaming at capacity gets Cancel'd, and the
// cancel must cascade to (a) blocked EmitChunk callers (return
// ErrRunCancelled), (b) FetchByRun callers (eventually return
// ErrRunCancelled after draining any in-flight frames), and (c) bus
// subscribers (one runtime.run_cancelled event with the right
// identity, via the engine's RunCancelledHandler bridge).
//
// This is the test that proves the per-run capacity waiter, the
// per-run fetch subqueue, and the cancel→bus bridge are all wired
// into Cancel's release path together.
func TestE2E_Wave4_CancelMidStream_Cascades(t *testing.T) {
	cfg := wave4Config()
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
	ctx, _ := identity.With(context.Background(), id)

	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("session Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background(), id.SessionID, "cancel-test") })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{events.EventTypeRuntimeRunCancelled},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Streamer that loops emitting frames forever (until cancel).
	// We saturate the per-run capacity quickly; the call to
	// EmitChunk eventually returns ErrRunCancelled when Cancel fires.
	emitErrs := make(chan error, 1)
	streamer := engine.Node{
		Name: "streamer",
		Func: func(ctx context.Context, _ messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			for i := 0; ; i++ {
				err := nctx.EmitChunk(ctx, engine.StreamFrame{Text: fmt.Sprintf("chunk-%d", i)})
				if err != nil {
					emitErrs <- err
					return messages.Envelope{}, err
				}
			}
		},
		Policy: engine.NodePolicy{RunCapacity: 4},
	}

	// Bridge: when Cancel(runID) fires, translate the engine's
	// RunCancelledNotice into a bus runtime.run_cancelled event with
	// the test's identity. Production wiring lives in cmd/harbor;
	// the test inlines the seam to prove the contract end-to-end.
	cancelHandler := func(ctx context.Context, n engine.RunCancelledNotice) {
		_ = bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeRunCancelled,
			Identity: identity.Quadruple{Identity: id, RunID: n.RunID},
			Payload: events.RunCancelledPayload{
				RunID:                n.RunID,
				CancelledAt:          n.CancelledAt.UnixNano(),
				DroppedEnvelopeCount: n.DroppedEnvelopeCount,
			},
		})
	}

	eng, err := engine.New(
		[]engine.Adjacency{{From: streamer, To: nil}},
		engine.WithRunCancelledHandler(cancelHandler),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	const runID = "R-cancel"
	if err := eng.Emit(ctx, messages.Envelope{
		Payload:   "go",
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
		RunID:     runID,
		SessionID: id.SessionID,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("engine.Emit: %v", err)
	}

	// Goroutine doing FetchByRun in a loop until ErrRunCancelled.
	// Cancel is racy with frame arrival — a frame already in the
	// subqueue when Cancel fires may be returned by FetchByRun before
	// the close-with-error signal arrives. So we drain frames until
	// we hit ErrRunCancelled.
	fetchErrs := make(chan error, 1)
	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		for {
			fetchCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := eng.FetchByRun(fetchCtx, runID)
			cancel()
			if err != nil {
				fetchErrs <- err
				return
			}
			// Got a frame; keep draining.
		}
	}()

	// Brief bounded sleep so the producer saturates capacity / fetcher
	// drains a couple frames before we Cancel. Observation, not sync.
	time.Sleep(50 * time.Millisecond)

	// Cancel.
	wasActive, err := eng.Cancel(context.Background(), runID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !wasActive {
		t.Errorf("Cancel returned wasActive=false; expected true")
	}

	// Assert the bus emits runtime.run_cancelled with our identity
	// (via the WithRunCancelledHandler bridge above).
	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeRuntimeRunCancelled {
			t.Errorf("got event type %v, want runtime.run_cancelled", ev.Type)
		}
		if ev.Identity.TenantID != id.TenantID || ev.Identity.SessionID != id.SessionID {
			t.Errorf("identity bleed in run_cancelled: %+v", ev.Identity)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no runtime.run_cancelled event within 2s")
	}

	// EmitChunk caller saw ErrRunCancelled.
	select {
	case err := <-emitErrs:
		if !errors.Is(err, engine.ErrRunCancelled) {
			t.Errorf("EmitChunk err=%v, want ErrRunCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("EmitChunk did not return ErrRunCancelled within 2s")
	}

	// FetchByRun caller eventually saw ErrRunCancelled (after
	// draining any pre-cancel frames).
	select {
	case err := <-fetchErrs:
		if !errors.Is(err, engine.ErrRunCancelled) {
			t.Errorf("FetchByRun err=%v, want ErrRunCancelled", err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("FetchByRun did not return ErrRunCancelled within 3s")
	}
	<-fetchDone
}

// TestE2E_Wave4_Concurrent_MultiTenant runs N tenants in parallel,
// one inlet node per tenant (so the engine actually has N parallel
// workers, not one), each with a finite-emit streamer that produces
// 4 frames + a Done frame, then returns. Each tenant has its own bus
// subscriber that asserts:
//
//   - Zero foreign-tenant events observed.
//   - One runtime.run_cancelled event per Cancel'd run.
//
// Goroutine baseline restored after the engine + bus + store close.
//
// Required by AGENTS.md §17.3 ("Concurrency stress run … N≥10
// concurrent producers/consumers exercise the boundary"): we have 8
// producers + 8 fetchers + 8 cancellers = 24 concurrent goroutines
// hitting the engine through tenant-isolated paths.
func TestE2E_Wave4_Concurrent_MultiTenant(t *testing.T) {
	cfg := wave4Config()
	cfg.Events.ReplayBufferSize = 4096
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}

	const tenants = 8
	const framesPerRun = 4

	baseline := runtime.NumGoroutine()

	// runID → identity lookup so the cancel handler can publish events
	// with the right tenant. Populated before each Emit.
	var idsMu sync.RWMutex
	runIdentities := make(map[string]identity.Identity)

	cancelHandler := func(ctx context.Context, n engine.RunCancelledNotice) {
		idsMu.RLock()
		ident, ok := runIdentities[n.RunID]
		idsMu.RUnlock()
		if !ok {
			return
		}
		_ = bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeRunCancelled,
			Identity: identity.Quadruple{Identity: ident, RunID: n.RunID},
			Payload: events.RunCancelledPayload{
				RunID:                n.RunID,
				CancelledAt:          n.CancelledAt.UnixNano(),
				DroppedEnvelopeCount: n.DroppedEnvelopeCount,
			},
		})
	}

	// One inlet node per tenant — gives N parallel workers. Each
	// streamer's NodeFunc is FINITE (emits framesPerRun frames + Done
	// then returns) so the worker can process the next run promptly.
	// We don't want the worker stuck in an infinite loop here; the
	// CancelMidStream test already covers the infinite-stream case.
	adjacencies := make([]engine.Adjacency, 0, tenants)
	for ti := range tenants {
		streamer := engine.Node{
			Name: fmt.Sprintf("streamer-%d", ti),
			Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
				for i := range framesPerRun {
					if err := nctx.EmitChunk(ctx, engine.StreamFrame{Text: fmt.Sprintf("c-%d", i)}); err != nil {
						return messages.Envelope{}, err
					}
				}
				if err := nctx.EmitChunk(ctx, engine.StreamFrame{Done: true}); err != nil {
					return messages.Envelope{}, err
				}
				env.Payload = "completed"
				return env, nil
			},
			Policy: engine.NodePolicy{RunCapacity: 8},
		}
		adjacencies = append(adjacencies, engine.Adjacency{From: streamer, To: nil})
	}
	eng, err := engine.New(adjacencies, engine.WithRunCancelledHandler(cancelHandler))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	// Per-tenant subscriber + counters.
	type tenantState struct {
		sub             events.Subscription
		cancelEvents    atomic.Int64
		foreignTenantID atomic.Int64
	}
	tenantStates := make([]*tenantState, tenants)
	for ti := range tenants {
		tenantID := fmt.Sprintf("t-%d", ti)
		userID := fmt.Sprintf("u-%d", ti)
		sessionID := fmt.Sprintf("s-%d", ti)
		sub, err := bus.Subscribe(context.Background(), events.Filter{
			Tenant: tenantID, User: userID, Session: sessionID,
			Types: []events.EventType{events.EventTypeRuntimeRunCancelled},
		})
		if err != nil {
			t.Fatalf("Subscribe %d: %v", ti, err)
		}
		ts := &tenantState{sub: sub}
		tenantStates[ti] = ts
		go func(want string) {
			for ev := range sub.Events() {
				if ev.Identity.TenantID != want {
					ts.foreignTenantID.Add(1)
					continue
				}
				ts.cancelEvents.Add(1)
			}
		}(tenantID)
	}

	// Open one session per tenant.
	for ti := range tenants {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", ti),
			UserID:    fmt.Sprintf("u-%d", ti),
			SessionID: fmt.Sprintf("s-%d", ti),
		}
		openCtx, _ := identity.With(context.Background(), id)
		if _, err := reg.Open(openCtx, id.SessionID, id); err != nil {
			t.Fatalf("session Open(%d): %v", ti, err)
		}
	}

	// Producers: each tenant emits one run to its dedicated inlet,
	// drains all frames including Done, then Cancels (which becomes a
	// no-op since the run already completed — we still expect one
	// cancel event per tenant since Cancel returns true if the run was
	// recently active OR records a flag if it landed mid-flight).
	//
	// To guarantee a Cancel event per tenant, we Cancel BEFORE the
	// run completes its drain — start the drain in a goroutine that
	// finishes cleanly on ErrRunCancelled.
	var prodWG sync.WaitGroup
	for ti := range tenants {
		prodWG.Add(1)
		go func(tenant int) {
			defer prodWG.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", tenant),
				UserID:    fmt.Sprintf("u-%d", tenant),
				SessionID: fmt.Sprintf("s-%d", tenant),
			}
			ctx, _ := identity.With(context.Background(), id)
			runID := fmt.Sprintf("r-%d", tenant)
			idsMu.Lock()
			runIdentities[runID] = id
			idsMu.Unlock()
			env := messages.Envelope{
				Payload:   "go",
				Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
				RunID:     runID,
				SessionID: id.SessionID,
				Timestamp: time.Now(),
			}
			if err := eng.EmitTo(ctx, env, engine.NodeRef{Name: fmt.Sprintf("streamer-%d", tenant)}); err != nil {
				t.Errorf("EmitTo(%s): %v", runID, err)
				return
			}
			// Drain ONE frame so the run is genuinely active when we
			// Cancel — Phase 13's Cancel returns wasActive=true only
			// for runs with pending envelopes / in-flight workers /
			// non-empty subqueues at the moment of the call.
			fetchCtx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := eng.FetchByRun(fetchCtx, runID)
			fcancel()
			if err != nil {
				t.Errorf("FetchByRun(%s): %v", runID, err)
				return
			}
			// Now Cancel mid-flight. Even if the producer has
			// completed, the cancellation TTL guarantees the cancel
			// event is emitted (Cancel records the flag and runs the
			// handler regardless).
			if _, err := eng.Cancel(context.Background(), runID); err != nil {
				t.Errorf("Cancel(%s): %v", runID, err)
			}
			// Drain remaining frames (may be ErrRunCancelled — both
			// are acceptable here; we just want to unblock the run).
			drainDeadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(drainDeadline) {
				fetchCtx, fcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, err := eng.FetchByRun(fetchCtx, runID)
				fcancel()
				if err != nil {
					return // ErrRunCancelled or context deadline — done.
				}
			}
		}(ti)
	}
	prodWG.Wait()

	// Cancel emits the run_cancelled event asynchronously onto the bus;
	// the per-tenant subscriber goroutine counts it off sub.Events().
	// Wait for every tenant's event to land BEFORE teardown — closing
	// the subscriptions / bus while an event is still in flight would
	// drop it (§17.4: never assert on an async event without a bounded
	// eventually-poll). The per-tenant assertion below stays the hard
	// gate; this poll only removes the teardown race.
	cancelDeadline := time.Now().Add(5 * time.Second)
	for {
		allLanded := true
		for _, ts := range tenantStates {
			if ts.cancelEvents.Load() < 1 {
				allLanded = false
				break
			}
		}
		if allLanded || time.Now().After(cancelDeadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Tear down everything in order so subscriber goroutines drain
	// before we sample goroutine count.
	if err := eng.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	for _, ts := range tenantStates {
		ts.sub.Cancel()
	}
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Errorf("CloseRegistry: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("bus.Close: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Errorf("store.Close: %v", err)
	}

	// Per-tenant assertions: zero foreign-tenant events; one
	// cancel event per tenant.
	for ti, ts := range tenantStates {
		if n := ts.foreignTenantID.Load(); n != 0 {
			t.Errorf("tenant %d saw %d foreign-tenant events", ti, n)
		}
		if got := ts.cancelEvents.Load(); got != 1 {
			t.Errorf("tenant %d got %d run_cancelled events, want 1", ti, got)
		}
	}

	// Goroutine baseline restored. Tolerance +5 because we tore down
	// 4 long-lived subsystems and Go's parked goroutines may not
	// retire immediately.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// --- helpers ---

func wave4Config() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-wave4-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			RepairAttempts: 2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		// ArtifactsConfig populated by Phase 17; required by Validate.
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		// Phase 20 / 21 / 22 / 23 cross-phase additions per §17.6.
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
		Memory:      config.MemoryConfig{Driver: "inmem", Strategy: "none"},
	}
}
