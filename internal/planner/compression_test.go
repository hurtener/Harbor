package planner_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// --- Test fixtures --------------------------------------------------

// staticSummariser returns a canned summary on every invocation.
// Thread-safe: the receiver is read-only; the call counter is
// atomic.
type staticSummariser struct {
	summary *planner.TrajectorySummary
	calls   atomic.Int64
}

func (s *staticSummariser) Summarise(
	_ context.Context,
	_ planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	s.calls.Add(1)
	return s.summary, nil
}

// errSummariser returns a programmable error on every invocation.
// Used for the fail-loud test.
type errSummariser struct {
	err   error
	calls atomic.Int64
}

func (e *errSummariser) Summarise(
	_ context.Context,
	_ planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	e.calls.Add(1)
	return nil, e.err
}

// emptySummariser returns (nil, nil) — the contract violation the
// runner detects with ErrEmptySummary.
type emptySummariser struct {
	calls atomic.Int64
}

func (e *emptySummariser) Summarise(
	_ context.Context,
	_ planner.RunContext,
	_ *planner.Trajectory,
) (*planner.TrajectorySummary, error) {
	e.calls.Add(1)
	return nil, nil
}

// recordingEmit collects emitted events. Mutex-guarded.
type recordingEmit struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingEmit) emit(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingEmit) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

// fixedQuadruple returns a populated identity quadruple for tests.
func fixedQuadruple(runID string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    runID,
	}
}

// rcWith builds a RunContext with the supplied bits.
func rcWith(q identity.Quadruple, tokenBudget int, emit func(events.Event)) planner.RunContext {
	return planner.RunContext{
		Quadruple: q,
		Budget:    planner.Budget{TokenBudget: tokenBudget},
		Emit:      emit,
		// Fixed clock so emitted payloads have a stable timestamp.
		Clock: func() time.Time { return time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC) },
	}
}

// smallTrajectory returns a near-empty trajectory whose token estimate
// is small (< 50 by chars/4).
func smallTrajectory() *planner.Trajectory {
	return &planner.Trajectory{
		Query: "hi",
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"k": "v"},
		},
	}
}

// bigTrajectory returns a trajectory whose token estimate exceeds the
// supplied budget (the LLMContext carries a deliberately oversized
// string to inflate the chars/4 count).
func bigTrajectory(approxChars int) *planner.Trajectory {
	return &planner.Trajectory{
		Query: "big",
		LLMContext: map[string]any{
			"bulk": strings.Repeat("x", approxChars),
		},
		Steps: []planner.Step{
			{Action: planner.CallTool{Tool: "alpha"}, LLMObservation: "obs"},
			{Action: planner.CallTool{Tool: "beta"}, LLMObservation: "obs"},
		},
	}
}

// cannedSummary is a deterministic summary value used by
// staticSummariser.
func cannedSummary() *planner.TrajectorySummary {
	return &planner.TrajectorySummary{
		Goals:            []string{"reach the goal"},
		Facts:            []string{"the sky is blue"},
		Pending:          []string{"verify with user"},
		LastOutputDigest: "last output: 3 hits",
		Note:             "compacted by test",
	}
}

// --- Tests ----------------------------------------------------------

// TestMaybeCompress_UnderBudget_NoSummariser asserts the happy
// short-circuit when the estimate is below the budget.
func TestMaybeCompress_UnderBudget_NoSummariser(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("r1"), 1_000_000, rec.emit) // budget high enough
	tr := smallTrajectory()

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped under-budget — want nil")
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked %d times under-budget — want 0", summ.calls.Load())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("emitted %d events under-budget — want 0", len(got))
	}
}

