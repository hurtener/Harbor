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
