// Phase 50 — Pause/Resume Coordinator durability integration test
// (RFC §6.3 + §3.3; master-plan Phase 50 detail block; D-067).
//
// This test wires the REAL state.StateStore across all three V1
// drivers (in-mem / SQLite / Postgres) into the pauseresume.Coordinator
// — no mocks at the seam (CLAUDE.md §17.3 #1). It exercises:
//
//   - the pause → serialise → checkpoint → load → resume round-trip,
//     proving a pause survives a simulated Runtime restart when (and
//     only when) a StateStore-backed checkpoint store is configured;
//   - identity propagation — the (tenant, user, session) triple flows
//     through Request, the checkpoint envelope, and Resume's scope
//     check (CLAUDE.md §17.3 #2);
//   - two failure modes (CLAUDE.md §17.3 #3): a lost tool-context
//     handle on resume → trajectory.ErrToolContextLost; a missing
//     identity on Request → pauseresume.ErrIdentityRequired.
//
// The Postgres leg skips-with-reason when HARBOR_PG_DSN is unset —
// matching the existing state-driver integration-test convention
// (wave5_test.go). It is NOT a TODO skip.
package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// phase50ID is a documented dummy identity triple — no secrets.
var phase50ID = identity.Identity{
	TenantID:  "tenant-phase50",
	UserID:    "user-phase50",
	SessionID: "session-phase50",
}

// phase50Store opens a state.StateStore for the named driver, or
// returns ("", false-shaped t.Skip) for Postgres when no DSN is set.
type storeCase struct {
	name string
	open func(t *testing.T) state.StateStore
}

func phase50StoreCases() []storeCase {
	return []storeCase{
		{
			name: "inmem",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
				if err != nil {
					t.Fatalf("state.Open(inmem): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				dsn := filepath.Join(t.TempDir(), "phase50.sqlite")
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: dsn})
				if err != nil {
					t.Fatalf("state.Open(sqlite): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
		{
			name: "postgres",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				dsn := os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set; skipping postgres durability leg — see docs/plans/phase-16-state-postgres.md")
				}
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "postgres", DSN: dsn})
				if err != nil {
					t.Fatalf("state.Open(postgres): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
	}
}

// TestE2E_PauseResume_DurabilityAcrossDrivers proves the
// pause→serialise→checkpoint→load→resume round-trip survives a
// simulated Runtime restart, for every V1 StateStore driver.
func TestE2E_PauseResume_DurabilityAcrossDrivers(t *testing.T) {
	for _, tc := range phase50StoreCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			ctx, err := identity.WithRun(context.Background(), phase50ID, "run-durability")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}

			// A trajectory that must round-trip byte-stably through
			// the checkpoint.
			tr := &trajectory.Trajectory{
				Query: "durable pause run",
				LLMContext: map[string]any{
					"prior_summary": "user asked to provision a database",
				},
				ToolContext: trajectory.ToolContext{
					Serializable: map[string]any{"region": "eu-west-1"},
				},
			}

			// --- Coordinator #1: record a checkpointed pause. ---
			c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			p, err := c1.Request(ctx, pauseresume.PauseRequest{
				Identity:   phase50ID,
				Reason:     pauseresume.ReasonApprovalRequired,
				Payload:    map[string]any{"prompt": "approve the provision call?"},
				Trajectory: tr,
			})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if p.Token == "" {
				t.Fatal("Request returned an empty token")
			}
			if p.Identity != phase50ID {
				t.Fatalf("pause identity = %+v, want %+v", p.Identity, phase50ID)
			}

			// --- Coordinator #2: the "restarted" Runtime. ---
			c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			st, err := c2.Status(ctx, p.Token)
			if err != nil {
				t.Fatalf("Status on restarted coordinator: %v", err)
			}
			if st.State != pauseresume.StatusPaused {
				t.Fatalf("Status.State = %q, want paused (pause did not survive restart)", st.State)
			}
			if st.Reason != pauseresume.ReasonApprovalRequired {
				t.Fatalf("Status.Reason = %q, want %q", st.Reason, pauseresume.ReasonApprovalRequired)
			}

			// Resume on the restarted coordinator. Identity propagation:
			// the resume scope must match the pause's recorded triple.
			if err := c2.Resume(ctx, p.Token, pauseresume.DecisionApprove, map[string]any{"approved": true}); err != nil {
				t.Fatalf("Resume on restarted coordinator: %v", err)
			}
			st, err = c2.Status(ctx, p.Token)
			if err != nil {
				t.Fatalf("Status after resume: %v", err)
			}
			if st.State != pauseresume.StatusResumed {
				t.Fatalf("Status.State = %q after resume, want resumed", st.State)
			}

			// The checkpoint was cleared on resume — a third
			// coordinator cannot find the token.
			c3 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			if _, err := c3.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
				t.Fatalf("Status after resume cleared the checkpoint: err=%v, want ErrPauseNotFound", err)
			}
		})
	}
}

