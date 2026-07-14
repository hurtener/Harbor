// Wave-end composing E2E (CLAUDE.md §17.5 / §17.7 step 5) for the
// v1.10 band — phases 145-151 (D-275..D-281): the governance
// attempt-cost tap, per-task structured output, the events-conformance
// fold (a pure test refactor; no runtime surface to compose here), MCP
// southbound OAuth + _meta provenance, the HTTP-manifest boot loader,
// the run-completion hook, and loading-mode exposure.
//
// Legs:
//  1. Composed happy path (146×150×149): a schema-constrained TASK whose
//     planner calls a manifest tool mid-run, burns one Validator retry,
//     and finishes valid — the validated answer_payload reaches the
//     awaiting parent through the real dispatch AwaitTask executor, and
//     the run-completion hook delivers the ordered transcript to the
//     manifest HTTP sink (auth_ref header asserted sink-side).
//  2. Governance × per-task schema retries (145×146×150) — THE seam no
//     phase test covers: with a budget ceiling between "final costs
//     only" and "final + tap-reported retry costs", a second task under
//     the same identity is PreCall-blocked IFF the attempt-cost tap is
//     composed on the per-task LLM chain. The blocked run's hook still
//     fires with outcome=error; a distinct user is unaffected.
//  3. Outcome-fidelity boundary (146×150, RFC §6.17 / D-280): a goal
//     Finish whose payload fails the RunOnce-edge schema validation —
//     the sink receives outcome "goal" while the caller gets
//     planner.ErrOutputInvalid. The sink tool is LoadingDeferred
//     (prompt-hidden), pinning D-280's full-catalog hook resolution.
//  4. Cancelled run × oauth-bound MCP hook target (148×150): the run is
//     cancelled mid-flight; the hook fires under the bounded
//     WithoutCancel bridge and its dispatch to a go-sdk MCP fixture on a
//     tokenexchange-bound connection still resolves the per-identity
//     bearer from the detached ctx — asserted server-side.
//  5. Concurrency stress (§17.3): N=12 concurrent schema tasks with
//     DISTINCT identity triples against ONE shared devstack, each with
//     the hook — no answer_payload bleed, no transcript bleed at the
//     sink, per-identity fidelity, goroutine baseline restored.
//
// Legs 1, 2 and 5 are deliberately NOT parallel: they t.Setenv the
// manifest's auth secret, and leg 2 additionally rides the
// process-global governance.SetFactory seam assemble installs. Legs 3
// and 4 are parallel-safe.
//
// This file is `package integration_test` and reuses its in-package
// helpers per the wave_v19 precedent: writeDevConfig (phase64), and
// wave_v19's broker env + subscribe/wait/text helpers.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

const (
	waveV110SinkTool  = "transcript.sink"
	waveV110WorkTool  = "wave.fetch"
	waveV110MCPServer = "sinkmcp"
	waveV110MCPHook   = "sinkmcp_ingest" // <server>_<tool> per the MCP driver's naming

	waveV110SinkKeyEnv   = "WAVE_V110_SINK_KEY"
	waveV110SinkKeyValue = "dummy-wave-v110-sink-key-not-a-secret" // §7 rule 2 fixture

	// Attempt-cost arithmetic (leg 2): the ceiling sits BETWEEN the
	// final-attempt cost and the folded total, so the second task is
	// blocked IFF the tap reported the validator-rejected attempt.
	waveV110BadCost  = 0.30 // validator-rejected attempt → reported via the tap
	waveV110GoodCost = 0.02 // final attempt → PostCall resp.Cost
	waveV110Ceiling  = 0.25 // GoodCost < Ceiling < GoodCost+BadCost
)

const waveV110MarkerSchema = `{
	"type": "object",
	"required": ["marker"],
	"properties": {"marker": {"type": "string"}},
	"additionalProperties": false
}`

