package turns

import (
	"context"
	"errors"
	"fmt"
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
	if page.Truncated {
		t.Errorf("Truncated=true without eviction")
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
	for _, bad := range []string{"!!!", "base64-ok-but-garbage", "v2|1|run", "v1|notanint|run", "v1|1|"} {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) error=%v, want ErrInvalidCursor", bad, err)
		}
	}
	if c, err := DecodeCursor(""); err != nil || c != nil {
		t.Errorf("empty cursor decodes to (%v, %v), want (nil, nil)", c, err)
	}
}

func TestList_LimitBounds(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	seedTurns(t, p, id, 3)

	if _, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit + 1}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-max limit error=%v, want ErrInvalidInput", err)
	}
	if _, err := p.List(context.Background(), id, ListOptions{Limit: -1}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative limit error=%v, want ErrInvalidInput", err)
	}
	// Zero limit means the documented default.
	page, err := p.List(context.Background(), id, ListOptions{})
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Errorf("default-limit page has %d rows, want 3", len(page.Rows))
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
