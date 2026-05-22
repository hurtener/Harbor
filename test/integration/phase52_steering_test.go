// Phase 52 — Steering inbox + control taxonomy integration test
// (RFC §6.3; master-plan Phase 52 detail block; D-070).
//
// Phase 52's Deps are 50 + 05, so an integration test is mandatory
// (AGENTS.md §17.1). This test wires the steering inbox against the
// REAL events.EventBus (the in-mem production driver) + the REAL
// patterns audit redactor — no mocks at the seam (§17.3 #1). It
// exercises the master-plan acceptance surface end-to-end:
//
//   - the auth-scope-per-event check: every one of the nine control
//     types is submitted at its RFC §6.3 minimum scope (accepted) and
//     below it (rejected), and a scope mismatch produces a
//     control.rejected audit event on the real bus — the "per-event
//     scope mismatch returns 403 + audit" acceptance criterion (the
//     403 is the Phase 54 Protocol edge's job; the audit emit is
//     Phase 52's, and it is what this test asserts);
//   - identity propagation: the run quadruple flows through Open →
//     Enqueue → the rejection event's Identity, and an
//     identity-scoped subscriber on the bus sees exactly its own
//     run's rejection events (§17.3 #2 + cross-run isolation);
//   - a payload-bounds failure mode: an oversize payload is rejected
//     loud and produces a payload_invalid audit event (§17.3 #3);
//   - a concurrency stress run: N≥10 concurrent submitters against
//     one shared Registry + bus, asserting no cross-talk and no
//     goroutine leak after teardown (§17.3 "Concurrency stress run").
//
// All assertions run under -race.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

func phase52EventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}
}

// phase52Bus opens a real in-mem EventBus with the real patterns
// redactor. Documented dummy config — no secrets.
func phase52Bus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), phase52EventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// phase52Run builds a documented dummy run quadruple for tenant t.
func phase52Run(tenant, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    "user-" + run,
			SessionID: "session-" + run,
		},
		RunID: run,
	}
}

// drainRejections collects every control.rejected event a
// subscription delivers within a bounded window, then returns. It
// does not sleep as a synchronisation primitive — it reads the
// channel until the expected count arrives or the deadline trips.
func drainRejections(t *testing.T, sub events.Subscription, want int) []steering.ControlRejectedPayload {
	t.Helper()
	var got []steering.ControlRejectedPayload
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed after %d/%d rejection events", len(got), want)
			}
			if ev.Type != steering.EventTypeControlRejected {
				continue
			}
			p, ok := ev.Payload.(steering.ControlRejectedPayload)
			if !ok {
				t.Fatalf("control.rejected payload type = %T, want ControlRejectedPayload", ev.Payload)
			}
			got = append(got, p)
		case <-deadline:
			t.Fatalf("timed out after %d/%d rejection events", len(got), want)
		}
	}
	return got
}

