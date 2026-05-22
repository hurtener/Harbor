package repair_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// integrationBus opens a real inmem events.EventBus for the
// integration tests. Tears down via t.Cleanup. The bus is the
// real Phase 05 wiring; we don't mock the seam (§17.3).
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
func integrationRC(bus events.EventBus, q identity.Quadruple) planner.RunContext {
	return planner.RunContext{
		Quadruple: q,
		Emit: func(ev events.Event) {
			// Stamp identity defensively — production wiring also does
			// this; our test mirrors the contract.
			ev.Identity = q
			_ = bus.Publish(context.Background(), ev)
		},
	}
}

// TestE2E_RepairLoop_RecoversOnRetry is the positive integration test:
// a stub llm.LLMClient returns malformed JSON for the first N attempts
// and a valid envelope on (N+1)th; the loop must return the valid
// CallTool.
func TestE2E_RepairLoop_RecoversOnRetry(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r-recover",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `garbage no JSON here`},                        // attempt 1: parse fails
			{Content: `still garbage`},                               // attempt 2: parse fails
			{Content: `{"tool":"answer","args":{"text":"finally"}}`}, // attempt 3: valid
		},
	}

	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            4,
		MaxConsecutiveArgFailures: 5,
	})

	dec, runErr := loop.Run(ctx, integrationRC(bus, q), client, sampleRequest(), passValidator)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	call, ok := dec.Decision.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if call.Tool != "answer" {
		t.Errorf("Tool = %q, want %q", call.Tool, "answer")
	}
	if client.callCount() != 3 {
		t.Errorf("client.calls = %d, want 3", client.callCount())
	}

	// Identity propagation: every Complete call carried the run's
	// quadruple in ctx.
	for i, sc := range client.snapshot() {
		if sc.id.TenantID != "t1" || sc.id.RunID != "r-recover" {
			t.Errorf("call %d identity = %+v, want tenant=t1 run=r-recover", i, sc.id)
		}
	}
}

// TestE2E_RepairLoop_GracefulFailure_EmitsEvent is the negative
// integration test: a stub LLM returns malformed JSON forever; the
// loop returns Finish{NoPath} AND emits planner.repair_exhausted on
// the real bus with the correct identity.
func TestE2E_RepairLoop_GracefulFailure_EmitsEvent(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"},
		RunID:    "r-fail",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Subscribe to the repair-exhausted event BEFORE we run.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  "t2",
		User:    "u2",
		Session: "s2",
		Types:   []events.EventType{planner.EventTypePlannerRepairExhausted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `not JSON at all`}, // repeats forever (the stub recycles last)
		},
	}

	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            5,
		MaxConsecutiveArgFailures: 2,
	})

	dec, runErr := loop.Run(ctx, integrationRC(bus, q), client, sampleRequest(), passValidator)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	fin, ok := dec.Decision.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] not true")
	}
	if errs, _ := fin.Metadata["repair_error"].(string); errs == "" {
		t.Errorf("Metadata[repair_error] empty")
	}

	// Receive the event from the real bus.
	ev := drainOneEvent(t, sub, 2*time.Second)
	if ev.Type != planner.EventTypePlannerRepairExhausted {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, planner.EventTypePlannerRepairExhausted)
	}
	if ev.Identity.TenantID != "t2" || ev.Identity.RunID != "r-fail" {
		t.Errorf("ev.Identity = %+v, want tenant=t2 run=r-fail", ev.Identity)
	}
	payload, ok := ev.Payload.(planner.RepairExhaustedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want RepairExhaustedPayload", ev.Payload)
	}
	if payload.ConsecutiveArgFailures < 2 {
		t.Errorf("payload.ConsecutiveArgFailures = %d, want ≥ 2", payload.ConsecutiveArgFailures)
	}
	if len(payload.Reasons) < 2 {
		t.Errorf("payload.Reasons len = %d, want ≥ 2", len(payload.Reasons))
	}
}

// TestE2E_RepairLoop_MultiActionSalvageOnRealBus verifies the multi-
// action salvage path produces CallParallel with a real bus wired
// (no events expected — multi-action salvage is a success path).
func TestE2E_RepairLoop_MultiActionSalvageOnRealBus(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t3", UserID: "u3", SessionID: "s3"},
		RunID:    "r-multi",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: "t3", User: "u3", Session: "s3",
		Types: []events.EventType{planner.EventTypePlannerRepairExhausted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `[{"tool":"a","args":{}}, {"tool":"b","args":{}}, {"tool":"c","args":{}}]`},
		},
	}

	loop := repair.New(repair.Config{ArgFillEnabled: true})
	dec, runErr := loop.Run(ctx, integrationRC(bus, q), client, sampleRequest(), passValidator)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	par, ok := dec.Decision.(planner.CallParallel)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallParallel", dec)
	}
	if len(par.Branches) != 3 {
		t.Errorf("Branches = %d, want 3", len(par.Branches))
	}
	// No repair-exhausted event should fire on a success path.
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected event on multi-action success: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestE2E_RepairLoop_RejectingValidatorRepairsThenSucceeds is the
// integration test for the schema-repair path with a real bus: the
// validator rejects once, then accepts.
func TestE2E_RepairLoop_RejectingValidatorRepairsThenSucceeds(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t4", UserID: "u4", SessionID: "s4"},
		RunID:    "r-schema-repair",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: "t4", User: "u4", Session: "s4",
		Types: []events.EventType{planner.EventTypePlannerRepairExhausted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"search","args":{"q":"too vague"}}`},
			{Content: `{"tool":"search","args":{"q":"specific!"}}`},
		},
	}
	v := &rejectingValidator{failN: 1}

	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 5,
	})

	dec, runErr := loop.Run(ctx, integrationRC(bus, q), client, sampleRequest(), v.Validate)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if _, ok := dec.Decision.(planner.CallTool); !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if client.callCount() != 2 {
		t.Errorf("client.calls = %d, want 2", client.callCount())
	}

	// No graceful-failure event on a success path.
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected event on success-after-repair: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// drainOneEvent reads one event with a bounded wall-clock deadline.
// Returns the event or fatals out.
func drainOneEvent(t *testing.T, sub events.Subscription, deadline time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before event arrived")
		}
		return ev
	case <-time.After(deadline):
		t.Fatal("timeout waiting for event")
	}
	return events.Event{}
}
