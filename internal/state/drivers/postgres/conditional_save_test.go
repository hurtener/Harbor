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
