// test/integration/wavec_test.go — the Wave C wave-end composed E2E
// (§17.7 step 5; landed by the Wave C checkpoint audit, which found
// each 111-band feature exercised only in its own phase's test).
//
// ONE devstack-assembled stack (production-parity fan-out over
// assemble.Assemble; real drivers everywhere: bifrost LLM against the
// 83l scripted wire server, react planner, steering RunLoop, inmem
// state/events/artifacts, localdb skills, inprocess tasks) with EVERY
// Wave C knob on simultaneously:
//
//   - governance.identity_tiers (111a) — a one-shot rate bucket,
//   - planner.token_budget (111e) — trajectory compression,
//   - pauseresume.max_park_duration + sweep_interval (111c) — durable
//     pauses + the max-park sweeper,
//   - skills.directory (111d) — the Directory-fed `<skills_context>`,
//   - the always-on telemetry band (111f) — Logger / tracer bridge,
//
// and asserts the composition: the run compresses AND its prompt
// carries the Directory block AND every one of its LLM calls drains
// the SAME governance bucket (the compaction-call-is-governed pin)
// AND the sweeper reaps a durable pause to timeout-terminal while the
// rest runs AND the bus→tracer bridge derives identity-attributed
// spans from the same event stream. §17.3: identity propagation on
// every surface, ≥1 failure mode (the rate-limited call + a
// cross-scope resume rejection), N≥10 concurrency stress, -race, and
// the goroutine baseline restored after Close.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"encoding/json"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"

	"github.com/hurtener/Harbor/harbortest/devstack"
)

// waveCFact is the load-bearing fact the compaction must carry into
// the post-compression prompt (the 111e quality floor, re-proven
// under the composed configuration).
const waveCFact = "WAVEC-CODE-7741"

// waveCSkillName is the operator-pinned skill the Directory must
// surface in the run's `<skills_context>` block.
const waveCSkillName = "wavec-pinned-skill"