// waveV110Driver is stateless per request (D-025-clean): a corrective
// re-ask (the retry wrapper's "failed validation" turn) yields the
// goal-embedded conforming payload; a goal carrying the CALLTOOL
// directive with no tool result yet yields a native tool call to the
// manifest work tool; anything else yields a schema-invalid answer.
type waveV110Driver struct {
	calls atomic.Int64
}

func (d *waveV110Driver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.calls.Add(1)
	text := waveV19AllText(req)
	var resp llm.CompleteResponse
	switch {
	case strings.Contains(text, "failed validation"):
		resp = llm.CompleteResponse{
			Content: waveV110ExtractPayload(text),
			Cost:    llm.Cost{TotalCost: waveV110GoodCost},
		}
	case strings.Contains(text, "WAVEV110_CALLTOOL") && !waveV19HasToolResult(req):
		resp = llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
			{ID: "call_fetch", Name: waveV110WorkTool, Args: json.RawMessage(`{}`)},
		}}
	default:
		resp = llm.CompleteResponse{
			Content: `{"wrong":true}`,
			Cost:    llm.Cost{TotalCost: waveV110BadCost},
		}
	}
	if req.Stream && req.OnContent != nil && resp.Content != "" {
		req.OnContent(resp.Content, false)
		req.OnContent("", true)
	}
	return resp, nil
}

func (d *waveV110Driver) Close(_ context.Context) error { return nil }

var waveV110DriverSeq atomic.Int64

func registerWaveV110Driver() (string, *waveV110Driver) {
	drv := &waveV110Driver{}
	name := fmt.Sprintf("wave-v110-scripted-%d", waveV110DriverSeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return drv, nil
	})
	return name, drv
}

// waveV110ExtractPayload returns the goal-embedded `<<...>>` payload, or a
// fixed conforming object when no marker is present.
func waveV110ExtractPayload(text string) string {
	if i := strings.Index(text, "<<"); i >= 0 {
		if j := strings.Index(text[i+2:], ">>"); j >= 0 {
			return text[i+2 : i+2+j]
		}
	}
	return `{"marker":"unmarked"}`
}

func waveV110Snapshot(cfg *config.Config, driver string) *llm.ConfigSnapshot {
	return &llm.ConfigSnapshot{
		Driver:               driver,
		Model:                "test/model",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"test/model": {
				ContextWindowTokens: 100000,
				TokenEstimator:      "chars_div_4",
				OutputMode:          llm.OutputModeNative,
				MaxRetries:          1, // exactly one corrective re-ask
			},
		},
	}
}

type waveV110SinkRecord struct {
	payload steering.RunCompletionPayload
	apiKey  string
}

type waveV110Sink struct {
	server *httptest.Server
	mu     sync.Mutex
	recs   []waveV110SinkRecord
	fetch  int
	once   sync.Once
}

func newWaveV110Sink(t *testing.T) *waveV110Sink {
	t.Helper()
	s := &waveV110Sink{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		var p steering.RunCompletionPayload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.recs = append(s.recs, waveV110SinkRecord{payload: p, apiKey: r.Header.Get("X-Sink-Key")})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/fetch", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.fetch++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"datum":42}`))
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.close)
	return s
}

// close is idempotent so the goroutine-baseline leg can tear the server
// down BEFORE the baseline check while the t.Cleanup stays registered.
func (s *waveV110Sink) close() { s.once.Do(s.server.Close) }

func (s *waveV110Sink) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetch
}

func (s *waveV110Sink) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// find returns the record whose transcript GOAL entry contains marker.
func (s *waveV110Sink) find(marker string) (waveV110SinkRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		for _, e := range r.payload.Conversation {
			if e.Kind == "goal" && strings.Contains(e.Content, marker) {
				return r, true
			}
		}
	}
	return waveV110SinkRecord{}, false
}

