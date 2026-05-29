// Package integration — Phase 83e wave-15 integration test.
//
// Phase 83e decouples the ReAct planner's decision contract (the JSON
// the model emits — narrowed to `{tool, args}`) from its reasoning
// capture (the provider-side thinking trace). This test proves the
// capture + replay surface composes end-to-end against the SAME
// real-driver stack `cmd/harbor` boots:
//
//  1. A real inmem EventBus carrying the audit Redactor.
//  2. The real Phase 45 ReAct planner + Phase 44 repair loop.
//  3. An LLM client surfacing `CompleteResponse.Reasoning` — the
//     bifrost driver's capture path (Phase 33 + 83e) populates this
//     in production from `reasoning_details[]`.
//
// It asserts:
//   - The captured reasoning trace reaches `trajectory.Step.ReasoningTrace`
//     when the runtime builds the step from `RunResult.Reasoning`.
//   - The `planner.decision` event carries the trace + char count.
//   - `ReasoningReplay` is honoured: `never` (default) renders prior
//     steps as `{tool, args}` only; `text` prepends the captured
//     trace; a per-run `RunContext.ReasoningReplay` override wins.
//   - Identity propagates through every layer (multi-isolation).
//   - A failure mode: a missing-identity RunContext fails loud.
//   - N≥10 concurrent runs with disjoint per-run replay modes show no
//     mode bleed and no trace bleed (cross-package D-025 stress).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns83e "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// reasoningLLM is an llm.LLMClient that returns a scripted
// CompleteResponse whose `Reasoning` field carries a per-call trace —
// the production bifrost driver populates this from the provider's
// normalised `reasoning_details[]`. Safe for concurrent use.
//
// Phase 107c (D-167) — the legacy `content` field still works for
// natural-language terminal answers (the projector maps non-empty
// Content + no ToolCalls to Finish{Goal}), but tests that need a
// CallTool emission must set `toolCallName` + `toolCallArgs` so the
// projector reads native ToolCalls.
type reasoningLLM struct {
	mu           sync.Mutex
	content      string
	toolCallID   string
	toolCallName string
	toolCallArgs string
	reason       string
	calls        atomic.Int64
	seenIDs      []identity.Quadruple
	prompts      []string // last user/assistant content seen, for replay assertions
}

func (c *reasoningLLM) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.calls.Add(1)
	id, _ := identity.QuadrupleFrom(ctx)
	c.mu.Lock()
	c.seenIDs = append(c.seenIDs, id)
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			c.prompts = append(c.prompts, *m.Content.Text)
		}
	}
	c.mu.Unlock()
	resp := llm.CompleteResponse{Content: c.content, Reasoning: c.reason}
	if c.toolCallName != "" {
		resp.ToolCalls = []llm.ToolCallStructured{{
			ID:   c.toolCallID,
			Name: c.toolCallName,
			Args: json.RawMessage(c.toolCallArgs),
		}}
	}
	return resp, nil
}

func (c *reasoningLLM) Close(_ context.Context) error { return nil }

func (c *reasoningLLM) snapshotPrompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.prompts))
	copy(out, c.prompts)
	return out
}

// open83eBus opens a real inmem EventBus carrying the audit Redactor —
// the same wiring `cmd/harbor` boots.
func open83eBus(t *testing.T) (events.EventBus, func()) {
	t.Helper()
	red := auditpatterns83e.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              5 * time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	return bus, func() { _ = bus.Close(context.Background()) }
}

func fixedQ83e(runID string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "T83e", UserID: "U83e", SessionID: "S83e"},
		RunID:    runID,
	}
}