// waveCConfig builds the all-knobs-on Wave C config. Mirrors
// phase111eConfig's production posture and adds the 111a / 111c /
// 111d blocks.
func waveCConfig(t *testing.T, serverURL string) *config.Config {
	t.Helper()
	const envKey = "HARBOR_TEST_WAVEC_FAKE_KEY"
	t.Setenv(envKey, "test-key-value")
	model := scriptedModel
	yaml := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [RS256, ES256]
  issuer: https://issuer.example.com
  audience: harbor-test-wavec
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-test-wavec
state:
  driver: inmem
llm:
  driver: bifrost
  provider: wavec-fake
  model: %s
  timeout: 10s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: wavec-fake
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
  default_tier: wavec
  identity_tiers:
    wavec:
      rate_limit:
        capacity: 4
events:
  driver: inmem
  max_subscribers_per_session: 32
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
skills:
  driver: localdb
  dsn: ":memory:"
  directory:
    pinned: [%s]
pauseresume:
  max_park_duration: 300ms
  sweep_interval: 100ms
tools:
  built_in:
    - text.echo
planner:
  driver: react
  max_steps: 4
  token_budget: 800
`, model, serverURL, envKey, model, model, waveCSkillName)
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

// waveCSummaryJSON is the scripted five-field compaction summary —
// carries the fact forward, exactly like the 111e fixture.
func waveCSummaryJSON() string {
	return fmt.Sprintf(`{"goals":["report the wave code"],`+
		`"facts":["the echoed text ended with the code %s"],`+
		`"pending":["state the code in the final answer"],`+
		`"last_output_digest":"text.echo returned the blob plus %s",`+
		`"note":"compacted by the wave-C scripted summariser"}`,
		waveCFact, waveCFact)
}

// TestE2E_WaveC_ComposedStack_AllFeaturesOn is the wave-end gate.
func TestE2E_WaveC_ComposedStack_AllFeaturesOn(t *testing.T) {
	// NOT t.Parallel(): waveCConfig calls t.Setenv.
	baseline := goruntime.NumGoroutine()

	blob := strings.Repeat("lorem ipsum dolor sit amet ", 100) // ~2.7 KB inflator
	echoText := blob + " " + waveCFact
	server := newScriptedLLMServer(t,
		// 1 — planner step 1: tool call inflating the trajectory.
		scriptedToolCallResponse("call_echo", "text.echo", fmt.Sprintf(`{"text":%q}`, echoText)),
		// 2 — the summariser's compaction call (governed like any other).
		scriptedFinishResponse(waveCSummaryJSON()),
		// 3 — planner step 2: the compacted-prompt finish.
		scriptedFinishResponse("The wave code is "+waveCFact+"."),
		// 4..7 — fillers for the post-run direct Completes that drain
		// the session bucket to zero (at most `capacity` succeed; the
		// over-limit call is rejected at PreCall and never reaches the
		// wire).
		scriptedFinishResponse("direct filler 1"),
		scriptedFinishResponse("direct filler 2"),
		scriptedFinishResponse("direct filler 3"),
		scriptedFinishResponse("direct filler 4"),
	)

	cfg := waveCConfig(t, server.URL())
	rec := tracetest.NewInMemoryExporter()
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		TracerOptions: []telemetry.TracerOption{telemetry.WithSpanExporter(rec)},
	})
	defer stack.Close()

	if stack.Tasks == nil || stack.RunLoopDriver == nil || stack.Coordinator == nil ||
		stack.Skills == nil || stack.Telemetry == nil || stack.Tracer == nil {
		t.Fatal("devstack wiring broken: a Wave C band is nil on the composed stack")
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

	// The Directory-fed skill (111d): pinned via cfg, ingested under
	// the run's identity through the real localdb store.
	if err := stack.Skills.Upsert(idCtx, identity.Quadruple{Identity: devID}, skills.Skill{
		Name:    waveCSkillName,
		Title:   "Echo-and-report playbook",
		Trigger: "when asked to echo text and report a code",
		Steps:   []string{"echo the text with text.echo", "report the trailing code"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}); err != nil {
		t.Fatalf("skills.Upsert: %v", err)
	}

	// One identity-scoped subscription across every composed surface's
	// events — seeing them on a plain session subscription IS the
	// identity-propagation assertion.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types: []events.EventType{
			planner.EventTypeTrajectoryCompressed,
			planner.EventTypeTrajectoryCompressionFailed,
			governance.EventTypeRateLimited,
			pauseresume.EventTypePauseResumed,
			events.EventTypeRuntimeError,
		},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// ── Leg 1: the run — compression + skills context + governed LLM
	// calls, all live at once. ──────────────────────────────────────
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "echo the blob, then tell me the wave code it ended with",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	if status := waitForTaskTerminal(t, stack, idCtx, h.ID, 15*time.Second); status != tasks.StatusComplete {
		t.Fatalf("task terminal status = %s, want Complete", status)
	}

	reqs := server.Requests()
	if len(reqs) != 3 {
		t.Fatalf("fake LLM saw %d requests during the run, want 3 (planner + summariser + planner)", len(reqs))
	}
	firstPrompt := flattenMessages(reqs[0].Messages)
	if !strings.Contains(firstPrompt, "<skills_context>") || !strings.Contains(firstPrompt, waveCSkillName) {
		t.Errorf("step-1 prompt missing the Directory-fed skills context (want <skills_context> + %q)", waveCSkillName)
	}
	postPrompt := flattenMessages(reqs[2].Messages)
	if !strings.Contains(postPrompt, "Trajectory summary so far:") || !strings.Contains(postPrompt, waveCFact) {
		t.Error("post-compression prompt did not render the summary path with the carried fact")
	}

	var runID string
	awaitWaveCEvent(t, sub, planner.EventTypeTrajectoryCompressed, devID, func(ev events.Event) {
		if ev.Identity.RunID == "" {
			t.Errorf("trajectory.compressed missing RunID — want the full quadruple")
		}
		runID = ev.Identity.RunID
	})

	// ── Leg 2a: the compaction-call-is-governed pin. Governance
	// buckets are quadruple-keyed and persisted through the runtime's
	// StateStore (`governance.bucket`); summing (capacity − level)
	// across the run's bucket record(s) counts how many of the run's
	// wire calls drained a governed bucket. The run made exactly 3
	// wire Completes (planner ×2 + summariser ×1) — all 3 MUST have
	// been governed. A bypassing compaction call would leave the total
	// at 2. Read BEFORE the direct calls below mutate the session
	// bucket. ────────────────────────────────────────────────────────
	const tierCapacity = 4
	runDrains := waveCBucketDrains(t, stack, identity.Quadruple{Identity: devID, RunID: runID}, tierCapacity)
	sessionDrains := waveCBucketDrains(t, stack, identity.Quadruple{Identity: devID}, tierCapacity)
	if runDrains+sessionDrains != 3 {
		t.Errorf("governed drains after the run = %d (run-bucket %d + session-bucket %d), want 3 — a run LLM call bypassed the governance chain (compaction call ungoverned?)",
			runDrains+sessionDrains, runDrains, sessionDrains)
	}

	// ── Leg 2b: enforcement live on the SAME composed stack (and the
	// first failure mode): drain the session bucket's remaining slots
	// with direct Completes, then the over-limit call fails closed
	// with the typed sentinel, emits governance.rate_limited under the
	// caller's identity, and never reaches the wire. ────────────────
	direct := func(text string) error {
		_, cerr := stack.LLMClient.Complete(idCtx, llm.CompleteRequest{
			Model: scriptedModel,
			Messages: []llm.ChatMessage{
				{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
			},
		})
		return cerr
	}
	remaining := tierCapacity - sessionDrains
	for i := range remaining {
		if err := direct(fmt.Sprintf("governed direct call %d", i+1)); err != nil {
			t.Fatalf("direct Complete %d/%d under the tier: %v", i+1, remaining, err)
		}
	}
	wireBefore := len(server.Requests())
	if err := direct("over the session bucket"); !errors.Is(err, governance.ErrRateLimited) {
		t.Fatalf("over-limit Complete: got %v, want ErrRateLimited", err)
	}
	if got := len(server.Requests()); got != wireBefore {
		t.Errorf("wire saw %d requests after the rejected call, want %d (rejection must fail at PreCall, never the provider)", got, wireBefore)
	}
	awaitWaveCEvent(t, sub, governance.EventTypeRateLimited, devID, nil)

	// ── Leg 3: telemetry on the composed stack — Logger.Error emits
	// the paired runtime.error bus event under the caller's identity.
	stack.Telemetry.Error(idCtx, "wavec synthetic failure for the pairing assertion")
	awaitWaveCEvent(t, sub, events.EventTypeRuntimeError, devID, nil)

	// ── Leg 4: a durable pause on the composed stack, reaped by the
	// assembly-started max-park sweeper to the typed timeout Decision
	// while everything above shares the process. ────────────────────
	pause, err := stack.Coordinator.Request(idCtx, pauseresume.PauseRequest{
		Identity: devID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"gate": "wavec"},
	})
	if err != nil {
		t.Fatalf("Coordinator.Request: %v", err)
	}
	if st, serr := stack.Coordinator.Status(idCtx, pause.Token); serr != nil || st.State != pauseresume.StatusPaused {
		t.Fatalf("pre-reap Status = %+v, %v — want paused", st, serr)
	}
	awaitWaveCEvent(t, sub, pauseresume.EventTypePauseResumed, devID, func(ev events.Event) {
		payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
		if !ok {
			t.Fatalf("pause.resumed payload type = %T", ev.Payload)
		}
		if payload.Token != string(pause.Token) || payload.Decision != pauseresume.DecisionTimeout {
			t.Errorf("pause.resumed = %+v, want token %q with the typed timeout Decision", payload, pause.Token)
		}
	})
	if st, serr := stack.Coordinator.Status(idCtx, pause.Token); serr != nil ||
		st.State != pauseresume.StatusResumed || st.Decision != pauseresume.DecisionTimeout {
		t.Fatalf("post-reap Status = %+v, %v — want resumed/timeout", st, serr)
	}

	// ── Leg 5: §17.3 concurrency stress — N=12 sessions drive the
	// pause lifecycle concurrently against the ONE Coordinator +
	// sweeper while the stack stays live: 6 resolve by manual Resume,
	// 6 by sweeper reap; a cross-scope resume rejects (the second
	// failure mode); no cross-session bleed. ────────────────────────
	const stressN = 12
	type stressOut struct {
		session string
		token   pauseresume.Token
		manual  bool
	}
	outs := make([]stressOut, stressN)
	var wg sync.WaitGroup
	errCh := make(chan error, stressN*2)
	for i := range stressN {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "wavec-stress", UserID: "u", SessionID: fmt.Sprintf("s-%02d", i)}
			ctx, ierr := identity.With(context.Background(), id)
			if ierr != nil {
				errCh <- ierr
				return
			}
			p, perr := stack.Coordinator.Request(ctx, pauseresume.PauseRequest{
				Identity: id,
				Reason:   pauseresume.ReasonAwaitInput,
				Payload:  map[string]any{"i": i},
			})
			if perr != nil {
				errCh <- fmt.Errorf("stress %d Request: %w", i, perr)
				return
			}
			manual := i%2 == 0
			outs[i] = stressOut{session: id.SessionID, token: p.Token, manual: manual}
			if manual {
				// Failure mode: a FOREIGN session may not resolve this
				// pause (fail closed before any mutation).
				foreign := identity.Identity{TenantID: "wavec-stress", UserID: "u", SessionID: "s-foreign"}
				fctx, ferr := identity.With(context.Background(), foreign)
				if ferr != nil {
					errCh <- ferr
					return
				}
				if rerr := stack.Coordinator.Resume(fctx, p.Token, pauseresume.DecisionResume, nil); !errors.Is(rerr, pauseresume.ErrScopeMismatch) {
					errCh <- fmt.Errorf("stress %d cross-scope Resume: got %w, want ErrScopeMismatch", i, rerr)
					return
				}
				if rerr := stack.Coordinator.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); rerr != nil &&
					!errors.Is(rerr, pauseresume.ErrAlreadyResumed) {
					// Losing the race to the sweeper is the documented
					// loser contract; anything else is a failure.
					errCh <- fmt.Errorf("stress %d own Resume: %w", i, rerr)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	// Every pause resolves exactly once: manual ones with resume (or
	// timeout if the sweeper won), swept ones with timeout — bounded
	// eventually-poll per token, no synchronisation sleeps.
	for _, o := range outs {
		ctx, _ := identity.With(context.Background(), identity.Identity{
			TenantID: "wavec-stress", UserID: "u", SessionID: o.session,
		})
		deadline := time.Now().Add(10 * time.Second)
		for {
			st, serr := stack.Coordinator.Status(ctx, o.token)
			if serr != nil {
				t.Fatalf("stress Status(%s): %v", o.session, serr)
			}
			if st.State == pauseresume.StatusResumed {
				if !o.manual && st.Decision != pauseresume.DecisionTimeout {
					t.Errorf("swept pause %s resolved with %q, want timeout", o.session, st.Decision)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("stress pause %s never resolved (manual=%v)", o.session, o.manual)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// ── Leg 6: the bus→tracer bridge derived identity-attributed
	// spans from the SAME event stream the run produced. ────────────
	spanDeadline := time.Now().Add(10 * time.Second)
	for {
		var found *tracetest.SpanStub
		for _, s := range rec.GetSpans() {
			if s.Name == "event task.started" {
				found = &s
				break
			}
		}
		if found != nil {
			attrs := map[string]string{}
			for _, kv := range found.Attributes {
				attrs[string(kv.Key)] = kv.Value.AsString()
			}
			if attrs["tenant_id"] != devID.TenantID || attrs["session_id"] != devID.SessionID {
				t.Errorf("task span identity attrs = %v, want the run's triple", attrs)
			}
			break
		}
		if time.Now().After(spanDeadline) {
			t.Fatalf("no task.started span derived from the composed run; recorder has %d spans", len(rec.GetSpans()))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ── Teardown: close the ONE stack; goroutine baseline restored
	// (the sweeper, bridges, run-loop driver, and notifications
	// subscriber all join their closers). ───────────────────────────
	subCancel()
	sub.Cancel()
	stack.Close()
	waveCSettle(t, baseline)
}

// waveCBucketDrains reads the persisted `governance.bucket` record for
// q from the stack's StateStore and returns Σ (capacity − level) over
// its per-model buckets — the count of governed drains charged to that
// quadruple. A missing record (no governed call under q) counts 0.
// The record's JSON shape is the rate limiter's persisted production
// schema, read through the public StateStore surface.
func waveCBucketDrains(t *testing.T, stack *devstack.DevStack, q identity.Quadruple, capacity int) int {
	t.Helper()
	rec, err := stack.State.Load(context.Background(), q, "governance.bucket")
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0
		}
		t.Fatalf("State.Load(governance.bucket, run=%q): %v", q.RunID, err)
	}
	var br struct {
		ByModel map[string]struct {
			Level int `json:"level"`
		} `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Bytes, &br); err != nil {
		t.Fatalf("unmarshal bucket record: %v", err)
	}
	drains := 0
	for _, b := range br.ByModel {
		drains += capacity - b.Level
	}
	return drains
}

