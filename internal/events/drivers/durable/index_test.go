package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

var indexEpoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// readerSnapshot captures every durable reader's view of one session so a
// migration (or any index change) can be proven observably neutral.
type readerSnapshot struct {
	Lo, Hi    uint64
	Truncated bool
	Replay0   []events.Event
	ReplayMid []events.Event
	Window    []events.Event
	WindowMid []events.Event
	ListPages []events.EventListPage
	MetaPages []events.MetadataListPage
	Pages     []events.ProjectionPage
}

func filterFor(id identity.Quadruple) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

func wireFilterFor(id identity.Quadruple) prototypes.EventFilter {
	return prototypes.EventFilter{TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, SessionIDs: []string{id.SessionID}}
}

func snapshotReaders(t *testing.T, b *bus, id identity.Quadruple, mid uint64) readerSnapshot {
	t.Helper()
	ctx := context.Background()
	var s readerSnapshot
	var err error
	s.Lo, s.Hi, s.Truncated, err = b.Bounds(ctx, filterFor(id))
	mustf(t, err, "Bounds")
	s.Replay0, err = b.Replay(ctx, events.Cursor{SessionID: id.SessionID}, filterFor(id))
	mustf(t, err, "Replay(0)")
	s.ReplayMid, err = b.Replay(ctx, events.Cursor{SessionID: id.SessionID, Sequence: mid}, filterFor(id))
	mustf(t, err, "Replay(mid)")
	s.Window, err = b.Window(ctx, 0, 50, filterFor(id))
	mustf(t, err, "Window")
	s.WindowMid, err = b.Window(ctx, mid, 300, filterFor(id))
	mustf(t, err, "Window(mid)")
	var before uint64
	for {
		page, err := b.ListWindow(ctx, events.EventListQuery{Filter: wireFilterFor(id), Before: before, Limit: 777})
		mustf(t, err, "ListWindow")
		s.ListPages = append(s.ListPages, page)
		if !page.HasMore {
			break
		}
		before = page.NextCursor
	}
	before = 0
	for {
		page, err := b.ListWindowMetadata(ctx, events.EventListQuery{Filter: wireFilterFor(id), Before: before, Limit: 1500})
		mustf(t, err, "ListWindowMetadata")
		s.MetaPages = append(s.MetaPages, page)
		if !page.HasMore {
			break
		}
		before = page.NextCursor
	}
	var after uint64
	for {
		page, err := b.Page(ctx, after, 600)
		mustf(t, err, "Page")
		s.Pages = append(s.Pages, page)
		if len(page.Events) == 0 || page.Quality == events.ProjectionCurrent {
			break
		}
		after = page.Next
	}
	return s
}

