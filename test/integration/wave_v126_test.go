// wave_v126_test.go verifies the released agent authority and lifecycle wave
// as one boundary-level checkpoint. Established fixtures own the detailed
// subsystem assertions; this file composes their real transports and drivers.
package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
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
	t.Run("retirement_inflight_drain", testE2EWaveV126RetirementInflightDrain)
	t.Run("retirement_cleanup_restart", testE2EWaveV126SQLiteRetirementRestart)
	t.Run("isolation", TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak)
	t.Run("postgres", testE2EWaveV126Postgres)
}

type waveV126DrainDetacher struct{ calls atomic.Int64 }

func (d *waveV126DrainDetacher) DetachConnection(context.Context, string, string, string) error {
	d.calls.Add(1)
	return nil
}

// testE2EWaveV126RetirementInflightDrain composes the process-local gate shared
// by run admission and retirement. The held lease is a pre-tombstone immutable
// run snapshot: cleanup cannot overtake it and later admissions fail closed.
func testE2EWaveV126RetirementInflightDrain(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	dsn := filepath.Join(t.TempDir(), "wave-v126-retirement-drain.db")
	store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(context.Background()) }()

	const tenant, agentID = "wave-v126-drain", "retirement-agent"
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "control"}}
	revision, err := reg.SetRevision(ctx, admin, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
			Name: "held-run-mcp", Transport: agentcfg.MCPTransportHTTP, URL: "https://drain.example.test/mcp",
		}}},
	}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gate := runsnapshot.NewGate()
	heldRun, err := gate.Acquire(ctx, tenant, agentID)
	if err != nil {
		t.Fatal(err)
	}
	detacher := &waveV126DrainDetacher{}
	service, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithConnectionDetacher(detacher),
		agentcfgprotocol.WithRunSnapshotGate(gate))
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.AgentConfigRetireRequest{
		Identity: prototypes.IdentityScope{Tenant: tenant, User: admin.UserID, Session: admin.SessionID},
		AgentID:  agentID, OperationID: "wave-v126-held-run", ExpectedContentHash: revision.ContentHash,
	}
	type retireResult struct {
		response prototypes.AgentConfigRetireResponse
		err      error
	}
	finished := make(chan retireResult, 1)
	go func() {
		response, retireErr := service.Retire(ctx, request)
		finished <- retireResult{response: response, err: retireErr}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		status, found, statusErr := reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, admin, agentID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if found {
			if status.OperationID != request.OperationID || status.Completed {
				t.Fatalf("tombstone while run held=%+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retirement did not durably tombstone while the run was held")
		}
		runtime.Gosched()
	}
	if detacher.calls.Load() != 0 {
		t.Fatalf("cleanup overtook held run: calls=%d", detacher.calls.Load())
	}
	for {
		probe, admissionErr := gate.Acquire(ctx, tenant, agentID)
		if errors.Is(admissionErr, runsnapshot.ErrAdmissionClosed) {
			break
		}
		if admissionErr != nil {
			t.Fatalf("new admission probe=%v, want ErrAdmissionClosed", admissionErr)
		}
		// Retire durably tombstones before it seals the in-process gate. A
		// probe in that narrow handoff is not a usable run (the lifecycle
		// resolver refuses it); release it and wait for the terminal seal.
		probe.Release()
		if time.Now().After(deadline) {
			t.Fatal("retirement did not seal new admissions")
		}
		runtime.Gosched()
	}
	select {
	case result := <-finished:
		t.Fatalf("retirement completed before drain: %+v", result)
	default:
	}
	heldRun.Release()
	select {
	case result := <-finished:
		if result.err != nil || !result.response.Status.Completed {
			t.Fatalf("retirement after drain=(%+v,%v)", result.response, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not resume after held run released")
	}
	if detacher.calls.Load() != 1 {
		t.Fatalf("cleanup calls=%d, want exactly one", detacher.calls.Load())
	}
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
	if os.Getenv("HARBOR_PG_DSN") == "" {
		t.Skip("HARBOR_PG_DSN not set; Postgres checkpoint is CI-gated")
	}
	t.Run("retirement_config_user_rollback_second_retirement", testE2EWaveV126PostgresAuthorityRaces)
	t.Run("retirement_signed_registration_removal", testE2EWaveV126PostgresSignedPairRaces)
	t.Run("erasure_overlay_personal", testE2EWaveV126PostgresErasureRaces)
}

func testE2EWaveV126PostgresAuthorityRaces(t *testing.T) {
	dsn := waveV126PostgresSchemaDSN(t)
	ctx := t.Context()
	first := newWaveV126PostgresRuntime(t, dsn)
	second := newWaveV126PostgresRuntime(t, dsn)
	defer first.Close()
	defer second.Close()

	type opponent func(context.Context, agentcfg.Registry, identity.Quadruple, string, agentcfg.Revision, agentcfg.Revision) error
	cases := []struct {
		name     string
		opponent opponent
	}{
		{name: "agent_config", opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, current agentcfg.Revision) error {
			_, err := reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"config-winner"}}},
				agentcfg.SetOptions{ExpectedContentHash: current.ContentHash})
			return err
		}},
		{name: "user_config", opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, _ agentcfg.Revision) error {
			_, err := reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"user-winner"}}}, agentcfg.SetOptions{})
			return err
		}},
		{name: "rollback", opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, prior, current agentcfg.Revision) error {
			_, err := reg.Rollback(ctx, id, agentID, prior.RevisionID, agentcfg.ConfigScopeAgent,
				agentcfg.SetOptions{ExpectedContentHash: current.ContentHash})
			return err
		}},
		{name: "second_retirement", opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, current agentcfg.Revision) error {
			_, err := reg.(agentcfg.RetirementRegistry).Retire(ctx, id, agentID, agentcfg.RetirementRequest{
				OperationID: "competing-retirement-" + agentID, ExpectedContentHash: current.ContentHash,
			})
			return err
		}},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentID := fmt.Sprintf("authority-race-%d", index)
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "wave-v126-pg-authority", UserID: "admin", SessionID: "control"}}
			prior, err := first.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"prior"}}}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			current, err := first.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"current"}}},
				agentcfg.SetOptions{ExpectedContentHash: prior.ContentHash})
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			go func() {
				<-start
				_, retireErr := first.reg.(agentcfg.RetirementRegistry).Retire(ctx, id, agentID, agentcfg.RetirementRequest{
					OperationID: "primary-retirement-" + agentID, ExpectedContentHash: current.ContentHash,
				})
				results <- retireErr
			}()
			go func() {
				<-start
				results <- tc.opponent(ctx, second.reg, id, agentID, prior, current)
			}()
			close(start)
			got := [2]error{<-results, <-results}
			successes := 0
			for _, raceErr := range got {
				if raceErr == nil {
					successes++
				}
			}
			if got[0] != nil && got[1] != nil {
				t.Fatalf("both cross-runtime transitions failed: %v", got)
			}
			// User config publishes to its own slot while conditioning the
			// lifecycle slot, so it may validly commit immediately before the
			// tombstone. Every other opponent mutates the same lifecycle slot
			// and therefore has exactly one CAS winner.
			if tc.name != "user_config" && successes != 1 {
				t.Fatalf("%s successes=%d results=%v, want exactly one", tc.name, successes, got)
			}
			for _, raceErr := range got {
				if raceErr != nil && !errors.Is(raceErr, agentcfg.ErrRetirementConflict) &&
					!errors.Is(raceErr, agentcfg.ErrRevisionConflict) && !errors.Is(raceErr, agentcfg.ErrAgentRetired) {
					t.Fatalf("unexpected race error: %v", raceErr)
				}
			}

			status, retired, err := first.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agentID)
			if err != nil {
				t.Fatal(err)
			}
			if retired && (!status.Completed || (status.OperationID != "primary-retirement-"+agentID && status.OperationID != "competing-retirement-"+agentID)) {
				t.Fatalf("terminal race status=%+v", status)
			}
			if retired {
				if _, err := second.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
					t.Fatalf("post-tombstone user write=%v, want ErrAgentRetired", err)
				}
			} else if active, set, err := second.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent); err != nil || !set || active.ContentHash == "" {
				t.Fatalf("active winner=(%+v,%t,%v)", active, set, err)
			}
			if gotPrior, err := first.reg.Get(ctx, id, agentID, prior.RevisionID, agentcfg.ConfigScopeAgent); err != nil || gotPrior.ContentHash != prior.ContentHash {
				t.Fatalf("immutable prior history=(%+v,%v)", gotPrior, err)
			}
		})
	}

	first.Close()
	second.Close()
	restarted := newWaveV126PostgresRuntime(t, dsn)
	defer restarted.Close()
	for index := range cases {
		agentID := fmt.Sprintf("authority-race-%d", index)
		id := identity.Quadruple{Identity: identity.Identity{TenantID: "wave-v126-pg-authority", UserID: "admin", SessionID: "control"}}
		status, retired, err := restarted.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agentID)
		if err != nil {
			t.Fatal(err)
		}
		if retired {
			if !status.Completed {
				t.Fatalf("restart lost retirement completion for %s: %+v", agentID, status)
			}
		} else if active, set, err := restarted.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent); err != nil || !set || active.ContentHash == "" {
			t.Fatalf("restart lost active winner for %s: (%+v,%t,%v)", agentID, active, set, err)
		}
	}
}

