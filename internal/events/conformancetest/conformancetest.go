// Package conformancetest exposes the canonical correctness suite every
// `events.EventBus` driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/events` does not import the standard library `testing` package
// (precedent: `internal/identity/conformancetest`, `internal/state/conformancetest`,
// `internal/memory/conformancetest`).
//
// Downstream drivers (in-memory, durable-log) consume it via:
//
//	import "github.com/hurtener/Harbor/internal/events/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() conformancetest.Harness {
//	        return conformancetest.Harness{
//	            NewBus:                 func(t *testing.T) events.EventBus { ... },
//	            NewReplayDisabledBus:   func(t *testing.T) events.EventBus { ... },
//	            NewBoundedRetentionBus: func(t *testing.T, capacity int) events.EventBus { ... },
//	        }
//	    })
//	}
//
// The factory is called once per top-level subtest; each constructor must
// register its own cleanup via t.Cleanup. All three constructors are
// mandatory — the suite's scenarios span three distinct bus configurations
// (default replay-capable, replay disabled, bounded retention) rather than
// one default fixture, and there is no capability-gated skip path: a
// driver that cannot express one of the three configurations is not
// eligible to consume this suite.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Harness bundles the driver-specific bus constructors the suite needs.
//
// All three are MANDATORY — no optional-capability ceremony: every driver
// consuming this suite must express all three configurations. Constructors
// register their own cleanup via t.Cleanup, mirroring each driver's
// existing test-helper shape.
type Harness struct {
	// NewBus returns a fresh bus in the driver's default replay-capable
	// configuration. The suite type-asserts events.Replayer,
	// events.HistoryReplayer and events.Fencer on it and FAILS loudly
	// (never skips) when an assertion misses.
	NewBus func(t *testing.T) events.EventBus

	// NewReplayDisabledBus returns a fresh bus configured with replay OFF
	// (ReplayBufferSize 0), so Replay / Window fail with
	// ErrReplayUnavailable.
	NewReplayDisabledBus func(t *testing.T) events.EventBus

	// NewBoundedRetentionBus returns a fresh bus whose replay retention is
	// exactly capacity events, so publishing past capacity evicts the
	// oldest sequences and a stale cursor fails with ErrCursorTooOld. The
	// one genuine cross-driver divergence (unbounded persisted retention
	// vs. a best-effort ring) is expressed here as a configuration choice,
	// never as a capability flag.
	NewBoundedRetentionBus func(t *testing.T, capacity int) events.EventBus
}

// Factory builds a fresh Harness. Called once per top-level subtest.
type Factory func() Harness

