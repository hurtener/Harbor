package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

type postgresRetirementExactDetacher struct{}

func (postgresRetirementExactDetacher) DetachConnection(context.Context, string, string, string) error {
	return nil
}
func (postgresRetirementExactDetacher) DetachExactConnection(context.Context, string, string, string, string) error {
	return nil
}
func (postgresRetirementExactDetacher) BeginExactConnectionTeardown(string, string, string, string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	return postgresRetirementExactFence{}, nil
}

type postgresRetirementExactFence struct{}

func (postgresRetirementExactFence) Seal()                        {}
func (postgresRetirementExactFence) Cancel(context.Context) error { return nil }

// TestPostgres_AgentConfigRetirement_TwoRegistriesN100 is deliberately kept
// with the disposable-schema Postgres suite: HARBOR_PG_DSN makes this
// non-vacuous in CI while developer machines without Postgres skip honestly.
func TestPostgres_AgentConfigRetirement_TwoRegistriesN100(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchema(t, requireDSN(t))
	open := func() agentcfg.Registry {
		st, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("open state: %v", err)
		}
		bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
		if err != nil {
			t.Fatalf("open bus: %v", err)
		}
		reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
		if err != nil {
			t.Fatalf("open registry: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close(ctx); _ = bus.Close(ctx); _ = st.Close(ctx) })
		return reg
	}
	a, b := open(), open()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "pg-retire", UserID: "admin", SessionID: "control"}}
	rev, err := a.SetRevision(ctx, id, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"seed"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			_, err := r.(agentcfg.RetirementRegistry).Retire(ctx, id, "agent", agentcfg.RetirementRequest{OperationID: "pg-op", ExpectedContentHash: rev.ContentHash})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("retire race: %v", err)
		}
	}
	status, ok, err := a.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, "agent")
	if err != nil || !ok || status.OperationID != "pg-op" || !status.Completed {
		t.Fatalf("status=(%+v,%v,%v)", status, ok, err)
	}
}

// TestPostgres_AgentConfigRetirement_RestartRetainsTerminalLifecycle closes
// every first-process dependency before opening fresh StateStore, EventBus,
// and Registry instances over the same schema. CI supplies HARBOR_PG_DSN, so
// this is non-vacuous wherever Postgres is a supported production driver.
func TestPostgres_AgentConfigRetirement_RestartRetainsTerminalLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchema(t, requireDSN(t))
	open := func() (agentcfg.Registry, func()) {
		st, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("open state: %v", err)
		}
		bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
		if err != nil {
			_ = st.Close(ctx)
			t.Fatalf("open bus: %v", err)
		}
		reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
		if err != nil {
			_ = bus.Close(ctx)
			_ = st.Close(ctx)
			t.Fatalf("open registry: %v", err)
		}
		return reg, func() {
			_ = reg.Close(ctx)
			_ = bus.Close(ctx)
			_ = st.Close(ctx)
		}
	}

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "pg-restart", UserID: "admin", SessionID: "control"}}
	const agent = "agent-restart"
	first, closeFirst := open()
	revision, err := first.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"seed"}}}, agentcfg.SetOptions{})
	if err != nil {
		closeFirst()
		t.Fatalf("seed: %v", err)
	}
	if _, err := first.(agentcfg.RetirementRegistry).Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "pg-restart-op", ExpectedContentHash: revision.ContentHash}); err != nil {
		closeFirst()
		t.Fatalf("retire: %v", err)
	}
	closeFirst()

	second, closeSecond := open()
	defer closeSecond()
	status, found, err := second.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agent)
	if err != nil || !found || !status.Completed || status.OperationID != "pg-restart-op" {
		t.Fatalf("post-restart status=(%+v,%v,%v)", status, found, err)
	}
	if _, _, err := second.Active(ctx, id, agent, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("post-restart active = %v, want ErrAgentRetired", err)
	}
	if got, err := second.Get(ctx, id, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
		t.Fatalf("post-restart history=(%+v,%v), want retained revision", got, err)
	}
}

// TestPostgres_AgentConfigRetirement_D401PairAcrossTwoRuntimes proves the
// hash-only retirement discovery and private exact cleanup adapter share the
// same Postgres receipt graph across independently opened runtime stores.
func TestPostgres_AgentConfigRetirement_D401PairAcrossTwoRuntimes(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchema(t, requireDSN(t))
	open := func() (state.StateStore, agentcfg.Registry) {
		st, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("open state: %v", err)
		}
		bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
		if err != nil {
			t.Fatalf("open bus: %v", err)
		}
		reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
		if err != nil {
			t.Fatalf("open registry: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close(ctx); _ = bus.Close(ctx); _ = st.Close(ctx) })
		return st, reg
	}
	firstStore, firstRegistry := open()
	secondStore, secondRegistry := open()
	const tenant = "pg-d401-retirement"
	const agent = "pg-d401-agent"
	binding := agentcfg.SignedOAuthMCPBinding{
		TenantID: tenant, UserID: "pair-owner", SessionID: "pair-session", AgentID: agent,
		Broker: "broker", ProviderName: "provider", CapabilityRevision: "v1", URLDigest: "url", SinkDigest: "sink", Audience: "audience",
		Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.invalid/mcp"},
	}
	operations, err := agentcfg.NewSignedOAuthMCPOperationStore(firstStore)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := operations.Claim(ctx, agentcfg.SignedOAuthMCPReplayKey{TenantID: tenant, TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "jti"}, binding, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	op, err = operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, "pair-revision")
	if err != nil {
		t.Fatal(err)
	}
	op, err = operations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePublished, op.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "control"}}
	revision, err := firstRegistry.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := agentcfgprotocol.NewService(secondRegistry,
		agentcfgprotocol.WithSignedOAuthMCPOperationState(secondStore),
		agentcfgprotocol.WithConnectionDetacher(postgresRetirementExactDetacher{}),
		agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()))
	if err != nil {
		t.Fatal(err)
	}
	response, err := svc.Retire(ctx, prototypes.AgentConfigRetireRequest{
		Identity: prototypes.IdentityScope{Tenant: tenant, User: admin.UserID, Session: admin.SessionID}, AgentID: agent,
		OperationID: "pg-d401-retire", ExpectedContentHash: revision.ContentHash,
	})
	if err != nil || !response.Status.Completed {
		t.Fatalf("retire=(%+v,%v)", response, err)
	}
	latest, err := operations.Load(ctx, op.ReplayKey)
	if err != nil || latest.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
		t.Fatalf("shared receipt phase=%s err=%v", latest.Phase, err)
	}
}