// waveV110WriteManifest writes the UTCP-style manifest declaring the sink
// (POST, no body template → the raw hook args ARE the body) and the
// planner work tool, both behind the manifest's static api_key auth.
// t.Setenv ⇒ the calling test must not be parallel.
func waveV110WriteManifest(t *testing.T, sinkURL string) string {
	t.Helper()
	t.Setenv(waveV110SinkKeyEnv, waveV110SinkKeyValue)
	dir := t.TempDir()
	path := filepath.Join(dir, "wave-v110-sink.yaml")
	content := fmt.Sprintf(`auth:
  sink_key:
    kind: api_key
    header: X-Sink-Key
    secret: ${%s}
tools:
  - name: %s
    method: POST
    url_template: %s/ingest
    description: run-completion transcript sink (wave v1.10 E2E).
    auth_ref: sink_key
    args_schema: '{"type":"object"}'
  - name: %s
    method: GET
    url_template: %s/fetch
    description: fetch one datum (planner work tool).
    auth_ref: sink_key
    args_schema: '{"type":"object"}'
`, waveV110SinkKeyEnv, waveV110SinkTool, sinkURL, waveV110WorkTool, sinkURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// waveV110DevStack builds the real devstack (thin assemble.Assemble
// wrapper): manifest boot-loaded, the yaml run-completion hook set, and
// (optionally) governance identity-tier enforcement.
func waveV110DevStack(t *testing.T, driverName, manifestPath string, governed bool) *devstack.DevStack {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	cfg.Tools.HTTPManifests = []string{manifestPath} // absolute (post-Load seam)
	cfg.Runtime.Hooks.RunCompletion = config.RunCompletionHookConfig{
		Tool:    waveV110SinkTool,
		Timeout: 5 * time.Second,
	}
	if governed {
		cfg.Governance.DefaultTier = "default"
		cfg.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
			"default": {BudgetCeilingUSD: waveV110Ceiling},
		}
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: waveV110Snapshot(cfg, driverName),
	})
	t.Cleanup(stack.Close)
	if stack.Tasks == nil || stack.RunLoopDriver == nil || stack.Catalog == nil {
		t.Fatal("devstack: Tasks / RunLoopDriver / Catalog nil — wiring broken")
	}
	if _, ok := stack.Catalog.Resolve(waveV110SinkTool); !ok {
		t.Fatalf("manifest tool %q not on the catalog — the boot loader did not run", waveV110SinkTool)
	}
	return stack
}

// waveV110WaitTerminal is a bounded terminal-state poll that RETURNS an
// error (goroutine-safe — never t.Fatalf off the test goroutine).
func waveV110WaitTerminal(ctx context.Context, reg tasks.TaskRegistry, id tasks.TaskID) (*tasks.Task, error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		task, err := reg.Get(ctx, id)
		if err == nil && (task.Status == tasks.StatusComplete ||
			task.Status == tasks.StatusFailed || task.Status == tasks.StatusCancelled) {
			return task, nil
		}
		time.Sleep(15 * time.Millisecond)
	}
	return nil, fmt.Errorf("task %q did not reach a terminal state within deadline", id)
}

func waveV110SpawnAndWait(stack *devstack.DevStack, id identity.Identity, goal string, schema json.RawMessage) (*tasks.Task, tasks.TaskID, error) {
	idCtx, err := identity.With(context.Background(), id)
	if err != nil {
		return nil, "", fmt.Errorf("identity.With: %w", err)
	}
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: id},
		Kind:         tasks.KindForeground,
		Query:        goal,
		OutputSchema: schema,
	})
	if err != nil {
		return nil, "", fmt.Errorf("Spawn: %w", err)
	}
	task, err := waveV110WaitTerminal(idCtx, stack.Tasks, h.ID)
	return task, h.ID, err
}

// waveV110AwaitPayload awaits the task through the REAL dispatch executor
// and returns answer_payload["marker"].
func waveV110AwaitPayload(t *testing.T, stack *devstack.DevStack, id identity.Identity, parentRun string, taskID tasks.TaskID) (string, error) {
	t.Helper()
	exec := dispatch.NewToolExecutor(tools.NewCatalog(), stack.Artifacts, stack.Tasks)
	raw, _, err := exec.ExecuteDecision(context.Background(),
		planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: parentRun}},
		planner.AwaitTask{TaskID: taskID})
	if err != nil {
		return "", err
	}
	obs, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("observation type %T", raw)
	}
	result, ok := obs["result"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("observation.result type %T (%v)", obs["result"], obs)
	}
	payload, ok := result["answer_payload"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("no answer_payload object in %v", result)
	}
	marker, _ := payload["marker"].(string)
	return marker, nil
}

