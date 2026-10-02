package durable_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"

	// The focused restart fixture uses only the three StateStore drivers.
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestDurable_InputReceiptsRestartStorageTriad(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			cfg := config.StateConfig{Driver: driver}
			if driver == "sqlite" {
				cfg.DSN = filepath.Join(t.TempDir(), "input.db")
			}
			if driver == "postgres" {
				cfg.DSN = os.Getenv("HARBOR_PG_DSN")
				if cfg.DSN == "" {
					t.Skip("HARBOR_PG_DSN not set; Postgres restart fixture requires a test database")
				}
			}
			st, err := state.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			q := quadA()
			q.SessionID = "receipts-" + string(state.NewEventID())
			ctx := ctxFor(t, q.Identity)
			bus := mkBus(t)
			r := openOver(t, st, bus)
			spawn := func() tasks.TaskID {
				h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.MarkRunning(ctx, h.ID); err != nil {
					t.Fatal(err)
				}
				return h.ID
			}
			running := spawn()
			paused := spawn()
			sealed := spawn()
			for _, id := range []tasks.TaskID{running, paused, sealed} {
				if _, err := r.AcceptInput(ctx, id, "same-event", "exact text", 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.MarkPaused(ctx, paused); err != nil {
				t.Fatal(err)
			}
			if _, err := r.MarkInputApplied(ctx, sealed, "same-event", 1); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkComplete(ctx, sealed, tasks.TaskResult{Value: []byte(`"answer"`), IncorporatedInputRevision: 1}); err != nil {
				t.Fatal(err)
			}
			_ = r.Close(ctx)
			_ = bus.Close(ctx)
			if driver != "inmem" {
				if err = st.Close(ctx); err != nil {
					t.Fatal(err)
				}
				st, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			bus = mkBus(t)
			defer func() { _ = bus.Close(context.Background()) }()
			r = openOver(t, st, bus)
			defer func() { _ = r.Close(context.Background()) }()
			for _, id := range []tasks.TaskID{running, paused} {
				receipt, err := r.GetInputReceipt(ctx, id, "same-event")
				if err != nil || receipt.Status != tasks.InputTerminal || receipt.Revision != 1 {
					t.Fatalf("interrupted=%+v err=%v", receipt, err)
				}
				reused, err := r.AcceptInput(ctx, id, "same-event", "exact text", 0)
				if err != nil || reused.Receipt != receipt {
					t.Fatalf("restart replay=%+v err=%v", reused, err)
				}
			}
			receipt, err := r.GetInputReceipt(ctx, sealed, "same-event")
			if err != nil || receipt.Status != tasks.InputApplied {
				t.Fatalf("sealed receipt=%+v err=%v", receipt, err)
			}
			got, err := r.Get(ctx, sealed)
			if err != nil || got.Result.IncorporatedInputRevision != 1 {
				t.Fatalf("sealed provenance=%+v err=%v", got, err)
			}
			if _, err := r.AcceptInput(ctx, sealed, "same-event", "different", 0); !errors.Is(err, tasks.ErrIdempotencyConflict) {
				t.Fatalf("restart conflict=%v", err)
			}
			if _, err := r.AcceptInput(ctx, paused, "stale-after-restart", "new intent", 0); !errors.Is(err, tasks.ErrInputRevisionConflict) {
				t.Fatalf("restart stale expectation=%v", err)
			}
			if got, err := r.Get(ctx, paused); err != nil || got.Status != tasks.StatusPaused {
				t.Fatalf("restart changed pause lifecycle=%+v err=%v", got, err)
			}
		})
	}
}
