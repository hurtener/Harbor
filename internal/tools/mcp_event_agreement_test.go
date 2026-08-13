// internal/tools/mcp_event_agreement_test.go — event/observation
// agreement for classified MCP failures (HA-54 amendment).
//
// The HA-54 amendment requires that the canonical terminal tool event
// (tool.failed / tool.policy_exhausted) and the planner observation the
// run loop builds describe the SAME final class / message / attempt
// outcome. Both are projected from ONE source of truth — the terminal
// *PolicyError chain the reliability shell returns — so this test pins
// the event side of the agreement against the chain, and the run-loop
// seam test (internal/runtime/steering/runloop_mcp_replay_test.go) pins
// the observation side against the same chain shape; the end-to-end
// integration test (test/integration/mcp_failure_replay_test.go) pins
// the two together on the live stack.
//
// External test package (`tools_test`) on purpose: the lifecycle emit
// helpers (newLifecycleBus / lifecycleCtx / registerInvoke) live in the
// external lifecycle test, and the bus wiring under test is the
// production catalog shell.

package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/tools"
)

// mcpPermanentInvoke mimics the MCP driver's Invoke closure: the inner
// call returns a bounded lowered MCP result alongside a typed
// MCPToolResultError, and the reliability shell projects the terminal
// outcome. Returns the invoke closure plus the tool's result value so
// the test can assert the retained bounded result too.
func mcpPermanentInvoke(t *testing.T, resultValue any) func(context.Context, json.RawMessage) (tools.ToolResult, error) {
	t.Helper()
	mcpErr := tools.NewMCPToolResultError(tools.MCPToolErrorConflict, "document changed; current revision is rev-42")
	var typed *tools.MCPToolResultError
	if !errors.As(mcpErr, &typed) {
		t.Fatalf("errors.As(mcpErr): %v", mcpErr)
	}
	typed.Result = tools.ToolResult{Value: resultValue}
	return func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(ctx, args, func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return typed.Result, typed
		}, nil, nil, tools.ToolPolicy{MaxRetries: 4})
	}
}