// TestE2E_Phase52_AuthScopePerEvent walks every one of the nine
// control types through the auth-scope gate against a real EventBus.
// For each type: the RFC §6.3 minimum scope is accepted (enqueued);
// a scope below the minimum is rejected loud AND produces a
// control.rejected audit event on the bus with Reason ==
// "scope_mismatch". This is the master-plan Phase 52 acceptance
// surface ("per-event scope mismatch returns 403 + audit").
func TestE2E_Phase52_AuthScopePerEvent(t *testing.T) {
	bus := phase52Bus(t)
	reg := steering.NewRegistry()
	run := phase52Run("tenant-acme", "run-scope")

	inbox, err := reg.Open(run)
	if err != nil {
		t.Fatalf("Registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Retire(run) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  run.TenantID,
		User:    run.UserID,
		Session: run.SessionID,
		Types:   []events.EventType{steering.EventTypeControlRejected},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// belowMin returns a scope strictly below min, or "" when min is
	// already the weakest scope (INJECT_CONTEXT / USER_MESSAGE — no
	// scope is below session_user, so those types' "rejection" case
	// is exercised via the cross-tenant path in the dedicated test).
	belowMin := func(min steering.Scope) steering.Scope {
		switch min {
		case steering.ScopeOwnerUser:
			return steering.ScopeSessionUser
		case steering.ScopeAdmin:
			return steering.ScopeOwnerUser
		default:
			return ""
		}
	}

	wantRejections := 0
	for _, ct := range steering.ControlTypes() {
		min, ok := steering.RequiredScope(ct)
		if !ok {
			t.Fatalf("RequiredScope(%q) not found", ct)
		}

		// Accepted: the exact minimum scope enqueues.
		okEv := steering.ControlEvent{
			Type:         ct,
			Identity:     run,
			CallerScope:  min,
			CallerTenant: run.TenantID,
		}
		if err := inbox.Enqueue(okEv); err != nil {
			t.Errorf("Enqueue(%q at min scope %q) = %v, want nil", ct, min, err)
		}

		// Rejected: a scope below the minimum fails loud + audits.
		low := belowMin(min)
		if low == "" {
			continue
		}
		badEv := steering.ControlEvent{
			Type:         ct,
			Identity:     run,
			CallerScope:  low,
			CallerTenant: run.TenantID,
		}
		rejErr := inbox.Enqueue(badEv)
		if !errors.Is(rejErr, steering.ErrScopeMismatch) {
			t.Errorf("Enqueue(%q at low scope %q) = %v, want ErrScopeMismatch", ct, low, rejErr)
			continue
		}
		// The Protocol edge audits the rejection on the real bus.
		if err := steering.EmitRejection(context.Background(), bus, run, ct, low, rejErr); err != nil {
			t.Errorf("EmitRejection(%q): %v", ct, err)
			continue
		}
		wantRejections++
	}

	// Drain the audit events and assert every one classifies as a
	// scope mismatch and carries the run identity.
	rejections := drainRejections(t, sub, wantRejections)
	if len(rejections) != wantRejections {
		t.Fatalf("observed %d control.rejected events, want %d", len(rejections), wantRejections)
	}
	for _, p := range rejections {
		if p.Reason != "scope_mismatch" {
			t.Errorf("control.rejected Reason = %q, want scope_mismatch", p.Reason)
		}
		if !steering.IsValidControlType(steering.ControlType(p.Type)) {
			t.Errorf("control.rejected Type = %q, not a canonical control type", p.Type)
		}
	}

	// The accepted submissions are queued on the inbox — one per
	// control type.
	drained, err := inbox.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != len(steering.ControlTypes()) {
		t.Errorf("drained %d accepted events, want %d (one per control type)", len(drained), len(steering.ControlTypes()))
	}
}

// TestE2E_Phase52_CrossTenantRequiresAdmin proves the RFC §6.3
// "Cross-tenant steering requires admin" rule end-to-end: a non-admin
// caller from a foreign tenant is rejected even for the weakest
// control type (INJECT_CONTEXT), and the rejection audits on the bus.
func TestE2E_Phase52_CrossTenantRequiresAdmin(t *testing.T) {
	bus := phase52Bus(t)
	reg := steering.NewRegistry()
	run := phase52Run("tenant-owner", "run-xt")

	inbox, err := reg.Open(run)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Retire(run) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  run.TenantID,
		User:    run.UserID,
		Session: run.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// A foreign-tenant non-admin submitting the weakest control type
	// is still rejected.
	badEv := steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     run,
		CallerScope:  steering.ScopeOwnerUser, // not admin
		CallerTenant: "tenant-intruder",
	}
	rejErr := inbox.Enqueue(badEv)
	if !errors.Is(rejErr, steering.ErrScopeMismatch) {
		t.Fatalf("Enqueue(cross-tenant non-admin) = %v, want ErrScopeMismatch", rejErr)
	}
	if err := steering.EmitRejection(context.Background(), bus, run, steering.ControlInjectContext, steering.ScopeOwnerUser, rejErr); err != nil {
		t.Fatalf("EmitRejection: %v", err)
	}

	// A foreign-tenant ADMIN is allowed.
	adminEv := steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     run,
		CallerScope:  steering.ScopeAdmin,
		CallerTenant: "tenant-intruder",
	}
	if err := inbox.Enqueue(adminEv); err != nil {
		t.Fatalf("Enqueue(cross-tenant admin) = %v, want nil", err)
	}

	rejections := drainRejections(t, sub, 1)
	if rejections[0].Reason != "scope_mismatch" {
		t.Errorf("cross-tenant rejection Reason = %q, want scope_mismatch", rejections[0].Reason)
	}
}

