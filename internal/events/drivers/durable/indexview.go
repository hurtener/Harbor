package durable

// Reader-side access to a session's routing index. Every durable read goes
// through one indexView plus a backward rowIterator, whatever the persisted
// format: a v2 header is walked tail-first and then chunk by chunk through the
// verified chain, while a legacy (v1) head is presented as a virtual header
// whose tail holds every row, so the legacy read semantics are unchanged.
//
// A chunk or entry body that disappears mid-read is classified by reloading
// the header (missWasTrimmed): if the missing position now lies inside the
// trimmed range, audit retention removed it during the read and each reader
// degrades honestly (ErrCursorTooOld, a shortened/truncated page, or
// RetentionGap). Anything else is a genuine gap and fails loudly.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// indexView is one reader's snapshot of a session's routing index.
type indexView struct {
	id identity.Quadruple
	// hdr is the verified v2 header, or a virtual header for a legacy head
	// (all rows in Tail, no chunks, no trim).
	hdr indexHeader
	// legacy marks a v1 head presented through the virtual header.
	legacy bool
	// exists is false when the session has no head record at all.
	exists bool
}

// indexMissError reports a chunk or entry record that an index referenced
// but storage no longer holds. Readers classify it with missWasTrimmed.
type indexMissError struct {
	id      identity.Quadruple
	entry   bool
	chunk   uint64
	seq     uint64
	wrapped error
}

func (e *indexMissError) Error() string {
	if e.entry {
		return fmt.Sprintf("durable: index lists seq=%d but entry record is missing: %v", e.seq, e.wrapped)
	}
	return fmt.Sprintf("durable: index header references chunk %d but it is missing: %v", e.chunk, e.wrapped)
}

func (e *indexMissError) Unwrap() error { return e.wrapped }

// noteIntegrity counts and logs an index integrity failure. It returns err so
// call sites can wrap and return in one expression.
func (b *bus) noteIntegrity(ctx context.Context, id identity.Quadruple, err error) error {
	b.recordIntegrity(ctx, id, err)
	return err
}

// recordIntegrity counts and logs err when it is an index integrity failure.
func (b *bus) recordIntegrity(ctx context.Context, id identity.Quadruple, err error) {
	if err == nil || !errors.Is(err, ErrIndexIntegrity) {
		return
	}
	b.indexIntegrityFailures.Add(1)
	b.logger.ErrorContext(ctx, "durable: event index integrity failure; the session's index is refused until it is rebuilt (harbor events rebuild-session-index)",
		slog.String("driver", "durable"),
		slog.String("head_identity_hash", headIdentityHash(id)),
		slog.String("error", err.Error()))
}

// skipCorruptSession records that a fleet-wide read (projection page or admin
// list) skipped a session whose index failed integrity checks. The failure was
// already counted by noteIntegrity; this adds the skip counter and a warning.
func (b *bus) skipCorruptSession(ctx context.Context, id identity.Quadruple, reader string) {
	b.corruptSessionSkips.Add(1)
	b.logger.WarnContext(ctx, "durable: fleet read skipped a session whose event index fails integrity checks",
		slog.String("driver", "durable"),
		slog.String("reader", reader),
		slog.String("head_identity_hash", headIdentityHash(id)))
}

// loadIndexView loads and verifies the session's index. Absence is an empty
// view, not an error.
func (b *bus) loadIndexView(ctx context.Context, id identity.Quadruple) (indexView, error) {
	rec, err := b.store.Load(ctx, id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return indexView{id: id, hdr: newIndexHeader()}, nil
	}
	if err != nil {
		return indexView{}, err
	}
	return b.indexViewFromRecord(ctx, id, rec.Bytes)
}

