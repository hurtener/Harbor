package turns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// seedTurns appends n turns (run-1..run-n) to the session and returns
// their rows.
func seedTurns(t *testing.T, p *Projector, id identity.Identity, n int) []TurnRow {
	t.Helper()
	rows := make([]TurnRow, 0, n)
	for i := 1; i <= n; i++ {
		row, err := appendTurn(p, id, TurnID(fmt.Sprintf("run-%03d", i)))
		if err != nil {
			t.Fatalf("seed append %d: %v", i, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestList_NewestFirstOrdering(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	rows := seedTurns(t, p, id, 5)

	page, err := p.List(context.Background(), id, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Rows) != 5 {
		t.Fatalf("page has %d rows, want 5", len(page.Rows))
	}
	// Newest first: sequence 5,4,3,2,1.
	for i, want := range []Seq{5, 4, 3, 2, 1} {
		if page.Rows[i].Sequence != want {
			t.Errorf("row %d sequence=%d, want %d (newest first)", i, page.Rows[i].Sequence, want)
		}
	}
	// The seeded rows are the newest — a fresh walk matches the seed
	// order reversed.
	for i := range rows {
		if page.Rows[i].TurnID != rows[len(rows)-1-i].TurnID {
			t.Errorf("row %d=%q, want %q", i, page.Rows[i].TurnID, rows[len(rows)-1-i].TurnID)
		}
	}
	if page.HasMore {
		t.Errorf("HasMore=true on a full small page")
	}
	if !page.Complete {
		t.Errorf("Complete=false without eviction (PartialReason=%q)", page.PartialReason)
	}
}

func TestList_KeysetPaging_WalksEveryTurnExactlyOnce(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 37)

	var walked []TurnID
	var cursor *Cursor
	pages := 0
	for {
		page, err := p.List(context.Background(), id, ListOptions{Before: cursor, Limit: 10})
		if err != nil {
			t.Fatalf("List page %d: %v", pages, err)
		}
		pages++
		for _, row := range page.Rows {
			walked = append(walked, row.TurnID)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 4 {
		t.Errorf("walked %d pages, want 4 (10+10+10+7)", pages)
	}
	if len(walked) != 37 {
		t.Fatalf("walked %d turns, want 37 (no skips)", len(walked))
	}
	seen := map[TurnID]bool{}
	for _, id := range walked {
		if seen[id] {
			t.Errorf("duplicate turn %q in the walk (no duplicates)", id)
		}
		seen[id] = true
	}
	// Newest-first: sequence strictly decreasing across the walk.
	for i := 1; i < len(walked); i++ {
		prev, _ := p.Get(context.Background(), id, walked[i-1])
		cur, _ := p.Get(context.Background(), id, walked[i])
		if prev.Sequence <= cur.Sequence {
			t.Errorf("walk not newest-first at %q (seq %d) → %q (seq %d)",
				walked[i-1], prev.Sequence, walked[i], cur.Sequence)
		}
	}
}

func TestList_KeysetBoundary_NoDuplicateOfBoundaryRow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 15)

	page1, err := p.List(context.Background(), id, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Rows) != 10 {
		t.Fatalf("page 1 has %d rows, want 10", len(page1.Rows))
	}
	page2, err := p.List(context.Background(), id, ListOptions{Before: page1.NextCursor, Limit: 10})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Rows) != 5 {
		t.Fatalf("page 2 has %d rows, want 5", len(page2.Rows))
	}
	// The boundary row (page 1's last) must not re-appear in page 2.
	boundary := page1.Rows[len(page1.Rows)-1]
	for _, row := range page2.Rows {
		if row.TurnID == boundary.TurnID {
			t.Errorf("boundary row %q duplicated across pages", boundary.TurnID)
		}
	}
	// And the keyset is strictly older: every page-2 row has a lower
	// sequence than the boundary row.
	for _, row := range page2.Rows {
		if row.Sequence >= boundary.Sequence {
			t.Errorf("page 2 row %q seq %d not strictly older than boundary seq %d",
				row.TurnID, row.Sequence, boundary.Sequence)
		}
	}
}

func TestList_CursorRoundTrip(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 3)

	page, err := p.List(context.Background(), id, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	enc := page.NextCursor.Encode()
	dec, err := DecodeCursor(enc)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if *dec != *page.NextCursor {
		t.Errorf("decoded cursor %+v, want %+v", dec, page.NextCursor)
	}

	// The decoded cursor drives the same next page as the original.
	pageA, _ := p.List(context.Background(), id, ListOptions{Before: page.NextCursor, Limit: 10})
	pageB, _ := p.List(context.Background(), id, ListOptions{Before: dec, Limit: 10})
	if len(pageA.Rows) != len(pageB.Rows) {
		t.Errorf("cursor round-trip changed the page size: %d vs %d", len(pageA.Rows), len(pageB.Rows))
	}
	for i := range pageA.Rows {
		if pageA.Rows[i].TurnID != pageB.Rows[i].TurnID {
			t.Errorf("cursor round-trip changed row %d: %q vs %q", i, pageA.Rows[i].TurnID, pageB.Rows[i].TurnID)
		}
	}

	// Malformed cursors fail loud — never a silent reset to page one.
	for _, bad := range []string{
		"!!!",
		"base64-ok-but-garbage",
		"v1|0|1|run|sess",       // version mismatch
		"2|notanint|1|run|sess", // snapshot not an integer
		"2|0|notanint|run|sess", // sequence not an integer
		"2|0|1||sess",           // empty turn id
		"2|0|1|run|",            // empty session id
		"2|0|1|run|sess|extra",  // too many fields
	} {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) error=%v, want ErrInvalidCursor", bad, err)
		}
	}
	if c, err := DecodeCursor(""); err != nil || c != nil {
		t.Errorf("empty cursor decodes to (%v, %v), want (nil, nil)", c, err)
	}
}

