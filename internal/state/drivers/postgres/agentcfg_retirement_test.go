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
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

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
