package serve

// Caller-named agent selection in the run loop (Phase 215 / D-360).
//
// Three properties are pinned here:
//
//  1. the run's EFFECTIVE config agent id drives every run-start
//     projection — the eleven `projection.*` reads — and is the caller's
//     named agent when the task carries one, the boot value otherwise;
//  2. the run-start reconcile legs run under that SAME effective agent,
//     which is why `tasks.Get` must precede `reconcileConnections` in
//     runOne (the ordering guard — mutate the order and the reconcile
//     assertion below fails);
//  3. the CREDENTIAL PLANE is untouched: the ctx a run hands southbound
//     carries the BOOT agent id even when the task names another. Both
//     downstream consumers (the MCP `_meta.agent_id` stamp and the RFC
//     8693 actor_token) read that one ctx value, so pinning it here pins
//     both; the end-to-end assertion over the real MCP fixture server and
//     the real token-exchange broker lives in
//     test/integration/agent_selection_test.go.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

const (
	// selBootAgent is the runtime's boot-configured agent id in these
	// tests — the value the credential plane must keep using.
	selBootAgent = "sel-boot-agent"
	// selNamedAgent is the caller-named agent a `start` selects.
	selNamedAgent = "sel-named-agent"
)

// selRegistryWithTwoAgents writes divergent ConfigScopeAgent revisions
// for the boot agent and the named agent under one tenant, so any
// projection that resolves the WRONG agent produces the wrong content
// rather than merely "no revision".
func selRegistryWithTwoAgents(t *testing.T, q identity.Quadruple) agentcfg.Registry {
	t.Helper()
	reg := acTestRegistry(t)
	ctx := context.Background()

	bootBase, bootModel := "BOOT-BASE-PROMPT", "boot-model"
	namedBase, namedModel := "NAMED-BASE-PROMPT", "named-model"

	if _, err := reg.SetRevision(ctx, q, selBootAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &bootBase},
		LLMParams:    &agentcfg.LLMParams{Model: &bootModel},
		Skills:       &agentcfg.SkillsSelection{Names: []string{"boot-skill"}},
		Hooks:        &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "boot_hook"}},
		Naming:       &agentcfg.NamingSection{Auto: true, MaxTitleLen: 40},
	}); err != nil {
		t.Fatalf("SetRevision(boot): %v", err)
	}
	if _, err := reg.SetRevision(ctx, q, selNamedAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &namedBase},
		LLMParams:    &agentcfg.LLMParams{Model: &namedModel},
		Skills:       &agentcfg.SkillsSelection{Names: []string{"named-skill"}},
		Hooks:        &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "named_hook"}},
		Naming:       &agentcfg.NamingSection{Auto: true, MaxTitleLen: 90},
	}); err != nil {
		t.Fatalf("SetRevision(named): %v", err)
	}
	return reg
}