// Run executes the canonical EventBus driver correctness suite.
//
// Subtests:
//
//   - Capabilities_ReplayerHistoryReplayerFencer_Present
//   - Publish_IdentityMandatory_Rejected
//   - Subscribe_EmptyTripleNonAdmin_Rejected
//   - Replay_EmptyTripleNonAdmin_Rejected
//   - Bounds_EmptyTriple_ErrIdentityScopeRequired
//   - Subscribe_CrossTenant_Isolation
//   - Replay_CrossIdentity_Isolation
//   - Replay_HeadCursor_ReturnsNil
//   - Replay_Disabled_ErrReplayUnavailable
//   - Replay_RetentionOverrun_ErrCursorTooOld
//   - Bounds_ReportsHeadAndTail
//   - Bounds_EmptySession_ErrNoHistory
//   - Window_TailFirst_MostRecentK_OldestFirst
//   - Window_BeforeCursor_ScrollsUp
//   - Window_EmptySession_Empty
//   - Window_ReachesHead
//   - Window_ReplayDisabled_ErrReplayUnavailable
//   - Fence_DropsLateEventsAndEmptiesHistory
//   - Fence_AfterClose_ErrBusClosed
//   - AfterClose_OpsRejected_ErrBusClosed
//   - Close_Idempotent
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Capabilities_ReplayerHistoryReplayerFencer_Present", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		_ = mustReplayer(t, bus)
		_ = mustHistoryReplayer(t, bus)
		_ = mustFencer(t, bus)
	})

	t.Run("Publish_IdentityMandatory_Rejected", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		id := mkID(1)
		ev := mkEvent(id, 1)
		ev.Identity.TenantID = ""
		err := bus.Publish(context.Background(), ev)
		if !errors.Is(err, events.ErrIdentityRequired) {
			t.Fatalf("Publish(missing tenant): err=%v, want ErrIdentityRequired", err)
		}
	})

	t.Run("Subscribe_EmptyTripleNonAdmin_Rejected", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		for _, f := range emptyTripleCases() {
			_, err := bus.Subscribe(context.Background(), f)
			if !errors.Is(err, events.ErrIdentityScopeRequired) {
				t.Errorf("Subscribe(filter=%+v): err=%v, want ErrIdentityScopeRequired", f, err)
			}
		}
	})

	t.Run("Replay_EmptyTripleNonAdmin_Rejected", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		rp := mustReplayer(t, bus)
		for _, f := range emptyTripleCases() {
			_, err := rp.Replay(context.Background(), events.Cursor{}, f)
			if !errors.Is(err, events.ErrIdentityScopeRequired) {
				t.Errorf("Replay(filter=%+v): err=%v, want ErrIdentityScopeRequired", f, err)
			}
		}
	})

	t.Run("Bounds_EmptyTriple_ErrIdentityScopeRequired", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		_, _, _, err := hr.Bounds(context.Background(), events.Filter{Tenant: "t-partial"})
		if !errors.Is(err, events.ErrIdentityScopeRequired) {
			t.Fatalf("Bounds(partial triple): err=%v, want ErrIdentityScopeRequired", err)
		}
	})

	t.Run("Subscribe_CrossTenant_Isolation", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		idA := mkID(1)
		idB := mkID(2)

		subA, err := bus.Subscribe(context.Background(), filterFor(idA))
		if err != nil {
			t.Fatalf("Subscribe A: %v", err)
		}
		defer subA.Cancel()

		for i := range 20 {
			if err := bus.Publish(context.Background(), mkEvent(idB, i)); err != nil {
				t.Fatalf("Publish B #%d: %v", i, err)
			}
		}

		select {
		case ev, ok := <-subA.Events():
			if ok {
				t.Fatalf("subscriber A leaked a cross-tenant event: %+v", ev)
			}
		case <-time.After(150 * time.Millisecond):
			// Expected: no cross-tenant delivery within the window.
		}
	})

	t.Run("Replay_CrossIdentity_Isolation", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		rp := mustReplayer(t, bus)

		// Axis 1: cross-tenant (distinct tenant, user AND session).
		idA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-A", SessionID: "sess-A"}}
		idB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-B", UserID: "user-B", SessionID: "sess-B"}}
		publishN(t, bus, idA, 5)
		publishN(t, bus, idB, 5)

		outA, err := rp.Replay(context.Background(), events.Cursor{SessionID: idA.SessionID}, filterFor(idA))
		if err != nil {
			t.Fatalf("Replay tenant A: %v", err)
		}
		if len(outA) != 5 {
			t.Fatalf("Replay tenant A returned %d events, want 5", len(outA))
		}
		for _, ev := range outA {
			if ev.Identity.TenantID != idA.TenantID {
				t.Fatalf("cross-tenant leak: replay for tenant %q returned event from tenant %q", idA.TenantID, ev.Identity.TenantID)
			}
		}

		// Axis 2: cross-session, SAME tenant and user.
		idC := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-C", UserID: "user-C", SessionID: "sess-C"}}
		idD := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-C", UserID: "user-C", SessionID: "sess-D"}}
		publishN(t, bus, idC, 4)
		publishN(t, bus, idD, 4)

		outC, err := rp.Replay(context.Background(), events.Cursor{SessionID: idC.SessionID}, filterFor(idC))
		if err != nil {
			t.Fatalf("Replay session C: %v", err)
		}
		if len(outC) != 4 {
			t.Fatalf("Replay session C returned %d events, want 4", len(outC))
		}
		for _, ev := range outC {
			if ev.Identity.SessionID != idC.SessionID {
				t.Fatalf("cross-session leak: replay for session %q returned event from session %q", idC.SessionID, ev.Identity.SessionID)
			}
		}
	})

	t.Run("Replay_HeadCursor_ReturnsNil", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		rp := mustReplayer(t, bus)
		id := mkID(1)
		const n = 3
		publishN(t, bus, id, n)
		headSeq := uint64(n)

		out, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: headSeq}, filterFor(id))
		if err != nil {
			t.Fatalf("Replay(cursor=head): %v", err)
		}
		if out != nil {
			t.Errorf("cursor at head: got %d events, want nil", len(out))
		}

		out, err = rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: headSeq + 100}, filterFor(id))
		if err != nil {
			t.Fatalf("Replay(cursor>head): %v", err)
		}
		if out != nil {
			t.Errorf("cursor past head: got %d events, want nil", len(out))
		}
	})

	t.Run("Replay_Disabled_ErrReplayUnavailable", func(t *testing.T) {
		h := factory()
		bus := h.NewReplayDisabledBus(t)
		rp := mustReplayer(t, bus)
		id := mkID(1)
		out, err := rp.Replay(context.Background(), events.Cursor{}, filterFor(id))
		if !errors.Is(err, events.ErrReplayUnavailable) {
			t.Fatalf("Replay(disabled): err=%v, want ErrReplayUnavailable", err)
		}
		if out != nil {
			t.Errorf("Replay(disabled): got %d events, want nil", len(out))
		}
	})

	t.Run("Replay_RetentionOverrun_ErrCursorTooOld", func(t *testing.T) {
		h := factory()
		const capacity = 4
		bus := h.NewBoundedRetentionBus(t, capacity)
		rp := mustReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, capacity*3) // guarantees the ring has wrapped past sequence 1.

		out, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: 1}, filterFor(id))
		if !errors.Is(err, events.ErrCursorTooOld) {
			t.Fatalf("Replay(overrun cursor): err=%v, want ErrCursorTooOld", err)
		}
		if out != nil {
			t.Errorf("Replay(overrun cursor): got %d events, want nil", len(out))
		}
	})

	t.Run("Bounds_ReportsHeadAndTail", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 5)
		head, tail, _, err := hr.Bounds(context.Background(), filterFor(id))
		if err != nil {
			t.Fatalf("Bounds: %v", err)
		}
		if head != 1 || tail != 5 {
			t.Fatalf("Bounds = (%d, %d), want (1, 5)", head, tail)
		}
	})

	t.Run("Bounds_EmptySession_ErrNoHistory", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 3) // seed a sibling session so an empty store isn't a trivial pass.

		empty := identity.Quadruple{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: "never-published"}}
		_, _, _, err := hr.Bounds(context.Background(), filterFor(empty))
		if !errors.Is(err, events.ErrNoHistory) {
			t.Fatalf("Bounds(empty session): err=%v, want ErrNoHistory", err)
		}
	})

	t.Run("Window_TailFirst_MostRecentK_OldestFirst", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 10)
		win, err := hr.Window(context.Background(), 0, 3, filterFor(id))
		if err != nil {
			t.Fatalf("Window: %v", err)
		}
		if len(win) != 3 || win[0].Sequence != 8 || win[1].Sequence != 9 || win[2].Sequence != 10 {
			t.Fatalf("Window(tail, 3) = %v, want seqs [8 9 10]", winSeqs(win))
		}
	})

	t.Run("Window_BeforeCursor_ScrollsUp", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 10)
		win, err := hr.Window(context.Background(), 8, 3, filterFor(id))
		if err != nil {
			t.Fatalf("Window: %v", err)
		}
		if len(win) != 3 || win[0].Sequence != 5 || win[1].Sequence != 6 || win[2].Sequence != 7 {
			t.Fatalf("Window(before=8, 3) = %v, want seqs [5 6 7]", winSeqs(win))
		}
	})

	t.Run("Window_EmptySession_Empty", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 3) // seed a sibling session so an empty store isn't a trivial pass.

		empty := identity.Quadruple{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: "never-published"}}
		win, err := hr.Window(context.Background(), 0, 50, filterFor(empty))
		if err != nil {
			t.Fatalf("Window(empty session): err=%v, want nil", err)
		}
		if len(win) != 0 {
			t.Fatalf("Window(empty session) returned %d events, want 0", len(win))
		}
	})

	t.Run("Window_ReachesHead", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 4)
		win, err := hr.Window(context.Background(), 0, 50, filterFor(id))
		if err != nil {
			t.Fatalf("Window: %v", err)
		}
		if len(win) != 4 || win[0].Sequence != 1 || win[3].Sequence != 4 {
			t.Fatalf("Window(all) = %v, want seqs 1..4", winSeqs(win))
		}
	})

	t.Run("Window_ReplayDisabled_ErrReplayUnavailable", func(t *testing.T) {
		h := factory()
		bus := h.NewReplayDisabledBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		if _, err := hr.Window(context.Background(), 0, 10, filterFor(id)); !errors.Is(err, events.ErrReplayUnavailable) {
			t.Fatalf("Window(disabled): err=%v, want ErrReplayUnavailable", err)
		}
	})

	t.Run("Fence_DropsLateEventsAndEmptiesHistory", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		fencer := mustFencer(t, bus)
		ctx := context.Background()
		id := mkID(1)

		publishN(t, bus, id, 2)
		if _, _, _, err := hr.Bounds(ctx, filterFor(id)); err != nil {
			t.Fatalf("pre-fence Bounds = %v, want non-empty history", err)
		}

		if err := fencer.Fence(ctx, id.Identity); err != nil {
			t.Fatalf("Fence: %v", err)
		}
		if _, _, _, err := hr.Bounds(ctx, filterFor(id)); !errors.Is(err, events.ErrNoHistory) {
			t.Fatalf("post-fence Bounds = %v, want ErrNoHistory", err)
		}

		// A late event for the fenced triple is a graceful drop, never a hard
		// error and never re-materialized history.
		if err := bus.Publish(ctx, mkEvent(id, 99)); err != nil {
			t.Fatalf("fenced publish should drop gracefully, got: %v", err)
		}
		if _, _, _, err := hr.Bounds(ctx, filterFor(id)); !errors.Is(err, events.ErrNoHistory) {
			t.Fatalf("post-fence-publish Bounds = %v, want ErrNoHistory", err)
		}
		if evs, err := hr.Window(ctx, 0, 100, filterFor(id)); err != nil || len(evs) != 0 {
			t.Fatalf("post-fence Window = (%d events, %v), want (0, nil)", len(evs), err)
		}

		// A DIFFERENT session is unaffected — no cross-session bleed from the
		// fence.
		other := mkID(2)
		if err := bus.Publish(ctx, mkEvent(other, 1)); err != nil {
			t.Fatalf("publish other session: %v", err)
		}
		if _, _, _, err := hr.Bounds(ctx, filterFor(other)); err != nil {
			t.Fatalf("other-session Bounds = %v, want retained history", err)
		}

		// Unfence restores normal retention for a reused triple.
		if err := fencer.Unfence(ctx, id.Identity); err != nil {
			t.Fatalf("Unfence: %v", err)
		}
		if err := bus.Publish(ctx, mkEvent(id, 100)); err != nil {
			t.Fatalf("post-unfence publish: %v", err)
		}
		if _, _, _, err := hr.Bounds(ctx, filterFor(id)); err != nil {
			t.Fatalf("post-unfence Bounds = %v, want retained history", err)
		}
	})

	t.Run("Fence_AfterClose_ErrBusClosed", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		fencer := mustFencer(t, bus)
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
		if err := fencer.Fence(context.Background(), id); !errors.Is(err, events.ErrBusClosed) {
			t.Fatalf("Fence after Close: err=%v, want ErrBusClosed", err)
		}
		if err := fencer.Unfence(context.Background(), id); !errors.Is(err, events.ErrBusClosed) {
			t.Fatalf("Unfence after Close: err=%v, want ErrBusClosed", err)
		}
	})

	t.Run("AfterClose_OpsRejected_ErrBusClosed", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		rp := mustReplayer(t, bus)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)

		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := bus.Publish(context.Background(), mkEvent(id, 1)); !errors.Is(err, events.ErrBusClosed) {
			t.Errorf("Publish after Close: err=%v, want ErrBusClosed", err)
		}
		if _, err := bus.Subscribe(context.Background(), filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
			t.Errorf("Subscribe after Close: err=%v, want ErrBusClosed", err)
		}
		if _, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
			t.Errorf("Replay after Close: err=%v, want ErrBusClosed", err)
		}
		if _, _, _, err := hr.Bounds(context.Background(), filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
			t.Errorf("Bounds after Close: err=%v, want ErrBusClosed", err)
		}
		if _, err := hr.Window(context.Background(), 0, 10, filterFor(id)); !errors.Is(err, events.ErrBusClosed) {
			t.Errorf("Window after Close: err=%v, want ErrBusClosed", err)
		}
	})

	t.Run("Close_Idempotent", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("Close #1: %v", err)
		}
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("Close #2 (idempotent): %v", err)
		}
	})
}

