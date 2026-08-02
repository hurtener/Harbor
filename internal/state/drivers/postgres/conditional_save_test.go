package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestPostgres_SaveIf_TwoClientsOneWinner exercises two independent database
// clients against one schema. It is environment-gated with the rest of the
// Postgres suite, but CI's HARBOR_PG_DSN makes absent-row predicate races real.
func TestPostgres_SaveIf_TwoClientsOneWinner(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	left, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open left: %v", err)
	}
	t.Cleanup(func() { _ = left.Close(context.Background()) })
	right, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open right: %v", err)
	}
	t.Cleanup(func() { _ = right.Close(context.Background()) })

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := state.StateRecord{ID: "01HABXXX00000000P0", Identity: q, Kind: "conditional.two-client", Bytes: []byte("base")}
	if err := left.Save(context.Background(), base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const workers = 128
	var winners atomic.Int64
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := left
			if i%2 == 1 {
				client = right
			}
			next := state.StateRecord{ID: state.EventID(fmt.Sprintf("01HABPG%019d", i)), Identity: q, Kind: base.Kind, Bytes: []byte(fmt.Sprintf("winner-%d", i))}
			err := client.SaveIf(context.Background(), []state.SlotExpectation{{Identity: q, Kind: base.Kind, ExpectedEventID: base.ID}}, next)
			if err == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(err, state.ErrConditionFailed) {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("two-client SaveIf error = %v, want ErrConditionFailed", err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("two-client winners = %d, want 1", got)
	}
}

func TestPostgres_DeleteIf_TwoClientsCASWinnerSurvivesReopen(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	left, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open left: %v", err)
	}
	right, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open right: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := state.StateRecord{ID: "01HABXXX00000000PD", Identity: q, Kind: "conditional.delete.two-client", Bytes: []byte("candidate")}
	next := state.StateRecord{ID: "01HABXXX00000000PW", Identity: q, Kind: base.Kind, Bytes: []byte("winner")}
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
	reopened, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
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
