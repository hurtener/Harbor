// internal/events/drivers/durable/projection.go — the durable driver's
// events.ProjectionSource implementation.
//
// In durable mode the source pages the PERSISTED global event log: it
// gathers every session head record via the explicitly-elevated
// maintenance scan (the same ListKind consumer as the sequence-counter
// recovery and the fleet windowed read), merges the candidate
// sequences in global ascending order, and lazily loads entry records
// up to the page bound — work is proportional to the page size, not
// the log. A Runtime restart changes nothing: the log and the
// rehydrated sequence counter ARE the source, so a projector resuming
// from its own durable checkpoint pages exactly the events it missed.
//
// The durable log is gap-free and untrimmed, so RetentionGap is always
// false; erasure is handled by fence exclusion (a fenced session's
// head is skipped) — erased sessions are never re-exposed. In
// best-effort mode (no StateStore) the source pages the fallback ring
// exactly like the in-memory driver, reporting eviction honestly.
package durable

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// Page implements events.ProjectionSource. It returns the bounded
// forward page of canonical persisted events strictly after the cursor,
// globally ordered by Sequence, across every session of the durable
// log. See the interface godoc for the page semantics.
func (b *bus) Page(ctx context.Context, after uint64, limit int) (events.ProjectionPage, error) {
	if b.closed.Load() {
		return events.ProjectionPage{}, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.ProjectionPage{}, err
	}
	if limit <= 0 {
		return events.ProjectionPage{Next: after, Quality: events.ProjectionCurrent}, nil
	}
	if b.bestEffort {
		if b.ringCap == 0 {
			// No fallback ring — the source is unavailable, not empty.
			return events.ProjectionPage{Next: after, Quality: events.ProjectionUnavailable}, nil
		}
		b.publishMu.Lock()
		snapshot := b.ringSnapshotLocked()
		wm := b.nextSeq
		evicted := b.evicted
		b.publishMu.Unlock()
		return events.ProjectionPageFromSnapshot(snapshot, after, limit, wm, evicted, func(ev events.Event) bool {
			return b.isFenced(ev.Identity)
		}), nil
	}
	return b.pageDurable(ctx, after, limit)
}

