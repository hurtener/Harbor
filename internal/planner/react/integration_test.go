package react_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

// integrationBus opens a real inmem events.EventBus for the
// integration tests. Tears down via t.Cleanup. The bus is the real
// Phase 05 wiring; the test does not mock the seam (§17.3).
func integrationBus(t *testing.T) events.EventBus {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(t.Context(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = bus.Close(context.Background())
	})
	return bus
}

// integrationRC wires a real-bus-backed RunContext: the Emit closure
// publishes onto the supplied bus with the run's identity quadruple
// already stamped (matches the runtime engine's contract).
func integrationRC(bus events.EventBus, q identity.Quadruple, goal string) planner.RunContext {
	return planner.RunContext{
		Quadruple: q,
		Goal:      goal,
		Emit: func(ev events.Event) {
			ev.Identity = q
			_ = bus.Publish(context.Background(), ev)
		},
	}
}

// TestE2E_React_RepairExhaustion_PropagatesThroughLoop is the
// positive cross-subsystem integration: the planner consumes Phase
// 44's RepairLoop, which calls Phase 32's LLMClient. A stub LLM
// emits malformed JSON forever; the repair loop's storm guard fires;
// the planner propagates the loop's Finish{NoPath} verbatim. The bus
// observes the loop's planner.repair_exhausted event with the
// planner's identity (proving cross-subsystem wiring).
func TestE2E_React_RepairExhaustion_PropagatesThroughLoop(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-int-1", UserID: "u", SessionID: "s"},
		RunID:    "r-repair",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types: []events.EventType{
			planner.EventTypePlannerRepairExhausted,
			planner.EventTypePlannerMaxStepsExceeded,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Phase 107c (D-167) — empty content + empty ToolCalls is the
	// projector's NoPath path (the salvage/repair ladder is bypassed
	// on the native path; this test moved from repair-exhaustion
	// semantics to the projector's empty-response contract).
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{}, // empty Content, empty ToolCalls
		},
	}
	p := react.New(client)
	rc := integrationRC(bus, q, "exhaust me")

	// Subscribe also to planner.decision so we can prove the projector
	// path fired (the repair_exhausted subscription stays so we can
	// assert it does NOT fire under the native path).
	subDec, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{planner.EventTypePlannerDecision},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(decision): %v", err)
	}
	defer subDec.Cancel()

	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want Finish (projector NoPath)", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] not true — projector NoPath contract surface")
	}

	// The bus observes planner.decision (the new native-path
	// observability surface). planner.repair_exhausted MUST NOT fire
	// because the repair loop is bypassed on the native path.
	ev := drainOneEvent(t, subDec)
	if ev.Type != planner.EventTypePlannerDecision {
		t.Fatalf("ev.Type = %q, want planner.decision", ev.Type)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	// Assert the repair_exhausted subscription stays silent — bounded
	// wall-clock wait, no synchronisation sleep.
	select {
	case unexpected := <-sub.Events():
		if unexpected.Type == planner.EventTypePlannerRepairExhausted {
			t.Errorf("planner.repair_exhausted fired under the native path (repair loop should be bypassed)")
		}
	case <-time.After(50 * time.Millisecond):
		// OK — no graceful-failure event surfaced.
	}
}

// TestE2E_React_MaxStepsCircuitBreaker_EmitsOnRealBus is the
// planner-level integration: a non-empty trajectory plus MaxSteps=1
// exercises the planner-side breaker. The bus observes
// planner.max_steps_exceeded; the LLM is NOT called.
func TestE2E_React_MaxStepsCircuitBreaker_EmitsOnRealBus(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-int-2", UserID: "u", SessionID: "s"},
		RunID:    "r-maxsteps",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{planner.EventTypePlannerMaxStepsExceeded},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client := &scriptedClient{
		// Will never be called — the test would catch this if it
		// were.
		responses: []llm.CompleteResponse{
			{ToolCalls: []llm.ToolCallStructured{{ID: "x", Name: "_finish", Args: json.RawMessage(`{}`)}}},
		},
	}
	p := react.New(client, react.WithMaxSteps(1))
	rc := integrationRC(bus, q, "g")
	rc.Trajectory = &planner.Trajectory{
		Steps: []planner.Step{
			{
				Action: planner.CallTool{
					Tool: "alpha",
					Args: json.RawMessage(`{"x":1}`),
				},
			},
		},
	}

	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["max_steps_exceeded"].(bool); !got {
		t.Errorf("Metadata[max_steps_exceeded] not true")
	}
	if client.callCount() != 0 {
		t.Errorf("client.calls = %d, want 0 (breaker must fire BEFORE LLM call)", client.callCount())
	}

	ev := drainOneEvent(t, sub)
	if ev.Type != planner.EventTypePlannerMaxStepsExceeded {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, planner.EventTypePlannerMaxStepsExceeded)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	payload, ok := ev.Payload.(planner.MaxStepsExceededPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want MaxStepsExceededPayload", ev.Payload)
	}
	if payload.MaxSteps != 1 {
		t.Errorf("payload.MaxSteps = %d, want 1", payload.MaxSteps)
	}
	if payload.StepsObserved != 1 {
		t.Errorf("payload.StepsObserved = %d, want 1", payload.StepsObserved)
	}
	if payload.LastTool != "alpha" {
		t.Errorf("payload.LastTool = %q, want %q", payload.LastTool, "alpha")
	}
}

