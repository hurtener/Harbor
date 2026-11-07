package durable

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

func viewOf(t *testing.T, b *bus, id identity.Quadruple) indexView {
	t.Helper()
	rec, err := b.store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load head: %v", err)
	}
	v, err := b.indexViewFromRecord(context.Background(), id, rec.Bytes)
	if err != nil {
		t.Fatalf("index view: %v", err)
	}
	return v
}

func seqsOf(rows []indexRow) []uint64 {
	out := make([]uint64, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

// TestChunkSpans_SeekEqualsVerifyingWalk: for every cursor position the
// cached forward seek returns exactly the rows the verifying descending
// walk returns, including after the chain grows (new chunks sealed).
func TestChunkSpans_SeekEqualsVerifyingWalk(t *testing.T) {
	store := newInmemTestStore(t)
	b := openIndexBus(t, store, 0)
	a, c := sid("big"), sid("small")
	for range 6 {
		publishN(t, b, a, 250, events.EventTypeRuntimeWarning, indexEpoch)
		publishN(t, b, c, 20, events.EventTypeRuntimeWarning, indexEpoch)
	}
	check := func() {
		for _, id := range []identity.Quadruple{a, c} {
			v := viewOf(t, b, id)
			for after := uint64(0); after <= v.hdr.LastSeq+1; after += 37 {
				for _, max := range []int{1, 257, 600} {
					want, werr := b.collectAbove(context.Background(), &v, after, max, nil)
					got, gerr := b.collectAboveSeek(context.Background(), &v, after, max, nil)
					if werr != nil || gerr != nil {
						t.Fatalf("errors: walk %v seek %v", werr, gerr)
					}
					if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
						t.Fatalf("%s after=%d max=%d: seek %v, walk %v", id.SessionID, after, max, seqsOf(got), seqsOf(want))
					}
				}
			}
		}
	}
	check()
	publishN(t, b, a, 700, events.EventTypeRuntimeWarning, indexEpoch) // seal more chunks
	check()
}

// TestChunkSpans_CatchUpLoadsAreProportional: a cold forward catch-up
// over a multi-chunk session loads each chunk a bounded number of times
// (the old read reloaded every chunk above the cursor on every page).
func TestChunkSpans_CatchUpLoadsAreProportional(t *testing.T) {
	store := newInmemTestStore(t)
	var loads atomic.Int64
	b := openIndexBus(t, store, 0)
	b.hookAfterChunkLoad = func(identity.Quadruple, uint64) { loads.Add(1) }
	ids := []identity.Quadruple{sid("deep-a"), sid("deep-b"), sid("deep-c")}
	for range 8 {
		for _, id := range ids {
			publishN(t, b, id, 300, events.EventTypeRuntimeWarning, indexEpoch)
		}
	}
	var chunks uint64
	var last uint64
	for _, id := range ids {
		v := viewOf(t, b, id)
		chunks += v.hdr.NextChunk
		last = max(last, v.hdr.LastSeq)
	}
	var after uint64
	pages := 0
	for {
		page, err := b.Page(context.Background(), after, 256)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		pages++
		if len(page.Events) == 0 {
			break
		}
		after = page.Next
		if page.Quality == events.ProjectionCurrent {
			break
		}
	}
	if after != last {
		t.Fatalf("catch-up ended at %d, want %d", after, last)
	}
	// One verifying walk per chunk plus, per page, at most the boundary
	// chunk of each session the merge reaches (the old read reloaded every
	// chunk above the cursor of every session on every page: ~3x this).
	if bound := int64(chunks) + int64((len(ids)+1)*pages); loads.Load() > bound {
		t.Fatalf("chunk loads = %d over %d pages / %d chunks; want <= %d (proportional, not quadratic)", loads.Load(), pages, chunks, bound)
	}
}

// TestChunkSpans_PageDropsSessionCorruptedMidMerge: a session whose cached
// chunk is corrupted after caching contributes nothing to the page and the
// page is not Current, while other sessions are served.
func TestChunkSpans_PageDropsSessionCorruptedMidMerge(t *testing.T) {
	store := newInmemTestStore(t)
	b := openIndexBus(t, store, 0)
	bad, good := sid("bad"), sid("good")
	publishN(t, b, bad, 900, events.EventTypeRuntimeWarning, indexEpoch)
	publishN(t, b, good, 50, events.EventTypeRuntimeWarning, indexEpoch)
	if _, err := b.Page(context.Background(), 0, 256); err != nil {
		t.Fatalf("warm page: %v", err)
	}
	corruptChunk(t, store, bad, 1, func(d map[string]any) {
		d["rows"].([]any)[10].(map[string]any)["type"] = string(events.EventTypeRuntimeError)
	})
	page, err := b.Page(context.Background(), 300, 256)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.Quality == events.ProjectionCurrent || !page.RetentionGap {
		t.Fatalf("page quality %v gap %v; want CatchingUp with RetentionGap", page.Quality, page.RetentionGap)
	}
	for _, ev := range page.Events {
		if ev.Identity == bad {
			t.Fatalf("corrupt session served seq %d", ev.Sequence)
		}
	}
	if len(page.Events) != 50 {
		t.Fatalf("good session events = %d, want 50", len(page.Events))
	}
}

// TestChunkSpans_CorruptionAfterCachingStillFailsLoud: a chunk corrupted
// after its span was cached is caught (the forward load authenticates the
// content by digest and the fallback walk reports the integrity failure).
func TestChunkSpans_CorruptionAfterCachingStillFailsLoud(t *testing.T) {
	store := newInmemTestStore(t)
	b := openIndexBus(t, store, 0)
	a := sid("bad")
	publishN(t, b, a, 900, events.EventTypeRuntimeWarning, indexEpoch)
	v := viewOf(t, b, a)
	if _, err := b.collectAboveSeek(context.Background(), &v, 0, 10, nil); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	corruptChunk(t, store, a, 0, func(d map[string]any) {
		d["rows"].([]any)[10].(map[string]any)["type"] = string(events.EventTypeRuntimeError)
	})
	if _, err := b.collectAboveSeek(context.Background(), &v, 0, 10, nil); !errors.Is(err, ErrIndexIntegrity) {
		t.Fatalf("seek over a corrupted cached chunk = %v; want ErrIndexIntegrity", err)
	}
}

// TestChunkSpans_CacheBoundHoldsForOneHugeSession: one session's spans
// never push the cache past its bound; the newest window is kept.
func TestChunkSpans_CacheBoundHoldsForOneHugeSession(t *testing.T) {
	var c chunkSpanCache
	huge := make([]chunkSpan, 2*maxCachedChunkSpans)
	for i := range huge {
		huge[i] = chunkSpan{index: uint64(i)}
	}
	c.put(sid("huge"), huge)
	if c.entries > maxCachedChunkSpans {
		t.Fatalf("cache holds %d spans; bound is %d", c.entries, maxCachedChunkSpans)
	}
	got := c.get(sid("huge"))
	if len(got) != maxSessionChunkSpans || got[len(got)-1].index != uint64(len(huge)-1) {
		t.Fatalf("kept %d spans ending at %d; want the newest %d", len(got), got[len(got)-1].index, maxSessionChunkSpans)
	}
	for i := range 10 {
		c.put(sid(fmt.Sprintf("s%d", i)), huge)
		if c.entries > maxCachedChunkSpans {
			t.Fatalf("cache holds %d spans after %d sessions; bound is %d", c.entries, i+1, maxCachedChunkSpans)
		}
	}
}
