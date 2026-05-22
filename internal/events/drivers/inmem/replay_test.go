package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

// replayCfg is defaultCfg() with the replay buffer turned ON. Tests
// that want to exercise the no-replay path call replayCfgN(0).
func replayCfg() config.EventsConfig { return replayCfgN(64) }

func replayCfgN(n int) config.EventsConfig {
	c := defaultCfg()
	c.ReplayBufferSize = n
	return c
}

// newReplayBus returns a bus opened with a non-zero replay buffer
// and asserts the type assertion to events.Replayer succeeds.
func newReplayBus(t *testing.T) (events.EventBus, events.Replayer) {
	t.Helper()
	bus, err := inmem.New(replayCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus does not implement events.Replayer")
	}
	return bus, rp
}

// TestReplay_HappyPath_ReturnsRequestedRange exercises the canonical
// path: publish N events, replay from cursor mid-stream, expect
// strictly newer events in Sequence order.
func TestReplay_HappyPath_ReturnsRequestedRange(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	// Subscribe so we can capture an actual cursor.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	for i := range 8 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	got := drainN(t, sub, 8, time.Second)
	if len(got) != 8 {
		t.Fatalf("drained %d, want 8", len(got))
	}
	cursor := events.Cursor{SessionID: id.SessionID, Sequence: got[3].Sequence}

	// Replay from after the 4th event — expect events 5..8 (4 events).
	out, err := rp.Replay(context.Background(), cursor, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("Replay returned %d events, want 4", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Sequence <= out[i-1].Sequence {
			t.Errorf("non-monotonic sequences in replay: out[%d]=%d out[%d]=%d",
				i-1, out[i-1].Sequence, i, out[i].Sequence)
		}
	}
	if out[0].Sequence != cursor.Sequence+1 {
		t.Errorf("first replayed seq=%d, want cursor+1=%d", out[0].Sequence, cursor.Sequence+1)
	}
}

// TestReplay_ZeroCursor_ReturnsEntireRing pins the "from the
// beginning" semantics — Cursor{Sequence: 0} is allowed and bypasses
// the ErrCursorTooOld check.
func TestReplay_ZeroCursor_ReturnsEntireRing(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	for i := range 5 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("Replay returned %d, want 5 (the entire ring matching filter)", len(out))
	}
}

// TestReplay_HeadCursor_ReturnsNilNil pins the "nothing newer to
// replay" case — cursor at or past the head returns (nil, nil).
func TestReplay_HeadCursor_ReturnsNilNil(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	for i := range 3 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got := drainN(t, sub, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("drained %d, want 3", len(got))
	}
	headSeq := got[len(got)-1].Sequence

	// Cursor at head:
	out, err := rp.Replay(context.Background(),
		events.Cursor{SessionID: id.SessionID, Sequence: headSeq},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Replay (cursor=head): %v", err)
	}
	if out != nil {
		t.Errorf("cursor at head: got %d events, want nil", len(out))
	}

	// Cursor past head (future): also nil/nil.
	out, err = rp.Replay(context.Background(),
		events.Cursor{SessionID: id.SessionID, Sequence: headSeq + 100},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Replay (cursor>head): %v", err)
	}
	if out != nil {
		t.Errorf("cursor past head: got %d events, want nil", len(out))
	}
}

// TestReplay_FilterApplied confirms the filter discriminates by Type.
// Cross-tenant isolation is its own test.
func TestReplay_FilterApplied(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	// Publish 4 runtime.error events.
	for i := range 4 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	// Replay asking only for runtime.warning — no events.
	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
			Types: []events.EventType{events.EventTypeRuntimeWarning},
		})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("type filter mismatch: returned %d, want 0", len(out))
	}

	// Replay asking for runtime.error — all 4.
	out, err = rp.Replay(context.Background(), events.Cursor{},
		events.Filter{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
			Types: []events.EventType{events.EventTypeRuntimeError},
		})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 4 {
		t.Errorf("type filter match: returned %d, want 4", len(out))
	}
}

// TestReplay_DisabledByConfig_ErrReplayUnavailable pins the
// configured-off path: ReplayBufferSize=0 ⇒ ErrReplayUnavailable.
func TestReplay_DisabledByConfig_ErrReplayUnavailable(t *testing.T) {
	bus, err := inmem.New(replayCfgN(0), auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("type assertion to Replayer should still succeed when configured off")
	}
	id := mkID(1)
	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if !errors.Is(err, events.ErrReplayUnavailable) {
		t.Fatalf("err=%v, want ErrReplayUnavailable", err)
	}
	if out != nil {
		t.Errorf("got %d events with replay disabled, want nil", len(out))
	}
}

