package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

// newProjectionBus returns an in-memory bus with its retention ring
// enabled, opened through the events.ProjectionSource discovery helper.
func newProjectionBus(t *testing.T) (events.EventBus, events.ProjectionSource) {
	t.Helper()
	bus, err := inmem.New(replayCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	return bus, src
}

// publishCanonical publishes n canonical events for id (runtime.error +
// a SafePayload, so the payload survives the bus unredacted and typed).
func publishCanonical(t *testing.T, bus events.EventBus, id identity.Quadruple, n int) {
	t.Helper()
	for i := range n {
		ev := events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}
}

// publishNotice publishes a bus-internal notice type directly (it is a
// registered canonical type; the projection source must exclude it).
func publishNotice(t *testing.T, bus events.EventBus, id identity.Quadruple) {
	t.Helper()
	ev := events.Event{
		Type:     events.EventTypeBusDropped,
		Identity: id,
		Payload:  events.BusDroppedPayload{FromSeq: 1, ToSeq: 1, DroppedCount: 1},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish notice: %v", err)
	}
}

func pageSeqs(page events.ProjectionPage) []uint64 {
	out := make([]uint64, len(page.Events))
	for i, ev := range page.Events {
		out[i] = ev.Sequence
	}
	return out
}

// ---------------------------------------------------------------------------
// Forward paging: cursor semantics, quality, watermark
// ---------------------------------------------------------------------------

func TestProjection_Page_BoundedForwardPages(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	publishCanonical(t, bus, id, 10)

	// Page 1: limit 4 → seq 1..4, catching up, watermark 10.
	p1, err := src.Page(context.Background(), 0, 4)
	if err != nil {
		t.Fatalf("Page(0,4): %v", err)
	}
	if got := pageSeqs(p1); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 2, 3, 4}) {
		t.Fatalf("Page(0,4) seqs=%v, want [1 2 3 4]", got)
	}
	if p1.Next != 4 || p1.Watermark != 10 || p1.Quality != events.ProjectionCatchingUp {
		t.Fatalf("Page(0,4) Next=%d Watermark=%d Quality=%s, want Next=4 Watermark=10 CatchingUp",
			p1.Next, p1.Watermark, p1.Quality)
	}

	// Page 2: continue from cursor 4 → seq 5..8.
	p2, err := src.Page(context.Background(), p1.Next, 4)
	if err != nil {
		t.Fatalf("Page(4,4): %v", err)
	}
	if got := pageSeqs(p2); fmt.Sprint(got) != fmt.Sprint([]uint64{5, 6, 7, 8}) {
		t.Fatalf("Page(4,4) seqs=%v, want [5 6 7 8]", got)
	}
	if p2.Quality != events.ProjectionCatchingUp {
		t.Fatalf("Page(4,4) Quality=%s, want CatchingUp", p2.Quality)
	}

	// Page 3: the tail → seq 9..10 and Current.
	p3, err := src.Page(context.Background(), p2.Next, 4)
	if err != nil {
		t.Fatalf("Page(8,4): %v", err)
	}
	if got := pageSeqs(p3); fmt.Sprint(got) != fmt.Sprint([]uint64{9, 10}) {
		t.Fatalf("Page(8,4) seqs=%v, want [9 10]", got)
	}
	if p3.Quality != events.ProjectionCurrent || p3.Next != 10 || p3.Watermark != 10 {
		t.Fatalf("Page(8,4) Quality=%s Next=%d Watermark=%d, want Current 10 10",
			p3.Quality, p3.Next, p3.Watermark)
	}

	// Page past the tail → empty, Next unchanged, Current.
	p4, err := src.Page(context.Background(), 10, 4)
	if err != nil {
		t.Fatalf("Page(10,4): %v", err)
	}
	if len(p4.Events) != 0 || p4.Next != 10 || p4.Quality != events.ProjectionCurrent {
		t.Fatalf("Page(10,4) = %d events Next=%d Quality=%s, want empty Next=10 Current",
			len(p4.Events), p4.Next, p4.Quality)
	}
}

func TestProjection_Page_StrictlyAfterCursor(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	publishCanonical(t, bus, id, 10)

	page, err := src.Page(context.Background(), 6, 100)
	if err != nil {
		t.Fatalf("Page(6,100): %v", err)
	}
	if got := pageSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{7, 8, 9, 10}) {
		t.Fatalf("Page(6,100) seqs=%v, want strictly after cursor: [7 8 9 10]", got)
	}
	for _, ev := range page.Events {
		if ev.Sequence <= 6 {
			t.Fatalf("Page(6,100) returned seq %d (not strictly after cursor)", ev.Sequence)
		}
	}
}

func TestProjection_Page_EmptyBusIsCurrent(t *testing.T) {
	_, src := newProjectionBus(t)
	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != 0 || page.Quality != events.ProjectionCurrent || page.Watermark != 0 {
		t.Fatalf("empty bus page = %d events Quality=%s Watermark=%d, want empty Current 0",
			len(page.Events), page.Quality, page.Watermark)
	}
}

