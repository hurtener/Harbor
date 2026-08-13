package turns

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestProjector_ConcurrentReuse_D025 is the D-025 concurrent-reuse
// gate for the projection core: N >= 100 concurrent invocations
// against ONE shared *Projector under -race, asserting the four
// guarantees:
//
//  1. No data races (the -race detector is the gate).
//  2. No context bleed — run A's identity/inputs never reach run B.
//  3. No cancellation cross-talk — cancelling run A's ctx never
//     affects run B.
//  4. No goroutine leaks — each invocation's goroutines are joined
//     before it returns; runtime.NumGoroutine returns to baseline.
func TestProjector_ConcurrentReuse_D025(t *testing.T) {
	p, st := newTestProjector(t, 0, false)
	id := tripleA()

	const workers = 16
	const invocations = 200 // total ops across workers
	baseline := runtime.NumGoroutine()

	start := make(chan struct{})
	var wg sync.WaitGroup
	opErrs := make(chan error, workers)

	// Per-worker run scopes: each worker owns its turns (no two
	// workers touch the same turn), so context bleed would be visible
	// as a foreign answer/query on the row.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < invocations/workers; i++ {
				turnID := TurnID(fmt.Sprintf("w%d-i%03d", w, i))
				row, err := p.Append(context.Background(), id, Append{TurnID: turnID, Query: fmt.Sprintf("query-%d-%d", w, i)})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d append: %w", w, err)
					return
				}
				ans := Answer{State: AnswerStateInline, Inline: fmt.Sprintf("answer-%d-%d", w, i)}
				if _, err := p.Update(context.Background(), id, turnID, row.Version, Update{Answer: &ans}); err != nil {
					opErrs <- fmt.Errorf("worker %d update: %w", w, err)
					return
				}
				got, err := p.Get(context.Background(), id, turnID)
				if err != nil {
					opErrs <- fmt.Errorf("worker %d get: %w", w, err)
					return
				}
				// Context-bleed assertion: the row must hold exactly
				// this worker's content.
				if got.Query.Text != fmt.Sprintf("query-%d-%d", w, i) || got.Answer.Inline != fmt.Sprintf("answer-%d-%d", w, i) {
					opErrs <- fmt.Errorf("worker %d: context bleed on %q: query=%q answer=%q",
						w, turnID, got.Query.Text, got.Answer.Inline)
					return
				}
			}
		}(w)
	}

	// A second cohort exercises cancellation cross-talk: cancelling
	// one worker's ctx must not abort another's in-flight op.
	var cancelWG sync.WaitGroup
	for w := 0; w < 4; w++ {
		cancelWG.Add(1)
		go func(w int) {
			defer cancelWG.Done()
			<-start
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for i := 0; i < 5; i++ {
				turnID := TurnID(fmt.Sprintf("cancel-%d-%03d", w, i))
				if i == 2 {
					cancel() // cancel this worker's scope mid-cohort
				}
				_, err := p.Append(ctx, id, Append{TurnID: turnID, Query: "q"})
				if i < 2 && err != nil {
					opErrs <- fmt.Errorf("cancel worker %d pre-cancel append: %w", w, err)
					return
				}
				// Post-cancel ops may fail with ctx cancellation — that
				// is this worker's OWN scope; it must never be visible
				// to the other cohort.
			}
		}(w)
	}

	close(start)
	wg.Wait()
	cancelWG.Wait()

	select {
	case err := <-opErrs:
		t.Fatalf("op: %v", err)
	default:
	}

	// Guarantee 4: goroutine baseline restored (no leaks).
	// The StateStore-backed test store spawns no long-lived goroutines;
	// wait briefly for any scheduler residue, then assert.
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

	// Everything the workers wrote is present exactly once: the main
	// cohort's turns plus the cancel cohort's pre-cancel turns. The
	// total exceeds MaxListLimit (50), so the check walks every page.
	mainTotal := workers * (invocations / workers)
	cancelTotal := 4 * 2 // four cancel workers, two pre-cancel appends each
	walked := seedFreeWalk(t, p, id)
	if len(walked) != mainTotal+cancelTotal {
		t.Errorf("listed %d rows, want %d (%d main + %d cancel)", len(walked), mainTotal+cancelTotal, mainTotal, cancelTotal)
	}
	_ = st
}

// TestProjector_ConcurrentReuse_ParallelCancelIsolation proves
// cancelling one caller's ctx does not cross-talk into a sibling
// caller's ops on the same shared projector: the cancel cohort's rows
// simply stop appearing, while the healthy cohort completes.
func TestProjector_ConcurrentReuse_ParallelCancelIsolation(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()

	healthy := make(chan error, 1)
	var healthyWG sync.WaitGroup
	healthyWG.Add(1)
	go func() {
		defer healthyWG.Done()
		for i := 0; i < 50; i++ {
			if _, err := appendTurn(p, id, TurnID(fmt.Sprintf("healthy-%03d", i))); err != nil {
				healthy <- err
				return
			}
		}
		healthy <- nil
	}()

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled caller's ops fail (with ctx errors or the store's
	// own cancellation handling) — but must not poison the healthy
	// caller or the shared store.
	for i := 0; i < 50; i++ {
		_, _ = p.Append(cancelCtx, id, Append{TurnID: TurnID(fmt.Sprintf("cancelled-%03d", i)), Query: "q"})
	}

	healthyWG.Wait()
	if err := <-healthy; err != nil {
		t.Fatalf("healthy cohort: %v", err)
	}
	// The cancelled caller's turns must not have landed.
	page, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range page.Rows {
		if strings.HasPrefix(string(row.TurnID), "cancelled-") {
			t.Errorf("cancelled caller's turn %q landed — cancellation cross-talk", row.TurnID)
		}
	}
	if len(page.Rows) != 50 {
		t.Errorf("listed %d rows, want 50 (healthy cohort only)", len(page.Rows))
	}
}

// TestProjector_ConcurrentReuse_UpdateSealRace exercises concurrent
// versioned mutations of the SAME turn: exactly one writer wins each
// version slot, losers fail with ErrStaleVersion, and the final sealed
// row is a consistent total order of the accepted writes.
func TestProjector_ConcurrentReuse_UpdateSealRace(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "contested")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	const writers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	versionMismatches := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			// Every writer races an update against the same base
			// version: exactly one wins, the rest fail with
			// ErrStaleVersion (a loud, typed loser — never a silent
			// overwrite).
			ans := Answer{State: AnswerStateInline, Inline: fmt.Sprintf("writer-%d", w)}
			if _, err := p.Update(context.Background(), id, "contested", row.Version, Update{Answer: &ans}); err != nil {
				if !errors.Is(err, ErrStaleVersion) {
					versionMismatches <- fmt.Errorf("writer %d: %w", w, err)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-versionMismatches:
		t.Fatalf("unexpected loser error: %v", err)
	default:
	}

	got, err := p.Get(context.Background(), id, "contested")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != row.Version+1 {
		t.Errorf("version=%d, want %d (exactly one accepted update)", got.Version, row.Version+1)
	}
}