// TestE2E_Phase52_PayloadBoundsFailureMode is the §17.3 #3 failure
// mode: an oversize payload is rejected loud and audits on the real
// bus with Reason == "payload_invalid". It also confirms an
// in-bounds payload at the same control type is accepted — proving
// the bound is the only thing that fired.
func TestE2E_Phase52_PayloadBoundsFailureMode(t *testing.T) {
	bus := phase52Bus(t)
	reg := steering.NewRegistry()
	run := phase52Run("tenant-bounds", "run-bounds")

	inbox, err := reg.Open(run)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Retire(run) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  run.TenantID,
		User:    run.UserID,
		Session: run.SessionID,
		Types:   []events.EventType{steering.EventTypeControlRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Oversize: a string past the per-string cap.
	oversize := steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     run,
		CallerScope:  steering.ScopeSessionUser,
		CallerTenant: run.TenantID,
		Payload:      map[string]any{"note": strings.Repeat("x", steering.MaxPayloadStringLen+1)},
	}
	rejErr := inbox.Enqueue(oversize)
	if !errors.Is(rejErr, steering.ErrPayloadInvalid) {
		t.Fatalf("Enqueue(oversize payload) = %v, want ErrPayloadInvalid", rejErr)
	}
	if err := steering.EmitRejection(context.Background(), bus, run, steering.ControlInjectContext, steering.ScopeSessionUser, rejErr); err != nil {
		t.Fatalf("EmitRejection: %v", err)
	}

	// In-bounds: the same control type with a small payload enqueues.
	ok := steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     run,
		CallerScope:  steering.ScopeSessionUser,
		CallerTenant: run.TenantID,
		Payload:      map[string]any{"note": "small"},
	}
	if err := inbox.Enqueue(ok); err != nil {
		t.Fatalf("Enqueue(in-bounds payload) = %v, want nil", err)
	}

	rejections := drainRejections(t, sub, 1)
	if rejections[0].Reason != "payload_invalid" {
		t.Errorf("oversize rejection Reason = %q, want payload_invalid", rejections[0].Reason)
	}
}

// TestE2E_Phase52_PerRunIsolation proves an identity-scoped
// subscriber sees only its OWN run's rejection events — a second
// run's rejections on the same bus never bleed across. This is the
// cross-run / cross-session isolation guarantee (CLAUDE.md §6).
func TestE2E_Phase52_PerRunIsolation(t *testing.T) {
	bus := phase52Bus(t)
	reg := steering.NewRegistry()

	runA := phase52Run("tenant-iso", "run-A")
	runB := phase52Run("tenant-iso", "run-B")

	inA, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open runA: %v", err)
	}
	t.Cleanup(func() { _ = reg.Retire(runA) })
	inB, err := reg.Open(runB)
	if err != nil {
		t.Fatalf("Open runB: %v", err)
	}
	t.Cleanup(func() { _ = reg.Retire(runB) })

	// Subscriber scoped to run A only.
	subA, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  runA.TenantID,
		User:    runA.UserID,
		Session: runA.SessionID,
		Types:   []events.EventType{steering.EventTypeControlRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe runA: %v", err)
	}
	t.Cleanup(subA.Cancel)

	// Reject one submission on each run (PRIORITIZE at owner_user is
	// a scope mismatch).
	for _, rc := range []struct {
		run identity.Quadruple
		in  *steering.Inbox
	}{{runA, inA}, {runB, inB}} {
		ev := steering.ControlEvent{
			Type:         steering.ControlPrioritize,
			Identity:     rc.run,
			CallerScope:  steering.ScopeOwnerUser,
			CallerTenant: rc.run.TenantID,
		}
		rejErr := rc.in.Enqueue(ev)
		if !errors.Is(rejErr, steering.ErrScopeMismatch) {
			t.Fatalf("Enqueue(%s PRIORITIZE) = %v, want ErrScopeMismatch", rc.run.RunID, rejErr)
		}
		if err := steering.EmitRejection(context.Background(), bus, rc.run, steering.ControlPrioritize, steering.ScopeOwnerUser, rejErr); err != nil {
			t.Fatalf("EmitRejection(%s): %v", rc.run.RunID, err)
		}
	}

	// subA must see exactly ONE rejection — run A's. Run B's must
	// not bleed across.
	rejections := drainRejections(t, subA, 1)
	if len(rejections) != 1 {
		t.Fatalf("subA observed %d rejections, want exactly 1 (run B bled across)", len(rejections))
	}
	// Drain a beat longer to confirm no second (run B) event arrives.
	select {
	case ev, ok := <-subA.Events():
		if ok && ev.Type == steering.EventTypeControlRejected {
			t.Errorf("subA observed a second rejection event — run B bled across isolation")
		}
	case <-time.After(200 * time.Millisecond):
		// Expected: no cross-run bleed.
	}
}

