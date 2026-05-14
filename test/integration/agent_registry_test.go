// Phase 53a cross-subsystem integration test per AGENTS.md §17.
//
// Wires the Agent Registry (internal/runtime/registry) over the REAL
// production drivers on every seam — no mocks:
//
//   - StateStore: in-mem (asserts the dev-only non-persistence posture),
//     SQLite (asserts agent_id stable across a simulated process
//     restart — the durable-driver posture), and Postgres (same, gated
//     on HARBOR_PG_DSN — skips cleanly when unset).
//   - EventBus: the real in-mem events driver — the registry's agent.*
//     events are observed end-to-end on the bus, and identity
//     propagation through the event Identity is asserted.
//   - Redactor: the real patterns audit driver.
//
// Failure mode covered (§17.3 #3): a Register call with an incomplete
// identity context fails closed (ErrIdentityRequired) before any
// storage is touched.
//
// All assertions run under -race.
package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

const phase53aPGSkip = "HARBOR_PG_DSN not set; skipping postgres leg of the Agent Registry integration test"

func phase53aEventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}
}

func phase53aSampleConfig() registry.AgentConfig {
	return registry.AgentConfig{
		Prompts:       []string{"system prompt"},
		Tools:         []registry.ToolDescriptor{{Name: "search", SchemaDigest: "d1"}},
		PlannerConfig: map[string]string{"mode": "react"},
		ModelPolicy:   map[string]string{"model": "haiku"},
	}
}

// openRegistry builds a registry over the given StateConfig with a
// fresh real EventBus + patterns redactor. Returns the registry, the
// store (so the caller can wire a "restart"), and the bus.
func openRegistry(t *testing.T, stateCfg config.StateConfig) (*registry.Registry, state.StateStore, events.EventBus) {
	t.Helper()
	store, err := state.Open(context.Background(), stateCfg)
	if err != nil {
		t.Fatalf("state.Open(%s): %v", stateCfg.Driver, err)
	}
	bus, err := events.Open(context.Background(), phase53aEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})
	return reg, store, bus
}

// TestE2E_Phase53a_Rehydration_AcrossDrivers exercises the registry's
// rehydration posture across all three StateStore drivers:
//
//   - in-mem: a fresh store after "restart" loses the registry (the
//     dev-only, non-persistent posture — D-060).
//   - sqlite / postgres: a fresh *Registry over the SAME durable DSN
//     rehydrates — the agent comes back with the SAME agent_id (the
//     intended fleet posture).
func TestE2E_Phase53a_Rehydration_AcrossDrivers(t *testing.T) {
	t.Run("inmem_non_persistent", func(t *testing.T) {
		// in-mem is process-resident: a SECOND state.Open mints a fresh
		// store, so the prior agent is gone — the documented dev-only
		// posture. (Rehydration over the SAME in-mem store is covered
		// by the package unit tests; here we assert the cold-start
		// posture the integration surface cares about.)
		cfg := config.StateConfig{Driver: "inmem"}
		reg1, _, _ := openRegistry(t, cfg)
		ctx := mustIdentity(t, "T", "U", "S")
		rec, err := reg1.Register(ctx, "agent-x", phase53aSampleConfig(), registry.RegisterOptions{})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		reg2, _, _ := openRegistry(t, cfg) // fresh in-mem store
		if _, err := reg2.Get(ctx, rec.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
			t.Fatalf("fresh in-mem store saw a prior agent (should be non-persistent): %v", err)
		}
	})

	t.Run("sqlite_durable_rehydrates", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "phase53a.db")
		dsn := "file:" + dbPath + "?cache=shared"
		cfg := config.StateConfig{Driver: "sqlite", DSN: dsn}
		assertDurableRehydration(t, cfg)
	})

	t.Run("postgres_durable_rehydrates", func(t *testing.T) {
		dsn := os.Getenv("HARBOR_PG_DSN")
		if dsn == "" {
			t.Skip(phase53aPGSkip)
		}
		cfg := config.StateConfig{Driver: "postgres", DSN: dsn}
		assertDurableRehydration(t, cfg)
	})
}

