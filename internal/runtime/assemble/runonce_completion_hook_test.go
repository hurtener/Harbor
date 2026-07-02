// runonce_completion_hook_test.go — the embed (RunOnce) half of the
// run-completion hook: a bare embed run resolves the hook from the stack's
// static `runtime.hooks.run_completion` config (the same yaml projection the
// task-driven drivers apply), WithCompletionHook overrides it per call
// (including an explicit-nil disable), and the no-hook default stays
// byte-identical. Without this wiring an embedder's configured hook silently
// no-oped — the §13 silent-degradation shape the review pinned.
package assemble_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// hookSink registers an in-proc transcript sink on the stack's catalog and
// records every payload it receives, keyed by run id. Thread-safe.
type hookSink struct {
	mu       sync.Mutex
	payloads map[string]steering.RunCompletionPayload
	identity map[string]identity.Quadruple
}

func registerHookSink(t *testing.T, stack *assemble.Stack, name string) *hookSink {
	t.Helper()
	s := &hookSink{
		payloads: map[string]steering.RunCompletionPayload{},
		identity: map[string]identity.Quadruple{},
	}
	if err := stack.Catalog.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: name, Description: "run-completion transcript sink (embed test)",
			Transport: tools.TransportInProcess, Source: "runonce-hook-test",
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var p steering.RunCompletionPayload
			if err := json.Unmarshal(args, &p); err != nil {
				return tools.ToolResult{}, err
			}
			q, _ := identity.QuadrupleFrom(ctx)
			s.mu.Lock()
			s.payloads[p.RunID] = p
			s.identity[p.RunID] = q
			s.mu.Unlock()
			return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
		},
	}); err != nil {
		t.Fatalf("register hook sink %q: %v", name, err)
	}
	return s
}

func (s *hookSink) get(runID string) (steering.RunCompletionPayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payloads[runID]
	return p, ok
}

func (s *hookSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

// hookedRunnableStack assembles a runnable mock-LLM stack whose static
// config enables the run-completion hook against yamlTool.
func hookedRunnableStack(t *testing.T, yamlTool string) *assemble.Stack {
	t.Helper()
	cfg := minimalCfg(t)
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	if yamlTool != "" {
		cfg.Runtime.Hooks.RunCompletion = config.RunCompletionHookConfig{
			Tool:    yamlTool,
			Timeout: 5 * time.Second,
		}
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return stack
}

// TestRunOnce_CompletionHook_YamlConfigFires — an embed run with a
// `runtime.hooks.run_completion` config dispatches the transcript to the
// sink, with identity observable at the receiving tool. This is the run
// type the review pinned as silently uncovered.
func TestRunOnce_CompletionHook_YamlConfigFires(t *testing.T) {
	ctx := context.Background()
	stack := hookedRunnableStack(t, "runonce_hook_sink")
	defer func() { _ = stack.Close(ctx) }()
	sink := registerHookSink(t, stack, "runonce_hook_sink")

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-hook"}
	env, err := stack.RunOnce(ctx, "summarise the status", id, assemble.WithRunID("embed-hook-run"))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Fatalf("FinishReason = %q, want goal", env.FinishReason)
	}
	p, ok := sink.get("embed-hook-run")
	if !ok {
		t.Fatal("the yaml-configured hook did not fire for an embed RunOnce run")
	}
	if p.Outcome != "goal" {
		t.Errorf("payload outcome = %q, want goal", p.Outcome)
	}
	if p.TenantID != id.TenantID || p.SessionID != id.SessionID {
		t.Errorf("payload identity = %s/%s, want %s/%s", p.TenantID, p.SessionID, id.TenantID, id.SessionID)
	}
	// The transcript carries the goal and the final answer.
	var sawGoal bool
	for _, e := range p.Conversation {
		if e.Kind == "goal" && e.Content == "summarise the status" {
			sawGoal = true
		}
	}
	if !sawGoal {
		t.Errorf("transcript missing the initial goal: %+v", p.Conversation)
	}
	// Identity at the receiving tool.
	sink.mu.Lock()
	seen := sink.identity["embed-hook-run"]
	sink.mu.Unlock()
	if seen.TenantID != id.TenantID || seen.RunID != "embed-hook-run" {
		t.Errorf("sink ctx identity = %+v, want tenant %s run embed-hook-run", seen, id.TenantID)
	}
}

// TestRunOnce_CompletionHook_OptionOverridesYaml — WithCompletionHook wins
// over the static config: the override sink receives the transcript; the
// yaml sink receives nothing.
func TestRunOnce_CompletionHook_OptionOverridesYaml(t *testing.T) {
	ctx := context.Background()
	stack := hookedRunnableStack(t, "runonce_yaml_sink")
	defer func() { _ = stack.Close(ctx) }()
	yamlSink := registerHookSink(t, stack, "runonce_yaml_sink")
	overrideSink := registerHookSink(t, stack, "runonce_override_sink")

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-hook-override"}
	_, err := stack.RunOnce(ctx, "do the thing", id,
		assemble.WithRunID("embed-hook-override"),
		assemble.WithCompletionHook(&steering.CompletionHookSpec{Tool: "runonce_override_sink", Timeout: 5 * time.Second}))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := overrideSink.get("embed-hook-override"); !ok {
		t.Fatal("WithCompletionHook override sink did not receive the transcript")
	}
	if yamlSink.count() != 0 {
		t.Errorf("yaml sink received %d payloads, want 0 (the option overrides the config)", yamlSink.count())
	}
}

// TestRunOnce_CompletionHook_ExplicitNilDisables — WithCompletionHook(nil)
// disables the hook for the run even when the config enables one.
func TestRunOnce_CompletionHook_ExplicitNilDisables(t *testing.T) {
	ctx := context.Background()
	stack := hookedRunnableStack(t, "runonce_yaml_sink2")
	defer func() { _ = stack.Close(ctx) }()
	yamlSink := registerHookSink(t, stack, "runonce_yaml_sink2")

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-hook-nil"}
	env, err := stack.RunOnce(ctx, "quiet run", id, assemble.WithCompletionHook(nil))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Fatalf("FinishReason = %q, want goal", env.FinishReason)
	}
	if yamlSink.count() != 0 {
		t.Errorf("yaml sink received %d payloads, want 0 (explicit nil disables the hook)", yamlSink.count())
	}
}

// TestRunOnce_CompletionHook_NoConfigNoOption_ByteIdentical — with neither
// the config nor the option, no hook fires and the run behaves exactly as
// before the hook shipped.
func TestRunOnce_CompletionHook_NoConfigNoOption_ByteIdentical(t *testing.T) {
	ctx := context.Background()
	stack := hookedRunnableStack(t, "") // no yaml hook
	defer func() { _ = stack.Close(ctx) }()
	sink := registerHookSink(t, stack, "runonce_unused_sink")

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-nohook"}
	env, err := stack.RunOnce(ctx, "plain run", id)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Fatalf("FinishReason = %q, want goal", env.FinishReason)
	}
	if sink.count() != 0 {
		t.Errorf("sink received %d payloads, want 0 (no hook configured)", sink.count())
	}
}
