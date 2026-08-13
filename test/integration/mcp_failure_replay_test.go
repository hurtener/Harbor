// test/integration/mcp_failure_replay_test.go — the HA-54 amendment's
// end-to-end gate: a classified MCP `IsError` failure survives the
// complete MCP driver → policy shell → dispatch → runloop → trajectory
// → next ReAct prompt path with its typed class, bounded actionable
// message, retryability/final policy class/attempt outcome, and retained
// bounded result intact — and a generic "tool execution failed" string
// never masks it (CLAUDE.md §17: real drivers on every seam; the MCP
// fixture is a REAL stdio subprocess per §17.8).
//
// The companion unit tests pin the seams (run loop payload assembly in
// internal/runtime/steering, render precedence in
// internal/planner/react, event↔chain agreement in internal/tools); this
// test pins the whole path at once against the production stack
// (devstack: real bifrost LLM against the scripted server, real event
// bus / state / tasks, real mcpdrv.Provider against the live
// subprocess, real dispatch + run loop).
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// mcpFailureConfig loads the shared 83l-style bifrost config (real LLM
// driver pointed at the scripted server) and appends one stdio MCP
// server pointing at the freshly-built fixture binary, with the given
// per-tool reliability policies (keyed by the SERVER-side tool name).
func mcpFailureConfig(t *testing.T, llmServerURL, binPath string, toolPolicies map[string]config.ToolPolicyConfig) *config.Config {
	t.Helper()
	cfg := phase83lConfig(t, llmServerURL)
	cfg.Tools.MCPServers = []config.MCPServerConfig{
		{
			Name:          "mcptest",
			TransportMode: "stdio",
			Command:       []string{binPath},
			ToolPolicies:  toolPolicies,
		},
	}
	return cfg
}

// mcpSpawnAndRun spawns a foreground task with the given goal, waits for
// it to reach a terminal status, and returns its task id (the key the
// RunLoopDriver stores the trajectory under).
func mcpSpawnAndRun(t *testing.T, stack *devstack.DevStack, goal string, maxWait time.Duration) tasks.TaskID {
	t.Helper()
	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    goal,
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	status := waitForTaskTerminal(t, stack, idCtx, h.ID, maxWait)
	if status != tasks.StatusComplete {
		t.Fatalf("task %s terminal status = %s, want Complete", h.ID, status)
	}
	return h.ID
}

// toolStepFromTraj extracts the first trajectory step whose Action JSON
// contains toolName.
func toolStepFromTraj(t *testing.T, traj *planner.Trajectory, toolName string) planner.Step {
	t.Helper()
	if traj == nil {
		t.Fatal("trajectory is nil")
	}
	for i := range traj.Steps {
		if jsonContains(t, traj.Steps[i].Action, toolName) {
			return traj.Steps[i]
		}
	}
	t.Fatalf("no trajectory step dispatched %q; steps=%d", toolName, len(traj.Steps))
	return planner.Step{}
}

// subscribeTerminalToolEvents subscribes to the stack's bus for the
// given terminal tool event types (tool.failed / tool.policy_exhausted)
// and returns the event channel.
func subscribeTerminalToolEvents(t *testing.T, stack *devstack.DevStack, types ...events.EventType) (<-chan events.Event, func()) {
	t.Helper()
	if stack.Bus == nil {
		t.Fatal("stack.Bus is nil — cannot observe tool events")
	}
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: types,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	return sub.Events(), sub.Cancel
}

// drainTerminalToolEvents collects the terminal tool events of the
// wanted type already published (the events are emitted synchronously
// inside the invocation, so by task-terminal time they are buffered on
// the bus). Bounded deadline, never a bare sleep-as-coordination.
func drainTerminalToolEvents(t *testing.T, ch <-chan events.Event, wantType events.EventType, maxWait time.Duration) []events.Event {
	t.Helper()
	deadline := time.After(maxWait)
	var out []events.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			if ev.Type == wantType {
				out = append(out, ev)
			}
		case <-deadline:
			return out
		}
	}
}

