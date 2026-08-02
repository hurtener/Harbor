package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestSQLite_SaveIf_TwoClientsOneWinner exercises independent SQLite driver
// handles over one database. Every loser must observe the winner and report
// ErrConditionFailed, never a driver-specific SQLITE_BUSY error.
func TestSQLite_SaveIf_TwoClientsOneWinner(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "conditional-save.sqlite")
	left, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open left: %v", err)
	}
	t.Cleanup(func() { _ = left.Close(context.Background()) })
	right, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open right: %v", err)
	}
	t.Cleanup(func() { _ = right.Close(context.Background()) })

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := state.StateRecord{ID: "01HABXXX00000000SL", Identity: q, Kind: "conditional.two-client", Bytes: []byte("base")}
	if err := left.Save(context.Background(), base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const workers = 128
	var winners atomic.Int64
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := left
			if i%2 == 1 {
				client = right
			}
			next := state.StateRecord{ID: state.EventID(fmt.Sprintf("01HABSQL%018d", i)), Identity: q, Kind: base.Kind, Bytes: []byte(fmt.Sprintf("winner-%d", i))}
			err := client.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: base.Kind, ExpectedEventID: base.ID}}, next)
			if err == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(err, state.ErrConditionFailed) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("two-client SaveIf error = %v, want ErrConditionFailed", err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("two-client winners = %d, want 1", got)
	}
}

func TestSQLite_DeleteIf_TwoClientsCASWinnerSurvivesReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "conditional-delete.sqlite")
	left, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open left: %v", err)
	}
	right, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open right: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := state.StateRecord{ID: "01HABXXX00000000SD", Identity: q, Kind: "conditional.delete.two-client", Bytes: []byte("candidate")}
	next := state.StateRecord{ID: "01HABXXX00000000SW", Identity: q, Kind: base.Kind, Bytes: []byte("winner")}
	if err := left.Save(context.Background(), base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	start := make(chan struct{})
	deleteResult := make(chan struct {
		changed bool
		err     error
	}, 1)
	saveResult := make(chan error, 1)
	go func() {
		<-start
		changed, err := left.DeleteIf(context.Background(), state.SlotExpectation{Identity: q, Kind: base.Kind, ExpectedEventID: base.ID})
		deleteResult <- struct {
			changed bool
			err     error
		}{changed: changed, err: err}
	}()
	go func() {
		<-start
		saveResult <- right.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: base.Kind, ExpectedEventID: base.ID}}, next)
	}()
	close(start)
	deleted, saveErr := <-deleteResult, <-saveResult
	if deleted.err != nil {
		t.Fatalf("DeleteIf: %v", deleted.err)
	}
	saved := saveErr == nil
	if saveErr != nil && !errors.Is(saveErr, state.ErrConditionFailed) {
		t.Fatalf("SaveIf: %v", saveErr)
	}
	if deleted.changed == saved {
		t.Fatalf("CAS winners: deleted=%t saved=%t, want exactly one", deleted.changed, saved)
	}
	if err := left.Close(context.Background()); err != nil {
		t.Fatalf("close left: %v", err)
	}
	if err := right.Close(context.Background()); err != nil {
		t.Fatalf("close right: %v", err)
	}
	reopened, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	got, loadErr := reopened.Load(context.Background(), q, base.Kind)
	if saved {
		if loadErr != nil || got.ID != next.ID {
			t.Fatalf("reopened winner = %+v err=%v, want %q", got, loadErr, next.ID)
		}
	} else if !errors.Is(loadErr, state.ErrNotFound) {
		t.Fatalf("reopened deleted slot = %+v err=%v, want ErrNotFound", got, loadErr)
	}
}

func TestSQLite_SaveIf_IdempotencyAndValidation(t *testing.T) {
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "conditional-validation.sqlite")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	next := state.StateRecord{ID: "01HABXXX00000000SV", Identity: q, Kind: "conditional.validation", Bytes: []byte("one"), Version: 1}
	expectations := []state.SlotExpectation{{Identity: q, Kind: next.Kind}}
	if err := s.SaveIf(context.Background(), expectations, next); err != nil {
		t.Fatalf("initial SaveIf: %v", err)
	}
	if err := s.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: next.Kind, ExpectedEventID: next.ID}}, next); err != nil {
		t.Fatalf("idempotent SaveIf: %v", err)
	}
	conflictingID := next
	conflictingID.Bytes = []byte("different")
	if err := s.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: next.Kind, ExpectedEventID: next.ID}}, conflictingID); !errors.Is(err, state.ErrIdempotencyConflict) {
		t.Fatalf("conflicting idempotent SaveIf = %v, want ErrIdempotencyConflict", err)
	}
	if err := s.SaveIf(context.Background(), nil, next); !errors.Is(err, state.ErrInvalidRecord) {
		t.Fatalf("empty predicates = %v, want ErrInvalidRecord", err)
	}
	if err := s.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: "other"}}, next); !errors.Is(err, state.ErrInvalidRecord) {
		t.Fatalf("unconditioned next slot = %v, want ErrInvalidRecord", err)
	}
}
