// runloop_compression_test.go — Phase 111e (D-202): the RunLoop's
// trajectory-compression gate. Covers the golden no-op contract
// (nil runner / zero budget = byte-identical to the pre-111e loop),
// the fires-over-budget path (summary stamped before the next
// planner step; trajectory.compressed emitted; single compression per
// run), and the fail-loud summariser-error path.
package steering

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
)

// recordingSummariser is a unit-test planner.Summariser that counts
// invocations and returns a canned summary (or a scripted error). Unit
// scope only — the real-driver wiring is exercised in
// test/integration/phase111e_compression_test.go.
type recordingSummariser struct {
	mu      sync.Mutex
	calls   int
	summary *planner.TrajectorySummary
	err     error
}

func (s *recordingSummariser) Summarise(_ context.Context, _ planner.RunContext, _ *planner.Trajectory) (*planner.TrajectorySummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.summary, nil
}

func (s *recordingSummariser) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// emitRecorder captures planner-side events the compression runner
// emits via RunContext.Emit.
type emitRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *emitRecorder) emit(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *emitRecorder) typesSeen() []events.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.EventType, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Type)
	}
	return out
}

// overBudgetEstimator always reports a huge estimate so any positive
// budget triggers compression.
func overBudgetEstimator(_ *planner.Trajectory) (int, error) { return 1_000_000, nil }

func compressionTestSummary() *planner.TrajectorySummary {
	return &planner.TrajectorySummary{
		Goals: []string{"g"},
		Facts: []string{"f"},
		Note:  "unit-test summary",
	}
}

// compressionSpec builds a RunSpec whose planner takes `steps`
// CallTool steps then finishes, with a live trajectory + emit
// recorder wired the way the production drivers wire them.
func compressionSpec(steps int, rec *emitRecorder) (RunSpec, *scriptedPlanner, *planner.Trajectory) {
	script := make([]scriptStep, 0, steps)
	for range steps {
		script = append(script, scriptStep{dec: planner.CallTool{Tool: "noop"}})
	}
	p := &scriptedPlanner{script: script, defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	tr := &planner.Trajectory{Query: "compress me"}
	spec := RunSpec{
		Planner: p,
		Base: planner.RunContext{
			Quadruple:  runA,
			Goal:       "test goal",
			Trajectory: tr,
			Emit:       rec.emit,
		},
		MaxSteps: 16,
	}
	return spec, p, tr
}

func TestRunLoop_Compression_NoOpWhenRunnerNil(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	rec := &emitRecorder{}
	spec, _, tr := compressionSpec(2, rec)
	// Budget set but NO runner — the pre-111e shape stays untouched.
	spec.Base.Budget = planner.Budget{TokenBudget: 1}

	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %s, want goal_satisfied", fin.Reason)
	}
	if tr.Summary != nil {
		t.Error("Summary stamped with a nil Compression runner — the gate leaked")
	}
	if tr.UnseenFrom != nil {
		t.Error("disabled compression changed persisted fresh-result state")
	}
	if got := rec.typesSeen(); len(got) != 0 {
		t.Errorf("events emitted on the no-runner path: %v, want none", got)
	}
}

func TestRunLoop_Compression_NoOpWhenBudgetZero(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	rec := &emitRecorder{}
	spec, _, tr := compressionSpec(2, rec)
	summ := &recordingSummariser{summary: compressionTestSummary()}
	spec.Compression = planner.NewCompressionRunner(summ,
		planner.WithTokenEstimator(overBudgetEstimator))
	// Zero budget: the runner must never be invoked — not even the
	// estimator (the gate short-circuits before MaybeCompress).

	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %s, want goal_satisfied", fin.Reason)
	}
	if summ.callCount() != 0 {
		t.Errorf("summariser invoked %d times with TokenBudget=0, want 0", summ.callCount())
	}
	if tr.Summary != nil {
		t.Error("Summary stamped with TokenBudget=0")
	}
	if tr.UnseenFrom != nil {
		t.Error("zero compression budget changed persisted fresh-result state")
	}
	if got := rec.typesSeen(); len(got) != 0 {
		t.Errorf("events emitted on the zero-budget path: %v, want none", got)
	}
}