// indexViewFromRecord builds a view from head bytes already in hand (the
// admin and projection paths read them through the maintenance scan).
func (b *bus) indexViewFromRecord(ctx context.Context, id identity.Quadruple, raw []byte) (indexView, error) {
	if isIndexV2(raw) {
		hdr, err := decodeHeader(raw)
		if err != nil {
			return indexView{}, b.noteIntegrity(ctx, id, err)
		}
		return indexView{id: id, hdr: hdr, exists: true}, nil
	}
	head, err := decodeHead(raw)
	if err != nil {
		return indexView{}, err
	}
	head, err = b.ensureHeadMetadata(ctx, id, head)
	if err != nil {
		return indexView{}, err
	}
	rows := make([]indexRow, len(head.Metadata))
	for i, m := range head.Metadata {
		rows[i] = rowFromMetadataRecord(m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	hdr := newIndexHeader()
	hdr.Tail = rows
	hdr.RetainedCount = uint64(len(rows))
	if len(rows) > 0 {
		hdr.FirstSeq = rows[0].Seq
		hdr.LastSeq = rows[len(rows)-1].Seq
	}
	return indexView{id: id, hdr: hdr, legacy: true, exists: true}, nil
}

// missWasTrimmed reloads the header after a mid-read miss and reports whether
// the missing chunk/entry is now inside the trimmed range.
// It also returns the reloaded header's trimmed through_seq: every sequence
// at or below it is gone, every one above it is still retained.
func (b *bus) missWasTrimmed(ctx context.Context, miss *indexMissError) (bool, uint64, error) {
	rec, err := b.store.Load(ctx, miss.id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		// Erasure removed the whole session; the caller's post-read fence
		// check decides what the read returns.
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !isIndexV2(rec.Bytes) {
		return false, 0, nil
	}
	hdr, err := decodeHeader(rec.Bytes)
	if err != nil {
		return false, 0, b.noteIntegrity(ctx, miss.id, err)
	}
	through := hdr.Trimmed.ThroughSeq
	if miss.entry {
		return hdr.Trimmed.Count > 0 && miss.seq <= through, through, nil
	}
	return miss.chunk < hdr.FirstChunk, through, nil
}

// classifyMissErr returns nil when err is a chunk/entry miss that a
// concurrent audit-retention trim explains (the reader then degrades
// honestly), and otherwise the loud error: a genuine index gap or any other
// failure.
func (b *bus) classifyMissErr(ctx context.Context, err error) error {
	_, classifyErr := b.classifyMissThrough(ctx, err)
	return classifyErr
}

// classifyMissThrough is classifyMissErr that also returns the session's
// current trimmed through_seq, so a reader can drop exactly the trimmed
// candidates and keep every retained one.
func (b *bus) classifyMissThrough(ctx context.Context, err error) (uint64, error) {
	var miss *indexMissError
	if !errors.As(err, &miss) {
		return 0, err
	}
	trimmed, through, classifyErr := b.missWasTrimmed(ctx, miss)
	if classifyErr != nil {
		return 0, fmt.Errorf("%w (classify: %w)", err, classifyErr)
	}
	if trimmed {
		return through, nil
	}
	return 0, fmt.Errorf("durable: index gap: %w", err)
}

// loadEntryBody loads and decodes one entry body. A missing body is an
// *indexMissError so readers can classify a concurrent trim.
func (b *bus) loadEntryBody(ctx context.Context, id identity.Quadruple, seq uint64) (events.Event, error) {
	if hook := b.hookBeforeBodyLoad; hook != nil {
		hook(id, seq)
	}
	rec, err := b.store.Load(ctx, id, kindEntryPrefix+seqToken(seq))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return events.Event{}, &indexMissError{id: id, entry: true, seq: seq, wrapped: err}
		}
		return events.Event{}, fmt.Errorf("load entry seq=%d: %w", seq, err)
	}
	ev, err := decodeEvent(rec.Bytes)
	if err != nil {
		return events.Event{}, fmt.Errorf("decode entry seq=%d: %w", seq, err)
	}
	return ev, nil
}

// rowIterator yields a view's rows newest-first, verifying each chunk's
// digest, chain link, and derived fields as it is reached.
type rowIterator struct {
	b    *bus
	ctx  context.Context
	view *indexView

	seg       []indexRow
	pos       int
	nextChunk uint64
	expected  string
	// belowSeq is the first sequence of the segment just exhausted; the next
	// older chunk must end strictly below it. Zero means "tail was empty":
	// the newest chunk must then end exactly at the header's last_seq.
	belowSeq uint64
	done     bool
}

func (b *bus) newRowIterator(ctx context.Context, view *indexView) *rowIterator {
	it := &rowIterator{
		b:         b,
		ctx:       ctx,
		view:      view,
		seg:       view.hdr.Tail,
		pos:       len(view.hdr.Tail),
		nextChunk: view.hdr.NextChunk,
		expected:  view.hdr.SealedDigest,
	}
	if len(view.hdr.Tail) > 0 {
		it.belowSeq = view.hdr.Tail[0].Seq
	}
	return it
}

// next returns the next older row, or ok=false at the bottom of the retained
// index.
func (it *rowIterator) next() (indexRow, bool, error) {
	for {
		if it.pos > 0 {
			it.pos--
			return it.seg[it.pos], true, nil
		}
		if it.done || it.view.legacy || it.nextChunk <= it.view.hdr.FirstChunk {
			it.done = true
			return indexRow{}, false, nil
		}
		if err := it.ctx.Err(); err != nil {
			return indexRow{}, false, err
		}
		c, err := it.loadChunk(it.nextChunk - 1)
		if err != nil {
			return indexRow{}, false, err
		}
		it.nextChunk--
		it.seg = c.Rows
		it.pos = len(c.Rows)
	}
}

func (it *rowIterator) loadChunk(k uint64) (indexChunk, error) {
	b, id, hdr := it.b, it.view.id, it.view.hdr
	rec, err := b.store.Load(it.ctx, id, chunkKind(k))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return indexChunk{}, &indexMissError{id: id, chunk: k, wrapped: err}
		}
		return indexChunk{}, fmt.Errorf("load index chunk %d: %w", k, err)
	}
	c, err := decodeChunk(rec.Bytes, k)
	if err != nil {
		return indexChunk{}, b.noteIntegrity(it.ctx, id, err)
	}
	fail := func(format string, args ...any) (indexChunk, error) {
		return indexChunk{}, b.noteIntegrity(it.ctx, id, integrityErrorf(format, args...))
	}
	if c.Digest != it.expected {
		return fail("chunk %d is not linked from its successor", k)
	}
	if it.belowSeq == 0 {
		if c.LastSeq != hdr.LastSeq {
			return fail("newest chunk %d ends at seq=%d, header last_seq=%d", k, c.LastSeq, hdr.LastSeq)
		}
	} else if c.LastSeq >= it.belowSeq {
		return fail("chunk %d ends at seq=%d, not below successor seq=%d", k, c.LastSeq, it.belowSeq)
	}
	if k == hdr.FirstChunk {
		if c.Prev != hdr.BaseDigest {
			return fail("oldest chunk %d is not anchored to the header base digest", k)
		}
		if c.FirstSeq != hdr.FirstSeq {
			return fail("oldest chunk %d starts at seq=%d, header first_seq=%d", k, c.FirstSeq, hdr.FirstSeq)
		}
		if hdr.OldestOccurredAt > c.MinOccurredAt {
			return fail("header oldest_occurred_at is above oldest chunk %d", k)
		}
	}
	it.expected = c.Prev
	it.belowSeq = c.FirstSeq
	if hook := b.hookAfterChunkLoad; hook != nil {
		hook(id, k)
	}
	return c, nil
}