func TestList_LimitBounds_AndPublicDefaults(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 30)

	if _, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit + 1}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-max limit error=%v, want ErrInvalidInput", err)
	}
	if _, err := p.List(context.Background(), id, ListOptions{Limit: -1}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative limit error=%v, want ErrInvalidInput", err)
	}
	// The PUBLIC defaults are pinned: default EXACTLY 20, max EXACTLY
	// 50 (the Protocol-mandated list surface).
	if DefaultListLimit != 20 {
		t.Errorf("DefaultListLimit=%d, want exactly 20", DefaultListLimit)
	}
	if MaxListLimit != 50 {
		t.Errorf("MaxListLimit=%d, want exactly 50", MaxListLimit)
	}
	// Zero limit means the documented default of 20.
	page, err := p.List(context.Background(), id, ListOptions{})
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if len(page.Rows) != 20 {
		t.Errorf("default-limit page has %d rows, want 20", len(page.Rows))
	}
	// The max limit (50) is accepted and pages the rest.
	page2, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("max limit: %v", err)
	}
	if len(page2.Rows) != 30 {
		t.Errorf("max-limit page has %d rows, want all 30", len(page2.Rows))
	}
}

// TestList_PageSnapshotAndLiveResumeSurface pins the Page's snapshot
// as-of / remaining / completeness / live-resume surface: the fields a
// consumer folds the durable page (page-before-subscribe) with and a
// Protocol layer maps onto its wire page shape.
func TestList_PageSnapshotAndLiveResumeSurface(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 5)

	// Give the rows distinct applied event sequences so LiveResumeSeq
	// is meaningful: the newest row reflects the highest sequence.
	row, err := p.Get(context.Background(), id, "run-005")
	if err != nil {
		t.Fatalf("get newest: %v", err)
	}
	ans := Answer{State: AnswerStateInline, Inline: "a"}
	if _, err := p.Update(context.Background(), id, "run-005", row.Version, Update{Answer: &ans, EventSeq: 7}); err != nil {
		t.Fatalf("update newest: %v", err)
	}

	page, err := p.List(context.Background(), id, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Snapshot as-of: the page is stamped with the read instant and
	// the projection snapshot generation it binds to.
	if page.AsOf.IsZero() {
		t.Errorf("AsOf is zero — the page must carry its snapshot as-of")
	}
	if page.Snapshot != 0 {
		t.Errorf("Snapshot=%d, want 0 (fresh session's initial generation)", page.Snapshot)
	}
	// Exact remaining: 3 older retained rows beyond the 2-row page.
	if !page.CountExact || page.Remaining != 3 {
		t.Errorf("Remaining=%d CountExact=%v, want 3/true", page.Remaining, page.CountExact)
	}
	if !page.HasMore || page.NextCursor == nil {
		t.Errorf("HasMore=%v NextCursor=%v, want true/non-nil", page.HasMore, page.NextCursor)
	}
	if !page.Complete || page.PartialReason != "" {
		t.Errorf("Complete=%v PartialReason=%q, want true/empty", page.Complete, page.PartialReason)
	}
	// Live-resume sequence: the newest row's applied event sequence —
	// the live-resume cursor the consumer subscribes from after folding
	// the durable page; the server replays only events strictly newer
	// than the snapshot.
	if page.LiveResumeSeq != 7 {
		t.Errorf("LiveResumeSeq=%d, want 7 (the newest row's applied event sequence)", page.LiveResumeSeq)
	}
	// The minted cursor binds the session + snapshot; an encoded
	// round-trip preserves them.
	dec, err := DecodeCursor(page.NextCursor.Encode())
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if dec.SessionID != id.SessionID || dec.Snapshot != page.Snapshot {
		t.Errorf("decoded cursor binding wrong: %+v", dec)
	}
}

