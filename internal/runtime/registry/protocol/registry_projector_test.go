package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// newRealRegistry builds a *registry.Registry over fresh in-mem state +
// events drivers + the patterns redactor — all PRODUCTION drivers, no
// mocks at the seam (CLAUDE.md §17.3).
func newRealRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         100,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
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
	return reg
}

// idCtx builds a context carrying the (tenant, user, session) triple.
func idCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestRegistryProjector_NilRegistry_FailsLoud(t *testing.T) {
	_, err := agentsprotocol.NewRegistryProjector(nil)
	if !errors.Is(err, agentsprotocol.ErrMisconfigured) {
		t.Fatalf("NewRegistryProjector(nil) err = %v, want ErrMisconfigured", err)
	}
}

// TestRegistryProjector_List_ScopedByTuple_NotAgentID proves the
// projection filters by the (tenant, user, session) tuple — NEVER by
// agent_id (D-059 / CLAUDE.md §6). Two tenants register agents; each
// tenant's List sees ONLY its own.
func TestRegistryProjector_List_ScopedByTuple_NotAgentID(t *testing.T) {
	reg := newRealRegistry(t)
	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}

	ctxA := idCtx(t, "tenant-a", "u", "s")
	ctxB := idCtx(t, "tenant-b", "u", "s")
	if _, err := reg.Register(ctxA, "agent-a", registry.AgentConfig{}, registry.RegisterOptions{DisplayName: "Alpha"}); err != nil {
		t.Fatalf("Register A: %v", err)
	}
	if _, err := reg.Register(ctxB, "agent-b", registry.AgentConfig{}, registry.RegisterOptions{DisplayName: "Bravo"}); err != nil {
		t.Fatalf("Register B: %v", err)
	}

	idA := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}
	idB := identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s"}

	listA, err := proj.ListAgents(ctxA, idA)
	if err != nil {
		t.Fatalf("ListAgents A: %v", err)
	}
	if len(listA) != 1 || listA[0].Name != "Alpha" {
		t.Fatalf("tenant-a sees %+v, want exactly [Alpha]", listA)
	}

	listB, err := proj.ListAgents(ctxB, idB)
	if err != nil {
		t.Fatalf("ListAgents B: %v", err)
	}
	if len(listB) != 1 || listB[0].Name != "Bravo" {
		t.Fatalf("tenant-b sees %+v, want exactly [Bravo]; cross-tenant leak", listB)
	}
}

// TestRegistryProjector_Get_NotFound proves a Get for an agent_id that
// does not exist under the caller's identity tuple maps to
// ErrAgentNotFound — including the cross-tenant case (tenant-b's agent
// id is invisible to tenant-a).
func TestRegistryProjector_Get_NotFound(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctxB := idCtx(t, "tenant-b", "u", "s")
	recB, err := reg.Register(ctxB, "agent-b", registry.AgentConfig{}, registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	// tenant-a asks for tenant-b's agent_id — invisible across the
	// isolation boundary, so ErrAgentNotFound (NOT a cross-tenant read).
	ctxA := idCtx(t, "tenant-a", "u", "s")
	idA := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}
	if _, err := proj.GetAgent(ctxA, idA, recB.AgentID); !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("cross-tenant GetAgent err = %v, want ErrAgentNotFound", err)
	}
}