func TestIndexV2_NewSessionsWriteV2AndAllReadersAgree(t *testing.T) {
	storeMatrix(t, func(t *testing.T, store state.StateStore) {
		b := openIndexBus(t, store, 0)
		id := sid("fresh")
		other := sid("other")
		var published []events.Event
		for i := 0; i < 700; i++ {
			typ := events.EventTypeRuntimeWarning
			if i%7 == 0 {
				typ = events.EventTypeAdminScopeUsed
			}
			ev := testEvent(id, typ, indexEpoch.Add(time.Duration(i)*time.Second))
			if i%5 == 0 {
				mustf(t, b.Publish(context.Background(), testEvent(other, events.EventTypeRuntimeWarning, indexEpoch)), "publish other")
			}
			mustf(t, b.Publish(context.Background(), ev), "publish %d", i)
			published = append(published, ev)
		}
		hdr, _ := loadHeaderV2(t, store, id)
		if hdr.NextChunk != 2 || len(hdr.Tail) != 700-512 || hdr.RetainedCount != 700 {
			t.Fatalf("header chunks=[%d,%d) tail=%d retained=%d, want [0,2) 188 700", hdr.FirstChunk, hdr.NextChunk, len(hdr.Tail), hdr.RetainedCount)
		}
		rows := sessionIndexRows(t, store, id)
		if len(rows) != 700 || rows[0].Seq != hdr.FirstSeq || rows[699].Seq != hdr.LastSeq {
			t.Fatalf("walked %d rows (%d..%d), header bounds %d..%d", len(rows), rows[0].Seq, rows[len(rows)-1].Seq, hdr.FirstSeq, hdr.LastSeq)
		}
		replayed, err := b.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
		mustf(t, err, "Replay")
		if len(replayed) != 700 {
			t.Fatalf("Replay returned %d events, want 700", len(replayed))
		}
		for i, ev := range replayed {
			if ev.Sequence != rows[i].Seq || ev.Type != published[i].Type {
				t.Fatalf("Replay[%d] = seq %d type %s, want seq %d type %s", i, ev.Sequence, ev.Type, rows[i].Seq, published[i].Type)
			}
		}
		lo, hi, truncated, err := b.Bounds(context.Background(), filterFor(id))
		mustf(t, err, "Bounds")
		if lo != hdr.FirstSeq || hi != hdr.LastSeq || truncated {
			t.Fatalf("Bounds = %d %d %v", lo, hi, truncated)
		}
		// Window/list reads exclude bus-internal rows (audit.admin_scope_used).
		var visible []indexRow
		for _, r := range rows {
			if !r.Internal {
				visible = append(visible, r)
			}
		}
		window, err := b.Window(context.Background(), visible[300].Seq, 40, filterFor(id))
		mustf(t, err, "Window")
		if len(window) != 40 || window[39].Sequence != visible[299].Seq || window[0].Sequence != visible[260].Seq {
			t.Fatalf("Window crossing a chunk boundary returned %d events", len(window))
		}
		meta, err := b.ListWindowMetadata(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Limit: 1000})
		mustf(t, err, "ListWindowMetadata")
		if len(meta.Events) != len(visible) || meta.HasMore || meta.Truncated {
			t.Fatalf("metadata page has %d rows more=%v truncated=%v", len(meta.Events), meta.HasMore, meta.Truncated)
		}
		admin, err := b.ListWindow(context.Background(), events.EventListQuery{Admin: true, Limit: 900})
		mustf(t, err, "admin ListWindow")
		if len(admin.Events) != len(visible)+140 || admin.HasMore {
			t.Fatalf("admin page has %d events more=%v, want %d false", len(admin.Events), admin.HasMore, len(visible)+140)
		}
		for i := 1; i < len(admin.Events); i++ {
			if admin.Events[i-1].Sequence >= admin.Events[i].Sequence {
				t.Fatalf("admin page not ascending at %d", i)
			}
		}
		page, err := b.Page(context.Background(), rows[600].Seq, 50)
		mustf(t, err, "Page")
		if len(page.Events) == 0 || page.Events[0].Sequence <= rows[600].Seq || page.RetentionGap {
			t.Fatalf("projection page after %d: %d events, gap=%v", rows[600].Seq, len(page.Events), page.RetentionGap)
		}
	})
}

func TestIndexV2_AC10_LegacyDecoderRejectsV2Header(t *testing.T) {
	hdr := newIndexHeader()
	if _, err := hdr.appendRows([]indexRow{{Seq: 1, Type: events.EventTypeRuntimeWarning, OccurredAt: 1}}); err != nil {
		t.Fatal(err)
	}
	raw, err := encodeHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeHead(raw); err == nil {
		t.Fatal("v1 decodeHead accepted a v2 header; older binaries would not fail closed")
	}
	var legacy headRecord
	if err := json.Unmarshal(raw, &legacy); err == nil {
		t.Fatal("a plain JSON decoder accepted the magic-prefixed header")
	}
}

// corruptChunk rewrites chunk k of id through mutate, leaving the stored
// digest as it was (a byte-level corruption that a reader must detect).
func corruptChunk(t *testing.T, store state.StateStore, id identity.Quadruple, k uint64, mutate func(map[string]any)) {
	t.Helper()
	rec, err := store.Load(context.Background(), id, chunkKind(k))
	mustf(t, err, "load chunk")
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.Bytes[len(indexMagic):]))
	dec.UseNumber() // exact int64 nanoseconds; float64 would round them
	mustf(t, dec.Decode(&doc), "decode chunk")
	mutate(doc)
	body, err := json.Marshal(doc)
	mustf(t, err, "encode chunk")
	rec.ID = state.NewEventID()
	rec.Bytes = append([]byte(indexMagic), body...)
	mustf(t, store.Save(context.Background(), rec), "save corrupt chunk")
}

func corruptHeader(t *testing.T, store state.StateStore, id identity.Quadruple, mutate func(*indexHeader), restamp bool) {
	t.Helper()
	hdr, rec := loadHeaderV2(t, store, id)
	mutate(&hdr)
	var body []byte
	var err error
	if restamp {
		digest, derr := computeHeaderDigest(hdr)
		mustf(t, derr, "digest")
		hdr.HeaderDigest = digest
	}
	body, err = json.Marshal(hdr)
	mustf(t, err, "encode header")
	rec.ID = state.NewEventID()
	rec.Bytes = append([]byte(indexMagic), body...)
	mustf(t, store.Save(context.Background(), rec), "save corrupt header")
}

