package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// phase167_owner_scoped_reconcile_test.go drives the OWNER-SCOPED run-start
// reconcile end-to-end with REAL drivers and a REAL stdio MCP fixture: two
// owners each runtime-add a connection into ONE shared runtime, plus a
// boot-declared server. Owner A's run-start reconcile detaches ONLY A's
// undeclared runtime-add — never the boot server, never owner B's add. It also
// proves the bounded guarantee (a shared-runtime same-name collision fails
// loud, no false dispatch isolation), boot visibility to an arbitrary session,
// the fail-closed missing-owner guard, and the concurrent-reuse contract.
//
// It EXTENDS the D-287 model (process-global registry/catalog/dispatch): the
// registry is NOT re-keyed; only the reconcile VIEW is owner-scoped.

// ownerCfg names one runtime-add owner (a distinct tenant + agent in the shared
// runtime).
type ownerCfg struct {
	tenant  string
	user    string
	session string
	agent   string
}

func (o ownerCfg) scope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: o.tenant, User: o.user, Session: o.session}
}
func (o ownerCfg) quad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: o.tenant, UserID: o.user, SessionID: o.session}}
}

type ownerHarness struct {
	svc      *agentcfgprotocol.Service
	registry agentcfg.Registry
	catalog  tools.ToolCatalog
	mcpReg   *mcpdrv.Registry
	attacher *serve.MCPConnectionAttacher
	detacher projection.ConnectionDetacher
	bus      events.EventBus
	binPath  string
}

func newOwnerHarness(t *testing.T, binPath string) *ownerHarness {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cat := tools.NewCatalog()
	mcpReg := mcpdrv.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	// One REAL attacher/detacher shared by every owner — the owner tag is
	// stamped per-add from the request identity, not per-attacher.
	attacher := serve.NewMCPConnectionAttacher(cat, mcpReg, bus, nil,
		identity.Identity{TenantID: "boot", UserID: "boot", SessionID: "boot"}, nil)
	detacher := serve.NewMCPConnectionDetacher(cat, mcpReg, nil)
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithStdioAllowlist([]string{binPath}),
		agentcfgprotocol.WithBootDeclaredMCPServers([]string{"boot-srv"}),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(func() {
		_ = attacher.Close(context.Background())
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &ownerHarness{
		svc: svc, registry: reg, catalog: cat, mcpReg: mcpReg,
		attacher: attacher, detacher: detacher, bus: bus, binPath: binPath,
	}
}

// add runs a runtime add for owner o under connection name; returns the state.
func (h *ownerHarness) add(t *testing.T, o ownerCfg, name string) string {
	t.Helper()
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: o.scope(), AgentID: o.agent,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: name, Transport: "stdio", Command: []string{h.binPath},
		},
	})
	if err != nil {
		t.Fatalf("add %q (owner %s): %v", name, o.agent, err)
	}
	if resp.State == string(agentcfgprotocol.ConnectionStateFailed) {
		t.Logf("add %q (owner %s) failed: reason=%q", name, o.agent, resp.Reason)
	}
	return resp.State
}

func (h *ownerHarness) remove(t *testing.T, o ownerCfg, name string) {
	t.Helper()
	if _, err := h.svc.RemoveMCPConnection(context.Background(), prototypes.AgentConfigRemoveMCPConnectionRequest{
		Identity: o.scope(), AgentID: o.agent, Name: name,
	}); err != nil {
		t.Fatalf("remove %q (owner %s): %v", name, o.agent, err)
	}
}

// reconcile runs the run-start reconcile for owner o (the SAME projection helper
// both run-loop drivers call), returning the number detached.
func (h *ownerHarness) reconcile(t *testing.T, o ownerCfg) int {
	t.Helper()
	n, err := projection.ReconcileConnections(context.Background(), h.registry, o.agent, o.quad(),
		h.detacher, map[string]struct{}{"boot-srv": {}})
	if err != nil {
		t.Fatalf("reconcile (owner %s): %v", o.agent, err)
	}
	return n
}

func (h *ownerHarness) registryLists(t *testing.T, name string) bool {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "obs", UserID: "obs", SessionID: "obs"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	servers, _, lerr := h.mcpReg.ListServers(ctx, mcpRegistryListFilterAll())
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	for _, s := range servers {
		if s.Name == name {
			return true
		}
	}
	return false
}

func ownerA167() ownerCfg {
	return ownerCfg{tenant: "tenant-a", user: "admin-a", session: "sess-a", agent: "agent-a"}
}
func ownerB167() ownerCfg {
	return ownerCfg{tenant: "tenant-b", user: "admin-b", session: "sess-b", agent: "agent-b"}
}

