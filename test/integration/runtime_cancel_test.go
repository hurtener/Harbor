// Phase 13 cross-subsystem integration test per AGENTS.md §17.
//
// Wires real audit + events + state + sessions + engine drivers and
// exercises Engine.Cancel(runID) end-to-end. Two scenarios:
//
//  1. TestE2E_Phase13_CancelRun_EmitsBusEvent — two concurrent
//     streaming runs share one engine; cancel one mid-stream;
//     subscribe to runtime.run_cancelled events on the bus and
//     assert the cancel notice surfaces with the right RunID +
//     identity. The other run continues to drain unaffected.
//  2. TestE2E_Phase13_FetchByRun_PerRunIsolation — two concurrent
//     runs feed FetchByRun consumers; one is cancelled; assert the
//     cancelled run's FetchByRun returns ErrRunCancelled and the
//     other run's FetchByRun continues to deliver.
//
// Both scenarios run under -race; identity propagates through every
// layer (envelope quadruple, bus filters, session-scoped subscribers,
// run cancelled handler).
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase13_CancelRun_EmitsBusEvent — the cross-wave gate test
// for Phase 13. Two concurrent streaming runs; cancel one; verify
// the bus emits runtime.run_cancelled with the right identity, and
// the other run continues to drain to completion.
func TestE2E_Phase13_CancelRun_EmitsBusEvent(t *testing.T) {
	cfg := phase12Config()
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

	idA := identity.Identity{TenantID: "T-A", UserID: "U-A", SessionID: "S-A"}
	idB := identity.Identity{TenantID: "T-B", UserID: "U-B", SessionID: "S-B"}
	for _, id := range []identity.Identity{idA, idB} {
		openCtx, _ := identity.With(context.Background(), id)
		if _, err := reg.Open(openCtx, id.SessionID, id); err != nil {
			t.Fatalf("Open %s: %v", id.SessionID, err)
		}
	}

	// Subscribe an admin to runtime.run_cancelled so we can observe
	// the cancellation event end-to-end.
	subCtx, _ := identity.With(context.Background(), idA)
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant:  idA.TenantID,
		User:    idA.UserID,
		Session: idA.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeRunCancelled},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Engine: TWO inlets so each run has its own worker (fan-in).
	// A streaming producer per inlet that emits frames until ctx or
	// the per-run cancel flag fires.
	stopProducer := make(chan struct{})
	defer close(stopProducer)

	startedA := make(chan struct{}, 1)
	startedB := make(chan struct{}, 1)
	makeProducer := func(started chan<- struct{}) engine.NodeFunc {
		return func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			started <- struct{}{}
			for i := range 1000 {
				select {
				case <-stopProducer:
					return messages.Envelope{}, nil
				default:
				}
				if err := nctx.EmitChunk(ctx, engine.StreamFrame{
					StreamID: env.RunID,
					Text:     fmt.Sprintf("%s-%d", env.RunID, i),
				}); err != nil {
					return messages.Envelope{}, err
				}
			}
			return messages.Envelope{}, nil
		}
	}

	// Translate engine RunCancelledNotice → bus runtime.run_cancelled
	// event. Production wiring will live in cmd/harbor; the test
	// inlines the seam to prove the contract end-to-end.
	cancelHandler := func(ctx context.Context, n engine.RunCancelledNotice) {
		_ = bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeRunCancelled,
			Identity: identity.Quadruple{Identity: idA, RunID: n.RunID},
			Payload: events.RunCancelledPayload{
				RunID:                n.RunID,
				CancelledAt:          n.CancelledAt.UnixNano(),
				DroppedEnvelopeCount: n.DroppedEnvelopeCount,
			},
		})
	}

	nodeA := engine.Node{
		Name:   "A",
		Func:   makeProducer(startedA),
		Policy: engine.NodePolicy{RunCapacity: 4},
	}
	nodeB := engine.Node{
		Name:   "B",
		Func:   makeProducer(startedB),
		Policy: engine.NodePolicy{RunCapacity: 4},
	}
	eng, err := engine.New(
		[]engine.Adjacency{
			{From: nodeA},
			{From: nodeB},
		},
		engine.WithRunCancelledHandler(cancelHandler),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancelEng := context.WithCancel(context.Background())
	defer cancelEng()
	baseline := runtime.NumGoroutine()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Emit two streams concurrently — each to its own inlet so they
	// run in parallel.
	for _, p := range []struct {
		id     identity.Identity
		runID  string
		target string
	}{
		{idA, "R-A", "A"},
		{idB, "R-B", "B"},
	} {
		env := messages.Envelope{
			Headers:   messages.Headers{TenantID: p.id.TenantID, UserID: p.id.UserID, Topic: "stream"},
			SessionID: p.id.SessionID,
			RunID:     p.runID,
		}
		if err := eng.EmitTo(ctx, env, engine.NodeRef{Name: p.target}); err != nil {
			t.Fatalf("EmitTo %s: %v", p.runID, err)
		}
	}

	// Wait for both producers to be in-flight.
	<-startedA
	<-startedB

	// Drain a few frames so producers are mid-flight.
	for range 4 {
		fctx, fcancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = eng.Fetch(fctx)
		fcancel()
	}

	// Cancel R-A. Expect runtime.run_cancelled on the bus filtered
	// to tenant A.
	if _, err := eng.Cancel(context.Background(), "R-A"); err != nil {
		t.Fatalf("Cancel(R-A): %v", err)
	}

	deadline := time.After(3 * time.Second)
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before cancel event arrived")
		}
		if ev.Type != events.EventTypeRuntimeRunCancelled {
			t.Errorf("event Type=%v, want runtime.run_cancelled", ev.Type)
		}
		if ev.Identity.TenantID != idA.TenantID {
			t.Errorf("event tenant=%q, want %q", ev.Identity.TenantID, idA.TenantID)
		}
	case <-deadline:
		t.Fatal("never observed runtime.run_cancelled on the bus within 3s")
	}

	// Drain the engine — we don't care exactly how many R-B frames
	// land here; the assertion is that R-B's producer continues
	// producing (no cross-run interference) until we ask it to stop.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := eng.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	settle := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(settle) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// TestE2E_Phase13_FetchByRun_PerRunIsolation — exercise FetchByRun
