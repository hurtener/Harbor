// phase146_task_output_schema_test.go — the §13 primitive-with-consumer
// E2E for per-task structured output (D-276). A schema-constrained task
// runs through the REAL devstack per-task RunLoop driver (production twin,
// real inmem drivers, the full assemble-composed LLM chain backed by a
// scripted driver), and a parent AWAITS it through the REAL dispatch
// toolExecutor — proving the validated `answer_payload` rides the whole
// request→task→run→envelope→observation seam.
//
// Legs:
//   - happy: a schema-constrained task completes; its validated
//     answer_payload arrives in the awaiting parent's observation.
//   - failure: a schema-invalid-after-budget task fails LOUD with the
//     output_invalid terminal code; the parent observes the error, never
//     a schemaless success.
//   - streaming: a schema-constrained task suppresses token deltas at the
//     llm.completion.chunk seam (bus-subscriber-verified) while a plain
//     task on the same stack streams content deltas untouched.
//   - D-026: a heavy answer_payload riding an AwaitTask observation is
//     offloaded to an ArtifactRef-shaped stub via projectForLLM.
//   - D-025: N≥100 concurrent tasks carrying DISTINCT schemas interleaved
//     with plain tasks against one shared stack — no schema/payload bleed,
//     no cancellation cross-talk, goroutine baseline restored.
//
// Run under -race.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// phase146EchoDriver is a scripted LLM driver: it scans the request's
// messages for a `<<...>>` marker and returns the enclosed text verbatim
// as the terminal content (the ReAct native terminal answer). Stateless →
// D-025-clean for concurrent reuse. A message with no marker yields a
// fixed plain answer.
type phase146EchoDriver struct {
	calls atomic.Int64
}

func (d *phase146EchoDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.calls.Add(1)
	content := "plain answer with no marker"
	for _, m := range req.Messages {
		var text string
		if m.Content.Text != nil {
			text = *m.Content.Text
		}
		for _, p := range m.Content.Parts {
			if p.Type == llm.PartText {
				text += p.Text
			}
		}
		if i := strings.Index(text, "<<"); i >= 0 {
			if j := strings.Index(text[i+2:], ">>"); j >= 0 {
				content = text[i+2 : i+2+j]
			}
		}
	}
	if req.Stream && req.OnContent != nil {
		req.OnContent(content, false)
		req.OnContent("", true)
	}
	return llm.CompleteResponse{Content: content}, nil
}

func (d *phase146EchoDriver) Close(_ context.Context) error { return nil }

var phase146DriverSeq atomic.Int64

func registerPhase146Driver() string {
	drv := &phase146EchoDriver{}
	name := fmt.Sprintf("phase146-echo-%d", phase146DriverSeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return drv, nil
	})
	return name
}

func phase146Snapshot(cfg *config.Config, driver string) *llm.ConfigSnapshot {
	return &llm.ConfigSnapshot{
		Driver:               driver,
		Model:                "test/model",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"test/model": {
				ContextWindowTokens: 100000,
				TokenEstimator:      "chars_div_4",
				// Native → the terminal completion carries the schema
				// through; MaxRetries 1 bounds the corrective loop.
				OutputMode: llm.OutputModeNative,
				MaxRetries: 1,
			},
		},
	}
}

func phase146DevStack(t *testing.T, driver string) *devstack.DevStack {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase146Snapshot(cfg, driver),
	})
	t.Cleanup(stack.Close)
	if stack.Tasks == nil || stack.RunLoopDriver == nil {
		t.Fatal("devstack: Tasks or RunLoopDriver nil — wiring broken")
	}
	return stack
}

func phase146DevIdentity() identity.Identity {
	return identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
}

// waitTaskTerminal polls a task to a terminal FSM state (bounded — not
// sleep-as-sync).
func waitTaskTerminal(t *testing.T, reg tasks.TaskRegistry, ctx context.Context, id tasks.TaskID) *tasks.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := reg.Get(ctx, id)
		if err == nil && (task.Status == tasks.StatusComplete || task.Status == tasks.StatusFailed || task.Status == tasks.StatusCancelled) {
			return task
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("task %q did not reach a terminal state within deadline", id)
	return nil
}