func waveV110Goal(withTool bool, marker string) string {
	directive := ""
	if withTool {
		directive = "WAVEV110_CALLTOOL "
	}
	return fmt.Sprintf(`%sproduce the marker object <<{"marker":%q}>>`, directive, marker)
}

func waveV110Kinds(p steering.RunCompletionPayload) []string {
	out := make([]string, 0, len(p.Conversation))
	for _, e := range p.Conversation {
		out = append(out, e.Kind)
	}
	return out
}

func waveV110InOrder(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// ---------------------------------------------------------------------------
// Leg 1 — composed happy path (146 × 150 × 149). NOT parallel (t.Setenv).
// ---------------------------------------------------------------------------

func TestE2E_WaveV110_ComposedSchemaTask_HookThroughManifestSink(t *testing.T) {
	driverName, drv := registerWaveV110Driver()
	sink := newWaveV110Sink(t)
	manifest := waveV110WriteManifest(t, sink.server.URL)
	stack := waveV110DevStack(t, driverName, manifest, false)

	id := identity.Identity{TenantID: "wave-v110", UserID: "alice", SessionID: "s-composed"}
	hookCh := waveV19Subscribe(t, stack.Bus, id, steering.EventTypeRunHookDispatched)

	const marker = "WAVEV110_MARK_COMPOSED_END"
	task, taskID, err := waveV110SpawnAndWait(stack, id,
		waveV110Goal(true, marker), json.RawMessage(waveV110MarkerSchema))
	if err != nil {
		t.Fatalf("spawn/wait: %v", err)
	}
	if task.Status != tasks.StatusComplete {
		t.Fatalf("task status = %q, want complete; error=%+v", task.Status, task.Error)
	}

	got, err := waveV110AwaitPayload(t, stack, id, "parent-composed", taskID)
	if err != nil {
		t.Fatalf("AwaitTask: %v", err)
	}
	if got != marker {
		t.Errorf("answer_payload.marker = %q, want %q", got, marker)
	}

	if n := drv.calls.Load(); n != 3 {
		t.Errorf("driver calls = %d, want 3 (tool turn + schema-invalid + corrective)", n)
	}
	if n := sink.fetchCount(); n != 1 {
		t.Errorf("manifest work-tool fetches = %d, want 1", n)
	}

	rec, ok := sink.find(marker)
	if !ok {
		t.Fatalf("no transcript at the sink for marker %q (records=%d)", marker, sink.recordCount())
	}
	if rec.apiKey != waveV110SinkKeyValue {
		t.Errorf("sink X-Sink-Key = %q, want the manifest auth_ref value", rec.apiKey)
	}
	p := rec.payload
	if p.FormatVersion != 1 || p.Outcome != "goal" {
		t.Errorf("payload format/outcome = %d/%q, want 1/goal", p.FormatVersion, p.Outcome)
	}
	if p.TenantID != id.TenantID || p.UserID != id.UserID || p.SessionID != id.SessionID {
		t.Errorf("payload identity = %s/%s/%s, want %s/%s/%s",
			p.TenantID, p.UserID, p.SessionID, id.TenantID, id.UserID, id.SessionID)
	}
	if p.ToolInvocations != 1 {
		t.Errorf("payload ToolInvocations = %d, want 1 (the hook dispatch is never counted)", p.ToolInvocations)
	}
	kinds := waveV110Kinds(p)
	if !waveV110InOrder(kinds, []string{"goal", "tool", "final_answer"}) {
		t.Errorf("transcript kinds = %v, want [goal ... tool ... final_answer] in order", kinds)
	}

	ev := waveV19WaitEvent(t, hookCh, steering.EventTypeRunHookDispatched)
	hp, ok := ev.Payload.(steering.RunHookDispatchedPayload)
	if !ok {
		t.Fatalf("run.hook_dispatched payload type = %T", ev.Payload)
	}
	if hp.Tool != waveV110SinkTool || hp.Outcome != "goal" {
		t.Errorf("hook event tool/outcome = %q/%q, want %q/goal", hp.Tool, hp.Outcome, waveV110SinkTool)
	}
}

// ---------------------------------------------------------------------------
// Leg 2 — governance tap on per-task schema retries (145 × 146 × 150).
// NOT parallel (t.Setenv + the process-global governance.SetFactory seam).
// ---------------------------------------------------------------------------

func TestE2E_WaveV110_GovernanceTap_OnPerTaskSchemaRetries(t *testing.T) {
	driverName, drv := registerWaveV110Driver()
	sink := newWaveV110Sink(t)
	manifest := waveV110WriteManifest(t, sink.server.URL)
	stack := waveV110DevStack(t, driverName, manifest, true /* governed */)

	govID := identity.Identity{TenantID: "wave-v110-gov", UserID: "gwen", SessionID: "s-gov"}
	retryCh := waveV19Subscribe(t, stack.Bus, govID, llm.EventTypeRetryWithFeedback)

	// Task 1: one validator-rejected attempt (0.30, tap-reported) + the
	// final attempt (0.02, PostCall) fold to 0.32 — over the 0.25 ceiling.
	task1, _, err := waveV110SpawnAndWait(stack, govID,
		waveV110Goal(false, "WAVEV110_GOV1_END"), json.RawMessage(waveV110MarkerSchema))
	if err != nil {
		t.Fatalf("task1: %v", err)
	}
	if task1.Status != tasks.StatusComplete {
		t.Fatalf("task1 status = %q, want complete (ceiling must not gate the first call); error=%+v",
			task1.Status, task1.Error)
	}
	waveV19WaitEvent(t, retryCh, llm.EventTypeRetryWithFeedback)
	callsAfter1 := drv.calls.Load() // 2 (invalid + corrective)

	// Task 2, same identity: PreCall must block on the FOLDED total. If the
	// attempt-cost tap were NOT composed on the per-task chain, the
	// accumulator would hold only 0.02 and this task would complete —
	// this assertion IS the 145×146 seam gate.
	task2, _, err := waveV110SpawnAndWait(stack, govID,
		waveV110Goal(false, "WAVEV110_GOV2_END"), json.RawMessage(waveV110MarkerSchema))
	if err != nil {
		t.Fatalf("task2: %v", err)
	}
	if task2.Status != tasks.StatusFailed {
		t.Fatalf("task2 status = %q, want failed — the folded retry spend did not reach PreCall (attempt-cost tap not composed on the per-task LLM chain?)", task2.Status)
	}
	if task2.Error != nil && task2.Error.Code == planner.TaskErrorCodeOutputInvalid {
		t.Errorf("task2 failed with output_invalid — expected a budget-shaped failure, not a schema one")
	}
	if n := drv.calls.Load(); n != callsAfter1 {
		t.Errorf("provider called %d times for the budget-blocked task — PreCall must short-circuit BEFORE any attempt", n-callsAfter1)
	}
	if rec, ok := sink.find("WAVEV110_GOV2_END"); !ok {
		t.Error("no transcript at the sink for the budget-blocked run — the hook must fire on error terminals too")
	} else if rec.payload.Outcome != "error" {
		t.Errorf("blocked run's hook outcome = %q, want error", rec.payload.Outcome)
	}

	otherID := identity.Identity{TenantID: "wave-v110-gov", UserID: "hank", SessionID: "s-gov-other"}
	task3, _, err := waveV110SpawnAndWait(stack, otherID,
		waveV110Goal(false, "WAVEV110_GOV3_END"), json.RawMessage(waveV110MarkerSchema))
	if err != nil {
		t.Fatalf("task3: %v", err)
	}
	if task3.Status != tasks.StatusComplete {
		t.Errorf("task3 (distinct user) status = %q, want complete — ceiling bled across identities; error=%+v",
			task3.Status, task3.Error)
	}
}

// ---------------------------------------------------------------------------
// Leg 3 — outcome-fidelity boundary (146 × 150). Parallel-safe.
// ---------------------------------------------------------------------------

type waveV110InvalidFinishPlanner struct{}

func (waveV110InvalidFinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"wrong": true}}, nil
}