// mustReplayer type-asserts bus to events.Replayer, failing the test loudly
// (never skipping) when the assertion misses.
func mustReplayer(t *testing.T, bus events.EventBus) events.Replayer {
	t.Helper()
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("conformancetest: bus %T does not implement events.Replayer — every driver consuming this suite must", bus)
	}
	return rp
}

// mustHistoryReplayer type-asserts bus to events.HistoryReplayer, failing
// the test loudly (never skipping) when the assertion misses.
func mustHistoryReplayer(t *testing.T, bus events.EventBus) events.HistoryReplayer {
	t.Helper()
	hr, ok := bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("conformancetest: bus %T does not implement events.HistoryReplayer — every driver consuming this suite must", bus)
	}
	return hr
}

// mustFencer type-asserts bus to events.Fencer, failing the test loudly
// (never skipping) when the assertion misses.
func mustFencer(t *testing.T, bus events.EventBus) events.Fencer {
	t.Helper()
	f, ok := bus.(events.Fencer)
	if !ok {
		t.Fatalf("conformancetest: bus %T does not implement events.Fencer — every driver consuming this suite must", bus)
	}
	return f
}

// emptyTripleCases enumerates the empty / partial identity filters every
// identity-mandatory rejection scenario exercises.
func emptyTripleCases() []events.Filter {
	return []events.Filter{
		{},
		{Tenant: "T"},
		{Tenant: "T", User: "U"},
	}
}

