package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

const testAuditMaxAge = 90 * 24 * time.Hour

var retentionNow = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

func oldAt() time.Time    { return retentionNow.Add(-100 * 24 * time.Hour) }
func recentAt() time.Time { return retentionNow.Add(-time.Hour) }

func retentionBus(t *testing.T, store state.StateStore, opts ...Option) *bus {
	t.Helper()
	return openIndexBus(t, store, testAuditMaxAge, append([]Option{WithClock(newSettableClock(retentionNow))}, opts...)...)
}

func runPass(t *testing.T, b *bus) retentionStats {
	t.Helper()
	stats, err := b.runRetentionPass(context.Background())
	mustf(t, err, "retention pass")
	return stats
}

func listKinds(t *testing.T, store state.StateStore, id identity.Quadruple, prefix string) []string {
	t.Helper()
	recs, err := store.ListKindForIdentity(context.Background(), id, prefix)
	mustf(t, err, "list %s", prefix)
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Kind)
	}
	return out
}

func TestRetention_AC1_PrefixTrimWholeChunksAndExactSummary(t *testing.T) {
	storeMatrix(t, func(t *testing.T, store state.StateStore) {
		b := retentionBus(t, store)
		id := sid("prefix")
		publishN(t, b, id, 1000, events.EventTypeAdminScopeUsed, oldAt())
		publishN(t, b, id, 10, events.EventTypeRuntimeWarning, recentAt())
		rows := sessionIndexRows(t, store, id)
		stats := runPass(t, b)
		// 771 deletes exceed one session's per-pass slice (514); the
		// second pass finishes the cleanup.
		if again := runPass(t, b); again.RowsTrimmed != 0 {
			t.Fatalf("second pass trimmed again: %+v", again)
		}
		if stats.ChunksTrimmed != 3 || stats.RowsTrimmed != 768 {
			t.Fatalf("pass trimmed %d chunks / %d rows, want 3 / 768 (only whole qualifying chunks)", stats.ChunksTrimmed, stats.RowsTrimmed)
		}
		hdr, _ := loadHeaderV2(t, store, id)
		if hdr.FirstChunk != 3 || hdr.RetainedCount != 242 || hdr.Trimmed.Count != 768 || hdr.Trimmed.ThroughSeq != rows[767].Seq || hdr.FirstSeq != rows[768].Seq {
			t.Fatalf("header after trim: %+v", hdr)
		}
		if hdr.Trimmed.CleanupPending {
			t.Fatal("cleanup marker still set after a complete pass")
		}
		// The retained range is chain-verified intact.
		retained := sessionIndexRows(t, store, id)
		if len(retained) != 242 || retained[0].Seq != rows[768].Seq {
			t.Fatalf("retained %d rows starting at %d", len(retained), retained[0].Seq)
		}
		// Trimmed records are gone; retained ones remain.
		if chunks := listKinds(t, store, id, kindChunkPrefix); len(chunks) != 0 {
			t.Fatalf("trimmed chunk records remain: %v", chunks)
		}
		if entries := listKinds(t, store, id, kindEntryPrefix); len(entries) != 242 {
			t.Fatalf("entries after trim = %d, want 242", len(entries))
		}
		summary, _, err := b.loadRetentionSummary(context.Background(), id)
		mustf(t, err, "summary")
		if summary.TrimmedCount != 768 || summary.ByType[string(events.EventTypeAdminScopeUsed)] != 768 ||
			summary.FirstTrimmedSeq != rows[0].Seq || summary.LastTrimmedSeq != rows[767].Seq ||
			summary.FirstTrimmedOccurredAt != rows[0].OccurredAt || summary.LastTrimmedOccurredAt != rows[767].OccurredAt ||
			summary.ChainAnchor != hdr.BaseDigest || summary.Passes != 1 || summary.Policy.AuditMaxAge != testAuditMaxAge.String() {
			t.Fatalf("summary not exact: %+v", summary)
		}
		_, _, truncated, err := b.Bounds(context.Background(), filterFor(id))
		if err != nil || !truncated {
			t.Fatalf("Bounds truncated = %v, %v; want true", truncated, err)
		}
		if _, err := b.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: 1}, filterFor(id)); !errors.Is(err, events.ErrCursorTooOld) {
			t.Fatalf("Replay from seq 1 = %v, want ErrCursorTooOld", err)
		}
		all, err := b.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
		if err != nil || len(all) != 242 {
			t.Fatalf("Replay from the start = %d, %v; want the 242 retained", len(all), err)
		}
		// A second pass changes nothing.
		if again := runPass(t, b); again.RowsTrimmed != 0 || again.EntriesDeleted != 0 {
			t.Fatalf("idempotent pass trimmed again: %+v", again)
		}
	})
}