// TestReplay_RingOverrun_ErrCursorTooOld pins the eviction-driven
// failure: a cursor older than the ring tail returns the sentinel
// with the (oldest, requested) detail wrapped in the message.
func TestReplay_RingOverrun_ErrCursorTooOld(t *testing.T) {
	bus, rp := newReplayBus(t /* default cap=64 */)
	id := mkID(1)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Drain in the background to keep the subscriber buffer healthy.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range sub.Events() {
		}
	}()

	// Capture an early cursor; we publish one event and wait for it
	// to surface so we know its Sequence.
	earlyCursor := events.Cursor{SessionID: id.SessionID, Sequence: 1}

	// Publish enough to overrun the ring (capacity 64, so 200
	// guarantees overrun).
	for i := range 200 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	out, err := rp.Replay(context.Background(), earlyCursor,
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if !errors.Is(err, events.ErrCursorTooOld) {
		t.Fatalf("err=%v, want ErrCursorTooOld", err)
	}
	if out != nil {
		t.Errorf("got %d events on too-old cursor, want nil", len(out))
	}
	// The wrapping message must carry the (oldest, requested) detail.
	msg := err.Error()
	if !strings.Contains(msg, "oldest=") || !strings.Contains(msg, "requested=") {
		t.Errorf("ErrCursorTooOld message missing detail: %q", msg)
	}

	sub.Cancel()
	<-drainDone
}

// TestReplay_RejectsEmptyFilter ensures the same identity-mandatory
// rule Subscribe enforces also gates Replay.
func TestReplay_RejectsEmptyFilter(t *testing.T) {
	_, rp := newReplayBus(t)

	cases := []events.Filter{
		{},
		{Tenant: "T"},
		{Tenant: "T", User: "U"},
	}
	for _, f := range cases {
		_, err := rp.Replay(context.Background(), events.Cursor{}, f)
		if !errors.Is(err, events.ErrIdentityScopeRequired) {
			t.Errorf("filter %+v: err=%v, want ErrIdentityScopeRequired", f, err)
		}
	}
}

// TestReplay_AdminEmitsAuditEvent pins the parity between Subscribe
// and Replay for admin scope: both emit audit.admin_scope_used.
func TestReplay_AdminEmitsAuditEvent(t *testing.T) {
	bus, rp := newReplayBus(t)

	// Open an admin subscriber so we can observe the audit emit.
	adminSub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer adminSub.Cancel()
	// First event the admin sees is its own AdminScopeUsed.
	first := drainN(t, adminSub, 1, time.Second)
	if len(first) != 1 || first[0].Type != events.EventTypeAdminScopeUsed {
		t.Fatalf("admin first event=%v, want AdminScopeUsed", first)
	}

	// Replay with Admin true should emit a SECOND AdminScopeUsed.
	_, err = rp.Replay(context.Background(), events.Cursor{}, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := drainN(t, adminSub, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("admin second event missing")
	}
	if got[0].Type != events.EventTypeAdminScopeUsed {
		t.Errorf("type=%v, want AdminScopeUsed", got[0].Type)
	}
}

// TestReplay_GapFreeWithinRunID asserts that interleaving two RunIDs
// on one session does not introduce per-RunID holes — replay's
// Sequence ordering is sufficient because Sequence is per-bus
// monotonic.
func TestReplay_GapFreeWithinRunID(t *testing.T) {
	bus, rp := newReplayBus(t)

	tri := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	idA := identity.Quadruple{Identity: tri, RunID: "rA"}
	idB := identity.Quadruple{Identity: tri, RunID: "rB"}

	// Interleave 10 events of A and 10 of B.
	for i := range 10 {
		evA := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: idA,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		evB := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: idB,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i + 100)},
		}
		if err := bus.Publish(context.Background(), evA); err != nil {
			t.Fatal(err)
		}
		if err := bus.Publish(context.Background(), evB); err != nil {
			t.Fatal(err)
		}
	}

	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: "T", User: "U", Session: "S"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) != 20 {
		t.Fatalf("got %d events, want 20", len(out))
	}

	// Extract per-RunID slices and verify each is monotonic in
	// Sequence (per-RunID gap-free).
	var aSeqs, bSeqs []uint64
	for _, ev := range out {
		switch ev.Identity.RunID {
		case "rA":
			aSeqs = append(aSeqs, ev.Sequence)
		case "rB":
			bSeqs = append(bSeqs, ev.Sequence)
		default:
			t.Fatalf("unexpected RunID %q", ev.Identity.RunID)
		}
	}
	if len(aSeqs) != 10 || len(bSeqs) != 10 {
		t.Fatalf("per-runID counts: A=%d B=%d, want 10 each", len(aSeqs), len(bSeqs))
	}
	for i := 1; i < len(aSeqs); i++ {
		if aSeqs[i] <= aSeqs[i-1] {
			t.Errorf("RunID rA: seq[%d]=%d <= seq[%d]=%d", i, aSeqs[i], i-1, aSeqs[i-1])
		}
	}
	for i := 1; i < len(bSeqs); i++ {
		if bSeqs[i] <= bSeqs[i-1] {
			t.Errorf("RunID rB: seq[%d]=%d <= seq[%d]=%d", i, bSeqs[i], i-1, bSeqs[i-1])
		}
	}
}

