package devstack

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tasks"
)

type devstackSkillSnapshotProbe struct {
	mu       sync.RWMutex
	base     skills.SkillStore
	bus      events.EventBus
	expected []string
	result   chan error
}

func newDevstackSkillSnapshotProbe() *devstackSkillSnapshotProbe {
	return &devstackSkillSnapshotProbe{result: make(chan error, 1)}
}

func (p *devstackSkillSnapshotProbe) bind(base skills.SkillStore, bus events.EventBus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = base
	p.bus = bus
}

func (p *devstackSkillSnapshotProbe) expect(names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expected = append([]string(nil), names...)
	sort.Strings(p.expected)
}

func (p *devstackSkillSnapshotProbe) Next(ctx context.Context, run planner.RunContext) (planner.Decision, error) {
	p.mu.RLock()
	base := p.base
	bus := p.bus
	want := append([]string(nil), p.expected...)
	p.mu.RUnlock()

	err := verifyDevstackSkillSnapshot(ctx, run, base, bus, want)
	select {
	case p.result <- err:
	default:
	}
	if err != nil {
		return nil, err
	}
	return planner.Finish{Reason: planner.FinishGoal, Payload: "snapshot verified"}, nil
}

func verifyDevstackSkillSnapshot(
	ctx context.Context,
	run planner.RunContext,
	base skills.SkillStore,
	bus events.EventBus,
	want []string,
) error {
	if base == nil || bus == nil {
		return fmt.Errorf("probe authority is not bound")
	}
	directoryNames := make([]string, 0, len(run.SkillsContext))
	for _, raw := range run.SkillsContext {
		view, ok := raw.(skills.SkillView)
		if !ok {
			return fmt.Errorf("directory row has type %T, want skills.SkillView", raw)
		}
		directoryNames = append(directoryNames, view.Name)
	}
	sort.Strings(directoryNames)
	if !devstackSkillNamesEqual(directoryNames, want) {
		return fmt.Errorf("Directory = %v, want %v", directoryNames, want)
	}

	listed, err := skilltools.ListHandler(ctx, base, bus, skilltools.ListArgs{Limit: 20})
	if err != nil {
		return fmt.Errorf("skill_list: %w", err)
	}
	if got := devstackSkillNames(listed.Skills); !devstackSkillNamesEqual(got, want) {
		return fmt.Errorf("skill_list = %v, want %v", got, want)
	}

	gotten, err := skilltools.GetHandler(ctx, base, bus, skilltools.GetArgs{Names: want, MaxTokens: 4096})
	if err != nil {
		return fmt.Errorf("skill_get: %w", err)
	}
	if got := devstackSkillNames(gotten.Skills); !devstackSkillNamesEqual(got, want) {
		return fmt.Errorf("skill_get = %v, want %v", got, want)
	}

	searched, err := skilltools.SearchHandler(ctx, base, bus, skilltools.SearchArgs{Query: "authority-probe", Limit: 20})
	if err != nil {
		return fmt.Errorf("skill_search: %w", err)
	}
	searchNames := make([]string, len(searched.Skills))
	for i := range searched.Skills {
		searchNames[i] = searched.Skills[i].Skill.Name
	}
	sort.Strings(searchNames)
	if !devstackSkillNamesEqual(searchNames, want) {
		return fmt.Errorf("skill_search = %v, want %v", searchNames, want)
	}
	return nil
}

func devstackSkillNames(rows []skills.Skill) []string {
	names := make([]string, len(rows))
	for i := range rows {
		names[i] = rows[i].Name
	}
	sort.Strings(names)
	return names
}

func devstackSkillNamesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func devstackAuthoritySkill(name string, scope skills.Scope) skills.Skill {
	return skills.Skill{
		Name: name, Title: name, Trigger: "authority-probe " + name,
		Steps: []string{"verify " + name}, Origin: skills.OriginGenerated, Scope: scope,
	}
}