func TestRetention_AC2_NonAuditRowStopsTrim(t *testing.T) {
	t.Run("prefix", func(t *testing.T) {
		store := newInmemTestStore(t)
		b := retentionBus(t, store)
		id := sid("mixed")
		publishN(t, b, id, 300, events.EventTypeAdminScopeUsed, oldAt())
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, oldAt())), "user row")
		publishN(t, b, id, 400, events.EventTypeAdminScopeUsed, oldAt())
		stats := runPass(t, b)
		hdr, _ := loadHeaderV2(t, store, id)
		if stats.ChunksTrimmed != 1 || hdr.FirstChunk != 1 {
			t.Fatalf("trimmed %d chunks (first_chunk=%d); the non-audit row in chunk 1 must stop the trim", stats.ChunksTrimmed, hdr.FirstChunk)
		}
	})
	t.Run("tail", func(t *testing.T) {
		store := newInmemTestStore(t)
		b := retentionBus(t, store)
		id := sid("tail-mixed")
		publishN(t, b, id, 5, events.EventTypeAdminScopeUsed, oldAt())
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, oldAt())), "user row")
		if stats := runPass(t, b); stats.RowsTrimmed != 0 {
			t.Fatalf("tail with a non-audit row was trimmed: %+v", stats)
		}
		if hdr, _ := loadHeaderV2(t, store, id); hdr.Trimmed.Count != 0 || hdr.RetainedCount != 6 {
			t.Fatalf("header changed: %+v", hdr)
		}
	})
	t.Run("recent_audit", func(t *testing.T) {
		store := newInmemTestStore(t)
		b := retentionBus(t, store)
		id := sid("recent")
		publishN(t, b, id, 600, events.EventTypeAdminScopeUsed, recentAt())
		if stats := runPass(t, b); stats.RowsTrimmed != 0 {
			t.Fatalf("recent audit rows were trimmed: %+v", stats)
		}
	})
}

func TestRetention_AC3_TailRuleTrimsOneRowAuditSession(t *testing.T) {
	storeMatrix(t, func(t *testing.T, store state.StateStore) {
		b := retentionBus(t, store)
		id := sid("one-row")
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeAdminScopeUsed, oldAt())), "publish")
		seq := sessionIndexSeqs(t, store, id)[0]
		stats := runPass(t, b)
		if stats.RowsTrimmed != 1 || stats.EntriesDeleted != 1 {
			t.Fatalf("tail rule stats %+v", stats)
		}
		hdr, _ := loadHeaderV2(t, store, id)
		if hdr.RetainedCount != 0 || len(hdr.Tail) != 0 || hdr.LastSeq != seq || hdr.Trimmed.ThroughSeq != seq || hdr.Trimmed.Count != 1 || hdr.Trimmed.CleanupPending || len(hdr.Trimmed.PendingEntries) != 0 {
			t.Fatalf("empty header wrong: %+v", hdr)
		}
		if entries := listKinds(t, store, id, kindEntryPrefix); len(entries) != 0 {
			t.Fatalf("trimmed tail entry remains: %v", entries)
		}
		summary, _, err := b.loadRetentionSummary(context.Background(), id)
		mustf(t, err, "summary")
		if summary.TrimmedCount != 1 || summary.FirstTrimmedSeq != seq || summary.LastTrimmedSeq != seq {
			t.Fatalf("summary %+v", summary)
		}
		if lo, hi, truncated, err := b.Bounds(context.Background(), filterFor(id)); err != nil || lo != 0 || hi != 0 || !truncated {
			t.Fatalf("Bounds on an emptied session = %d %d %v %v, want an empty truncated window", lo, hi, truncated, err)
		}
		// The session keeps working: new events append after the cut.
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, recentAt())), "publish after trim")
		lo, hi, truncated, err := b.Bounds(context.Background(), filterFor(id))
		if err != nil || lo != hi || lo <= seq || !truncated {
			t.Fatalf("Bounds after new publish = %d %d %v %v", lo, hi, truncated, err)
		}
	})
}

// deleteFaultStore fails every DeleteIf while armed (a crash between the trim
// commit and its deletes).
type deleteFaultStore struct {
	state.StateStore
	armed   atomic.Bool
	deletes atomic.Int64
}

