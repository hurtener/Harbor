package protocol

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// verifiedCtxMain seats the verified identity without touching
// *testing.T — safe from worker goroutines. Panics only when the
// triple is invalid, which cannot happen for the fixture triples the
// race tests construct.
func verifiedCtxMain(id identity.Identity) context.Context {
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		panic(err)
	}
	return ctx
}

// TestService_ConcurrentMixedIdentities_NoBleed is the focused
// authority/cursor race gate for the turns Protocol service: N >= 100
// concurrent list/get calls across MIXED identities (own-session
// consumers, foreign-session probes, admin and fleet operations
// readers, agent-gated callers) against ONE shared Service +
// projector + store under -race, with cancellation barriers and a
// final goroutine baseline. It asserts:
//
//  1. No data races (the -race detector is the gate).
//  2. No identity bleed: a consumer's list/get of its own session
//     never returns another session's rows, and a foreign-session
//     probe always gets the non-oracular typed not-found.
//  3. No cancellation cross-talk: cancelling one caller's ctx never
//     breaks a sibling caller's in-flight read.
//  4. No goroutine leaks: runtime.NumGoroutine returns to baseline.
func TestService_ConcurrentMixedIdentities_NoBleed(t *testing.T) {
	svc, st, _, _ := newTestService(t)

	// Three sessions of one user, each with content rows bound to
	// agent-a plus one unbound row.
	const sessions = 3
	for s := 0; s < sessions; s++ {
		sid := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: fmt.Sprintf("session-%d", s)}
		mustSeedRow(t, st, sid, fixtureRow("turn-a", turns.StatusComplete, "agent-a"))
		mustSeedRow(t, st, sid, fixtureRow("turn-b", turns.StatusComplete, "agent-a"))
		mustSeedRow(t, st, sid, fixtureRow("turn-c", turns.StatusComplete, ""))
	}

	const workers = 100
	baseline := runtime.NumGoroutine()
	start := make(chan struct{})
	var wg sync.WaitGroup
	opErrs := make(chan error, workers)
	cancelErr := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			s := w % sessions
			own := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: fmt.Sprintf("session-%d", s)}
			role := w % 5
			// verifiedCtxMain seats the verified identity without
			// touching *testing.T (safe from worker goroutines).
			mkCtx := func() context.Context { return verifiedCtxMain(own) }
			switch role {
			case 0: // own-session consumer list, agent reach admitted
				ctx := auth.WithAgentReach(mkCtx(), []string{"agent-a"})
				resp, err := svc.List(ctx, ListRequest{SessionID: own.SessionID, Limit: 20})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d own list: %w", w, err)
					return
				}
				if len(resp.Turns) != 3 {
					opErrs <- fmt.Errorf("worker %d own list returned %d turns, want 3 (identity bleed?)", w, len(resp.Turns))
					return
				}
				for _, r := range resp.Turns {
					if r.SessionID != own.SessionID {
						opErrs <- fmt.Errorf("worker %d list bled a row from session %q", w, r.SessionID)
						return
					}
				}
			case 1: // own-session consumer get, reach admitted
				ctx := auth.WithAgentReach(mkCtx(), []string{"agent-a"})
				resp, err := svc.Get(ctx, GetRequest{SessionID: own.SessionID, TaskID: "turn-b"})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d own get: %w", w, err)
					return
				}
				if resp.Turn.SessionID != own.SessionID || resp.Turn.TaskID != "turn-b" {
					opErrs <- fmt.Errorf("worker %d own get bled identity: %+v", w, resp.Turn)
					return
				}
			case 2: // foreign-session consumer probe: NON-ORACULAR typed not-found
				foreign := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-99"}
				foreignCtx, err := identity.WithVerified(context.Background(), foreign)
				if err != nil {
					opErrs <- fmt.Errorf("worker %d foreign ctx: %w", w, err)
					return
				}
				if _, err := svc.List(foreignCtx, ListRequest{SessionID: "session-0"}); !errIs(err, ErrTurnNotFound) {
					opErrs <- fmt.Errorf("worker %d foreign list: err = %v, want ErrTurnNotFound", w, err)
					return
				}
				if _, err := svc.Get(foreignCtx, GetRequest{SessionID: "session-0", TaskID: "turn-a"}); !errIs(err, ErrTurnNotFound) {
					opErrs <- fmt.Errorf("worker %d foreign get: err = %v, want ErrTurnNotFound", w, err)
					return
				}
			case 3: // admin operations read of a sibling session (widened, audited)
				ctx := auth.WithScopes(mkCtx(), []auth.Scope{auth.ScopeAdmin})
				sibling := (s + 1) % sessions
				resp, err := svc.Get(ctx, GetRequest{
					SessionID:  fmt.Sprintf("session-%d", sibling),
					TaskID:     "turn-a",
					Projection: ProjectionOperations,
				})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d ops get: %w", w, err)
					return
				}
				if resp.OpsTurn == nil || resp.OpsTurn.TurnID != "turn-a" {
					opErrs <- fmt.Errorf("worker %d ops get returned a malformed OpsTurn: %+v", w, resp.OpsTurn)
					return
				}
			default: // fleet operations read of the caller's OWN session (no widening)
				ctx := auth.WithScopes(mkCtx(), []auth.Scope{auth.ScopeConsoleFleet})
				resp, err := svc.Get(ctx, GetRequest{
					SessionID:  own.SessionID,
					TaskID:     "turn-c",
					Projection: ProjectionOperations,
				})
				if err != nil {
					opErrs <- fmt.Errorf("worker %d fleet own ops get: %w", w, err)
					return
				}
				if resp.OpsTurn == nil || resp.OpsTurn.TurnID != "turn-c" {
					opErrs <- fmt.Errorf("worker %d fleet own ops get malformed: %+v", w, resp.OpsTurn)
					return
				}
			}
		}(w)
	}

	// A cancellation cohort: pre-cancelled callers interleave with the
	// healthy cohort. Their failures are their OWN scope — the healthy
	// cohort must never see them.
	var cancelWG sync.WaitGroup
	for w := 0; w < 20; w++ {
		cancelWG.Add(1)
		go func(w int) {
			defer cancelWG.Done()
			<-start
			cancelCtx, cancel := context.WithCancel(verifiedCtxMain(fixtureID))
			cancel()
			if _, err := svc.List(cancelCtx, ListRequest{SessionID: "session-1"}); err != nil {
				if !errors.Is(err, context.Canceled) && !errIs(err, ErrTurnNotFound) {
					cancelErr <- fmt.Errorf("cancel worker %d: unexpected err %v", w, err)
				}
			}
		}(w)
	}

	close(start)
	wg.Wait()
	cancelWG.Wait()

	select {
	case err := <-opErrs:
		t.Fatalf("mixed-identity op: %v", err)
	default:
	}
	select {
	case err := <-cancelErr:
		t.Fatalf("cancel cohort: %v", err)
	default:
	}

	// Goroutine baseline restored (no leaks).
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

