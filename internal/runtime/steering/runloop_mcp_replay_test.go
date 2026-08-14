// internal/runtime/steering/runloop_mcp_replay_test.go — the run loop's
// preservation of a CLASSIFIED MCP failure onto the step's observation:
// the typed provider class, the bounded actionable message, the terminal
// policy projection (retryability class / attempts / budget), and the
// retained bounded result all survive dispatch → runloop → trajectory,
// and a GENERIC Step.Error is never stamped on top of a structured
// classified observation (the HA-54 amendment). Legacy unstructured
// failures keep the generic safe fallback.
//
// External test package (`steering_test`) on purpose: the MCP error
// chain under test is produced by `internal/tools` (which is imported
// by `steering`, so an in-package test importing it would be an import
// cycle). The run loop, the registry, and the coordinator are the real
// production constructors; only the executor is a narrow test double
// returning the exact (observation, llmObservation, error) triple
// `internal/runtime/dispatch` produces for an MCP `IsError` result.

package steering_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// fixedExecutor is a narrow steering.ToolExecutor test double that
// returns a fixed (observation, llmObservation, error) triple — the
// exact shape the production dispatch executor returns for an MCP
// `IsError` result (bounded lowered result + classified error) and for
// a legacy unstructured tool error (nil observations + plain error).
type fixedExecutor struct {
	obs, llmObs any
	err         error
}

func (e *fixedExecutor) ExecuteDecision(_ context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
	return e.obs, e.llmObs, e.err
}

// mcpRunOneStep drives the real run loop over a single decision with
// the caller-supplied executor and returns the recorded step.
func mcpRunOneStep(t *testing.T, exec steering.ToolExecutor, decision planner.Decision) planner.Step {
	t.Helper()
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New())
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-mcp-replay",
			UserID:    "user-mcp-replay",
			SessionID: "session-mcp-replay",
		},
		RunID: "run-mcp-replay",
	}
	traj := &trajectory.Trajectory{}
	if _, err := rl.Run(context.Background(), steering.RunSpec{
		Planner: &classifyPlanner{decision: decision},
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "mcp-replay",
			Trajectory: traj,
		},
		MaxSteps:     4,
		ToolExecutor: exec,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(traj.Steps) != 1 {
		t.Fatalf("trajectory carries %d steps, want 1", len(traj.Steps))
	}
	return traj.Steps[0]
}

// mcpConflictChain builds the exact error chain the production stack
// produces for a permanent MCP `IsError` result: the policy shell's
// terminal PolicyError wrapping the MCPToolResultError (whose bounded
// lowered Result carries the domain value), the shape
// `internal/runtime/dispatch` hands back to the run loop.
func mcpConflictChain(t *testing.T, attempts, budget int) error {
	t.Helper()
	mcpErr := tools.NewMCPToolResultError(tools.MCPToolErrorConflict, "document changed; current revision is rev-42")
	var typed *tools.MCPToolResultError
	if !errors.As(mcpErr, &typed) {
		t.Fatalf("errors.As(mcpErr): %v", mcpErr)
	}
	typed.Result = tools.ToolResult{Value: map[string]any{"current_revision": "rev-42"}}
	return &tools.PolicyError{Err: mcpErr, Attempts: attempts, Budget: budget, Class: tools.ErrClassPermanent}
}

// TestRunLoop_MCPPermanentFailure_PreservesClassMessageResultAndRetry
// is the HA-54 preservation gate at the run-loop seam: the typed MCP
// class, the bounded provider message, the terminal policy projection,
// and the retained bounded result all land on the step's observation —
// and the generic "tool execution failed" stamp is SUPPRESSED so it can
// never mask the richer payload on the ReAct render path.
func TestRunLoop_MCPPermanentFailure_PreservesClassMessageResultAndRetry(t *testing.T) {
	t.Parallel()
	step := mcpRunOneStep(t, &fixedExecutor{
		obs:    map[string]any{"current_revision": "rev-42"},
		llmObs: map[string]any{"current_revision": "rev-42"},
		err:    mcpConflictChain(t, 1, 1),
	}, planner.CallTool{Tool: "mcp_conflict", CallID: "c1", Args: json.RawMessage(`{}`)})

	if step.Error != "" {
		t.Errorf("Step.Error = %q, want \"\" (a generic stamp must not mask the classified observation)", step.Error)
	}
	for label, obs := range map[string]any{"Step.Observation": step.Observation, "Step.LLMObservation": step.LLMObservation} {
		m, ok := obs.(map[string]any)
		if !ok {
			t.Fatalf("%s = %#v, want the classified error-observation map", label, obs)
		}
		if got := m[planner.ObservationMCPClassKey]; got != string(tools.MCPToolErrorConflict) {
			t.Errorf("%s mcp_class = %v, want %q", label, got, tools.MCPToolErrorConflict)
		}
		if got := m[planner.ObservationPolicyClassKey]; got != string(tools.ErrClassPermanent) {
			t.Errorf("%s policy_class = %v, want %q (the retryability the planner may act on)", label, got, tools.ErrClassPermanent)
		}
		if got := m[planner.ObservationPolicyAttemptsKey]; got != 1 {
			t.Errorf("%s attempts = %v, want 1 (a permanent class invokes exactly once)", label, got)
		}
		if got := m[planner.ObservationPolicyBudgetKey]; got != 1 {
			t.Errorf("%s budget = %v, want 1", label, got)
		}
		errText, _ := m["error"].(string)
		if !strings.Contains(errText, string(tools.MCPToolErrorConflict)) || !strings.Contains(errText, "document changed") {
			t.Errorf("%s error = %q, want the bounded message carrying the class", label, errText)
		}
		result, ok := m["result"].(map[string]any)
		if !ok || result["current_revision"] != "rev-42" {
			t.Errorf("%s result = %#v, want the retained bounded result with current_revision=rev-42", label, m["result"])
		}
	}
}