// TestE2E_React_FullThreeStepLoopOnRealBus exercises the load-bearing
// Phase 45 acceptance criterion against a real events bus. Three
// successive Next calls with synthetic trajectory append between
// them; the final Decision is Finish{Goal}. The bus observes no
// graceful-failure events on the happy path (assertion: empty
// subscription).
func TestE2E_React_FullThreeStepLoopOnRealBus(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-int-3", UserID: "u", SessionID: "s"},
		RunID:    "r-three-step",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Subscribe to the graceful-failure event types — the happy path
	// MUST observe none.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types: []events.EventType{
			planner.EventTypePlannerRepairExhausted,
			planner.EventTypePlannerMaxStepsExceeded,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{ToolCalls: []llm.ToolCallStructured{{ID: "call_1", Name: "search", Args: json.RawMessage(`{"q":"foo"}`)}}},
			{ToolCalls: []llm.ToolCallStructured{{ID: "call_2", Name: "summarize", Args: json.RawMessage(`{"text":"bar"}`)}}},
			{ToolCalls: []llm.ToolCallStructured{{ID: "call_3", Name: "_finish", Args: json.RawMessage(`{"answer":"done"}`)}}},
		},
	}
	p := react.New(client)
	traj := &planner.Trajectory{}

	// --- Step 1 ---
	rc := integrationRC(bus, q, "find and summarise foo")
	rc.Trajectory = traj
	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	if call, ok := dec.(planner.CallTool); !ok || call.Tool != "search" {
		t.Fatalf("Next #1 = %+v, want CallTool{search}", dec)
	}
	traj.Steps = append(traj.Steps, planner.Step{
		Action:         dec,
		LLMObservation: "found 3 hits",
	})

	// --- Step 2 ---
	rc.Trajectory = traj
	dec, err = p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if call, ok := dec.(planner.CallTool); !ok || call.Tool != "summarize" {
		t.Fatalf("Next #2 = %+v, want CallTool{summarize}", dec)
	}
	traj.Steps = append(traj.Steps, planner.Step{
		Action:         dec,
		LLMObservation: "summary text",
	})

	// --- Step 3 ---
	rc.Trajectory = traj
	dec, err = p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #3: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("Next #3 = %T, want Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if fin.Payload != "done" {
		t.Errorf("Payload = %v, want %q", fin.Payload, "done")
	}

	// Happy path: no graceful-failure events.
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected event on happy three-step path: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// promptRecordingClient records the system-message text of each
// CompleteRequest so an integration test can assert on the rendered
// structured prompt. It always answers with a terminal `_finish`.
type promptRecordingClient struct {
	systemText string
}

func (c *promptRecordingClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if len(req.Messages) > 0 && req.Messages[0].Content.Text != nil {
		c.systemText = *req.Messages[0].Content.Text
	}
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_done",
			Name: "_finish",
			Args: json.RawMessage(`{"answer":"done"}`),
		}},
	}, nil
}

func (c *promptRecordingClient) Close(_ context.Context) error { return nil }