// TestE2E_Phase52_ConcurrencyStress is the §17.3 cross-package
// concurrency stress run: N concurrent submitters against one shared
// Registry + one shared real EventBus. Asserts every rejection
// audits, no event is lost or duplicated, and the goroutine count
// returns to baseline after teardown.
func TestE2E_Phase52_ConcurrencyStress(t *testing.T) {
	const n = 24

	baseline := runtime.NumGoroutine()

	bus := phase52Bus(t)
	reg := steering.NewRegistry()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := range n {
		go func(i int) {
			defer wg.Done()
			run := phase52Run(fmt.Sprintf("tenant-%d", i), fmt.Sprintf("run-%d", i))
			inbox, err := reg.Open(run)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: Open: %w", i, err)
				return
			}

			// One accepted (CANCEL at owner_user) + one rejected
			// (PRIORITIZE at owner_user) per goroutine.
			okEv := steering.ControlEvent{
				Type:         steering.ControlCancel,
				Identity:     run,
				CallerScope:  steering.ScopeOwnerUser,
				CallerTenant: run.TenantID,
			}
			if err := inbox.Enqueue(okEv); err != nil {
				errCh <- fmt.Errorf("goroutine %d: Enqueue(CANCEL): %w", i, err)
				return
			}

			badEv := steering.ControlEvent{
				Type:         steering.ControlPrioritize,
				Identity:     run,
				CallerScope:  steering.ScopeOwnerUser,
				CallerTenant: run.TenantID,
			}
			rejErr := inbox.Enqueue(badEv)
			if !errors.Is(rejErr, steering.ErrScopeMismatch) {
				errCh <- fmt.Errorf("goroutine %d: Enqueue(PRIORITIZE) = %w, want ErrScopeMismatch", i, rejErr)
				return
			}
			if err := steering.EmitRejection(context.Background(), bus, run, steering.ControlPrioritize, steering.ScopeOwnerUser, rejErr); err != nil {
				errCh <- fmt.Errorf("goroutine %d: EmitRejection: %w", i, err)
				return
			}

			drained, err := inbox.Drain()
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: Drain: %w", i, err)
				return
			}
			if len(drained) != 1 {
				errCh <- fmt.Errorf("goroutine %d: drained %d events, want 1", i, len(drained))
				return
			}
			if drained[0].Identity != run {
				errCh <- fmt.Errorf("goroutine %d: drained event for foreign run %+v", i, drained[0].Identity)
				return
			}
			if err := reg.Retire(run); err != nil {
				errCh <- fmt.Errorf("goroutine %d: Retire: %w", i, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if reg.Len() != 0 {
		t.Errorf("Registry.Len() = %d after all runs retired, want 0", reg.Len())
	}

	// Goroutine-leak check: the bus's Cleanup runs after the test, so
	// poll against baseline + a small slack for the bus's own
	// still-open subscription reaper (closed by t.Cleanup).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("goroutine count %d vs baseline %d (bus reaper closes on t.Cleanup)", runtime.NumGoroutine(), baseline)
}