// mkID builds a deterministic, seed-scoped identity quadruple.
func mkID(seed int) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", seed),
			UserID:    fmt.Sprintf("u-%d", seed),
			SessionID: fmt.Sprintf("s-%d", seed),
		},
		RunID: fmt.Sprintf("r-%d", seed),
	}
}

// mkEvent builds a canonical, SafePayload-carrying event for id. Using a
// SafePayload (events.SubscriptionIdleClosedPayload) keeps the suite's
// assertions independent of each driver's redaction/rehydration shape —
// the suite asserts identity, sequence and error-sentinel behavior, never
// exact payload type.
func mkEvent(id identity.Quadruple, seed int) events.Event {
	return events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(seed)}, //nolint:gosec // G115: seed is a small, non-negative test-local loop index, never attacker-controlled
	}
}

// publishN publishes n canonical events for id, failing the test loudly on
// the first Publish error.
func publishN(t *testing.T, bus events.EventBus, id identity.Quadruple, n int) {
	t.Helper()
	for i := range n {
		if err := bus.Publish(context.Background(), mkEvent(id, i)); err != nil {
			t.Fatalf("Publish #%d for %+v: %v", i, id.Identity, err)
		}
	}
}

// filterFor builds the identity-scoped Filter for id's triple.
func filterFor(id identity.Quadruple) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// winSeqs extracts the Sequence values of evs, for compact failure messages.
func winSeqs(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}