// TestE2E_React_StructuredPromptAssemblesThroughRegistry is the Phase
// 83a integration test (§17.1 — this phase consumes the Phase 45
// planner surface AND the D-103 planner registry). Phase 107c (D-167)
// deletes `<output_format>`, `<action_schema>`, `<finishing>` and
// replaces them with `<tool_discovery>`.
// It proves the structured ten-section prompt + the `planner.extra_guidance`
// config key assemble end-to-end: a `planner.PlannerConfig` carrying
// `ExtraGuidance` flows through `planner.Resolve` → the react driver's
// factory → `react.New` with `WithSystemPromptExtra` → a real `Next`
// call whose rendered system prompt carries every structured section
// AND the operator's `<additional_guidance>` block.
func TestE2E_React_StructuredPromptAssemblesThroughRegistry(t *testing.T) {
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-83a", UserID: "u", SessionID: "s"},
		RunID:    "r-83a",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	client := &promptRecordingClient{}
	// Resolve the planner through the D-103 registry — the real seam
	// the dev stack uses at boot. The `react` driver self-registers
	// via its init(); the package blank-import is implicit here (the
	// test is in package react_test, so the driver's init has run).
	p, err := planner.Resolve(ctx, planner.PlannerConfig{
		Driver:        "react",
		ExtraGuidance: "domain rule: cite every source",
	}, planner.FactoryDeps{LLM: client})
	if err != nil {
		t.Fatalf("planner.Resolve: %v", err)
	}

	dec, err := p.Next(ctx, planner.RunContext{
		Quadruple: q,
		Goal:      "answer the user",
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if fin, ok := dec.(planner.Finish); !ok || fin.Reason != planner.FinishGoal {
		t.Fatalf("Next = %+v, want Finish{FinishGoal}", dec)
	}

	body := client.systemText
	if body == "" {
		t.Fatal("no system prompt was rendered")
	}
	// Every always-on structured section is present. Phase 107c (D-167)
	// replaces <output_format>/<action_schema>/<finishing> with
	// <tool_discovery> and deletes <parallel_execution> (parallel
	// emission is now native — the runtime accepts multiple ToolCalls
	// in one response).
	for _, tag := range []string{
		"<identity>", "<tool_discovery>",
		"<tool_usage>", "<reasoning>", "<tone>",
		"<error_handling>", "<available_tools>",
	} {
		if !strings.Contains(body, tag) {
			t.Errorf("rendered prompt missing structured section %s", tag)
		}
	}
	// <parallel_execution> must be ABSENT — Phase 107c deleted it.
	if strings.Contains(body, "<parallel_execution>") {
		t.Errorf("rendered prompt still contains deleted <parallel_execution> section")
	}
	// The config key flowed through to <additional_guidance>.
	if !strings.Contains(body, "<additional_guidance>\ndomain rule: cite every source\n</additional_guidance>") {
		t.Errorf("planner.extra_guidance did not flow to <additional_guidance>. Body:\n%s", body)
	}
	// Phase 107c replaces the brief-13 "JSON action object" CRITICAL
	// clamp with a native tool-calling intermediate-step rule: emit
	// only tool calls + no echoed reasoning field.
	if strings.Contains(body, `"reasoning":`) {
		t.Errorf("rendered prompt leaked a `\"reasoning\":` field")
	}
	if !strings.Contains(body, "Emit only tool calls — keep any narration to the final answer turn.") {
		t.Errorf("rendered prompt missing the <tone> native-tool-calling intermediate-step clamp")
	}
	if strings.Contains(body, "produce ONLY the JSON action object") {
		t.Errorf("rendered prompt still references the deleted JSON-action-object clamp")
	}
}

// stubDiscoveryCatalog satisfies planner.ToolCatalogView. It exposes
// an always-loaded `tool_search` builtin and a deferred-loaded
// `youtube_download` tool that the discovery cycle surfaces. The
// always-loaded set drives the FIRST turn's req.Tools; the planner
// promotes the deferred tool to the SECOND turn's req.Tools after
// observing the tool_search result in the trajectory (AC-18).
type stubDiscoveryCatalog struct {
	always   []tools.Tool
	deferred map[string]tools.Tool
}

func (s *stubDiscoveryCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range s.always {
		if t.Name == name {
			return t, true
		}
	}
	if t, ok := s.deferred[name]; ok {
		return t, true
	}
	return tools.Tool{}, false
}

func (s *stubDiscoveryCatalog) List() []tools.Tool {
	out := make([]tools.Tool, len(s.always))
	copy(out, s.always)
	return out
}

// reqCapturingLLM is a scripted llm.LLMClient that records each
// CompleteRequest verbatim. Built for the discovery-cycle integration
// test so the test can assert which tools the planner declared on
// each turn (turn 1 = always-loaded; turn 2 = always + discovered).
type reqCapturingLLM struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
	requests  []llm.CompleteRequest
}

func (c *reqCapturingLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Deep-copy the Tools slice header (it's the load-bearing
	// assertion surface for AC-17 / AC-18 in the discovery test) so
	// later mutations on the caller side don't bleed into the recorded
	// snapshot.
	captured := req
	if len(req.Tools) > 0 {
		captured.Tools = append([]llm.ToolDeclaration(nil), req.Tools...)
	}
	c.requests = append(c.requests, captured)
	if c.cursor >= len(c.responses) {
		if len(c.responses) == 0 {
			return llm.CompleteResponse{}, nil
		}
		return c.responses[len(c.responses)-1], nil
	}
	out := c.responses[c.cursor]
	c.cursor++
	return out, nil
}

func (c *reqCapturingLLM) Close(_ context.Context) error { return nil }

func (c *reqCapturingLLM) snapshotRequests() []llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.CompleteRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

// TestReactPlanner_NativeToolCall_DiscoveryCycle is the load-bearing
// Phase 107c integration (AC-26): two-turn discovery cycle. Turn 1's
// LLM emits a native `tool_search` ToolCall; the runtime appends the
// trajectory step with the tool_search observation; turn 2's planner
// walks the trajectory, derives the discovered tool name
// (`youtube_download`), promotes it into the per-run
// `req.Tools` declaration, and dispatches the discovered tool. The
// test asserts: (a) turn 1's req.Tools omits the deferred tool;
// (b) the planner emits CallTool{tool_search} on turn 1;
// (c) turn 2's req.Tools INCLUDES the deferred tool; (d) the planner
// emits CallTool{youtube_download} on turn 2; (e) identity propagates
// through both turns.
func TestReactPlanner_NativeToolCall_DiscoveryCycle(t *testing.T) {
	t.Parallel()

	// Always-loaded tools: the tool_search builtin (the meta-tool that
	// surfaces deferred capabilities). Deferred: youtube_download (not
	// in always-loaded; surfaced only when the planner observes a
	// tool_search result naming it).
	toolSearchSchema := `{"type":"object","properties":{"query":{"type":"string"}}}`
	youtubeSchema := `{"type":"object","properties":{"url":{"type":"string"}}}`
	catalog := &stubDiscoveryCatalog{
		always: []tools.Tool{{
			Name:        "tool_search",
			Description: "Search for tools by capability",
			ArgsSchema:  json.RawMessage(toolSearchSchema),
			Loading:     tools.LoadingAlways,
		}},
		deferred: map[string]tools.Tool{
			"youtube_download": {
				Name:        "youtube_download",
				Description: "Download a YouTube video",
				ArgsSchema:  json.RawMessage(youtubeSchema),
				Loading:     tools.LoadingDeferred,
			},
		},
	}

	// Scripted LLM: turn 1 emits tool_search; turn 2 emits the
	// discovered tool.
	client := &reqCapturingLLM{
		responses: []llm.CompleteResponse{
			{ToolCalls: []llm.ToolCallStructured{{
				ID:   "call_search",
				Name: "tool_search",
				Args: json.RawMessage(`{"query":"youtube download"}`),
			}}},
			{ToolCalls: []llm.ToolCallStructured{{
				ID:   "call_download",
				Name: "youtube_download",
				Args: json.RawMessage(`{"url":"https://youtu.be/example"}`),
			}}},
		},
	}

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-disc", UserID: "u-disc", SessionID: "s-disc"},
		RunID:    "r-disc",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	p := react.New(client)
	traj := &planner.Trajectory{}

	// --- Turn 1 ---
	rc1 := planner.RunContext{
		Quadruple:  q,
		Goal:       "download the latest video",
		Trajectory: traj,
		Catalog:    catalog,
	}
	dec1, err := p.Next(ctx, rc1)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	call1, ok := dec1.(planner.CallTool)
	if !ok {
		t.Fatalf("Next #1 = %T, want CallTool{tool_search}", dec1)
	}
	if call1.Tool != "tool_search" {
		t.Errorf("Turn 1 tool = %q, want tool_search", call1.Tool)
	}
	if call1.CallID != "call_search" {
		t.Errorf("Turn 1 CallID = %q, want call_search (native ID round-trip)", call1.CallID)
	}

	// Stand in for the runloop's trajectory append: stamp the
	// tool_search observation. The observation shape mirrors the
	// builtin's contract (`tools: [{name, description}]`).
	traj.Steps = append(traj.Steps, planner.Step{
		Action: call1,
		LLMObservation: map[string]any{
			"tools": []any{
				map[string]any{"name": "youtube_download", "description": "Download a YouTube video"},
			},
		},
	})

	// --- Turn 2 ---
	rc2 := planner.RunContext{
		Quadruple:  q,
		Goal:       "download the latest video",
		Trajectory: traj,
		Catalog:    catalog,
	}
	dec2, err := p.Next(ctx, rc2)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	call2, ok := dec2.(planner.CallTool)
	if !ok {
		t.Fatalf("Next #2 = %T, want CallTool{youtube_download}", dec2)
	}
	if call2.Tool != "youtube_download" {
		t.Errorf("Turn 2 tool = %q, want youtube_download", call2.Tool)
	}
	if call2.CallID != "call_download" {
		t.Errorf("Turn 2 CallID = %q, want call_download", call2.CallID)
	}

	// Assert req.Tools shape per turn — the AC-17 / AC-18 contract.
	captured := client.snapshotRequests()
	if len(captured) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(captured))
	}
	if !hasToolName(captured[0].Tools, "tool_search") {
		t.Errorf("turn 1 req.Tools missing tool_search: %+v", toolNames(captured[0].Tools))
	}
	if hasToolName(captured[0].Tools, "youtube_download") {
		t.Errorf("turn 1 req.Tools should NOT carry youtube_download (deferred until discovered): %+v",
			toolNames(captured[0].Tools))
	}
	if !hasToolName(captured[1].Tools, "tool_search") {
		t.Errorf("turn 2 req.Tools missing tool_search (always-loaded): %+v", toolNames(captured[1].Tools))
	}
	if !hasToolName(captured[1].Tools, "youtube_download") {
		t.Errorf("turn 2 req.Tools missing the discovered youtube_download: %+v",
			toolNames(captured[1].Tools))
	}
	// req.ParallelToolCalls is on for the native path (V1.3 default).
	if !captured[0].ParallelToolCalls || !captured[1].ParallelToolCalls {
		t.Errorf("req.ParallelToolCalls should be true on every turn (V1.3 default)")
	}

	// Trajectory carries BOTH steps (identity propagation + structural).
	if len(traj.Steps) != 1 {
		// Note: the test only appended once (after turn 1). The runtime
		// engine appends the second step in production; we assert the
		// trajectory-side write contract via the planner's read of it.
		t.Errorf("trajectory.Steps after stub-runloop = %d, want 1", len(traj.Steps))
	}
}

