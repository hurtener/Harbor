package react_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
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

	// Malformed JSON forever — exhausts the repair loop's storm guard
	// (default MaxConsecutiveArgFailures=2).
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `not a JSON envelope at all`},
		},
	}
	p := react.New(client)
	rc := integrationRC(bus, q, "exhaust me")

	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want Finish (graceful failure)", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] not true — Phase 44 contract surface")
	}

	// The bus observes planner.repair_exhausted (from Phase 44's
	// loop) — NOT planner.max_steps_exceeded.
	ev := drainOneEvent(t, sub)
	if ev.Type != planner.EventTypePlannerRepairExhausted {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, planner.EventTypePlannerRepairExhausted)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	payload, ok := ev.Payload.(planner.RepairExhaustedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want RepairExhaustedPayload", ev.Payload)
	}
	if payload.ConsecutiveArgFailures < 2 {
		t.Errorf("payload.ConsecutiveArgFailures = %d, want ≥ 2", payload.ConsecutiveArgFailures)
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
			{Content: `{"tool":"_finish","args":{}}`},
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
			{Content: `{"tool":"search","args":{"q":"foo"},"reasoning":"step1"}`},
			{Content: `{"tool":"summarize","args":{"text":"bar"},"reasoning":"step2"}`},
			{Content: `{"tool":"_finish","args":{"answer":"done"},"reasoning":"step3"}`},
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
	return llm.CompleteResponse{Content: `{"tool":"_finish","args":{"answer":"done"}}`}, nil
}

func (c *promptRecordingClient) Close(_ context.Context) error { return nil }

