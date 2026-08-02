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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsdurable "github.com/hurtener/Harbor/internal/events/drivers/durable"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
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
	return newWaveV126PostgresRuntimeWithStore(t, store)
}

func newWaveV126PostgresRuntimeWithStore(t *testing.T, store state.StateStore) *waveV126PostgresRuntime {
	t.Helper()
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
	ctx := t.Context()

	type opponent func(context.Context, agentcfg.Registry, identity.Quadruple, string, agentcfg.Revision, agentcfg.Revision) error
	cases := []struct {
		name       string
		targetKind string
		opponent   opponent
	}{
		{name: "agent_config", targetKind: agentcfg.ActiveSlotKind, opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, current agentcfg.Revision) error {
			_, err := reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"config-winner"}}},
				agentcfg.SetOptions{ExpectedContentHash: current.ContentHash})
			return err
		}},
		{name: "user_config", targetKind: "agentcfg.user.active", opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, _ agentcfg.Revision) error {
			_, err := reg.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"user-winner"}}}, agentcfg.SetOptions{})
			return err
		}},
		{name: "rollback", targetKind: agentcfg.ActiveSlotKind, opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, prior, current agentcfg.Revision) error {
			_, err := reg.Rollback(ctx, id, agentID, prior.RevisionID, agentcfg.ConfigScopeAgent,
				agentcfg.SetOptions{ExpectedContentHash: current.ContentHash})
			return err
		}},
		{name: "second_retirement", targetKind: agentcfg.ActiveSlotKind, opponent: func(ctx context.Context, reg agentcfg.Registry, id identity.Quadruple, agentID string, _, current agentcfg.Revision) error {
			_, err := reg.(agentcfg.RetirementRegistry).Retire(ctx, id, agentID, agentcfg.RetirementRequest{
				OperationID: "competing-retirement-" + agentID, ExpectedContentHash: current.ContentHash,
			})
			return err
		}},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := waveV126PostgresSchemaDSN(t)
			first := newWaveV126PostgresRuntime(t, dsn)
			underlying, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			barrier := newWaveV126SaveIfBarrier(underlying, tc.targetKind)
			second := newWaveV126PostgresRuntimeWithStore(t, barrier)
			defer first.Close()
			defer second.Close()
			defer barrier.unblock()
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
			result := make(chan error, 1)
			go func() {
				result <- tc.opponent(ctx, second.reg, id, agentID, prior, current)
			}()
			select {
			case <-barrier.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("competing mutation did not reach conditional publication")
			}
			status, err := first.reg.(agentcfg.RetirementRegistry).Retire(ctx, id, agentID, agentcfg.RetirementRequest{
				OperationID: "primary-retirement-" + agentID, ExpectedContentHash: current.ContentHash,
			})
			if err != nil || !status.Completed {
				t.Fatalf("primary retirement=(%+v,%v)", status, err)
			}
			barrier.unblock()
			select {
			case raceErr := <-result:
				if raceErr == nil || (!errors.Is(raceErr, agentcfg.ErrRetirementConflict) &&
					!errors.Is(raceErr, agentcfg.ErrRevisionConflict) && !errors.Is(raceErr, agentcfg.ErrAgentRetired)) {
					t.Fatalf("stale %s publication=%v, want typed retirement refusal", tc.name, raceErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("competing mutation did not return")
			}
			if gotPrior, err := first.reg.Get(ctx, id, agentID, prior.RevisionID, agentcfg.ConfigScopeAgent); err != nil || gotPrior.ContentHash != prior.ContentHash {
				t.Fatalf("immutable prior history=(%+v,%v)", gotPrior, err)
			}
			first.Close()
			second.Close()
			restarted := newWaveV126PostgresRuntime(t, dsn)
			defer restarted.Close()
			restartedStatus, retired, err := restarted.reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agentID)
			if err != nil || !retired || !restartedStatus.Completed || restartedStatus.OperationID != status.OperationID {
				t.Fatalf("restart retirement=(%+v,%t,%v)", restartedStatus, retired, err)
			}
			if _, _, err := restarted.reg.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
				t.Fatalf("restart active=%v, want ErrAgentRetired", err)
			}
		})
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
	open := func(store state.StateStore) (*waveV126PostgresRuntime, *agentcfgprotocol.Service, *waveV126CapabilityPreparer, *waveV126CapabilityInstaller) {
		var runtime *waveV126PostgresRuntime
		if store == nil {
			runtime = newWaveV126PostgresRuntime(t, dsn)
		} else {
			runtime = newWaveV126PostgresRuntimeWithStore(t, store)
		}
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
	first, firstService, _, _ := open(nil)
	second, secondService, _, _ := open(nil)
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
		underlying, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		barrier := newWaveV126SaveIfBarrier(underlying, agentcfg.ActiveSlotKind)
		registrationRuntime, registrationService, registrationPrep, _ := open(barrier)
		defer registrationRuntime.Close()
		defer barrier.unblock()
		result := make(chan error, 1)
		go func() {
			_, registerErr := registrationService.RegisterOAuthMCPCapability(ctx, request)
			result <- registerErr
		}()
		select {
		case <-barrier.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("signed registration did not reach conditional publication")
		}
		retired, err := secondService.Retire(ctx, prototypes.AgentConfigRetireRequest{
			Identity: scope, AgentID: agentID, OperationID: "retire-registration-race", ExpectedContentHash: seed.ContentHash,
		})
		if err != nil || !retired.Status.Completed {
			t.Fatalf("registration-race retirement=(%+v,%v)", retired, err)
		}
		barrier.unblock()
		select {
		case registerErr := <-result:
			if registerErr == nil || (!errors.Is(registerErr, agentcfg.ErrRevisionConflict) &&
				!errors.Is(registerErr, agentcfg.ErrAgentRetired) && !errors.Is(registerErr, agentcfg.ErrSignedCapabilityPending)) {
				t.Fatalf("stale signed registration=%v, want typed retirement refusal", registerErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("stale signed registration did not return")
		}
		if registrationPrep.matches(scope.Tenant, agentID, "registration-race") {
			t.Fatal("retirement winner left registration published")
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
		underlying, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		barrier := newWaveV126SaveIfBarrier(underlying, agentcfg.ActiveSlotKind)
		removalRuntime, removalService, _, _ := open(barrier)
		defer removalRuntime.Close()
		defer barrier.unblock()
		result := make(chan error, 1)
		go func() {
			_, removeErr := removalService.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
				Identity: scope, AgentID: agentID, ExpectedContentHash: registered.Revision.ContentHash,
			})
			result <- removeErr
		}()
		select {
		case <-barrier.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("signed removal did not reach conditional publication")
		}
		retired, err := firstService.Retire(ctx, prototypes.AgentConfigRetireRequest{
			Identity: scope, AgentID: agentID, OperationID: "retire-removal-race", ExpectedContentHash: registered.Revision.ContentHash,
		})
		if err != nil || !retired.Status.Completed {
			t.Fatalf("removal-race retirement=(%+v,%v)", retired, err)
		}
		barrier.unblock()
		select {
		case removeErr := <-result:
			if removeErr == nil || (!errors.Is(removeErr, agentcfg.ErrRevisionConflict) &&
				!errors.Is(removeErr, agentcfg.ErrAgentRetired) && !errors.Is(removeErr, agentcfg.ErrSignedCapabilityPending)) {
				t.Fatalf("stale signed removal=%v, want typed retirement refusal", removeErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("stale signed removal did not return")
		}
		operations, err := agentcfg.NewSignedOAuthMCPOperationStore(second.store)
		if err != nil {
			t.Fatal(err)
		}
		operation, err := operations.LoadForPair(ctx, scope.Tenant, pair)
		if err != nil || operation.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
			t.Fatalf("removal race operation=(%+v,%v)", operation, err)
		}
	})

	first.Close()
	second.Close()
	restarted, _, restartedPrep, restartedInstaller := open(nil)
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

func newWaveV126PostgresEraser(t *testing.T, dsn string) (*sessions.CascadeEraser, *sessions.Registry, func()) {
	t.Helper()
	ctx := context.Background()
	store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	redactor := auditpatterns.New()
	bus, err := eventsdurable.New(ctx, config.EventsConfig{
		Driver: "durable", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second, ReplayBufferSize: 64,
	}, redactor, store)
	if err != nil {
		_ = store.Close(ctx)
		t.Fatal(err)
	}
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	skillStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatal(err)
	}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: store, Memory: mem, Artifacts: arts, Skills: skillStore, Bus: bus, Redactor: redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = reg.CloseRegistry(ctx)
		_ = mem.Close(ctx)
		_ = arts.Close(ctx)
		_ = skillStore.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}
	return eraser, reg, cleanup
}

