package serve

// Agent-provenance producer test: proves the per-task run-loop driver
// actually stamps its agentConfigID onto the run ctx via
// tools.WithInvokingAgent — the ctx a southbound tool transport reads at
// dispatch. This pins the PRODUCER side of the provenance seam (the MCP
// driver's `_meta.agent_id` stamp is tested in its own package); the
// devstack twin (harbortest/devstack) carries the identical assertion so
// the two run loops cannot drift (§17.6 twin discipline).

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// provenanceProbePlanner records the invoking-agent provenance carried on
// the ctx the RunLoop hands the planner, then finishes immediately.
type provenanceProbePlanner struct {
	got       chan string // receives InvokingAgentFrom's value ("" when absent)
	effective chan string // receives EffectiveAgentConfigFrom's value ("" when absent)
	route     chan providerRouteObservation
}

type providerRouteObservation struct {
	trusted llm.TrustedProviderRouteContext
	q       identity.Quadruple
	present bool
}

func (p *provenanceProbePlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	agentID, _ := tools.InvokingAgentFrom(ctx)
	effectiveAgentID, _ := tools.EffectiveAgentConfigFrom(ctx)
	trustedRoute, routePresent := llm.TrustedProviderRouteFrom(ctx)
	q, _ := identity.QuadrupleFrom(ctx)
	select {
	case p.got <- agentID:
	default:
	}
	select {
	case p.effective <- effectiveAgentID:
	default:
	}
	select {
	case p.route <- providerRouteObservation{trusted: trustedRoute, q: q, present: routePresent}:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func runProviderRouteProbe(t *testing.T) providerRouteObservation {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{
		got: make(chan string, 1), effective: make(chan string, 1),
		route: make(chan providerRouteObservation, 1),
	}
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-provider-route"
	const runtimeID = "runtime-provider-route"
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus: bus, RunLoop: rl, Planner: p, Tasks: reg,
		AgentConfigID: agentID, AgentReachAdmissions: authority,
		ProviderRouteRuntimeID: runtimeID,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	spawnCtx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	spawnCtx, err = authority.Admit(spawnCtx, runLoopDriverTestID, agentID)
	if err != nil {
		t.Fatalf("authority.Admit: %v", err)
	}
	route := &llm.ProviderRoute{
		RouteID: "route-a", RouteGeneration: 4,
		ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 3,
		CredentialAssetGeneration: 2, ModelSelector: "balanced",
	}
	if _, err := reg.Spawn(spawnCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID}, Kind: tasks.KindForeground,
		Query: "provider route probe", AgentID: agentID, ProviderRoute: route,
	}); err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}

	select {
	case observed := <-p.route:
		return observed
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never observed the trusted provider route")
		return providerRouteObservation{}
	}
}

// runProvenanceProbe boots a real driver (bus + RunLoop + TaskRegistry +
// probe planner) with the given agentConfigID, spawns one task, and returns
// the provenance value the planner observed on the run ctx.
func runProvenanceProbe(t *testing.T, agentConfigID string) string {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{got: make(chan string, 1), effective: make(chan string, 1)}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:           bus,
		RunLoop:       rl,
		Planner:       p,
		Tasks:         reg,
		AgentConfigID: agentConfigID,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	_ = spawnDriverTestTask(t, reg)

	select {
	case got := <-p.got:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired — driver did not pick up task.spawned")
		return ""
	}
}

func runEffectiveAgentConfigProbe(t *testing.T, agentConfigID string, admitted bool) string {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{got: make(chan string, 1), effective: make(chan string, 1)}
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{Bus: bus, RunLoop: rl, Planner: p, Tasks: reg, AgentConfigID: agentConfigID, AgentReachAdmissions: authority})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	spawnCtx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if admitted {
		spawnCtx, err = authority.Admit(spawnCtx, runLoopDriverTestID, agentConfigID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Spawn(spawnCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID}, Kind: tasks.KindForeground,
		Query: "effective admission probe", AgentID: agentConfigID,
	}); err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	select {
	case got := <-p.effective:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired — driver did not pick up task.spawned")
		return ""
	}
}

