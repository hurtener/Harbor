// MCP agent-calls-tool end-to-end integration test (CLAUDE.md §17).
//
// The existing MCP integration test
// (phase83g_mcp_dev_consumer_test.go) proves a configured stdio MCP
// server is spawned at boot and its tools reach the tool catalog —
// the discovery leg. The existing executor-level tool-invocation test
// (phase83l_real_bifrost_test.go) drives a goal through
// planner → executor → trajectory, but the tool it dispatches is an
// in-process BUILTIN (`text.echo`), not an MCP-sourced tool. Neither
// proves the agent-call leg for an MCP tool: that the planner can
// decide to call a tool discovered from a real MCP server and the
// runtime dispatches it THROUGH the executor to the live subprocess,
// routing the result back into the trajectory.
//
// This test closes that gap. It boots the SAME real stdio MCP server
// fixture (`cmd/harbor-mcptest-stdio`, whose one tool — `echo` —
// lands in the catalog as `mcptest_echo`), assembles a devstack with
// that server in config, and drives a goal whose scripted LLM decides
// to call `mcptest_echo`. The load-bearing assertion is that the
// echoed sentinel — produced ONLY by an actual dispatch to the
// subprocess — appears in the CallTool step's OBSERVATION (the
// executor's result, not the planner's input args) and is fed back
// into the planner's next prompt.
//
// §17.8 (external-protocol fixtures derive from the real spec): the
// fixture is a REAL MCP stdio server driven over the real wire, not a
// hand-authored in-test fixture that could pass while the code is
// wired to the wrong field.
//
// §17.3: real drivers everywhere on the seam (real bifrost LLM driver
// against a scripted OpenAI-compatible server, real EventBus,
// StateStore, Coordinator, tools catalog, `mcpdrv.Provider` against a
// real stdio subprocess); identity propagated end-to-end (the MCP
// driver fails closed without the triple, so a successful echo proves
// the triple reached the MCP `_meta`; plus a cross-identity
// `Tasks.Get` isolation assertion); and a failure mode (bad args
// rejected by the server, surfaced through the executor, the planner
// re-plans). `-race` is the gate.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// mcpCallDispatchSentinel is a unique token the goal asks the MCP echo
// tool to return. The `harbor-mcptest-stdio` fixture echoes its
// `message` argument verbatim, so this exact string can ONLY appear in
// a step's observation if the executor actually dispatched the call to
// the live subprocess and routed the result back. A test that merely
// lists the catalog can never produce it.
const mcpCallDispatchSentinel = "HARBOR_MCP_DISPATCH_SENTINEL_136"

// buildMCPEchoServer compiles the `cmd/harbor-mcptest-stdio` fixture
// into a per-test tempdir and returns its absolute path. Mirrors the
// package-`integration_test` helper of the same intent
// (phase83g_mcp_dev_consumer_test.go) — duplicated here because that
// helper lives in the external test package and is not visible from
// package `integration`, where the scripted-bifrost harness this test
// reuses lives.
func buildMCPEchoServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "harbor-mcptest-stdio")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/harbor-mcptest-stdio")
	cmd.Dir = mcpCallRepoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build cmd/harbor-mcptest-stdio: %v\n%s", err, out)
	}
	return binPath
}