// TestE2E_PauseResume_ScopeIsolationAcrossDrivers proves a resume
// arriving under a different tenant is rejected — the identity triple
// is the isolation boundary, enforced at the resume seam.
func TestE2E_PauseResume_ScopeIsolationAcrossDrivers(t *testing.T) {
	for _, tc := range phase50StoreCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			pauseCtx, err := identity.WithRun(context.Background(), phase50ID, "run-1")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}

			c := pauseresume.New(pauseresume.WithCheckpointStore(store))
			p, err := c.Request(pauseCtx, pauseresume.PauseRequest{
				Identity: phase50ID,
				Reason:   pauseresume.ReasonAwaitInput,
			})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}

			// A different tenant attempts the resume against a
			// freshly-restarted coordinator (rehydrates from the
			// checkpoint, then enforces scope).
			intruder := identity.Identity{
				TenantID:  "tenant-intruder",
				UserID:    "user-phase50",
				SessionID: "session-phase50",
			}
			intruderCtx, err := identity.WithRun(context.Background(), intruder, "run-intruder")
			if err != nil {
				t.Fatalf("identity.WithRun(intruder): %v", err)
			}
			c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			if err := c2.Resume(intruderCtx, p.Token, pauseresume.DecisionResume, nil); !errors.Is(err, pauseresume.ErrScopeMismatch) {
				t.Fatalf("cross-tenant Resume: err=%v, want ErrScopeMismatch", err)
			}

			// The legitimate owner can still resume.
			if err := c2.Resume(pauseCtx, p.Token, pauseresume.DecisionResume, nil); err != nil {
				t.Fatalf("owner Resume after intruder rejection: %v", err)
			}
		})
	}
}

// TestE2E_PauseResume_LostHandleFailsLoud is the failure-mode leg
// (CLAUDE.md §17.3 #3): a pause whose trajectory references a
// HandleID that is not live in the resuming coordinator's
// HandleRegistry surfaces trajectory.ErrToolContextLost — never a
// silent nil tool context.
func TestE2E_PauseResume_LostHandleFailsLoud(t *testing.T) {
	store := phase50StoreCases()[1].open(t) // sqlite — real durable driver
	ctx, err := identity.WithRun(context.Background(), phase50ID, "run-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Coordinator #1 has a registry with the handle live; it
	// checkpoints a pause referencing it.
	liveReg := trajectory.NewProcessLocalRegistry()
	const handleID trajectory.HandleID = "handle-socket-1"
	liveReg.Set(handleID, "a-live-socket")
	c1 := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithHandleRegistry(liveReg),
	)
	tr := &trajectory.Trajectory{
		ToolContext: trajectory.ToolContext{
			Handles: []trajectory.HandleID{handleID},
		},
	}
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity:   phase50ID,
		Reason:     pauseresume.ReasonExternalEvent,
		Trajectory: tr,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Coordinator #2 (the "restarted" Runtime) has a FRESH registry —
	// the handle is lost. Resume must fail loud.
	freshReg := trajectory.NewProcessLocalRegistry()
	c2 := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithHandleRegistry(freshReg),
	)
	err = c2.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
	var lost trajectory.ErrToolContextLost
	if !errors.As(err, &lost) {
		t.Fatalf("Resume with lost handle: err=%v, want trajectory.ErrToolContextLost", err)
	}
	if lost.Handle != handleID {
		t.Fatalf("ErrToolContextLost.Handle = %q, want %q", lost.Handle, handleID)
	}
}

// TestE2E_PauseResume_MissingIdentityFailsClosed is the second
// failure-mode leg: a Request with a structurally-incomplete identity
// fails closed with ErrIdentityRequired — no pause is recorded, no
// checkpoint is written.
func TestE2E_PauseResume_MissingIdentityFailsClosed(t *testing.T) {
	store := phase50StoreCases()[1].open(t) // sqlite
	c := pauseresume.New(pauseresume.WithCheckpointStore(store))

	_, err := c.Request(context.Background(), pauseresume.PauseRequest{
		Identity: identity.Identity{TenantID: "tenant-only"}, // missing user + session
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if !errors.Is(err, pauseresume.ErrIdentityRequired) {
		t.Fatalf("Request with incomplete identity: err=%v, want ErrIdentityRequired", err)
	}
}
