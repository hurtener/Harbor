// chunkspans.go — the projection read path's verified chunk-span cache.
//
// A forward projection page needs, per session, the few rows just above
// the reader's cursor. The chained v2 index authenticates chunks from the
// newest one down (each chunk's digest is named by its successor), so a
// plain read walks — and loads — every chunk between the tail and the
// cursor on EVERY page: a reader far behind the head pays O(history) chunk
// loads per page and O(history²) over a catch-up.
//
// The cache keeps, per session, the (first seq, last seq, digest) span of
// chunks already authenticated by such a walk. Chunks are immutable, so a
// span stays valid as long as the chain it was verified on is still the
// session's chain: every read re-anchors at the current header (walking
// only chunks sealed since, down to the first cached chunk whose digest the
// chain names) and then loads FORWARD from the chunk holding the cursor,
// authenticating each loaded chunk by its digest (decodeChunk recomputes it
// from the content) against the verified span. Cost per page becomes
// proportional to the rows the page returns. Any surprise (a trimmed or
// missing chunk, a digest that no longer matches, a rebuilt chain) drops
// the session's spans and falls back to the full verifying walk, which
// keeps its established failure semantics.
//
// Memory is bounded: each span is ~100 bytes and the whole cache is reset
// when it exceeds maxCachedChunkSpans entries (~3 MiB).
package durable

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
)

// maxCachedChunkSpans bounds the cache (~100 B per span; 256 events per
// chunk, so the default covers ~8M indexed events).
const maxCachedChunkSpans = 32_768

type chunkSpan struct {
	index    uint64
	firstSeq uint64
	lastSeq  uint64
	digest   string
	prev     string
}

// sessionSpans are one session's verified spans, ascending by chunk index
// and contiguous up to top (the newest cached chunk).
type sessionSpans struct {
	spans []chunkSpan
}

type chunkSpanCache struct {
	mu      sync.Mutex
	m       map[identity.Quadruple]*sessionSpans
	entries int
}

func (c *chunkSpanCache) get(id identity.Quadruple) []chunkSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.m[id]; s != nil {
		return append([]chunkSpan(nil), s.spans...)
	}
	return nil
}

func (c *chunkSpanCache) put(id identity.Quadruple, spans []chunkSpan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[identity.Quadruple]*sessionSpans{}
	}
	if old := c.m[id]; old != nil {
		c.entries -= len(old.spans)
	}
	if c.entries+len(spans) > maxCachedChunkSpans {
		c.m = map[identity.Quadruple]*sessionSpans{}
		c.entries = 0
	}
	c.m[id] = &sessionSpans{spans: spans}
	c.entries += len(spans)
}

func (c *chunkSpanCache) drop(id identity.Quadruple) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.m[id]; old != nil {
		c.entries -= len(old.spans)
		delete(c.m, id)
	}
}

// pageStream is one session's lazy, ascending stream of rows above a
// cursor for the projection page merge.
type pageStream struct {
	view  indexView
	after uint64
	match func(indexRow) bool
	// rows are loaded rows (ascending, > after, matching), not yet emitted.
	rows []indexRow
	// spans are verified, not yet loaded chunks (ascending).
	spans []chunkSpan
	// tail reports the header tail is still to be appended.
	tail bool
	// last is the last emitted sequence (for the eager fallback).
	last uint64
	key  uint64 // heap key: the lower bound of the next row
}

func nonInternalRow(r indexRow) bool { return !r.Internal }

// newPageStream builds the session's lazy stream. ok=false means the
// cheap path cannot be used and the caller reads the session eagerly.
func (b *bus) newPageStream(ctx context.Context, view indexView, after uint64) (*pageStream, bool) {
	return b.newRowStream(ctx, view, after, nonInternalRow)
}

func (b *bus) newRowStream(ctx context.Context, view indexView, after uint64, match func(indexRow) bool) (*pageStream, bool) {
	hdr := view.hdr
	st := &pageStream{view: view, after: after, match: match, tail: true, last: after}
	if view.legacy || hdr.NextChunk <= hdr.FirstChunk || (len(hdr.Tail) > 0 && hdr.Tail[0].Seq <= after+1) {
		// Every row above the cursor sits in the (already loaded) tail.
		return st, true
	}
	spans, ok := b.verifiedSpans(ctx, &st.view, after)
	if !ok {
		b.chunkSpans.drop(view.id)
		return nil, false
	}
	for _, sp := range spans {
		if sp.lastSeq > after {
			st.spans = append(st.spans, sp)
		}
	}
	return st, true
}