type waveV110Capture struct {
	mu      sync.Mutex
	payload *steering.RunCompletionPayload
	quad    identity.Quadruple
}

func (c *waveV110Capture) descriptor(name string, loading tools.LoadingMode) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Description: "wave-v110 hook capture", Loading: loading},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var p steering.RunCompletionPayload
			if err := json.Unmarshal(args, &p); err != nil {
				return tools.ToolResult{}, fmt.Errorf("capture decode: %w", err)
			}
			q, _ := identity.QuadrupleFrom(ctx)
			c.mu.Lock()
			c.payload, c.quad = &p, q
			c.mu.Unlock()
			return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
		},
	}
}

func (c *waveV110Capture) get() (*steering.RunCompletionPayload, identity.Quadruple) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.payload, c.quad
}

func TestE2E_WaveV110_OutcomeFidelity_SinkGetsGoal_CallerGetsOutputInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driverName, _ := registerWaveV110Driver() // resolved but never called

	cfg, err := config.Load(ctx, writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"

	cap := &waveV110Capture{}
	const sinkName = "wave.capture_deferred"
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
		LLMSnapshot:      waveV110Snapshot(cfg, driverName),
		PlannerOverride:  waveV110InvalidFinishPlanner{},
		PreRegisterTools: []tools.ToolDescriptor{cap.descriptor(sinkName, tools.LoadingDeferred)},
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "wave-v110", UserID: "fiona", SessionID: "s-fidelity"}
	env, err := stack.RunOnce(ctx, "produce the marker object", id,
		assemble.WithOutputSchema(json.RawMessage(waveV110MarkerSchema)),
		assemble.WithCompletionHook(&steering.CompletionHookSpec{Tool: sinkName, Timeout: 5 * time.Second}))

	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Fatalf("RunOnce err = %v, want errors.Is planner.ErrOutputInvalid", err)
	}
	if env.Answer != "" || env.AnswerPayload != nil {
		t.Errorf("expected a zero envelope on edge-validation failure, got %+v", env)
	}

	p, q := cap.get()
	if p == nil {
		t.Fatal("the deferred hook target never received the transcript — full-catalog hook resolution broken")
	}
	if p.Outcome != "goal" {
		t.Errorf("sink outcome = %q, want goal (the hook reflects the run-loop terminal, not post-run validation)", p.Outcome)
	}
	if q.Identity != id {
		t.Errorf("sink ctx identity = %+v, want %+v", q.Identity, id)
	}
}

