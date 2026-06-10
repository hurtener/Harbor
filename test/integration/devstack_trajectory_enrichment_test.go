// Devstack trajectory-enrichment parity test — the D-195 dated-note
// follow-up (program follow-ups chore, 2026-06-10).
//
// The Wave B checkpoint audit recorded one residual cmd↔devstack
// driver-shell divergence: production (`cmd/harbor`) registers per-run
// trajectories on its run-loop driver and wires the tasks-protocol
// Enricher (`devEnricher` over `TrajectoryByTaskID`), while devstack
// discarded the trajectory and wired no enricher — so a devstack
// `tasks.get` read carried no trajectory enrichment and the official
// test surface validated weaker semantics than production ships
// (§17.6). This test pins the closed gap end-to-end:
//
//  1. A devstack-run task whose planner emits a reasoning trace
//     produces a `tasks.get` wire response (POST /v1/tasks/get through
//     the real transports + Phase 61 auth middleware) whose
//     `trajectory.steps` carry the trace.
//  2. The driver-side accessor (`RunLoopDriver.TrajectoryByTaskID`)
//     returns the retained per-run trajectory post-completion — the
//     D-094 mirror of production's accessor.
//  3. Failure mode (§17.3 #3): a `tasks.get` for an unknown id fails
//     closed (404) — the enricher never fabricates a trajectory.
//
// Real drivers everywhere on the seam (§17.3): real audit redactor,
// real inmem bus / state / tasks registry, real steering.RunLoop +
// dispatch executor, real ReAct planner, real wire transport + JWT
// validator — assembled through `harbortest/devstack.Assemble`. The
// scripted LLM is the only stub (it replays a deterministic
// reasoning + tool-call + answer sequence so the trace is hermetic),
// per the Phase 110b parity-test precedent.

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// devstackTrajReasoning is the deterministic provider-side trace the
// scripted LLM emits and the wire read must surface.
const devstackTrajReasoning = "scripted reasoning for trajectory parity"

// devstackTrajLLM replays: call 1 — a reasoning trace + one regular
// tool-call (to a tool the catalog does not declare, so the executor's
// loud error observation drives the repair loop and the step is
// appended with the trace); call 2 — the terminal content answer.
type devstackTrajLLM struct {
	mu    sync.Mutex
	calls int
}

func (c *devstackTrajLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	n := c.calls
	c.calls++
	c.mu.Unlock()
	if n == 0 {
		return llm.CompleteResponse{
			Reasoning: devstackTrajReasoning,
			ToolCalls: []llm.ToolCallStructured{{
				ID:   "traj-call-1",
				Name: "tool_the_catalog_never_declared",
				Args: json.RawMessage(`{}`),
			}},
		}, nil
	}
	return llm.CompleteResponse{Content: "trajectory parity answer"}, nil
}

func (c *devstackTrajLLM) Close(_ context.Context) error { return nil }

// TestE2E_DevstackTasksGet_CarriesTrajectoryEnrichment is the parity
// gate: the devstack `tasks.get` wire read carries the trajectory
// enrichment, exactly as production's does.
func TestE2E_DevstackTasksGet_CarriesTrajectoryEnrichment(t *testing.T) {
	t.Parallel()

	cfg := phase110bConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		PlannerOverride:   react.New(&devstackTrajLLM{}),
	})
	defer stack.Close()
	if stack.RunLoopDriver == nil {
		t.Fatal("stack.RunLoopDriver is nil — the assembly produced no run-loop driver")
	}
	if stack.Handler == nil || stack.Token == "" {
		t.Fatal("stack.Handler / stack.Token missing — the wire surface is not assembled")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

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
		Query:    "trajectory enrichment parity",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	// Wait for the terminal status (bounded poll — the registry has no
	// completion channel surface at this layer).
	waitDeadline := time.Now().Add(15 * time.Second)
	for {
		got, gErr := stack.Tasks.Get(idCtx, h.ID)
		if gErr == nil && got.Status == tasks.StatusComplete {
			break
		}
		if gErr == nil && got.Status == tasks.StatusFailed {
			t.Fatalf("task failed: %+v", got.Error)
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("task never reached a terminal status")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// (1) Driver-side accessor parity: the trajectory is retained
	// post-completion and carries the scripted trace.
	traj := stack.RunLoopDriver.TrajectoryByTaskID(h.ID)
	if traj == nil {
		t.Fatal("RunLoopDriver.TrajectoryByTaskID returned nil — the devstack driver discarded the trajectory")
	}
	foundTrace := false
	for _, step := range traj.Steps {
		if step.ReasoningTrace == devstackTrajReasoning {
			foundTrace = true
		}
	}
	if !foundTrace {
		t.Fatalf("retained trajectory carries no scripted reasoning trace; steps=%d", len(traj.Steps))
	}

	// (2) The wire read: POST /v1/tasks/get through the real transport
	// + auth middleware carries the enrichment.
	status, body := devstackPostTasksGet(t, srv.URL, `{"id":"`+string(h.ID)+`"}`, stack.Token)
	if status != http.StatusOK {
		t.Fatalf("tasks.get status = %d, want 200; body=%s", status, body)
	}
	var detail prototypes.TaskDetail
	if uErr := json.Unmarshal(body, &detail); uErr != nil {
		t.Fatalf("decode TaskDetail: %v; body=%s", uErr, body)
	}
	if detail.Trajectory == nil {
		t.Fatalf("tasks.get carries NO trajectory enrichment — the devstack Enricher is not wired; body=%s", body)
	}
	if len(detail.Trajectory.Steps) == 0 {
		t.Fatal("tasks.get trajectory has zero steps")
	}
	foundWire := false
	for _, step := range detail.Trajectory.Steps {
		if strings.Contains(step.ReasoningTrace, devstackTrajReasoning) {
			foundWire = true
		}
	}
	if !foundWire {
		t.Fatalf("tasks.get trajectory steps carry no scripted reasoning trace: %+v", detail.Trajectory.Steps)
	}

	// (3) Failure mode: an unknown task id fails closed — no fabricated
	// enrichment, the read 404s before the enricher runs.
	status, body = devstackPostTasksGet(t, srv.URL, `{"id":"task-never-spawned"}`, stack.Token)
	if status != http.StatusNotFound {
		t.Fatalf("tasks.get unknown id: status = %d, want 404; body=%s", status, body)
	}
}

// devstackPostTasksGet issues a POST /v1/tasks/get with the supplied
// dev token.
func devstackPostTasksGet(t *testing.T, srvURL, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/tasks/get", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
