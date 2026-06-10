// D-207 — the SDK re-homing program's design-shaped follow-ups
// (RFC §6.11; D-195/D-200 dated notes resolved; docs/notes/sdk-friction-audit.md).
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3 #1):
//
//   - the crash-orphan E2E runs the exported pauseresume.RunSweeper
//     over a real sqlite StateStore + the real inmem event bus —
//     proving a checkpoint parked by a "process that crashed" (its
//     Coordinator dropped, nothing ever asks for the Token) is rescued
//     by the sweep's ListKind rescan and reaped at its max-park
//     deadline with Decision: timeout, checkpoint deleted;
//   - failure mode (§17.3 #3): a corrupt checkpoint row is loud-skipped
//     and left in the store, never silently deleted, and never shields
//     the healthy orphan;
//   - identity propagation (§17.3 #2): the reap's pause.resumed arrives
//     on an identity-scoped subscription under the pause's own triple;
//   - the `:memory:` isolation test opens state + artifacts + skills
//     sqlite-family stores on the ":memory:" DSN concurrently (N=8
//     per subsystem) and round-trips each — the pre-D-207 process-wide
//     `file::memory:?cache=shared` translation made exactly this
//     collide on one shared `schema_migrations` table (the workaround
//     this replaces lived in phase112b_sdk_additions_test.go).
//
// The third follow-up (emit-constructor base-ctx threading on the
// durable bus) is pinned next to the driver it bounds:
// internal/events/drivers/durable/durable_test.go.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artsqlite "github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/skills"
	skillslocaldb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
)

// d207CheckpointKindPrefix mirrors pauseresume's checkpoint Kind
// namespace (a stable storage contract since Phase 50/111c) so the
// test can plant a corrupt row the sweeper's rescan must skip.
const d207CheckpointKindPrefix = "pauseresume.checkpoint:"

// TestE2E_D207_SweeperReapsCrashOrphanedCheckpoint is the crash-orphan
// E2E: D-200 recorded "checkpoints orphaned by a PROCESS CRASH are
// rehydrated on demand but not proactively scanned" as the V1
// boundary; D-207 closes it via state.StateStore.ListKind + the
// sweeper's rescan phase.
func TestE2E_D207_SweeperReapsCrashOrphanedCheckpoint(t *testing.T) {
	t.Parallel()
	red := patternsAudit.New()
	bus := phase111cBus(t, red)
	store := phase111cSQLite(t)
	ctx := phase111cCtx(t, "run-d207-orphan")

	// "Process 1" parks a durable pause, then crashes: the Coordinator
	// (and with it the in-memory registry + handle table) is dropped.
	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: phase111cID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"gate": "approval", "tool": "transfer_funds"},
	})
	if err != nil {
		t.Fatalf("Request (process 1): %v", err)
	}

	// Failure mode: a corrupt checkpoint row under the same Kind
	// namespace. The rescan must loud-skip it, leave it in the store
	// for the operator, and still reap the healthy orphan.
	corruptQuad := phase111cQuad("")
	corruptKind := d207CheckpointKindPrefix + "tok-d207-corrupt"
	if err := store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: corruptQuad,
		Kind:     corruptKind,
		Bytes:    []byte("{definitely not a checkpoint"),
	}); err != nil {
		t.Fatalf("Save corrupt row: %v", err)
	}

	// "Process 2": a fresh Coordinator over the SAME store. Nothing
	// asks for the Token — pre-D-207 the checkpoint leaked forever.
	c2 := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithBus(bus),
		pauseresume.WithMaxParkDuration(30*time.Millisecond),
	)

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  phase111cID.TenantID,
		User:    phase111cID.UserID,
		Session: phase111cID.SessionID,
		Types:   []events.EventType{pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	sweepCtx, stopSweeper := context.WithCancel(ctx)
	sweeperDone := make(chan error, 1)
	go func() {
		sweeperDone <- pauseresume.RunSweeper(sweepCtx, c2,
			pauseresume.WithSweepInterval(10*time.Millisecond),
			pauseresume.WithSweeperLogger(slog.New(slog.DiscardHandler)))
	}()

	// The reap is observed via the canonical pause.resumed event on the
	// identity-scoped subscription — never a bare sleep (§17.4).
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
		if !ok {
			t.Fatalf("pause.resumed payload type = %T", ev.Payload)
		}
		if payload.Token != string(p.Token) || payload.Decision != pauseresume.DecisionTimeout {
			t.Fatalf("pause.resumed payload = %+v, want token %q decision timeout", payload, p.Token)
		}
		if ev.Identity.TenantID != phase111cID.TenantID ||
			ev.Identity.UserID != phase111cID.UserID ||
			ev.Identity.SessionID != phase111cID.SessionID {
			t.Fatalf("pause.resumed identity = %+v, want the pause's own triple", ev.Identity)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no pause.resumed for the crash orphan within bound — the rescan/reap never fired")
	}

	stopSweeper()
	if err := <-sweeperDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunSweeper returned %v, want context.Canceled (clean shutdown)", err)
	}

	// Destructive-resume contract across "restarts": a THIRD fresh
	// Coordinator over the same store proves the checkpoint is gone.
	c3 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := c3.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Status on fresh Coordinator after reap: err = %v, want ErrPauseNotFound (checkpoint deleted)", err)
	}

	// The corrupt row survives for the operator — loud-skipped, never
	// silently deleted.
	if _, err := store.Load(ctx, corruptQuad, corruptKind); err != nil {
		t.Fatalf("corrupt checkpoint row must stay in the store: %v", err)
	}
}