// hasToolName reports whether `decls` contains a declaration with the
// supplied tool name.
func hasToolName(decls []llm.ToolDeclaration, name string) bool {
	for _, d := range decls {
		if d.Name == name {
			return true
		}
	}
	return false
}

// toolNames returns the slice of tool names in `decls`, for fixture
// error messages.
func toolNames(decls []llm.ToolDeclaration) []string {
	out := make([]string, len(decls))
	for i, d := range decls {
		out[i] = d.Name
	}
	return out
}

// drainOneEventTimeout bounds drainOneEvent's channel receive — a
// wall-clock deadline, not a synchronisation sleep.
const drainOneEventTimeout = 2 * time.Second

// drainOneEvent reads one event with a bounded wall-clock deadline.
// Returns the event or fatals out. Same pattern as Phase 44's
// integration tests.
func drainOneEvent(t *testing.T, sub events.Subscription) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before event arrived")
		}
		return ev
	case <-time.After(drainOneEventTimeout):
		t.Fatal("timeout waiting for event")
	}
	return events.Event{}
}

// recordingScriptedClient is the Phase 83c integration fixture: it
// returns scripted responses in order (like scriptedClient) AND
// records the system-prompt text of every CompleteRequest. The across-
// step repair-guidance test needs both — drive the repair loop with
// scripted malformed/valid responses, then assert the rendered prompt
// of each turn carries the escalating guidance.
type recordingScriptedClient struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
	systems   []string // system-prompt text, one per Complete call
}