func (st *pageStream) push(rows []indexRow) {
	for _, r := range rows {
		if r.Seq > st.last && (st.match == nil || st.match(r)) {
			st.rows = append(st.rows, r)
		}
	}
}

// bound reports the lower bound of the stream's next row, appending the
// free tail when only it remains; ok=false when exhausted.
func (st *pageStream) bound() (uint64, bool) {
	for {
		if len(st.rows) > 0 {
			return st.rows[0].Seq, true
		}
		if len(st.spans) > 0 {
			return max(st.spans[0].firstSeq, st.after+1), true
		}
		if !st.tail {
			return 0, false
		}
		st.tail = false
		st.push(st.view.hdr.Tail)
	}
}

// fill loads the stream's next verified chunk, authenticating its content
// by digest against the verified span.
func (b *bus) fill(ctx context.Context, st *pageStream) error {
	sp := st.spans[0]
	rec, err := b.store.Load(ctx, st.view.id, chunkKind(sp.index))
	if err != nil {
		return err
	}
	c, err := decodeChunk(rec.Bytes, sp.index)
	if err != nil {
		return err
	}
	if c.Digest != sp.digest {
		return integrityErrorf("chunk %d digest differs from its verified span", sp.index)
	}
	if hook := b.hookAfterChunkLoad; hook != nil {
		hook(st.view.id, sp.index)
	}
	st.spans = st.spans[1:]
	st.push(c.Rows)
	return nil
}

// eagerSessionRows is the verifying descending read of one session with
// the projection page's established trim / integrity semantics: a trim
// that explains a miss keeps every retained row above the cut (gap); an
// integrity failure skips the session (corrupt); anything else is fatal.
func (b *bus) eagerSessionRows(ctx context.Context, view *indexView, after uint64, max int) (rows []indexRow, gap, corrupt bool, err error) {
	rows, err = b.collectAbove(ctx, view, after, max, nonInternalRow)
	if err == nil {
		return rows, false, false, nil
	}
	through, classifyErr := b.classifyMissThrough(ctx, err)
	if errors.Is(classifyErr, ErrIndexIntegrity) {
		b.skipCorruptSession(ctx, view.id, "projection page")
		return nil, true, true, nil
	}
	if classifyErr != nil {
		return nil, false, false, fmt.Errorf("durable: projection page: %w", classifyErr)
	}
	// A trim deleted a chunk below the rows already collected. Those rows
	// are every retained row above the cut, so they stay candidates
	// (dropping them would let a Current page's watermark skip retained
	// events); only the trimmed range is gone.
	kept := rows[:0]
	for _, r := range rows {
		if r.Seq > through {
			kept = append(kept, r)
		}
	}
	return kept, true, false, nil
}

type mergedRow struct {
	id  identity.Quadruple
	row indexRow
}

type streamHeap []*pageStream

func (h streamHeap) Len() int           { return len(h) }
func (h streamHeap) Less(i, j int) bool { return h[i].key < h[j].key }
func (h streamHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *streamHeap) Push(x any)        { *h = append(*h, x.(*pageStream)) }
func (h *streamHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return x
}

// mergePageStreams k-way merges the session streams into the max smallest
// rows above the cursor, loading a chunk only when the merge reaches it. A
// lazy load that fails falls back to that session's verifying read (whose
// trim / integrity semantics decide); a session found corrupt contributes
// nothing to the page.
func (b *bus) mergePageStreams(ctx context.Context, streams []*pageStream, after uint64, max int) (out []mergedRow, gap, corrupt bool, err error) {
	h := make(streamHeap, 0, len(streams))
	for _, st := range streams {
		if k, ok := st.bound(); ok {
			st.key = k
			h = append(h, st)
		}
	}
	heap.Init(&h)
	for h.Len() > 0 && len(out) < max {
		if err := ctx.Err(); err != nil {
			return nil, false, false, err
		}
		st := heap.Pop(&h).(*pageStream)
		if len(st.rows) == 0 {
			if ferr := b.fill(ctx, st); ferr != nil {
				b.chunkSpans.drop(st.view.id)
				rows, g, c, eerr := b.eagerSessionRows(ctx, &st.view, after, max)
				if eerr != nil {
					return nil, false, false, eerr
				}
				gap = gap || g
				if c {
					corrupt = true
					kept := out[:0]
					for _, r := range out {
						if r.id != st.view.id {
							kept = append(kept, r)
						}
					}
					out = kept
					continue
				}
				st.rows, st.spans, st.tail = nil, nil, false
				st.push(rows)
			}
		} else {
			r := st.rows[0]
			st.rows = st.rows[1:]
			st.last = r.Seq
			out = append(out, mergedRow{id: st.view.id, row: r})
		}
		if k, ok := st.bound(); ok {
			st.key = k
			heap.Push(&h, st)
		}
	}
	return out, gap, corrupt, nil
}