func TestRunLoop_Compression_FiresOverBudget_Repeatedly(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	rec := &emitRecorder{}
	spec, p, tr := compressionSpec(3, rec)
	summ := &recordingSummariser{summary: compressionTestSummary()}
	spec.Compression = planner.NewCompressionRunner(summ,
		planner.WithTokenEstimator(overBudgetEstimator))
	spec.Base.Budget = planner.Budget{TokenBudget: 10}

	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %s, want goal_satisfied", fin.Reason)
	}
	if tr.Summary == nil {
		t.Fatal("Summary not stamped — compression did not fire")
	}
	if tr.Summary.Note != "unit-test summary" {
		t.Errorf("Summary.Note = %q, want the summariser's", tr.Summary.Note)
	}
	// With three exchanges, the first two eligible prefixes compact once each.
	if summ.callCount() != 2 {
		t.Errorf("summariser invoked %d times, want exactly 2", summ.callCount())
	}
	// The planner re-entered after compression: 3 CallTool steps + the
	// terminal Finish = 4 Next calls.
	if p.stepCount() != 4 {
		t.Errorf("planner saw %d steps, want 4", p.stepCount())
	}
	// trajectory.compressed emitted with the run's identity.
	var compressed int
	for _, ev := range rec.events {
		if ev.Type == planner.EventTypeTrajectoryCompressed {
			compressed++
			if ev.Identity != runA {
				t.Errorf("trajectory.compressed identity = %+v, want the run quadruple", ev.Identity)
			}
		}
	}
	if compressed != 2 {
		t.Errorf("trajectory.compressed emitted %d times, want 2", compressed)
	}
}

func TestRunLoop_Compression_SummariserError_FailsRunLoud(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	rec := &emitRecorder{}
	spec, _, tr := compressionSpec(2, rec)
	sentinel := errors.New("compaction model unavailable")
	summ := &recordingSummariser{err: sentinel}
	spec.Compression = planner.NewCompressionRunner(summ,
		planner.WithTokenEstimator(overBudgetEstimator))
	spec.Base.Budget = planner.Budget{TokenBudget: 10}

	_, err := rl.Run(context.Background(), spec)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run err = %v, want the wrapped summariser error (fail-loud, no silent fall-through)", err)
	}
	if tr.Summary != nil {
		t.Error("Summary stamped despite the summariser error")
	}
	// trajectory.compression_failed emitted before the error returned.
	var failed bool
	for _, ev := range rec.events {
		if ev.Type == planner.EventTypeTrajectoryCompressionFailed {
			failed = true
			if ev.Identity != runA {
				t.Errorf("trajectory.compression_failed identity = %+v, want the run quadruple", ev.Identity)
			}
		}
	}
	if !failed {
		t.Error("trajectory.compression_failed was not emitted")
	}
}

// A blocked summarizer must not block inspection, and publication is guarded.
func TestRunLoop_Compression_InspectionDuringGeneration(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	rec := &emitRecorder{}
	spec, _, tr := compressionSpec(3, rec)
	var mu sync.RWMutex
	spec.TrajectoryMu = &mu
	started, release := make(chan struct{}), make(chan struct{})
	spec.Compression = planner.NewCompressionRunner(&inspectionSummariser{started: started, release: release}, planner.WithTokenEstimator(overBudgetEstimator))
	spec.Base.Budget = planner.Budget{TokenBudget: 10}
	done := make(chan error, 1)
	go func() { _, err := rl.Run(t.Context(), spec); done <- err }()
	<-started
	mu.RLock()
	if tr.Summary != nil {
		t.Error("candidate published before generation finished")
	}
	mu.RUnlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type inspectionSummariser struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *inspectionSummariser) Summarise(ctx context.Context, _ planner.RunContext, _ *planner.Trajectory) (*planner.TrajectorySummary, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return compressionTestSummary(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