// TestMaybeCompress_OverBudget_StampsSummary asserts the
// over-threshold happy path: summariser invoked once, summary
// stamped, success-path event emitted.
func TestMaybeCompress_OverBudget_StampsSummary(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	// Budget = 10 tokens; trajectory has ~5000 chars → ~1250 tokens.
	rc := rcWith(fixedQuadruple("r2"), 10, rec.emit)
	tr := bigTrajectory(5000)

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary == nil {
		t.Fatalf("tr.Summary not stamped over-budget")
	}
	if tr.Summary.Note != "compacted by test" {
		t.Errorf("Summary.Note = %q, want canned summary", tr.Summary.Note)
	}
	if summ.calls.Load() != 1 {
		t.Errorf("summariser invoked %d times, want 1", summ.calls.Load())
	}

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1 (trajectory.compressed)", len(evs))
	}
	ev := evs[0]
	if ev.Type != planner.EventTypeTrajectoryCompressed {
		t.Errorf("ev.Type = %q, want %q", ev.Type, planner.EventTypeTrajectoryCompressed)
	}
	payload, ok := ev.Payload.(planner.TrajectoryCompressedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want TrajectoryCompressedPayload", ev.Payload)
	}
	if payload.Identity.RunID != "r2" {
		t.Errorf("payload.Identity.RunID = %q, want r2", payload.Identity.RunID)
	}
	if payload.StepsBefore != len(tr.Steps) {
		t.Errorf("payload.StepsBefore = %d, want %d", payload.StepsBefore, len(tr.Steps))
	}
	if payload.StepsAfter != payload.StepsBefore {
		t.Errorf("payload.StepsAfter = %d, want StepsBefore=%d (Phase 46 does not truncate Steps)",
			payload.StepsAfter, payload.StepsBefore)
	}
	if payload.TokenEstimate <= 10 {
		t.Errorf("payload.TokenEstimate = %d, want > 10 (the budget)", payload.TokenEstimate)
	}
}

// TestMaybeCompress_AlreadyCompressed_Idempotent asserts the
// short-circuit when tr.Summary is already non-nil.
func TestMaybeCompress_AlreadyCompressed_Idempotent(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("r3"), 10, rec.emit)
	tr := bigTrajectory(5000)
	tr.Summary = &planner.TrajectorySummary{Note: "pre-stamped"}

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary.Note != "pre-stamped" {
		t.Errorf("pre-stamped Summary clobbered: Note = %q", tr.Summary.Note)
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked %d times on idempotent path — want 0", summ.calls.Load())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("emitted %d events on idempotent path — want 0", len(got))
	}
}

// TestMaybeCompress_RejectsMissingIdentity asserts the §6 rule 9 +
// D-001 identity-mandatory guard.
func TestMaybeCompress_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	runner := planner.NewCompressionRunner(summ)

	cases := []struct {
		name string
		q    identity.Quadruple
	}{
		{"missing tenant", identity.Quadruple{Identity: identity.Identity{UserID: "u", SessionID: "s"}, RunID: "r"}},
		{"missing user", identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}, RunID: "r"}},
		{"missing session", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}, RunID: "r"}},
		{"missing run", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}},
		{"all missing", identity.Quadruple{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := planner.RunContext{Quadruple: c.q, Budget: planner.Budget{TokenBudget: 10}}
			err := runner.MaybeCompress(context.Background(), rc, smallTrajectory())
			if !errors.Is(err, llm.ErrIdentityMissing) {
				t.Errorf("err = %v, want wrapping llm.ErrIdentityMissing", err)
			}
		})
	}
}

// TestMaybeCompress_HonoursCtxCancel asserts ctx.Err() is checked at
// entry, before the estimator runs.
func TestMaybeCompress_HonoursCtxCancel(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rc"), 10, nil)
	tr := bigTrajectory(5000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runner.MaybeCompress(ctx, rc, tr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked %d times with pre-cancelled ctx — want 0", summ.calls.Load())
	}
}

// TestMaybeCompress_FailLoudOnSummariserError asserts the §13
// fail-loudly contract: a non-nil error from the summariser
// propagates wrapped AND the trajectory.compression_failed event
// fires before return.
func TestMaybeCompress_FailLoudOnSummariserError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("downstream LLM 500")
	summ := &errSummariser{err: wantErr}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rfail"), 10, rec.emit)
	tr := bigTrajectory(5000)

	err := runner.MaybeCompress(context.Background(), rc, tr)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping wantErr", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped on summariser error — want nil (no silent fall-through)")
	}

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1 (trajectory.compression_failed)", len(evs))
	}
	ev := evs[0]
	if ev.Type != planner.EventTypeTrajectoryCompressionFailed {
		t.Errorf("ev.Type = %q, want %q", ev.Type, planner.EventTypeTrajectoryCompressionFailed)
	}
	payload, ok := ev.Payload.(planner.TrajectoryCompressionFailedPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want TrajectoryCompressionFailedPayload", ev.Payload)
	}
	if payload.ErrorCode != "summariser_error" {
		t.Errorf("payload.ErrorCode = %q, want summariser_error", payload.ErrorCode)
	}
	if !strings.Contains(payload.ErrorMessage, "downstream LLM 500") {
		t.Errorf("payload.ErrorMessage = %q, want substring downstream LLM 500", payload.ErrorMessage)
	}
}

