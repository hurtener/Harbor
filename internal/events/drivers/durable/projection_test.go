package durable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// newProjectionBus returns a durable bus over a fresh in-memory
// StateStore, opened through the events.ProjectionSource discovery
// helper.
func newProjectionBus(t *testing.T, store state.StateStore) (events.EventBus, events.ProjectionSource) {
	t.Helper()
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	return bus, src
}

// safeCanonical builds a canonical SafePayload event (survives the
// audit redactor unredacted; the durable log stores it generically and
// rehydrates it as a RedactedMap — the documented fidelity boundary).
func safeCanonical(id identity.Quadruple, n uint64) events.Event {
	return events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: n},
	}
}

func publishSafeN(t *testing.T, bus events.EventBus, id identity.Quadruple, n int) {
	t.Helper()
	for i := range n {
		if err := bus.Publish(context.Background(), safeCanonical(id, uint64(i))); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}
}

func projSeqs(page events.ProjectionPage) []uint64 {
	out := make([]uint64, len(page.Events))
	for i, ev := range page.Events {
		out[i] = ev.Sequence
	}
	return out
}

// ---------------------------------------------------------------------------
// Forward paging over the persisted log
// ---------------------------------------------------------------------------

func TestProjection_Page_BoundedForwardPages(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")
	publishSafeN(t, bus, id, 10)

	p1, err := src.Page(context.Background(), 0, 4)
	if err != nil {
		t.Fatalf("Page(0,4): %v", err)
	}
	if got := projSeqs(p1); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 2, 3, 4}) {
		t.Fatalf("Page(0,4) seqs=%v, want [1 2 3 4]", got)
	}
	if p1.Next != 4 || p1.Watermark != 10 || p1.Quality != events.ProjectionCatchingUp {
		t.Fatalf("Page(0,4) Next=%d Watermark=%d Quality=%s, want Next=4 Watermark=10 CatchingUp",
			p1.Next, p1.Watermark, p1.Quality)
	}
	if p1.RetentionGap {
		t.Fatal("the durable log is gap-free — RetentionGap must be false")
	}

	p2, err := src.Page(context.Background(), p1.Next, 4)
	if err != nil {
		t.Fatalf("Page(4,4): %v", err)
	}
	if got := projSeqs(p2); fmt.Sprint(got) != fmt.Sprint([]uint64{5, 6, 7, 8}) {
		t.Fatalf("Page(4,4) seqs=%v, want [5 6 7 8]", got)
	}

	p3, err := src.Page(context.Background(), p2.Next, 4)
	if err != nil {
		t.Fatalf("Page(8,4): %v", err)
	}
	if got := projSeqs(p3); fmt.Sprint(got) != fmt.Sprint([]uint64{9, 10}) {
		t.Fatalf("Page(8,4) seqs=%v, want [9 10]", got)
	}
	if p3.Quality != events.ProjectionCurrent || p3.Next != 10 || p3.Watermark != 10 {
		t.Fatalf("Page(8,4) Quality=%s Next=%d Watermark=%d, want Current 10 10",
			p3.Quality, p3.Next, p3.Watermark)
	}
}

func TestProjection_Page_GloballyOrderedAcrossSessions(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	idA := quad("t1", "u1", "sA")
	idB := quad("t1", "u1", "sB")
	idC := quad("t2", "u9", "sC")

	publishSafeN(t, bus, idA, 1) // seq 1
	publishSafeN(t, bus, idB, 2) // seq 2..3
	publishSafeN(t, bus, idA, 1) // seq 4
	publishSafeN(t, bus, idC, 1) // seq 5

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := projSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("global page seqs=%v, want [1 2 3 4 5] (globally ordered by Sequence)", got)
	}
	wantSession := map[uint64]string{1: "sA", 2: "sB", 3: "sB", 4: "sA", 5: "sC"}
	for _, ev := range page.Events {
		if wantSession[ev.Sequence] != ev.Identity.SessionID {
			t.Fatalf("seq %d belongs to session %q, want %q", ev.Sequence, ev.Identity.SessionID, wantSession[ev.Sequence])
		}
	}
}