// mcpCallRepoRoot walks up from the test's working directory until it
// finds a `go.mod` — the repo root — so `go build` resolves the
// fixture package no matter where `go test` runs.
func mcpCallRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("mcpCallRepoRoot: walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// withMCPEchoServer loads the shared 83l-style bifrost config (real
// LLM driver pointed at the scripted server) and appends one stdio MCP
// server pointing at the freshly-built fixture binary. The fixture's
// `echo` tool lands in the catalog as `mcptest_echo` (source-prefix +
// underscore separator).
func withMCPEchoServer(t *testing.T, llmServerURL, binPath string) *config.Config {
	t.Helper()
	cfg := phase83lConfig(t, llmServerURL)
	cfg.Tools.MCPServers = []config.MCPServerConfig{
		{
			Name:          "mcptest",
			TransportMode: "stdio",
			Command:       []string{binPath},
		},
	}
	return cfg
}

// jsonContains marshals v and reports whether the canonical JSON
// encoding contains needle. Used to substring-search a trajectory step
// observation (typed `any`) without depending on its concrete shape.
func jsonContains(t *testing.T, v any, needle string) bool {
	t.Helper()
	if v == nil {
		return false
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal observation for substring search: %v", err)
	}
	return strings.Contains(string(b), needle)
}

// TestE2E_Phase83g_MCPAgentCallsTool — the agent-call leg for an
// MCP-sourced tool. The exact test name is pinned by the v1.8.0
// adopter-path wave coordination doc: the original gate regex
// (`…Phase83g.*Call`) matched ZERO tests (the existing 83g test is
// `…ReachTheCatalog`), a SKIP-that-should-be-OK false-green. The
// companion smoke (scripts/smoke/phase-136.sh) carries a
// `-list`/no-match-fails guard so the gate can never silently match
// nothing again.
func TestE2E_Phase83g_MCPAgentCallsTool(t *testing.T) {
	// Build the real stdio MCP server once; both subtests reuse it.
	// NOT t.Parallel(): the scripted-bifrost config calls t.Setenv to
	// populate the fake-provider API-key env var.
	binPath := buildMCPEchoServer(t)

	t.Run("DispatchThroughExecutor", func(t *testing.T) {
		// The scripted LLM decides to call the MCP tool, then finishes.
		server := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_mcp_echo", "mcptest_echo",
				`{"message":"`+mcpCallDispatchSentinel+`"}`),
			scriptedFinishResponse("the echo tool returned the message"),
		)

		cfg := withMCPEchoServer(t, server.URL(), binPath)
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()

		if stack.Tasks == nil || stack.RunLoopDriver == nil {
			t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
		}
		// Precondition: the MCP tool reached the catalog (the discovery
		// leg the existing 83g test owns). The value this test adds is
		// the DISPATCH leg below, not this resolve.
		if stack.Catalog == nil {
			t.Fatal("devstack: Catalog is nil — MCP discovery path skipped")
		}
		if _, ok := stack.Catalog.Resolve("mcptest_echo"); !ok {
			t.Fatal("catalog: mcptest_echo not registered — MCP discovery did not reach the catalog")
		}

		devID := identity.Identity{
			TenantID:  devstack.DefaultDevTenant,
			UserID:    devstack.DefaultDevUser,
			SessionID: devstack.DefaultDevSession,
		}
		idCtx, err := identity.With(context.Background(), devID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		seedQ := identity.Quadruple{Identity: devID}

		h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
			Identity: seedQ,
			Kind:     tasks.KindForeground,
			Query:    "ask the echo tool to return '" + mcpCallDispatchSentinel + "'",
		})
		if err != nil {
			t.Fatalf("Tasks.Spawn: %v", err)
		}

		status := waitForTaskTerminal(t, stack, idCtx, h.ID, 15*time.Second)
		if status != tasks.StatusComplete {
			t.Fatalf("task terminal status = %s, want Complete (CallTool(mcptest_echo)+Finish should succeed)", status)
		}

		// --- Dispatch-through-executor signal (load-bearing) ---------
		traj := stack.RunLoopDriver.TrajectoryByTaskID(h.ID)
		if traj == nil {
			t.Fatal("RunLoopDriver.TrajectoryByTaskID returned nil — the driver discarded the trajectory")
		}
		var dispatched, obsHasSentinel bool
		for i := range traj.Steps {
			step := traj.Steps[i]
			if !jsonContains(t, step.Action, "mcptest_echo") {
				continue
			}
			// This is the CallTool(mcptest_echo) step. The SENTINEL in
			// the OBSERVATION (the executor's result) — not in the
			// Action's input args — is the proof the executor dispatched
			// the call to the live MCP subprocess and routed the result
			// back. A catalog-listing test produces no observation.
			dispatched = true
			obsHasSentinel = jsonContains(t, step.Observation, mcpCallDispatchSentinel) ||
				jsonContains(t, step.LLMObservation, mcpCallDispatchSentinel)
			break
		}
		if !dispatched {
			t.Fatalf("no trajectory step dispatched mcptest_echo — the planner never invoked the MCP tool; steps=%d", len(traj.Steps))
		}
		if !obsHasSentinel {
			t.Fatal("the mcptest_echo step carries no echoed sentinel in its observation — the call did not dispatch through the executor to the real MCP subprocess (a catalog-listing test would also fail here, which is the point)")
		}

		// Corroboration that the echoed result round-tripped into the
		// planner's NEXT prompt. The sentinel appears ONCE in the persistent
		// goal message (rendered into every prompt), so a SECOND occurrence
		// is the discriminating signal that the tool OBSERVATION was rendered
		// into the trajectory — a broken observation render would leave only
		// the single goal occurrence. (A bare strings.Contains would be
		// trivially satisfied by the goal alone and prove nothing.)
		reqs := server.Requests()
		if len(reqs) < 2 {
			t.Fatalf("scripted LLM saw %d requests, want >= 2 (CallTool + Finish)", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		if n := strings.Count(secondPrompt, mcpCallDispatchSentinel); n < 2 {
			t.Errorf("second LLM prompt contains the echoed sentinel %d time(s), want >= 2 (goal occurrence + rendered observation) — the MCP tool result did not round-trip into the planner\nsecond prompt:\n%s", n, secondPrompt)
		}

		// --- Identity propagation + cross-identity isolation ---------
		// The successful echo above already proves identity reached the
		// MCP dispatch: the MCP driver fails closed with
		// ErrIdentityMissing when the per-invocation ctx has no triple,
		// so an echoed result is impossible without the triple flowing
		// run → planner → executor → MCP `_meta`. The Tasks.Get pair
		// below pins the isolation half: the run is visible to its own
		// identity and rejected (ErrNotFound) for a different tenant.
		if _, err := stack.Tasks.Get(idCtx, h.ID); err != nil {
			t.Fatalf("Tasks.Get with the run's own identity: unexpected error %v", err)
		}
		intruderCtx, err := identity.With(context.Background(), identity.Identity{
			TenantID:  "intruder-tenant",
			UserID:    "intruder-user",
			SessionID: "intruder-session",
		})
		if err != nil {
			t.Fatalf("identity.With(intruder): %v", err)
		}
		if _, err := stack.Tasks.Get(intruderCtx, h.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("Tasks.Get with a foreign identity: err = %v, want tasks.ErrNotFound (cross-identity read must be rejected)", err)
		}
	})

	t.Run("BadArgsRejectedThroughExecutor", func(t *testing.T) {
		// Failure mode (§17.3): the planner calls the MCP tool with a
		// wrong-typed argument (`message` as a number). MCP tools carry
		// no client-side validator (Validate: nil — the server validates
		// on the wire), so the bad args reach the live subprocess, the
		// go-sdk server rejects them, the executor surfaces the error as
		// the step observation, and the planner re-plans to a Finish.
		server := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_bad_mcp", "mcptest_echo", `{"message":12345}`),
			scriptedFinishResponse("I could not call the echo tool — bad arguments."),
		)

		cfg := withMCPEchoServer(t, server.URL(), binPath)
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()

		if stack.Tasks == nil || stack.RunLoopDriver == nil {
			t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
		}

		devID := identity.Identity{
			TenantID:  devstack.DefaultDevTenant,
			UserID:    devstack.DefaultDevUser,
			SessionID: devstack.DefaultDevSession,
		}
		idCtx, err := identity.With(context.Background(), devID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		seedQ := identity.Quadruple{Identity: devID}

		h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
			Identity: seedQ,
			Kind:     tasks.KindForeground,
			Query:    "try to echo something with bad arguments",
		})
		if err != nil {
			t.Fatalf("Tasks.Spawn: %v", err)
		}

		status := waitForTaskTerminal(t, stack, idCtx, h.ID, 15*time.Second)
		if status != tasks.StatusComplete {
			t.Fatalf("task terminal status = %s, want Complete (the re-plan should reach Finish)", status)
		}

		// The failing call must have been DISPATCHED (reached the
		// executor → subprocess) and surfaced an error observation the
		// planner could re-plan against: the second prompt carries both
		// the attempted tool name and an error trace.
		traj := stack.RunLoopDriver.TrajectoryByTaskID(h.ID)
		if traj == nil {
			t.Fatal("RunLoopDriver.TrajectoryByTaskID returned nil")
		}
		foundErrStep := false
		for i := range traj.Steps {
			step := traj.Steps[i]
			if !jsonContains(t, step.Action, "mcptest_echo") {
				continue
			}
			// The executor stamps the failed dispatch with the MCP wire
			// error, wrapped with the documented "mcp:" prefix
			// (ErrMCPToolError). Asserting that prefix — not merely the
			// generic word "error" — pins the rejection to a real
			// subprocess/wire failure rather than any pre-dispatch error.
			if jsonContains(t, step.Observation, "mcp:") || jsonContains(t, step.LLMObservation, "mcp:") || strings.Contains(step.Error, "mcp:") {
				foundErrStep = true
			}
			break
		}
		if !foundErrStep {
			t.Fatal("the mcptest_echo step carries no mcp:-prefixed error observation — the bad-args dispatch was not rejected through the executor by the real MCP subprocess")
		}

		reqs := server.Requests()
		if len(reqs) < 2 {
			t.Fatalf("scripted LLM saw %d requests, want >= 2 (bad CallTool + re-plan + Finish)", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		// `mcptest_echo` appears ONCE in every prompt's available-tools
		// block, so a SECOND occurrence is the discriminating signal that
		// the failed CallTool turn was rendered back into the trajectory for
		// the planner to re-plan against — a bare Contains would be satisfied
		// by the tool catalog alone and prove nothing.
		if n := strings.Count(secondPrompt, "mcptest_echo"); n < 2 {
			t.Errorf("second LLM prompt references mcptest_echo %d time(s), want >= 2 (tool catalog + rendered failed call) — the error observation did not round-trip into the planner\nsecond prompt:\n%s", n, secondPrompt)
		}
	})
}
