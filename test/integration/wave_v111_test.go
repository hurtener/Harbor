// Wave-end composing E2E (CLAUDE.md §17.5 / §17.7 step 5) for the
// v1.11 "fleet-and-lifecycle" band — phases 152-156 (D-283..D-287):
// the agent-config Hooks carry-forward fix, admin-widened fleet
// enumeration, the broker credential source (no runtime surface to
// compose end-to-end here beyond its config seam — covered by its own
// phase test), the session-erasure audit-integrity fix, and
// `remove_mcp_connection` + detach-on-reconcile.
//
// Legs:
//
//  1. Control-plane composition (152×156×150): a pinned run-completion
//     Hook survives EVERY subsequent section edit — add_mcp_connection
//     (a runtime-added stdio fixture), set_tool_exposure (an unrelated
//     edit), remove_mcp_connection — proving the D-283 carry-forward
//     against the real connection verbs, while asserting the added
//     server's tool reaches the next-turn PROJECTED catalog and the
//     removal emits `mcp.connection.removed` exactly once.
//  2. Detach-on-reconcile + re-add (156): a real foreground run fires the
//     devstack run-loop's run-start reconcile, which detaches the removed
//     server — gone from the projected catalog, deregistered from the MCP
//     registry, its transport closed; an in-flight dispatch to the
//     removed tool fails LOUD (typed not-found / not-connected), never a
//     hang. Re-adding the same connection re-attaches cleanly (no second
//     consent — stdio needs none).
//  3. Fleet enumeration (153): tasks seeded across tenant-A and tenant-B;
//     `tasks.list` with `filter.tenant_ids=[A,B]` under adminScoped=true
//     returns both tenants' rows with per-row identity attribution and
//     emits `audit.admin_scope_used`; the same request adminScoped=false
//     fails loud with ErrScopeMismatch and no rows; a widen to A only
//     never sees B.
//  4. Erasure audit integrity (155): a session whose record-of-fact emit
//     is fault-injected to fail once — the first `sessions.delete` fails
//     loud with ErrErasureRecordFailed (data gone, ledger durable), and a
//     re-invoke converges (skips destructive steps, re-emits) yielding
//     exactly one `session.erased` with cumulative counts; the erased
//     triple never disturbs the agent-config scope.
//  5. Concurrency stress (§17.3): N=12 goroutines with DISTINCT identity
//     triples — a mix of admin/non-admin fleet listers, connection
//     add/remove/reconcile cycles, and one erasure each — asserting no
//     cross-tenant bleed, per-op success, and a restored goroutine
//     baseline after devstack Close.
//
// Real drivers on every seam; the only test seams are the scripted LLM
// driver (registered through the real llm registry) and the real-stdio
// MCP fixture binary (`cmd/harbor-mcptest-stdio`, built into a temp dir).
// Runs under -race. No time.Sleep as a sync primitive — bounded channel
// waits + the run-loop's own terminal-event signal only. The
// terminal-state helper reused from wave_v110 polls with a bounded
// deadline (the FSM exposes no per-transition channel); it is a bounded
// wait, not a sleep-for-sync of an unobservable condition.
//
// This file is `package integration_test` and reuses the in-package
// helpers: writeDevConfig (phase64), the wave_v19 subscribe/wait helpers,
// the wave_v110 scripted-driver + snapshot + terminal-wait helpers, and
// the phase83g fixture-build helpers (buildMCPTestServer / repoRoot).
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// The reserved observability session slot the erasure cascade publishes
// its record-of-fact under (NOT the erased triple). Pinned locally per
// the phase-155 test convention (the literal is unexported in the
// sessions package).
const waveV111ErasureAuditSession = "<erasure-audit>"

// waveV111HookTool is the run-completion hook target pinned in leg 1. It
// need not resolve on the catalog — the leg asserts the Hooks SECTION
// survives edits, not that the hook fires.
const waveV111HookTool = "answer_payload"

