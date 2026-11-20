package devstack_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

type revisionApprovalPlanner struct {
	frozen  chan struct{}
	release chan struct{}
}

func (p revisionApprovalPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if len(rc.Trajectory.Steps) != 0 {
		return planner.Finish{Reason: planner.FinishGoal, Payload: "done"}, nil
	}
	call := planner.CallTool{Tool: rc.Goal, Args: json.RawMessage(`{"value":"original"}`)}
	switch rc.Goal {
	case "frozen":
		close(p.frozen)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		call.Tool = "revision_gated"
	case "parallel":
		call.Tool = "revision_gated"
		return planner.CallParallel{Branches: []planner.CallTool{call}, Join: &planner.JoinSpec{Kind: planner.JoinAll}}, nil
	case "declarative":
		call.Tool = "declarative_action"
		call.Args = json.RawMessage(`{"tool":"revision_gated","args":{"value":"original"}}`)
	}
	return call, nil
}

func TestDevStack_RevisionApprovalDispatch(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.Tools.BuiltIn = []string{"declarative_action"}
	cfg.Tools.ApprovalRules = []config.ToolApprovalRuleConfig{
		{Tools: []string{"boot_gated"}, Approval: config.ToolApprovalConfig{Policy: "deny-all", Reason: "boot rule"}},
		{Tools: []string{"boot_tag_mismatch"}, Approval: config.ToolApprovalConfig{Policy: "tagged", RequireTags: []string{"other"}}},
	}
	cfg.Tools.Entries = []config.ToolEntryConfig{
		{Name: "entry_gated", Approval: &config.ToolApprovalConfig{Policy: "deny-all", Reason: "tool entry"}},
		{Name: "entry_allow", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
	}
	probe := revisionApprovalPlanner{frozen: make(chan struct{}), release: make(chan struct{})}
	calls := map[string]*atomic.Int32{}
	opts := devstack.AssembleOpts{PlannerOverride: probe, SkipAuth: true, SkipTransports: true}
	for _, name := range []string{"revision_gated", "boot_gated", "boot_tag_mismatch", "entry_gated", "entry_allow", "untagged", "untouched"} {
		count := new(atomic.Int32)
		calls[name] = count
		tool := tools.Tool{Name: name}
		if name != "untagged" {
			tool.Tags = []string{"sensitive"}
		}
		opts.PreRegisterTools = append(opts.PreRegisterTools, tools.ToolDescriptor{
			Tool: tool,
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				if string(args) != `{"value":"original"}` {
					t.Errorf("%s received changed arguments %s", name, args)
				}
				count.Add(1)
				return tools.ToolResult{Value: "called"}, nil
			},
		})
	}
	stack := devstack.Assemble(t, cfg, opts)
	defer stack.Close()
	id := identity.Identity{TenantID: devstack.DefaultDevTenant, UserID: devstack.DefaultDevUser, SessionID: devstack.DefaultDevSession}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ctx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	setRules := func(t *testing.T, agent string, section *agentcfg.ApprovalSection) {
		t.Helper()
		if _, err := stack.AgentConfig.SetRevision(ctx, identity.Quadruple{Identity: id}, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Approval: section}, agentcfg.SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(t *testing.T, agent, goal string) (identity.Quadruple, events.Subscription) {
		t.Helper()
		sub, err := stack.Bus.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
			Types: []events.EventType{approval.EventTypeToolApprovalRequested, tasks.EventTypeTaskCompleted, tasks.EventTypeTaskFailed}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sub.Cancel)
		h, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, AgentID: agent, Query: goal})
		if err != nil {
			t.Fatal(err)
		}
		return identity.Quadruple{Identity: id, RunID: string(h.ID)}, sub
	}
	readEvent := func(t *testing.T, sub events.Subscription) events.Event {
		t.Helper()
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("approval subscription closed")
			}
			return ev
		case <-ctx.Done():
			t.Fatalf("waiting for approval or completion: %v", ctx.Err())
			return events.Event{}
		}
	}
	checkRun := func(t *testing.T, q identity.Quadruple, sub events.Subscription, tool, reason string, decision steering.ControlType, callsBefore int32) {
		t.Helper()
		ev := readEvent(t, sub)
		if reason != "" {
			p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
			if !ok || p.Tool != tool || p.Reason != reason || ev.Identity != q || p.PauseToken == "" {
				t.Fatalf("approval for %s = %s %+v", tool, ev.Type, ev.Payload)
			}
			if calls[tool].Load() != callsBefore {
				t.Fatalf("%s invoked before approval", tool)
			}
			inbox, err := stack.Steering.Lookup(q)
			if err != nil {
				t.Fatal(err)
			}
			if err := inbox.Enqueue(steering.ControlEvent{Type: decision, Identity: q, CallerTenant: id.TenantID, CallerScope: steering.ScopeOwnerUser, Payload: map[string]any{"token": p.PauseToken}}); err != nil {
				t.Fatal(err)
			}
			ev = readEvent(t, sub)
		}
		if ev.Type != tasks.EventTypeTaskCompleted {
			task, err := stack.Tasks.Get(ctx, tasks.TaskID(q.RunID))
			t.Fatalf("expected completion after governing %s, saw %s, task=%+v, err=%v", tool, ev.Type, task, err)
		}
		want := callsBefore
		if decision != steering.ControlReject {
			want++
		}
		if got := calls[tool].Load(); got != want {
			t.Fatalf("%s invoked %d times, want %d", tool, got, want)
		}
	}
	t.Run("before revision", func(t *testing.T) {
		q, sub := start(t, "", "revision_gated")
		checkRun(t, q, sub, "revision_gated", "", "", 0)
	})
	setRules(t, stack.AgentConfigID, &agentcfg.ApprovalSection{Rules: []agentcfg.ApprovalRule{
		{
			Tools:    []string{"revision_gated", "boot_gated", "boot_tag_mismatch", "entry_gated", "entry_allow", "untagged"},
			Approval: agentcfg.ApprovalPolicy{Policy: "tagged", RequireTags: []string{"sensitive"}, Reason: "agent revision"},
		},
		{Tools: []string{"revision_gated", "untagged"}, Approval: agentcfg.ApprovalPolicy{Policy: "deny-all", Reason: "later rule"}},
	}})
	const otherAgent = "other-agent"
	setRules(t, otherAgent, nil)
	for _, tc := range []struct {
		name, agent, goal, tool, reason string
		decision                        steering.ControlType
	}{
		{name: "approve", goal: "revision_gated", tool: "revision_gated", reason: "agent revision", decision: steering.ControlApprove},
		{name: "reject", goal: "revision_gated", tool: "revision_gated", reason: "agent revision", decision: steering.ControlReject},
		{name: "parallel", goal: "parallel", tool: "revision_gated", reason: "agent revision", decision: steering.ControlApprove},
		{name: "declarative", goal: "declarative", tool: "revision_gated", reason: "agent revision", decision: steering.ControlApprove},
		{name: "boot overlap", goal: "boot_gated", tool: "boot_gated", reason: "boot rule", decision: steering.ControlApprove},
		{name: "boot tag mismatch", goal: "boot_tag_mismatch", tool: "boot_tag_mismatch", reason: "agent revision", decision: steering.ControlApprove},
		{name: "entry overlap", goal: "entry_gated", tool: "entry_gated", reason: "tool entry", decision: steering.ControlApprove},
		{name: "approve-all entry", goal: "entry_allow", tool: "entry_allow", reason: "agent revision", decision: steering.ControlApprove},
		{name: "first matching rule", goal: "untagged", tool: "untagged"},
		{name: "unmatched tool", goal: "untouched", tool: "untouched"},
		{name: "different agent", agent: otherAgent, goal: "revision_gated", tool: "revision_gated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls[tc.tool].Load()
			q, sub := start(t, tc.agent, tc.goal)
			checkRun(t, q, sub, tc.tool, tc.reason, tc.decision, before)
		})
	}
	t.Run("in-flight run keeps removed rule", func(t *testing.T) {
		before := calls["revision_gated"].Load()
		q, sub := start(t, "", "frozen")
		select {
		case <-probe.frozen:
		case <-ctx.Done():
			t.Fatalf("run did not reach planner: %v", ctx.Err())
		}
		setRules(t, stack.AgentConfigID, &agentcfg.ApprovalSection{})
		close(probe.release)
		checkRun(t, q, sub, "revision_gated", "agent revision", steering.ControlApprove, before)
	})
	t.Run("next run sees removal", func(t *testing.T) {
		before := calls["revision_gated"].Load()
		q, sub := start(t, "", "revision_gated")
		checkRun(t, q, sub, "revision_gated", "", "", before)
	})
}