// TestCatalogLifecycle_MCPPermanentFailureEventAgreesWithObservationProjection
// — a permanent MCP class (conflict) invokes exactly once through the
// reliability shell; the tool.failed event's ErrorClass / Attempts /
// ConfiguredBudget equal the terminal PolicyError chain's Class /
// Attempts / Budget — the exact facts the run loop stamps under the
// planner observation keys
// (planner.ObservationPolicyClassKey / ...AttemptsKey / ...BudgetKey),
// so the event and the planner observation describe the same final
// class / attempt outcome.
func TestCatalogLifecycle_MCPPermanentFailureEventAgreesWithObservationProjection(t *testing.T) {
	t.Parallel()
	bus := newLifecycleBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolFailed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	registerInvoke(t, cat, "mcp_permanent", tools.TransportMCP, mcpPermanentInvoke(t, map[string]any{"current_revision": "rev-42"}))
	desc, _ := cat.Resolve("mcp_permanent")
	_, invokeErr := desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`))
	if invokeErr == nil {
		t.Fatal("Invoke succeeded, want a terminal MCP failure")
	}

	// The terminal chain is the source of truth for BOTH the event and
	// the run loop's observation payload.
	var policyErr *tools.PolicyError
	if !errors.As(invokeErr, &policyErr) {
		t.Fatalf("invoke error %v does not carry the terminal PolicyError", invokeErr)
	}
	var mcpErr *tools.MCPToolResultError
	if !errors.As(invokeErr, &mcpErr) || mcpErr.Class != tools.MCPToolErrorConflict {
		t.Fatalf("invoke error %v does not carry the typed MCP class %q", invokeErr, tools.MCPToolErrorConflict)
	}

	evs := collectTypes(t, sub, 1)
	if len(evs) != 1 {
		t.Fatalf("observed %d events, want 1 tool.failed: %+v", len(evs), evs)
	}
	payload, ok := evs[0].Payload.(tools.ToolFailedPayload)
	if !ok {
		t.Fatalf("event payload = %T, want ToolFailedPayload", evs[0].Payload)
	}
	if payload.ErrorClass != policyErr.Class {
		t.Errorf("event ErrorClass = %q, want the terminal chain class %q (the observation's policy_class mirrors the same chain)", payload.ErrorClass, policyErr.Class)
	}
	if payload.Attempts != policyErr.Attempts {
		t.Errorf("event Attempts = %d, want the terminal chain attempts %d (the observation's attempts mirrors the same chain)", payload.Attempts, policyErr.Attempts)
	}
	if payload.ConfiguredBudget != policyErr.Budget {
		t.Errorf("event ConfiguredBudget = %d, want the terminal chain budget %d", payload.ConfiguredBudget, policyErr.Budget)
	}
	if payload.ErrorMessage == "" || !strings.Contains(payload.ErrorMessage, "document changed") {
		t.Errorf("event ErrorMessage = %q, want the bounded provider message", payload.ErrorMessage)
	}
	if policyErr.Attempts != 1 {
		t.Errorf("permanent class made %d attempts, want exactly 1 (no retry)", policyErr.Attempts)
	}
}

// TestCatalogLifecycle_MCPTransientExhaustedEventAgreesWithObservationProjection
// — a retryable MCP provider class exhausts the configured budget; the
// tool.policy_exhausted event's LastClass / Attempts / ConfiguredBudget
// equal the terminal chain's Class / Attempts / Budget — the same facts
// the run loop stamps on the observation after the FINAL attempt.
func TestCatalogLifecycle_MCPTransientExhaustedEventAgreesWithObservationProjection(t *testing.T) {
	t.Parallel()
	bus := newLifecycleBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolPolicyExhausted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	mcpErr := tools.NewMCPToolResultError(tools.MCPToolErrorProviderUnavailable, "provider down")
	var typed *tools.MCPToolResultError
	if !errors.As(mcpErr, &typed) {
		t.Fatalf("errors.As(mcpErr): %v", mcpErr)
	}
	typed.Result = tools.ToolResult{Value: map[string]any{"text": "final attempt"}}

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	registerInvoke(t, cat, "mcp_transient", tools.TransportMCP, func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(ctx, args, func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return typed.Result, typed
		}, nil, nil, tools.ToolPolicy{MaxRetries: 2, BackoffBase: 0})
	})
	desc, _ := cat.Resolve("mcp_transient")
	_, invokeErr := desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`))
	if invokeErr == nil {
		t.Fatal("Invoke succeeded, want a terminal exhausted MCP failure")
	}

	var policyErr *tools.PolicyError
	if !errors.As(invokeErr, &policyErr) {
		t.Fatalf("invoke error %v does not carry the terminal PolicyError", invokeErr)
	}
	if !errors.Is(invokeErr, tools.ErrToolPolicyExhausted) {
		t.Fatalf("invoke error %v does not wrap ErrToolPolicyExhausted", invokeErr)
	}
	if policyErr.Attempts != 3 {
		t.Fatalf("terminal attempts = %d, want 3 (MaxRetries 2 + the first)", policyErr.Attempts)
	}

	evs := collectTypes(t, sub, 1)
	if len(evs) != 1 {
		t.Fatalf("observed %d events, want 1 tool.policy_exhausted: %+v", len(evs), evs)
	}
	payload, ok := evs[0].Payload.(tools.ToolPolicyExhaustedPayload)
	if !ok {
		t.Fatalf("event payload = %T, want ToolPolicyExhaustedPayload", evs[0].Payload)
	}
	if payload.LastClass != policyErr.Class {
		t.Errorf("event LastClass = %q, want the terminal chain class %q (the observation's policy_class mirrors the same chain)", payload.LastClass, policyErr.Class)
	}
	if payload.Attempts != policyErr.Attempts {
		t.Errorf("event Attempts = %d, want the terminal chain attempts %d (the observation's attempts mirrors the same chain)", payload.Attempts, policyErr.Attempts)
	}
	if payload.ConfiguredBudget != policyErr.Budget {
		t.Errorf("event ConfiguredBudget = %d, want the terminal chain budget %d", payload.ConfiguredBudget, policyErr.Budget)
	}
	if payload.LastError == "" || !strings.Contains(payload.LastError, "provider down") {
		t.Errorf("event LastError = %q, want the bounded provider message", payload.LastError)
	}
}