func waveV126PostgresSchemaDSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv("HARBOR_PG_DSN")
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "harbor_235_" + hex.EncodeToString(random[:])
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+waveV126QuoteIdentifier(schema)); err != nil {
		t.Fatalf("create Postgres schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, openErr := sql.Open("pgx", base)
		if openErr != nil {
			t.Errorf("open Postgres cleanup: %v", openErr)
			return
		}
		defer func() { _ = cleanup.Close() }()
		if _, dropErr := cleanup.ExecContext(context.Background(), "DROP SCHEMA "+waveV126QuoteIdentifier(schema)+" CASCADE"); dropErr != nil {
			t.Errorf("drop Postgres schema: %v", dropErr)
		}
	})
	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		parsed, err := url.Parse(base)
		if err == nil {
			query := parsed.Query()
			option := "-c search_path=" + schema
			if existing := query.Get("options"); existing != "" {
				option = existing + " " + option
			}
			query.Set("options", option)
			parsed.RawQuery = query.Encode()
			return parsed.String()
		}
	}
	return base + " options='-c search_path=" + schema + "'"
}

func waveV126QuoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func testE2EWaveV126PostgresSignedPairRaces(t *testing.T) {
	dsn := waveV126PostgresSchemaDSN(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*waveV126PostgresRuntime, *agentcfgprotocol.Service, *waveV126CapabilityPreparer, *waveV126CapabilityInstaller) {
		runtime := newWaveV126PostgresRuntime(t, dsn)
		prep := &waveV126CapabilityPreparer{live: make(map[string]string)}
		installer := &waveV126CapabilityInstaller{}
		service, serviceErr := agentcfgprotocol.NewService(runtime.reg,
			agentcfgprotocol.WithClock(func() time.Time { return now }),
			agentcfgprotocol.WithConnectionPreparer(prep),
			agentcfgprotocol.WithConnectionDetacher(prep),
			agentcfgprotocol.WithProviderInstaller(installer),
			agentcfgprotocol.WithSignedOAuthMCPOperationState(runtime.store),
			agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()),
			agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
				"broker": {Broker: "broker", Issuer: "issuer", Keys: waveV126KeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
			}),
		)
		if serviceErr != nil {
			runtime.Close()
			t.Fatal(serviceErr)
		}
		return runtime, service, prep, installer
	}
	first, firstService, firstPrep, firstInstaller := open()
	second, secondService, _, _ := open()
	defer first.Close()
	defer second.Close()

	t.Run("registration", func(t *testing.T) {
		const agentID = "signed-registration-race"
		scope := prototypes.IdentityScope{Tenant: "wave-v126-pg-signed", User: "operator", Session: "register"}
		id := identity.Quadruple{Identity: identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}}
		seed, err := first.reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
			agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"seed"}}}, agentcfg.SetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		request := waveV126SignedRequest(t, key, now, scope, agentID, "pg-registration-race", "registration-race")
		type result struct {
			kind     string
			response prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse
			err      error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		go func() {
			<-start
			response, registerErr := firstService.RegisterOAuthMCPCapability(ctx, request)
			results <- result{kind: "register", response: response, err: registerErr}
		}()
		go func() {
			<-start
			_, retireErr := secondService.Retire(ctx, prototypes.AgentConfigRetireRequest{
				Identity: scope, AgentID: agentID, OperationID: "retire-registration-race", ExpectedContentHash: seed.ContentHash,
			})
			results <- result{kind: "retire", err: retireErr}
		}()
		close(start)
		got := []result{<-results, <-results}
		successes := 0
		for _, item := range got {
			if item.err == nil {
				successes++
				continue
			}
			if !errors.Is(item.err, agentcfg.ErrRetirementConflict) && !errors.Is(item.err, agentcfg.ErrRevisionConflict) &&
				!errors.Is(item.err, agentcfg.ErrAgentRetired) && !errors.Is(item.err, agentcfg.ErrSignedCapabilityPending) {
				t.Fatalf("%s race error=%v", item.kind, item.err)
			}
		}
		if successes != 1 {
			t.Fatalf("registration/retirement successes=%d results=%+v, want exactly one", successes, got)
		}
		status, retired, err := first.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agentID)
		if err != nil {
			t.Fatal(err)
		}
		if retired {
			if !status.Completed || firstPrep.matches(scope.Tenant, agentID, "registration-race") {
				t.Fatalf("retirement winner left published registration: status=%+v live=%t", status, firstPrep.matches(scope.Tenant, agentID, "registration-race"))
			}
			beforeBroker, beforeDownstream := firstInstaller.counts()
			if err := firstInstaller.dispatch(ctx); err == nil {
				t.Fatal("retirement winner left signed provider usable")
			}
			if broker, downstream := firstInstaller.counts(); broker != beforeBroker || downstream != beforeDownstream {
				t.Fatal("retirement loser crossed signed use gate")
			}
		} else {
			active, set, err := second.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent)
			if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil {
				t.Fatalf("registration winner active=(%+v,%t,%v)", active, set, err)
			}
		}
	})

	t.Run("removal", func(t *testing.T) {
		const agentID = "signed-removal-race"
		scope := prototypes.IdentityScope{Tenant: "wave-v126-pg-signed", User: "operator", Session: "remove"}
		registered, err := firstService.RegisterOAuthMCPCapability(ctx,
			waveV126SignedRequest(t, key, now, scope, agentID, "pg-removal-race", "removal-race"))
		if err != nil {
			t.Fatal(err)
		}
		id := identity.Quadruple{Identity: identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}}
		active, set, err := first.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent)
		if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil {
			t.Fatalf("seed signed pair=(%+v,%t,%v)", active, set, err)
		}
		pair := active.Payload.SignedOAuthMCPPair
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, removeErr := secondService.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
				Identity: scope, AgentID: agentID, ExpectedContentHash: registered.Revision.ContentHash,
			})
			results <- removeErr
		}()
		go func() {
			<-start
			_, retireErr := firstService.Retire(ctx, prototypes.AgentConfigRetireRequest{
				Identity: scope, AgentID: agentID, OperationID: "retire-removal-race", ExpectedContentHash: registered.Revision.ContentHash,
			})
			results <- retireErr
		}()
		close(start)
		got := [2]error{<-results, <-results}
		successes := 0
		for _, raceErr := range got {
			if raceErr == nil {
				successes++
				continue
			}
			if !errors.Is(raceErr, agentcfg.ErrRetirementConflict) && !errors.Is(raceErr, agentcfg.ErrRevisionConflict) &&
				!errors.Is(raceErr, agentcfg.ErrAgentRetired) && !errors.Is(raceErr, agentcfg.ErrSignedCapabilityPending) {
				t.Fatalf("unexpected removal race error=%v", raceErr)
			}
		}
		if successes != 1 {
			t.Fatalf("removal/retirement successes=%d results=%v, want exactly one", successes, got)
		}
		operations, err := agentcfg.NewSignedOAuthMCPOperationStore(second.store)
		if err != nil {
			t.Fatal(err)
		}
		operation, err := operations.LoadForPair(ctx, scope.Tenant, pair)
		if err != nil || operation.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
			t.Fatalf("removal race operation=(%+v,%v)", operation, err)
		}
		beforeBroker, beforeDownstream := firstInstaller.counts()
		if err := firstInstaller.dispatch(ctx); err == nil {
			t.Fatal("removed/retired signed publisher remained usable")
		}
		if broker, downstream := firstInstaller.counts(); broker != beforeBroker || downstream != beforeDownstream {
			t.Fatal("removed publisher crossed durable use gate")
		}
	})

	first.Close()
	second.Close()
	restarted, _, restartedPrep, restartedInstaller := open()
	defer restarted.Close()
	for _, check := range []struct {
		agentID string
		scope   prototypes.IdentityScope
		name    string
	}{
		{agentID: "signed-registration-race", scope: prototypes.IdentityScope{Tenant: "wave-v126-pg-signed", User: "operator", Session: "register"}, name: "registration-race"},
		{agentID: "signed-removal-race", scope: prototypes.IdentityScope{Tenant: "wave-v126-pg-signed", User: "operator", Session: "remove"}, name: "removal-race"},
	} {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: check.scope.Tenant, UserID: check.scope.User, SessionID: check.scope.Session}}
		reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(restarted.reg, restarted.store, restartedPrep, restartedPrep, restartedInstaller)
		if err != nil {
			t.Fatal(err)
		}
		reconcileErr := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, check.agentID)
		status, retired, statusErr := restarted.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, q, check.agentID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if retired {
			if !status.Completed || (reconcileErr != nil && !errors.Is(reconcileErr, agentcfg.ErrAgentRetired)) || restartedPrep.matches(check.scope.Tenant, check.agentID, check.name) {
				t.Fatalf("restart retired pair status=%+v reconcile=%v live=%t", status, reconcileErr, restartedPrep.matches(check.scope.Tenant, check.agentID, check.name))
			}
			continue
		}
		active, set, activeErr := restarted.reg.Active(ctx, q, check.agentID, agentcfg.ConfigScopeAgent)
		if activeErr != nil || !set {
			t.Fatalf("restart active outcome=(%+v,%t,%v)", active, set, activeErr)
		}
		if active.Payload.SignedOAuthMCPPair == nil {
			if reconcileErr != nil || restartedPrep.matches(check.scope.Tenant, check.agentID, check.name) {
				t.Fatalf("restart removed pair reconcile=%v live=%t", reconcileErr, restartedPrep.matches(check.scope.Tenant, check.agentID, check.name))
			}
		} else if reconcileErr != nil || !restartedPrep.matches(check.scope.Tenant, check.agentID, check.name) {
			t.Fatalf("restart published pair reconcile=%v live=%t", reconcileErr, restartedPrep.matches(check.scope.Tenant, check.agentID, check.name))
		}
	}
}