// ---------------------------------------------------------------------------
// Leg 4 — cancelled run × oauth-bound MCP hook target (148 × 150). Parallel-safe.
// ---------------------------------------------------------------------------

type waveV110MCPCapture struct {
	authz string
	meta  map[string]string
}

type waveV110MCPRecorder struct {
	inner http.Handler
	mu    sync.Mutex
	calls []waveV110MCPCapture
}

func (r *waveV110MCPRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Meta map[string]string `json:"_meta"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &msg) == nil && msg.Method == "tools/call" {
				r.mu.Lock()
				r.calls = append(r.calls, waveV110MCPCapture{
					authz: req.Header.Get("Authorization"),
					meta:  msg.Params.Meta,
				})
				r.mu.Unlock()
			}
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	r.inner.ServeHTTP(w, req)
}

func (r *waveV110MCPRecorder) snapshot() []waveV110MCPCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]waveV110MCPCapture, len(r.calls))
	copy(out, r.calls)
	return out
}

func waveV110MCPSink(t *testing.T) (*httptest.Server, *waveV110MCPRecorder) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-wave-v110-sink", Version: "v0"}, nil)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "ingest",
			Description: "run-completion transcript ingest",
			InputSchema: map[string]any{"type": "object"},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			FormatVersion int `json:"format_version"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if in.FormatVersion != 1 {
				return nil, nil, fmt.Errorf("format_version = %d, want 1", in.FormatVersion)
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
		},
	)
	rec := &waveV110MCPRecorder{inner: mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)}
	hs := httptest.NewServer(rec)
	t.Cleanup(hs.Close)
	return hs, rec
}