// TestRunLoopDriver_AgentSelection_ProjectionsFollowTheEffectiveAgent —
// each run-start projection resolves against the agent id it is HANDED,
// not against the driver's boot field. Table-driven across the projection
// helpers with two agents holding divergent revisions.
func TestRunLoopDriver_AgentSelection_ProjectionsFollowTheEffectiveAgent(t *testing.T) {
	ctx := context.Background()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	reg := selRegistryWithTwoAgents(t, q)

	// ONE shared driver booted as the BOOT agent. Every assertion below
	// hands it the NAMED agent and expects the named agent's content —
	// a projection that reached for d.agentConfigID instead fails.
	d := &RunLoopDriver{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentConfig:   reg,
		agentConfigID: selBootAgent,
	}

	t.Run("LLMOverrides", func(t *testing.T) {
		boot, err := d.resolveLLMOverrides(ctx, selBootAgent, q)
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		named, err := d.resolveLLMOverrides(ctx, selNamedAgent, q)
		if err != nil {
			t.Fatalf("named: %v", err)
		}
		if boot == nil || boot.Model == nil || *boot.Model != "boot-model" {
			t.Fatalf("boot model = %v, want boot-model", boot)
		}
		if named == nil || named.Model == nil || *named.Model != "named-model" {
			t.Fatalf("named model = %v, want named-model", named)
		}
	})

	t.Run("PromptLayers", func(t *testing.T) {
		boot, err := d.projectAgentConfigPromptLayers(ctx, selBootAgent, q, nil)
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		named, err := d.projectAgentConfigPromptLayers(ctx, selNamedAgent, q, nil)
		if err != nil {
			t.Fatalf("named: %v", err)
		}
		if boot == nil || boot.BasePromptLayer == nil || *boot.BasePromptLayer != "BOOT-BASE-PROMPT" {
			t.Fatalf("boot base layer = %v, want BOOT-BASE-PROMPT", boot)
		}
		if named == nil || named.BasePromptLayer == nil || *named.BasePromptLayer != "NAMED-BASE-PROMPT" {
			t.Fatalf("named base layer = %v, want NAMED-BASE-PROMPT", named)
		}
	})

	t.Run("SkillViews", func(t *testing.T) {
		views := acTestViews("boot-skill", "named-skill")
		boot, err := d.projectAgentConfigSkills(ctx, selBootAgent, q, views)
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		named, err := d.projectAgentConfigSkills(ctx, selNamedAgent, q, acTestViews("boot-skill", "named-skill"))
		if err != nil {
			t.Fatalf("named: %v", err)
		}
		if names := acViewNames(boot); len(names) != 1 || names[0] != "boot-skill" {
			t.Fatalf("boot skills = %v, want [boot-skill]", names)
		}
		if names := acViewNames(named); len(names) != 1 || names[0] != "named-skill" {
			t.Fatalf("named skills = %v, want [named-skill]", names)
		}
	})

	t.Run("RunCompletionHook", func(t *testing.T) {
		boot, err := d.projectRunCompletionHook(ctx, selBootAgent, q)
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		named, err := d.projectRunCompletionHook(ctx, selNamedAgent, q)
		if err != nil {
			t.Fatalf("named: %v", err)
		}
		if boot == nil || boot.Tool != "boot_hook" {
			t.Fatalf("boot hook = %v, want boot_hook", boot)
		}
		if named == nil || named.Tool != "named_hook" {
			t.Fatalf("named hook = %v, want named_hook", named)
		}
	})
}

// selRecordingDetacher records the owner every reconcile leg is invoked
// with. It is the ORDERING guard: the reconcile must see the RUN's
// effective agent, which is only knowable after `tasks.Get`.
type selRecordingDetacher struct {
	mu     sync.Mutex
	owners []auth.Owner
}

func (r *selRecordingDetacher) AttachedSources(_ context.Context, owner auth.Owner) []string {
	r.mu.Lock()
	r.owners = append(r.owners, owner)
	r.mu.Unlock()
	return nil
}

func (r *selRecordingDetacher) Detach(context.Context, string, auth.Owner) error { return nil }

func (r *selRecordingDetacher) snapshot() []auth.Owner {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auth.Owner, len(r.owners))
	copy(out, r.owners)
	return out
}

// selProbe captures the per-run state the run loop hands the planner:
// the resolved LLM override bundle AND the southbound provenance on the
// run ctx.
type selProbe struct {
	got chan selObservation
}

type selObservation struct {
	provenance string
	model      string
	basePrompt string
}

func (p *selProbe) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	obs := selObservation{}
	obs.provenance, _ = tools.InvokingAgentFrom(ctx)
	if rc.LLMOverrides != nil {
		if rc.LLMOverrides.Model != nil {
			obs.model = *rc.LLMOverrides.Model
		}
		if rc.LLMOverrides.BasePromptLayer != nil {
			obs.basePrompt = *rc.LLMOverrides.BasePromptLayer
		}
	}
	select {
	case p.got <- obs:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// runSelectionProbe boots a driver as selBootAgent, spawns ONE task
// carrying taskAgentID (empty = the caller named none), and returns what
// the planner observed plus the owners the reconcile legs ran under.
func runSelectionProbe(t *testing.T, taskAgentID string) (selObservation, []auth.Owner) {
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

	q := identity.Quadruple{Identity: runLoopDriverTestID}
	cfgReg := selRegistryWithTwoAgents(t, q)
	detacher := &selRecordingDetacher{}

	p := &selProbe{got: make(chan selObservation, 1)}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:                bus,
		RunLoop:            rl,
		Planner:            p,
		Tasks:              reg,
		AgentConfig:        cfgReg,
		AgentConfigID:      selBootAgent,
		ConnectionDetacher: detacher,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: q,
		Kind:     tasks.KindForeground,
		Query:    "selection probe",
		AgentID:  taskAgentID,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case obs := <-p.got:
		return obs, detacher.snapshot()
	case <-time.After(5 * time.Second):
		t.Fatal("planner.Next never fired — driver did not pick up task.spawned")
		return selObservation{}, nil
	}
}