// TestList_CursorBinding_RejectsForeignStaleExpired pins the DISTINCT
// domain errors an opaque cursor is rejected with: a foreign-session
// cursor, a stale-snapshot cursor (projection erased under the walk),
// and an expired cursor (boundary row no longer retained). Each maps
// onto its own Protocol error later; none ever silently resets to
// page one.
func TestList_CursorBinding_RejectsForeignStaleExpired(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 5)

	page, err := p.List(context.Background(), id, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Foreign session: a cursor minted for a DIFFERENT session is
	// rejected with its distinct error — never re-scoped.
	foreign := *page.NextCursor
	foreign.SessionID = "some-other-session"
	_, err = p.List(context.Background(), id, ListOptions{Before: &foreign, Limit: 2})
	if !errors.Is(err, ErrCursorForeignSession) {
		t.Errorf("foreign-session cursor error=%v, want ErrCursorForeignSession", err)
	}
	// The distinct errors also satisfy the umbrella ErrInvalidCursor
	// (Protocol mapping can use either).
	if !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("ErrCursorForeignSession must wrap ErrInvalidCursor for the existing umbrella contract")
	}

	// Expired cursor: a boundary row that is not retained (never
	// existed) on the LIVE session is rejected with its distinct error.
	expired := *page.NextCursor
	expired.TurnID = "never-existed"
	_, err = p.List(context.Background(), id, ListOptions{Before: &expired, Limit: 2})
	if !errors.Is(err, ErrCursorExpired) {
		t.Errorf("expired cursor error=%v, want ErrCursorExpired", err)
	}
	if !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("ErrCursorExpired must wrap ErrInvalidCursor for the existing umbrella contract")
	}

	// Stale snapshot: erase the projection (the snapshot generation
	// advances), then reuse the pre-erase cursor.
	if _, err := p.Erase(context.Background(), id); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	_, err = p.List(context.Background(), id, ListOptions{Before: page.NextCursor, Limit: 2})
	if !errors.Is(err, ErrCursorSnapshotStale) {
		t.Errorf("stale-snapshot cursor error=%v, want ErrCursorSnapshotStale", err)
	}
	if !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("ErrCursorSnapshotStale must wrap ErrInvalidCursor for the existing umbrella contract")
	}
}

// TestList_CursorBinding_RejectsForgedBoundarySequence pins the
// authoritative-boundary-row binding: a forged / altered cursor that
// names a RETAINED boundary turn but carries a sequence that does not
// equal the stored row's immutable sequence is refused with the typed
// ErrInvalidCursor — never silently re-keyset (which would skip or
// repeat rows).
func TestList_CursorBinding_RejectsForgedBoundarySequence(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 5)

	page, err := p.List(context.Background(), id, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.NextCursor == nil {
		t.Fatalf("no next cursor — the walk cannot continue")
	}

	// The genuine cursor names boundary row "run-003" with its real
	// sequence; a forged variant keeps the session, snapshot, and turn
	// id intact but alters the sequence.
	boundary, err := p.Get(context.Background(), id, page.NextCursor.TurnID)
	if err != nil {
		t.Fatalf("Get boundary row: %v", err)
	}
	if boundary.Sequence != page.NextCursor.Seq {
		t.Fatalf("test precondition: cursor seq %d must equal the boundary row's %d", page.NextCursor.Seq, boundary.Sequence)
	}
	for _, delta := range []Seq{1000, -1} {
		forged := *page.NextCursor
		forged.Seq = page.NextCursor.Seq + delta
		_, err = p.List(context.Background(), id, ListOptions{Before: &forged, Limit: 2})
		if !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("forged boundary-sequence cursor (seq %d) error=%v, want ErrInvalidCursor", forged.Seq, err)
		}
		// The forged cursor is NOT one of the distinct binding errors:
		// the session and snapshot bindings are intact and the boundary
		// row IS retained — only the sequence is forged.
		if errors.Is(err, ErrCursorForeignSession) || errors.Is(err, ErrCursorSnapshotStale) || errors.Is(err, ErrCursorExpired) {
			t.Errorf("forged-seq cursor misclassified as a distinct binding error: %v", err)
		}
	}

	// The genuine cursor still pages the walk with no skips / no
	// duplicates.
	page2, err := p.List(context.Background(), id, ListOptions{Before: page.NextCursor, Limit: 10})
	if err != nil {
		t.Fatalf("genuine cursor page 2: %v", err)
	}
	if len(page2.Rows) != 3 {
		t.Errorf("genuine cursor page 2 has %d rows, want 3 (no skips)", len(page2.Rows))
	}
	seen := map[TurnID]bool{}
	for _, r := range page2.Rows {
		if seen[r.TurnID] {
			t.Errorf("duplicate %q after genuine cursor (no duplicates)", r.TurnID)
		}
		seen[r.TurnID] = true
	}
}

