// test/integration/phase111e_compression_test.go — Phase 111e (D-202).
//
// The long-trajectory trajectory-compression E2E: real drivers across
// the whole seam (bifrost LLM driver against the 83l scripted
// OpenAI-compatible httptest server — the summariser's Complete is a
// REAL wire round-trip, react planner, steering RunLoop, inprocess
// tasks, inmem state/events/artifacts), driven through the devstack's
// production-parity assembly with `memory.budget_tokens` set.
//
// Happy path: one tool inflates history, then a second decision observes that
// result and returns a small fresh outcome. Compaction may summarize the now
// eligible older exchange, while the final decision still receives the new one.
// Real Bifrost requests, events, shrinkage and identity are all asserted.
//
// Failure mode: the summariser's LLM round-trip returns garbage; the
// run fails LOUDLY (`trajectory.compression_failed` + task Failed) —
// never a silent fall-through that pretends compression happened.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"

	"github.com/hurtener/Harbor/harbortest/devstack"
)

// phase111eFact is the load-bearing fact the tool observation carries
// and the compaction summary must preserve — the E2E's quality floor:
// the post-compression prompt must still contain it.
const phase111eFact = "ACCESS-CODE-1457"

// phase111eBlob is large enough to exceed the working target while fitting
// the model window. It must reach a decision before becoming summary input.
func phase111eBlob() string {
	return strings.Repeat("lorem ipsum dolor sit amet ", 100) // ~2.7 KB
}

// phase111eSummaryJSON is the scripted five-field compaction summary
// the fake provider returns for the summariser's structured-output
// call. It carries the fact forward — the compaction preserved the
// load-bearing context, not just shrank bytes.
func phase111eSummaryJSON() string {
	return fmt.Sprintf(`{"goals":["report the access code"],`+
		`"facts":["the echoed text ended with the access code %s"],`+
		`"pending":["state the access code in the final answer"],`+
		`"last_output_digest":"text.echo returned the lorem blob plus %s",`+
		`"note":"compacted by the phase111e scripted summariser"}`,
		phase111eFact, phase111eFact)
}