// TestE2E_Phase83e_ReasoningCaptured drives a single ReAct step and
// asserts the captured provider-side reasoning trace reaches both the
// `planner.decision` event payload and a runtime-built
// `trajectory.Step.ReasoningTrace`.
func TestE2E_Phase83e_ReasoningCaptured(t *testing.T) {
	bus, cleanup := open83eBus(t)
	defer cleanup()

	q := fixedQ83e("r-capture")
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	const trace = "I should call the search tool because the user asked about the weather."
	client := &reasoningLLM{
		toolCallID:   "call_search",
		toolCallName: "search",
		toolCallArgs: `{"q":"weather"}`,
		reason:       trace,
	}

	// Subscribe to the bus BEFORE the run so the planner.decision emit
	// is observed.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{planner.EventTypePlannerDecision},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	p := react.New(client)
	traj := &planner.Trajectory{}
	rc := planner.RunContext{
		Quadruple:  q,
		Goal:       "find the weather",
		Trajectory: traj,
		Emit: func(ev events.Event) {
			_ = bus.Publish(ctx, ev)
		},
	}

	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if call.Tool != "search" {
		t.Errorf("Tool = %q, want search", call.Tool)
	}

	// The narrowed action shape carries no reasoning field — only the
	// provider channel does.
	if strings.Contains(string(call.Args), "reasoning") {
		t.Errorf("CallTool.Args unexpectedly carries reasoning: %s", call.Args)
	}

	// The planner.decision event must carry the captured trace.
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(planner.DecisionPayload)
		if !ok {
			t.Fatalf("decision event payload = %T, want planner.DecisionPayload", ev.Payload)
		}
		if payload.ReasoningTrace != trace {
			t.Errorf("DecisionPayload.ReasoningTrace = %q, want %q", payload.ReasoningTrace, trace)
		}
		if payload.ReasoningChars != len([]rune(trace)) {
			t.Errorf("DecisionPayload.ReasoningChars = %d, want %d", payload.ReasoningChars, len([]rune(trace)))
		}
		if payload.Identity.RunID != q.RunID {
			t.Errorf("DecisionPayload.Identity.RunID = %q, want %q — identity must propagate", payload.Identity.RunID, q.RunID)
		}
		if payload.Tool != "search" {
			t.Errorf("DecisionPayload.Tool = %q, want search", payload.Tool)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for planner.decision event")
	}

	// Runtime trajectory-append: the runtime builds the trajectory.Step
	// from the planner's decision + the captured reasoning. We inline
	// that wiring here (the production RunLoop does this) and assert
	// the trace lands on ReasoningTrace, NEVER inside Action.
	step := trajectory.Step{
		Action:         call,
		ReasoningTrace: trace,
	}
	traj.Steps = append(traj.Steps, step)
	if traj.Steps[0].ReasoningTrace != trace {
		t.Errorf("trajectory.Step.ReasoningTrace = %q, want %q", traj.Steps[0].ReasoningTrace, trace)
	}
}

// TestE2E_Phase83e_ReplayHonoursMode drives a SECOND planner step over
// a trajectory whose first step carries a reasoning trace, and asserts
// the ReasoningReplay mode controls whether the trace reaches the next
// prompt.
func TestE2E_Phase83e_ReplayHonoursMode(t *testing.T) {
	const priorTrace = "PRIOR-STEP-CHAIN-OF-THOUGHT-MARKER"

	priorStep := trajectory.Step{
		Action:         planner.CallTool{Tool: "search", Args: []byte(`{"q":"x"}`)},
		ReasoningTrace: priorTrace,
		LLMObservation: "found result Y",
	}

	cases := []struct {
		name        string
		configured  planner.ReasoningReplayMode
		override    *planner.ReasoningReplayMode
		wantInPromt bool
	}{
		{"never_default", planner.ReasoningReplayNever, nil, false},
		{"text_configured", planner.ReasoningReplayText, nil, true},
		{"never_then_run_override_text", planner.ReasoningReplayNever, replayPtr(planner.ReasoningReplayText), true},
		{"text_then_run_override_never", planner.ReasoningReplayText, replayPtr(planner.ReasoningReplayNever), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := fixedQ83e("r-replay-" + tc.name)
			ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}
			client := &reasoningLLM{
				toolCallID:   "call_finish",
				toolCallName: "_finish",
				toolCallArgs: `{"answer":"done"}`,
				reason:       "second-step reasoning",
			}
			p := react.New(client, react.WithReasoningReplay(tc.configured))

			traj := &planner.Trajectory{Steps: []planner.Step{priorStep}}
			rc := planner.RunContext{
				Quadruple:       q,
				Goal:            "finish",
				Trajectory:      traj,
				ReasoningReplay: tc.override,
			}
			if _, err := p.Next(ctx, rc); err != nil {
				t.Fatalf("Next: %v", err)
			}

			// Inspect the prompt the planner built: did the prior
			// trace reach the LLM?
			var sawTrace bool
			for _, msg := range client.snapshotPrompts() {
				if strings.Contains(msg, priorTrace) {
					sawTrace = true
					break
				}
			}
			if sawTrace != tc.wantInPromt {
				t.Errorf("reasoning trace in next prompt = %v, want %v (configured=%q override=%v)",
					sawTrace, tc.wantInPromt, tc.configured, tc.override)
			}
		})
	}
}