func TestProjection_Page_AfterRestart(t *testing.T) {
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	// First Runtime: 6 events persisted, then the bus closes.
	bus1, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (run 1): %v", err)
	}
	publishSafeN(t, bus1, id, 6)
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("bus1.Close: %v", err)
	}

	// Second Runtime over the SAME store: the source pages the persisted
	// log — restart changes nothing for a projector resuming its cursor.
	bus2, src := newProjectionBus(t, store)
	p1, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page after restart: %v", err)
	}
	if got := projSeqs(p1); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("post-restart page seqs=%v, want [1..6]", got)
	}
	if p1.Watermark != 6 {
		t.Fatalf("post-restart Watermark=%d, want 6 (rehydrated from the log)", p1.Watermark)
	}

	// Post-restart publishes extend the sequence strictly past the
	// pre-restart high-water mark.
	publishSafeN(t, bus2, id, 2) // seq 7..8
	p2, err := src.Page(context.Background(), 6, 100)
	if err != nil {
		t.Fatalf("Page(6,100): %v", err)
	}
	if got := projSeqs(p2); fmt.Sprint(got) != fmt.Sprint([]uint64{7, 8}) {
		t.Fatalf("post-restart forward page seqs=%v, want [7 8] (no collision, no skip)", got)
	}
	if p2.Quality != events.ProjectionCurrent || p2.Watermark != 8 {
		t.Fatalf("Page(6,100) Quality=%s Watermark=%d, want Current 8", p2.Quality, p2.Watermark)
	}
}

func TestProjection_Page_ExcludesBusInternalNotices(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")
	publishSafeN(t, bus, id, 1) // seq 1
	// A notice type published directly IS persisted by the durable
	// driver; the projection source must exclude it from pages.
	notice := events.Event{
		Type:     events.EventTypeBusDropped,
		Identity: id,
		Payload:  events.BusDroppedPayload{FromSeq: 1, ToSeq: 1, DroppedCount: 1},
	}
	if err := bus.Publish(context.Background(), notice); err != nil {
		t.Fatalf("Publish notice: %v", err)
	}
	publishSafeN(t, bus, id, 1) // seq 3

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := projSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 3}) {
		t.Fatalf("page seqs=%v, want canonical-only [1 3]", got)
	}
	if page.Watermark != 3 {
		t.Fatalf("Watermark=%d, want 3", page.Watermark)
	}
}

func TestProjection_Page_NonPositiveLimitIsEmptyPage(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	publishSafeN(t, bus, quad("t1", "u1", "s1"), 3)
	page, err := src.Page(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Page(0,0): %v", err)
	}
	if len(page.Events) != 0 || page.Quality != events.ProjectionCurrent || page.Next != 0 {
		t.Fatalf("Page(0,0) = %d events Quality=%s Next=%d, want empty Current Next=0",
			len(page.Events), page.Quality, page.Next)
	}
}

// ---------------------------------------------------------------------------
// Erasure/fence exclusion
// ---------------------------------------------------------------------------

func TestProjection_Fence_ExcludesErasedSession(t *testing.T) {
	store := newInmemStore(t)
	bus, src := newProjectionBus(t, store)
	idA := quad("t1", "u1", "sA")
	idB := quad("t1", "u1", "sB")
	publishSafeN(t, bus, idA, 3) // seq 1..3
	publishSafeN(t, bus, idB, 3) // seq 4..6

	fencer, ok := bus.(events.Fencer)
	if !ok {
		t.Fatalf("durable bus does not implement events.Fencer")
	}
	if err := fencer.Fence(context.Background(), idA.Identity); err != nil {
		t.Fatalf("Fence(A): %v", err)
	}

	// Session A is erased: its persisted heads are skipped by the page —
	// an erasure can never be re-exposed by a forward read.
	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("fenced page has %d events, want 3 (session A excluded)", len(page.Events))
	}
	for _, ev := range page.Events {
		if ev.Identity.SessionID == idA.SessionID {
			t.Fatalf("fenced session A event re-exposed: %+v", ev.Identity)
		}
	}

	// The erasure cascade physically removes the fenced session's
	// persisted records (the durable fence is in-memory; DeleteScope is
	// the durable sweep, exactly as the session-erasure flow runs it).
	if n, err := store.DeleteScope(context.Background(), idA.Identity); err != nil {
		t.Fatalf("DeleteScope(A): %v", err)
	} else if n < 4 {
		t.Fatalf("DeleteScope(A) removed %d records, want >= 4 (3 entries + head)", n)
	}

	// Unfence + fresh publish on the reused triple: the NEW events are
	// retained, the ERASED history stays gone — no replay re-exposure.
	if err := fencer.Unfence(context.Background(), idA.Identity); err != nil {
		t.Fatalf("Unfence(A): %v", err)
	}
	publishSafeN(t, bus, idA, 1) // seq 7
	page2, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page after unfence: %v", err)
	}
	if len(page2.Events) != 4 {
		t.Fatalf("post-unfence page has %d events, want 4 (B's 3 + A's new 1; erased A history must not resurface)", len(page2.Events))
	}
	for _, ev := range page2.Events {
		if ev.Identity.SessionID == idA.SessionID && ev.Sequence <= 3 {
			t.Fatalf("erased session A history re-exposed after unfence: seq %d", ev.Sequence)
		}
	}
}