func waveV111ID(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// waveV111DevStack builds a real devstack with a scripted LLM (never
// bifrost) and the stdio fixture allowed for runtime add. No boot MCP
// servers, so the fixture is a RUNTIME-ADDED (removable) connection.
func waveV111DevStack(t *testing.T, binPath string, override planner.Planner) *devstack.DevStack {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	driverName, _ := registerWaveV110Driver()
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: waveV110Snapshot(cfg, driverName),
		MCPStdioAllowlist: []string{binPath},
		PlannerOverride:   override,
	})
	t.Cleanup(stack.Close)
	if stack.AgentConfig == nil || stack.Catalog == nil || stack.MCPRegistry == nil {
		t.Fatal("devstack: AgentConfig / Catalog / MCPRegistry nil — wiring broken")
	}
	return stack
}

// waveV111ConnService builds an agent-config Protocol Service over the
// devstack's SHARED registry + catalog + MCP registry, with a real
// stdio-capable ConnectionAttacher and a ConnectionDetacher. Because the
// catalog + registry are the devstack's own instances, an add reaches the
// projected catalog and the run-loop's run-start reconcile detaches a
// removed server (both read the SAME shared state).
func waveV111ConnService(t *testing.T, stack *devstack.DevStack, binPath string) (*agentcfgprotocol.Service, projection.ConnectionDetacher) {
	t.Helper()
	dev := identity.Identity{TenantID: devstack.DefaultDevTenant, UserID: devstack.DefaultDevUser, SessionID: devstack.DefaultDevSession}
	attacher := serve.NewMCPConnectionAttacher(stack.Catalog, stack.MCPRegistry, stack.Bus, nil, dev, stack.OAuthProviders, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })
	detacher := serve.NewMCPConnectionDetacher(stack.Catalog, stack.MCPRegistry, nil)

	svc, err := agentcfgprotocol.NewService(stack.AgentConfig,
		agentcfgprotocol.WithBus(stack.Bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(pauseresume.New(pauseresume.WithBus(stack.Bus))),
		agentcfgprotocol.WithStdioAllowlist([]string{binPath}),
		// No boot-declared MCP servers in the dev config → the fixture is
		// runtime-added and freely removable.
		agentcfgprotocol.WithBootDeclaredMCPServers(nil),
	)
	if err != nil {
		t.Fatalf("agentcfgprotocol.NewService: %v", err)
	}
	return svc, detacher
}

func waveV111Scope(id identity.Identity) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// waveV111ProjectedView returns the agent's next-turn planner catalog view
// (the projection the run-loop applies at run start).
func waveV111ProjectedView(t *testing.T, stack *devstack.DevStack, agentID string, id identity.Identity) tools.PlannerCatalogView {
	t.Helper()
	view, err := projection.ActivePlannerCatalogView(context.Background(),
		stack.AgentConfig, stack.SessionOverlay, agentID,
		identity.Quadruple{Identity: id}, stack.Catalog,
		tools.CatalogFilter{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
			LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
		})
	if err != nil {
		t.Fatalf("ActivePlannerCatalogView: %v", err)
	}
	return view
}

// waveV111ViewSourceTool scans a projected view for a tool whose Source is
// the named MCP connection, returning its catalog name.
func waveV111ViewSourceTool(view tools.PlannerCatalogView, source string) (string, bool) {
	for _, tl := range view.List() {
		if string(tl.Source) == source {
			return tl.Name, true
		}
	}
	return "", false
}

// waveV111HookPinned asserts the agent's active revision still carries the
// pinned run-completion hook after an edit.
func waveV111HookPinned(t *testing.T, svc *agentcfgprotocol.Service, id identity.Identity, agentID, after string) {
	t.Helper()
	gr, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: waveV111Scope(id), AgentID: agentID})
	if err != nil {
		t.Fatalf("agent_config.get after %s: %v", after, err)
	}
	if !gr.Set || gr.Revision == nil || gr.Revision.Payload.Hooks == nil ||
		gr.Revision.Payload.Hooks.RunCompletion == nil ||
		gr.Revision.Payload.Hooks.RunCompletion.Tool != waveV111HookTool {
		t.Fatalf("after %s: pinned run-completion hook was erased (D-283 carry-forward broken): %+v",
			after, gr.Revision)
	}
}

// ---------------------------------------------------------------------------
// Leg 1 — control-plane composition (152 × 156 × 150). Parallel-safe.
// ---------------------------------------------------------------------------