func (s *deleteFaultStore) DeleteIf(ctx context.Context, exp state.SlotExpectation) (bool, error) {
	if s.armed.Load() {
		return false, errors.New("injected crash before delete")
	}
	s.deletes.Add(1)
	return s.StateStore.DeleteIf(ctx, exp)
}

func TestRetention_AC4_CrashBetweenCommitAndDeleteRecoversNextPass(t *testing.T) {
	inner := newInmemTestStore(t)
	store := &deleteFaultStore{StateStore: inner}
	b := retentionBus(t, store)
	id := sid("crashy")
	publishN(t, b, id, 600, events.EventTypeAdminScopeUsed, oldAt())
	publishN(t, b, id, 20, events.EventTypeRuntimeWarning, recentAt())
	store.armed.Store(true)
	_ = runPass(t, b)
	hdr, _ := loadHeaderV2(t, inner, id)
	if hdr.Trimmed.Count != 512 || !hdr.Trimmed.CleanupPending {
		t.Fatalf("trim did not commit before the crash: %+v", hdr.Trimmed)
	}
	if chunks := listKinds(t, inner, id, kindChunkPrefix); len(chunks) != 2 {
		t.Fatalf("expected the 2 trimmed chunks as orphans, found %v", chunks)
	}
	// Readers never see a gap error while orphans remain.
	if _, err := b.Window(context.Background(), 0, 1000, filterFor(id)); err != nil {
		t.Fatalf("Window with orphans: %v", err)
	}
	if got, err := b.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id)); err != nil || len(got) != 108 {
		t.Fatalf("Replay with orphans = %d, %v", len(got), err)
	}
	if _, err := b.ListWindow(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Limit: 1000}); err != nil {
		t.Fatalf("ListWindow with orphans: %v", err)
	}
	if _, err := b.Page(context.Background(), 0, 1000); err != nil {
		t.Fatalf("Page with orphans: %v", err)
	}
	store.armed.Store(false)
	stats := runPass(t, b)
	if stats.ChunksDeleted != 2 || stats.EntriesDeleted != 512 {
		t.Fatalf("recovery pass stats %+v", stats)
	}
	if chunks := listKinds(t, inner, id, kindChunkPrefix); len(chunks) != 0 {
		t.Fatalf("orphans remain: %v", chunks)
	}
	if hdr, _ := loadHeaderV2(t, inner, id); hdr.Trimmed.CleanupPending {
		t.Fatal("cleanup marker not cleared after recovery")
	}
}