// ---------------------------------------------------------------------------
// Payload fidelity (the durable log's documented RedactedMap boundary)
// ---------------------------------------------------------------------------

func TestProjection_PayloadFidelity_RedactedMapBoundary(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")

	// SafePayload: the durable log persists it generically, so the page
	// rehydrates it as a RedactedMap whose fields match the published
	// values — never a typed payload (the documented fidelity boundary).
	publishSafeN(t, bus, id, 1)
	// Non-safe payload: goes through the audit redactor; the persisted
	// shape is the post-redaction map.
	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  testPayload{Note: "hello"},
	}); err != nil {
		t.Fatalf("Publish non-safe: %v", err)
	}

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("page has %d events, want 2", len(page.Events))
	}
	first := page.Events[0]
	if first.Type != events.EventTypeRuntimeError || first.Sequence != 1 || first.Identity.SessionID != id.SessionID {
		t.Fatalf("safe event fidelity broken: %+v", first)
	}
	rm, ok := first.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("durable page payload type=%T, want RedactedMap (the documented boundary)", first.Payload)
	}
	if rm.Data["subscriber_id"] == nil && rm.Data["SubscriberID"] == nil {
		t.Fatalf("persisted safe payload fields missing: %v", rm.Data)
	}
	second := page.Events[1]
	rm2, ok := second.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("non-safe payload type=%T, want RedactedMap", second.Payload)
	}
	if rm2.Data["note"] == nil && rm2.Data["Note"] == nil {
		t.Fatalf("persisted non-safe payload fields missing: %v", rm2.Data)
	}
}

// ---------------------------------------------------------------------------
// Wake/watermark notification
// ---------------------------------------------------------------------------

func TestProjection_Watch_NotifiesOnCanonicalPublish(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")
	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	publishSafeN(t, bus, id, 1)
	select {
	case wm := <-wake:
		if wm != 1 {
			t.Fatalf("wake watermark=%d, want 1", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake after canonical publish")
	}
}

func TestProjection_Watch_SeedsCurrentWatermark(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	publishSafeN(t, bus, quad("t1", "u1", "s1"), 3)

	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	select {
	case wm := <-wake:
		if wm != 3 {
			t.Fatalf("seed watermark=%d, want 3 (current)", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not seed the current watermark")
	}
}

func TestProjection_Watch_UnsubscribeStopsDelivery(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")
	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	watch.Unsubscribe()

	publishSafeN(t, bus, id, 1)
	select {
	case wm := <-wake:
		t.Fatalf("unsubscribed sink received watermark %d", wm)
	case <-time.After(80 * time.Millisecond):
		// Expected: no delivery after unsubscribe.
	}
}

func TestProjection_Watch_FullSinkNeverBlocksPublish(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	id := quad("t1", "u1", "s1")
	wake := make(chan uint64, 1)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		publishSafeN(t, bus, id, 50)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full wake sink — publication coupling")
	}
}

// ---------------------------------------------------------------------------
// Closed bus + cancellation
// ---------------------------------------------------------------------------

func TestProjection_Close_ReturnsBusClosed(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := src.Page(context.Background(), 0, 10); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Page after close err=%v, want ErrBusClosed", err)
	}
	if _, err := src.Watermark(context.Background()); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Watermark after close err=%v, want ErrBusClosed", err)
	}
	if _, err := src.Watch(context.Background(), make(chan uint64, 1)); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Watch after close err=%v, want ErrBusClosed", err)
	}
}