// TestService_ConcurrentPagingAndAppends_NoSkipDuplicate races
// concurrent turn appends against concurrent page walks on ONE shared
// projection: every issued cursor pages a stable, gap-free, duplicate-
// free view of the rows that existed when the cursor was minted — the
// immutable keyset guarantee holds under -race.
func TestService_ConcurrentPagingAndAppends_NoSkipDuplicate(t *testing.T) {
	svc, st, proj, _ := newTestService(t)
	id := fixtureID
	ctx := verifiedCtx(t, id)
	seedN(t, st, id, 10, turns.StatusComplete, "")

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Appender cohort: mints 200 new turns via the real projector
	// (atomic sequence minting under contention).
	const appenders = 8
	for a := 0; a < appenders; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			<-start
			for i := 0; i < 25; i++ {
				if _, err := proj.Append(ctx, id, turns.Append{
					TurnID:  turns.TurnID(fmt.Sprintf("race-%d-%03d", a, i)),
					TaskID:  fmt.Sprintf("race-%d-%03d", a, i),
					Query:   fmt.Sprintf("q-%d-%d", a, i),
					QueryAt: time.Now(),
					Status:  turns.StatusRunning,
				}); err != nil {
					t.Errorf("append %d-%d: %v", a, i, err)
					return
				}
			}
		}(a)
	}

	// Pager cohort: full walks from the newest page while appends land.
	const pagers = 8
	walkErrs := make(chan error, pagers)
	for p := 0; p < pagers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			seen := map[string]bool{}
			cur := ""
			for {
				page, err := svc.List(ctx, ListRequest{SessionID: id.SessionID, OlderCursor: cur, Limit: 7})
				if err != nil {
					walkErrs <- fmt.Errorf("pager %d page: %w", p, err)
					return
				}
				for _, r := range page.Turns {
					if seen[string(r.TurnID)] {
						walkErrs <- fmt.Errorf("pager %d duplicate %q", p, r.TurnID)
						return
					}
					seen[string(r.TurnID)] = true
				}
				if page.NextOlderCursor == "" {
					break
				}
				cur = page.NextOlderCursor
			}
			// The walk must have covered every row that existed BEFORE
			// the race started (the 10 seeded turns) exactly once; the
			// appended turns may or may not appear but never duplicate.
			for i := 0; i < 10; i++ {
				if !seen[turnIDAt(i)] {
					walkErrs <- fmt.Errorf("pager %d skipped seeded turn %q — keyset paging is not gap-free", p, turnIDAt(i))
					return
				}
			}
		}(p)
	}

	close(start)
	wg.Wait()
	select {
	case err := <-walkErrs:
		t.Fatalf("paging walk: %v", err)
	default:
	}
}
