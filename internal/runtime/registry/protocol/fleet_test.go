package protocol_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// newFleetService builds a real *registry.Registry + a RegistryProjector +
// an agents/protocol.Service wired with the bus + redactor — all PRODUCTION
// drivers, no mocks at the seam (CLAUDE.md §17.3). The bus is returned so a
// test can subscribe to the admin-scope audit event.
func newFleetService(t *testing.T) (*agentsprotocol.Service, *registry.Registry, events.EventBus) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := agentsprotocol.NewService(proj,
		agentsprotocol.WithBus(bus), agentsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})
	return svc, reg, bus
}

// TestAgentsList_FleetWidening_RequiresAdmin — an `agents.list` naming
// tenants in Filter.TenantIDs is the admin-widened fleet enumeration:
// without the verified admin claim it fails LOUD with ErrScopeMismatch;
// with the claim it enumerates every agent across all sessions of the
// named tenants + emits the admin-scope audit event.
func TestAgentsList_FleetWidening_RequiresAdmin(t *testing.T) {
	svc, reg, bus := newFleetService(t)

	// Tenant A: two sessions, one agent each.
	if _, err := reg.Register(idCtx(t, "tenant-A", "u1", "s1"), "k1", sampleFleetConfig(), registry.RegisterOptions{DisplayName: "A one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(idCtx(t, "tenant-A", "u2", "s2"), "k2", sampleFleetConfig(), registry.RegisterOptions{DisplayName: "A two"}); err != nil {
		t.Fatal(err)
	}
	// Tenant B: one agent that must NEVER surface in tenant-A's fleet read.
	if _, err := reg.Register(idCtx(t, "tenant-B", "u9", "s9"), "k9", sampleFleetConfig(), registry.RegisterOptions{DisplayName: "B one"}); err != nil {
		t.Fatal(err)
	}

	observer := prototypes.IdentityScope{Tenant: "tenant-A", User: "observer", Session: "obs"}
	req := prototypes.AgentListRequest{
		Identity: observer,
		Filter:   prototypes.AgentFilter{TenantIDs: []string{"tenant-A"}},
	}

	t.Run("non-admin widened fails closed", func(t *testing.T) {
		_, err := svc.List(context.Background(), req, false)
		if !errors.Is(err, agentsprotocol.ErrScopeMismatch) {
			t.Fatalf("want ErrScopeMismatch, got %v", err)
		}
	})

	t.Run("admin widened enumerates the tenant + emits audit", func(t *testing.T) {
		ctx := context.Background()
		sub, err := bus.Subscribe(ctx, events.Filter{
			Tenant: "tenant-A", User: "observer", Session: "obs", Admin: true,
			Types: []events.EventType{events.EventTypeAdminScopeUsed},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Cancel()

		resp, err := svc.List(ctx, req, true)
		if err != nil {
			t.Fatalf("admin widened List: %v", err)
		}
		if len(resp.Agents) != 2 {
			t.Fatalf("widened count=%d, want 2 (both tenant-A sessions)", len(resp.Agents))
		}
		for _, a := range resp.Agents {
			if a.Identity.Tenant != "tenant-A" {
				t.Errorf("widened row leaked tenant %q", a.Identity.Tenant)
			}
			if a.Identity.User == "" || a.Identity.Session == "" {
				t.Errorf("widened row missing identity attribution: %+v", a.Identity)
			}
		}
		select {
		case ev := <-sub.Events():
			if ev.Type != events.EventTypeAdminScopeUsed {
				t.Fatalf("want admin_scope_used, got %q", ev.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no audit.admin_scope_used event observed within 2s")
		}
	})
}

// TestAgentsList_NonWidened_ByteCompatible — a non-widened `agents.list`
// stays identity-scoped even under adminScoped, and carries per-row
// identity attribution for the caller's own scope.
func TestAgentsList_NonWidened_ByteCompatible(t *testing.T) {
	svc, reg, _ := newFleetService(t)
	if _, err := reg.Register(idCtx(t, "t1", "u1", "s1"), "own", sampleFleetConfig(), registry.RegisterOptions{DisplayName: "own"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(idCtx(t, "t1", "u2", "s2"), "other", sampleFleetConfig(), registry.RegisterOptions{DisplayName: "other"}); err != nil {
		t.Fatal(err)
	}
	// The narrow (identity-scoped) read reads the caller's triple from ctx
	// — exactly as the wire handler folds it in before dispatch.
	resp, err := svc.List(idCtx(t, "t1", "u1", "s1"), prototypes.AgentListRequest{
		Identity: prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	}, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("non-widened count=%d, want 1 (the caller's own session)", len(resp.Agents))
	}
	if resp.Agents[0].Identity.Session != "s1" {
		t.Errorf("row identity attribution = %+v, want session s1", resp.Agents[0].Identity)
	}
}

// TestListTenantAgents_ConcurrentReuse_D025 exercises the D-025
// concurrent-reuse contract on the aggregating fleet projector: N=128
// concurrent goroutines mixing admin-widened fleet reads and narrow
// identity-scoped reads against a SINGLE shared Service under `-race`. It
// asserts no data races, no context bleed, and no goroutine leak.
func TestListTenantAgents_ConcurrentReuse_D025(t *testing.T) {
	svc, reg, _ := newFleetService(t)
	if _, err := reg.Register(idCtx(t, "tenant-A", "u1", "s1"), "k1", sampleFleetConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(idCtx(t, "tenant-A", "u2", "s2"), "k2", sampleFleetConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(idCtx(t, "tenant-B", "u1", "s1"), "k3", sampleFleetConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}

	// The narrow read reads its triple from ctx (as the handler folds it);
	// build it once so the goroutines don't call t.Fatalf.
	narrowCtx := idCtx(t, "tenant-A", "u1", "s1")

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				resp, err := svc.List(context.Background(), prototypes.AgentListRequest{
					Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "obs", Session: "obs"},
					Filter:   prototypes.AgentFilter{TenantIDs: []string{"tenant-A"}},
				}, true)
				if err != nil {
					errCh <- err
					return
				}
				for _, a := range resp.Agents {
					if a.Identity.Tenant != "tenant-A" {
						errCh <- errors.New("widened read leaked a foreign tenant")
						return
					}
				}
			} else {
				resp, err := svc.List(narrowCtx, prototypes.AgentListRequest{
					Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "u1", Session: "s1"},
				}, false)
				if err != nil {
					errCh <- err
					return
				}
				for _, a := range resp.Agents {
					if a.Identity.Session != "s1" {
						errCh <- errors.New("narrow read leaked a foreign session")
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent fleet/narrow read: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// sampleFleetConfig is a minimal AgentConfig fixture for the fleet tests.
func sampleFleetConfig() registry.AgentConfig {
	return registry.AgentConfig{
		Prompts:       []string{"you are a fleet test agent"},
		PlannerConfig: map[string]string{"type": "react"},
		ModelPolicy:   map[string]string{"model": "test-model"},
	}
}

// failingRedactor is an audit.Redactor that always errors — used to
// exercise the emitAdminAudit "redaction failed → do NOT publish" branch.
type failingRedactor struct{}

func (failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, errors.New("redactor down")
}

// TestAgentsList_Widened_AuditEmitBranches covers the emitAdminAudit
// degradation branches: (a) no bus wired ⇒ the fleet read still succeeds
// (audit logged only, never silent); (b) a failing redactor ⇒ the read
// still succeeds but no event is published (fail-safe, never unredacted).
func TestAgentsList_Widened_AuditEmitBranches(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 64,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})
	if _, err := reg.Register(idCtx(t, "tenant-A", "u1", "s1"), "k", sampleFleetConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	widenReq := prototypes.AgentListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "obs", Session: "obs"},
		Filter:   prototypes.AgentFilter{TenantIDs: []string{"tenant-A"}},
	}

	// (a) No bus wired — the widened read succeeds (audit logged only).
	noBusSvc, err := agentsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService(no bus): %v", err)
	}
	if resp, err := noBusSvc.List(context.Background(), widenReq, true); err != nil {
		t.Fatalf("no-bus widened List: %v", err)
	} else if len(resp.Agents) != 1 {
		t.Fatalf("no-bus widened count=%d, want 1", len(resp.Agents))
	}

	// (b) Failing redactor — the widened read still succeeds; the event is
	//     NOT published (never unredacted).
	badSvc, err := agentsprotocol.NewService(proj,
		agentsprotocol.WithBus(bus), agentsprotocol.WithRedactor(failingRedactor{}))
	if err != nil {
		t.Fatalf("NewService(bad redactor): %v", err)
	}
	if _, err := badSvc.List(context.Background(), widenReq, true); err != nil {
		t.Fatalf("bad-redactor widened List: %v", err)
	}
}

// fleetConfigSource is an in-test ConfigSource that records the identity
// each join runs under and returns per-agent planner/model/tool data. It
// proves the widened read runs the config join under each record's OWN
// registration identity (never the caller's).
type fleetConfigSource struct {
	mu        sync.Mutex
	seenScope map[string]string // agentID → tenant the join ran under
}

func (c *fleetConfigSource) note(id identity.Identity, agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seenScope == nil {
		c.seenScope = map[string]string{}
	}
	c.seenScope[agentID] = id.TenantID
}

func (c *fleetConfigSource) Config(_ context.Context, id identity.Identity, agentID string) (prototypes.AgentConfig, error) {
	c.note(id, agentID)
	return prototypes.AgentConfig{PlannerType: "react", Model: "test-model"}, nil
}

func (c *fleetConfigSource) Tools(_ context.Context, _ identity.Identity, _ string) ([]prototypes.AgentToolBinding, error) {
	return []prototypes.AgentToolBinding{{ToolName: "t1", Transport: "MCP"}, {ToolName: "t2", Transport: "in-proc"}}, nil
}

func (c *fleetConfigSource) Memory(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentMemoryBinding, error) {
	return prototypes.AgentMemoryBinding{}, nil
}

func (c *fleetConfigSource) Governance(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentGovernance, error) {
	return prototypes.AgentGovernance{}, nil
}

func (c *fleetConfigSource) Skills(_ context.Context, _ identity.Identity, _ string) ([]prototypes.AgentSkillBinding, error) {
	return nil, nil
}

// TestAgentsList_Widened_ConfigJoinsPerRecordIdentity — the widened read
// projects planner/model/tool counts through the ConfigSource join, and
// runs each join under the AGENT's own registration identity (never the
// caller's), matching the maintenance-scan "act under each record's own
// identity" contract.
func TestAgentsList_Widened_ConfigJoinsPerRecordIdentity(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 64,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})

	recA, err := reg.Register(idCtx(t, "tenant-A", "u1", "s1"), "k1", sampleFleetConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recB, err := reg.Register(idCtx(t, "tenant-A", "u2", "s2"), "k2", sampleFleetConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}

	src := &fleetConfigSource{}
	proj, err := agentsprotocol.NewRegistryProjector(reg, agentsprotocol.WithConfigSource(src))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := agentsprotocol.NewService(proj, agentsprotocol.WithBus(bus), agentsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	resp, err := svc.List(context.Background(), prototypes.AgentListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "obs", Session: "obs"},
		Filter:   prototypes.AgentFilter{TenantIDs: []string{"tenant-A"}},
	}, true)
	if err != nil {
		t.Fatalf("widened List: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("widened count=%d, want 2", len(resp.Agents))
	}
	for _, a := range resp.Agents {
		if a.PlannerType != "react" || a.Model != "test-model" {
			t.Errorf("agent %q missing config join: planner=%q model=%q", a.ID, a.PlannerType, a.Model)
		}
		if a.ToolsCount != 2 || a.MCPCount != 1 {
			t.Errorf("agent %q tool counts wrong: tools=%d mcp=%d", a.ID, a.ToolsCount, a.MCPCount)
		}
	}
	// The join ran under each agent's OWN registration identity (tenant-A);
	// both agents were seen.
	if src.seenScope[recA.AgentID] != "tenant-A" || src.seenScope[recB.AgentID] != "tenant-A" {
		t.Errorf("config join identity attribution wrong: %+v", src.seenScope)
	}
}