func publishN(t *testing.T, b *bus, id identity.Quadruple, n int, typ events.EventType, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		mustf(t, b.Publish(context.Background(), testEvent(id, typ, at.Add(time.Duration(i)*time.Millisecond))), "publish %d", i)
	}
}

func TestIndexV2_AC4_ChunkControlFieldCorruptionFailsLoudly(t *testing.T) {
	cases := map[string]func(map[string]any){
		"rows": func(d map[string]any) {
			d["rows"].([]any)[10].(map[string]any)["type"] = string(events.EventTypeRuntimeError)
		},
		"row_seq": func(d map[string]any) { r := d["rows"].([]any)[10].(map[string]any); r["seq"] = addNumber(r["seq"], 1) },
		"row_occurred_at": func(d map[string]any) {
			r := d["rows"].([]any)[10].(map[string]any)
			r["occurred_at"] = addNumber(r["occurred_at"], 1)
		},
		"first_seq":       func(d map[string]any) { d["first_seq"] = addNumber(d["first_seq"], 1) },
		"last_seq":        func(d map[string]any) { d["last_seq"] = addNumber(d["last_seq"], -1) },
		"min_occurred_at": func(d map[string]any) { d["min_occurred_at"] = addNumber(d["min_occurred_at"], -1) },
		"max_occurred_at": func(d map[string]any) { d["max_occurred_at"] = addNumber(d["max_occurred_at"], 1) },
		"index":           func(d map[string]any) { d["index"] = addNumber(d["index"], 1) },
		"prev":            func(d map[string]any) { d["prev"] = strings.Repeat("a", 64) },
		"digest":          func(d map[string]any) { d["digest"] = strings.Repeat("b", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store := newInmemTestStore(t)
			b := openIndexBus(t, store, 0)
			bad, good := sid("bad"), sid("good")
			publishN(t, b, bad, 600, events.EventTypeRuntimeWarning, indexEpoch)
			publishN(t, b, good, 300, events.EventTypeRuntimeWarning, indexEpoch)
			corruptChunk(t, store, bad, 0, mutate)
			before := b.indexIntegrityFailures.Load()
			if _, err := b.Replay(context.Background(), events.Cursor{SessionID: bad.SessionID}, filterFor(bad)); !errors.Is(err, ErrIndexIntegrity) {
				t.Fatalf("Replay over corrupt chunk = %v, want ErrIndexIntegrity", err)
			}
			if _, err := b.Window(context.Background(), 0, 1000, filterFor(bad)); !errors.Is(err, ErrIndexIntegrity) {
				t.Fatalf("Window over corrupt chunk = %v, want ErrIndexIntegrity", err)
			}
			if _, err := b.ListWindowMetadata(context.Background(), events.EventListQuery{Filter: wireFilterFor(bad), Limit: 1000}); !errors.Is(err, ErrIndexIntegrity) {
				t.Fatalf("ListWindowMetadata over corrupt chunk = %v, want ErrIndexIntegrity", err)
			}
			if b.indexIntegrityFailures.Load() <= before {
				t.Fatal("integrity failure counter did not move")
			}
			// Other sessions are unaffected.
			got, err := b.Replay(context.Background(), events.Cursor{SessionID: good.SessionID}, filterFor(good))
			if err != nil || len(got) != 300 {
				t.Fatalf("unaffected session Replay = %d, %v", len(got), err)
			}
			mustf(t, b.Publish(context.Background(), testEvent(good, events.EventTypeRuntimeWarning, indexEpoch)), "publish to unaffected session")
		})
	}
}

