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
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
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
		// The wrapped message must carry the (oldest, requested) detail so
		// a caller falling through to a durable log can interpret the gap
		// (the ErrCursorTooOld sentinel's documented contract).
		msg := err.Error()
		if !strings.Contains(msg, "oldest=") || !strings.Contains(msg, "requested=") {
			t.Errorf("ErrCursorTooOld message missing (oldest, requested) detail: %q", msg)
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

	// ListWindow — the cross-session, time-ranged, cursor-paged
	// `events.list` read. Both V1 drivers serve it through the extended
	// HistoryReplayer seam (no capability ceremony); the scenarios pin the
	// paging grammar (shared with state.history), the non-admin own-triple
	// scoping, and the admin cross-session fan-in.

	t.Run("ListWindow_TailFirst_MostRecentK_OldestFirst", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 10)
		page, err := hr.ListWindow(context.Background(), events.EventListQuery{
			Filter: wireFilterFor(id), Limit: 3,
		})
		if err != nil {
			t.Fatalf("ListWindow: %v", err)
		}
		if got := winSeqs(page.Events); len(got) != 3 || got[0] != 8 || got[1] != 9 || got[2] != 10 {
			t.Fatalf("ListWindow(tail, 3) = %v, want seqs [8 9 10]", got)
		}
		if !page.HasMore || page.NextCursor != 8 {
			t.Fatalf("ListWindow(tail, 3): HasMore=%v NextCursor=%d, want true / 8", page.HasMore, page.NextCursor)
		}
	})

	t.Run("ListWindow_CursorScrollsUp_ToHead", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		publishN(t, bus, id, 5)
		// First page (tail): seqs [3 4 5], NextCursor 3, HasMore.
		p1, err := hr.ListWindow(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Limit: 3})
		if err != nil {
			t.Fatalf("ListWindow page1: %v", err)
		}
		if p1.NextCursor != 3 || !p1.HasMore {
			t.Fatalf("page1 NextCursor=%d HasMore=%v, want 3 / true", p1.NextCursor, p1.HasMore)
		}
		// Second page (scroll up): seqs [1 2], head reached — NextCursor 0.
		p2, err := hr.ListWindow(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Before: p1.NextCursor, Limit: 3})
		if err != nil {
			t.Fatalf("ListWindow page2: %v", err)
		}
		if got := winSeqs(p2.Events); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatalf("page2 = %v, want seqs [1 2]", got)
		}
		if p2.HasMore || p2.NextCursor != 0 {
			t.Fatalf("page2 HasMore=%v NextCursor=%d, want false / 0 at the head", p2.HasMore, p2.NextCursor)
		}
	})

	t.Run("ListWindow_NonAdmin_ScopedToOwnTriple", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		idA, idB := mkID(1), mkID(2)
		publishN(t, bus, idA, 4)
		publishN(t, bus, idB, 4)
		page, err := hr.ListWindow(context.Background(), events.EventListQuery{Filter: wireFilterFor(idA), Limit: 50})
		if err != nil {
			t.Fatalf("ListWindow: %v", err)
		}
		if len(page.Events) != 4 {
			t.Fatalf("non-admin ListWindow returned %d events, want 4 (own triple only)", len(page.Events))
		}
		for _, ev := range page.Events {
			if ev.Identity.SessionID != idA.SessionID {
				t.Fatalf("non-admin ListWindow leaked session %q, want only %q", ev.Identity.SessionID, idA.SessionID)
			}
		}
	})

	t.Run("ListWindow_Admin_FansInAcrossSessions", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		idA, idB := mkID(1), mkID(2)
		publishN(t, bus, idA, 3)
		publishN(t, bus, idB, 3)
		// An admin query naming no identity axes (empty sets = any) fans in
		// across every session in global-sequence order.
		page, err := hr.ListWindow(context.Background(), events.EventListQuery{
			Filter: prototypes.EventFilter{}, Admin: true, Limit: 50,
		})
		if err != nil {
			t.Fatalf("ListWindow(admin): %v", err)
		}
		sessions := map[string]int{}
		for _, ev := range page.Events {
			sessions[ev.Identity.SessionID]++
		}
		if sessions[idA.SessionID] != 3 || sessions[idB.SessionID] != 3 {
			t.Fatalf("admin fan-in saw sessions %v, want 3 from each of A/B", sessions)
		}
		// Global-sequence order: oldest-first, strictly increasing.
		for i := 1; i < len(page.Events); i++ {
			if page.Events[i-1].Sequence >= page.Events[i].Sequence {
				t.Fatalf("admin fan-in not in global-sequence order at %d: %v", i, winSeqs(page.Events))
			}
		}
	})

	t.Run("ListWindow_EmptyTripleNonAdmin_ErrIdentityScopeRequired", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		hr := mustHistoryReplayer(t, bus)
		if _, err := hr.ListWindow(context.Background(), events.EventListQuery{
			Filter: prototypes.EventFilter{}, Limit: 10,
		}); !errors.Is(err, events.ErrIdentityScopeRequired) {
			t.Fatalf("ListWindow(empty-triple non-admin): err=%v, want ErrIdentityScopeRequired", err)
		}
	})

	t.Run("ListWindow_ReplayDisabled_ErrReplayUnavailable", func(t *testing.T) {
		h := factory()
		bus := h.NewReplayDisabledBus(t)
		hr := mustHistoryReplayer(t, bus)
		id := mkID(1)
		if _, err := hr.ListWindow(context.Background(), events.EventListQuery{
			Filter: wireFilterFor(id), Limit: 10,
		}); !errors.Is(err, events.ErrReplayUnavailable) {
			t.Fatalf("ListWindow(disabled): err=%v, want ErrReplayUnavailable", err)
		}
	})

	// Aggregate — the time-bucketed count series (the `events.aggregate`
	// Protocol method) built over each driver's bus. The aggregator sources
	// its snapshot from the SAME HistoryReplayer windowed fan-in ListWindow
	// uses, so it must fan in session-less on every driver (the hole that let
	// the durable-500 bug ship: aggregate was never parametrized by driver).
	// The scenario carries an explicit ISOLATION assertion — parity is not
	// isolation; two drivers sharing a cross-tenant leak agree on the leaky
	// answer.
	t.Run("Aggregate_Admin_SessionLess_FansInAcrossSessions", func(t *testing.T) {
		h := factory()
		bus := h.NewBus(t)
		clk := fixedConformanceClock{t: aggregateAnchor}
		agg, err := events.NewAggregator(bus, events.WithAggregatorClock(clk))
		if err != nil {
			t.Fatalf("NewAggregator: %v", err)
		}
		// Multi-tenant fixture: 3 tenants × 2 users × 2 sessions × 1 event =
		// 4 events per tenant, 12 total, all backdated into the window.
		tenants := []string{"agg-tenant-A", "agg-tenant-B", "agg-tenant-C"}
		perTenant := int64(4)
		at := aggregateAnchor.Add(-10 * time.Minute)
		for _, ten := range tenants {
			for u := 1; u <= 2; u++ {
				for s := 1; s <= 2; s++ {
					publishAt(t, bus, identity.Quadruple{Identity: identity.Identity{
						TenantID:  ten,
						UserID:    fmt.Sprintf("%s-u%d", ten, u),
						SessionID: fmt.Sprintf("%s-u%d-s%d", ten, u, s),
					}}, at)
				}
			}
		}

		req := prototypes.EventAggregateRequest{Window: time.Hour, Bucket: time.Minute}

		// Leg 1 — widened, empty filter → fan in across every session/tenant.
		wide, err := agg.Aggregate(context.Background(), req, true)
		if err != nil {
			t.Fatalf("Aggregate(widened, empty): %v", err)
		}
		if got := sumRuntimeError(wide); got != perTenant*int64(len(tenants)) {
			t.Fatalf("widened session-less aggregate saw %d events, want %d (fan-in across all sessions failed)", got, perTenant*int64(len(tenants)))
		}

		// Leg 2 — ISOLATION (widened, single tenant): a widened read naming
		// ONLY tenant-B attributes to tenant-B and leaks neither A nor C.
		bReq := prototypes.EventAggregateRequest{
			Filter: prototypes.EventFilter{TenantIDs: []string{tenants[1]}},
			Window: time.Hour, Bucket: time.Minute,
		}
		bOnly, err := agg.Aggregate(context.Background(), bReq, true)
		if err != nil {
			t.Fatalf("Aggregate(widened, tenant-B): %v", err)
		}
		if got := sumRuntimeError(bOnly); got != perTenant {
			t.Fatalf("widened tenant-B aggregate saw %d events, want %d (cross-tenant leak or undercount)", got, perTenant)
		}

		// Leg 3 — ISOLATION (non-admin, own tuple): a non-admin read scoped
		// to one session sees ONLY that session, not the tenant's other
		// sessions/users, not other tenants.
		ownReq := prototypes.EventAggregateRequest{
			Filter: prototypes.EventFilter{
				TenantIDs:  []string{tenants[0]},
				UserIDs:    []string{tenants[0] + "-u1"},
				SessionIDs: []string{tenants[0] + "-u1-s1"},
			},
			Window: time.Hour, Bucket: time.Minute,
		}
		own, err := agg.Aggregate(context.Background(), ownReq, false)
		if err != nil {
			t.Fatalf("Aggregate(non-admin, own tuple): %v", err)
		}
		if got := sumRuntimeError(own); got != 1 {
			t.Fatalf("non-admin own-session aggregate saw %d events, want 1 (scoped to own tuple)", got)
		}

		// Leg 4 — ATTRIBUTION (widened, ByTenant): the same widened,
		// empty-filter fan-in with per-tenant attribution splits the totals
		// across exactly the fixture's tenants, each tenant reconciles to
		// perTenant, and every bucket satisfies
		// Σ_tenant CountsByTenant[tenant][type] == Counts[type]. This runs on
		// EVERY driver the matrix parametrizes, so inmem and durable must
		// agree on the attribution (the same parity the fan-in leg pins).
		attrReq := prototypes.EventAggregateRequest{Window: time.Hour, Bucket: time.Minute, ByTenant: true}
		attr, err := agg.Aggregate(context.Background(), attrReq, true)
		if err != nil {
			t.Fatalf("Aggregate(widened, ByTenant): %v", err)
		}
		attrKeys := map[string]struct{}{}
		for _, b := range attr.Buckets {
			// Per-bucket Σ invariant.
			perType := map[string]int64{}
			for tenant, byType := range b.CountsByTenant {
				attrKeys[tenant] = struct{}{}
				for typ, n := range byType {
					perType[typ] += n
				}
			}
			for typ, want := range b.Counts {
				if perType[typ] != want {
					t.Fatalf("attribution Σ mismatch: bucket type %q Σ=%d want Counts=%d", typ, perType[typ], want)
				}
			}
		}
		// Keys are a SUBSET of the fixture tenants (no leak of a tenant the
		// widened filter did not authorize). A subset check is more robust than
		// exact cardinality — it does not assume every fixture tenant had an
		// in-window event.
		tenantSet := map[string]struct{}{}
		for _, ten := range tenants {
			tenantSet[ten] = struct{}{}
		}
		for k := range attrKeys {
			if _, ok := tenantSet[k]; !ok {
				t.Fatalf("attribution leaked tenant %q outside the authorized set %v", k, tenants)
			}
		}
		// Every fixture tenant (each seeded with perTenant in-window events)
		// reconciles to perTenant.
		for _, ten := range tenants {
			var total int64
			for _, b := range attr.Buckets {
				total += b.CountsByTenant[ten][string(events.EventTypeRuntimeError)]
			}
			if total != perTenant {
				t.Fatalf("attribution for %s = %d, want %d", ten, total, perTenant)
			}
		}

		// Leg 5 — ATTRIBUTION IS OPT-IN + FAIL-CLOSED: the same widened read
		// WITHOUT ByTenant carries no attribution, and a NON-widened read WITH
		// ByTenant is ignored (no attribution) — an unelevated caller gains
		// nothing it could not already read.
		noFlag, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{Window: time.Hour, Bucket: time.Minute}, true)
		if err != nil {
			t.Fatalf("Aggregate(widened, no ByTenant): %v", err)
		}
		for _, b := range noFlag.Buckets {
			if len(b.CountsByTenant) != 0 {
				t.Fatalf("widened read without ByTenant carried attribution %v, want none", b.CountsByTenant)
			}
		}
		ownAttr, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
			Filter: prototypes.EventFilter{
				TenantIDs:  []string{tenants[0]},
				UserIDs:    []string{tenants[0] + "-u1"},
				SessionIDs: []string{tenants[0] + "-u1-s1"},
			},
			Window: time.Hour, Bucket: time.Minute, ByTenant: true,
		}, false)
		if err != nil {
			t.Fatalf("Aggregate(non-admin, ByTenant): %v", err)
		}
		for _, b := range ownAttr.Buckets {
			if len(b.CountsByTenant) != 0 {
				t.Fatalf("non-widened ByTenant read carried attribution %v, want none (fail-closed)", b.CountsByTenant)
			}
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
// identity-mandatory rejection scenario exercises: fully empty, each
// leading-prefix partial, and the single-component user-only /
// session-only shapes (a session id alone must never satisfy the triple).
func emptyTripleCases() []events.Filter {
	return []events.Filter{
		{},
		{Tenant: "T"},
		{Tenant: "T", User: "U"},
		{User: "U"},
		{Session: "S"},
	}
}

// aggregateAnchor is the fixed clock instant the aggregate conformance
// scenario anchors its window on, so backdated fixture events fall in a
// deterministic bucket range across every driver.
var aggregateAnchor = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fixedConformanceClock returns a fixed instant from Now(), so the aggregate
// conformance scenario's bucket arithmetic is driver-independent.
type fixedConformanceClock struct{ t time.Time }

func (f fixedConformanceClock) Now() time.Time { return f.t }

// publishAt publishes one canonical (runtime.error) event for id backdated to
// `at`, so the aggregator counts it into a known window slice.
func publishAt(t *testing.T, bus events.EventBus, id identity.Quadruple, at time.Time) {
	t.Helper()
	ev := mkEvent(id, 1)
	ev.OccurredAt = at.UTC()
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publishAt %+v: %v", id.Identity, err)
	}
}

// sumRuntimeError totals the runtime.error counts across an aggregate
// response's buckets — the fixture publishes only that event type.
func sumRuntimeError(resp prototypes.EventAggregateResponse) int64 {
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts[string(events.EventTypeRuntimeError)]
	}
	return total
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

// wireFilterFor builds the single-session wire EventFilter for id's triple
// — the shape the events.list handler folds a non-widened caller's triple
// into before calling ListWindow.
func wireFilterFor(id identity.Quadruple) prototypes.EventFilter {
	return prototypes.EventFilter{
		TenantIDs:  []string{id.TenantID},
		UserIDs:    []string{id.UserID},
		SessionIDs: []string{id.SessionID},
	}
}

// winSeqs extracts the Sequence values of evs, for compact failure messages.
func winSeqs(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}
