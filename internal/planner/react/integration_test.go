package react_test

import (
	"context"
	"encoding/json"
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