func TestIndexV2_AC4_HeaderCorruptionRefusesPublishForThatSessionOnly(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*indexHeader)
		restamp bool
	}{
		"last_seq_unstamped":       {func(h *indexHeader) { h.LastSeq++ }, false},
		"last_seq_restamped":       {func(h *indexHeader) { h.LastSeq++ }, true},
		"retained_count_restamped": {func(h *indexHeader) { h.RetainedCount-- }, true},
		"first_chunk_restamped":    {func(h *indexHeader) { h.FirstChunk++ }, true},
		"tail_row_unstamped":       {func(h *indexHeader) { h.Tail[3].Type = events.EventTypeRuntimeError }, false},
		"tail_order_restamped":     {func(h *indexHeader) { h.Tail[3].Seq = h.Tail[2].Seq }, true},
		"sealed_digest_restamped":  {func(h *indexHeader) { h.SealedDigest = strings.Repeat("c", 64) }, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newInmemTestStore(t)
			b := openIndexBus(t, store, 0)
			bad, good := sid("bad"), sid("good")
			publishN(t, b, bad, 300, events.EventTypeRuntimeWarning, indexEpoch)
			publishN(t, b, good, 10, events.EventTypeRuntimeWarning, indexEpoch)
			corruptHeader(t, store, bad, tc.mutate, tc.restamp)
			err := b.Publish(context.Background(), testEvent(bad, events.EventTypeRuntimeWarning, indexEpoch))
			if !errors.Is(err, ErrIndexIntegrity) {
				// A sealed-digest corruption with a valid header stamp is only
				// detectable on the chain walk; the publish itself appends to
				// the tail.
				if name != "sealed_digest_restamped" {
					t.Fatalf("publish to corrupt header = %v, want ErrIndexIntegrity", err)
				}
			}
			if _, err := b.Window(context.Background(), 0, 1000, filterFor(bad)); !errors.Is(err, ErrIndexIntegrity) {
				t.Fatalf("read of corrupt header = %v, want ErrIndexIntegrity", err)
			}
			mustf(t, b.Publish(context.Background(), testEvent(good, events.EventTypeRuntimeWarning, indexEpoch)), "publish to unaffected session")
			got, err := b.Window(context.Background(), 0, 100, filterFor(good))
			if err != nil || len(got) != 11 {
				t.Fatalf("unaffected session Window = %d, %v", len(got), err)
			}
		})
	}
}

func TestIndexV2_InlineMigrationOfSmallLegacyHead(t *testing.T) {
	store := newInmemTestStore(t)
	legacy := openIndexBus(t, store, 0, withLegacyIndexWrites())
	id := sid("small-legacy")
	publishN(t, legacy, id, 600, events.EventTypeRuntimeWarning, indexEpoch)
	if isV2Head(t, store, id) {
		t.Fatal("legacy writer produced a v2 head")
	}
	b := openIndexBus(t, store, 0)
	before := snapshotReaders(t, b, id, 300)
	mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch.Add(time.Hour))), "publish")
	if !isV2Head(t, store, id) {
		t.Fatal("a legacy head of <= 1024 rows was not migrated inline by publish")
	}
	hdr, _ := loadHeaderV2(t, store, id)
	if hdr.NextChunk != 2 || hdr.RetainedCount != 601 {
		t.Fatalf("inline migration header chunks=%d retained=%d", hdr.NextChunk, hdr.RetainedCount)
	}
	after := snapshotReaders(t, b, id, 300)
	// The new event extends the tail; compare the shared prefix of each read.
	if !reflect.DeepEqual(before.Replay0, after.Replay0[:600]) || !reflect.DeepEqual(before.ReplayMid, after.ReplayMid[:len(before.ReplayMid)]) {
		t.Fatal("Replay differs across inline migration")
	}
	if !reflect.DeepEqual(before.WindowMid, after.WindowMid) {
		t.Fatal("Window below the new event differs across inline migration")
	}
}

func TestIndexV2_LargeLegacyHeadKeepsAppendingLegacyAndNudges(t *testing.T) {
	store := newInmemTestStore(t)
	id := sid("large-legacy")
	seedLegacySession(t, store, seedSpec{id: id, n: inlineMigrationMaxRows + 1, at: indexEpoch, bodies: true})
	b := openIndexBus(t, store, 0)
	mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch.Add(time.Hour))), "publish")
	if isV2Head(t, store, id) {
		t.Fatal("publish migrated a large legacy head on the publish path")
	}
	select {
	case <-b.maint.nudge:
	default:
		t.Fatal("publish to a large legacy head did not nudge the migrator")
	}
	if seqs := sessionIndexSeqs(t, store, id); len(seqs) != inlineMigrationMaxRows+2 {
		t.Fatalf("legacy append lost rows: %d", len(seqs))
	}
}

