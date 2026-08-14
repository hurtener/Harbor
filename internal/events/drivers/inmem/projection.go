// internal/events/drivers/inmem/projection.go — the in-memory driver's
// events.ProjectionSource implementation.
//
// The source pages the driver's retained ring — the same bounded,
// sequence-ordered substrate that backs Replay — reporting eviction
// honestly as a retention gap (a wrapped ring has dropped older
// events, so a projector starting before the retained head must learn
// its history is incomplete). Erased (fenced) sessions are excluded:
// the ring never retains them after a fence and the page also checks
// the fence set per event, so a concurrent Fence can never be
// re-exposed by a later page. The watermark is the driver's assigned
// sequence counter; wake notifications fire after each successful
// persistence of a canonical event via the shared best-effort hub.
package inmem

import (
	"context"

	"github.com/hurtener/Harbor/internal/events"
)

// Page implements events.ProjectionSource. It returns the bounded
// forward page of canonical retained events strictly after the cursor,
// globally ordered by Sequence. See the interface godoc for the page
// semantics; a ring-disabled bus (ReplayBufferSize 0) has no retained
// substrate and reports ProjectionUnavailable.
func (b *bus) Page(ctx context.Context, after uint64, limit int) (events.ProjectionPage, error) {
	if b.closed.Load() {
		return events.ProjectionPage{}, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.ProjectionPage{}, err
	}
	if b.ringCap == 0 {
		// No retention substrate — the source is unavailable, not empty.
		return events.ProjectionPage{Next: after, Quality: events.ProjectionUnavailable}, nil
	}
	if limit <= 0 {
		return events.ProjectionPage{Next: after, Quality: events.ProjectionCurrent}, nil
	}

	// Snapshot the ring, the watermark, the eviction flag, and the fence
	// set under one publishMu acquisition so the page is consistent with
	// the fence/purge and the ring state (mirrors Replay's snapshot
	// discipline).
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	wm := b.nextSeq
	evicted := b.evicted
	fenced := make(map[string]struct{}, len(b.fenced))
	for k := range b.fenced {
		fenced[k] = struct{}{}
	}
	b.publishMu.Unlock()

	return events.ProjectionPageFromSnapshot(snapshot, after, limit, wm, evicted, func(ev events.Event) bool {
		_, ok := fenced[fenceKey(ev.Identity.Identity)]
		return ok
	}), nil
}

// Watermark implements events.ProjectionSource. It returns the highest
// sequence assigned by the bus — the highest sequence that could have
// been retained (fenced-session holes advance the counter without a
// row, exactly as the ring's other internal paths do). A ring-disabled
// bus reports ErrProjectionUnavailable.
func (b *bus) Watermark(_ context.Context) (uint64, error) {
	if b.closed.Load() {
		return 0, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return 0, events.ErrProjectionUnavailable
	}
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	return b.nextSeq, nil
}

// Watch implements events.ProjectionSource. It registers wake on the
// shared best-effort hub, seeds the sink with the current watermark
// (so a projector that missed wakes while busy catches up
// immediately), and returns an unsubscribe handle. See the interface
// godoc for the notification contract.
func (b *bus) Watch(_ context.Context, wake chan<- uint64) (events.ProjectionWatch, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return nil, events.ErrProjectionUnavailable
	}
	unsub := b.wake.Register(wake)
	b.publishMu.Lock()
	wm := b.nextSeq
	b.publishMu.Unlock()
	if wm > 0 {
		b.wake.NotifyWatermark(wm)
	}
	return events.ProjectionWatchFunc(unsub), nil
}

// notifyProjectionWatermark wakes projection watchers after a canonical
// event was accepted and retained. Bus-internal notices are skipped
// (they are excluded from pages, so waking for them is pure noise), and
// a ring-disabled bus has no watchers to wake. Best-effort: the hub's
// non-blocking sends can never fail or delay Publish.
func (b *bus) notifyProjectionWatermark(ev events.Event) {
	if b.ringCap == 0 || events.IsBusInternalNotice(ev.Type) {
		return
	}
	b.wake.NotifyWatermark(ev.Sequence)
}