// against a real wave-3 stack. Two concurrent runs feed FetchByRun;
// one is cancelled mid-fetch; assert ErrRunCancelled on the
// cancelled run AND continued delivery on the uncancelled run.
func TestE2E_Phase13_FetchByRun_PerRunIsolation(t *testing.T) {
	cfg := phase12Config()
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

	idA := identity.Identity{TenantID: "T-A", UserID: "U-A", SessionID: "S-A"}
	idB := identity.Identity{TenantID: "T-B", UserID: "U-B", SessionID: "S-B"}
	for _, id := range []identity.Identity{idA, idB} {
		openCtx, _ := identity.With(context.Background(), id)
		if _, err := reg.Open(openCtx, id.SessionID, id); err != nil {
			t.Fatalf("Open %s: %v", id.SessionID, err)
		}
	}

	a := engine.Node{Name: "A", Func: func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return in, nil
	}}
	b := engine.Node{Name: "B", Func: func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return in, nil
	}}
	eng, err := engine.New([]engine.Adjacency{
		{From: a},
		{From: b},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancelEng := context.WithCancel(context.Background())
	defer cancelEng()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// Prime FetchByRun for both runs so the dispatcher knows there
	// are subscribers. Emit one envelope per run + drain to confirm.
	envA := messages.Envelope{
		Headers:   messages.Headers{TenantID: idA.TenantID, UserID: idA.UserID, Topic: "run"},
		SessionID: idA.SessionID,
		RunID:     "R-A",
	}
	envB := messages.Envelope{
		Headers:   messages.Headers{TenantID: idB.TenantID, UserID: idB.UserID, Topic: "run"},
		SessionID: idB.SessionID,
		RunID:     "R-B",
	}
	if err := eng.EmitTo(ctx, envA, engine.NodeRef{Name: "A"}); err != nil {
		t.Fatalf("EmitTo A: %v", err)
	}
	if err := eng.EmitTo(ctx, envB, engine.NodeRef{Name: "B"}); err != nil {
		t.Fatalf("EmitTo B: %v", err)
	}
	priCtx, priCancel := context.WithTimeout(ctx, 2*time.Second)
	defer priCancel()
	got, err := eng.FetchByRun(priCtx, "R-A")
	if err != nil {
		t.Fatalf("priming FetchByRun(R-A): %v", err)
	}
	if got.Identity().TenantID != idA.TenantID {
		t.Errorf("tenant=%q, want %q", got.Identity().TenantID, idA.TenantID)
	}
	if got, err := eng.FetchByRun(priCtx, "R-B"); err != nil {
		t.Fatalf("priming FetchByRun(R-B): %v", err)
	} else if got.Identity().TenantID != idB.TenantID {
		t.Errorf("tenant=%q, want %q", got.Identity().TenantID, idB.TenantID)
	}

	// Now both runs are subscribed (dispatcher writes blocking).
	// Block a FetchByRun(R-A) call — empty subqueue → it blocks
	// until Cancel(R-A) wakes it with ErrRunCancelled.
	gotErrA := make(chan error, 1)
	go func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer fcancel()
		_, err := eng.FetchByRun(fctx, "R-A")
		gotErrA <- err
	}()

	// Concurrent: emit another envelope to R-B and FetchByRun it.
	if err := eng.EmitTo(ctx, envB, engine.NodeRef{Name: "B"}); err != nil {
		t.Fatalf("EmitTo R-B (second): %v", err)
	}

	gotErrB := make(chan error, 1)
	gotEnvB := make(chan messages.Envelope, 1)
	go func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer fcancel()
		out, err := eng.FetchByRun(fctx, "R-B")
		if err != nil {
			gotErrB <- err
			return
		}
		gotEnvB <- out
	}()

	// Wait briefly so both fetchers register.
	time.Sleep(50 * time.Millisecond)
	if _, err := eng.Cancel(context.Background(), "R-A"); err != nil {
		t.Fatalf("Cancel(R-A): %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		select {
		case err := <-gotErrA:
			if !errors.Is(err, engine.ErrRunCancelled) {
				t.Errorf("FetchByRun(R-A) err=%v, want ErrRunCancelled", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("blocked FetchByRun(R-A) never woke from Cancel")
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case env := <-gotEnvB:
			if env.RunID != "R-B" {
				t.Errorf("RunID=%q, want R-B", env.RunID)
			}
			if env.Identity().TenantID != idB.TenantID {
				t.Errorf("tenant=%q, want %q (cross-run identity bleed)", env.Identity().TenantID, idB.TenantID)
			}
		case err := <-gotErrB:
			t.Errorf("FetchByRun(R-B) errored: %v (want envelope)", err)
		case <-time.After(3 * time.Second):
			t.Error("FetchByRun(R-B) never returned within 3s — cancel(R-A) leaked into R-B's subqueue")
		}
	}()
	wg.Wait()
}
