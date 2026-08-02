// wave_v126_test.go verifies the released agent authority and lifecycle wave
// as one boundary-level checkpoint. Established fixtures own the detailed
// subsystem assertions; this file composes their real transports and drivers.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestE2E_WaveV126 is the v1.26 checkpoint. The non-Postgres legs reuse the
// existing end-to-end fixtures through named subtests. The Postgres leg opens
// two independent StateStore/Registry pairs, races retirement admission, and
// proves a fresh runtime retains the terminal lifecycle and immutable history.
func TestE2E_WaveV126(t *testing.T) {
	t.Run("reach", TestE2E_AgentReach_AuthenticatedMuxMatrix)
	t.Run("conditional_writes", TestE2E_AgentConfig_ConditionalWrite)
	t.Run("session_personal_resolver_cutover", func(t *testing.T) {
		// This fixture is deliberately wire-level: it performs the session
		// mutations through the real handler, then consumes the durable personal
		// authority through the planner projection. Its lower-tier scope denial
		// and concurrent session isolation make the four-slot boundary observable.
		TestE2E_AgentConfig_SessionUserSafeSubset(t)
		TestE2E_AgentConfig_SessionUser_ConcurrentIsolation(t)
	})
	t.Run("oauth_registration_restart_reconcile_removal", testE2EWaveV126SignedOAuth)
	t.Run("erasure", TestE2E_Phase130_SessionErasure)
	t.Run("retirement_cleanup_restart", testE2EWaveV126SQLiteRetirementRestart)
	t.Run("isolation", TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak)
	t.Run("postgres", testE2EWaveV126Postgres)
}

// testE2EWaveV126SQLiteRetirementRestart is intentionally a small, fresh
// driver composition rather than a call into the statestore package's unit
// suite. It creates both session-owned four-slot records, checkpoints exactly
// one retirement cleanup item, destroys the runtime, and proves the reopened
// SQLite registry resumes the frozen manifest without resurrecting either
// mutable authority or the immutable revision history.
func testE2EWaveV126SQLiteRetirementRestart(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "wave-v126-retirement.db")
	type runtime struct {
		store    state.StateStore
		bus      interface{ Close(context.Context) error }
		reg      agentcfg.Registry
		personal *sessionoverlay.DurableStore
		overlay  sessionoverlay.Store
	}
	open := func() *runtime {
		store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("open SQLite StateStore: %v", err)
		}
		bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
		if err != nil {
			_ = store.Close(ctx)
			t.Fatalf("open event bus: %v", err)
		}
		reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
		if err != nil {
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
			t.Fatalf("open registry: %v", err)
		}
		personal, err := sessionoverlay.NewDurableStore(store, nil)
		if err != nil {
			t.Fatalf("open personal store: %v", err)
		}
		overlay, err := sessionoverlay.NewStore(store, nil)
		if err != nil {
			t.Fatalf("open overlay store: %v", err)
		}
		return &runtime{store: store, bus: bus, reg: reg, personal: personal, overlay: overlay}
	}
	closeRuntime := func(r *runtime) { _ = r.reg.Close(ctx); _ = r.bus.Close(ctx); _ = r.store.Close(ctx) }

	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "wave-v126-sqlite", UserID: "admin", SessionID: "control"}}
	sessions := []identity.Quadruple{
		{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "alice", SessionID: "one"}},
		{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "bob", SessionID: "two"}},
	}
	const agentID = "wave-v126-retirement-agent"
	first := open()
	revision, err := first.reg.SetRevision(ctx, admin, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	for i, id := range sessions {
		if _, err := first.personal.SavePersonal(ctx, id, agentID, sessionoverlaySkill(fmt.Sprintf("personal-%d", i)), "", ""); err != nil {
			t.Fatalf("seed personal %d: %v", i, err)
		}
		if _, err := first.overlay.SetUserPrompt(ctx, id, agentID, fmt.Sprintf("session-%d", i)); err != nil {
			t.Fatalf("seed legacy overlay %d: %v", i, err)
		}
	}
	retirer := first.reg.(agentcfg.RetirementRegistry)
	status, err := retirer.Retire(ctx, admin, agentID, agentcfg.RetirementRequest{OperationID: "wave-v126-sqlite-retirement", ExpectedContentHash: revision.ContentHash})
	if err != nil || status.Completed || len(status.Cleanup) != 1 {
		t.Fatalf("admitted cleanup=(%+v,%v)", status, err)
	}
	step := status.Cleanup[0]
	status, err = retirer.CompleteRetirementStep(ctx, admin, agentID, status.OperationID, step.Class, step.Resource)
	if err != nil || status.Completed {
		t.Fatalf("partial cleanup=(%+v,%v), want resumable operation", status, err)
	}
	closeRuntime(first) // process-crash boundary after one persisted side effect

	second := open()
	defer closeRuntime(second)
	retirer = second.reg.(agentcfg.RetirementRegistry)
	for !status.Completed {
		status, err = retirer.Retire(ctx, admin, agentID, agentcfg.RetirementRequest{OperationID: status.OperationID, ExpectedContentHash: revision.ContentHash})
		if err != nil || len(status.Cleanup) != 1 {
			t.Fatalf("restart resume=(%+v,%v)", status, err)
		}
		step = status.Cleanup[0]
		status, err = retirer.CompleteRetirementStep(ctx, admin, agentID, status.OperationID, step.Class, step.Resource)
		if err != nil {
			t.Fatalf("complete resumed cleanup: %v", err)
		}
	}
	if _, _, err := second.reg.Active(ctx, admin, agentID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("terminal active=%v, want retired", err)
	}
	if got, err := second.reg.Get(ctx, admin, agentID, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
		t.Fatalf("immutable history=(%+v,%v)", got, err)
	}
	for i, id := range sessions {
		if _, _, err := second.personal.LoadPersonal(ctx, id, agentID, fmt.Sprintf("personal-%d", i)); err != nil && !errors.Is(err, agentcfg.ErrAgentRetired) {
			t.Fatalf("retired personal read: %v", err)
		}
		if _, _, err := second.overlay.Get(ctx, id, agentID); !errors.Is(err, agentcfg.ErrAgentRetired) {
			t.Fatalf("retired overlay read: %v", err)
		}
	}
}

func sessionoverlaySkill(name string) skills.Skill {
	return skills.Skill{Name: name, Trigger: "when needed", Steps: []string{"do it"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
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