// TestRunLoopDriver_AgentSelection_NamedAgentDrivesTheRun — a task
// naming an agent resolves THAT agent's config end-to-end, and the
// reconcile legs run under the same owner.
func TestRunLoopDriver_AgentSelection_NamedAgentDrivesTheRun(t *testing.T) {
	obs, owners := runSelectionProbe(t, selNamedAgent)

	if obs.model != "named-model" {
		t.Errorf("run model = %q, want named-model (the projections did not follow the named agent)", obs.model)
	}
	if obs.basePrompt != "NAMED-BASE-PROMPT" {
		t.Errorf("run base prompt = %q, want NAMED-BASE-PROMPT", obs.basePrompt)
	}
	if len(owners) == 0 {
		t.Fatal("the reconcile leg never ran — the ordering guard cannot be evaluated")
	}
	for i, o := range owners {
		// THE ORDERING GUARD. `tasks.Get` must precede
		// `reconcileConnections` in runOne; if the reconcile ran first it
		// could only have known the boot agent, and this fails.
		if o.Agent != selNamedAgent {
			t.Errorf("reconcile leg %d ran under owner agent %q, want %q — tasks.Get must precede reconcileConnections",
				i, o.Agent, selNamedAgent)
		}
		if o.Tenant != runLoopDriverTestID.TenantID {
			t.Errorf("reconcile leg %d owner tenant = %q, want %q", i, o.Tenant, runLoopDriverTestID.TenantID)
		}
	}
}

// TestRunLoopDriver_AgentSelection_UnnamedRunKeepsTheBootAgent — the
// unchanged path: a task carrying no AgentID resolves the boot value on
// every plane.
func TestRunLoopDriver_AgentSelection_UnnamedRunKeepsTheBootAgent(t *testing.T) {
	obs, owners := runSelectionProbe(t, "")

	if obs.model != "boot-model" {
		t.Errorf("run model = %q, want boot-model", obs.model)
	}
	if obs.basePrompt != "BOOT-BASE-PROMPT" {
		t.Errorf("run base prompt = %q, want BOOT-BASE-PROMPT", obs.basePrompt)
	}
	for i, o := range owners {
		if o.Agent != selBootAgent {
			t.Errorf("reconcile leg %d ran under owner agent %q, want the boot %q", i, o.Agent, selBootAgent)
		}
	}
}

// TestRunLoopDriver_AgentSelection_CredentialPlaneStaysBootDerived is
// the Ruling-A pin. A run naming agent B under a runtime booted as agent
// A stamps A as southbound provenance — the ONE ctx value both the MCP
// `_meta.agent_id` stamp and the RFC 8693 actor_token read.
//
// MUTATION CHECK: threading the run's effective agent into the
// `tools.WithInvokingAgent` call site in runOne must fail this test.
func TestRunLoopDriver_AgentSelection_CredentialPlaneStaysBootDerived(t *testing.T) {
	obs, _ := runSelectionProbe(t, selNamedAgent)

	if obs.provenance != selBootAgent {
		t.Fatalf("southbound provenance = %q, want the BOOT value %q — a caller-named agent must NOT reach the credential plane",
			obs.provenance, selBootAgent)
	}
	// And the configuration plane DID follow the named agent, so this is
	// a genuine separation rather than "nothing happened".
	if obs.model != "named-model" {
		t.Fatalf("config plane did not follow the named agent (model = %q); the pin would be vacuous", obs.model)
	}
}