func phase146AwaitExecutor(t *testing.T, stack *devstack.DevStack) steering.ToolExecutor {
	t.Helper()
	return dispatch.NewToolExecutor(tools.NewCatalog(), stack.Artifacts, stack.Tasks)
}

func phase146ParentRC(runID string) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{Identity: phase146DevIdentity(), RunID: runID},
	}
}

// TestE2E_Phase146_AwaitTask_SchemaConstrained_ValidatedPayload — the
// §13 consumer happy path.
func TestE2E_Phase146_AwaitTask_SchemaConstrained_ValidatedPayload(t *testing.T) {
	t.Parallel()
	driver := registerPhase146Driver()
	stack := phase146DevStack(t, driver)

	idCtx, err := identity.With(context.Background(), phase146DevIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	schema := json.RawMessage(`{"type":"object","required":["tag"],"properties":{"tag":{"type":"string"}},"additionalProperties":false}`)
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: phase146DevIdentity()},
		Kind:         tasks.KindForeground,
		Query:        `produce the tag object <<{"tag":"alpha"}>>`,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	task := waitTaskTerminal(t, stack.Tasks, idCtx, h.ID)
	if task.Status != tasks.StatusComplete {
		t.Fatalf("task status = %q, want complete; error=%+v", task.Status, task.Error)
	}

	// The parent awaits the child through the REAL dispatch executor.
	exec := phase146AwaitExecutor(t, stack)
	raw, _, err := exec.ExecuteDecision(context.Background(), phase146ParentRC("parent-run"), planner.AwaitTask{TaskID: h.ID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	obs, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("observation type = %T, want map", raw)
	}
	result, ok := obs["result"].(map[string]any)
	if !ok {
		t.Fatalf("observation.result type = %T, want map (%v)", obs["result"], obs)
	}
	payload, ok := result["answer_payload"].(map[string]any)
	if !ok {
		t.Fatalf("observation carried no answer_payload object: %v", result)
	}
	if payload["tag"] != "alpha" {
		t.Fatalf("answer_payload.tag = %v, want alpha", payload["tag"])
	}
}

// TestE2E_Phase146_AwaitTask_SchemaInvalid_FailsLoud — the failure mode:
// a schema-invalid answer after the retry budget fails the task with the
// output_invalid terminal code; the parent observes the error.
func TestE2E_Phase146_AwaitTask_SchemaInvalid_FailsLoud(t *testing.T) {
	t.Parallel()
	driver := registerPhase146Driver()
	stack := phase146DevStack(t, driver)

	idCtx, err := identity.With(context.Background(), phase146DevIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	schema := json.RawMessage(`{"type":"object","required":["tag"],"properties":{"tag":{"type":"string"}},"additionalProperties":false}`)
	// The scripted answer is missing the required "tag" — every attempt
	// fails validation, so the correction budget exhausts.
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: phase146DevIdentity()},
		Kind:         tasks.KindForeground,
		Query:        `produce something wrong <<{"wrong":"value"}>>`,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	task := waitTaskTerminal(t, stack.Tasks, idCtx, h.ID)
	if task.Status != tasks.StatusFailed {
		t.Fatalf("task status = %q, want failed (no schemaless success)", task.Status)
	}
	if task.Error == nil || task.Error.Code != planner.TaskErrorCodeOutputInvalid {
		t.Fatalf("task error = %+v, want code %q", task.Error, planner.TaskErrorCodeOutputInvalid)
	}

	exec := phase146AwaitExecutor(t, stack)
	raw, _, err := exec.ExecuteDecision(context.Background(), phase146ParentRC("parent-run-2"), planner.AwaitTask{TaskID: h.ID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	obs := raw.(map[string]any)
	errObj, ok := obs["error"].(map[string]any)
	if !ok {
		t.Fatalf("observation carried no error object: %v", obs)
	}
	if errObj["code"] != planner.TaskErrorCodeOutputInvalid {
		t.Fatalf("observation error code = %v, want output_invalid", errObj["code"])
	}
}

// TestE2E_Phase146_StreamingSuppression_SchemaTaskEmitsNoDeltas — the
// D-272 streaming posture mirrored to the per-task path: a
// schema-constrained task suppresses assistant-content/reasoning token
// DELTAS at the driver's OnChunk → llm.completion.chunk seam (only
// Done:true step-boundary chunks with an EMPTY delta reach the bus),
// while a plain task on the same stack streams content deltas as today.
// A real bus subscriber observes both, so inverting the suppression gate
// fails this test.
func TestE2E_Phase146_StreamingSuppression_SchemaTaskEmitsNoDeltas(t *testing.T) {
	t.Parallel()
	driver := registerPhase146Driver()
	stack := phase146DevStack(t, driver)

	idCtx, err := identity.With(context.Background(), phase146DevIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Subscribe BEFORE spawning so no chunk is missed.
	devID := phase146DevIdentity()
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types:   []events.EventType{llm.EventTypeCompletionChunk},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	type chunk struct {
		delta string
		done  bool
	}
	var mu sync.Mutex
	byTask := map[string][]chunk{}
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for ev := range sub.Events() {
			p, ok := ev.Payload.(llm.CompletionChunkPayload)
			if !ok {
				continue
			}
			mu.Lock()
			byTask[p.TaskID] = append(byTask[p.TaskID], chunk{delta: p.Delta, done: p.Done})
			mu.Unlock()
		}
	}()

	schema := json.RawMessage(`{"type":"object","required":["tag"],"properties":{"tag":{"type":"string"}},"additionalProperties":false}`)
	schemaH, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: devID},
		Kind:         tasks.KindForeground,
		Query:        `stream task <<{"tag":"quiet"}>>`,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("Spawn(schema): %v", err)
	}
	plainH, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    `plain stream task <<loud text answer>>`,
	})
	if err != nil {
		t.Fatalf("Spawn(plain): %v", err)
	}

	schemaTask := waitTaskTerminal(t, stack.Tasks, idCtx, schemaH.ID)
	if schemaTask.Status != tasks.StatusComplete {
		t.Fatalf("schema task status = %q, want complete; err=%+v", schemaTask.Status, schemaTask.Error)
	}
	plainTask := waitTaskTerminal(t, stack.Tasks, idCtx, plainH.ID)
	if plainTask.Status != tasks.StatusComplete {
		t.Fatalf("plain task status = %q, want complete; err=%+v", plainTask.Status, plainTask.Error)
	}

	// Bounded eventually-wait for the trailing done chunks of BOTH tasks
	// to arrive (publish is async relative to task completion). The done
	// chunk is emitted after every delta from the same run-loop goroutine,
	// so once observed, the task's full chunk stream has been recorded.
	seenDone := func(id tasks.TaskID) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range byTask[string(id)] {
			if c.done {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (!seenDone(schemaH.ID) || !seenDone(plainH.ID)) {
		time.Sleep(10 * time.Millisecond)
	}
	sub.Cancel()
	<-collectDone

	mu.Lock()
	defer mu.Unlock()
	schemaChunks := byTask[string(schemaH.ID)]
	plainChunks := byTask[string(plainH.ID)]

	// Schema task: EVERY chunk is a Done:true step boundary with an empty
	// delta — no token content reached the bus.
	if len(schemaChunks) == 0 {
		t.Fatal("schema task emitted no chunks at all — the step-boundary done signal must still fire")
	}
	for i, c := range schemaChunks {
		if !c.done {
			t.Fatalf("schema task leaked a token delta chunk %d (delta=%q done=%v) — suppression gate broken", i, c.delta, c.done)
		}
		if c.delta != "" {
			t.Fatalf("schema task's done chunk %d carries text (%q) — the done forward must use an empty delta", i, c.delta)
		}
	}

	// Plain task on the SAME stack: content deltas stream as today.
	var sawContentDelta bool
	for _, c := range plainChunks {
		if !c.done && c.delta != "" {
			sawContentDelta = true
			break
		}
	}
	if !sawContentDelta {
		t.Fatalf("plain task emitted no content deltas (chunks: %+v) — streaming should be unaffected off the schema path", plainChunks)
	}
}

// TestE2E_Phase146_AwaitTask_HeavyAnswerPayload_Offloaded — D-026: a
// large answer_payload riding an AwaitTask observation is offloaded to an
// ArtifactRef-shaped stub through projectForLLM's EXISTING heavy-output
// mechanism (no second content-size path). Uses a direct MarkComplete
// (the envelope shape, not the run loop, is what this leg pins).
func TestE2E_Phase146_AwaitTask_HeavyAnswerPayload_Offloaded(t *testing.T) {
	t.Parallel()
	driver := registerPhase146Driver()
	stack := phase146DevStack(t, driver)
	// This leg owns the task lifecycle directly so it can inject the exact
	// heavy AnswerEnvelope shape. Stop and join the stack's asynchronous
	// RunLoopDriver before Spawn; otherwise it may consume task.spawned and
	// complete the task before the manual MarkRunning/MarkComplete sequence.
	// Close is idempotent, so the stack cleanup may safely call it again.
	if err := stack.RunLoopDriver.Close(context.Background()); err != nil {
		t.Fatalf("close RunLoopDriver: %v", err)
	}

	idCtx, err := identity.With(context.Background(), phase146DevIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: phase146DevIdentity()},
		Kind:     tasks.KindForeground,
		Query:    "heavy",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := stack.Tasks.MarkRunning(idCtx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	big := strings.Repeat("x", 64*1024)
	env := planner.AnswerEnvelope{
		Answer:        big,
		FinishReason:  string(planner.FinishGoal),
		AnswerPayload: json.RawMessage(fmt.Sprintf(`{"blob":%q}`, big)),
	}
	rawEnv, _ := json.Marshal(env)
	if err := stack.Tasks.MarkComplete(idCtx, h.ID, tasks.TaskResult{Value: rawEnv}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Tiny heavy threshold so the offload fires deterministically.
	exec := dispatch.NewToolExecutor(tools.NewCatalog(), stack.Artifacts, stack.Tasks,
		dispatch.WithHeavyThreshold(1024))
	rawObs, llmObs, err := exec.ExecuteDecision(context.Background(), phase146ParentRC("parent-heavy"), planner.AwaitTask{TaskID: h.ID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	// Raw observation keeps the full value.
	rawEnc, _ := json.Marshal(rawObs)
	if len(rawEnc) < 64*1024 {
		t.Errorf("raw observation looks truncated (%d bytes); should carry the full value", len(rawEnc))
	}
	stub, ok := llmObs.(map[string]any)
	if !ok {
		t.Fatalf("llmObs type = %T, want an offload stub map", llmObs)
	}
	if stub["truncated"] != true {
		t.Fatalf("heavy answer_payload was not offloaded to an ArtifactRef stub: %v", stub)
	}
	// The stub must carry the artifact ref the parent can fetch the full
	// value through — offload without a ref would silently lose data.
	if ref, _ := stub["artifact_ref"].(string); ref == "" {
		t.Fatalf("offload stub carries no artifact_ref: %v", stub)
	}
}

// TestE2E_Phase146_ConcurrentMixedSchemas_NoBleed — D-025: N≥100
// concurrent tasks carrying DISTINCT schemas interleaved with plain
// tasks against one shared stack; each task's result carries ITS OWN
// payload/answer — no schema/payload bleed across task results. Every
// tenth task is cancelled right after its spawn: a cancellation must
// never cross-talk into a sibling task's outcome. After all tasks are
// terminal the goroutine count returns to baseline (no per-run leak).
//
// NOT t.Parallel(): the goroutine-baseline assertion needs the process
// to itself while it settles (sibling parallel tests would skew the
// count).
func TestE2E_Phase146_ConcurrentMixedSchemas_NoBleed(t *testing.T) {
	driver := registerPhase146Driver()
	stack := phase146DevStack(t, driver)

	idCtx, err := identity.With(context.Background(), phase146DevIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	const n = 120
	tagSchema := json.RawMessage(`{"type":"object","required":["tag"],"properties":{"tag":{"type":"string"}},"additionalProperties":false}`)
	numSchema := json.RawMessage(`{"type":"object","required":["count"],"properties":{"count":{"type":"number"}},"additionalProperties":false}`)

	type spawned struct {
		id        tasks.TaskID
		schema    bool
		wantJSON  string // the exact answer_payload for schema tasks
		kind      int    // 0 tag-schema, 1 num-schema, 2 plain
		cancelled bool   // cancellation cross-talk probe
	}
	items := make([]spawned, n)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := tasks.SpawnRequest{
				Identity: identity.Quadruple{Identity: phase146DevIdentity()},
				Kind:     tasks.KindForeground,
			}
			var it spawned
			switch i % 3 {
			case 0:
				want := fmt.Sprintf(`{"tag":"t-%d"}`, i)
				req.Query = "tag task <<" + want + ">>"
				req.OutputSchema = tagSchema
				it = spawned{schema: true, wantJSON: want, kind: 0}
			case 1:
				want := fmt.Sprintf(`{"count":%d}`, i)
				req.Query = "num task <<" + want + ">>"
				req.OutputSchema = numSchema
				it = spawned{schema: true, wantJSON: want, kind: 1}
			default:
				req.Query = fmt.Sprintf("plain task <<plain-%d>>", i)
				it = spawned{schema: false, kind: 2}
			}
			h, sErr := stack.Tasks.Spawn(idCtx, req)
			if sErr != nil {
				errs <- fmt.Errorf("spawn %d: %w", i, sErr)
				return
			}
			it.id = h.ID
			// Cancellation cross-talk probe: every tenth task is cancelled
			// immediately after its spawn. The cancel races the (fast)
			// scripted run — either terminal outcome is fine for the
			// cancelled task itself; what MUST hold is that no sibling
			// task's outcome is affected.
			if i%10 == 5 {
				it.cancelled = true
				if _, cErr := stack.Tasks.Cancel(idCtx, h.ID, "cross-talk probe"); cErr != nil {
					errs <- fmt.Errorf("cancel %d: %w", i, cErr)
					return
				}
			}
			items[i] = it
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	// Await + assert each task's own payload landed, with no cross-talk.
	for i := range items {
		it := items[i]
		task := waitTaskTerminal(t, stack.Tasks, idCtx, it.id)
		if it.cancelled {
			// The cancel raced the run: cancelled, failed{cancelled}, or
			// complete (the run won) are all legitimate terminals for THIS
			// task. If it completed, its payload must still be its own.
			if task.Status == tasks.StatusFailed && (task.Error == nil || task.Error.Code != planner.TaskErrorCodeCancelled) {
				t.Fatalf("cancelled task %d failed with a non-cancel code: %+v", i, task.Error)
			}
			if task.Status != tasks.StatusComplete {
				continue
			}
		}
		if task.Status != tasks.StatusComplete {
			t.Fatalf("task %d (%q) status = %q, want complete; err=%+v", i, it.id, task.Status, task.Error)
		}
		var env planner.AnswerEnvelope
		if err := json.Unmarshal(task.Result.Value, &env); err != nil {
			t.Fatalf("task %d: unmarshal envelope: %v (%s)", i, err, task.Result.Value)
		}
		if it.schema {
			if string(env.AnswerPayload) != it.wantJSON {
				t.Fatalf("task %d bleed: answer_payload = %s, want %s", i, env.AnswerPayload, it.wantJSON)
			}
		} else if len(env.AnswerPayload) != 0 {
			t.Fatalf("task %d (plain) unexpectedly carries an answer_payload: %s", i, env.AnswerPayload)
		}
	}

	// Goroutine baseline restored after all runs are terminal — the
	// per-run goroutines (run loop, chunk publishers) are joined, not
	// leaked. Bounded settle loop; tolerance 3 for runtime jitter.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
	}
	if now := runtime.NumGoroutine(); now > baseline+3 {
		t.Fatalf("goroutine leak after %d tasks: baseline=%d now=%d", n, baseline, now)
	}
}