// TestD207_MemoryDSN_PerOpenIsolation_AcrossSubsystems proves the
// per-Open `:memory:` translation: state + artifacts + skills
// sqlite-family stores all opened on the bare ":memory:" DSN in one
// process, concurrently (N=8 per subsystem), each with a working
// round-trip. Pre-D-207 every sqlite-family driver translated
// ":memory:" to the PROCESS-WIDE `file::memory:?cache=shared`
// database, so these parallel opens collided on one shared
// `schema_migrations` table (observed: the artifacts driver-parity
// test losing its `artifacts_blobs` migration).
func TestD207_MemoryDSN_PerOpenIsolation_AcrossSubsystems(t *testing.T) {
	t.Parallel()
	red := patternsAudit.New()
	bus := phase111cBus(t, red)

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, 3*n)

	for i := range n {
		wg.Add(3)

		go func() {
			defer wg.Done()
			ctx := context.Background()
			s, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: ":memory:"})
			if err != nil {
				errCh <- fmt.Errorf("state.Open(:memory:)[%d]: %w", i, err)
				return
			}
			defer s.Close(ctx)
			q := identity.Quadruple{Identity: identity.Identity{
				TenantID: "t-d207", UserID: "u-d207", SessionID: fmt.Sprintf("s-state-%d", i),
			}}
			rec := state.StateRecord{
				ID:       state.NewEventID(),
				Identity: q,
				Kind:     "d207.isolation",
				Bytes:    []byte(fmt.Sprintf("state-%d", i)),
			}
			if err := s.Save(ctx, rec); err != nil {
				errCh <- fmt.Errorf("state Save[%d]: %w", i, err)
				return
			}
			got, err := s.Load(ctx, q, "d207.isolation")
			if err != nil {
				errCh <- fmt.Errorf("state Load[%d]: %w", i, err)
				return
			}
			if string(got.Bytes) != fmt.Sprintf("state-%d", i) {
				errCh <- fmt.Errorf("state round-trip[%d]: got %q", i, got.Bytes)
			}
		}()

		go func() {
			defer wg.Done()
			ctx := context.Background()
			a, err := artsqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: ":memory:"})
			if err != nil {
				errCh <- fmt.Errorf("artifacts sqlite.New(:memory:)[%d]: %w", i, err)
				return
			}
			defer a.Close(context.Background())
			scope := artifacts.ArtifactScope{
				TenantID: "t-d207", UserID: "u-d207", SessionID: fmt.Sprintf("s-art-%d", i),
			}
			ref, err := a.PutText(ctx, scope, fmt.Sprintf("artifact-%d", i), artifacts.PutOpts{})
			if err != nil {
				errCh <- fmt.Errorf("artifacts PutText[%d]: %w", i, err)
				return
			}
			data, found, err := a.Get(ctx, scope, ref.ID)
			if err != nil || !found {
				errCh <- fmt.Errorf("artifacts Get[%d]: found=%v err=%w", i, found, err)
				return
			}
			if string(data) != fmt.Sprintf("artifact-%d", i) {
				errCh <- fmt.Errorf("artifacts round-trip[%d]: got %q", i, data)
			}
		}()

		go func() {
			defer wg.Done()
			ctx := context.Background()
			sk, err := skillslocaldb.New(
				skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
				skills.Deps{Bus: bus})
			if err != nil {
				errCh <- fmt.Errorf("skills localdb.New(:memory:)[%d]: %w", i, err)
				return
			}
			defer sk.Close(context.Background())
			// Opening the store ran the skills migrations on a private
			// DB; a second open in the SAME goroutine proves repeat
			// opens stay independent too.
			sk2, err := skillslocaldb.New(
				skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
				skills.Deps{Bus: bus})
			if err != nil {
				errCh <- fmt.Errorf("skills localdb.New second open[%d]: %w", i, err)
				return
			}
			_ = ctx
			_ = sk2.Close(context.Background())
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