func testE2EWaveV126PostgresErasureRaces(t *testing.T) {
	dsn := waveV126PostgresSchemaDSN(t)
	ctx := t.Context()
	first := newWaveV126PostgresRuntime(t, dsn)
	eraser, sessionRegistry, closeEraser := newWaveV126PostgresEraser(t, dsn)
	defer first.Close()
	defer closeEraser()

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
		seed       func(state.StateStore, identity.Quadruple) error
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
			name:       "personal_upsert",
			targetKind: func(name string) (string, error) { return sessionoverlay.PersonalSkillKind(agentID, name) },
			mutate: func(st state.StateStore, id identity.Quadruple) error {
				personal, err := sessionoverlay.NewDurableStore(st, nil)
				if err != nil {
					return err
				}
				_, err = personal.SavePersonal(context.Background(), id, agentID, sessionoverlaySkill("personal_upsert"), "", "")
				return err
			},
		},
		{
			name:       "personal_delete",
			targetKind: func(name string) (string, error) { return sessionoverlay.PersonalSkillKind(agentID, name) },
			seed: func(st state.StateStore, id identity.Quadruple) error {
				personal, err := sessionoverlay.NewDurableStore(st, nil)
				if err != nil {
					return err
				}
				_, err = personal.SavePersonal(context.Background(), id, agentID, sessionoverlaySkill("personal_delete"), "", "")
				return err
			},
			mutate: func(st state.StateStore, id identity.Quadruple) error {
				personal, err := sessionoverlay.NewDurableStore(st, nil)
				if err != nil {
					return err
				}
				_, err = personal.DeletePersonal(context.Background(), id, agentID, "personal_delete")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "target-" + tc.name, SessionID: "session-" + tc.name}}
			if _, err := sessionRegistry.Open(ctx, target.SessionID, target.Identity); err != nil {
				t.Fatalf("open erasure target: %v", err)
			}
			if tc.seed != nil {
				if err := tc.seed(first.store, target); err != nil {
					t.Fatalf("seed mutation target: %v", err)
				}
			}
			kind, err := tc.targetKind(tc.name)
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
			response, err := eraser.Erase(ctx, target.Identity)
			if err != nil || !response.Deleted {
				t.Fatalf("production erasure=(%+v,%v)", response, err)
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
			if _, err := first.store.Load(ctx, target, kind); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("fenced mutation left target kind %q: %v", kind, err)
			}
		})
	}

	first.Close()
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
