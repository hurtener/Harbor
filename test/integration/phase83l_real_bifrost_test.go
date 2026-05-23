// test/integration/phase83l_real_bifrost_test.go — Phase 83l (D-155).
//
// The audit lesson from D-151 verbatim: every existing dev-binary
// integration test uses the mock LLM driver, which masked two
// real-bifrost+real-stack bugs through Wave 13/14/15 audits (empty
// Model field + missing trajectory append). 83l plugs that hole.
//
// The tests below boot the FULL devstack — same code path
// `cmd/harbor/cmd_dev.go::bootDevStack` runs — with the bifrost LLM
// driver pointed at a scripted `httptest.NewServer` that mimics an
// OpenAI-compatible /v1/chat/completions endpoint. The fake server
// records every request the dev stack made, so the assertions reach
// wire-level shapes the mock LLM could never expose.
//
// The two tests below are tight by design — one happy path, one
// failure path. Coverage expansions (streaming SSE, multi-step
// CallParallel, structured-output downgrade chain) live in their
// own phase plans when those surfaces get touched. CLAUDE.md §17.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/planner/react"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"

	"github.com/hurtener/Harbor/harbortest/devstack"
)

// scriptedLLMServer is the test-only fake HTTP server that mimics an
// OpenAI-compatible /v1/chat/completions endpoint. It records every
// request the dev stack made (callers assert against `Requests()`) and
// replays a scripted JSON-response sequence keyed by request index.
//
// Phase 83l / D-155. The shape is deliberately minimal — no SSE, no
// streaming, no provider-correction edge cases. The unary chat-
// completions path is what `cmd/harbor`'s dev binary exercises through
// the planner's Complete() call; streaming has its own phase.
type scriptedLLMServer struct {
	t         *testing.T
	server    *httptest.Server
	responses []string // canned JSON responses, one per request
	mu        sync.Mutex
	received  []openAIRequestEnvelope // records ALL inbound requests
}

// openAIRequestEnvelope is the subset of the OpenAI chat-completions
// request shape 83l asserts against. Bifrost emits more fields; only
// what the tests inspect lives here.
type openAIRequestEnvelope struct {
	Model    string                  `json:"model"`
	Messages []openAIChatMessageJSON `json:"messages"`
}

type openAIChatMessageJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// newScriptedLLMServer constructs a fake server primed with the given
// canned responses. The first request returns responses[0], the
// second returns responses[1], etc. Exhausting the script yields HTTP
// 500 + a t.Errorf so a runaway test is noisy.
func newScriptedLLMServer(t *testing.T, responses ...string) *scriptedLLMServer {
	t.Helper()
	s := &scriptedLLMServer{t: t, responses: responses}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *scriptedLLMServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("scriptedLLMServer: read body: %v", err)
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	var env openAIRequestEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		s.t.Errorf("scriptedLLMServer: parse body as OpenAI envelope: %v\nbody=%s", err, body)
		http.Error(w, "parse body", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	idx := len(s.received)
	s.received = append(s.received, env)
	s.mu.Unlock()
	if idx >= len(s.responses) {
		s.t.Errorf("scriptedLLMServer: request %d exhausted the script (only %d responses scripted); request envelope=%+v",
			idx, len(s.responses), env)
		http.Error(w, "script exhausted", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.responses[idx]))
}

// URL returns the fake server's base URL — what the test wires into
// the custom_provider's base_url.
func (s *scriptedLLMServer) URL() string {
	return s.server.URL
}

// Requests returns a snapshot of the recorded request envelopes.
func (s *scriptedLLMServer) Requests() []openAIRequestEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]openAIRequestEnvelope, len(s.received))
	copy(out, s.received)
	return out
}