// pauseOnce blocks the first hook invocation that matches until released.
type pauseOnce struct {
	fired   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func newPauseOnce() *pauseOnce {
	return &pauseOnce{reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *pauseOnce) hit() {
	if p.fired.CompareAndSwap(false, true) {
		close(p.reached)
		<-p.release
	}
}

// TestRetention_AC5_ReaderRacesTrim pauses each body reader deterministically
// inside its index walk while a trim commits and deletes, and proves the
// honest outcomes: ErrCursorTooOld, a shortened window, a truncated list and
// RetentionGap — never a gap error.
func TestRetention_AC5_ReaderRacesTrim(t *testing.T) {
	setup := func(t *testing.T) (*bus, *bus, identity.Quadruple, []indexRow) {
		store := newInmemTestStore(t)
		writer := retentionBus(t, store)
		id := sid("raced")
		publishN(t, writer, id, 512, events.EventTypeAdminScopeUsed, oldAt())
		publishN(t, writer, id, 300, events.EventTypeRuntimeWarning, recentAt())
		reader := retentionBus(t, store)
		return writer, reader, id, sessionIndexRows(t, store, id)
	}
	trimWhilePaused := func(t *testing.T, sweeper *bus, p *pauseOnce, read func()) {
		done := make(chan struct{})
		go func() { defer close(done); read() }()
		select {
		case <-p.reached:
		case <-time.After(10 * time.Second):
			t.Fatal("reader never reached the pause point")
		}
		stats := runPass(t, sweeper)
		if stats.ChunksTrimmed != 2 || stats.EntriesDeleted != 512 || stats.ChunksDeleted != 2 {
			t.Errorf("concurrent trim stats %+v", stats)
		}
		close(p.release)
		<-done
	}
	t.Run("replay_body_load", func(t *testing.T) {
		writer, reader, id, rows := setup(t)
		p := newPauseOnce()
		reader.hookBeforeBodyLoad = func(_ identity.Quadruple, seq uint64) {
			if seq <= rows[511].Seq {
				p.hit()
			}
		}
		var err error
		trimWhilePaused(t, writer, p, func() {
			_, err = reader.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: rows[4].Seq}, filterFor(id))
		})
		if !errors.Is(err, events.ErrCursorTooOld) {
			t.Fatalf("Replay raced by a trim = %v, want ErrCursorTooOld", err)
		}
	})
	t.Run("replay_from_start_restarts", func(t *testing.T) {
		writer, reader, id, rows := setup(t)
		p := newPauseOnce()
		reader.hookBeforeBodyLoad = func(_ identity.Quadruple, seq uint64) {
			if seq <= rows[511].Seq {
				p.hit()
			}
		}
		var got []events.Event
		var err error
		trimWhilePaused(t, writer, p, func() {
			got, err = reader.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
		})
		if err != nil || len(got) != 300 {
			t.Fatalf("from-start Replay raced by a trim = %d, %v; want the 300 retained", len(got), err)
		}
	})
	chunkPause := func(reader *bus, p *pauseOnce) {
		reader.hookAfterChunkLoad = func(_ identity.Quadruple, chunk uint64) {
			if chunk == 2 {
				p.hit()
			}
		}
	}
	t.Run("window", func(t *testing.T) {
		writer, reader, id, _ := setup(t)
		p := newPauseOnce()
		chunkPause(reader, p)
		var got []events.Event
		var err error
		trimWhilePaused(t, writer, p, func() { got, err = reader.Window(context.Background(), 0, 1000, filterFor(id)) })
		if err != nil || len(got) != 300 {
			t.Fatalf("Window raced by a trim = %d, %v; want a page ending at the new boundary", len(got), err)
		}
	})
	t.Run("events_list", func(t *testing.T) {
		writer, reader, id, _ := setup(t)
		p := newPauseOnce()
		chunkPause(reader, p)
		var page events.EventListPage
		var err error
		trimWhilePaused(t, writer, p, func() {
			page, err = reader.ListWindow(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Limit: 1000})
		})
		if err != nil || !page.Truncated || len(page.Events) != 300 {
			t.Fatalf("events.list raced by a trim = %d events truncated=%v err=%v", len(page.Events), page.Truncated, err)
		}
	})
	t.Run("events_list_metadata", func(t *testing.T) {
		writer, reader, id, _ := setup(t)
		p := newPauseOnce()
		chunkPause(reader, p)
		var page events.MetadataListPage
		var err error
		trimWhilePaused(t, writer, p, func() {
			page, err = reader.ListWindowMetadata(context.Background(), events.EventListQuery{Filter: wireFilterFor(id), Limit: 1000})
		})
		if err != nil || !page.Truncated || len(page.Events) != 300 {
			t.Fatalf("metadata list raced by a trim = %d truncated=%v err=%v", len(page.Events), page.Truncated, err)
		}
	})
	t.Run("projection_page", func(t *testing.T) {
		writer, reader, _, _ := setup(t)
		p := newPauseOnce()
		chunkPause(reader, p)
		var page events.ProjectionPage
		var err error
		trimWhilePaused(t, writer, p, func() { page, err = reader.Page(context.Background(), 0, 1000) })
		if err != nil || !page.RetentionGap {
			t.Fatalf("projection page raced by a trim: gap=%v err=%v", page.RetentionGap, err)
		}
		// A lagging projector reading after the trim also sees the gap.
		later, err := reader.Page(context.Background(), 0, 1000)
		if err != nil || !later.RetentionGap || len(later.Events) != 300 {
			t.Fatalf("post-trim page from 0: %d events gap=%v err=%v", len(later.Events), later.RetentionGap, err)
		}
	})
}