// TestReplay_NoDuplicatesWithLiveSubscribe models the canonical
// reconnect-after-disconnect pattern: drain N events, capture
// cursor, open a fresh Subscribe, Replay from cursor, live-tail.
// The union must contain every published event exactly once.
func TestReplay_NoDuplicatesWithLiveSubscribe(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	// Subscriber 1 — drains some events, captures cursor, cancels.
	sub1, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 5 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got1 := drainN(t, sub1, 5, time.Second)
	if len(got1) != 5 {
		t.Fatalf("drain1: %d, want 5", len(got1))
	}
	cursor := events.Cursor{SessionID: id.SessionID, Sequence: got1[len(got1)-1].Sequence}
	sub1.Cancel()

	// More events arrive while the client is "disconnected".
	for i := 5; i < 10; i++ {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	// Reconnect: subscribe FIRST, then replay so any published-during-
	// reconnect events land in either the snapshot or the live tail.
	sub2, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub2.Cancel()

	snapshot, err := rp.Replay(context.Background(), cursor,
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// One more event after replay — must surface live.
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 999},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	// Drain whatever the live sub buffered.
	live := drainN(t, sub2, 16, 500*time.Millisecond)

	// Caller-side dedup: keep the highest Sequence we've seen and
	// drop anything <= it from the next source.
	seenSeqs := map[uint64]struct{}{}
	for _, e := range got1 {
		seenSeqs[e.Sequence] = struct{}{}
	}
	for _, e := range snapshot {
		if _, ok := seenSeqs[e.Sequence]; ok {
			t.Errorf("snapshot duplicates seq %d already in drain1", e.Sequence)
		}
		seenSeqs[e.Sequence] = struct{}{}
	}
	for _, e := range live {
		if _, ok := seenSeqs[e.Sequence]; ok {
			// The live tail MAY include events the snapshot already
			// covered — that's normal because Subscribe was opened
			// before Replay. The CALLER dedupes; the bus does not.
			// We just ensure the post-replay tail covers the new
			// event.
			continue
		}
		seenSeqs[e.Sequence] = struct{}{}
	}

	// Must have seen all 11 distinct sequences (5 + 5 + 1).
	if len(seenSeqs) != 11 {
		t.Errorf("union covered %d distinct sequences, want 11", len(seenSeqs))
	}
}

// TestReplay_CrossTenant_Isolation is the AGENTS.md §13 forbidden-
// practice pin: tenant A's Replay must not see tenant B's events.
//
// The test sizes the ring large enough to hold every published
// event so the assertion isolates the filter from any
// eviction-driven undercount; ring overrun has its own dedicated
// test (TestReplay_RingOverrun_ErrCursorTooOld).
func TestReplay_CrossTenant_Isolation(t *testing.T) {
	bus, err := inmem.New(replayCfgN(256), auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("bus is not a Replayer")
	}

	idA := mkID(1)
	idB := mkID(2)

	// Interleave 50 events from A and 50 from B.
	for i := range 50 {
		evA := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: idA,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		evB := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: idB,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i + 100)},
		}
		if err := bus.Publish(context.Background(), evA); err != nil {
			t.Fatal(err)
		}
		if err := bus.Publish(context.Background(), evB); err != nil {
			t.Fatal(err)
		}
	}

	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, ev := range out {
		if ev.Identity.TenantID != idA.TenantID {
			t.Fatalf("cross-tenant leak: replay returned tenant=%q event for tenant=%q filter",
				ev.Identity.TenantID, idA.TenantID)
		}
	}
	if len(out) != 50 {
		t.Errorf("got %d tenant-A events, want 50", len(out))
	}
}

// TestReplay_AfterClose_ErrBusClosed pins the post-Close behavior.
func TestReplay_AfterClose_ErrBusClosed(t *testing.T) {
	bus, rp := newReplayBus(t)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := mkID(1)
	_, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if !errors.Is(err, events.ErrBusClosed) {
		t.Errorf("err=%v, want ErrBusClosed", err)
	}
}