// scriptedResponse returns a canned OpenAI-compatible /v1/chat/completions
// response whose assistant `content` is the supplied envelope (the
// react planner reads `content` and parses it as `{"tool":...,
// "args":...}`).
func scriptedResponse(model, envelopeJSON string) string {
	return fmt.Sprintf(`{
		"id":"chatcmpl-83l-test",
		"object":"chat.completion",
		"created":1700000000,
		"model":%q,
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":%q},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}
	}`, model, envelopeJSON)
}

// phase83lConfig writes the 83l test yaml + loads/validates it. The
// yaml mirrors the dev binary's production posture (bifrost driver,
// real state/events/tasks, no mock anywhere) with one custom_provider
// pointing at the scripted server.
func phase83lConfig(t *testing.T, serverURL string) *config.Config {
	t.Helper()
	const envKey = "HARBOR_TEST_83L_FAKE_KEY"
	t.Setenv(envKey, "test-key-value")
	const model = "google/gemma-4-31b-it"
	yaml := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [RS256, ES256]
  issuer: https://issuer.example.com
  audience: harbor-test-83l
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: info
  service_name: harbor-test-83l
state:
  driver: inmem
llm:
  driver: bifrost
  provider: 83l-fake
  model: %s
  timeout: 10s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: 83l-fake
      base_url: %s
      api_key_env_var: %s
      models: [%s]
      timeout: 10s
      max_retries: 0
  model_profiles:
    %s:
      context_window_tokens: 8192
      token_estimator: chars_div_4
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
tools:
  built_in:
    - text.echo
planner:
  driver: react
  max_steps: 4
`, model, serverURL, envKey, model, model)
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, err := config.Load(context.Background(), p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// waitForTaskTerminal polls stack.Tasks.Get until the task reaches a
// terminal status (Complete or Failed) or the deadline elapses.
// CLAUDE.md §17.4 — bounded-wait-for-state, not sleep-as-coordination.
func waitForTaskTerminal(t *testing.T, stack *devstack.DevStack, idCtx context.Context, taskID tasks.TaskID, maxWait time.Duration) tasks.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		task, gErr := stack.Tasks.Get(idCtx, taskID)
		if gErr == nil && (task.Status == tasks.StatusComplete || task.Status == tasks.StatusFailed) {
			return task.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := stack.Tasks.Get(idCtx, taskID)
	t.Fatalf("task %s did not reach terminal status within %s; last seen status=%s", taskID, maxWait, task.Status)
	return tasks.TaskStatus("")
}

// TestE2E_RealBifrost_PlannerExecutorTrajectory_HappyPath — the
// canonical 83l shape. The scripted LLM returns CallTool(text.echo)
// then Finish; the dev stack drives the planner → executor →
// trajectory append cycle end to end. Catches every regression D-151
// and D-152 closed (empty Model, missing trajectory append, missing
// catalog projection, missing memory writeback).
func TestE2E_RealBifrost_PlannerExecutorTrajectory_HappyPath(t *testing.T) {
	// NOT t.Parallel(): phase83lConfig calls t.Setenv to populate the
	// fake-provider API-key env var, which the testing package forbids
	// alongside t.Parallel.
	const model = "google/gemma-4-31b-it"

	server := newScriptedLLMServer(t,
		scriptedResponse(model, `{"tool":"text.echo","args":{"text":"hello from 83l"}}`),
		scriptedResponse(model, `{"tool":"_finish","args":{"answer":"echo returned 'hello from 83l'"}}`),
	)

	cfg := phase83lConfig(t, server.URL())
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	if stack.Tasks == nil || stack.RunLoopDriver == nil {
		t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
	}
	if stack.Catalog == nil {
		t.Fatal("devstack: Catalog is nil — built-in registration path skipped")
	}
	if _, ok := stack.Catalog.Resolve("text.echo"); !ok {
		t.Fatal("text.echo not in catalog — devstack's builtin.Register call did not fire (regression of 83n D-153)")
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
		Query:    "echo 'hello from 83l' back to me",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	status := waitForTaskTerminal(t, stack, idCtx, h.ID, 10*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task terminal status = %s, want Complete (script:CallTool+Finish should always succeed)", status)
	}

	// Wire-level assertions — the value-prop 83l brings that the mock
	// LLM could never reach.
	reqs := server.Requests()
	if len(reqs) < 2 {
		t.Fatalf("fake LLM saw %d requests, want >= 2 (CallTool + Finish)", len(reqs))
	}
	for i, req := range reqs {
		if req.Model == "" {
			t.Errorf("LLM request %d has empty model — regression of 83h V2 (D-151)", i)
		}
		if req.Model != model {
			t.Errorf("LLM request %d model = %q, want %q", i, req.Model, model)
		}
		if len(req.Messages) == 0 {
			t.Errorf("LLM request %d has no messages — empty prompt would be a serious break", i)
		}
	}
	// Trajectory append assertion: the SECOND request's messages must
	// contain a reference to the FIRST request's tool call observation.
	// Without 83i's trajectory append, every step's prompt is identical
	// — the live operator validation showed 30 LLM calls with byte-for-
	// byte identical sizes. The assertion below catches that regression
	// loud.
	secondReqText := flattenMessages(reqs[1].Messages)
	if !strings.Contains(secondReqText, "text.echo") {
		t.Errorf("second LLM request's prompt does not reference text.echo — trajectory append regression (D-152)\nsecond prompt:\n%s",
			secondReqText)
	}
	if !strings.Contains(secondReqText, "hello from 83l") {
		t.Errorf("second LLM request's prompt does not contain the first tool call's args — trajectory observation projection regression\nsecond prompt:\n%s",
			secondReqText)
	}
}

// TestE2E_RealBifrost_ToolValidationFailure_PlannerReplans — the
// failure-mode shape. Scripts the LLM to call text.echo with bad args
// (missing required `text` field); the runtime's tool validator
// rejects; the planner re-plans with the validator error as the
// observation; the next LLM response finishes with an apology.
func TestE2E_RealBifrost_ToolValidationFailure_PlannerReplans(t *testing.T) {
	// NOT t.Parallel(): same t.Setenv reason as the happy-path test.
	const model = "google/gemma-4-31b-it"

	server := newScriptedLLMServer(t,
		// Bad-args call: text.echo's input requires a `text` string,
		// but we send `wrong_field`. The inproc validator rejects;
		// the planner sees the error observation.
		scriptedResponse(model, `{"tool":"text.echo","args":{"wrong_field":"oops"}}`),
		// Recovery: the planner re-plans with the validator error in
		// the trajectory + finishes with an apology.
		scriptedResponse(model, `{"tool":"_finish","args":{"answer":"I could not call the echo tool — bad arguments. Sorry."}}`),
	)

	cfg := phase83lConfig(t, server.URL())
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
		Query:    "try to echo something",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	status := waitForTaskTerminal(t, stack, idCtx, h.ID, 10*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task terminal status = %s, want Complete (the replan should reach Finish)", status)
	}

	reqs := server.Requests()
	if len(reqs) < 2 {
		t.Fatalf("fake LLM saw %d requests, want >= 2 (bad CallTool + replan + Finish)", len(reqs))
	}
	// The second prompt must carry the validator error as an
	// observation so the planner can see WHY it failed and adjust.
	secondReqText := flattenMessages(reqs[1].Messages)
	if !strings.Contains(secondReqText, "wrong_field") && !strings.Contains(secondReqText, "text") {
		t.Errorf("second LLM request's prompt lacks any trace of the failed call's args — observation projection broken\nsecond prompt:\n%s",
			secondReqText)
	}
}

// flattenMessages concatenates every message's content into one string
// for substring searches. The render uses raw `Content` because the
// 83a/b/c prompts emit XML-style sections (`<available_tools>` etc.)
// as plain text inside the system message.
func flattenMessages(msgs []openAIChatMessageJSON) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(":\n")
		b.WriteString(m.Content)
		b.WriteString("\n---\n")
	}
	return b.String()
}