func runDevstackSkillSnapshot(t *testing.T, stack *DevStack, probe *devstackSkillSnapshotProbe, id identity.Identity, agentID string, want ...string) {
	t.Helper()
	probe.bind(stack.Skills, stack.Bus)
	probe.expect(want...)
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    "verify the immutable skill authority",
		AgentID:  agentID,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case probeErr := <-probe.result:
		if probeErr != nil {
			t.Fatalf("planner snapshot probe: %v", probeErr)
		}
	case <-time.After(5 * time.Second):
		task, getErr := stack.Tasks.Get(ctx, handle.ID)
		t.Fatalf("planner snapshot probe timed out: task=%+v getErr=%v", task, getErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, getErr := stack.Tasks.Get(ctx, handle.ID)
		if getErr != nil {
			t.Fatalf("Tasks.Get: %v", getErr)
		}
		switch task.Status {
		case tasks.StatusComplete:
			return
		case tasks.StatusFailed, tasks.StatusCancelled:
			t.Fatalf("snapshot task ended %s: %+v", task.Status, task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("snapshot task %s did not complete", handle.ID)
}

// TestDevStack_RunSkillSnapshotAuthority_SelectedAgentEmptyMembershipAndRestart
// assembles the real devstack graph twice over durable stores. It proves the
// caller-selected agent, not the boot agent, supplies admin membership; the
// Directory and all three skill tools consume one authority; agent and full
// identity-triple foreign session rows cannot bleed into that view; an explicit
// empty admin membership retains only the user/session personal tiers; and
// that exact result survives process reconstruction.
func TestDevStack_RunSkillSnapshotAuthority_SelectedAgentEmptyMembershipAndRestart(t *testing.T) {
	root := t.TempDir()
	cfg := devstackSessionCfg()
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(root, "state.sqlite")}
	cfg.Skills = config.SkillsConfig{
		Driver: "localdb",
		DSN:    filepath.Join(root, "skills.sqlite"),
		SessionPersonalCutover: config.SessionPersonalCutoverConfig{Tenants: []config.SessionPersonalCutoverTenant{
			{TenantID: DefaultDevTenant, Epoch: "phase-233a", RosterDigest: "empty-legacy-roster", LegacyWritersDrained: true},
			{TenantID: "foreign-tenant", Epoch: "phase-233a", RosterDigest: "empty-legacy-roster", LegacyWritersDrained: true},
		}},
	}
	id := identity.Identity{TenantID: DefaultDevTenant, UserID: DefaultDevUser, SessionID: DefaultDevSession}
	q := identity.Quadruple{Identity: id}
	const (
		selectedAgent = "selected-snapshot-agent"
		otherAgent    = "foreign-snapshot-agent"
	)
	firstProbe := newDevstackSkillSnapshotProbe()
	first := Assemble(t, cfg, AssembleOpts{PlannerOverride: firstProbe})
	if first.SessionPersonalSkillAuthority == nil {
		t.Fatal("session personal authority is nil with skills configured")
	}
	for _, skill := range []skills.Skill{
		devstackAuthoritySkill("boot-admin", skills.ScopeGlobal),
		devstackAuthoritySkill("selected-admin", skills.ScopeGlobal),
		devstackAuthoritySkill("user-personal", skills.ScopeUser),
	} {
		if err := first.Skills.Upsert(t.Context(), q, skill); err != nil {
			t.Fatalf("seed %s: %v", skill.Name, err)
		}
	}
	if _, err := first.AgentConfig.SetRevision(t.Context(), q, first.AgentConfigID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"boot-admin"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("boot membership: %v", err)
	}
	if _, err := first.AgentConfig.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"selected-admin"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("selected membership: %v", err)
	}
	if _, err := first.AgentConfig.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"user-personal"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("user membership: %v", err)
	}
	if err := first.SessionPersonalSkillAuthority.Controller.UpsertSessionSkill(
		t.Context(), q, selectedAgent, devstackAuthoritySkill("session-personal", skills.ScopeSession)); err != nil {
		t.Fatalf("session personal upsert: %v", err)
	}
	foreignRows := []struct {
		identity identity.Identity
		agentID  string
		name     string
	}{
		{identity: id, agentID: otherAgent, name: "foreign-agent-session"},
		{identity: identity.Identity{TenantID: DefaultDevTenant, UserID: DefaultDevUser, SessionID: "foreign-session"}, agentID: selectedAgent, name: "foreign-session-personal"},
		{identity: identity.Identity{TenantID: DefaultDevTenant, UserID: "foreign-user", SessionID: DefaultDevSession}, agentID: selectedAgent, name: "foreign-user-personal"},
		{identity: identity.Identity{TenantID: "foreign-tenant", UserID: DefaultDevUser, SessionID: DefaultDevSession}, agentID: selectedAgent, name: "foreign-tenant-personal"},
	}
	for _, foreign := range foreignRows {
		foreignQ := identity.Quadruple{Identity: foreign.identity}
		if foreign.agentID != selectedAgent || foreign.identity.TenantID != DefaultDevTenant {
			if _, err := first.AgentConfig.SetRevision(t.Context(), foreignQ, foreign.agentID, agentcfg.ConfigScopeAgent,
				agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{}}}, agentcfg.SetOptions{}); err != nil {
				t.Fatalf("foreign lifecycle %s/%s: %v", foreign.identity.TenantID, foreign.agentID, err)
			}
		}
		if err := first.SessionPersonalSkillAuthority.Controller.UpsertSessionSkill(
			t.Context(), foreignQ, foreign.agentID, devstackAuthoritySkill(foreign.name, skills.ScopeSession)); err != nil {
			t.Fatalf("foreign session upsert %s: %v", foreign.name, err)
		}
	}
	runDevstackSkillSnapshot(t, first, firstProbe, id, selectedAgent,
		"selected-admin", "session-personal", "user-personal")

	if _, err := first.AgentConfig.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("explicit-empty membership: %v", err)
	}
	runDevstackSkillSnapshot(t, first, firstProbe, id, selectedAgent,
		"session-personal", "user-personal")
	first.Close()

	restartProbe := newDevstackSkillSnapshotProbe()
	restarted := Assemble(t, cfg, AssembleOpts{PlannerOverride: restartProbe})
	defer restarted.Close()
	runDevstackSkillSnapshot(t, restarted, restartProbe, id, selectedAgent,
		"session-personal", "user-personal")
}

// TestDevStack_RunSkillSnapshot_NoBootPacks_DoesNotPanic is the P0
// regression for the default-config run-start snapshot path: a stack with
// skills but NO boot declarations drives the REAL run-loop driver's
// run-start skill snapshot end-to-end. The pre-fix composition handed the
// driver a typed-nil `*bootpacks.Index` inside a non-nil BootPackReader
// interface, bypassing the driver's nil guard and panicking on the first
// run-start Lookup. The actual-nil reader keeps the guard live: the run
// captures a valid no-baseline snapshot and completes.
func TestDevStack_RunSkillSnapshot_NoBootPacks_DoesNotPanic(t *testing.T) {
	cfg := devstackSessionCfg()
	cfg.Skills = config.SkillsConfig{
		Driver: "localdb",
		DSN:    filepath.Join(t.TempDir(), "skills.sqlite"),
	}
	probe := newDevstackSkillSnapshotProbe()
	stack := Assemble(t, cfg, AssembleOpts{PlannerOverride: probe})
	defer stack.Close()
	id := identity.Identity{TenantID: DefaultDevTenant, UserID: DefaultDevUser, SessionID: DefaultDevSession}
	runDevstackSkillSnapshot(t, stack, probe, id, stack.AgentConfigID)
}