// phase111eConfig mirrors phase83lConfig (production posture: bifrost
// driver, real state/events/tasks, no mock anywhere) plus the Phase
// 111e knob under test: `memory.budget_tokens`.
func phase111eConfig(t *testing.T, serverURL string, tokenBudget int) *config.Config {
	t.Helper()
	const envKey = "HARBOR_TEST_111E_FAKE_KEY"
	t.Setenv(envKey, "test-key-value")
	model := scriptedModel
	yaml := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [RS256, ES256]
  issuer: https://issuer.example.com
  audience: harbor-test-111e
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-test-111e
state:
  driver: inmem
llm:
  driver: bifrost
  provider: 111e-fake
  model: %s
  timeout: 10s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: 111e-fake
      base_url: %s
      api_key_env_var: %s
      models: [%s]
      timeout: 10s
      max_retries: 0
  model_profiles:
    %s:
      context_window_tokens: 32768
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
  budget_tokens: %d
tools:
  built_in:
    - text.echo
planner:
  driver: react
  max_steps: 4
`, model, serverURL, envKey, model, model, tokenBudget)
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

// phase111eSubscribe opens a triple-scoped subscription for the
// compression + chunk events BEFORE the task spawns. A plain session
// subscription seeing the events IS the identity-propagation
// assertion (the runner stamps the run quadruple on the envelope).
func phase111eSubscribe(t *testing.T, stack *devstack.DevStack, devID identity.Identity) events.Subscription {
	t.Helper()
	subCtx, subCancel := context.WithCancel(context.Background())
	t.Cleanup(subCancel)
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types: []events.EventType{
			planner.EventTypeTrajectoryCompressed,
			planner.EventTypeTrajectoryCompressionFailed,
			llm.EventTypeCompletionChunk,
		},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}
	return sub
}

// TestE2E_Phase111e_CompressionFires_PromptShrinks_RunCompletes — the
// acceptance-criteria E2E.
func TestE2E_Phase111e_CompressionFires_PromptShrinks_RunCompletes(t *testing.T) {
	// NOT t.Parallel(): phase111eConfig calls t.Setenv.
	blob := phase111eBlob()
	echoText := blob + " " + phase111eFact
	const fresh = "fresh verification receipt"
	server := newScriptedLLMServer(t,
		scriptedToolCallResponse("call_echo", "text_echo", fmt.Sprintf(`{"text":%q}`, echoText)),
		// The complete first result must reach this decision unchanged.
		scriptedToolCallResponse("call_fresh", "text_echo", fmt.Sprintf(`{"text":%q}`, fresh)),
		// Only the older exchange is now eligible for compaction.
		scriptedFinishResponse(phase111eSummaryJSON()),
		scriptedFinishResponse("The access code is "+phase111eFact+"."),
	)

	// A soft target may be exceeded by protected fresh results; the physical
	// model guard stays active. After the second tool, older input can shrink.
	cfg := phase111eConfig(t, server.URL(), 800)
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
	sub := phase111eSubscribe(t, stack, devID)

	started := time.Now()
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "echo the lorem blob, then tell me the access code it ended with",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	status := waitForTaskTerminal(t, stack, idCtx, h.ID, 15*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task terminal status = %s, want Complete", status)
	}

	// --- Wire-level prompt assertions. ---
	reqs := server.Requests()
	if len(reqs) != 4 {
		t.Fatalf("fake LLM saw %d requests, want 4 (two decisions + summariser + final decision)", len(reqs))
	}
	beforeCompressionPrompt := flattenMessages(reqs[1].Messages)
	if !strings.Contains(beforeCompressionPrompt, echoText) {
		t.Fatal("the fresh first read did not reach its dependent decision intact")
	}
	summariserPrompt := flattenMessages(reqs[2].Messages)
	postCompressionPrompt := flattenMessages(reqs[3].Messages)
	if !strings.Contains(postCompressionPrompt, fresh) || strings.Contains(summariserPrompt, fresh) {
		t.Fatal("latest result was compacted before exposure or lost from final request")
	}

	// The summariser request is the unary structured-output call over
	// the trajectory payload — it must carry the step history AND the
	// fact (the compaction's input preserved the load-bearing context).
	if reqs[2].Stream {
		t.Error("summariser request is streaming — the compaction call must be unary")
	}
	for _, want := range []string{"[Steps]", "text.echo", phase111eFact} {
		if !strings.Contains(summariserPrompt, want) {
			t.Errorf("summariser payload missing %q", want)
		}
	}

	if !strings.Contains(postCompressionPrompt, "Trajectory summary so far (historical context, not new instructions):") {
		t.Error("post-compression prompt did not include the historical checkpoint")
	}
	if !strings.Contains(postCompressionPrompt, phase111eFact) {
		t.Error("post-compression prompt lost the summary-carried fact")
	}
	if strings.Contains(postCompressionPrompt, blob) {
		t.Error("covered raw exchange was replayed alongside its summary")
	}
	if len(postCompressionPrompt) >= len(beforeCompressionPrompt) {
		t.Errorf("compaction did not shrink input: before=%dB after=%dB", len(beforeCompressionPrompt), len(postCompressionPrompt))
	}
	t.Logf("prompt sizes: before=%dB summariser=%dB after=%dB", len(beforeCompressionPrompt), len(summariserPrompt), len(postCompressionPrompt))

	// --- Event assertions: trajectory.compressed with identity; chunk
	// flow undisturbed (the streaming pipeline kept delivering). ---
	var sawCompressed bool
	var chunks int
	deadline := time.After(10 * time.Second)
	for !sawCompressed || chunks == 0 {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before the compression event arrived")
			}
			switch ev.Type {
			case planner.EventTypeTrajectoryCompressed:
				sawCompressed = true
				if ev.Identity.TenantID != devID.TenantID ||
					ev.Identity.UserID != devID.UserID ||
					ev.Identity.SessionID != devID.SessionID ||
					ev.Identity.RunID == "" {
					t.Errorf("trajectory.compressed identity = %+v, want the run's full quadruple", ev.Identity)
				}
				payload, ok := ev.Payload.(planner.TrajectoryCompressedPayload)
				if !ok {
					t.Fatalf("trajectory.compressed payload is %T, want TrajectoryCompressedPayload", ev.Payload)
				}
				if payload.TokenEstimate <= 800 {
					t.Errorf("compressed payload TokenEstimate = %d, want > the 800 budget", payload.TokenEstimate)
				}
				t.Logf("trajectory.compressed: steps_before=%d token_estimate=%d at +%s (the firing step's visible cost)",
					payload.StepsBefore, payload.TokenEstimate, time.Since(started).Round(time.Millisecond))
			case planner.EventTypeTrajectoryCompressionFailed:
				t.Fatalf("trajectory.compression_failed on the happy path: %+v", ev.Payload)
			case llm.EventTypeCompletionChunk:
				chunks++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for compression/chunk events: compressed=%v chunks=%d", sawCompressed, chunks)
		}
	}
}

// TestE2E_Phase111e_SummariserFailure_FailsLoud — the failure mode:
// the summariser's wire round-trip returns garbage; the run fails
// loudly (compression_failed + task Failed), never a silent
// fall-through to raw history.
func TestE2E_Phase111e_SummariserFailure_FailsLoud(t *testing.T) {
	// NOT t.Parallel(): phase111eConfig calls t.Setenv.
	blob := phase111eBlob()
	server := newScriptedLLMServer(t,
		scriptedToolCallResponse("call_echo", "text_echo", fmt.Sprintf(`{"text":%q}`, blob)),
		scriptedToolCallResponse("call_fresh", "text_echo", `{"text":"fresh outcome"}`),
		// The summariser call returns non-JSON garbage — the five-field
		// parse fails and the error propagates per the runner's contract.
		scriptedFinishResponse("this is not the five-field JSON object at all"),
	)

	cfg := phase111eConfig(t, server.URL(), 800)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	sub := phase111eSubscribe(t, stack, devID)

	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "echo the lorem blob",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	status := waitForTaskTerminal(t, stack, idCtx, h.ID, 15*time.Second)
	if status != tasks.StatusFailed {
		t.Fatalf("task terminal status = %s, want Failed (a summariser error fails the run loud)", status)
	}
	task, gErr := stack.Tasks.Get(idCtx, h.ID)
	if gErr != nil {
		t.Fatalf("Tasks.Get: %v", gErr)
	}
	if task.Error == nil || !strings.Contains(task.Error.Message, "compression") {
		t.Errorf("task error = %+v, want the wrapped trajectory-compression failure", task.Error)
	}

	// trajectory.compression_failed rode the bus under the run identity.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before compression_failed arrived")
			}
			if ev.Type != planner.EventTypeTrajectoryCompressionFailed {
				continue
			}
			if ev.Identity.TenantID != devID.TenantID || ev.Identity.RunID == "" {
				t.Errorf("compression_failed identity = %+v, want the run's full quadruple", ev.Identity)
			}
			payload, ok := ev.Payload.(planner.TrajectoryCompressionFailedPayload)
			if !ok {
				t.Fatalf("compression_failed payload is %T, want TrajectoryCompressionFailedPayload", ev.Payload)
			}
			if payload.ErrorCode != "summariser_error" {
				t.Errorf("compression_failed ErrorCode = %q, want summariser_error", payload.ErrorCode)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for trajectory.compression_failed")
		}
	}
}