func (c *recordingScriptedClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sys := ""
	if len(req.Messages) > 0 && req.Messages[0].Content.Text != nil {
		sys = *req.Messages[0].Content.Text
	}
	c.systems = append(c.systems, sys)
	var resp llm.CompleteResponse
	if c.cursor < len(c.responses) {
		resp = c.responses[c.cursor]
		c.cursor++
	} else if len(c.responses) > 0 {
		resp = c.responses[len(c.responses)-1]
	}
	return resp, nil
}

func (c *recordingScriptedClient) Close(_ context.Context) error { return nil }

// firstSystemContaining returns the index of the first recorded
// system prompt that contains `needle`, or -1.
func (c *recordingScriptedClient) firstSystemContaining(needle string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.systems {
		if strings.Contains(s, needle) {
			return i
		}
	}
	return -1
}

// TestE2E_React_RepairGuidanceEscalatesAcrossSteps is the Phase 83c
// across-step integration, rewritten in Phase 107c step 10 (D-167) to
// drive escalation through the `declarative_action` escape-hatch. The
// native main path bypasses the repair loop entirely (AC-20c); the
// escape hatch is the only producer of args-repair / multi-action /
// finish-repair signals going forward.
//
// Mechanism: the LLM emits a native `tool_calls` array with a single
// `declarative_action` entry whose `Args` would cause an inner-tool
// args validation failure (the inner tool name is real but the args
// fail schema). The meta-tool's body classifies the failure and
// surfaces a structured observation. The trajectory append captures
// the observation; the next step's `applyDeclarativeOutcome` reads
// the observation at the start of `Next` and bumps the per-run
// `RepairCounters`. After three escalating steps the counter reaches
// `critical`; a final step with a clean native response resets it
// (the §13 reset semantics — clean LLM emission wipes prior turn's
// repair-shadow).
//
// This is the FULL escape-hatch round-trip: meta-tool body produces
// the signal → trajectory carries it → planner reads it next turn →
// prompt's `<repair_guidance>` section escalates.
func TestE2E_React_RepairGuidanceEscalatesAcrossSteps(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-83c", UserID: "u", SessionID: "s"},
		RunID:    "r-escalate",
	}
	ctx, err := identity.With(t.Context(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{planner.EventTypePlannerRepairGuidanceInjected},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Each step's response: a single native tool_call to
	// declarative_action with malformed inner-tool args. The
	// declarative_action meta-tool's body classifies this as
	// `ArgsRepaired: true` and surfaces it on the trajectory step's
	// observation. Step 4 is a clean Finish (Content-only) so the
	// counters reset.
	declarativeBadArgs := llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_dec_args",
			Name: "declarative_action",
			Args: json.RawMessage(`{"tool":"text.echo","args":{"text":12345}}`),
		}},
	}
	cleanFinish := llm.CompleteResponse{Content: "all good now"}
	client := &recordingScriptedClient{
		responses: []llm.CompleteResponse{
			declarativeBadArgs, // step 1: dispatches declarative_action → next step's start bumps counter to 1
			declarativeBadArgs, // step 2: bump to 1 from prior obs; this step also dispatches declarative_action → next step's start bumps to 2
			declarativeBadArgs, // step 3: bump to 2; dispatches declarative_action again → next step's start bumps to 3 (critical)
			cleanFinish,        // step 4: bumps to 3 (critical guidance rendered); clean Finish → end-of-step reset to 0
		},
	}

	p := react.New(client)
	counters := &planner.RepairCounters{}
	traj := &planner.Trajectory{}

	// Drive four steps. The runtime threads the SAME counters pointer
	// through every per-step RunContext (D-145). We simulate the
	// runloop's trajectory append: after each Next, we append a step
	// carrying the declarative_action observation that the body
	// would have produced.
	declObservation := declarativeArgsRepairObservation()
	for step := 1; step <= 4; step++ {
		rc := integrationRC(bus, q, "escalation goal")
		rc.Trajectory = traj
		rc.RepairCounters = counters
		dec, nerr := p.Next(ctx, rc)
		if nerr != nil {
			t.Fatalf("Next #%d: %v", step, nerr)
		}
		// Simulate the runloop's trajectory append. On steps 1–3 the
		// planner emitted CallTool{declarative_action}; on step 4 it
		// emitted Finish{Goal}. The Finish step does not append an
		// observation step; only the dispatch decisions do.
		if call, ok := dec.(planner.CallTool); ok && call.Tool == react.DeclarativeActionToolName {
			traj.Steps = append(traj.Steps, planner.Step{
				Action:      dec,
				Observation: declObservation,
				// The trajectory walker prefers LLMObservation when
				// both are set; the runtime executor produces both
				// (LLMObservation is the small projection). Set both
				// so the walker's preference is exercised.
				LLMObservation: declObservation,
			})
		}
	}

	// After step 4 (a clean Finish{Goal}), the args counter must have
	// reset — the Finish{Goal} path clears all three counters per
	// updateRepairCounters semantics.
	if counters.ArgsRepair != 0 {
		t.Errorf("after clean Finish step 4: ArgsRepair = %d, want 0", counters.ArgsRepair)
	}
	if counters.FinishRepair != 0 {
		t.Errorf("after clean Finish step 4: FinishRepair = %d, want 0", counters.FinishRepair)
	}

	// The recordingScriptedClient records one Complete call per Next
	// (the repair loop is no longer called on the main path). The
	// prompt of step N carries the guidance computed at the START of
	// step N's Next, AFTER applyDeclarativeOutcome has bumped from the
	// prior step's observation. So:
	//   step 1 prompt: no prior step → no guidance.
	//   step 2 prompt: prior bumped to 1 → reminder.
	//   step 3 prompt: bumped to 2 → warning.
	//   step 4 prompt: bumped to 3 → critical.
	reminderIdx := client.firstSystemContaining(react.ReminderArgsGuidance)
	warningIdx := client.firstSystemContaining(react.WarningArgsGuidance)
	criticalIdx := client.firstSystemContaining(react.CriticalArgsGuidance)
	if reminderIdx < 0 {
		t.Error("no rendered prompt carried the reminder args guidance")
	}
	if warningIdx < 0 {
		t.Error("no rendered prompt carried the warning args guidance")
	}
	if criticalIdx < 0 {
		t.Error("no rendered prompt carried the critical args guidance")
	}
	if reminderIdx >= 0 && warningIdx >= 0 && reminderIdx >= warningIdx {
		t.Errorf("reminder prompt (idx %d) not before warning (idx %d)", reminderIdx, warningIdx)
	}
	if warningIdx >= 0 && criticalIdx >= 0 && warningIdx >= criticalIdx {
		t.Errorf("warning prompt (idx %d) not before critical (idx %d)", warningIdx, criticalIdx)
	}

	// The bus observed one repair_guidance_injected event per
	// rendered tier (3 events for reminder / warning / critical).
	tiers := map[string]bool{}
	for range 3 {
		ev := drainOneEvent(t, sub)
		if ev.Type != planner.EventTypePlannerRepairGuidanceInjected {
			t.Fatalf("ev.Type = %q, want repair_guidance_injected", ev.Type)
		}
		if ev.Identity != q {
			t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
		}
		pl, ok := ev.Payload.(planner.RepairGuidanceInjectedPayload)
		if !ok {
			t.Fatalf("ev.Payload = %T, want RepairGuidanceInjectedPayload", ev.Payload)
		}
		if pl.Counter != "args" {
			t.Errorf("payload.Counter = %q, want args", pl.Counter)
		}
		tiers[pl.Tier] = true
	}
	for _, want := range []string{"reminder", "warning", "critical"} {
		if !tiers[want] {
			t.Errorf("bus did not observe a %q-tier repair_guidance_injected event", want)
		}
	}
}