type waveV110BlockingPlanner struct {
	once    sync.Once
	started chan struct{}
}

func (p *waveV110BlockingPlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, fmt.Errorf("wave-v110 blocking planner: %w", ctx.Err())
}

func TestE2E_WaveV110_CancelledRun_HookResolvesBearerOnOAuthBoundMCP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hs, rec := waveV110MCPSink(t)
	// The provider's downstream-sink allow-list must list the bound MCP
	// connection's host (the credential-plane invariant); build the sink
	// server first so its host is known at provider construction.
	env := newWaveV19Env(t, hs.URL)
	driverName, _ := registerWaveV110Driver()

	cfg, err := config.Load(ctx, writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:                   waveV19Provider,
		Driver:                 "tokenexchange",
		ClientIDEnv:            "WAVE_V110_BROKER_CLIENT",
		ClientSecretEnv:        "WAVE_V110_BROKER_SECRET",
		TokenURL:               env.broker.tokenURL(),
		Extra:                  map[string]string{"audience": waveV19Audience},
		AllowedDownstreamHosts: []string{hs.URL},
	}}
	cfg.Tools.OAuthTokenKEKEnv = "WAVE_V110_OAUTH_KEK"
	cfg.Tools.MCPServers = []config.MCPServerConfig{{
		Name:            waveV110MCPServer,
		TransportMode:   "streamable_http",
		URL:             hs.URL,
		OAuthProvider:   waveV19Provider,
		MetaAnnotations: map[string]string{"deployment": "wave-v110"},
	}}

	parker := &waveV110BlockingPlanner{started: make(chan struct{})}
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
		LLMSnapshot:     waveV110Snapshot(cfg, driverName),
		OAuthProviders:  map[string]toolauth.OAuthProvider{waveV19Provider: env.provider},
		PlannerOverride: parker,
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "wave-v110", UserID: "mona", SessionID: "s-mcp-hook"}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, rErr := stack.RunOnce(runCtx, "hold until cancelled", id,
			assemble.WithCompletionHook(&steering.CompletionHookSpec{
				Tool: waveV110MCPHook, Timeout: 10 * time.Second,
			}))
		errCh <- rErr
	}()

	select {
	case <-parker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never started")
	}
	cancel() // REAL mid-flight cancellation

	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(15 * time.Second):
		t.Fatal("RunOnce did not return after cancellation")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("RunOnce err = %v, want a context.Canceled-wrapped error", runErr)
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("MCP sink saw %d tools/call requests, want 1 (the hook dispatch)", len(calls))
	}
	c := calls[0]
	wantBearer := "Bearer brokered-" + id.TenantID + "-" + id.UserID
	if c.authz != wantBearer {
		t.Errorf("Authorization = %q, want %q — the WithoutCancel bridge dropped the bearer path", c.authz, wantBearer)
	}
	if c.meta["tenant"] != id.TenantID || c.meta["user"] != id.UserID || c.meta["session"] != id.SessionID {
		t.Errorf("_meta triple = %v, want %s/%s/%s", c.meta, id.TenantID, id.UserID, id.SessionID)
	}
	if c.meta["deployment"] != "wave-v110" {
		t.Errorf("_meta deployment = %q, want the operator annotation", c.meta["deployment"])
	}
	if _, has := c.meta["agent_id"]; has {
		t.Errorf("_meta carries agent_id %q on a bare embed run — absence is the contract", c.meta["agent_id"])
	}
	if n := env.broker.subjectCount(id.TenantID + "/" + id.UserID); n != 1 {
		t.Errorf("broker exchanges for %s/%s = %d, want 1", id.TenantID, id.UserID, n)
	}
}