func TestE2E_WaveV111_ControlPlane_HookSurvivesConnectionEdits(t *testing.T) {
	t.Parallel()
	binPath := buildMCPTestServer(t)
	stack := waveV111DevStack(t, binPath, nil)
	svc, _ := waveV111ConnService(t, stack, binPath)

	id := waveV111ID("wave-v111", "coord", "s-cp")
	agentID := stack.AgentConfigID
	ctx := context.Background()

	const connName = "wavemcp1"

	// mcp.connection.removed is an admin-family event — an Admin:true
	// subscription with an empty triple fans it in (the addconnection_test
	// convention). Subscribe BEFORE the remove so the publish is never raced.
	remSub, err := stack.Bus.Subscribe(ctx, events.Filter{
		Admin: true, Types: []events.EventType{agentcfg.EventTypeMCPConnectionRemoved},
	})
	if err != nil {
		t.Fatalf("subscribe mcp.connection.removed: %v", err)
	}
	defer remSub.Cancel()

	// Pin a run-completion hook via a full revision.
	if _, err := svc.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: waveV111HookTool, TimeoutMS: 5000},
			},
		},
	}); err != nil {
		t.Fatalf("set_revision (pin hook): %v", err)
	}
	waveV111HookPinned(t, svc, id, agentID, "set_revision")

	// add_mcp_connection (runtime-added stdio fixture).
	add, err := svc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: connName, Transport: "stdio", Command: []string{binPath},
		},
	})
	if err != nil {
		t.Fatalf("add_mcp_connection: %v", err)
	}
	if add.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("add state = %q, want online (reason=%q)", add.State, add.Reason)
	}
	waveV111HookPinned(t, svc, id, agentID, "add_mcp_connection")

	// The added server's tool reaches the next-turn PROJECTED catalog.
	view := waveV111ProjectedView(t, stack, agentID, id)
	toolName, ok := waveV111ViewSourceTool(view, connName)
	if !ok {
		t.Fatalf("added server %q has no tool in the projected catalog: %v", connName, viewToolNames(view))
	}

	// set_tool_exposure — an UNRELATED edit that must not erase the hook.
	if _, err := svc.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		ToolExposure: prototypes.AgentConfigToolExposure{DisabledTools: []string{"some_other_tool"}},
	}); err != nil {
		t.Fatalf("set_tool_exposure: %v", err)
	}
	waveV111HookPinned(t, svc, id, agentID, "set_tool_exposure")

	// remove_mcp_connection.
	if _, err := svc.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
		Identity: waveV111Scope(id), AgentID: agentID, Name: connName,
	}); err != nil {
		t.Fatalf("remove_mcp_connection: %v", err)
	}
	waveV111HookPinned(t, svc, id, agentID, "remove_mcp_connection")

	// Note: the remove VERB records a revision + emits the event but does NOT
	// tear the transport down — exposure is next-turn, teardown is a run-start
	// reconcile (leg 2 asserts the catalog/registry/transport clear). So the
	// tool that was projected (toolName) stays on the live catalog here.
	_ = toolName

	// mcp.connection.removed fired exactly once.
	ev := waveV19WaitEvent(t, remSub.Events(), agentcfg.EventTypeMCPConnectionRemoved)
	if p, ok := ev.Payload.(agentcfg.MCPConnectionRemovedPayload); ok {
		if p.ServerID != connName {
			t.Errorf("removed event ServerID = %q, want %q", p.ServerID, connName)
		}
	}
	select {
	case dup := <-remSub.Events():
		if dup.Type == agentcfg.EventTypeMCPConnectionRemoved {
			t.Errorf("mcp.connection.removed fired more than once: %+v", dup)
		}
	case <-time.After(300 * time.Millisecond):
		// bounded absence window — no second emit is the pass
	}
}

