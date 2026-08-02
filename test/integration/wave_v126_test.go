// wave_v126_test.go verifies the released agent authority and lifecycle wave
// as one boundary-level checkpoint. Established fixtures own the detailed
// subsystem assertions; this file composes their real transports and drivers.
package integration_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestE2E_WaveV126 is the v1.26 checkpoint. The non-Postgres legs reuse the
// existing end-to-end fixtures through named subtests. The Postgres leg opens
// two independent StateStore/Registry pairs, races retirement admission, and
// proves a fresh runtime retains the terminal lifecycle and immutable history.
func TestE2E_WaveV126(t *testing.T) {
	t.Run("reach", TestE2E_AgentReach_AuthenticatedMuxMatrix)
	t.Run("cas", TestE2E_AgentConfig_ConditionalWrite)
	t.Run("erasure", TestE2E_Phase130_SessionErasure)
	t.Run("isolation", TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak)
	t.Run("postgres", testE2EWaveV126Postgres)
}

type waveV126PostgresRuntime struct {
	store state.StateStore
	bus   interface{ Close(context.Context) error }
	reg   agentcfg.Registry
}

func newWaveV126PostgresRuntime(t *testing.T, dsn string) *waveV126PostgresRuntime {
	t.Helper()
	store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open Postgres state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("open event bus: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("open agent-config registry: %v", err)
	}
	return &waveV126PostgresRuntime{store: store, bus: bus, reg: reg}
}

func (r *waveV126PostgresRuntime) Close() {
	ctx := context.Background()
	_ = r.reg.Close(ctx)
	_ = r.bus.Close(ctx)
	_ = r.store.Close(ctx)
}

func testE2EWaveV126Postgres(t *testing.T) {
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_PG_DSN not set; Postgres checkpoint is CI-gated")
	}

	ctx := context.Background()
	first := newWaveV126PostgresRuntime(t, dsn)
	second := newWaveV126PostgresRuntime(t, dsn)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "wave-v126-postgres", UserID: "checkpoint-admin", SessionID: "checkpoint-control",
	}}
	const agentID = "wave-v126-retirement-agent"
	seed, err := first.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"checkpoint"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed agent revision: %v", err)
	}

	retirement, ok := first.reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatal("agent-config registry omits mandatory retirement capability")
	}
	secondRetirement, ok := second.reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatal("second agent-config registry omits mandatory retirement capability")
	}

	const callers = 10
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := retirement
			if i%2 == 1 {
				r = secondRetirement
			}
			_, err := r.Retire(ctx, id, agentID, agentcfg.RetirementRequest{
				OperationID: "wave-v126-postgres-retirement", ExpectedContentHash: seed.ContentHash,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("two-runtime retirement retry: %v", err)
		}
	}

	if _, err := second.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("post-tombstone user write=%v, want ErrAgentRetired", err)
	}
	if _, _, err := second.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("post-tombstone active=%v, want ErrAgentRetired", err)
	}

	first.Close()
	second.Close()
	restarted := newWaveV126PostgresRuntime(t, dsn)
	t.Cleanup(restarted.Close)
	status, found, err := restarted.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agentID)
	if err != nil || !found || !status.Completed || status.OperationID != "wave-v126-postgres-retirement" {
		t.Fatalf("restarted retirement status=(%+v,%t,%v)", status, found, err)
	}
	if got, err := restarted.reg.Get(ctx, id, agentID, seed.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != seed.ContentHash {
		t.Fatalf("restarted immutable history=(%+v,%v), want seed revision", got, err)
	}
}