func TestProjection_CancelledContext(t *testing.T) {
	_, src := newProjectionBus(t, newInmemStore(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Page(ctx, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Page with cancelled ctx err=%v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Best-effort mode (no StateStore): the fallback ring, eviction honest
// ---------------------------------------------------------------------------

func TestProjection_BestEffortRing_PagesWithRetentionGap(t *testing.T) {
	cfg := durableCfg()
	cfg.ReplayBufferSize = 4 // tiny fallback ring: wraps fast
	bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), nil,
		durable.WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("durable.New (best-effort): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	id := quad("t1", "u1", "s1")
	publishSafeN(t, bus, id, 10) // ring holds seq 7..10

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := projSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{7, 8, 9, 10}) {
		t.Fatalf("best-effort page seqs=%v, want [7 8 9 10]", got)
	}
	if !page.RetentionGap {
		t.Fatal("best-effort wrapped ring must report RetentionGap=true")
	}
	if page.Watermark != 10 {
		t.Fatalf("best-effort Watermark=%d, want 10", page.Watermark)
	}

	// Watch works on the best-effort ring too.
	wake := make(chan uint64, 4)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch (best-effort): %v", err)
	}
	defer watch.Unsubscribe()
	select {
	case wm := <-wake:
		if wm != 10 {
			t.Fatalf("best-effort seed watermark=%d, want 10", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("best-effort Watch did not seed the watermark")
	}
}

// ---------------------------------------------------------------------------
// Concurrent readers + publishers (N >= 100 total operations)
// ---------------------------------------------------------------------------

func TestProjection_ConcurrentReadersPublishers(t *testing.T) {
	bus, src := newProjectionBus(t, newInmemStore(t))
	const publishers = 16
	const perPublisher = 8
	const total = publishers * perPublisher // 128 >= 100

	ids := make([]identity.Quadruple, publishers)
	for i := range publishers {
		ids[i] = quad("t1", fmt.Sprintf("u%d", i), fmt.Sprintf("s%d", i))
	}

	var pwg sync.WaitGroup
	for p := range publishers {
		pwg.Add(1)
		go func(p int) {
			defer pwg.Done()
			for i := range perPublisher {
				if err := bus.Publish(context.Background(), safeCanonical(ids[p], uint64(i))); err != nil {
					t.Errorf("publisher %d Publish: %v", p, err)
					return
				}
			}
		}(p)
	}

	const readers = 8
	errs := make(chan error, readers)
	var rwg sync.WaitGroup
	for r := range readers {
		rwg.Add(1)
		go func(r int) {
			defer rwg.Done()
			seen := make(map[uint64]bool, total)
			last := uint64(0)
			var cursor uint64
			deadline := time.Now().Add(10 * time.Second)
			for {
				page, err := src.Page(context.Background(), cursor, 7)
				if err != nil {
					errs <- fmt.Errorf("reader %d Page: %w", r, err)
					return
				}
				for _, ev := range page.Events {
					if ev.Sequence <= last {
						errs <- fmt.Errorf("reader %d: non-increasing sequence %d after %d", r, ev.Sequence, last)
						return
					}
					if seen[ev.Sequence] {
						errs <- fmt.Errorf("reader %d: duplicate sequence %d", r, ev.Sequence)
						return
					}
					seen[ev.Sequence] = true
					last = ev.Sequence
				}
				cursor = page.Next
				if page.Quality == events.ProjectionCurrent && page.Watermark >= total {
					return
				}
				if time.Now().After(deadline) {
					errs <- fmt.Errorf("reader %d timed out at cursor %d watermark %d (saw %d)", r, cursor, page.Watermark, len(seen))
					return
				}
				time.Sleep(100 * time.Microsecond) // eventually-style backoff
			}
		}(r)
	}
	pwg.Wait()
	rwg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	final, err := src.Page(context.Background(), 0, total)
	if err != nil {
		t.Fatalf("final Page: %v", err)
	}
	if len(final.Events) != total {
		t.Fatalf("final page has %d events, want %d", len(final.Events), total)
	}
	for i, ev := range final.Events {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("final page seq[%d]=%d, want %d (gap or reorder)", i, ev.Sequence, i+1)
		}
	}
	if final.Quality != events.ProjectionCurrent {
		t.Fatalf("final page Quality=%s, want Current", final.Quality)
	}
	if wm, err := src.Watermark(context.Background()); err != nil || wm != total {
		t.Fatalf("final Watermark=%d err=%v, want %d", wm, err, total)
	}
}