// collectBelow walks newest-first and returns up to max rows (max<=0 means
// unbounded) with Seq < before (before==0 means no bound) that satisfy match,
// newest-first. exhausted reports that the walk reached the bottom of the
// retained index.
func (b *bus) collectBelow(ctx context.Context, view *indexView, before uint64, max int, match func(indexRow) bool) (rows []indexRow, exhausted bool, err error) {
	it := b.newRowIterator(ctx, view)
	for {
		r, ok, err := it.next()
		if err != nil {
			return rows, false, err
		}
		if !ok {
			return rows, true, nil
		}
		if before != 0 && r.Seq >= before {
			continue
		}
		if match != nil && !match(r) {
			continue
		}
		rows = append(rows, r)
		if max > 0 && len(rows) >= max {
			return rows, false, nil
		}
	}
}

// collectAbove returns rows with Seq > after, oldest-first. When max > 0 only
// the max smallest such rows are kept (memory stays bounded while the walk
// descends to the cursor). On a walk error it still returns the rows
// collected so far with the error: after a chunk miss that a trim explains,
// those are exactly the retained rows above the cut.
func (b *bus) collectAbove(ctx context.Context, view *indexView, after uint64, max int, match func(indexRow) bool) ([]indexRow, error) {
	if view.hdr.RetainedCount == 0 || view.hdr.LastSeq <= after {
		return nil, nil
	}
	it := b.newRowIterator(ctx, view)
	var desc []indexRow
	var walkErr error
	for {
		r, ok, err := it.next()
		if err != nil {
			walkErr = err
			break
		}
		if !ok || r.Seq <= after {
			break
		}
		if match != nil && !match(r) {
			continue
		}
		desc = append(desc, r)
		if max > 0 && len(desc) >= 2*max+indexChunkRows {
			desc = append([]indexRow(nil), desc[len(desc)-max:]...)
		}
	}
	if max > 0 && len(desc) > max {
		desc = desc[len(desc)-max:]
	}
	out := make([]indexRow, len(desc))
	for i := range desc {
		out[len(desc)-1-i] = desc[i]
	}
	return out, walkErr
}