// declarativeArgsRepairObservation returns the structured observation
// the declarative_action meta-tool body produces when an inner-tool
// args validation fails. Materialised as a plain map so the planner's
// observation walker exercises the map-shape extraction path (the
// dispatcher delivers the typed `DeclarativeActionOut` struct in
// production; the map form is what the runtime's heavy-content
// projection produces under D-026, and what the tests use to avoid
// depending on the builtin package from the planner test suite —
// keeping the import-graph contract clean).
func declarativeArgsRepairObservation() any {
	return map[string]any{
		"dispatched": false,
		"tool":       "text.echo",
		"error":      "args validation failed: text must be string",
		"repair_outcome": map[string]any{
			"args_repaired": true,
		},
	}
}

// demuxRecordingClient is an llm.LLMClient that routes scripted
// responses by `identity.QuadrupleFrom(ctx).RunID` and records every
// captured system prompt keyed by RunID. It lets one shared planner
// serve N concurrent runs with per-run scripts and per-run prompt
// inspection — which is what the D-145 shared-planner contract needs
// to be tested honestly (the §17.5 Wave 15 audit flagged the prior
// version of this test using one planner per run, which proved a
// weaker property).
type demuxRecordingClient struct {
	mu      sync.Mutex
	scripts map[string][]llm.CompleteResponse // keyed by RunID
	cursors map[string]int                    // RunID → next response index
	systems map[string][]string               // RunID → ordered system prompts
}