// ---------------------------------------------------------------------------
// Leg 5 — concurrency stress (§17.3). NOT parallel (t.Setenv).
// ---------------------------------------------------------------------------

func TestE2E_WaveV110_ConcurrencyStress(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	driverName, _ := registerWaveV110Driver()
	sink := newWaveV110Sink(t)
	manifest := waveV110WriteManifest(t, sink.server.URL)
	stack := waveV110DevStack(t, driverName, manifest, false)

	const n = 12 // ≥10 (CLAUDE.md §17.3)
	markerFor := func(i int) string { return fmt.Sprintf("WAVEV110_STRESS_%d_END", i) }
	idFor := func(i int) identity.Identity {
		return identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", i),
			UserID:    fmt.Sprintf("u-%d", i),
			SessionID: fmt.Sprintf("s-%d", i),
		}
	}

	type result struct {
		idx    int
		marker string
		err    error
	}
	results := make([]result, n)
	exec := dispatch.NewToolExecutor(tools.NewCatalog(), stack.Artifacts, stack.Tasks)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := idFor(i)
			task, taskID, err := waveV110SpawnAndWait(stack, id,
				waveV110Goal(true, markerFor(i)), json.RawMessage(waveV110MarkerSchema))
			if err != nil {
				results[i] = result{idx: i, err: err}
				return
			}
			if task.Status != tasks.StatusComplete {
				results[i] = result{idx: i, err: fmt.Errorf("status %q (error=%+v)", task.Status, task.Error)}
				return
			}
			raw, _, err := exec.ExecuteDecision(context.Background(),
				planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: fmt.Sprintf("parent-%d", i)}},
				planner.AwaitTask{TaskID: taskID})
			if err != nil {
				results[i] = result{idx: i, err: fmt.Errorf("AwaitTask: %w", err)}
				return
			}
			marker := ""
			if obs, ok := raw.(map[string]any); ok {
				if res, ok := obs["result"].(map[string]any); ok {
					if p, ok := res["answer_payload"].(map[string]any); ok {
						marker, _ = p["marker"].(string)
					}
				}
			}
			results[i] = result{idx: i, marker: marker}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("run %d: %v", r.idx, r.err)
			continue
		}
		if r.marker != markerFor(r.idx) {
			t.Errorf("run %d: answer_payload marker = %q, want %q — payload bleed", r.idx, r.marker, markerFor(r.idx))
		}
	}

	if got := sink.recordCount(); got != n {
		t.Errorf("sink records = %d, want %d (one hook per run)", got, n)
	}
	for i := range n {
		rec, ok := sink.find(markerFor(i))
		if !ok {
			t.Errorf("run %d: no transcript at the sink", i)
			continue
		}
		id := idFor(i)
		p := rec.payload
		if p.TenantID != id.TenantID || p.UserID != id.UserID || p.SessionID != id.SessionID {
			t.Errorf("run %d: transcript identity %s/%s/%s, want %s/%s/%s — cross-identity bleed",
				i, p.TenantID, p.UserID, p.SessionID, id.TenantID, id.UserID, id.SessionID)
		}
		for j := range n {
			if j == i {
				continue
			}
			for _, e := range p.Conversation {
				if strings.Contains(e.Content, markerFor(j)) {
					t.Errorf("run %d: transcript contains run %d's marker — CROSS-RUN TRANSCRIPT BLEED", i, j)
				}
			}
		}
	}

	// Teardown order matters: close the sink FIRST (kills the HTTP
	// keep-alive conn goroutines), then the stack, then the baseline.
	sink.close()
	stack.Close()
	const slack = 6
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+slack {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta := goruntime.NumGoroutine() - baseline; delta > slack {
		t.Errorf("goroutine count rose by %d after teardown (baseline=%d, now=%d) — join-on-shutdown contract violated",
			delta, baseline, goruntime.NumGoroutine())
	}
}