// TestList_NoSkipNoDuplicate_UnderConcurrentAppends is the load-bearing
// paging guarantee: appends race while a reader walks every page; each
// turn present when the walk starts is returned exactly once, in a
// stable newest-first order — immutable sequence + tie-breaker keys
// mean a concurrent append can never satisfy an already-issued cursor.
func TestList_NoSkipNoDuplicate_UnderConcurrentAppends(t *testing.T) {
	p, st := newTestProjector(t, 0, false)
	id := tripleA()

	const appenders = 16
	const turnsPerAppender = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, appenders)
	for w := 0; w < appenders; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < turnsPerAppender; i++ {
				turnID := TurnID(fmt.Sprintf("w%d-t%02d", w, i))
				if _, err := p.Append(context.Background(), id, Append{TurnID: turnID, Query: "q"}); err != nil {
					errs <- fmt.Errorf("append %q: %w", turnID, err)
					return
				}
			}
		}(w)
	}

	// The reader walks pages while the appenders race.
	readerErrs := make(chan error, 1)
	var walked []TurnID
	var cursor *Cursor
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for pageNo := 0; ; pageNo++ {
			page, err := p.List(context.Background(), id, ListOptions{Before: cursor, Limit: 7})
			if err != nil {
				readerErrs <- fmt.Errorf("page %d: %w", pageNo, err)
				return
			}
			for _, row := range page.Rows {
				walked = append(walked, row.TurnID)
			}
			if !page.HasMore {
				return
			}
			cursor = page.NextCursor
		}
	}()

	close(start)
	wg.Wait()
	<-readerDone

	select {
	case err := <-readerErrs:
		t.Fatalf("reader: %v", err)
	default:
	}
	select {
	case err := <-errs:
		t.Fatalf("appender: %v", err)
	default:
	}

	// The reader's walk must be duplicate-free and strictly
	// newest-first — the immutable-keys guarantee.
	seen := map[TurnID]struct{}{}
	var prevSeq Seq = 1 << 60
	for _, tid := range walked {
		if _, dup := seen[tid]; dup {
			t.Errorf("duplicate %q in the concurrent walk — keyset paging must not duplicate", tid)
		}
		seen[tid] = struct{}{}
		row, err := st.GetTurn(context.Background(), id, tid)
		if err != nil {
			t.Fatalf("get %q: %v", tid, err)
		}
		if row.Sequence >= prevSeq {
			t.Errorf("walk not newest-first at %q (seq %d >= %d)", tid, row.Sequence, prevSeq)
		}
		prevSeq = row.Sequence
	}

	// After the appenders settle, a FRESH walk returns every turn
	// exactly once — nothing was skipped or duplicated.
	fresh := seedFreeWalk(t, p, id)
	if len(fresh) != appenders*turnsPerAppender {
		t.Fatalf("fresh walk has %d turns, want %d (no skips)", len(fresh), appenders*turnsPerAppender)
	}
}

