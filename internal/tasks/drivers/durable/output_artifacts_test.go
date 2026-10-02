package durable_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestDurable_OutputWitnessRestartStorageTriad(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			cfg := config.StateConfig{Driver: driver}
			if driver == "sqlite" {
				cfg.DSN = filepath.Join(t.TempDir(), "outputs.db")
			}
			if driver == "postgres" {
				cfg.DSN = os.Getenv("HARBOR_PG_DSN")
				if cfg.DSN == "" {
					t.Skip("HARBOR_PG_DSN required for PostgreSQL qualification")
				}
			}
			st, err := state.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			bus := mkBus(t)
			reg := openOver(t, st, bus)
			q := quadA()
			q.SessionID = "output-" + string(state.NewEventID())
			ctx := ctxFor(t, q.Identity)
			ctx, _ = identity.WithRun(ctx, q.Identity, "shared-first-writer")
			ids := []tasks.TaskID{}
			intents := []tasks.OutputInvocationIntent{}
			hash := ""
			for i := 0; i < 3; i++ {
				h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, h.ID)
				bound := tasks.WithOutputTask(ctx, h.ID)
				if err = reg.MarkRunning(bound, h.ID); err != nil {
					t.Fatal(err)
				}
				intent := tasks.OutputInvocationIntent{Position: 0, ToolName: "native", RequestSHA256: strings.Repeat("a", 64)}
				intents = append(intents, intent)
				inv, err := reg.BeginOutputInvocation(bound, h.ID, intent)
				if err != nil {
					t.Fatal(err)
				}
				if i > 0 {
					ref := tasks.ProducedArtifact{ID: "same-content-first-writer", SHA256: strings.Repeat("b", 64), MIMEType: "application/octet-stream", SizeBytes: 4, InvocationID: inv}
					if err = reg.FinishOutputInvocation(bound, h.ID, inv, []tasks.ProducedArtifact{ref}, true); err != nil {
						t.Fatal(err)
					}
				}
				if i == 2 {
					if err = reg.MarkComplete(bound, h.ID, tasks.TaskResult{}); err != nil {
						t.Fatal(err)
					}
					task, _ := reg.Get(bound, h.ID)
					hash = task.OutputManifest.SHA256
				}
			}
			_ = reg.Close(ctx)
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
			defer func() { _ = bus.Close(ctx) }()
			reg = openOver(t, st, bus)
			defer func() { _ = reg.Close(ctx) }()
			for i, id := range ids {
				bound := tasks.WithOutputTask(ctx, id)
				task, err := reg.Get(bound, id)
				if err != nil {
					t.Fatal(err)
				}
				if i < 2 {
					if task.Status != tasks.StatusFailed {
						t.Fatalf("interrupted task re-executable: %s", task.Status)
					}
					_, err = reg.BeginOutputInvocation(bound, id, intents[i])
					want := tasks.ErrOutputInvocationUnknown
					if i == 1 {
						want = tasks.ErrOutputInvocationSettled
					}
					if !errors.Is(err, want) {
						t.Fatalf("recovered slot%d: %v", i, err)
					}
				} else if task.OutputManifest == nil || !task.OutputManifest.Sealed || task.OutputManifest.SHA256 != hash || tasks.ValidateOutputManifest(task) != nil {
					t.Fatalf("lost seal: %+v", task.OutputManifest)
				}
			}
		})
	}
}

func TestDurable_OutputWitnessOverlappingRecoveryFencesOldWriter(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			cfg := config.StateConfig{Driver: driver}
			if driver == "sqlite" {
				cfg.DSN = filepath.Join(t.TempDir(), "overlap.db")
			}
			if driver == "postgres" {
				cfg.DSN = os.Getenv("HARBOR_PG_DSN")
				if cfg.DSN == "" {
					t.Skip("HARBOR_PG_DSN required for PostgreSQL qualification")
				}
			}
			st, err := state.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			other := st
			if driver != "inmem" {
				other, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = other.Close(context.Background()) }()
			}
			bus1, bus2 := mkBus(t), mkBus(t)
			defer func() { _ = bus1.Close(context.Background()); _ = bus2.Close(context.Background()) }()
			first := openOver(t, st, bus1)
			defer func() { _ = first.Close(context.Background()) }()
			q := quadA()
			q.SessionID = "overlap-" + string(state.NewEventID())
			ctx := ctxFor(t, q.Identity)
			ctx, _ = identity.WithRun(ctx, q.Identity, "run")
			h, err := first.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
			if err != nil {
				t.Fatal(err)
			}
			ctx = tasks.WithOutputTask(ctx, h.ID)
			if err = first.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			intent := tasks.OutputInvocationIntent{Position: 0, ToolName: "native", RequestSHA256: strings.Repeat("a", 64)}
			inv, err := first.BeginOutputInvocation(ctx, h.ID, intent)
			if err != nil {
				t.Fatal(err)
			}
			// A second registry's recovery is an exclusive-owner takeover. It retains
			// the pending historical fence but terminally invalidates the old writer.
			second := openOver(t, other, bus2)
			defer func() { _ = second.Close(context.Background()) }()
			recovered, err := second.Get(ctx, h.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Status != tasks.StatusFailed || len(recovered.OutputManifest.Invocations) != 1 || recovered.OutputManifest.Invocations[0].State != "pending" {
				t.Fatalf("recovery lost fence: %+v", recovered)
			}
			if err = first.FinishOutputInvocation(ctx, h.ID, inv, nil, true); !errors.Is(err, state.ErrConditionFailed) {
				t.Fatalf("stale capture overwrote recovery: %v", err)
			}
			if err = first.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err == nil {
				t.Fatal("old owner completed after takeover")
			}
			cached, readErr := first.Get(ctx, h.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if cached.Status == tasks.StatusComplete || cached.OutputManifest.Sealed || !cached.OutputManifest.Uncertain {
				t.Fatalf("stale cache exposes uncommitted seal: %+v", cached)
			}
			// Reopen from the underlying store, rather than trusting second's cache.
			_ = second.Close(ctx)
			third := openOver(t, other, bus2)
			defer func() { _ = third.Close(ctx) }()
			got, err := third.Get(ctx, h.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tasks.StatusFailed || got.OutputManifest.Sealed || got.OutputManifest.Invocations[0].State != "pending" {
				t.Fatalf("stale writer altered durable fence: %+v", got)
			}
		})
	}
}