// collectAboveSeek is collectAbove (rows with Seq > after, oldest-first,
// at most max, matching) served through the lazy verified-span stream.
func (b *bus) collectAboveSeek(ctx context.Context, view *indexView, after uint64, max int, match func(indexRow) bool) ([]indexRow, error) {
	if max <= 0 || view.hdr.RetainedCount == 0 || view.hdr.LastSeq <= after {
		return b.collectAbove(ctx, view, after, max, match)
	}
	st, ok := b.newRowStream(ctx, *view, after, match)
	if !ok {
		return b.collectAbove(ctx, view, after, max, match)
	}
	var out []indexRow
	for len(out) < max {
		if _, ok := st.bound(); !ok {
			break
		}
		if len(st.rows) == 0 {
			if err := b.fill(ctx, st); err != nil {
				b.chunkSpans.drop(view.id)
				return b.collectAbove(ctx, view, after, max, match)
			}
			continue
		}
		st.last = st.rows[0].Seq
		out = append(out, st.rows[0])
		st.rows = st.rows[1:]
	}
	return out, nil
}

// verifiedSpans returns the session's authenticated spans from the chunk
// holding the first row above after up to the newest chunk, ascending.
// Only chunks the cache does not hold (or whose cached digest the current
// chain does not name) are loaded and verified; the rest are authenticated
// by following the digest chain through the cached spans. ok=false means
// the cheap path cannot be used (the caller falls back).
func (b *bus) verifiedSpans(ctx context.Context, view *indexView, after uint64) ([]chunkSpan, bool) {
	hdr := view.hdr
	byIndex := map[uint64]chunkSpan{}
	for _, sp := range b.chunkSpans.get(view.id) {
		if sp.index >= hdr.FirstChunk && sp.index < hdr.NextChunk {
			byIndex[sp.index] = sp
		}
	}
	it := b.newRowIterator(ctx, view)
	cur := hdr.SealedDigest // the digest the chain names for the next older chunk
	var walked []chunkSpan  // newest-first
	for k := hdr.NextChunk; k > hdr.FirstChunk; {
		k--
		sp, cached := byIndex[k]
		if cached && sp.digest == cur && (k != hdr.FirstChunk || sp.prev == hdr.BaseDigest) {
			// Authenticated by the chain: no load.
		} else {
			// Resume the verifying iterator exactly where the chain is.
			it.expected = cur
			if len(walked) > 0 {
				it.belowSeq = walked[len(walked)-1].firstSeq
			}
			c, err := it.loadChunk(k)
			if err != nil {
				return nil, false
			}
			sp = chunkSpan{index: k, firstSeq: c.FirstSeq, lastSeq: c.LastSeq, digest: c.Digest, prev: c.Prev}
		}
		walked = append(walked, sp)
		cur = sp.prev
		if sp.firstSeq <= after {
			break
		}
	}
	if len(walked) == 0 {
		return nil, false
	}
	// Keep the cached spans below the walk that still chain onto it.
	keep := append([]chunkSpan(nil), walked...)
	for k := walked[len(walked)-1].index; k > hdr.FirstChunk; {
		k--
		sp, ok := byIndex[k]
		if !ok || sp.digest != cur {
			break
		}
		keep = append(keep, sp)
		cur = sp.prev
	}
	reverse := func(xs []chunkSpan) {
		for l, r := 0, len(xs)-1; l < r; l, r = l+1, r-1 {
			xs[l], xs[r] = xs[r], xs[l]
		}
	}
	reverse(keep)
	reverse(walked)
	b.chunkSpans.put(view.id, keep)
	return walked, true
}