// seedBootServer registers a boot-declared (untagged / zero-owner) server
// directly on the shared registry — the deployment-wide server attached once
// under one identity and read under many sessions.
func seedBootServer(t *testing.T, h *ownerHarness) {
	t.Helper()
	if err := h.mcpReg.Register(mcpdrv.ServerRegistration{
		Provider:     &bootStubProvider{id: "boot-srv"},
		Transport:    "stdio",
		URLOrCommand: "/usr/bin/boot-srv",
		InitialState: mcpdrv.ServerStateOnline,
		// Owner left zero → boot-declared, never reconcilable.
	}); err != nil {
		t.Fatalf("seed boot server: %v", err)
	}
}

// bootStubProvider is a minimal serverProvider for the boot server seed (no
// wire, no subprocess).
type bootStubProvider struct{ id tools.ToolSourceID }

func (p *bootStubProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *bootStubProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (p *bootStubProvider) DisplayModes() []string { return nil }
func (p *bootStubProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}
func (p *bootStubProvider) Close(context.Context) error { return nil }

// TestE2E_Phase167_OwnerScopedReconcile_NeverDetachesBootOrOtherOwner is the
// §17.1 headline: real drivers, real stdio subprocesses, two owners + a boot
// server. Owner A's run-start reconcile detaches ONLY A's undeclared
// runtime-add; the boot server and owner B's add stay attached; the boot server
// is visible to an arbitrary session's posture read.
func TestE2E_Phase167_OwnerScopedReconcile_NeverDetachesBootOrOtherOwner(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newOwnerHarness(t, binPath)
	seedBootServer(t, h)

	a, b := ownerA167(), ownerB167()

	// Owner A adds "alpha"; owner B adds "beta" — distinct source ids in the
	// shared, process-global catalog/registry.
	if st := h.add(t, a, "alpha"); st != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("owner A add alpha state = %q, want online", st)
	}
	if st := h.add(t, b, "beta"); st != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("owner B add beta state = %q, want online", st)
	}

	// The owner-scoped reconcile view is per owner: A sees only alpha, B only
	// beta; neither sees the boot server.
	if got := h.mcpReg.RuntimeAddedSources(toolauth.Owner{Tenant: a.tenant, Agent: a.agent}); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("owner A view = %v, want [alpha]", got)
	}
	if got := h.mcpReg.RuntimeAddedSources(toolauth.Owner{Tenant: b.tenant, Agent: b.agent}); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("owner B view = %v, want [beta]", got)
	}

	// Owner A removes "alpha" (revision drops it); owner B keeps beta declared.
	h.remove(t, a, "alpha")

	// Owner A's run-start reconcile detaches ONLY alpha.
	if n := h.reconcile(t, a); n != 1 {
		t.Fatalf("owner A reconcile detached %d, want 1 (alpha only)", n)
	}
	if h.registryLists(t, "alpha") {
		t.Error("alpha still attached after owner A reconcile")
	}
	// The boot server and owner B's add are UNTOUCHED by A's reconcile — the
	// over-detach the process-global enumeration would have caused is closed.
	if !h.registryLists(t, "boot-srv") {
		t.Error("boot server detached by owner A's reconcile — owner scoping failed")
	}
	if !h.registryLists(t, "beta") {
		t.Error("owner B's beta detached by owner A's reconcile — owner scoping failed")
	}

	// Owner B's own reconcile leaves beta attached (still declared).
	if n := h.reconcile(t, b); n != 0 {
		t.Fatalf("owner B reconcile detached %d, want 0 (beta still declared)", n)
	}
	if !h.registryLists(t, "beta") {
		t.Error("beta detached by its own owner's reconcile despite being declared")
	}

	// Boot server visible to an ARBITRARY session's posture read (the property
	// full-triple keying of the registry would have broken).
	for _, sess := range []identity.Identity{
		{TenantID: "tenant-c", UserID: "u-c", SessionID: "s-c"},
		{TenantID: "tenant-z", UserID: "u-z", SessionID: "s-z"},
	} {
		ctx, err := identity.With(context.Background(), sess)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		v, gerr := h.mcpReg.GetServer(ctx, "boot-srv")
		if gerr != nil || v == nil || v.Name != "boot-srv" {
			t.Fatalf("boot server not visible to session %s: v=%v err=%v", sess.TenantID, v, gerr)
		}
	}
}

