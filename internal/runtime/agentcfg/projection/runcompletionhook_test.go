package projection_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// TestActiveRunCompletionHook_Precedence pins the resolution order the two
// run-loop drivers share (§17.6): agent-config `hooks` section over the static
// yaml default over none. The precedence lives in ONE helper so the binaries
// cannot drift.
func TestActiveRunCompletionHook_Precedence(t *testing.T) {
	ctx := context.Background()
	yaml := &steering.CompletionHookSpec{Tool: "yaml-sink", Timeout: 3 * time.Second}

	t.Run("agentcfg over yaml", func(t *testing.T) {
		reg := newRegistry(t)
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Hooks: &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "cfg-sink", TimeoutMS: 7000}},
		}); err != nil {
			t.Fatalf("SetRevision: %v", err)
		}
		got, ok, err := projection.ActiveRunCompletionHook(ctx, reg, projAgent, projID(), yaml)
		if err != nil || !ok {
			t.Fatalf("resolve: ok=%v err=%v", ok, err)
		}
		if got.Tool != "cfg-sink" {
			t.Errorf("Tool = %q, want cfg-sink (agent-config wins over yaml)", got.Tool)
		}
		if got.Timeout != 7*time.Second {
			t.Errorf("Timeout = %v, want 7s (from the agent-config section)", got.Timeout)
		}
		if got.AgentID != projAgent {
			t.Errorf("AgentID = %q, want %q (stamped from the acting agent)", got.AgentID, projAgent)
		}
	})

	t.Run("yaml when no agentcfg hook", func(t *testing.T) {
		reg := newRegistry(t) // no hooks section set
		got, ok, err := projection.ActiveRunCompletionHook(ctx, reg, projAgent, projID(), yaml)
		if err != nil || !ok {
			t.Fatalf("resolve: ok=%v err=%v", ok, err)
		}
		if got.Tool != "yaml-sink" || got.Timeout != 3*time.Second {
			t.Errorf("resolved = %q/%v, want yaml-sink/3s (fall through to yaml)", got.Tool, got.Timeout)
		}
		if got.AgentID != projAgent {
			t.Errorf("AgentID = %q, want %q (stamped even on the yaml leg)", got.AgentID, projAgent)
		}
		// The returned spec is a fresh copy — mutating it must not touch the
		// shared boot-time default.
		got.Tool = "mutated"
		if yaml.Tool != "yaml-sink" {
			t.Error("resolving mutated the shared yaml default (must return a copy)")
		}
	})

	t.Run("agentcfg with empty tool falls through to yaml", func(t *testing.T) {
		reg := newRegistry(t)
		// An empty-tool hooks section normalises away — it must NOT shadow yaml.
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Hooks: &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: ""}},
		}); err != nil {
			t.Fatalf("SetRevision: %v", err)
		}
		got, ok, err := projection.ActiveRunCompletionHook(ctx, reg, projAgent, projID(), yaml)
		if err != nil || !ok {
			t.Fatalf("resolve: ok=%v err=%v", ok, err)
		}
		if got.Tool != "yaml-sink" {
			t.Errorf("Tool = %q, want yaml-sink (empty agentcfg tool falls through)", got.Tool)
		}
	})

	t.Run("none when neither set", func(t *testing.T) {
		reg := newRegistry(t)
		got, ok, err := projection.ActiveRunCompletionHook(ctx, reg, projAgent, projID(), nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ok || got != nil {
			t.Errorf("resolved = %+v, ok=%v, want (nil, false)", got, ok)
		}
	})

	t.Run("nil registry uses yaml", func(t *testing.T) {
		got, ok, err := projection.ActiveRunCompletionHook(ctx, nil, projAgent, projID(), yaml)
		if err != nil || !ok || got.Tool != "yaml-sink" {
			t.Fatalf("nil-registry resolve = %+v ok=%v err=%v, want yaml-sink", got, ok, err)
		}
	})
}