func TestProjection_Page_ExcludesBusInternalNotices(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	publishCanonical(t, bus, id, 1) // seq 1
	publishNotice(t, bus, id)       // seq 2 — bus-internal, excluded
	publishCanonical(t, bus, id, 1) // seq 3

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := pageSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{1, 3}) {
		t.Fatalf("page seqs=%v, want canonical-only [1 3] (notice at seq 2 excluded)", got)
	}
	// The watermark still reflects every assigned sequence, notice included.
	if page.Watermark != 3 {
		t.Fatalf("Watermark=%d, want 3 (all sequences, notice included)", page.Watermark)
	}
}

func TestProjection_Page_NonPositiveLimitIsEmptyPage(t *testing.T) {
	bus, src := newProjectionBus(t)
	publishCanonical(t, bus, mkID(1), 3)
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
// Retention gaps (the wrapped ring is never silently complete)
// ---------------------------------------------------------------------------

func TestProjection_Page_RetentionGapAfterWrap(t *testing.T) {
	cfg := replayCfgN(4) // tiny ring: wraps fast
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	id := mkID(1)
	publishCanonical(t, bus, id, 10) // ring holds seq 7..10

	// Fresh reader from the beginning: the retained head is 7, so events
	// 1..6 are gone — the page must report the gap honestly.
	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page(0,100): %v", err)
	}
	if got := pageSeqs(page); fmt.Sprint(got) != fmt.Sprint([]uint64{7, 8, 9, 10}) {
		t.Fatalf("wrapped-ring page seqs=%v, want [7 8 9 10]", got)
	}
	if !page.RetentionGap {
		t.Fatal("fresh reader over a wrapped ring must report RetentionGap=true (events 1..6 evicted)")
	}

	// A reader whose cursor is inside the retained window is unaffected
	// by the eviction — no gap for it.
	page2, err := src.Page(context.Background(), 8, 100)
	if err != nil {
		t.Fatalf("Page(8,100): %v", err)
	}
	if page2.RetentionGap {
		t.Fatal("reader past the retained head must NOT report a retention gap")
	}
	if got := pageSeqs(page2); fmt.Sprint(got) != fmt.Sprint([]uint64{9, 10}) {
		t.Fatalf("Page(8,100) seqs=%v, want [9 10]", got)
	}

	// A cursor before the retained head (but non-zero) is gap-affected.
	page3, err := src.Page(context.Background(), 3, 100)
	if err != nil {
		t.Fatalf("Page(3,100): %v", err)
	}
	if !page3.RetentionGap {
		t.Fatal("cursor before the retained head over a wrapped ring must report RetentionGap=true")
	}
}

func TestProjection_Page_NoRetentionGapBeforeWrap(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	publishCanonical(t, bus, id, 4) // ring (64) nowhere near capacity

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.RetentionGap {
		t.Fatal("a ring that never evicted must report RetentionGap=false")
	}
}

// ---------------------------------------------------------------------------
// Unavailable (ring disabled) + closed + cancellation
// ---------------------------------------------------------------------------

func TestProjection_RingDisabled_ReportsUnavailable(t *testing.T) {
	// defaultCfg has ReplayBufferSize=0 → no retention substrate.
	bus, err := inmem.New(defaultCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}

	page, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.Quality != events.ProjectionUnavailable || len(page.Events) != 0 {
		t.Fatalf("ring-disabled Page = %d events Quality=%s, want empty Unavailable",
			len(page.Events), page.Quality)
	}
	if _, err := src.Watermark(context.Background()); !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("ring-disabled Watermark err=%v, want ErrProjectionUnavailable", err)
	}
	if _, err := src.Watch(context.Background(), make(chan uint64, 1)); !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("ring-disabled Watch err=%v, want ErrProjectionUnavailable", err)
	}
}