// TestRetention_AC6_ConcurrentPublishDuringTrimPostgres runs two bus
// instances on one real Postgres store: both publish to sessions with an old
// audit prefix while both sweep. No event is lost.
func TestRetention_AC6_ConcurrentPublishDuringTrimPostgres(t *testing.T) {
	store := newPostgresStore(t)
	const sessions = 4
	var ids []identity.Quadruple
	var start uint64
	for s := range sessions {
		id := sid(fmt.Sprintf("busy-%d", s))
		ids = append(ids, id)
		seedV2Session(t, store, seedSpec{id: id, startSeq: start, n: 3 * indexChunkRows, at: oldAt(), bodies: true,
			typeOf: func(int) events.EventType { return events.EventTypeAdminScopeUsed }})
		start += 3 * indexChunkRows
	}
	left := retentionBus(t, store)
	right := retentionBus(t, store)
	var published sync.Map // session -> *atomic.Int64
	for _, id := range ids {
		published.Store(id.SessionID, new(atomic.Int64))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 1024)
	for i, bb := range []*bus{left, right} {
		for p := range 6 {
			wg.Add(1)
			go func(bb *bus, p int) {
				defer wg.Done()
				for k := range 25 {
					id := ids[(p+k+i)%sessions]
					if err := bb.Publish(ctx, testEvent(id, events.EventTypeRuntimeWarning, recentAt())); err != nil {
						errCh <- err
						return
					}
					v, _ := published.Load(id.SessionID)
					v.(*atomic.Int64).Add(1)
				}
			}(bb, p)
		}
		wg.Add(1)
		go func(bb *bus) {
			defer wg.Done()
			for range 4 {
				if _, err := bb.runRetentionPass(ctx); err != nil {
					errCh <- err
					return
				}
			}
		}(bb)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent publish/trim: %v", err)
	}
	// Settle any cleanup a racing pass deferred.
	runPass(t, left)
	for _, id := range ids {
		v, _ := published.Load(id.SessionID)
		want := int(v.(*atomic.Int64).Load())
		got, err := left.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
		mustf(t, err, "Replay")
		recent := 0
		for _, ev := range got {
			if ev.Type == events.EventTypeRuntimeWarning {
				recent++
			}
		}
		if recent != want {
			t.Fatalf("session %s: %d recent events retained, %d published", id.SessionID, recent, want)
		}
		hdr, _ := loadHeaderV2(t, store, id)
		summary, _, err := left.loadRetentionSummary(context.Background(), id)
		mustf(t, err, "summary")
		if hdr.Trimmed.Count != summary.TrimmedCount || hdr.Trimmed.Count+hdr.RetainedCount != uint64(3*indexChunkRows+want) {
			t.Fatalf("session %s: trimmed %d (summary %d) + retained %d != %d", id.SessionID, hdr.Trimmed.Count, summary.TrimmedCount, hdr.RetainedCount, 3*indexChunkRows+want)
		}
	}
}

// countingDeleteStore counts DeleteIf calls.
type countingDeleteStore struct {
	state.StateStore
	deletes atomic.Int64
	calls   atomic.Int64
}

func (s *countingDeleteStore) DeleteIf(ctx context.Context, exp state.SlotExpectation) (bool, error) {
	s.deletes.Add(1)
	return s.StateStore.DeleteIf(ctx, exp)
}

func (s *countingDeleteStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

func (s *countingDeleteStore) ListKind(ctx context.Context, scope state.ListScope, prefix string) ([]state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.ListKind(ctx, scope, prefix)
}

func TestRetention_AC7_ScaleBudgetAndResume(t *testing.T) {
	inner := newInmemTestStore(t)
	const sessions = 12_000
	for i := range sessions {
		id := sid(fmt.Sprintf("audit-%05d", i))
		ev := testEvent(id, events.EventTypeAdminScopeUsed, oldAt())
		ev.Sequence = uint64(i + 1)
		raw, err := encodeEvent(ev)
		mustf(t, err, "encode")
		mustf(t, inner.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindEntryPrefix + seqToken(ev.Sequence), Bytes: raw}), "entry")
		hdr := newIndexHeader()
		row, _ := rowFromEvent(ev)
		_, _ = hdr.appendRows([]indexRow{row})
		hraw, err := encodeHeader(hdr)
		mustf(t, err, "header")
		mustf(t, inner.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: hraw}), "header")
	}
	floorAuthority(t, inner, sessions)
	store := &countingDeleteStore{StateStore: inner}
	b := retentionBus(t, store)
	var resumes []string
	trimmed := 0
	cleaned := func() int {
		n := 0
		for i := range sessions {
			hdr, _ := loadHeaderV2(t, inner, sid(fmt.Sprintf("audit-%05d", i)))
			if !hdr.Trimmed.CleanupPending && hdr.Trimmed.Count == 1 {
				n++
			}
		}
		return n
	}
	for pass := 1; pass <= 10; pass++ {
		before := store.deletes.Load()
		stats := runPass(t, b)
		if got := store.deletes.Load() - before; got > retentionDeleteBudget {
			t.Fatalf("pass %d issued %d deletes, budget is %d", pass, got, retentionDeleteBudget)
		}
		if stats.SessionsScanned != sessions || b.retentionHeads.Load() != sessions {
			t.Fatalf("pass %d scanned %d (heads gauge %d); every pass must traverse every head", pass, stats.SessionsScanned, b.retentionHeads.Load())
		}
		trimmed += stats.RowsTrimmed
		if pass == 1 && trimmed != sessions {
			// The delete budget must not starve trim commits: every
			// eligible session is trimmed in the first pass.
			t.Fatalf("pass 1 trimmed %d of %d sessions", trimmed, sessions)
		}
		resumes = append(resumes, b.maint.resumeKey)
		if b.maint.resumeKey == "" {
			break
		}
	}
	if got := cleaned(); got != sessions {
		t.Fatalf("%d of %d sessions fully cleaned within 10 passes", got, sessions)
	}
	for i := 1; i < len(resumes)-1; i++ {
		if resumes[i] <= resumes[i-1] {
			t.Fatalf("resume point did not advance: %q then %q", resumes[i-1], resumes[i])
		}
	}
	if len(resumes) < 6 {
		t.Fatalf("12k one-row sessions were cleaned in %d passes; the 2,000-delete budget was not applied", len(resumes))
	}
	// Every session ends empty, cleaned, with its entry gone.
	for _, i := range []int{0, 1, 5999, sessions - 1} {
		id := sid(fmt.Sprintf("audit-%05d", i))
		hdr, _ := loadHeaderV2(t, inner, id)
		if hdr.RetainedCount != 0 || hdr.Trimmed.CleanupPending {
			t.Fatalf("session %d not fully trimmed: %+v", i, hdr)
		}
	}
}

