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
// The durable log is gap-free unless opt-in audit retention (D-493)
// trimmed a session's oldest audit-only rows: RetentionGap is true when
// the reader's cursor sits before such a cut, or a trim removed rows
// during the read. Erasure is handled by fence exclusion (a fenced
// session's head is skipped) — erased sessions are never re-exposed. In
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
	var streams []*pageStream
	retentionGap := false
	skippedCorrupt := false
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return events.ProjectionPage{}, err
		}
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
		view, err := b.indexViewFromRecord(ctx, rec.Identity, rec.Bytes)
		if err != nil {
			if errors.Is(err, ErrIndexIntegrity) {
				// One corrupt session must not stall every projector. Its
				// events are reported as possibly missing (RetentionGap);
				// its own reads and publishes keep failing loudly.
				b.skipCorruptSession(ctx, rec.Identity, "projection page")
				retentionGap = true
				skippedCorrupt = true
				continue
			}
			return events.ProjectionPage{}, fmt.Errorf("durable: projection page: index head (id=%s): %w", rec.ID, err)
		}
		// A reader whose cursor sits before this session's audit-retention
		// cut has lost canonical events it never applied.
		if view.hdr.Trimmed.Count > 0 && after < view.hdr.Trimmed.ThroughSeq {
			retentionGap = true
		}
		// O(1) skip of a session with nothing after the cursor. Otherwise
		// the session joins the page merge as a LAZY row stream: rows in
		// the header tail are free, and a sealed chunk is loaded only when
		// the merge actually reaches its sequence range (verified spans
		// are cached), so a page loads the chunks holding the rows it
		// returns — not every chunk between the tail and a far-behind
		// cursor, and not limit+1 rows from every session.
		if view.hdr.RetainedCount == 0 || view.hdr.LastSeq <= after {
			continue
		}
		st, ok := b.newPageStream(ctx, view, after)
		if !ok {
			rows, gap, corrupt, err := b.eagerSessionRows(ctx, &view, after, limit+1)
			if err != nil {
				return events.ProjectionPage{}, err
			}
			retentionGap = retentionGap || gap
			if corrupt {
				skippedCorrupt = true
				continue
			}
			st = &pageStream{view: view, rows: rows}
		}
		streams = append(streams, st)
	}
	merged, gap, corrupt, err := b.mergePageStreams(ctx, streams, after, limit+1)
	if err != nil {
		return events.ProjectionPage{}, err
	}
	retentionGap = retentionGap || gap
	skippedCorrupt = skippedCorrupt || corrupt
	for _, r := range merged {
		cands = append(cands, candidate{id: r.id, meta: r.row.metadata(r.id)})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].meta.Sequence < cands[j].meta.Sequence })

	// trimmedThrough holds, per session, the trimmed through_seq observed
	// after a body vanished mid-read: candidates at or below it are gone,
	// candidates above it are retained and still served.
	trimmedThrough := map[identity.Quadruple]uint64{}
	matches := make([]events.Event, 0, limit+1)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return events.ProjectionPage{}, err
		}
		if through, gone := trimmedThrough[c.id]; gone && c.meta.Sequence <= through {
			continue
		}
		ev, err := b.loadEntryBody(ctx, c.id, c.meta.Sequence)
		if err != nil {
			through, classifyErr := b.classifyMissThrough(ctx, err)
			if classifyErr != nil {
				return events.ProjectionPage{}, fmt.Errorf("durable: projection page: %w", classifyErr)
			}
			if through < c.meta.Sequence {
				// Defensive: a missing retained body is a genuine gap.
				return events.ProjectionPage{}, fmt.Errorf("durable: projection page: index gap at seq=%d above trimmed through_seq=%d", c.meta.Sequence, through)
			}
			trimmedThrough[c.id] = through
			retentionGap = true
			continue
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
	// Overflow is an exhaustion proof from the pre-filter snapshot. Keep it
	// even if a fence lands during the final check and removes the extra
	// event: another canonical candidate still existed beyond the caller's
	// bounded page, so reporting Current would let a watermark-promoting
	// consumer skip it permanently.
	hadOverflow := len(matches) > limit
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
	// A skipped corrupt session's events were not examined, so the page
	// cannot prove it reached the watermark: never report Current, or a
	// projector would advance its checkpoint past them.
	if hadOverflow || skippedCorrupt {
		quality = events.ProjectionCatchingUp
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	next := after
	if len(matches) > 0 {
		next = matches[len(matches)-1].Sequence
	}
	return events.ProjectionPage{Events: matches, Next: next, Watermark: wm, Quality: quality, RetentionGap: retentionGap}, nil
}

// PageSession implements events.ProjectionSource: one session's forward
// page, read from that session's index alone (cost proportional to the
// session's rows above the cursor). A fenced session yields an empty
// Current page; an integrity failure or an unexplained gap fails loud.
func (b *bus) PageSession(ctx context.Context, id identity.Identity, after uint64, limit int) (events.ProjectionPage, error) {
	if b.closed.Load() {
		return events.ProjectionPage{}, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.ProjectionPage{}, err
	}
	quad := identity.Quadruple{Identity: id}
	if limit <= 0 {
		return events.ProjectionPage{Next: after, Quality: events.ProjectionCurrent}, nil
	}
	if b.bestEffort {
		if b.ringCap == 0 {
			return events.ProjectionPage{Next: after, Quality: events.ProjectionUnavailable}, nil
		}
		b.publishMu.Lock()
		snapshot := b.ringSnapshotLocked()
		wm := b.nextSeq
		evicted := b.evicted
		b.publishMu.Unlock()
		return events.ProjectionPageFromSnapshot(snapshot, after, limit, wm, evicted, func(ev events.Event) bool {
			return ev.Identity.Identity != id || b.isFenced(ev.Identity)
		}), nil
	}
	wm, _, err := b.loadSequenceAuthority(ctx)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: session page: load sequence authority: %w", err)
	}
	empty := events.ProjectionPage{Next: after, Watermark: wm, Quality: events.ProjectionCurrent}
	fences, err := b.loadDurableFenceSnapshot(ctx)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: session page: %w", err)
	}
	if fences.contains(quad) {
		return empty, nil
	}
	rec, err := b.store.Load(ctx, quad, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return empty, nil
	}
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: session page: load head: %w", err)
	}
	view, err := b.indexViewFromRecord(ctx, quad, rec.Bytes)
	if err != nil {
		return events.ProjectionPage{}, fmt.Errorf("durable: session page: index head: %w", err)
	}
	retentionGap := view.hdr.Trimmed.Count > 0 && after < view.hdr.Trimmed.ThroughSeq
	rows, err := b.collectAboveSeek(ctx, &view, after, limit+1, nonInternalRow)
	if err != nil {
		rows, err = b.collectAbove(ctx, &view, after, limit+1, nonInternalRow)
		if err != nil {
			through, classifyErr := b.classifyMissThrough(ctx, err)
			if classifyErr != nil {
				return events.ProjectionPage{}, fmt.Errorf("durable: session page: %w", classifyErr)
			}
			retentionGap = true
			kept := rows[:0]
			for _, r := range rows {
				if r.Seq > through {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
	}
	matches := make([]events.Event, 0, len(rows))
	for _, r := range rows {
		ev, err := b.loadEntryBody(ctx, quad, r.Seq)
		if err != nil {
			if _, classifyErr := b.classifyMissThrough(ctx, err); classifyErr != nil {
				return events.ProjectionPage{}, fmt.Errorf("durable: session page: %w", classifyErr)
			}
			retentionGap = true
			continue
		}
		if err := validateMetadataEvent(r.metadata(quad), ev); err != nil {
			return events.ProjectionPage{}, fmt.Errorf("durable: session page metadata mismatch seq=%d: %w", r.Seq, err)
		}
		if events.IsBusInternalNotice(ev.Type) {
			continue
		}
		matches = append(matches, ev)
	}
	quality := events.ProjectionCurrent
	if len(matches) > limit {
		quality = events.ProjectionCatchingUp
		matches = matches[:limit]
	}
	next := after
	if len(matches) > 0 {
		next = matches[len(matches)-1].Sequence
	}
	return events.ProjectionPage{Events: matches, Next: next, Watermark: wm, Quality: quality, RetentionGap: retentionGap}, nil
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