func TestIndexV2_AC9_ErasureRemovesHeaderChunksEntriesAndSummary(t *testing.T) {
	storeMatrix(t, func(t *testing.T, store state.StateStore) {
		clock := newSettableClock(indexEpoch.Add(200 * 24 * time.Hour))
		b := openIndexBus(t, store, 90*24*time.Hour, WithClock(clock))
		id := sid("erase-me")
		publishN(t, b, id, 300, events.EventTypeAdminScopeUsed, indexEpoch)
		publishN(t, b, id, 300, events.EventTypeRuntimeWarning, indexEpoch.Add(199*24*time.Hour))
		if _, err := b.runRetentionPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), id, kindRetentionSummary); err != nil {
			t.Fatalf("summary not written before erasure: %v", err)
		}
		mustf(t, b.Fence(context.Background(), id.Identity), "Fence")
		if _, err := store.DeleteScope(context.Background(), id.Identity); err != nil {
			t.Fatal(err)
		}
		left, err := store.ListKindForIdentity(context.Background(), id, "events.durable.")
		mustf(t, err, "list")
		if len(left) != 0 {
			kinds := make([]string, 0, len(left))
			for _, r := range left {
				kinds = append(kinds, r.Kind)
			}
			t.Fatalf("erasure left %d records: %v", len(left), kinds)
		}
	})
}

// TestIndexV2_AC8_TwoBusesPostgresConcurrentPublishers proves the global
// sequence stays gap-free and every session index equals its entries with
// two bus instances × 50 publishers on one real Postgres store.
func TestIndexV2_AC8_TwoBusesPostgresConcurrentPublishers(t *testing.T) {
	store := newPostgresStore(t)
	left := openIndexBus(t, store, 0)
	right := openIndexBus(t, store, 0)
	const publishersPerBus, perPublisher, sessions = 50, 6, 7
	var wg sync.WaitGroup
	errCh := make(chan error, 2*publishersPerBus*perPublisher)
	start := make(chan struct{})
	for busIdx, bb := range []*bus{left, right} {
		for p := 0; p < publishersPerBus; p++ {
			wg.Add(1)
			go func(bb *bus, p int) {
				defer wg.Done()
				<-start
				for i := 0; i < perPublisher; i++ {
					id := sid(fmt.Sprintf("s%d", (p+i+busIdx)%sessions))
					errCh <- bb.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch.Add(time.Duration(i)*time.Second)))
				}
			}(bb, p)
		}
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	total := 2 * publishersPerBus * perPublisher
	var all []uint64
	for s := 0; s < sessions; s++ {
		id := sid(fmt.Sprintf("s%d", s))
		seqs := sessionIndexSeqs(t, store, id)
		entries, err := store.ListKindForIdentity(context.Background(), id, kindEntryPrefix)
		mustf(t, err, "list entries")
		if len(entries) != len(seqs) {
			t.Fatalf("session %d: index has %d rows, store has %d entries", s, len(seqs), len(entries))
		}
		for _, seq := range seqs {
			if _, err := store.Load(context.Background(), id, kindEntryPrefix+seqToken(seq)); err != nil {
				t.Fatalf("session %d: indexed seq %d has no entry: %v", s, seq, err)
			}
		}
		all = append(all, seqs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	if len(all) != total {
		t.Fatalf("indexed %d events, published %d", len(all), total)
	}
	for i, seq := range all {
		if seq != uint64(i+1) {
			t.Fatalf("global sequence gap/duplicate at %d: %d", i, seq)
		}
	}
}

// addNumber adds delta to an exact JSON number decoded with UseNumber.
func addNumber(v any, delta int64) json.Number {
	n, err := v.(json.Number).Int64()
	if err != nil {
		panic(err)
	}
	return json.Number(strconv.FormatInt(n+delta, 10))
}

func TestIndexV2_LegacyRepairReportsV2HeadsAndNeverMutatesThem(t *testing.T) {
	store := newInmemTestStore(t)
	b := openIndexBus(t, store, 0)
	id := sid("v2-head")
	publishN(t, b, id, 300, events.EventTypeRuntimeWarning, indexEpoch)
	before, err := store.Load(context.Background(), id, kindHead)
	mustf(t, err, "load")
	report, err := InspectLegacyHeads(context.Background(), store)
	mustf(t, err, "inspect")
	if report.AffectedHeadCount != 0 || len(report.Heads) != 1 || report.Heads[0].Outcome != legacyRepairOutcomeNotLegacyV2 {
		t.Fatalf("inspect report %+v", report)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); err != nil {
		t.Fatalf("apply over a v2 head: %v", err)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify}); err != nil {
		t.Fatalf("verify over a v2 head: %v", err)
	}
	after, err := store.Load(context.Background(), id, kindHead)
	mustf(t, err, "reload")
	if after.ID != before.ID || !bytes.Equal(after.Bytes, before.Bytes) {
		t.Fatal("legacy repair mutated a v2 header")
	}
}