// TestRunLoop_MCPTransientExhausted_PreservesTerminalAttemptOutcome —
// a retryable provider failure follows the policy's retry budget and the
// terminal observation carries the FINAL attempt outcome (class /
// attempts / budget) — the same facts the canonical tool event
// (tool.policy_exhausted) carries, so the event and the planner
// observation agree.
func TestRunLoop_MCPTransientExhausted_PreservesTerminalAttemptOutcome(t *testing.T) {
	t.Parallel()
	mcpErr := tools.NewMCPToolResultError(tools.MCPToolErrorProviderUnavailable, "provider down")
	var typed *tools.MCPToolResultError
	if !errors.As(mcpErr, &typed) {
		t.Fatalf("errors.As(mcpErr): %v", mcpErr)
	}
	typed.Result = tools.ToolResult{Value: map[string]any{"text": "last attempt"}}
	exhausted := &tools.PolicyError{
		Err:      fmt.Errorf("%w: 3 attempts, last class=%s: %w", tools.ErrToolPolicyExhausted, tools.ErrClassTransient, mcpErr),
		Attempts: 3,
		Budget:   3,
		Class:    tools.ErrClassTransient,
	}
	step := mcpRunOneStep(t, &fixedExecutor{
		obs:    map[string]any{"text": "last attempt"},
		llmObs: map[string]any{"text": "last attempt"},
		err:    exhausted,
	}, planner.CallTool{Tool: "mcp_transient", CallID: "c1", Args: json.RawMessage(`{}`)})

	if step.Error != "" {
		t.Errorf("Step.Error = %q, want \"\" (the terminal policy outcome is classified)", step.Error)
	}
	m, ok := step.LLMObservation.(map[string]any)
	if !ok {
		t.Fatalf("Step.LLMObservation = %#v, want the classified error-observation map", step.LLMObservation)
	}
	if got := m[planner.ObservationMCPClassKey]; got != string(tools.MCPToolErrorProviderUnavailable) {
		t.Errorf("mcp_class = %v, want %q", got, tools.MCPToolErrorProviderUnavailable)
	}
	if got := m[planner.ObservationPolicyClassKey]; got != string(tools.ErrClassTransient) {
		t.Errorf("policy_class = %v, want %q", got, tools.ErrClassTransient)
	}
	if got := m[planner.ObservationPolicyAttemptsKey]; got != 3 {
		t.Errorf("attempts = %v, want 3 (the configured retry budget, terminal outcome)", got)
	}
	if got := m[planner.ObservationPolicyBudgetKey]; got != 3 {
		t.Errorf("budget = %v, want 3", got)
	}
	if !strings.Contains(m["error"].(string), "provider down") {
		t.Errorf("error = %q, want the bounded provider message", m["error"])
	}
	if result, ok := m["result"].(map[string]any); !ok || result["text"] != "last attempt" {
		t.Errorf("result = %#v, want the retained bounded final-attempt result", m["result"])
	}
}

// TestRunLoop_LegacyUnstructuredFailure_KeepsGenericFallback pins the
// acceptance-6 leg: a legacy unstructured tool error (no result, no
// class) keeps the generic "tool execution failed" stamp and the
// single-key error observation — byte-identical to what every prior
// turn produced.
func TestRunLoop_LegacyUnstructuredFailure_KeepsGenericFallback(t *testing.T) {
	t.Parallel()
	step := mcpRunOneStep(t, &fixedExecutor{
		err: errors.New("tool blew up"),
	}, planner.CallTool{Tool: "plain", CallID: "c1", Args: json.RawMessage(`{}`)})

	if step.Error != "tool execution failed" {
		t.Errorf("Step.Error = %q, want the generic safe fallback %q", step.Error, "tool execution failed")
	}
	m, ok := step.Observation.(map[string]any)
	if !ok {
		t.Fatalf("Step.Observation = %#v, want the single-key error map", step.Observation)
	}
	if len(m) != 1 || m["error"] != "tool blew up" {
		t.Errorf("Step.Observation = %#v, want exactly {\"error\": \"tool blew up\"}", m)
	}
}

// TestRunLoop_ClassifiedArtifactFailure_SuppressesGenericStamp — the
// planner observation-class leg: a classified failure (the artifact
// reference classes) also suppresses the generic stamp, exactly like an
// MCP class, so its error_class payload reaches the next prompt unmasked.
// Uses the REAL production dispatch executor over the real artifact
// stack (runOneStep from runloop_classification_test.go).
func TestRunLoop_ClassifiedArtifactFailure_SuppressesGenericStamp(t *testing.T) {
	t.Parallel()
	step := runOneStep(t, classifyStack(t, true), artifactDecision("id_the_model_invented"))
	if step.Error != "" {
		t.Errorf("Step.Error = %q, want \"\" (the classified error_class observation must not be masked by the generic stamp)", step.Error)
	}
	if got := observationClass(t, "Step.LLMObservation", step.LLMObservation); got != string(planner.ObservationClassArtifactRefNotFound) {
		t.Errorf("Step.LLMObservation class = %q, want %q", got, planner.ObservationClassArtifactRefNotFound)
	}
}