func TestRetention_AC8_CorruptCandidateChunkAbortsThatSessionOnly(t *testing.T) {
	store := newInmemTestStore(t)
	b := retentionBus(t, store)
	bad, good := sid("bad"), sid("good")
	publishN(t, b, bad, 600, events.EventTypeAdminScopeUsed, oldAt())
	publishN(t, b, good, 600, events.EventTypeAdminScopeUsed, oldAt())
	corruptChunk(t, store, bad, 0, func(d map[string]any) { d["max_occurred_at"] = addNumber(d["max_occurred_at"], 1) })
	before := b.indexIntegrityFailures.Load()
	stats := runPass(t, b)
	if hdr, _ := loadHeaderV2(t, store, bad); hdr.Trimmed.Count != 0 {
		t.Fatalf("corrupt session was trimmed: %+v", hdr.Trimmed)
	}
	if hdr, _ := loadHeaderV2(t, store, good); hdr.Trimmed.Count != 600 {
		t.Fatalf("healthy session trimmed %d, want 600 (two chunks plus the audit-only tail)", hdr.Trimmed.Count)
	}
	if stats.SessionsTrimmed != 1 || b.indexIntegrityFailures.Load() <= before {
		t.Fatalf("stats %+v integrity failures %d->%d", stats, before, b.indexIntegrityFailures.Load())
	}
}

func TestRetention_AC9_DisabledDoesNoWorkAndValidation(t *testing.T) {
	inner := newInmemTestStore(t)
	store := &countingDeleteStore{StateStore: inner}
	b := openIndexBus(t, store, 0)
	id := sid("kept")
	publishN(t, b, id, 600, events.EventTypeAdminScopeUsed, oldAt())
	before := store.calls.Load()
	stats, err := b.runRetentionPass(context.Background())
	if err != nil || stats.SessionsScanned != 0 || store.calls.Load() != before {
		t.Fatalf("disabled retention did work: %+v, %v, %d store calls", stats, err, store.calls.Load()-before)
	}
	if hdr, _ := loadHeaderV2(t, inner, id); hdr.Trimmed.Count != 0 {
		t.Fatal("disabled retention trimmed")
	}
	cfg := metadataCfg()
	for _, tc := range []struct {
		age time.Duration
		ok  bool
	}{{0, true}, {time.Hour, false}, {23 * time.Hour, false}, {24 * time.Hour, true}, {2160 * time.Hour, true}} {
		cfg.Retention.AuditMaxAge = tc.age
		bb, err := New(context.Background(), cfg, auditpatterns.New(), newInmemTestStore(t), withMaintenance(func(m *maintenance) { m.disabled = true }))
		if (err == nil) != tc.ok {
			t.Fatalf("New(audit_max_age=%s) err=%v, want ok=%v", tc.age, err, tc.ok)
		}
		if bb != nil {
			_ = bb.Close(context.Background())
		}
	}
}