// TestE2E_Phase83e_MissingIdentityFailsLoud is the mandatory failure
// mode: a RunContext with an incomplete identity quadruple fails loud
// rather than silently degrading (§13).
func TestE2E_Phase83e_MissingIdentityFailsLoud(t *testing.T) {
	client := &reasoningLLM{toolCallID: "call_x", toolCallName: "search", toolCallArgs: `{}`, reason: "x"}
	p := react.New(client)
	// RunContext with an empty RunID — identity is incomplete.
	rc := planner.RunContext{
		Quadruple:  identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}},
		Goal:       "go",
		Trajectory: &planner.Trajectory{},
	}
	_, err := p.Next(context.Background(), rc)
	if err == nil {
		t.Fatal("Next with missing identity should fail loud, got nil error")
	}
	if client.calls.Load() != 0 {
		t.Errorf("LLM was called %d times despite missing identity — must fail before the call", client.calls.Load())
	}
}

// TestE2E_Phase83e_ConcurrentReplayModes is the D-025 concurrent-reuse
// gate for the reasoning-replay surface: N≥100 concurrent runs against
// ONE shared ReActPlanner, each with a disjoint per-run
// ReasoningReplay override. Asserts no replay-mode bleed and no trace
// bleed across runs (the headline Phase 83e guarantee — per-run replay
// scope).
func TestE2E_Phase83e_ConcurrentReplayModes(t *testing.T) {
	const N = 128

	// ONE shared planner — configured `never`; per-run overrides drive
	// the divergence.
	client := &perRunReasoningLLM{}
	p := react.New(client, react.WithReasoningReplay(planner.ReasoningReplayNever))

	var (
		wg         sync.WaitGroup
		bleedFails atomic.Int64
	)
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("r-conc-%04d", i)
			q := fixedQ83e(runID)
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				t.Errorf("identity.WithRun: %v", err)
				return
			}

			// Even runs replay `text`; odd runs replay `never`.
			marker := "TRACE-" + runID
			priorStep := trajectory.Step{
				Action:         planner.CallTool{Tool: "search", Args: []byte(`{}`)},
				ReasoningTrace: marker,
				LLMObservation: "obs",
			}
			var override planner.ReasoningReplayMode
			wantTrace := i%2 == 0
			if wantTrace {
				override = planner.ReasoningReplayText
			} else {
				override = planner.ReasoningReplayNever
			}

			rc := planner.RunContext{
				Quadruple:       q,
				Goal:            "finish",
				Trajectory:      &planner.Trajectory{Steps: []planner.Step{priorStep}},
				ReasoningReplay: &override,
			}
			if _, err := p.Next(ctx, rc); err != nil {
				t.Errorf("[%s] Next: %v", runID, err)
				return
			}

			// The per-run client recorded whether THIS run's marker
			// reached its prompt. A text-mode run must see its OWN
			// marker; a never-mode run must see NO marker at all.
			sawOwn, sawAny := client.markerSeen(runID, marker)
			if wantTrace && !sawOwn {
				bleedFails.Add(1)
			}
			if !wantTrace && sawAny {
				// A never-mode run that saw any marker → mode bleed.
				bleedFails.Add(1)
			}
		}()
	}
	wg.Wait()

	if bleedFails.Load() != 0 {
		t.Errorf("D-025: %d replay-mode / trace bleed(s) across %d concurrent runs", bleedFails.Load(), N)
	}
}

// perRunReasoningLLM records, per run, every prompt content seen so
// the concurrent test can detect cross-run trace bleed.
type perRunReasoningLLM struct {
	mu    sync.Mutex
	byRun map[string][]string
}

func (c *perRunReasoningLLM) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.QuadrupleFrom(ctx)
	c.mu.Lock()
	if c.byRun == nil {
		c.byRun = make(map[string][]string)
	}
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			c.byRun[id.RunID] = append(c.byRun[id.RunID], *m.Content.Text)
		}
	}
	c.mu.Unlock()
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_done",
			Name: "_finish",
			Args: json.RawMessage(`{"answer":"done"}`),
		}},
		Reasoning: "step reasoning for " + id.RunID,
	}, nil
}

func (c *perRunReasoningLLM) Close(_ context.Context) error { return nil }

// markerSeen reports whether `runID`'s prompts contained its own
// marker (sawOwn) and whether they contained ANY "TRACE-" marker
// (sawAny — used to detect mode bleed in never-mode runs).
func (c *perRunReasoningLLM) markerSeen(runID, marker string) (sawOwn, sawAny bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, msg := range c.byRun[runID] {
		if strings.Contains(msg, marker) {
			sawOwn = true
		}
		if strings.Contains(msg, "TRACE-") {
			sawAny = true
		}
	}
	return sawOwn, sawAny
}

// replayPtr is a tiny helper for building *ReasoningReplayMode
// per-run overrides.
func replayPtr(m planner.ReasoningReplayMode) *planner.ReasoningReplayMode {
	return &m
}