func TestProjection_Close_ReturnsBusClosed(t *testing.T) {
	bus, src := newProjectionBus(t)
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
	_, src := newProjectionBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Page(ctx, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Page with cancelled ctx err=%v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Wake/watermark notification
// ---------------------------------------------------------------------------

func TestProjection_Watch_NotifiesOnCanonicalPublish(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	publishCanonical(t, bus, id, 1)
	select {
	case wm := <-wake:
		if wm != 1 {
			t.Fatalf("wake watermark=%d, want 1", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake after canonical publish")
	}

	publishCanonical(t, bus, id, 1)
	select {
	case wm := <-wake:
		if wm != 2 {
			t.Fatalf("wake watermark=%d, want 2", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake after second canonical publish")
	}
}

func TestProjection_Watch_DoesNotNotifyForBusInternalNotices(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	// Drain the seed wake (nothing persisted yet, so none is sent — but
	// drain defensively).
	select {
	case <-wake:
	default:
	}

	publishNotice(t, bus, id)
	select {
	case wm := <-wake:
		t.Fatalf("bus-internal notice produced a wake (watermark %d)", wm)
	case <-time.After(80 * time.Millisecond):
		// Expected: notices never wake projectors.
	}
}

func TestProjection_Watch_SeedsCurrentWatermark(t *testing.T) {
	bus, src := newProjectionBus(t)
	publishCanonical(t, bus, mkID(1), 3)

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
	bus, src := newProjectionBus(t)
	id := mkID(1)
	wake := make(chan uint64, 8)
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	watch.Unsubscribe()

	publishCanonical(t, bus, id, 1)
	select {
	case wm := <-wake:
		t.Fatalf("unsubscribed sink received watermark %d", wm)
	case <-time.After(80 * time.Millisecond):
		// Expected: no delivery after unsubscribe.
	}
}

func TestProjection_Watch_FullSinkNeverBlocksPublish(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)
	wake := make(chan uint64, 1) // tiny: fills after one wake
	watch, err := src.Watch(context.Background(), wake)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watch.Unsubscribe()

	// Publish far more than the sink can hold, never draining. Publish
	// must complete promptly (the best-effort drop), not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishCanonical(t, bus, id, 50)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full wake sink — publication coupling")
	}
	// The sink holds at most one watermark (the rest were dropped).
	select {
	case <-wake:
	default:
		t.Fatal("expected at least one wake to have landed")
	}
	select {
	case <-wake:
		t.Fatal("expected the bounded sink to hold at most one watermark")
	default:
	}
}

// ---------------------------------------------------------------------------
// Erasure/fence exclusion
// ---------------------------------------------------------------------------

func TestProjection_Fence_ExcludesErasedSession(t *testing.T) {
	bus, src := newProjectionBus(t)
	idA := mkID(1)
	idB := mkID(2)
	publishCanonical(t, bus, idA, 3) // seq 1..3
	publishCanonical(t, bus, idB, 3) // seq 4..6

	fencer, ok := bus.(events.Fencer)
	if !ok {
		t.Fatalf("inmem bus does not implement events.Fencer")
	}
	if err := fencer.Fence(context.Background(), idA.Identity); err != nil {
		t.Fatalf("Fence(A): %v", err)
	}

	// Session A is erased: its events must not be paged, and the purge
	// must prevent any replay from re-exposing them.
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

	// After Unfence, a freshly-reused session id retains normally.
	if err := fencer.Unfence(context.Background(), idA.Identity); err != nil {
		t.Fatalf("Unfence(A): %v", err)
	}
	publishCanonical(t, bus, idA, 1) // seq 7
	page2, err := src.Page(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("Page after unfence: %v", err)
	}
	if len(page2.Events) != 4 {
		t.Fatalf("post-unfence page has %d events, want 4 (B's 3 + A's new 1)", len(page2.Events))
	}
}

// ---------------------------------------------------------------------------
// Payload fidelity
// ---------------------------------------------------------------------------

func TestProjection_PayloadFidelity_SafeAndRedacted(t *testing.T) {
	bus, src := newProjectionBus(t)
	id := mkID(1)

	// SafePayload: survives the bus unredacted, so the page carries the
	// typed payload exactly as published.
	publishCanonical(t, bus, id, 1)
	// Non-safe payload: goes through the audit redactor; the page carries
	// the post-redaction RedactedMap — never the raw secret.
	secretEv := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  notSafePayload{APIKey: "leak-me-not"},
	}
	if err := bus.Publish(context.Background(), secretEv); err != nil {
		t.Fatalf("Publish secret event: %v", err)
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
	if _, ok := first.Payload.(events.SubscriptionIdleClosedPayload); !ok {
		t.Fatalf("safe payload type=%T, want typed SubscriptionIdleClosedPayload", first.Payload)
	}
	second := page.Events[1]
	rm, ok := second.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("non-safe payload type=%T, want RedactedMap", second.Payload)
	}
	if v, _ := rm.Data["api_key"].(string); v == "leak-me-not" {
		t.Fatalf("secret leaked through the projection page: %v", rm.Data)
	}
}

// ---------------------------------------------------------------------------
// Concurrent readers + publishers (N >= 100 total operations)
// ---------------------------------------------------------------------------

func TestProjection_ConcurrentReadersPublishers(t *testing.T) {
	// A ring comfortably larger than the event count: the concurrent
	// exercise must complete with every event still retained (no
	// eviction noise in the final-pass assertions).
	bus, err := inmem.New(replayCfgN(256), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	const publishers = 16
	const perPublisher = 8
	const total = publishers * perPublisher // 128 >= 100

	ids := make([]identity.Quadruple, publishers)
	for i := range publishers {
		ids[i] = mkID(100 + i)
	}

	// Publishers race readers: each publishes perPublisher canonical
	// events under its own identity.
	var pwg sync.WaitGroup
	for p := range publishers {
		pwg.Add(1)
		go func(p int) {
			defer pwg.Done()
			for i := 0; i < perPublisher; i++ {
				ev := events.Event{
					Type:     events.EventTypeRuntimeError,
					Identity: ids[p],
					Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
				}
				if err := bus.Publish(context.Background(), ev); err != nil {
					t.Errorf("publisher %d Publish: %v", p, err)
					return
				}
			}
		}(p)
	}

	// Readers page concurrently while publishers run; each collects its
	// own strictly-increasing, duplicate-free stream.
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

	// Deterministic final pass: a fresh read from the beginning must see
	// every sequence 1..128 exactly once, in order.
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