// TestMaybeCompress_FailLoudOnEmptySummary asserts the (nil, nil)
// contract-violation path.
func TestMaybeCompress_FailLoudOnEmptySummary(t *testing.T) {
	t.Parallel()
	summ := &emptySummariser{}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rempty"), 10, rec.emit)
	tr := bigTrajectory(5000)

	err := runner.MaybeCompress(context.Background(), rc, tr)
	if !errors.Is(err, planner.ErrEmptySummary) {
		t.Errorf("err = %v, want wrapping ErrEmptySummary", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped on empty-summary error — want nil")
	}

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1 (trajectory.compression_failed)", len(evs))
	}
	payload := evs[0].Payload.(planner.TrajectoryCompressionFailedPayload)
	if payload.ErrorCode != "empty_summary" {
		t.Errorf("payload.ErrorCode = %q, want empty_summary", payload.ErrorCode)
	}
}

// TestMaybeCompress_ZeroBudget_NoOp asserts the "no budget set"
// short-circuit.
func TestMaybeCompress_ZeroBudget_NoOp(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rz"), 0, rec.emit) // TokenBudget = 0
	tr := bigTrajectory(50_000)                     // huge

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary != nil {
		t.Errorf("tr.Summary stamped with TokenBudget=0 — want nil")
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked %d times with TokenBudget=0", summ.calls.Load())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("emitted %d events with TokenBudget=0 — want 0", len(got))
	}
}

// TestMaybeCompress_NilTrajectory_FailLoud asserts the nil-tr guard.
func TestMaybeCompress_NilTrajectory_FailLoud(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rnil"), 10, nil)
	err := runner.MaybeCompress(context.Background(), rc, nil)
	if !errors.Is(err, planner.ErrNilTrajectory) {
		t.Errorf("err = %v, want wrapping ErrNilTrajectory", err)
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked on nil trajectory")
	}
}

// TestNewCompressionRunner_PanicsOnNilSummariser asserts composition-
// error catch at construction.
func TestNewCompressionRunner_PanicsOnNilSummariser(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewCompressionRunner(nil) did not panic")
		}
	}()
	_ = planner.NewCompressionRunner(nil)
}

// TestNewCompressionRunner_AppliesDefaults asserts the zero-options
// constructor wires DefaultTokenEstimator. The estimator is consulted
// on the over-budget path; a zero-options runner must produce the
// success-path emit when summariser produces a value.
func TestNewCompressionRunner_AppliesDefaults(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ) // no options

	rc := rcWith(fixedQuadruple("rdef"), 10, rec.emit)
	tr := bigTrajectory(5000)

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary == nil {
		t.Fatalf("Summary not stamped (default estimator may have under-counted)")
	}
}

// TestWithTokenEstimator_Overrides asserts the functional option
// replaces the default estimator. We supply an estimator that always
// returns 1 (well below any budget); the runner must short-circuit.
func TestWithTokenEstimator_Overrides(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	always1 := func(_ *planner.Trajectory) (int, error) { return 1, nil }
	runner := planner.NewCompressionRunner(summ, planner.WithTokenEstimator(always1))

	rc := rcWith(fixedQuadruple("rest"), 10, rec.emit)
	tr := bigTrajectory(50_000) // would normally exceed

	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress: %v", err)
	}
	if tr.Summary != nil {
		t.Errorf("Summary stamped despite always-1 estimator (under-budget path broken)")
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked despite always-1 estimator")
	}
}