// pageDurable serves the forward page from the persisted log. The
// watermark is snapshotted FIRST, under publishMu: every sequence ≤ the
// watermark completed persistence (its head record exists) before this
// read, so the subsequent head scan is guaranteed to find every
// candidate ≤ watermark and the current/catching-up determination is
// exact. A candidate that persisted concurrently with the read (its
// head appears in the scan although its sequence exceeds the
// watermark) is served as-is — it is a real persisted event, and its
// wake notification lets the reader re-check.
func (b *bus) pageDurable(ctx context.Context, after uint64, limit int) (events.ProjectionPage, error) {
	wm, _, err := b.loadSequenceAuthority(ctx)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: projection page: load sequence authority: %w", err)
	}
	fences, err := b.loadDurableFenceSnapshot(ctx)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: projection page: %w", err)
	}

	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: projection page: scan head records: %w", err)
	}

	type candidate struct {
		id   identity.Quadruple
		meta events.EventMetadata
	}
	var cands []candidate
	for _, rec := range recs {
		// ListKind matches kindHead as a literal PREFIX — guard on the
		// exact kind so a future sibling kind under the same stem is not
		// mis-decoded (mirrors recoverNextSeq / listWindowDurable).
		if rec.Kind != kindHead {
			continue
		}
		// Erased (fenced) session — exclude its history from the page
		// (events.Fencer): an erasure that landed before this read must
		// never be re-exposed.
		if fences.contains(rec.Identity) {
			continue
		}
		hd, err := decodeHead(rec.Bytes)
		if err != nil {
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page: decode head (id=%s): %w", rec.ID, err)
		}
		hd, err = b.ensureHeadMetadata(ctx, rec.Identity, hd)
		if err != nil {
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page: index head (id=%s): %w", rec.ID, err)
		}
		for _, meta := range metadataFromHead(hd) {
			if meta.Sequence <= after {
				continue
			}
			if meta.Internal {
				continue
			}
			cands = append(cands, candidate{id: rec.Identity, meta: meta})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].meta.Sequence < cands[j].meta.Sequence })

	matches := make([]events.Event, 0, limit+1)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return events.ProjectionPage{}, err
		}
		rec, err := b.store.Load(ctx, c.id, kindEntryPrefix+seqToken(c.meta.Sequence))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is missing —
				// a torn write or a storage bug. Fail loudly rather than
				// serving a gap (the durable log's gap-free contract).
				return events.ProjectionPage{}, fmt.Errorf("durable: projection page gap — index lists seq=%d but entry record is missing: %w",
					c.meta.Sequence, err)
			}
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page: load entry seq=%d: %w", c.meta.Sequence, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page: decode entry seq=%d: %w", c.meta.Sequence, err)
		}
		if err := validateMetadataEvent(c.meta, ev); err != nil {
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page metadata mismatch seq=%d: %w", c.meta.Sequence, err)
		}
		if events.IsBusInternalNotice(ev.Type) {
			continue
		}
		matches = append(matches, ev)
		if len(matches) > limit {
			break
		}
	}
	currentFences, err := b.loadDurableFenceSnapshot(ctx)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: projection page final fence check: %w", err)
	}
	kept := matches[:0]
	for _, ev := range matches {
		if !currentFences.contains(ev.Identity) {
			kept = append(kept, ev)
		}
	}
	matches = kept

	quality := events.ProjectionCurrent
	if len(matches) > limit {
		quality = events.ProjectionCatchingUp
		matches = matches[:limit]
	}
	next := after
	if len(matches) > 0 {
		next = matches[len(matches)-1].Sequence
	}
	return events.ProjectionPage{Events: matches, Next: next, Watermark: wm, Quality: quality}, nil
}

// Watermark implements events.ProjectionSource. In durable mode it
// returns the persisted watermark — the highest sequence whose entry
// and head records completed persistence, rehydrated across restarts
// from the log. In best-effort mode it returns the fallback ring's
// assigned counter.
func (b *bus) Watermark(ctx context.Context) (uint64, error) {
	if b.closed.Load() {
		return 0, events.ErrBusClosed
	}
	if b.bestEffort && b.ringCap == 0 {
		return 0, events.ErrProjectionUnavailable
	}
	if b.bestEffort {
		b.publishMu.Lock()
		defer b.publishMu.Unlock()
		return b.nextSeq, nil
	}
	wm, _, err := b.loadSequenceAuthority(ctx)
	return wm, err
}

// Watch implements events.ProjectionSource. It registers wake on the
// shared best-effort hub, seeds the sink with the current watermark
// (so a projector that missed wakes — or restarted — catches up
// immediately), and returns an unsubscribe handle.
func (b *bus) Watch(ctx context.Context, wake chan<- uint64) (events.ProjectionWatch, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if b.bestEffort && b.ringCap == 0 {
		return nil, events.ErrProjectionUnavailable
	}
	unsub := b.wake.Register(wake)
	wm, err := b.Watermark(ctx)
	if err != nil {
		unsub()
		return nil, err
	}
	if wm > 0 {
		b.wake.NotifyWatermark(wm)
	}
	return events.ProjectionWatchFunc(unsub), nil
}

// notifyProjectionWatermark wakes projection watchers after a canonical
// event was successfully persisted (durable mode) or retained in the
// fallback ring (best-effort mode). Bus-internal notices are skipped
// (they are excluded from pages), and a best-effort bus with the ring
// disabled has no substrate and no watchers. Best-effort: the hub's
// non-blocking sends can never fail or delay Publish.
func (b *bus) notifyProjectionWatermark(ev events.Event) {
	if events.IsBusInternalNotice(ev.Type) {
		return
	}
	if b.bestEffort && b.ringCap == 0 {
		return
	}
	b.wake.NotifyWatermark(ev.Sequence)
}