// TestE2E_Phase167_RuntimeAdd_SharedRuntime_NameCollisionFailsLoud proves the
// bounded guarantee: two owners adding the SAME connection name in a shared
// runtime is NOT a silent overwrite / cross-serve — the second add fails LOUD
// (the bare-name catalog rejects the duplicate, ErrToolDuplicateName), and the
// first owner keeps its connection.
func TestE2E_Phase167_RuntimeAdd_SharedRuntime_NameCollisionFailsLoud(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newOwnerHarness(t, binPath)
	a, b := ownerA167(), ownerB167()

	if st := h.add(t, a, "collide"); st != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("owner A add collide state = %q, want online", st)
	}
	// Owner B adds the SAME name — the bare-name catalog collision surfaces
	// LOUD as a failed lifecycle (state=failed + a reason), never a silent
	// overwrite of owner A's connection.
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: b.scope(), AgentID: b.agent,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: "collide", Transport: "stdio", Command: []string{binPath},
		},
	})
	if err != nil {
		t.Fatalf("owner B add (collision) returned a transport error, want a recorded failed state: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateFailed) {
		t.Fatalf("owner B collision add state = %q, want failed (loud, not a silent overwrite)", resp.State)
	}
	if strings.TrimSpace(resp.Reason) == "" {
		t.Fatal("owner B collision add has an empty reason — the loud failure must carry a reason")
	}
	// The connection still belongs to owner A only — B never shadowed it.
	if got := h.mcpReg.RuntimeAddedSources(toolauth.Owner{Tenant: a.tenant, Agent: a.agent}); len(got) != 1 || got[0] != "collide" {
		t.Fatalf("owner A view after collision = %v, want [collide] (unchanged)", got)
	}
	if got := h.mcpReg.RuntimeAddedSources(toolauth.Owner{Tenant: b.tenant, Agent: b.agent}); len(got) != 0 {
		t.Fatalf("owner B view after failed collision = %v, want empty (no shadow entry)", got)
	}
}

// TestE2E_Phase167_RuntimeAdd_MissingOwner_FailsClosed is the fail-closed
// failure mode: a runtime add reaching the attacher without an agent (no owner)
// is rejected LOUD (ErrRuntimeAddOwnerMissing), never an untagged orphan.
func TestE2E_Phase167_RuntimeAdd_MissingOwner_FailsClosed(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newOwnerHarness(t, binPath)

	err := h.attacher.Attach(context.Background(), agentcfgprotocol.AttachRequest{
		Identity:  identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"},
		AgentID:   "", // no agent → no owner
		Name:      "orphan",
		Transport: "stdio",
		Command:   []string{binPath},
	})
	if !errors.Is(err, serve.ErrRuntimeAddOwnerMissing) {
		t.Fatalf("missing-owner attach err = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	// Nothing was registered — the orphan never reached the registry.
	if h.registryLists(t, "orphan") {
		t.Fatal("an owner-less runtime add reached the registry — fail-closed breached")
	}
}

// TestE2E_Phase167_OwnerScopedReconcile_ConcurrentReuse is the cross-package
// concurrency stress (§17.3): N concurrent owner-A reconciles + boot/other-owner
// registry reads against ONE shared runtime under -race never detach the boot
// server or owner B's add, and never race.
func TestE2E_Phase167_OwnerScopedReconcile_ConcurrentReuse(t *testing.T) {
	binPath := buildMCPTestServer(t)
	h := newOwnerHarness(t, binPath)
	seedBootServer(t, h)
	a, b := ownerA167(), ownerB167()

	h.add(t, a, "alpha")
	h.add(t, b, "beta")
	h.remove(t, a, "alpha") // undeclared for A → eligible for detach

	obsCtx, err := identity.With(context.Background(), identity.Identity{TenantID: "obs", UserID: "obs", SessionID: "obs"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_ = h.reconcile(t, a) // idempotent detach of alpha
			case 1:
				_, _, _ = h.mcpReg.ListServers(obsCtx, mcpRegistryListFilterAll())
			default:
				_ = h.mcpReg.RuntimeAddedSources(toolauth.Owner{Tenant: b.tenant, Agent: b.agent})
			}
		}(i)
	}
	wg.Wait()

	// After the storm: alpha gone, boot + beta intact.
	if h.registryLists(t, "alpha") {
		t.Error("alpha survived the concurrent reconcile storm")
	}
	if !h.registryLists(t, "boot-srv") {
		t.Error("boot server was detached under concurrent owner-A reconciles")
	}
	if !h.registryLists(t, "beta") {
		t.Error("owner B's beta was detached under concurrent owner-A reconciles")
	}
}