// viewToolNames / viewToolCount are small diagnostics for the projected view.
func viewToolNames(v tools.PlannerCatalogView) []string {
	out := make([]string, 0, len(v.List()))
	for _, tl := range v.List() {
		out = append(out, tl.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Leg 2 — detach-on-reconcile + re-add (156). Parallel-safe.
// ---------------------------------------------------------------------------

// waveV111FinishPlanner immediately finishes so a spawned foreground run
// reaches terminal quickly; the run-start reconcile fires BEFORE the
// planner, so the reconcile still runs.
type waveV111FinishPlanner struct{}

func (waveV111FinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"ok": true}}, nil
}

func TestE2E_WaveV111_DetachOnReconcile_AndReAdd(t *testing.T) {
	t.Parallel()
	binPath := buildMCPTestServer(t)
	stack := waveV111DevStack(t, binPath, waveV111FinishPlanner{})
	svc, detacher := waveV111ConnService(t, stack, binPath)

	id := waveV111ID("wave-v111-rc", "coord", "s-rc")
	agentID := stack.AgentConfigID
	ctx := context.Background()
	const connName = "wavemcp2"

	// Add, then capture the live tool descriptor for the in-flight-dispatch
	// assertion, then remove (revision only — teardown is at reconcile).
	add, err := svc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: connName, Transport: "stdio", Command: []string{binPath},
		},
	})
	if err != nil || add.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("add_mcp_connection: state=%q err=%v", add.State, err)
	}
	view := waveV111ProjectedView(t, stack, agentID, id)
	toolName, ok := waveV111ViewSourceTool(view, connName)
	if !ok {
		t.Fatalf("added server %q not projected", connName)
	}
	desc, ok := stack.Catalog.Resolve(toolName)
	if !ok {
		t.Fatalf("tool %q not resolvable on the catalog after add", toolName)
	}

	if _, err := svc.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
		Identity: waveV111Scope(id), AgentID: agentID, Name: connName,
	}); err != nil {
		t.Fatalf("remove_mcp_connection: %v", err)
	}
	// Still attached until reconcile (teardown is process-global, run-start).
	if !waveV111SourcesContain(detacher.AttachedSources(ctx, toolauth.Owner{Tenant: id.TenantID, Agent: agentID}), connName) {
		t.Fatalf("connection detached before reconcile — teardown must be run-start, not on the remove verb")
	}

	// Drive a real foreground run → the devstack run-loop's run-start
	// reconcile detaches the no-longer-declared server. Wait for terminal on
	// a bus subscription (no sleep-for-sync).
	termCh := waveV19Subscribe(t, stack.Bus, id,
		tasks.EventTypeTaskCompleted, tasks.EventTypeTaskFailed)
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Query: "reconcile trigger",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waveV111WaitTaskTerminal(t, termCh)

	// Post-reconcile: gone from the projected catalog AND the catalog itself,
	// deregistered from the MCP registry, transport closed.
	if _, still := waveV111ViewSourceTool(waveV111ProjectedView(t, stack, agentID, id), connName); still {
		t.Errorf("removed server %q still projected after reconcile", connName)
	}
	if _, still := stack.Catalog.Resolve(toolName); still {
		t.Errorf("tool %q still resolvable on the catalog after reconcile detach", toolName)
	}
	if waveV111SourcesContain(detacher.AttachedSources(ctx, toolauth.Owner{Tenant: id.TenantID, Agent: agentID}), connName) {
		t.Errorf("connection %q still attached in the MCP registry after reconcile detach", connName)
	}

	// An in-flight dispatch to the detached tool fails LOUD (typed), never a
	// hang: the previously-resolved descriptor now errors on invoke.
	if _, ierr := desc.Invoke(idCtx, json.RawMessage(`{"message":"post-detach"}`)); ierr == nil {
		t.Errorf("dispatch to the detached tool succeeded — must fail loud")
	} else if !errors.Is(ierr, mcpdrv.ErrNotConnected) && !errors.Is(ierr, tools.ErrToolNotFound) {
		t.Errorf("dispatch to the detached tool err = %v, want a typed not-connected / not-found", ierr)
	}

	// Re-add the same connection → re-attaches cleanly (stdio has no consent
	// step, so a successful direct online re-attach IS the "reuses the
	// persisted binding / no second consent" property for this transport).
	readd, err := svc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: connName, Transport: "stdio", Command: []string{binPath},
		},
	})
	if err != nil || readd.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("re-add: state=%q err=%v — re-attach must reuse the persisted binding with no re-consent", readd.State, err)
	}
	if _, ok := waveV111ViewSourceTool(waveV111ProjectedView(t, stack, agentID, id), connName); !ok {
		t.Errorf("re-added server %q not projected", connName)
	}
}

func waveV111SourcesContain(sources []string, want string) bool {
	for _, s := range sources {
		if s == want {
			return true
		}
	}
	return false
}