// awaitWaveCEvent blocks (bounded) until an event of wantType arrives
// on sub, asserts the identity triple, and runs the optional extra
// check. Other event types are skipped (one shared subscription
// serves every leg).
func awaitWaveCEvent(t *testing.T, sub events.Subscription, wantType events.EventType, id identity.Identity, extra func(events.Event)) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before %s arrived", wantType)
			}
			if ev.Type != wantType {
				continue
			}
			if ev.Identity.TenantID != id.TenantID || ev.Identity.UserID != id.UserID || ev.Identity.SessionID != id.SessionID {
				t.Errorf("%s identity = %+v, want %+v", wantType, ev.Identity, id)
			}
			if extra != nil {
				extra(ev)
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for %s", wantType)
		}
	}
}

// waveCSettle polls (bounded) until the goroutine count returns to
// the pre-assembly baseline — never a synchronisation sleep. The
// tolerance covers TEST-INFRA goroutines outside the stack's closer
// chain: the scripted httptest server (its goServe + per-keep-alive
// conn.serve goroutines close in the t.Cleanup that runs AFTER this
// check) and bifrost's fasthttp idle-connection watchers (they exit
// on idle expiry, not on Shutdown). The assertion still has teeth:
// the bug this gate caught on first run — the bifrost driver's Close
// never calling `(*Bifrost).Shutdown()` — leaked ~1000 worker-pool
// goroutines, three orders of magnitude past the tolerance.
func waveCSettle(t *testing.T, baseline int) {
	t.Helper()
	const testInfraTolerance = 8
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+testInfraTolerance {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	n := goruntime.Stack(buf, true)
	t.Logf("goroutine dump:\n%s", buf[:n])
	t.Errorf("goroutine baseline not restored after Close: started %d, now %d", baseline, goruntime.NumGoroutine())
}
