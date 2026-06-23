package benchmarks

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// BenchmarkPauseResume_RequestCheckpoint measures the pause-checkpoint
// save path against the REAL inmem StateStore + Coordinator (no mocks —
// CLAUDE.md §13). Each iteration calls Coordinator.Request with a
// checkpoint store configured, so the pause record is minted, serialised
// and persisted — the durability hot path the unified pause/resume
// primitive runs whenever a planner emits RequestPause. The Coordinator
// and store are built once and shared; per-pause state lives in the
// record, never on the Coordinator artifact.
func BenchmarkPauseResume_RequestCheckpoint(b *testing.B) {
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		b.Fatalf("state.Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close(context.Background()) })

	coord := pauseresume.New(pauseresume.WithCheckpointStore(store))

	// Documented dummy identity triple — no secrets (CLAUDE.md §13).
	id := identity.Identity{TenantID: "bench-tenant", UserID: "bench-user", SessionID: "bench-session"}
	ctx, err := identity.WithRun(context.Background(), id, "bench-run")
	if err != nil {
		b.Fatalf("identity.WithRun: %v", err)
	}

	req := pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := coord.Request(ctx, req); err != nil {
			b.Fatalf("Request: %v", err)
		}
	}
}