// waveV111WaitTaskTerminal blocks (bounded) on a terminal task event.
func waveV111WaitTaskTerminal(t *testing.T, ch <-chan events.Event) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("task subscription closed before terminal")
			}
			if ev.Type == tasks.EventTypeTaskCompleted || ev.Type == tasks.EventTypeTaskFailed {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the reconcile-triggering run to reach terminal")
		}
	}
}

// ---------------------------------------------------------------------------
// Leg 3 — fleet enumeration (153). Parallel-safe (standalone real drivers).
// ---------------------------------------------------------------------------

// waveV111FleetStack builds a standalone real stack (inmem state + bus +
// patterns redactor + inprocess tasks) so seeded tasks are NOT executed by
// a run loop — the fleet leg drives tasksprotocol.Service.List directly.
func waveV111FleetStack(t *testing.T) (tasks.TaskRegistry, events.EventBus) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 128,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store: st, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks OpenDriver: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg, bus
}

func waveV111SeedTask(t *testing.T, reg tasks.TaskRegistry, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Description: "fleet task", Query: "q",
	}); err != nil {
		t.Fatalf("Spawn(%v): %v", id, err)
	}
}

func TestE2E_WaveV111_FleetEnumeration(t *testing.T) {
	t.Parallel()
	reg, bus := waveV111FleetStack(t)
	red := auditpatterns.New()

	seeds := []identity.Identity{
		{TenantID: "tenant-A", UserID: "u1", SessionID: "s1"},
		{TenantID: "tenant-A", UserID: "u2", SessionID: "s2"},
		{TenantID: "tenant-B", UserID: "u1", SessionID: "s1"},
		{TenantID: "tenant-B", UserID: "u2", SessionID: "s2"},
	}
	for _, id := range seeds {
		waveV111SeedTask(t, reg, id)
	}

	proj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj,
		tasksprotocol.WithBus(bus), tasksprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Admin-widened over both tenants → 4 rows, per-row identity attribution,
	// audit.admin_scope_used observed on the real bus.
	obs := identity.Identity{TenantID: "tenant-A", UserID: "obs", SessionID: "obs"}
	auditSub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: obs.TenantID, User: obs.UserID, Session: obs.SessionID, Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("subscribe admin_scope_used: %v", err)
	}
	defer auditSub.Cancel()

	both, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: waveV111Scope(obs),
		Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A", "tenant-B"}},
	}, true)
	if err != nil {
		t.Fatalf("admin widened List: %v", err)
	}
	if len(both.Rows) != 4 {
		t.Fatalf("admin widened rows = %d, want 4", len(both.Rows))
	}
	seenTenants := map[string]int{}
	for _, row := range both.Rows {
		if row.Identity.Tenant == "" || row.Identity.User == "" || row.Identity.Session == "" {
			t.Errorf("row missing per-row identity attribution: %+v", row.Identity)
		}
		seenTenants[row.Identity.Tenant]++
	}
	if seenTenants["tenant-A"] != 2 || seenTenants["tenant-B"] != 2 {
		t.Errorf("per-tenant row counts = %v, want A:2 B:2", seenTenants)
	}
	waveV19WaitEvent(t, auditSub.Events(), events.EventTypeAdminScopeUsed)

	// Same request without the admin claim → loud ErrScopeMismatch, no rows.
	nrows, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: waveV111Scope(obs),
		Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A", "tenant-B"}},
	}, false)
	if !errors.Is(err, tasksprotocol.ErrScopeMismatch) {
		t.Fatalf("non-admin widened err = %v, want ErrScopeMismatch", err)
	}
	if len(nrows.Rows) != 0 {
		t.Errorf("non-admin widened returned %d rows, want 0", len(nrows.Rows))
	}

	// Widen to A only → never sees B.
	onlyA, err := svc.List(context.Background(), prototypes.TaskListRequest{
		Identity: waveV111Scope(obs),
		Filter:   prototypes.TaskFilter{TenantIDs: []string{"tenant-A"}},
	}, true)
	if err != nil {
		t.Fatalf("widen-to-A List: %v", err)
	}
	for _, row := range onlyA.Rows {
		if row.Identity.Tenant == "tenant-B" {
			t.Errorf("widen-to-A leaked a tenant-B row: %+v", row.Identity)
		}
	}
	if len(onlyA.Rows) != 2 {
		t.Errorf("widen-to-A rows = %d, want 2", len(onlyA.Rows))
	}
}

