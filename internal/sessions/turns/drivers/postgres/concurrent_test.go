package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
)

// TestPostgres_Concurrent_AppendList_SingleSchema — supplemental
// concurrent-reuse test (D-025) that complements the conformance
// suite's `Concurrent_AppendList_NoRace`: N goroutines hammer a SINGLE
// shared driver with appends against a SINGLE session, then a full
// keyset walk must see every row exactly once (no skips, no
// duplicates, distinct sequences). This flushes out lock-ordering bugs
// in the sequence-mint upsert, connection-pool exhaustion under hot
// contention, and goroutine leaks. DSN-dependent.
func TestPostgres_Concurrent_AppendList_SingleSchema(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	baseline := runtime.NumGoroutine()

	const appenders = 16
	const perAppender = 8
	start := make(chan struct{})
	errs := make(chan error, appenders)
	var wg sync.WaitGroup
	for w := range appenders {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range perAppender {
				if _, err := s.AppendTurnIf(ctx, fixtureID, freshRow(fmt.Sprintf("w%d-%02d", w, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("append: %v", err)
	default:
	}

	// Distinct per-session sequences: no two rows share one.
	seqs := map[int64]string{}
	var walked []turns.TurnRow
	var cursor *turns.Cursor
	for {
		rows, next, _, err := s.ListTurns(ctx, fixtureID, cursor, 7)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		walked = append(walked, rows...)
		if next == nil {
			break
		}
		cursor = next
	}
	if len(walked) != appenders*perAppender {
		t.Fatalf("walked %d rows, want %d", len(walked), appenders*perAppender)
	}
	seen := map[string]bool{}
	for _, row := range walked {
		if seen[string(row.TurnID)] {
			t.Errorf("duplicate %q in walk", row.TurnID)
		}
		seen[string(row.TurnID)] = true
		if prev, dup := seqs[int64(row.Sequence)]; dup {
			t.Errorf("sequence %d shared by %q and %q — the atomic mint leaked a duplicate",
				row.Sequence, prev, row.TurnID)
		}
		seqs[int64(row.Sequence)] = string(row.TurnID)
	}

	// Goroutine-leak check with the pool's idle-connection slack.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 12 {
		t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// TestPostgres_Concurrent_ConditionalWrites_NoLostUpdate proves the
// version-guarded conditional write under contention: N goroutines
// each try to advance a single row from its current version; exactly
// one succeeds per version step and every winner observes a strictly
// increasing version (a lost update is impossible). DSN-dependent.
func TestPostgres_Concurrent_ConditionalWrites_NoLostUpdate(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	row, err := s.AppendTurnIf(ctx, fixtureID, freshRow("run-1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = row // the concurrent writers reload their own current version

	const writers = 16
	const rounds = 10
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	var ok atomic.Int64
	var stale atomic.Int64
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			current, err := s.GetTurn(ctx, fixtureID, "run-1")
			if err != nil {
				errs <- err
				return
			}
			for r := range rounds {
				next := current
				next.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: fmt.Sprintf("w%d-r%d", w, r)}
				got, err := s.UpdateTurnIf(ctx, fixtureID, "run-1", current.Version, next)
				if err != nil {
					if errors.Is(err, turns.ErrStaleVersion) {
						stale.Add(1)
						// Reload and retry against the fresh version.
						current, err = s.GetTurn(ctx, fixtureID, "run-1")
						if err != nil {
							errs <- err
							return
						}
						continue
					}
					errs <- err
					return
				}
				if got.Version != current.Version+1 {
					errs <- fmt.Errorf("version jump: got %d, want %d", got.Version, current.Version+1)
					return
				}
				ok.Add(1)
				current = got
			}
		}(w)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("writer: %v", err)
	default:
	}

	final, err := s.GetTurn(ctx, fixtureID, "run-1")
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	// Every accepted update bumped the version exactly once: the final
	// version equals 1 (append) + the number of accepted updates.
	want := final.Version
	if want != 1+int(ok.Load()) {
		t.Errorf("final version=%d but %d updates were accepted — lost or duplicate version bumps",
			want, ok.Load())
	}
	if ok.Load()+stale.Load() != writers*rounds {
		t.Errorf("total attempts=%d (ok=%d stale=%d), want %d",
			ok.Load()+stale.Load(), ok.Load(), stale.Load(), writers*rounds)
	}
}

// TestPostgres_Concurrent_MultiSessionIsolation runs N sessions
// concurrently and asserts each session's walk contains exactly its own
// rows — the identity triple is the isolation boundary even under
// contention. DSN-dependent.
func TestPostgres_Concurrent_MultiSessionIsolation(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	const sessions = 8
	const perSession = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := identity.Identity{TenantID: fmt.Sprintf("t-%d", i%3), UserID: "u-shared", SessionID: fmt.Sprintf("s-%d", i)}
			for j := range perSession {
				if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("r-%d", j))); err != nil {
					errs <- err
					return
				}
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

	for i := range sessions {
		id := identity.Identity{TenantID: fmt.Sprintf("t-%d", i%3), UserID: "u-shared", SessionID: fmt.Sprintf("s-%d", i)}
		var walked []turns.TurnRow
		var cursor *turns.Cursor
		for {
			rows, next, _, err := s.ListTurns(ctx, id, cursor, 5)
			if err != nil {
				t.Fatalf("list session %d: %v", i, err)
			}
			walked = append(walked, rows...)
			if next == nil {
				break
			}
			cursor = next
		}
		if len(walked) != perSession {
			t.Errorf("session %d walked %d rows, want %d (cross-session bleed under contention)",
				i, len(walked), perSession)
		}
		for _, r := range walked {
			if r.SessionID != id.SessionID {
				t.Errorf("session %d saw row %q with SessionID %q", i, r.TurnID, r.SessionID)
			}
		}
	}
}