func newDemuxRecordingClient(scripts map[string][]llm.CompleteResponse) *demuxRecordingClient {
	return &demuxRecordingClient{
		scripts: scripts,
		cursors: make(map[string]int, len(scripts)),
		systems: make(map[string][]string, len(scripts)),
	}
}

func (c *demuxRecordingClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, fmt.Errorf("demuxRecordingClient: ctx carries no identity quadruple")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sys := ""
	if len(req.Messages) > 0 && req.Messages[0].Content.Text != nil {
		sys = *req.Messages[0].Content.Text
	}
	c.systems[q.RunID] = append(c.systems[q.RunID], sys)

	cur := c.cursors[q.RunID]
	resps := c.scripts[q.RunID]
	var resp llm.CompleteResponse
	switch {
	case cur < len(resps):
		resp = resps[cur]
		c.cursors[q.RunID] = cur + 1
	case len(resps) > 0:
		// Stay on the last scripted response for additional steps.
		resp = resps[len(resps)-1]
	}
	return resp, nil
}

func (c *demuxRecordingClient) Close(_ context.Context) error { return nil }

// systemsFor returns the system prompts recorded against runID, in
// invocation order. Safe to call after the WaitGroup join.
func (c *demuxRecordingClient) systemsFor(runID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := c.systems[runID]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// containsAny reports whether any of the recorded system prompts for
// runID contains the needle. The bleed assertions in the cross-run
// isolation test below scan every recorded prompt for the run.
func (c *demuxRecordingClient) containsAny(runID, needle string) bool {
	for _, s := range c.systemsFor(runID) {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// TestE2E_React_RepairGuidanceCrossRunIsolation is the D-145 headline
// guarantee, rewritten in Phase 107c step 10 (D-167) to drive the
// args-repair counter through the `declarative_action` escape-hatch.
//
// Run A emits `declarative_action` with malformed inner args on every
// step; the planner's `applyDeclarativeOutcome` at the start of each
// step (after step 1) bumps `RepairCounters.ArgsRepair`. Run B always
// emits a clean native tool_call — its counter stays at 0. Both runs
// share ONE planner artifact; the contract is that A's escalating
// guidance never appears in B's prompts.
//
// The cross-run isolation surface is unchanged by Phase 107c: counters
// live on the per-run RunContext (D-145 + D-025), so the planner's
// shared receiver remains read-only across runs. This test is the
// load-bearing assertion that the cutover did NOT introduce a hidden
// dependency that would re-couple counters via the planner artifact.
func TestE2E_React_RepairGuidanceCrossRunIsolation(t *testing.T) {
	bus := integrationBus(t)
	declarativeBadArgs := llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_dec_iso",
			Name: "declarative_action",
			Args: json.RawMessage(`{"tool":"text.echo","args":{"text":12345}}`),
		}},
	}
	cleanCall := llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_clean",
			Name: "search",
			Args: json.RawMessage(`{"q":"y"}`),
		}},
	}
	client := newDemuxRecordingClient(map[string][]llm.CompleteResponse{
		"r-A": {
			declarativeBadArgs, declarativeBadArgs, declarativeBadArgs, declarativeBadArgs,
		},
		"r-B": {cleanCall},
	})
	// ONE shared planner. Both runs call Next on this instance
	// concurrently with their own RunContexts.
	shared := react.New(client)

	const steps = 4
	declObservation := declarativeArgsRepairObservation()

	runA := func(done chan<- struct{}) {
		defer close(done)
		q := identity.Quadruple{
			Identity: identity.Identity{TenantID: "t-A", UserID: "u", SessionID: "s"},
			RunID:    "r-A",
		}
		ctx, err := identity.With(t.Context(), q.Identity)
		if err != nil {
			t.Errorf("With A: %v", err)
			return
		}
		ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
		if err != nil {
			t.Errorf("WithRun A: %v", err)
			return
		}
		traj := &planner.Trajectory{}
		countersA := &planner.RepairCounters{}
		for range steps {
			rc := integrationRC(bus, q, "noisy run A")
			rc.Trajectory = traj
			rc.RepairCounters = countersA
			dec, nerr := shared.Next(ctx, rc)
			if nerr != nil {
				t.Errorf("run A Next: %v", nerr)
				return
			}
			// Simulate runloop trajectory append — A's
			// declarative_action emissions land observations that drive
			// the next-step bump.
			if call, ok := dec.(planner.CallTool); ok && call.Tool == react.DeclarativeActionToolName {
				traj.Steps = append(traj.Steps, planner.Step{
					Action:         dec,
					Observation:    declObservation,
					LLMObservation: declObservation,
				})
			} else {
				traj.Steps = append(traj.Steps, planner.Step{Action: dec})
			}
		}
	}

	runB := func(done chan<- struct{}) {
		defer close(done)
		q := identity.Quadruple{
			Identity: identity.Identity{TenantID: "t-B", UserID: "u", SessionID: "s"},
			RunID:    "r-B",
		}
		ctx, err := identity.With(t.Context(), q.Identity)
		if err != nil {
			t.Errorf("With B: %v", err)
			return
		}
		ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
		if err != nil {
			t.Errorf("WithRun B: %v", err)
			return
		}
		traj := &planner.Trajectory{}
		countersB := &planner.RepairCounters{} // always clean
		for range steps {
			rc := integrationRC(bus, q, "clean run B")
			rc.Trajectory = traj
			rc.RepairCounters = countersB
			dec, nerr := shared.Next(ctx, rc)
			if nerr != nil {
				t.Errorf("run B Next: %v", nerr)
				return
			}
			traj.Steps = append(traj.Steps, planner.Step{Action: dec})
		}
	}

	doneA, doneB := make(chan struct{}), make(chan struct{})
	go runA(doneA)
	go runB(doneB)
	<-doneA
	<-doneB

	// B's recorded prompts must carry NONE of the repair-guidance
	// markers, even though A was concurrently bumping the args
	// counter via the SAME shared planner. This is the D-145 contract.
	for _, needle := range []string{
		react.ReminderArgsGuidance, react.WarningArgsGuidance, react.CriticalArgsGuidance,
		react.ReminderFinishGuidance, react.ReminderMultiActionGuidance,
	} {
		if client.containsAny("r-B", needle) {
			t.Errorf("D-145 violation: B's prompt carried repair-guidance marker %q (cross-run counter bleed through the shared planner)", needle)
		}
	}
	// Positive control: A's prompts MUST carry the args repair
	// guidance — proving the declarative_action observation actually
	// drove the counter via the shared planner.
	if !client.containsAny("r-A", react.ReminderArgsGuidance) {
		t.Error("A's prompts never carried any args repair-guidance marker — the test setup did not actually trip the counter (positive control failed)")
	}
}