// ---------------------------------------------------------------------------
// Leg 4 — erasure audit integrity (155). Parallel-safe.
// ---------------------------------------------------------------------------

// waveV111FlakyBus fails the FIRST record-of-fact (session.erased) publish
// while `fail` is set, delegating Fence/Unfence + the HistoryReplayer read
// path to the real bus so the cascade's fence + re-emit-dedup still work.
type waveV111FlakyBus struct {
	events.EventBus
	fencer   events.Fencer
	replayer events.HistoryReplayer
	fail     *atomic.Bool
}

func (b *waveV111FlakyBus) Publish(ctx context.Context, ev events.Event) error {
	if b.fail.Load() && ev.Type == sessions.EventTypeSessionErased {
		return errors.New("wave-v111: forced record-of-fact publish failure")
	}
	return b.EventBus.Publish(ctx, ev)
}

func (b *waveV111FlakyBus) Fence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Fence(ctx, id)
}
func (b *waveV111FlakyBus) Unfence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Unfence(ctx, id)
}
func (b *waveV111FlakyBus) Bounds(ctx context.Context, f events.Filter) (uint64, uint64, bool, error) {
	return b.replayer.Bounds(ctx, f)
}
func (b *waveV111FlakyBus) Window(ctx context.Context, before uint64, limit int, f events.Filter) ([]events.Event, error) {
	return b.replayer.Window(ctx, before, limit, f)
}
func (b *waveV111FlakyBus) ListWindow(ctx context.Context, q events.EventListQuery) (events.EventListPage, error) {
	return b.replayer.ListWindow(ctx, q)
}

func waveV111SeedSession(t *testing.T, stack *devstack.DevStack, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("sessions.Open: %v", err)
	}
	if err := stack.Memory.AddTurn(ctx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("memory.AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	for i := range 3 {
		if _, err := stack.Artifacts.PutBytes(ctx, scope, []byte(fmt.Sprintf("wave-v111-blob-%d", i)),
			artifacts.PutOpts{Namespace: "test"}); err != nil {
			t.Fatalf("artifacts.PutBytes: %v", err)
		}
	}
}

func TestE2E_WaveV111_ErasureAuditIntegrity(t *testing.T) {
	t.Parallel()
	binPath := buildMCPTestServer(t)
	stack := waveV111DevStack(t, binPath, nil)

	id := waveV111ID("wave-v111-erase", "gwen", "s-erase")
	waveV111SeedSession(t, stack, id)

	// Pin an agent-config revision under the SAME tenant so we can prove the
	// erasure never disturbs the agent-config scope.
	svc, _ := waveV111ConnService(t, stack, binPath)
	agentID := stack.AgentConfigID
	if _, err := svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: waveV111Scope(id), AgentID: agentID,
		Payload: prototypes.AgentConfigPayload{
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: strPtrV111("operator-base")},
		},
	}); err != nil {
		t.Fatalf("seed agent-config revision: %v", err)
	}

	fencer, ok := stack.Bus.(events.Fencer)
	if !ok {
		t.Fatal("stack.Bus is not an events.Fencer")
	}
	replayer, ok := stack.Bus.(events.HistoryReplayer)
	if !ok {
		t.Fatal("stack.Bus is not an events.HistoryReplayer")
	}
	var fail atomic.Bool
	fail.Store(true)
	flaky := &waveV111FlakyBus{EventBus: stack.Bus, fencer: fencer, replayer: replayer, fail: &fail}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: stack.Sessions, State: stack.State, Memory: stack.Memory,
		Artifacts: stack.Artifacts, Bus: flaky, Redactor: stack.Audit,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// Count session.erased on the reserved erasure-audit observability slot.
	erasedSub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: waveV111ErasureAuditSession, Admin: true,
		Types: []events.EventType{sessions.EventTypeSessionErased},
	})
	if err != nil {
		t.Fatalf("subscribe session.erased: %v", err)
	}
	defer erasedSub.Cancel()

	// First delete: destructive steps succeed, the record-of-fact publish
	// fails → loud ErrErasureRecordFailed.
	if _, err := eraser.Erase(context.Background(), id); !errors.Is(err, sessions.ErrErasureRecordFailed) {
		t.Fatalf("first erase err = %v, want ErrErasureRecordFailed", err)
	}
	// Data gone despite the record failure.
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if refs, _ := stack.Artifacts.List(context.Background(), scope); len(refs) != 0 {
		t.Errorf("artifacts survived the failed erase: %d", len(refs))
	}
	if _, lerr := stack.State.Load(context.Background(), identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session lifecycle record survived the failed erase: %v", lerr)
	}
	// No record-of-fact yet.
	select {
	case ev := <-erasedSub.Events():
		if ev.Type == sessions.EventTypeSessionErased {
			t.Fatalf("session.erased emitted despite the record failure")
		}
	case <-time.After(300 * time.Millisecond):
	}

	// Heal + re-invoke → converges (skips destructive steps, re-emits) with
	// cumulative counts.
	fail.Store(false)
	resp, err := eraser.Erase(context.Background(), id)
	if err != nil {
		t.Fatalf("converging erase: %v", err)
	}
	if !resp.Deleted {
		t.Errorf("converging erase resp.Deleted = false, want true")
	}
	if resp.ArtifactsDeleted != 3 || !resp.MemoryPurged || resp.StateRecordsDeleted <= 0 {
		t.Errorf("cumulative counts wrong: artifacts=%d memory=%v state=%d (want 3/true/>0)",
			resp.ArtifactsDeleted, resp.MemoryPurged, resp.StateRecordsDeleted)
	}
	// Exactly one session.erased across the two attempts.
	waveV19WaitEvent(t, erasedSub.Events(), sessions.EventTypeSessionErased)
	select {
	case dup := <-erasedSub.Events():
		if dup.Type == sessions.EventTypeSessionErased {
			t.Errorf("session.erased emitted more than once across the converging attempts")
		}
	case <-time.After(300 * time.Millisecond):
	}

	// The erased triple never disturbed the agent-config scope.
	gr, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: waveV111Scope(id), AgentID: agentID})
	if err != nil || !gr.Set || gr.Revision == nil || gr.Revision.Payload.PromptLayers == nil {
		t.Fatalf("agent-config scope disturbed by the erasure: set=%v err=%v", gr.Set, err)
	}
}