// TestPerTaskRunLoopDriver_StampsInvokingAgentProvenance — the run loop
// stamps its non-empty agentConfigID as ctx provenance at run start.
func TestPerTaskRunLoopDriver_StampsInvokingAgentProvenance(t *testing.T) {
	if got := runProvenanceProbe(t, "agent-prov-1"); got != "agent-prov-1" {
		t.Fatalf("run ctx provenance = %q, want %q (run loop did not stamp WithInvokingAgent)", got, "agent-prov-1")
	}
}

func TestPerTaskRunLoopDriver_StampsEffectiveAgentConfigAdmission(t *testing.T) {
	if got := runEffectiveAgentConfigProbe(t, "agent-selected-1", true); got != "agent-selected-1" {
		t.Fatalf("run ctx effective configuration = %q, want reach-admitted agent-selected-1", got)
	}
}

func TestPerTaskRunLoopDriver_InstallsProviderRouteOnlyAfterReachRestore(t *testing.T) {
	observed := runProviderRouteProbe(t)
	if !observed.present {
		t.Fatal("trusted provider route absent after sealed Agent-reach restore")
	}
	if observed.trusted.EffectiveAgentID != "agent-provider-route" ||
		observed.trusted.RuntimeID != "runtime-provider-route" ||
		observed.trusted.TaskID == "" || observed.trusted.Route.RouteID != "route-a" ||
		observed.trusted.Route.RouteGeneration != 4 {
		t.Fatalf("trusted provider route = %+v, want exact admitted Agent/runtime/task/route binding", observed.trusted)
	}
	if observed.q.Identity != runLoopDriverTestID || observed.q.RunID == "" {
		t.Fatalf("trusted route context identity = %+v, want verified run identity", observed.q)
	}
}

func TestPerTaskRunLoopDriver_RejectsProviderRouteWithoutReachRestore(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New(pauseresume.WithBus(bus)), steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &provenanceProbePlanner{
		got: make(chan string, 1), effective: make(chan string, 1),
		route: make(chan providerRouteObservation, 1),
	}
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus: bus, RunLoop: rl, Planner: p, Tasks: reg,
		AgentConfigID: "agent-provider-route", AgentReachAdmissions: authority,
		ProviderRouteRuntimeID: "runtime-provider-route",
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	spawnCtx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	handle, err := reg.Spawn(spawnCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID}, Kind: tasks.KindForeground,
		Query: "forged provider route", AgentID: "agent-provider-route",
		ProviderRoute: &llm.ProviderRoute{
			RouteID: "route-a", RouteGeneration: 4,
			ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 3,
			CredentialAssetGeneration: 2, ModelSelector: "balanced",
		},
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if got := waitForTaskStatus(t, reg, handle.ID, tasks.StatusFailed, 2*time.Second); got != tasks.StatusFailed {
		t.Fatalf("unadmitted provider-route task status = %q, want failed", got)
	}
	stored, err := reg.Get(spawnCtx, handle.ID)
	if err != nil {
		t.Fatalf("reg.Get: %v", err)
	}
	if stored.Error == nil || stored.Error.Code != "provider_route_unauthorized" {
		t.Fatalf("unadmitted provider-route task error = %+v, want provider_route_unauthorized", stored.Error)
	}
	select {
	case observed := <-p.route:
		t.Fatalf("planner observed forged provider route: %+v", observed)
	default:
	}
}

func TestPerTaskRunLoopDriver_ForgedSDKAgentIDHasNoCredentialAdmission(t *testing.T) {
	if got := runEffectiveAgentConfigProbe(t, "agent-selected-1", false); got != "" {
		t.Fatalf("bare SDK AgentID minted credential admission %q", got)
	}
}

// TestPerTaskRunLoopDriver_NoAgentConfigID_NoProvenance — a bare run (empty
// agentConfigID) stamps nothing: absence is the valid embedder shape.
func TestPerTaskRunLoopDriver_NoAgentConfigID_NoProvenance(t *testing.T) {
	if got := runProvenanceProbe(t, ""); got != "" {
		t.Fatalf("run ctx provenance = %q, want absent for an empty agentConfigID", got)
	}
}