// TestList_AppendDuringOlderPaging_NoSkipNoDuplicate pins the P5
// guarantee explicitly: appending NEWER turns while a walk is paging
// OLDER pages never skips or duplicates a row the walk already issued
// a cursor for — the newly appended turn (higher sequence) can never
// satisfy an already-issued keyset cursor, and the pre-append rows
// each appear exactly once.
func TestList_AppendDuringOlderPaging_NoSkipNoDuplicate(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 8)

	page1, err := p.List(context.Background(), id, ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Rows) != 3 || !page1.HasMore {
		t.Fatalf("page 1 has %d rows (HasMore=%v), want 3/true", len(page1.Rows), page1.HasMore)
	}
	// Append NEWER turns mid-walk.
	for i := 0; i < 3; i++ {
		if _, err := appendTurn(p, id, TurnID(fmt.Sprintf("mid-%d", i))); err != nil {
			t.Fatalf("mid-walk append: %v", err)
		}
	}

	var walked []TurnID
	for _, r := range page1.Rows {
		walked = append(walked, r.TurnID)
	}
	cursor := page1.NextCursor
	for cursor != nil {
		page, err := p.List(context.Background(), id, ListOptions{Before: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("walk page: %v", err)
		}
		for _, r := range page.Rows {
			walked = append(walked, r.TurnID)
		}
		cursor = page.NextCursor
	}

	// The continued walk returns EXACTLY the 8 pre-append rows (the
	// mid-walk appends are newer than every issued cursor).
	if len(walked) != 8 {
		t.Fatalf("walked %d rows, want 8 (the pre-append rows exactly once)", len(walked))
	}
	seen := map[TurnID]bool{}
	for _, tid := range walked {
		if seen[tid] {
			t.Errorf("duplicate %q in the older walk (no duplicates)", tid)
		}
		seen[tid] = true
		if strings.HasPrefix(string(tid), "mid-") {
			t.Errorf("mid-walk append %q appeared in the older walk — a newer row cannot satisfy an issued cursor", tid)
		}
	}
	// A FRESH walk sees everything exactly once (no skips): 8 pre +
	// 3 mid.
	fresh := seedFreeWalk(t, p, id)
	if len(fresh) != 11 {
		t.Errorf("fresh walk has %d turns, want 11 (8 pre + 3 mid)", len(fresh))
	}
}

// seedFreeWalk walks every page from the newest without a bound and
// returns the turn ids.
func seedFreeWalk(t *testing.T, p *Projector, id identity.Identity) []TurnID {
	t.Helper()
	var out []TurnID
	var cursor *Cursor
	for {
		page, err := p.List(context.Background(), id, ListOptions{Before: cursor, Limit: MaxListLimit})
		if err != nil {
			t.Fatalf("fresh walk page: %v", err)
		}
		for _, row := range page.Rows {
			out = append(out, row.TurnID)
		}
		if !page.HasMore {
			return out
		}
		cursor = page.NextCursor
	}
}

// TestList_ConcurrentAppends_UniquePerSessionSequences proves the
// store mints unique per-session sequences under a full race: no two
// turns ever share a sequence (the tie-breaker stays a defensive
// fallback, never a repair path).
func TestList_ConcurrentAppends_UniquePerSessionSequences(t *testing.T) {
	p, st := newTestProjector(t, 0, false)
	id := tripleA()

	const n = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := appendTurn(p, id, TurnID(fmt.Sprintf("r-%03d", i))); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("append: %v", err)
	default:
	}

	rows, _, _, err := st.ListTurns(context.Background(), id, nil, n*2)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	seqs := map[Seq]TurnID{}
	for _, row := range rows {
		if other, dup := seqs[row.Sequence]; dup {
			t.Errorf("duplicate sequence %d shared by %q and %q", row.Sequence, other, row.TurnID)
		}
		seqs[row.Sequence] = row.TurnID
	}
	if len(seqs) != n {
		t.Errorf("minted %d unique sequences, want %d", len(seqs), n)
	}
}

func TestList_RetentionEviction_NewestWindowSurvives(t *testing.T) {
	p, _ := newTestProjector(t, testStoreRetentionTiny, false)
	id := tripleA()
	rows := seedTurns(t, p, id, testStoreRetentionTiny+2)

	page, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Rows) != testStoreRetentionTiny {
		t.Fatalf("retained %d rows, want %d", len(page.Rows), testStoreRetentionTiny)
	}
	// The retained window is the NEWEST rows.
	wantNewest := rows[len(rows)-testStoreRetentionTiny:]
	for i := range page.Rows {
		want := wantNewest[len(wantNewest)-1-i].TurnID // newest first
		if page.Rows[i].TurnID != want {
			t.Errorf("retained row %d=%q, want %q (newest window)", i, page.Rows[i].TurnID, want)
		}
	}
}