func strPtrV111(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Leg 5 — concurrency stress (§17.3). NOT parallel (goroutine baseline).
// ---------------------------------------------------------------------------

func TestE2E_WaveV111_ConcurrencyStress(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	binPath := buildMCPTestServer(t)
	stack := waveV111DevStack(t, binPath, nil)
	svc, detacher := waveV111ConnService(t, stack, binPath)
	agentID := stack.AgentConfigID

	// Standalone fleet stack seeded with one task per goroutine's tenant.
	fleetReg, fleetBus := waveV111FleetStack(t)
	fleetRed := auditpatterns.New()

	const n = 12 // ≥10 (CLAUDE.md §17.3)
	idFor := func(i int) identity.Identity {
		return identity.Identity{
			TenantID: fmt.Sprintf("wv111-t-%d", i), UserID: fmt.Sprintf("u-%d", i), SessionID: fmt.Sprintf("s-%d", i),
		}
	}
	connNameFor := func(i int) string { return fmt.Sprintf("wv111-conn-%d", i) }
	agentFor := func(i int) string { return fmt.Sprintf("%s-%d", agentID, i) }

	// Seed a fleet task per tenant; precompute the connection-goroutine name
	// set so each reconcile protects the OTHER goroutines' connections via the
	// bootDeclared set (the reconcile diff assumes one agent per registry; the
	// protection keeps the stress's distinct-agent adds from tearing each
	// other down — a cross-agent teardown that cannot occur in production's
	// one-dev-agent model).
	connNames := map[string]struct{}{}
	for i := range n {
		waveV111SeedTask(t, fleetReg, idFor(i))
		if i%3 == 2 {
			connNames[connNameFor(i)] = struct{}{}
		}
	}
	fleetProj, err := tasksprotocol.NewRegistryProjector(fleetReg)
	if err != nil {
		t.Fatalf("fleet projector: %v", err)
	}
	fleetSvc, err := tasksprotocol.NewService(fleetProj,
		tasksprotocol.WithBus(fleetBus), tasksprotocol.WithRedactor(fleetRed))
	if err != nil {
		t.Fatalf("fleet service: %v", err)
	}

	// One shared, concurrent-safe eraser (D-025 compiled artifact).
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: stack.Sessions, State: stack.State, Memory: stack.Memory,
		Artifacts: stack.Artifacts, Bus: stack.Bus, Redactor: stack.Audit,
	})
	if err != nil {
		t.Fatalf("stress eraser: %v", err)
	}

	// Seed each goroutine's session up front (Open + data) so the concurrent
	// phase only erases.
	for i := range n {
		waveV111SeedSession(t, stack, idFor(i))
	}

	errsCh := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := idFor(i)
			ctx := context.Background()

			switch i % 3 {
			case 0: // admin fleet lister — sees ONLY its own tenant's row.
				resp, lerr := fleetSvc.List(ctx, prototypes.TaskListRequest{
					Identity: waveV111Scope(id),
					Filter:   prototypes.TaskFilter{TenantIDs: []string{id.TenantID}},
				}, true)
				if lerr != nil {
					errsCh <- fmt.Errorf("goroutine %d admin list: %w", i, lerr)
					return
				}
				for _, row := range resp.Rows {
					if row.Identity.Tenant != id.TenantID {
						errsCh <- fmt.Errorf("goroutine %d fleet bleed: saw tenant %q", i, row.Identity.Tenant)
						return
					}
				}
			case 1: // non-admin own lister.
				resp, lerr := fleetSvc.List(ctx, prototypes.TaskListRequest{
					Identity: waveV111Scope(id),
				}, false)
				if lerr != nil {
					errsCh <- fmt.Errorf("goroutine %d own list: %w", i, lerr)
					return
				}
				for _, row := range resp.Rows {
					if row.Identity.Tenant != id.TenantID {
						errsCh <- fmt.Errorf("goroutine %d own-list bleed: saw tenant %q", i, row.Identity.Tenant)
						return
					}
				}
			case 2: // connection add → remove → run-start reconcile detach.
				connName := connNameFor(i)
				add, aerr := svc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
					Identity: waveV111Scope(id), AgentID: agentFor(i),
					Connection: prototypes.AgentConfigMCPConnectionDescriptor{
						Name: connName, Transport: "stdio", Command: []string{binPath},
					},
				})
				if aerr != nil {
					errsCh <- fmt.Errorf("goroutine %d add: %w", i, aerr)
					return
				}
				if add.State != string(agentcfgprotocol.ConnectionStateOnline) {
					errsCh <- fmt.Errorf("goroutine %d add: state=%q, want online", i, add.State)
					return
				}
				if _, rerr := svc.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
					Identity: waveV111Scope(id), AgentID: agentFor(i), Name: connName,
				}); rerr != nil {
					errsCh <- fmt.Errorf("goroutine %d remove: %w", i, rerr)
					return
				}
				// bootDeclared protects the other goroutines' connections.
				boot := map[string]struct{}{}
				for name := range connNames {
					if name != connName {
						boot[name] = struct{}{}
					}
				}
				detached, cerr := projection.ReconcileConnections(ctx, stack.AgentConfig, agentFor(i),
					identity.Quadruple{Identity: id}, detacher, boot)
				if cerr != nil {
					errsCh <- fmt.Errorf("goroutine %d reconcile: %w", i, cerr)
					return
				}
				if detached != 1 {
					errsCh <- fmt.Errorf("goroutine %d reconcile detached %d, want 1", i, detached)
					return
				}
			}

			// Every goroutine erases its own session (distinct triple).
			resp, eerr := eraser.Erase(ctx, id)
			if eerr != nil {
				errsCh <- fmt.Errorf("goroutine %d erase: %w", i, eerr)
				return
			}
			if !resp.Deleted {
				errsCh <- fmt.Errorf("goroutine %d erase not Deleted", i)
			}
		}(i)
	}
	wg.Wait()
	close(errsCh)
	for e := range errsCh {
		t.Error(e)
	}

	// Teardown, then assert the goroutine baseline is restored (every stdio
	// subprocess drained, every long-lived goroutine joined).
	stack.Close()
	const slack = 8
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+slack {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta := goruntime.NumGoroutine() - baseline; delta > slack {
		t.Errorf("goroutine count rose by %d after teardown (baseline=%d, now=%d) — join-on-shutdown contract violated",
			delta, baseline, goruntime.NumGoroutine())
	}
}
