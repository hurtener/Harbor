package react_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// staticSummariserIT is the integration-test fixture mirror of the
// in-package staticSummariser. The integration test lives in
// `react_test` (external test), so we re-declare a fixture rather
// than cross-import a `_test.go` file (Go's test packages don't allow
// that import direction).
type staticSummariserIT struct {
	summary *planner.TrajectorySummary
	calls   atomic.Int64
}

func (s *staticSummariserIT) Summarise(
	_ context.Context,
	_ planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	s.calls.Add(1)
	return s.summary, nil
}

// errSummariserIT — fail-mode fixture.
type errSummariserIT struct {
	err   error
	calls atomic.Int64
}

func (e *errSummariserIT) Summarise(
	_ context.Context,
	_ planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	e.calls.Add(1)
	return nil, e.err
}

// TestE2E_ReactCompression_OverBudgetTriggersCompaction_PlannerSeesCompactedView
// is the §13 primitive-with-consumer gate: Phase 46's CompressionRunner
// MUST be exercised by an existing planner (ReAct from Phase 45) within
// this PR.
//
// Flow:
//
//  1. Build an over-budget trajectory (heavy LLMContext + multiple
//     prior Steps).
//  2. Invoke CompressionRunner.MaybeCompress with a static summariser
//     fixture; assert `tr.Summary` is stamped + the bus observes
//     `trajectory.compressed` carrying the run's identity.
//  3. Invoke ReActPlanner.Next with a scripted `_finish` LLM
//     response.
//  4. Assert the planner returns `Finish{Goal}` AND the scripted LLM
//     was called exactly once (the compaction did not double-call).
//  5. Inspect the LLM request the planner sent: it must contain the
//     summary's text and ZERO assistant messages from the step
//     history (the Phase 46 D-055 swap).
func TestE2E_ReactCompression_OverBudgetTriggersCompaction_PlannerSeesCompactedView(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-c1", UserID: "u", SessionID: "s"},
		RunID:    "r-compress-happy",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Subscribe to trajectory + planner events so we can assert the
	// success-path event fires.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types: []events.EventType{
			planner.EventTypeTrajectoryCompressed,
			planner.EventTypeTrajectoryCompressionFailed,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Build the over-budget trajectory.
	tr := &planner.Trajectory{
		Query: "find and summarise",
		LLMContext: map[string]any{
			"bulk": strings.Repeat("x", 8192), // ~2050 tokens at chars/4
		},
		Steps: []planner.Step{
			{
				Action:         planner.CallTool{Tool: "search"},
				LLMObservation: "found 3 hits with details about topic A",
			},
			{
				Action:         planner.CallTool{Tool: "fetch"},
				LLMObservation: "fetched articles A1, A2, A3",
			},
		},
	}

	// Build the runner with a staticSummariser. Identity emit closure
	// wires onto the real bus so the integration test exercises the
	// production observability path end-to-end.
	emitClosure := func(ev events.Event) {
		ev.Identity = q
		_ = bus.Publish(context.Background(), ev)
	}
	rc := planner.RunContext{
		Quadruple:  q,
		Goal:       "the goal",
		Trajectory: tr,
		Budget:     planner.Budget{TokenBudget: 100}, // well below 2050
		Emit:       emitClosure,
	}

	summary := &planner.TrajectorySummary{
		Goals:            []string{"find topic A summary"},
		Facts:            []string{"three articles fetched", "topic A covered"},
		Pending:          []string{"present final answer"},
		LastOutputDigest: "3 articles fetched on topic A",
		Note:             "compacted at step 2 by Phase 46 runner",
	}
	summ := &staticSummariserIT{summary: summary}
	runner := planner.NewCompressionRunner(summ)

	if err = runner.MaybeCompress(ctx, rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary == nil {
		t.Fatalf("tr.Summary not stamped — compression did not run")
	}
	if summ.calls.Load() != 1 {
		t.Errorf("summariser calls = %d, want 1", summ.calls.Load())
	}

	// Drain the success event.
	ev := drainOneEvent(t, sub)
	if ev.Type != planner.EventTypeTrajectoryCompressed {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, planner.EventTypeTrajectoryCompressed)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	payload, ok := ev.Payload.(planner.TrajectoryCompressedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want TrajectoryCompressedPayload", ev.Payload)
	}
	if payload.StepsBefore != 2 {
		t.Errorf("payload.StepsBefore = %d, want 2", payload.StepsBefore)
	}

	// Now invoke the ReAct planner — it MUST see the compacted view.
	// A capturingClient records the request the planner sent so we
	// can assert the prompt contains the summary and zero assistant
	// turns from the step loop.
	cap := &capturingClient{
		response: llm.CompleteResponse{
			Content: `{"tool":"_finish","args":{"answer":"all done"},"reasoning":"compacted view sufficient"}`,
		},
	}
	p := react.New(cap)

	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if fin.Payload != "all done" {
		t.Errorf("Payload = %v, want all done", fin.Payload)
	}
	if cap.calls.Load() != 1 {
		t.Errorf("LLM client calls = %d, want 1 (the planner's call only — compaction must not double-call)", cap.calls.Load())
	}

	// Inspect the planner's last LLM request — it must reflect the
	// compacted view (summary text present; zero assistant turns).
	captured := cap.lastRequest()
	asstCount := 0
	for _, m := range captured.Messages {
		if m.Role == llm.RoleAssistant {
			asstCount++
		}
	}
	if asstCount != 0 {
		t.Errorf("Phase 46 contract: assistant-message count in planner prompt = %d, want 0 (Summary present → no step history)", asstCount)
	}
	if !messageBodyContains(captured.Messages, "compacted at step 2 by Phase 46 runner") {
		t.Errorf("planner prompt missing summary Note text")
	}
	if messageBodyContains(captured.Messages, "found 3 hits with details about topic A") {
		t.Errorf("planner prompt leaked raw step-history text despite stamped Summary")
	}
}

// TestE2E_ReactCompression_SummariserFailure_SurfacesOnBus is the
// failure-mode integration: the summariser errors; MaybeCompress
// surfaces the error wrapped; the bus observes
// `trajectory.compression_failed` carrying the run's identity. The
// planner is NOT called (the runner's failure path short-circuits
// before the planner step).
func TestE2E_ReactCompression_SummariserFailure_SurfacesOnBus(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-c2", UserID: "u", SessionID: "s"},
		RunID:    "r-compress-fail",
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
			planner.EventTypeTrajectoryCompressed,
			planner.EventTypeTrajectoryCompressionFailed,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	tr := &planner.Trajectory{
		LLMContext: map[string]any{"bulk": strings.Repeat("y", 8192)},
	}
	wantErr := errors.New("summariser LLM unreachable")
	summ := &errSummariserIT{err: wantErr}
	runner := planner.NewCompressionRunner(summ)

	emitClosure := func(ev events.Event) {
		ev.Identity = q
		_ = bus.Publish(context.Background(), ev)
	}
	rc := planner.RunContext{
		Quadruple:  q,
		Trajectory: tr,
		Budget:     planner.Budget{TokenBudget: 100},
		Emit:       emitClosure,
	}

	err = runner.MaybeCompress(ctx, rc, tr)
	if !errors.Is(err, wantErr) {
		t.Errorf("MaybeCompress err = %v, want wrapping wantErr", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped on failure path — want nil (no silent fall-through)")
	}

	ev := drainOneEvent(t, sub)
	if ev.Type != planner.EventTypeTrajectoryCompressionFailed {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, planner.EventTypeTrajectoryCompressionFailed)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	payload, ok := ev.Payload.(planner.TrajectoryCompressionFailedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want TrajectoryCompressionFailedPayload", ev.Payload)
	}
	if payload.ErrorCode != "summariser_error" {
		t.Errorf("payload.ErrorCode = %q, want summariser_error", payload.ErrorCode)
	}
	if !strings.Contains(payload.ErrorMessage, "summariser LLM unreachable") {
		t.Errorf("payload.ErrorMessage missing original error text: %q", payload.ErrorMessage)
	}
}

// TestE2E_ReactCompression_UnderBudget_NoCompaction_PlannerSeesRawHistory
// asserts the negative case: when the trajectory is under-budget, the
// runner does not compress; ReAct's prompt builder renders the raw
// per-step pairs as in Phase 45.
func TestE2E_ReactCompression_UnderBudget_NoCompaction_PlannerSeesRawHistory(t *testing.T) {
	bus := integrationBus(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-c3", UserID: "u", SessionID: "s"},
		RunID:    "r-compress-under",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	tr := &planner.Trajectory{
		Steps: []planner.Step{
			{
				Action:         planner.CallTool{Tool: "search"},
				LLMObservation: "hits",
			},
		},
	}

	summ := &staticSummariserIT{
		summary: &planner.TrajectorySummary{Note: "should-not-fire"},
	}
	runner := planner.NewCompressionRunner(summ)

	emitClosure := func(ev events.Event) {
		ev.Identity = q
		_ = bus.Publish(context.Background(), ev)
	}
	rc := planner.RunContext{
		Quadruple:  q,
		Goal:       "g",
		Trajectory: tr,
		Budget:     planner.Budget{TokenBudget: 1_000_000}, // huge
		Emit:       emitClosure,
	}

	if err = runner.MaybeCompress(ctx, rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped under-budget — want nil")
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked under-budget — want 0 calls")
	}

	// Now invoke the planner; it MUST render the raw step history.
	cap := &capturingClient{
		response: llm.CompleteResponse{
			Content: `{"tool":"_finish","args":{"answer":"ok"},"reasoning":"raw history sufficient"}`,
		},
	}
	p := react.New(cap)
	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := dec.(planner.Finish); !ok {
		t.Fatalf("decision = %T, want Finish", dec)
	}

	// Captured prompt MUST contain the raw step's tool reference.
	captured := cap.lastRequest()
	asstCount := 0
	for _, m := range captured.Messages {
		if m.Role == llm.RoleAssistant {
			asstCount++
		}
	}
	if asstCount != 1 {
		t.Errorf("under-budget path: assistant count = %d, want 1 (raw step rendered)", asstCount)
	}
}

// --- helpers --------------------------------------------------------

// capturingClient records the request it received so the integration
// test can assert prompt shape. Concurrent-safe via atomic.Int64 +
// mutex around the request snapshot.
type capturingClient struct {
	lastReq  llm.CompleteRequest
	response llm.CompleteResponse
	calls    atomic.Int64
	mu       sync.Mutex
	captured bool
}

func (c *capturingClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.lastReq = req
	c.captured = true
	c.mu.Unlock()
	return c.response, nil
}

func (c *capturingClient) Close(_ context.Context) error { return nil }

func (c *capturingClient) lastRequest() llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastReq
}

// messageBodyContains reports whether any message in msgs has a Text
// content that contains s.
func messageBodyContains(msgs []llm.ChatMessage, s string) bool {
	for _, m := range msgs {
		if m.Content.Text == nil {
			continue
		}
		if strings.Contains(*m.Content.Text, s) {
			return true
		}
	}
	return false
}