// TestE2E_React_StructuredPromptAssemblesThroughRegistry is the Phase
// 83a integration test (§17.1 — this phase consumes the Phase 45
// planner surface AND the D-103 planner registry). It proves the
// structured twelve-section prompt + the `planner.extra_guidance`
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
	// Every always-on structured section is present.
	for _, tag := range []string{
		"<identity>", "<output_format>", "<action_schema>", "<finishing>",
		"<tool_usage>", "<parallel_execution>", "<reasoning>", "<tone>",
		"<error_handling>", "<available_tools>",
	} {
		if !strings.Contains(body, tag) {
			t.Errorf("rendered prompt missing structured section %s", tag)
		}
	}
	// The config key flowed through to <additional_guidance>.
	if !strings.Contains(body, "<additional_guidance>\ndomain rule: cite every source\n</additional_guidance>") {
		t.Errorf("planner.extra_guidance did not flow to <additional_guidance>. Body:\n%s", body)
	}
	// The CRITICAL clamp and the no-reasoning-field discipline hold.
	if strings.Contains(body, `"reasoning":`) {
		t.Errorf("rendered prompt leaked a `\"reasoning\":` field")
	}
	if !strings.Contains(body, "Do not include a 'thought' or 'reasoning' field in the JSON.") {
		t.Errorf("rendered prompt missing the <tone> CRITICAL clamp")
	}
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
// across-step integration (§17.1 — this phase consumes Phase 83a's
// prompt surface AND Phase 44's repair surface). Across four planner
// steps sharing ONE per-run RepairCounters, the runtime drives a
// steady transient args-repair on steps 1–3 (one malformed response
// recovered by a valid one inside the step), then a clean step 4.
//
// Asserts the escalation: step 2's prompt carries the `reminder`
// args guidance, step 3 the `warning`, step 4 the `critical`, and
// — after the clean step 4 resets the counter — a hypothetical step 5
// prompt would carry none. The real Phase 05 bus observes one
// `planner.repair_guidance_injected` event per injected block with
// the matching tier.
func TestE2E_React_RepairGuidanceEscalatesAcrossSteps(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-83c", UserID: "u", SessionID: "s"},
		RunID:    "r-escalate",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
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

	// Each step's Next issues: a malformed response (parser fails →
	// args-repair signal) then a valid CallTool the loop recovers
	// with. Step 4 issues a single clean response (no repair).
	malformed := llm.CompleteResponse{Content: `this is not json`}
	validCall := llm.CompleteResponse{Content: `{"tool":"search","args":{"q":"x"}}`}
	client := &recordingScriptedClient{
		responses: []llm.CompleteResponse{
			malformed, validCall, // step 1 — recovers, ArgsRepair → 1
			malformed, validCall, // step 2 — recovers, ArgsRepair → 2
			malformed, validCall, // step 3 — recovers, ArgsRepair → 3
			validCall, // step 4 — clean, ArgsRepair reset → 0
		},
	}

	p := react.New(client)
	counters := &planner.RepairCounters{}
	traj := &planner.Trajectory{}

	// Drive four steps. The runtime threads the SAME counters pointer
	// through every per-step RunContext (D-145).
	for step := 1; step <= 4; step++ {
		rc := integrationRC(bus, q, "escalation goal")
		rc.Trajectory = traj
		rc.RepairCounters = counters
		dec, nerr := p.Next(ctx, rc)
		if nerr != nil {
			t.Fatalf("Next #%d: %v", step, nerr)
		}
		traj.Steps = append(traj.Steps, planner.Step{Action: dec, LLMObservation: "obs"})
	}

	// After step 4 (a clean step), the args counter must have reset.
	if counters.ArgsRepair != 0 {
		t.Errorf("after clean step 4: ArgsRepair = %d, want 0", counters.ArgsRepair)
	}

	// Step N's prompt is built from the counter value AFTER step N-1.
	// Step 1's prompt: no guidance (counter 0). Step 2: reminder
	// (counter 1). Step 3: warning (counter 2). Step 4: critical
	// (counter 3). The recordingScriptedClient records 2 Complete
	// calls per repaired step; the prompt builder runs ONCE per Next,
	// so the system prompt of a step's FIRST Complete carries that
	// step's guidance. Both Complete calls within a step share the
	// builder's output, so a `firstSystemContaining` match is enough.
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
	// Escalation order: reminder before warning before critical.
	if reminderIdx >= 0 && warningIdx >= 0 && reminderIdx >= warningIdx {
		t.Errorf("reminder prompt (idx %d) not before warning (idx %d)", reminderIdx, warningIdx)
	}
	if warningIdx >= 0 && criticalIdx >= 0 && warningIdx >= criticalIdx {
		t.Errorf("warning prompt (idx %d) not before critical (idx %d)", warningIdx, criticalIdx)
	}

	// The bus observed one repair_guidance_injected event per injected
	// block — three turns injected guidance (steps 2, 3, 4).
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

// TestE2E_React_RepairGuidanceCrossRunIsolation is the D-145
// headline guarantee at the integration level: two concurrent runs
// share ONE *ReActPlanner but carry disjoint RepairCounters. Run A
// repeatedly trips the args counter; run B never does. Run B's
// rendered prompts must NEVER carry repair guidance — no cross-run
// counter bleed through the shared planner artifact.
func TestE2E_React_RepairGuidanceCrossRunIsolation(t *testing.T) {
	bus := integrationBus(t)
	p := react.New(&recordingScriptedClient{
		responses: []llm.CompleteResponse{{Content: `{"tool":"_finish","args":{"answer":"ok"}}`}},
	})

	// Run B: a clean run sharing the planner. Its own client + its
	// own (nil-then-zero) counters. We record B's prompts.
	runB := func() (badPrompt bool) {
		q := identity.Quadruple{
			Identity: identity.Identity{TenantID: "t-B", UserID: "u", SessionID: "s"},
			RunID:    "r-B",
		}
		ctx, cerr := identity.WithRun(t.Context(), q.Identity, q.RunID)
		if cerr != nil {
			t.Errorf("WithRun B: %v", cerr)
			return false
		}
		clientB := &recordingScriptedClient{
			responses: []llm.CompleteResponse{{Content: `{"tool":"search","args":{"q":"y"}}`}},
		}
		pB := react.New(clientB)
		traj := &planner.Trajectory{}
		countersB := &planner.RepairCounters{} // B's own counters, always clean
		for range 5 {
			rc := integrationRC(bus, q, "clean run B")
			rc.Trajectory = traj
			rc.RepairCounters = countersB
			dec, nerr := pB.Next(ctx, rc)
			if nerr != nil {
				t.Errorf("run B Next: %v", nerr)
				return false
			}
			traj.Steps = append(traj.Steps, planner.Step{Action: dec})
		}
		// B's rendered prompts must carry NO repair guidance.
		for _, needle := range []string{
			react.ReminderArgsGuidance, react.WarningArgsGuidance, react.CriticalArgsGuidance,
			react.ReminderFinishGuidance, react.ReminderMultiActionGuidance,
		} {
			if clientB.firstSystemContaining(needle) >= 0 {
				return true
			}
		}
		return false
	}

	// Run A: trips the args counter hard, concurrently with B.
	runA := func() {
		q := identity.Quadruple{
			Identity: identity.Identity{TenantID: "t-A", UserID: "u", SessionID: "s"},
			RunID:    "r-A",
		}
		ctx, cerr := identity.WithRun(t.Context(), q.Identity, q.RunID)
		if cerr != nil {
			t.Errorf("WithRun A: %v", cerr)
			return
		}
		clientA := &recordingScriptedClient{
			responses: []llm.CompleteResponse{
				{Content: `garbage`}, {Content: `{"tool":"search","args":{"q":"a"}}`},
			},
		}
		pA := react.New(clientA)
		traj := &planner.Trajectory{}
		countersA := &planner.RepairCounters{}
		for range 5 {
			rc := integrationRC(bus, q, "noisy run A")
			rc.Trajectory = traj
			rc.RepairCounters = countersA
			clientA.mu.Lock()
			clientA.cursor = 0 // replay malformed-then-valid each step
			clientA.mu.Unlock()
			dec, nerr := pA.Next(ctx, rc)
			if nerr != nil {
				t.Errorf("run A Next: %v", nerr)
				return
			}
			traj.Steps = append(traj.Steps, planner.Step{Action: dec})
		}
	}

	var wg sync.WaitGroup
	var bLeaked atomic.Bool
	wg.Add(2)
	go func() { defer wg.Done(); runA() }()
	go func() { defer wg.Done(); bLeaked.Store(runB()) }()
	wg.Wait()
	_ = p // the top-level planner is intentionally unused beyond construction

	if bLeaked.Load() {
		t.Error("run B's prompt carried repair guidance — cross-run counter bleed (D-145 violation)")
	}
}