// TestReplay_ConcurrentReuse_ReuseContract is the D-025 contract:
// ≥100 goroutines doing mixed Publish/Replay/Subscribe on a single
// shared bus, under -race, no leak, snapshots internally consistent.
func TestReplay_ConcurrentReuse_ReuseContract(t *testing.T) {
	bus, rp := newReplayBus(t)
	const tenants = 16
	const publishers = 64
	const replayers = 16
	const subscribers = 32
	const eventsPerPublisher = 16

	ids := make([]identity.Quadruple, tenants)
	for i := range ids {
		ids[i] = mkID(i)
	}

	// Subscribers — one per tenant.
	subs := make([]events.Subscription, subscribers)
	for i := range subscribers {
		id := ids[i%tenants]
		s, err := bus.Subscribe(context.Background(), events.Filter{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		})
		if err != nil {
			t.Fatal(err)
		}
		subs[i] = s
	}
	// Drain in background; verify cross-tenant isolation in flight.
	var drainWG sync.WaitGroup
	mismatches := atomic.Int64{}
	for i, s := range subs {
		drainWG.Add(1)
		go func(i int, s events.Subscription) {
			defer drainWG.Done()
			id := ids[i%tenants]
			for ev := range s.Events() {
				if ev.Type == events.EventTypeBusDropped {
					continue
				}
				if ev.Identity.TenantID != id.TenantID {
					mismatches.Add(1)
				}
			}
		}(i, s)
	}

	// Publishers + replayers run concurrently.
	var wg sync.WaitGroup
	for p := range publishers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			id := ids[p%tenants]
			for j := range eventsPerPublisher {
				ev := events.Event{
					Type:     events.EventTypeRuntimeError,
					Identity: id,
					Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(p*100 + j)},
				}
				_ = bus.Publish(context.Background(), ev)
			}
		}(p)
	}

	replayInconsistencies := atomic.Int64{}
	for r := range replayers {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := ids[r%tenants]
			for range 8 {
				out, err := rp.Replay(context.Background(), events.Cursor{},
					events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
				if err != nil && !errors.Is(err, events.ErrBusClosed) {
					replayInconsistencies.Add(1)
				}
				// Each snapshot is internally consistent: strictly
				// increasing Sequence, all events belong to id's
				// tenant.
				for k := 1; k < len(out); k++ {
					if out[k].Sequence <= out[k-1].Sequence {
						replayInconsistencies.Add(1)
					}
				}
				for _, ev := range out {
					if ev.Identity.TenantID != id.TenantID {
						replayInconsistencies.Add(1)
					}
				}
			}
		}(r)
	}
	wg.Wait()

	for _, s := range subs {
		s.Cancel()
	}
	drainWG.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d cross-tenant subscriber deliveries observed", n)
	}
	if n := replayInconsistencies.Load(); n != 0 {
		t.Fatalf("%d replay snapshot inconsistencies observed", n)
	}
}

// TestReplay_NoGoroutineLeak_AfterClose asserts the standard
// baseline-restored leak guarantee, including for a bus that has
// been actively Replay'd.
func TestReplay_NoGoroutineLeak_AfterClose(t *testing.T) {
	baseline := runtime.NumGoroutine()

	bus, err := inmem.New(replayCfg(), auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("bus is not a Replayer")
	}
	id := mkID(1)

	// Saturate ring + run a few replays.
	for i := range 32 {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		_ = bus.Publish(context.Background(), ev)
	}
	for range 4 {
		_, _ = rp.Replay(context.Background(), events.Cursor{},
			events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	}

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestCursor_OrderingInvariant pins the load-bearing property: every
// event the bus emits has a strictly greater Sequence than every
// event emitted before it. The ring's snapshot ordering depends on
// this, and Replay's "strictly increasing Sequence" return contract
// depends on the ring snapshot.
func TestCursor_OrderingInvariant(t *testing.T) {
	bus, rp := newReplayBus(t)
	id := mkID(1)

	// Hammer the bus from 8 goroutines, then snapshot via Replay.
	const goroutines = 8
	const perGoroutine = 64
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := range perGoroutine {
				ev := events.Event{
					Type:     events.EventTypeRuntimeError,
					Identity: id,
					Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(g*100 + j)},
				}
				_ = bus.Publish(context.Background(), ev)
			}
		}(g)
	}
	wg.Wait()

	out, err := rp.Replay(context.Background(), events.Cursor{},
		events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Replay returned no events")
	}
	for i := 1; i < len(out); i++ {
		if out[i].Sequence <= out[i-1].Sequence {
			t.Fatalf("non-monotonic snapshot at i=%d: %d <= %d",
				i, out[i].Sequence, out[i-1].Sequence)
		}
	}
}

// avoid unused-import flagged by goimports if a refactor removes a
// reference; this anchor is intentional.
var _ = fmt.Sprintf