// TestRegistryProjector_Get_ProjectsRegistrationIdentity proves the
// projection carries the registration identity (agent_id, incarnation,
// version_hash) and the AgentConfig hash chain (D-068).
func TestRegistryProjector_Get_ProjectsRegistrationIdentity(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rec, err := reg.Register(ctx, "support", registry.AgentConfig{
		Prompts: []string{"be helpful"},
	}, registry.RegisterOptions{DisplayName: "Support Bot"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, err := proj.GetAgent(ctx, id, rec.AgentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if resp.Agent.ID != rec.AgentID {
		t.Fatalf("Agent.ID = %q, want %q", resp.Agent.ID, rec.AgentID)
	}
	if resp.Agent.Incarnation != 1 {
		t.Fatalf("Incarnation = %d, want 1", resp.Agent.Incarnation)
	}
	if resp.Agent.VersionHash == "" {
		t.Fatalf("VersionHash empty — D-068 hash chain not projected")
	}
	if resp.Agent.Hosting != prototypes.AgentHostingLocal {
		t.Fatalf("Hosting = %q, want local", resp.Agent.Hosting)
	}
	if resp.Agent.Status != prototypes.AgentStatusActive {
		t.Fatalf("Status = %q, want active", resp.Agent.Status)
	}
}

// TestRegistryProjector_NoConfigSource_HonestEmptyProjections proves the
// configuration-derived tabs return HONEST empty projections when no
// ConfigSource is wired — empty slices / zero values, NOT a faked
// success and NOT an error (CLAUDE.md §13). The methods still validate
// the agent exists.
func TestRegistryProjector_NoConfigSource_HonestEmptyProjections(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rec, _ := reg.Register(ctx, "a", registry.AgentConfig{}, registry.RegisterOptions{})

	tools, err := proj.AgentTools(ctx, id, rec.AgentID)
	if err != nil || len(tools) != 0 {
		t.Fatalf("AgentTools (no config) = %+v err=%v, want empty slice", tools, err)
	}
	skills, err := proj.AgentSkills(ctx, id, rec.AgentID)
	if err != nil || len(skills) != 0 {
		t.Fatalf("AgentSkills (no config) = %+v err=%v, want empty slice", skills, err)
	}
	// A configuration-derived method against a NON-existent agent still
	// fails loud with ErrAgentNotFound — it does not silently degrade.
	if _, err := proj.AgentTools(ctx, id, "ghost"); !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("AgentTools(ghost) err = %v, want ErrAgentNotFound", err)
	}
}

// TestRegistryProjector_Permissions_ImplicitV1Default proves the V1
// permission model is implicit (page-agents.md §10).
func TestRegistryProjector_Permissions_ImplicitV1Default(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rec, _ := reg.Register(ctx, "a", registry.AgentConfig{}, registry.RegisterOptions{})

	perms, err := proj.AgentPermissions(ctx, id, rec.AgentID)
	if err != nil {
		t.Fatalf("AgentPermissions: %v", err)
	}
	if perms.Model != "implicit" {
		t.Fatalf("permission model = %q, want implicit", perms.Model)
	}
}

// TestRegistryProjector_Metrics_CountsActiveAgents proves the rollup
// counts active agents from the identity-scoped registry view.
func TestRegistryProjector_Metrics_CountsActiveAgents(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	for _, key := range []string{"a", "b", "c"} {
		if _, err := reg.Register(ctx, key, registry.AgentConfig{}, registry.RegisterOptions{}); err != nil {
			t.Fatalf("Register %s: %v", key, err)
		}
	}
	m, err := proj.Metrics(ctx, id)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.ActiveAgents != 3 {
		t.Fatalf("ActiveAgents = %d, want 3", m.ActiveAgents)
	}
}

// wellKnownDefaultID is the synthetic default agent's well-known id used
// across the default-agent projector tests.
const wellKnownDefaultID = "harbor-default-agent"

// fakeConfigSource is a minimal ConfigSource returning non-empty
// projections so the config-join branches (planner/model/tool counts +
// the detail tabs + the governance rollup) are exercised. Its data is
// fixed, not identity-derived — the test asserts the join wiring, not a
// per-identity policy.
type fakeConfigSource struct{}

func (fakeConfigSource) Config(context.Context, identity.Identity, string) (prototypes.AgentConfig, error) {
	return prototypes.AgentConfig{PlannerType: "react", Model: "test-model"}, nil
}

func (fakeConfigSource) Tools(context.Context, identity.Identity, string) ([]prototypes.AgentToolBinding, error) {
	return []prototypes.AgentToolBinding{{Transport: "MCP"}, {Transport: "InProc"}}, nil
}

func (fakeConfigSource) Memory(context.Context, identity.Identity, string) (prototypes.AgentMemoryBinding, error) {
	return prototypes.AgentMemoryBinding{StrategyID: "window"}, nil
}

func (fakeConfigSource) Governance(context.Context, identity.Identity, string) (prototypes.AgentGovernance, error) {
	return prototypes.AgentGovernance{Ceilings: []prototypes.AgentCostCeiling{{SpendUSD: 1.5}}}, nil
}

func (fakeConfigSource) Skills(context.Context, identity.Identity, string) ([]prototypes.AgentSkillBinding, error) {
	return []prototypes.AgentSkillBinding{{}}, nil
}

// TestRegistryProjector_WithConfigSource_JoinsProjections proves the
// optional ConfigSource join hydrates the config-derived fields on the
// list/get rows, the detail tabs, and the governance rollup.
func TestRegistryProjector_WithConfigSource_JoinsProjections(t *testing.T) {
	reg := newRealRegistry(t)
	proj, err := agentsprotocol.NewRegistryProjector(reg, agentsprotocol.WithConfigSource(fakeConfigSource{}))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rec, err := reg.Register(ctx, "a", registry.AgentConfig{}, registry.RegisterOptions{DisplayName: "A"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	rows, err := proj.ListAgents(ctx, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListAgents = %+v err=%v", rows, err)
	}
	if rows[0].PlannerType != "react" || rows[0].Model != "test-model" {
		t.Errorf("row config join = %+v, want react/test-model", rows[0])
	}
	if rows[0].ToolsCount != 2 || rows[0].MCPCount != 1 {
		t.Errorf("row tool counts = tools:%d mcp:%d, want 2/1", rows[0].ToolsCount, rows[0].MCPCount)
	}

	resp, err := proj.GetAgent(ctx, id, rec.AgentID)
	if err != nil || resp.Config.PlannerType != "react" {
		t.Fatalf("GetAgent config = %+v err=%v", resp.Config, err)
	}
	tools, err := proj.AgentTools(ctx, id, rec.AgentID)
	if err != nil || len(tools) != 2 {
		t.Fatalf("AgentTools = %+v err=%v, want 2", tools, err)
	}
	mem, err := proj.AgentMemory(ctx, id, rec.AgentID)
	if err != nil || mem.StrategyID != "window" {
		t.Fatalf("AgentMemory = %+v err=%v", mem, err)
	}
	gov, err := proj.AgentGovernance(ctx, id, rec.AgentID)
	if err != nil || len(gov.Ceilings) != 1 {
		t.Fatalf("AgentGovernance = %+v err=%v", gov, err)
	}
	sk, err := proj.AgentSkills(ctx, id, rec.AgentID)
	if err != nil || len(sk) != 1 {
		t.Fatalf("AgentSkills = %+v err=%v", sk, err)
	}
	m, err := proj.Metrics(ctx, id)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.TotalCostUSD != 1.5 {
		t.Errorf("Metrics TotalCostUSD = %v, want 1.5 (governance rollup)", m.TotalCostUSD)
	}
}

// TestRegistryProjector_DefaultAgent_SynthesizedWhenRegistryEmpty proves
// the synthetic default row is emitted for a runtime whose Agent Registry
// holds zero records for the caller's scope: exactly one row, IsDefault
// true, the well-known id, and the caller's own verified triple as the
// row's identity attribution.
func TestRegistryProjector_DefaultAgent_SynthesizedWhenRegistryEmpty(t *testing.T) {
	reg := newRealRegistry(t)
	proj, err := agentsprotocol.NewRegistryProjector(reg,
		agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{
			ID:          wellKnownDefaultID,
			DisplayName: "Harbor default agent",
			BootedAt:    time.Now(),
		}))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rows, err := proj.ListAgents(ctx, id)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAgents rows=%d, want exactly 1 synthetic default row", len(rows))
	}
	got := rows[0]
	if !got.IsDefault {
		t.Errorf("row IsDefault=false, want true")
	}
	if got.ID != wellKnownDefaultID {
		t.Errorf("row ID=%q, want %q", got.ID, wellKnownDefaultID)
	}
	if got.Status != prototypes.AgentStatusActive {
		t.Errorf("row Status=%q, want active", got.Status)
	}
	// Identity attribution is the caller's own verified triple on the
	// non-widened read (D-311 / D-299): the default agent serves THAT call
	// under THAT scope.
	if got.Identity.Tenant != "t1" || got.Identity.User != "u1" || got.Identity.Session != "s1" {
		t.Errorf("row Identity=%+v, want caller's own triple {t1,u1,s1}", got.Identity)
	}
}

// TestRegistryProjector_DefaultAgent_GetResolves proves agents.get on the
// well-known id resolves the synthetic projection (no ErrAgentNotFound)
// when no colliding real registration exists.
func TestRegistryProjector_DefaultAgent_GetResolves(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg,
		agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{
			ID: wellKnownDefaultID, DisplayName: "Harbor default agent",
		}))

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	resp, err := proj.GetAgent(ctx, id, wellKnownDefaultID)
	if err != nil {
		t.Fatalf("GetAgent(default id) err=%v, want the synthetic projection", err)
	}
	if !resp.Agent.IsDefault || resp.Agent.ID != wellKnownDefaultID {
		t.Fatalf("GetAgent(default id) = %+v, want IsDefault row with the well-known id", resp.Agent)
	}
	// A get for an unrelated absent id still fails loud.
	if _, err := proj.GetAgent(ctx, id, "ghost"); !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("GetAgent(ghost) err=%v, want ErrAgentNotFound", err)
	}
}