// assertDurableRehydration is the shared durable-driver assertion:
// register an agent, "restart" by opening a FRESH *Registry over the
// SAME DSN, and confirm the agent_id is stable. Also confirms a plain
// restart bumps incarnation but keeps version_hash.
func assertDurableRehydration(t *testing.T, cfg config.StateConfig) {
	t.Helper()
	ctx := mustIdentity(t, "T", "U", "S")

	// --- process 1: register. ---
	reg1, _, _ := openRegistry(t, cfg)
	first, err := reg1.Register(ctx, "agent-x", phase53aSampleConfig(), registry.RegisterOptions{DisplayName: "Agent X"})
	if err != nil {
		t.Fatalf("Register #1: %v", err)
	}
	if first.Incarnation != 1 {
		t.Fatalf("first registration incarnation = %d, want 1", first.Incarnation)
	}

	// --- process 2 ("restart"): fresh registry, SAME DSN. ---
	reg2, _, _ := openRegistry(t, cfg)
	got, err := reg2.Get(ctx, first.AgentID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.AgentID != first.AgentID {
		t.Fatalf("agent_id NOT stable across restart on a durable driver: %q -> %q", first.AgentID, got.AgentID)
	}
	if got.DisplayName != "Agent X" {
		t.Errorf("rehydrated display name = %q", got.DisplayName)
	}

	// Re-register on the restarted process: incarnation bumps, agent_id
	// + version_hash stable (plain restart, no config change).
	second, err := reg2.Register(ctx, "agent-x", phase53aSampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register #2 (restart): %v", err)
	}
	if second.AgentID != first.AgentID {
		t.Errorf("re-register changed agent_id: %q -> %q", first.AgentID, second.AgentID)
	}
	if second.VersionHash != first.VersionHash {
		t.Errorf("plain restart bumped version_hash: %q -> %q", first.VersionHash, second.VersionHash)
	}
	if second.Incarnation != 2 {
		t.Errorf("re-register incarnation = %d, want 2", second.Incarnation)
	}
}

// TestE2E_Phase53a_EventSeam_IdentityPropagation wires the registry to
// the REAL EventBus and asserts agent.* events are observed
// end-to-end carrying the registering identity triple (§17.3 #2 —
// identity propagation through every layer the test wires).
func TestE2E_Phase53a_EventSeam_IdentityPropagation(t *testing.T) {
	cfg := config.StateConfig{Driver: "inmem"}
	reg, _, bus := openRegistry(t, cfg)
	ctx := mustIdentity(t, "tenant-a", "user-a", "session-a")

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "tenant-a", User: "user-a", Session: "session-a",
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	rec, err := reg.Register(ctx, "agent", phase53aSampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ev := waitForType(t, sub, registry.EventTypeAgentRegistered)
	// Identity propagated through the bus.
	if ev.Identity.TenantID != "tenant-a" || ev.Identity.UserID != "user-a" || ev.Identity.SessionID != "session-a" {
		t.Errorf("agent.registered identity = %+v, want (tenant-a,user-a,session-a)", ev.Identity)
	}
	// agent_id propagated through the payload (RFC §6.16 "Events").
	p, ok := ev.Payload.(registry.AgentRegisteredPayload)
	if !ok {
		t.Fatalf("agent.registered payload type = %T", ev.Payload)
	}
	if p.AgentID != rec.AgentID {
		t.Errorf("agent.registered payload agent_id = %q, want %q", p.AgentID, rec.AgentID)
	}

	// A fleet-control command's event also propagates identity + the
	// audit-redacted reason.
	ctrl := registry.WithControlScope(ctx)
	if err := reg.Drain(ctrl, rec.AgentID, "rolling deploy"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	cev := waitForType(t, sub, registry.EventTypeAgentDrained)
	if cev.Identity.TenantID != "tenant-a" {
		t.Errorf("agent.drained identity tenant = %q, want tenant-a", cev.Identity.TenantID)
	}
	if cp, _ := cev.Payload.(registry.AgentControlPayload); cp.AgentID != rec.AgentID || cp.Command != "drain" {
		t.Errorf("agent.drained payload = %+v", cp)
	}
}

// TestE2E_Phase53a_FailureMode_MissingIdentityFailsClosed is the
// mandatory ≥1 failure mode (§17.3 #3): a Register with no identity in
// context fails closed BEFORE any storage is touched. We assert no
// agent landed in the store by confirming a subsequent identity-scoped
// List is empty.
func TestE2E_Phase53a_FailureMode_MissingIdentityFailsClosed(t *testing.T) {
	cfg := config.StateConfig{Driver: "inmem"}
	reg, _, _ := openRegistry(t, cfg)

	// No identity in context — fail closed.
	if _, err := reg.Register(context.Background(), "agent", phase53aSampleConfig(), registry.RegisterOptions{}); !errors.Is(err, registry.ErrIdentityRequired) {
		t.Fatalf("Register with missing identity: err=%v, want ErrIdentityRequired", err)
	}

	// Nothing was persisted: an identity-scoped List sees zero agents.
	ctx := mustIdentity(t, "T", "U", "S")
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List returned %d agents — a failed-closed Register leaked state", len(list))
	}
}

// mustIdentity builds an identity-bearing context or fails the test.
func mustIdentity(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// waitForType drains sub until it sees an event of type want, or fails
// after a bounded real-time deadline (not a synchronisation sleep —
// AGENTS.md §17.4).
func waitForType(t *testing.T, sub events.Subscription, want events.EventType) events.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before %q arrived", want)
			}
			if ev.Type == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}