// TestDefaultTokenEstimator_PropagatesSerializeError asserts the
// estimator surfaces a Phase 43 ErrUnserializable from Serialize.
// (The walker rejects non-JSON-encodable leaves; the estimator must
// propagate, not swallow.)
func TestDefaultTokenEstimator_PropagatesSerializeError(t *testing.T) {
	t.Parallel()
	tr := &planner.Trajectory{
		LLMContext: map[string]any{
			"callback": func() {}, // non-JSON-encodable
		},
	}
	_, err := planner.DefaultTokenEstimator(tr)
	if err == nil {
		t.Fatalf("DefaultTokenEstimator returned nil error on unserialisable trajectory")
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Errorf("err = %v, want wrapping trajectory.ErrUnserializable", err)
	}
}

// TestDefaultTokenEstimator_RejectsNilTrajectory asserts the
// estimator surfaces ErrNilTrajectory on nil input — fail-closed.
func TestDefaultTokenEstimator_RejectsNilTrajectory(t *testing.T) {
	t.Parallel()
	_, err := planner.DefaultTokenEstimator(nil)
	if !errors.Is(err, planner.ErrNilTrajectory) {
		t.Errorf("err = %v, want ErrNilTrajectory", err)
	}
}

// TestMaybeCompress_EstimatorError_EmitsFailedEvent asserts the
// fail-loudly path when the estimator (not the summariser) errors.
// The event code must be "estimator_error".
func TestMaybeCompress_EstimatorError_EmitsFailedEvent(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	rec := &recordingEmit{}
	failingEst := func(_ *planner.Trajectory) (int, error) {
		return 0, errors.New("simulated estimator failure")
	}
	runner := planner.NewCompressionRunner(summ, planner.WithTokenEstimator(failingEst))

	rc := rcWith(fixedQuadruple("rest2"), 10, rec.emit)
	tr := smallTrajectory()

	err := runner.MaybeCompress(context.Background(), rc, tr)
	if err == nil {
		t.Fatalf("MaybeCompress returned nil on estimator error")
	}
	if summ.calls.Load() != 0 {
		t.Errorf("summariser invoked despite estimator failure")
	}

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1 (trajectory.compression_failed)", len(evs))
	}
	payload := evs[0].Payload.(planner.TrajectoryCompressionFailedPayload)
	if payload.ErrorCode != "estimator_error" {
		t.Errorf("payload.ErrorCode = %q, want estimator_error", payload.ErrorCode)
	}
}

// TestMaybeCompress_NilEmit_DoesNotPanic asserts the runner handles a
// nil Emit closure (tests that omit the bus wiring).
func TestMaybeCompress_NilEmit_DoesNotPanic(t *testing.T) {
	t.Parallel()
	summ := &staticSummariser{summary: cannedSummary()}
	runner := planner.NewCompressionRunner(summ)

	rc := planner.RunContext{
		Quadruple: fixedQuadruple("rnoemt"),
		Budget:    planner.Budget{TokenBudget: 10},
		// Emit deliberately nil.
	}
	tr := bigTrajectory(5000)
	if err := runner.MaybeCompress(context.Background(), rc, tr); err != nil {
		t.Fatalf("MaybeCompress with nil Emit: %v", err)
	}
	if tr.Summary == nil {
		t.Errorf("Summary not stamped with nil Emit")
	}
}

// TestTruncateErrorMessage_Boundary asserts the truncation helper
// short-circuits on short messages and adds ellipsis on long. Tested
// through the runner's failure-emit path — the message-cap is
// internal but the visible effect is the payload.ErrorMessage shape.
func TestTruncateErrorMessage_Boundary(t *testing.T) {
	t.Parallel()
	// Use a very long error to exercise the truncation.
	longMsg := strings.Repeat("a", 1024)
	summ := &errSummariser{err: errors.New(longMsg)}
	rec := &recordingEmit{}
	runner := planner.NewCompressionRunner(summ)

	rc := rcWith(fixedQuadruple("rtrunc"), 10, rec.emit)
	tr := bigTrajectory(5000)
	_ = runner.MaybeCompress(context.Background(), rc, tr)

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1", len(evs))
	}
	payload := evs[0].Payload.(planner.TrajectoryCompressionFailedPayload)
	if len(payload.ErrorMessage) > 256 {
		t.Errorf("payload.ErrorMessage length = %d, want ≤ 256", len(payload.ErrorMessage))
	}
	if !strings.HasSuffix(payload.ErrorMessage, "...") {
		t.Errorf("payload.ErrorMessage does not end with ellipsis after truncation")
	}
}