// TestRegistryProjector_DefaultAgent_CollisionSuppresses proves the
// collision rule: when a real AgentRecord is registered under the
// well-known default id, the registered record wins and NO synthetic row
// is emitted — one row per id, real data over the placeholder.
func TestRegistryProjector_DefaultAgent_CollisionSuppresses(t *testing.T) {
	reg := newRealRegistry(t)
	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rec, err := reg.Register(ctx, "real", registry.AgentConfig{}, registry.RegisterOptions{DisplayName: "Real Agent"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Point the descriptor at the real record's minted id so it collides.
	proj, _ := agentsprotocol.NewRegistryProjector(reg,
		agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{
			ID: rec.AgentID, DisplayName: "Harbor default agent",
		}))

	rows, err := proj.ListAgents(ctx, id)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAgents rows=%d, want exactly 1 (no duplicate id)", len(rows))
	}
	if rows[0].IsDefault {
		t.Errorf("colliding row IsDefault=true, want the REAL record (IsDefault false) to win")
	}
	if rows[0].Name != "Real Agent" {
		t.Errorf("colliding row Name=%q, want the real record's display name", rows[0].Name)
	}
	// GetAgent on the id returns the real record (registry.Get succeeds).
	resp, err := proj.GetAgent(ctx, id, rec.AgentID)
	if err != nil {
		t.Fatalf("GetAgent(colliding id): %v", err)
	}
	if resp.Agent.IsDefault {
		t.Errorf("GetAgent(colliding id) IsDefault=true, want the real record")
	}
	// Metrics counts one active agent (the real record), not two.
	m, err := proj.Metrics(ctx, id)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.ActiveAgents != 1 {
		t.Errorf("ActiveAgents=%d under collision, want 1 (no double count)", m.ActiveAgents)
	}
}

