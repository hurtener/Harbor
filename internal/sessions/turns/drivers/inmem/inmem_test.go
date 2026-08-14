package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/conformancetest"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/inmem"
)

// TestInMem_Conformance drives the canonical conformance suite against
// the in-memory driver — the shared factory registration that gates
// this driver and every later driver (SQLite, Postgres) verbatim.
func TestInMem_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (turns.Store, func()) {
		s, err := inmem.New()
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestInMem_ConcurrentMixedReadsWrites_N100 is the store contract's
// concurrent-reuse gate (D-025) plus the lane's N >= 100 mixed
// read/write requirement: N = 100 workers against ONE shared store
// under -race, each interleaving appends, versioned updates, seals,
// point gets, list pages, and monotonic checkpoint saves. It asserts
// the four guarantees: no data races, no context bleed (every row
// holds exactly its writer's content), no cancellation cross-talk, and
// no goroutine leaks.
func TestInMem_ConcurrentMixedReadsWrites_N100(t *testing.T) {
	const workers = 100
	const perWorker = 10
	s, err := inmem.New(inmem.WithRetention(workers * perWorker)) // no eviction under test
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-mixed", UserID: "user-mixed", SessionID: "session-mixed"}

	baseline := runtime.NumGoroutine()
	start := make(chan struct{})
	opErrs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range perWorker {
				turnID := turns.TurnID(fmt.Sprintf("w%03d-i%02d", w, i))
				row, err := s.AppendTurnIf(ctx, id, turns.TurnRow{
					TurnID:    turnID,
					TaskID:    "task-" + string(turnID),
					RunID:     "run-" + string(turnID), // distinct task / run ids, never conflated
					SessionID: id.SessionID,
					Status:    turns.StatusRunning,
					Query:     turns.Query{Text: fmt.Sprintf("q-%d-%d", w, i), Complete: turns.CompletenessComplete},
				})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d append %d: %w", w, i, err)
					return
				}
				switch i % 3 {
				case 0: // versioned update
					next := row
					next.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: fmt.Sprintf("a-%d-%d", w, i)}
					if _, err := s.UpdateTurnIf(ctx, id, turnID, row.Version, next); err != nil {
						opErrs <- fmt.Errorf("worker %d update %d: %w", w, i, err)
						return
					}
				case 1: // seal to a terminal row, then read it back
					sealed := row
					sealed.Status = turns.StatusComplete
					sealed.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: fmt.Sprintf("final-%d-%d", w, i)}
					if _, err := s.SealTurnIf(ctx, id, turnID, row.Version, sealed); err != nil {
						opErrs <- fmt.Errorf("worker %d seal %d: %w", w, i, err)
						return
					}
				case 2: // point read — the row must hold exactly this worker's content
					got, err := s.GetTurn(ctx, id, turnID)
					if err != nil {
						opErrs <- fmt.Errorf("worker %d get %d: %w", w, i, err)
						return
					}
					if got.Query.Text != fmt.Sprintf("q-%d-%d", w, i) {
						opErrs <- fmt.Errorf("worker %d: context bleed on %q: query=%q", w, turnID, got.Query.Text)
						return
					}
				}
				// Every iteration also advances the shared checkpoint
				// (monotonic) and reads a newest page (mixed read).
				if err := s.SaveCheckpoint(ctx, id, uint64(w*perWorker+i)); err != nil {
					opErrs <- fmt.Errorf("worker %d checkpoint %d: %w", w, i, err)
					return
				}
				if _, _, _, err := s.ListTurns(ctx, id, nil, 7); err != nil {
					opErrs <- fmt.Errorf("worker %d list %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-opErrs:
		t.Fatalf("mixed op: %v", err)
	default:
	}

	// Checkpoint converges to the maximum saved value (monotonic under
	// concurrency — never a regression).
	seq, err := s.LoadCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if want := uint64(workers*perWorker - 1); seq != want {
		t.Errorf("checkpoint=%d, want %d (concurrent advances converge to the max)", seq, want)
	}

	// Every row landed exactly once, newest-first, with only its own
	// writer's content (no bleed, no skips, no duplicates).
	walked := 0
	seen := map[string]bool{}
	var cursor *turns.Cursor
	for {
		rows, next, info, err := s.ListTurns(ctx, id, cursor, 50)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		for _, row := range rows {
			walked++
			if seen[string(row.TurnID)] {
				t.Errorf("duplicate %q in walk (no duplicates)", row.TurnID)
			}
			seen[string(row.TurnID)] = true
			var w, i int
			if _, err := fmt.Sscanf(string(row.TurnID), "w%d-i%d", &w, &i); err != nil {
				t.Errorf("unexpected turn id %q: %v", row.TurnID, err)
				continue
			}
			if row.Query.Text != fmt.Sprintf("q-%d-%d", w, i) {
				t.Errorf("turn %q: context bleed, query=%q", row.TurnID, row.Query.Text)
			}
			switch i % 3 {
			case 0:
				if row.Answer.Inline != fmt.Sprintf("a-%d-%d", w, i) || row.Version != 2 {
					t.Errorf("turn %q: update lost, answer=%q version=%d", row.TurnID, row.Answer.Inline, row.Version)
				}
			case 1:
				if !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != fmt.Sprintf("final-%d-%d", w, i) {
					t.Errorf("turn %q: seal lost, sealed=%v status=%q answer=%q", row.TurnID, row.Sealed, row.Status, row.Answer.Inline)
				}
			case 2:
				// The driver is a transport: the zero Answer fed to
				// append round-trips as-is — nothing is invented.
				if row.Answer.State != "" {
					t.Errorf("turn %q: driver invented answer state %q for an unfed answer", row.TurnID, row.Answer.State)
				}
			}
		}
		if info.CountExact != true {
			t.Errorf("walk page: CountExact=%v, want true (the driver knows the exact remaining count)", info.CountExact)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if walked != workers*perWorker {
		t.Fatalf("walked %d rows, want %d (no skips)", walked, workers*perWorker)
	}

	// Goroutine baseline restored (the driver spawns no goroutines and
	// every test goroutine is joined above).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, got)
	}
}

// TestInMem_RetentionEviction_TruncationAndCursorExpiry pins the
// driver-specific retention contract: beyond the configured bound the
// oldest rows are evicted (GetTurn fails with ErrTurnNotFound), the
// session's truncation flag is set and surfaced on every page, and a
// cursor whose boundary row is later evicted is refused with
// ErrCursorExpired — the honest no-longer-retained position, never a
// silent re-keyset.
func TestInMem_RetentionEviction_TruncationAndCursorExpiry(t *testing.T) {
	const retain = 3
	s, err := inmem.New(inmem.WithRetention(retain))
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-ret", UserID: "user-ret", SessionID: "session-ret"}

	for i := 1; i <= 5; i++ {
		if _, err := s.AppendTurnIf(ctx, id, turns.TurnRow{
			TurnID:    turns.TurnID(fmt.Sprintf("r%d", i)),
			SessionID: id.SessionID,
			Status:    turns.StatusRunning,
			Query:     turns.Query{Text: fmt.Sprintf("q%d", i), Complete: turns.CompletenessComplete},
		}); err != nil {
			t.Fatalf("append r%d: %v", i, err)
		}
	}
	// The two oldest rows (r1, r2) are evicted; the newest three are
	// retained.
	for _, evicted := range []string{"r1", "r2"} {
		if _, err := s.GetTurn(ctx, id, turns.TurnID(evicted)); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("GetTurn(%q) after eviction error=%v, want ErrTurnNotFound", evicted, err)
		}
	}
	if _, err := s.GetTurn(ctx, id, "r3"); err != nil {
		t.Errorf("GetTurn(r3) error=%v, want nil (the newest rows survive eviction)", err)
	}

	rows, next, info, err := s.ListTurns(ctx, id, nil, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != retain || next != nil {
		t.Fatalf("page=%d rows next=%v, want %d rows / nil next", len(rows), next != nil, retain)
	}
	if !info.Truncated {
		t.Errorf("Truncated=false after retention eviction, want true (explicit, never silent)")
	}
	if info.CountExact != true || info.Remaining != 0 {
		t.Errorf("info Remaining=%d CountExact=%v, want 0/true", info.Remaining, info.CountExact)
	}
	for i, want := range []string{"r5", "r4", "r3"} {
		if string(rows[i].TurnID) != want {
			t.Errorf("page row %d=%q, want %q (newest-first)", i, rows[i].TurnID, want)
		}
	}

	// A cursor minted on a retained boundary row is stable while the
	// boundary survives; once the boundary is evicted by later appends,
	// the cursor is refused with ErrCursorExpired (never a silent
	// re-keyset that would skip or repeat rows).
	page, cursor, _, err := s.ListTurns(ctx, id, nil, 1)
	if err != nil || len(page) != 1 || cursor == nil {
		t.Fatalf("page 1: %d rows cursor=%v err=%v, want 1/non-nil/nil", len(page), cursor != nil, err)
	}
	if string(page[0].TurnID) != "r5" {
		t.Fatalf("page 1 row=%q, want r5", page[0].TurnID)
	}
	// Appending three more rows evicts r5 (the cursor's boundary).
	for i := 6; i <= 8; i++ {
		if _, err := s.AppendTurnIf(ctx, id, turns.TurnRow{
			TurnID:    turns.TurnID(fmt.Sprintf("r%d", i)),
			SessionID: id.SessionID,
			Status:    turns.StatusRunning,
			Query:     turns.Query{Text: fmt.Sprintf("q%d", i), Complete: turns.CompletenessComplete},
		}); err != nil {
			t.Fatalf("append r%d: %v", i, err)
		}
	}
	if _, _, _, err := s.ListTurns(ctx, id, cursor, 1); !errors.Is(err, turns.ErrCursorExpired) {
		t.Errorf("expired-boundary cursor error=%v, want ErrCursorExpired", err)
	}
	// The retained window is the newest three rows, and the truncation
	// flag remains set.
	rows, _, info, err = s.ListTurns(ctx, id, nil, 10)
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	if len(rows) != retain || !info.Truncated {
		t.Errorf("re-list: %d rows truncated=%v, want %d/true", len(rows), info.Truncated, retain)
	}
	for i, want := range []string{"r8", "r7", "r6"} {
		if string(rows[i].TurnID) != want {
			t.Errorf("re-list row %d=%q, want %q", i, rows[i].TurnID, want)
		}
	}
}

// TestInMem_CancelledContext_FailsLoud_AndStoreSurvives pins the
// cancellation contract: an operation on an already-cancelled context
// fails loudly (no silent no-op), and the failure is scoped to that
// call — the shared store keeps serving sibling callers (no
// cancellation cross-talk).
func TestInMem_CancelledContext_FailsLoud_AndStoreSurvives(t *testing.T) {
	s, err := inmem.New()
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	id := identity.Identity{TenantID: "tenant-cancel", UserID: "user-cancel", SessionID: "session-cancel"}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.AppendTurnIf(cancelCtx, id, turns.TurnRow{TurnID: "r", SessionID: id.SessionID, Status: turns.StatusRunning}); err == nil {
		t.Errorf("append on cancelled ctx succeeded, want a loud failure")
	}
	if _, _, _, err := s.ListTurns(cancelCtx, id, nil, 5); err == nil {
		t.Errorf("list on cancelled ctx succeeded, want a loud failure")
	}
	// The cancelled callers never poisoned the store or the healthy
	// path: a fresh append on a live context lands and reads back.
	row, err := s.AppendTurnIf(context.Background(), id, turns.TurnRow{TurnID: "r", SessionID: id.SessionID, Status: turns.StatusRunning})
	if err != nil {
		t.Fatalf("healthy append after cancelled calls: %v", err)
	}
	if row.Sequence != 1 {
		t.Errorf("healthy append sequence=%d, want 1 (the cancelled append never landed)", row.Sequence)
	}
	if got, err := s.GetTurn(context.Background(), id, "r"); err != nil || got.TurnID != "r" {
		t.Errorf("healthy get after cancelled calls = (%v, %v), want (r, nil)", got.TurnID, err)
	}
}