type waveV126SaveIfBarrier struct {
	state.StateStore
	targetKind string
	entered    chan struct{}
	release    chan struct{}
	blocked    atomic.Bool
	once       sync.Once
}

func newWaveV126SaveIfBarrier(st state.StateStore, kind string) *waveV126SaveIfBarrier {
	return &waveV126SaveIfBarrier{StateStore: st, targetKind: kind, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *waveV126SaveIfBarrier) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == s.targetKind && s.blocked.CompareAndSwap(false, true) {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *waveV126SaveIfBarrier) unblock() { s.once.Do(func() { close(s.release) }) }

func testE2EWaveV126PostgresErasureRaces(t *testing.T) {
	dsn := waveV126PostgresSchemaDSN(t)
	ctx := t.Context()
	first := newWaveV126PostgresRuntime(t, dsn)
	second := newWaveV126PostgresRuntime(t, dsn)
	defer first.Close()
	defer second.Close()

	const tenant, agentID = "wave-v126-pg-erasure", "erasure-agent"
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "control"}}
	if _, err := first.reg.SetRevision(ctx, admin, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	neighbor := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "neighbor", SessionID: "stable"}}
	neighborOverlay, err := sessionoverlay.NewStore(first.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := neighborOverlay.SetUserPrompt(ctx, neighbor, agentID, "neighbor-prompt"); err != nil {
		t.Fatal(err)
	}
	neighborPersonal, err := sessionoverlay.NewDurableStore(first.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := neighborPersonal.SavePersonal(ctx, neighbor, agentID, sessionoverlaySkill("neighbor-skill"), "", ""); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		targetKind func(string) (string, error)
		mutate     func(state.StateStore, identity.Quadruple) error
	}{
		{
			name:       "overlay",
			targetKind: func(string) (string, error) { return sessionoverlay.LegacyOverlayKind(agentID), nil },
			mutate: func(st state.StateStore, id identity.Quadruple) error {
				overlay, err := sessionoverlay.NewStore(st, nil)
				if err != nil {
					return err
				}
				_, err = overlay.SetUserPrompt(context.Background(), id, agentID, "must-not-land")
				return err
			},
		},
		{
			name:       "personal",
			targetKind: func(name string) (string, error) { return sessionoverlay.PersonalSkillKind(agentID, name) },
			mutate: func(st state.StateStore, id identity.Quadruple) error {
				personal, err := sessionoverlay.NewDurableStore(st, nil)
				if err != nil {
					return err
				}
				_, err = personal.SavePersonal(context.Background(), id, agentID, sessionoverlaySkill("personal"), "", "")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "target-" + tc.name, SessionID: "session-" + tc.name}}
			kindName := tc.name
			if tc.name == "personal" {
				kindName = "personal"
			}
			kind, err := tc.targetKind(kindName)
			if err != nil {
				t.Fatal(err)
			}
			barrier := newWaveV126SaveIfBarrier(first.store, kind)
			defer barrier.unblock()
			result := make(chan error, 1)
			go func() { result <- tc.mutate(barrier, target) }()
			select {
			case <-barrier.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("mutation did not reach its conditional publication")
			}
			fenceQ, fenceKind, err := sessionfence.PendingSlot(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.store.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: fenceQ, Kind: fenceKind, Bytes: []byte(`{"pending":true}`)}); err != nil {
				t.Fatalf("admit erasure fence: %v", err)
			}
			barrier.unblock()
			select {
			case mutationErr := <-result:
				if !errors.Is(mutationErr, state.ErrConditionFailed) && !errors.Is(mutationErr, sessionoverlay.ErrSessionErased) {
					t.Fatalf("mutation racing erasure=%v, want conditional/erased refusal", mutationErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("fenced mutation did not return")
			}
			if _, err := second.store.Load(ctx, target, kind); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("fenced mutation left target kind %q: %v", kind, err)
			}
		})
	}

	first.Close()
	second.Close()
	restarted := newWaveV126PostgresRuntime(t, dsn)
	defer restarted.Close()
	restartedOverlay, err := sessionoverlay.NewStore(restarted.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotOverlay, set, err := restartedOverlay.Get(ctx, neighbor, agentID)
	if err != nil || !set || gotOverlay.UserPrompt != "neighbor-prompt" {
		t.Fatalf("neighbor overlay after erasure races=(%+v,%t,%v)", gotOverlay, set, err)
	}
	restartedPersonal, err := sessionoverlay.NewDurableStore(restarted.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotSkill, set, err := restartedPersonal.LoadPersonal(ctx, neighbor, agentID, "neighbor-skill")
	if err != nil || !set || gotSkill.Skill.Name != "neighbor-skill" {
		t.Fatalf("neighbor personal after erasure races=(%+v,%t,%v)", gotSkill, set, err)
	}
}