// TestRegistryProjector_DefaultAgent_AbsentWhenUnwired is the regression
// guard: without WithDefaultAgent the projector is byte-identical to
// today — an empty registry yields zero rows and a get on the (would-be)
// well-known id still fails loud.
func TestRegistryProjector_DefaultAgent_AbsentWhenUnwired(t *testing.T) {
	reg := newRealRegistry(t)
	proj, _ := agentsprotocol.NewRegistryProjector(reg)

	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	rows, err := proj.ListAgents(ctx, id)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unwired ListAgents rows=%d, want 0 (byte-identical to pre-default behavior)", len(rows))
	}
	if _, err := proj.GetAgent(ctx, id, wellKnownDefaultID); !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("unwired GetAgent(default id) err=%v, want ErrAgentNotFound", err)
	}
	m, err := proj.Metrics(ctx, id)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.ActiveAgents != 0 {
		t.Fatalf("unwired Metrics ActiveAgents=%d, want 0", m.ActiveAgents)
	}
}

// TestRegistryProjector_DefaultAgent_AppendedAlongsideRealRows proves the
// synthetic row co-exists with real registered rows (no collision) — the
// real rows plus exactly one IsDefault row, and Metrics counts them all.
func TestRegistryProjector_DefaultAgent_AppendedAlongsideRealRows(t *testing.T) {
	reg := newRealRegistry(t)
	ctx := idCtx(t, "t1", "u1", "s1")
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	for _, key := range []string{"a", "b"} {
		if _, err := reg.Register(ctx, key, registry.AgentConfig{}, registry.RegisterOptions{}); err != nil {
			t.Fatalf("Register %s: %v", key, err)
		}
	}
	proj, _ := agentsprotocol.NewRegistryProjector(reg,
		agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{ID: wellKnownDefaultID}))

	rows, err := proj.ListAgents(ctx, id)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	defaults := 0
	for _, r := range rows {
		if r.IsDefault {
			defaults++
		}
	}
	if len(rows) != 3 || defaults != 1 {
		t.Fatalf("rows=%d defaults=%d, want 3 rows with exactly 1 IsDefault", len(rows), defaults)
	}
	m, err := proj.Metrics(ctx, id)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.ActiveAgents != 3 {
		t.Fatalf("ActiveAgents=%d, want 3 (2 real + 1 synthetic)", m.ActiveAgents)
	}
}