// roleToolBodies returns the text bodies of the role:"tool" messages in
// a recorded LLM request — the observation surfaces the planner saw.
func roleToolBodies(req openAIRequestEnvelope) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role == "tool" && m.Content != "" {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestE2E_MCPIsErrorClassificationReplaysIntoNextPrompt is the HA-54
// amendment's acceptance gate. NOT t.Parallel(): phase83lConfig calls
// t.Setenv to populate the fake-provider API-key env var.
func TestE2E_MCPIsErrorClassificationReplaysIntoNextPrompt(t *testing.T) {
	// Build the real stdio MCP server once; all subtests reuse it.
	binPath := buildMCPEchoServer(t)

	// Subtest 1 — acceptance 1, 2, 3, 5 (secrets): a namespaced
	// `conflict` IsError is classified permanent (invoked exactly once),
	// the runloop keeps the typed class / bounded message / policy
	// outcome / bounded result on the step, the next prompt renders them
	// (the generic "tool execution failed" never masks them), and the
	// retained `current_revision` lets the planner choose a reread
	// (scripted: it rereads via echo, then finishes). The raw argument
	// secret sentinel never reaches the observation surfaces.
	t.Run("PermanentConflict_RetainsRevisionForReread", func(t *testing.T) {
		const secretSentinel = "sk-secret-sentinel-91"
		server := newScriptedLLMServer(t,
			// 1. The planner calls the conflict tool (args carry a
			//    secret-shaped sentinel the observation must never echo).
			scriptedToolCallResponse("call_conflict", "mcptest_fail_permanent",
				`{"document_id":"doc-1","secret":"`+secretSentinel+`"}`),
			// 2. Seeing the revision in the observation, the planner
			//    rereads the document once (the echo tool succeeds).
			scriptedToolCallResponse("call_reread", "mcptest_echo",
				`{"message":"reread after conflict; current revision is rev-42"}`),
			// 3. The reread resolved the conflict; the planner finishes.
			scriptedFinishResponse("the document now reads consistently"),
		)

		cfg := mcpFailureConfig(t, server.URL(), binPath, nil)
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()
		if stack.Tasks == nil || stack.RunLoopDriver == nil {
			t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
		}
		toolEvents, cancel := subscribeTerminalToolEvents(t, stack, tools.EventTypeToolFailed)
		defer cancel()

		taskID := mcpSpawnAndRun(t, stack, "handle the document conflict", 20*time.Second)

		traj := stack.RunLoopDriver.TrajectoryByTaskID(taskID)
		step := toolStepFromTraj(t, traj, "mcptest_fail_permanent")

		// The classified payload survives on BOTH observation slots.
		for label, obs := range map[string]any{"Step.Observation": step.Observation, "Step.LLMObservation": step.LLMObservation} {
			encoded, err := json.Marshal(obs)
			if err != nil {
				t.Fatalf("%s marshal: %v", label, err)
			}
			text := string(encoded)
			if !strings.Contains(text, `"`+planner.ObservationMCPClassKey+`":"conflict"`) {
				t.Errorf("%s drops the typed MCP class:\n%s", label, text)
			}
			if !strings.Contains(text, `"`+planner.ObservationPolicyClassKey+`":"permanent"`) {
				t.Errorf("%s drops the retryability projection:\n%s", label, text)
			}
			if !strings.Contains(text, "document changed") || !strings.Contains(text, "rev-42") {
				t.Errorf("%s drops the bounded message / retained revision:\n%s", label, text)
			}
			if strings.Contains(text, secretSentinel) {
				t.Errorf("%s leaks the raw argument secret sentinel:\n%s", label, text)
			}
		}
		if step.Error != "" {
			t.Errorf("Step.Error = %q, want \"\" (a generic stamp must not mask the classified observation)", step.Error)
		}

		// The terminal tool event agrees: one attempt, permanent class.
		evs := drainTerminalToolEvents(t, toolEvents, tools.EventTypeToolFailed, 3*time.Second)
		if len(evs) != 1 {
			t.Fatalf("observed %d tool.failed events, want exactly 1: %+v", len(evs), evs)
		}
		payload, ok := evs[0].Payload.(tools.ToolFailedPayload)
		if !ok {
			t.Fatalf("event payload = %T, want ToolFailedPayload", evs[0].Payload)
		}
		if payload.ErrorClass != tools.ErrClassPermanent || payload.Attempts != 1 {
			t.Errorf("event = {class:%q attempts:%d}, want {permanent, 1} — a permanent class invokes exactly once", payload.ErrorClass, payload.Attempts)
		}

		// The next prompt the planner saw BEFORE the reread carries the
		// class + bounded message + revision — and never the generic
		// stamp, never the secret.
		reqs := server.Requests()
		if len(reqs) < 3 {
			t.Fatalf("scripted LLM saw %d requests, want >= 3 (conflict call + reread + finish)", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		if !strings.Contains(secondPrompt, "conflict") || !strings.Contains(secondPrompt, "document changed") {
			t.Errorf("second prompt drops the classified class/message:\n%s", secondPrompt)
		}
		if !strings.Contains(secondPrompt, "rev-42") {
			t.Errorf("second prompt drops the retained current revision — the planner could not choose a reread:\n%s", secondPrompt)
		}
		if strings.Contains(secondPrompt, "tool execution failed") {
			t.Errorf("second prompt shows only the generic 'tool execution failed' — the classified observation was masked:\n%s", secondPrompt)
		}
		for i, body := range roleToolBodies(reqs[1]) {
			if strings.Contains(body, secretSentinel) {
				t.Errorf("second prompt tool message %d leaks the raw argument secret sentinel:\n%s", i, body)
			}
		}
	})

	// Subtest 2 — acceptance 4: a retryable provider failure follows the
	// CONFIGURED retry attempts (tool_policies max_attempts: 3 → 3 total
	// invocations), and the final prompt + terminal tool event agree with
	// the terminal result (policy class / attempts / budget).
	t.Run("TransientExhaustion_FollowsConfiguredAttempts_AndAgreesWithEvent", func(t *testing.T) {
		server := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_transient", "mcptest_fail_transient", `{}`),
			scriptedFinishResponse("the upstream tool is unavailable"),
		)

		cfg := mcpFailureConfig(t, server.URL(), binPath, map[string]config.ToolPolicyConfig{
			"fail_transient": {MaxAttempts: 3},
		})
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()
		if stack.Tasks == nil || stack.RunLoopDriver == nil {
			t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
		}
		toolEvents, cancel := subscribeTerminalToolEvents(t, stack, tools.EventTypeToolPolicyExhausted)
		defer cancel()

		taskID := mcpSpawnAndRun(t, stack, "ask the flaky upstream tool for a result", 20*time.Second)

		traj := stack.RunLoopDriver.TrajectoryByTaskID(taskID)
		step := toolStepFromTraj(t, traj, "mcptest_fail_transient")
		if step.Error != "" {
			t.Errorf("Step.Error = %q, want \"\" (the terminal policy outcome is classified)", step.Error)
		}
		m, ok := step.LLMObservation.(map[string]any)
		if !ok {
			t.Fatalf("Step.LLMObservation = %#v, want the classified error-observation map", step.LLMObservation)
		}
		if m[planner.ObservationMCPClassKey] != string(tools.MCPToolErrorTransient) {
			t.Errorf("mcp_class = %v, want %q", m[planner.ObservationMCPClassKey], tools.MCPToolErrorTransient)
		}
		if m[planner.ObservationPolicyClassKey] != string(tools.ErrClassTransient) {
			t.Errorf("policy_class = %v, want %q", m[planner.ObservationPolicyClassKey], tools.ErrClassTransient)
		}
		if m[planner.ObservationPolicyAttemptsKey] != 3 {
			t.Errorf("attempts = %v, want 3 (max_attempts:3 followed)", m[planner.ObservationPolicyAttemptsKey])
		}
		if m[planner.ObservationPolicyBudgetKey] != 3 {
			t.Errorf("budget = %v, want 3", m[planner.ObservationPolicyBudgetKey])
		}
		errText, _ := m["error"].(string)
		if !strings.Contains(errText, "upstream hiccup") {
			t.Errorf("error = %q, want the bounded provider message", errText)
		}

		// The terminal tool event agrees with the observation payload:
		// the SAME final class / attempts / budget.
		evs := drainTerminalToolEvents(t, toolEvents, tools.EventTypeToolPolicyExhausted, 3*time.Second)
		if len(evs) != 1 {
			t.Fatalf("observed %d tool.policy_exhausted events, want exactly 1: %+v", len(evs), evs)
		}
		payload, ok := evs[0].Payload.(tools.ToolPolicyExhaustedPayload)
		if !ok {
			t.Fatalf("event payload = %T, want ToolPolicyExhaustedPayload", evs[0].Payload)
		}
		if payload.LastClass != tools.ErrClassTransient || payload.Attempts != 3 || payload.ConfiguredBudget != 3 {
			t.Errorf("event = {class:%q attempts:%d budget:%d}, want {transient, 3, 3} — must agree with the observation payload", payload.LastClass, payload.Attempts, payload.ConfiguredBudget)
		}

		// The next prompt renders the classified terminal outcome, not
		// the generic stamp.
		reqs := server.Requests()
		if len(reqs) < 2 {
			t.Fatalf("scripted LLM saw %d requests, want >= 2 (transient call + finish)", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		if !strings.Contains(secondPrompt, "transient") || !strings.Contains(secondPrompt, "upstream hiccup") {
			t.Errorf("second prompt drops the classified class/message:\n%s", secondPrompt)
		}
		if strings.Contains(secondPrompt, "tool execution failed") {
			t.Errorf("second prompt shows only the generic 'tool execution failed' — the classified observation was masked:\n%s", secondPrompt)
		}
	})

	// Subtest 3 — acceptance 5: an oversized provider message is bounded
	// through the existing projection (truncated to MCPToolErrorMessageLimit)
	// and an oversized provider result is bounded through the heavy-content
	// artifact promotion (preview + ref, never the raw bytes); neither the
	// tail sentinels nor raw args ever reach the prompt/event.
	t.Run("OversizedMessageAndResult_AreBoundedThroughExistingProjection", func(t *testing.T) {
		const msgTailSentinel = "MCP_MSG_TAIL_SENTINEL_77"
		const rawTailSentinel = "MCP_RAW_TAIL_SENTINEL_88"
		server := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_oversized", "mcptest_fail_oversized",
				`{"bulk":"BULK_ARG_SENTINEL_66"}`),
			scriptedFinishResponse("the oversized failure is bounded"),
		)

		// The fixture's fail_oversized carries a PERMANENT class
		// (invalid_argument), so exactly one attempt is made — the
		// assertion is about bounding, not retries.
		cfg := mcpFailureConfig(t, server.URL(), binPath, nil)
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()
		if stack.Tasks == nil || stack.RunLoopDriver == nil {
			t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
		}
		toolEvents, cancel := subscribeTerminalToolEvents(t, stack, tools.EventTypeToolFailed)
		defer cancel()

		taskID := mcpSpawnAndRun(t, stack, "call the noisy failing tool", 20*time.Second)

		traj := stack.RunLoopDriver.TrajectoryByTaskID(taskID)
		step := toolStepFromTraj(t, traj, "mcptest_fail_oversized")

		// The LLM-FACING projection is bounded: the raw oversized bytes
		// and the untruncated message tail never reach Step.LLMObservation
		// — the heavy result rides the artifact promotion (artifact_ref +
		// preview) and the message rides the 512-char bound. (Step.
		// Observation is the RAW canonical record and may persist the
		// full result by the pre-existing heavy-content contract; it is
		// never rendered to the LLM.)
		llmJSON, err := json.Marshal(step.LLMObservation)
		if err != nil {
			t.Fatalf("marshal Step.LLMObservation: %v", err)
		}
		llmText := string(llmJSON)
		for _, forbidden := range []string{msgTailSentinel, rawTailSentinel, "BULK_ARG_SENTINEL_66"} {
			if strings.Contains(llmText, forbidden) {
				t.Errorf("Step.LLMObservation leaks %q:\n%s", forbidden, llmText)
			}
		}
		// The heavy result was promoted: the LLM-facing payload carries
		// the artifact reference (and the bounded preview) — never the
		// raw oversized bytes.
		if !strings.Contains(llmText, "artifact_ref") {
			t.Errorf("Step.LLMObservation does not show the heavy-content artifact promotion (artifact_ref):\n%s", llmText)
		}
		// And the message is bounded: the payload's error text carries
		// the truncated head (with the class) but never the >512-char
		// tail sentinel.
		if !strings.Contains(llmText, "invalid_argument") || !strings.Contains(llmText, "policy_class") {
			t.Errorf("Step.LLMObservation drops the typed class / policy projection:\n%s", llmText)
		}
		// The terminal tool event carries only the bounded message.
		if evs := drainTerminalToolEvents(t, toolEvents, tools.EventTypeToolFailed, 3*time.Second); len(evs) == 1 {
			if fp, ok := evs[0].Payload.(tools.ToolFailedPayload); ok && strings.Contains(fp.ErrorMessage, msgTailSentinel) {
				t.Errorf("tool.failed event carries the untruncated provider message tail")
			}
		}

		// The prompt surfaces the bounded projection and never the raw
		// tail sentinels or the raw argument.
		reqs := server.Requests()
		if len(reqs) < 2 {
			t.Fatalf("scripted LLM saw %d requests, want >= 2", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		for _, forbidden := range []string{msgTailSentinel, rawTailSentinel} {
			if strings.Contains(secondPrompt, forbidden) {
				t.Errorf("second prompt leaks %q:\n%s", forbidden, secondPrompt)
			}
		}
		// The raw argument appears ONLY in the assistant tool_calls
		// replay (the model's own emitted args — the wire contract
		// requires echoing them), never in the observation (tool-role)
		// bodies.
		for i, body := range roleToolBodies(reqs[1]) {
			if strings.Contains(body, "BULK_ARG_SENTINEL_66") {
				t.Errorf("second prompt tool message %d leaks the raw argument bytes:\n%s", i, body)
			}
		}
		if !strings.Contains(secondPrompt, "artifact_ref") {
			t.Errorf("second prompt does not show the bounded heavy-content projection (artifact_ref):\n%s", secondPrompt)
		}
		if strings.Contains(secondPrompt, "tool execution failed") {
			t.Errorf("second prompt shows only the generic 'tool execution failed':\n%s", secondPrompt)
		}
	})
}
