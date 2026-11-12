package steering

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// limitPlanner calls a tool every step, advancing the clock and/or reporting
// model usage, and answers when asked to finalize.
type limitPlanner struct {
	mu        sync.Mutex
	clk       *fakeClock
	stepTime  time.Duration
	tokens    int
	finalErr  error
	finalized []planner.FinalizeRequest
	steps     int
}

func (p *limitPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rc.Control.Finalize != nil {
		p.finalized = append(p.finalized, *rc.Control.Finalize)
		if p.finalErr != nil {
			return nil, p.finalErr
		}
		return planner.Finish{Reason: planner.FinishLimitReached, Payload: "Did three searches; the comparison remains."}, nil
	}
	p.steps++
	if p.clk != nil {
		p.clk.advance(p.stepTime)
	}
	llm.RunUsageFrom(ctx).Add(llm.Usage{PromptTokens: p.tokens, CompletionTokens: p.tokens / 10})
	return planner.CallTool{Tool: "web_search"}, nil
}

func limitRunLoop(t *testing.T) (*RunLoop, *fakeClock, *recordingBus) {
	t.Helper()
	clk := newFakeClock()
	bus := &recordingBus{}
	rl, err := NewRunLoop(NewRegistry(WithClock(clk)), &stubCoordinator{}, WithRunLoopClock(clk), WithRunLoopBus(bus))
	if err != nil {
		t.Fatal(err)
	}
	return rl, clk, bus
}

func limitEvents(bus *recordingBus) []RunLimitReachedPayload {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	var out []RunLimitReachedPayload
	for _, ev := range bus.published {
		if ev.Type == EventTypeRunLimitReached {
			out = append(out, ev.Payload.(RunLimitReachedPayload))
		}
	}
	return out
}

func TestRunLimits_DurationStopsWithPlannerFinalAnswer(t *testing.T) {
	rl, clk, bus := limitRunLoop(t)
	p := &limitPlanner{clk: clk, stepTime: 8 * time.Minute}
	spec := runSpecFor(runA, p)
	spec.MaxSteps = 50
	spec.Base.Trajectory = &planner.Trajectory{Query: "q"}
	spec.Limits = planner.RunLimits{MaxDuration: 20 * time.Minute}
	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("a limit must never fail the run: %v", err)
	}
	if fin.Reason != planner.FinishLimitReached || fin.Payload != "Did three searches; the comparison remains." {
		t.Fatalf("fin = %+v", fin)
	}
	if fin.Metadata["limit"] != planner.RunLimitMaxDuration || fin.Metadata["via"] != "planner" {
		t.Fatalf("metadata = %+v", fin.Metadata)
	}
	if p.steps != 3 || len(p.finalized) != 1 || p.finalized[0].Limit != planner.RunLimitMaxDuration || p.finalized[0].MaxOutputTokens <= 0 {
		t.Fatalf("steps=%d finalize=%+v; want three tool steps then exactly one final call", p.steps, p.finalized)
	}
	if !strings.Contains(p.finalized[0].Instructions, "Do not call any tools") {
		t.Fatal("final instructions do not forbid tools")
	}
	events := limitEvents(bus)
	if len(events) != 1 || events[0].Limit != planner.RunLimitMaxDuration || events[0].MaxDurationMS != (20*time.Minute).Milliseconds() || events[0].ElapsedMS < (20*time.Minute).Milliseconds() {
		t.Fatalf("run.limit_reached events = %+v", events)
	}
}

func TestRunLimits_TokensCountEveryModelCall(t *testing.T) {
	rl, _, bus := limitRunLoop(t)
	p := &limitPlanner{tokens: 400}
	spec := runSpecFor(runA, p)
	spec.MaxSteps = 50
	spec.Limits = planner.RunLimits{MaxTokens: 1000}
	fin, err := rl.Run(context.Background(), spec)
	if err != nil || fin.Reason != planner.FinishLimitReached {
		t.Fatalf("fin=%+v err=%v", fin, err)
	}
	// 440 tokens per step: the limit is reached after the third step.
	if p.steps != 3 {
		t.Fatalf("steps=%d, want 3", p.steps)
	}
	events := limitEvents(bus)
	if len(events) != 1 || events[0].Limit != planner.RunLimitMaxTokens || events[0].TokensUsed != 1320 || events[0].MaxTokens != 1000 {
		t.Fatalf("run.limit_reached events = %+v", events)
	}
}

func TestRunLimits_FinalAnswerFailureUsesDeterministicPartialAnswer(t *testing.T) {
	rl, clk, _ := limitRunLoop(t)
	p := &limitPlanner{clk: clk, stepTime: 11 * time.Minute, finalErr: errors.New("provider unavailable")}
	spec := runSpecFor(runA, p)
	spec.MaxSteps = 50
	spec.Base.Trajectory = &planner.Trajectory{Query: "q"}
	spec.Limits = planner.RunLimits{MaxDuration: 20 * time.Minute, MaxTokens: 1 << 40}
	fin, err := rl.Run(context.Background(), spec)
	if err != nil || fin.Reason != planner.FinishLimitReached {
		t.Fatalf("fin=%+v err=%v; a failed final answer must not fail the run", fin, err)
	}
	answer, _ := fin.Payload.(string)
	if fin.Metadata["via"] != "runtime" || !strings.Contains(answer, "maximum duration") || !strings.Contains(answer, "web_search (2)") {
		t.Fatalf("deterministic answer = %q metadata=%+v", answer, fin.Metadata)
	}
}

func TestRunLimits_NoLimitLeavesRunUnchanged(t *testing.T) {
	rl, clk, bus := limitRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "noop"}}}, defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	clk.advance(time.Hour)
	fin, err := rl.Run(context.Background(), runSpecFor(runA, p))
	if err != nil || fin.Reason != planner.FinishGoal || len(limitEvents(bus)) != 0 {
		t.Fatalf("fin=%+v err=%v", fin, err)
	}
	if p.controlAt(0).Finalize != nil {
		t.Fatal("finalize requested without a limit")
	}
}

func TestRunLimits_InvalidLimitsRefused(t *testing.T) {
	rl, _, _ := limitRunLoop(t)
	spec := runSpecFor(runA, &scriptedPlanner{})
	spec.Limits = planner.RunLimits{MaxTokens: -1}
	if _, err := rl.Run(context.Background(), spec); !errors.Is(err, ErrRunLoopMisconfigured) {
		t.Fatalf("err=%v", err)
	}
}

// Concurrent runs keep independent clocks and usage totals.
func TestRunLimits_ConcurrentRunsDoNotShareUsage(t *testing.T) {
	rl, _, _ := limitRunLoop(t)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := runA
			q.RunID = "run-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String()
			p := &limitPlanner{tokens: 100 * (1 + i%4)}
			spec := runSpecFor(q, p)
			spec.MaxSteps = 100
			spec.Limits = planner.RunLimits{MaxTokens: 2000}
			fin, err := rl.Run(context.Background(), spec)
			if err != nil || fin.Reason != planner.FinishLimitReached {
				t.Errorf("run %d: fin=%+v err=%v", i, fin, err)
				return
			}
			perStep := int64(p.tokens + p.tokens/10)
			want := (2000 + perStep - 1) / perStep
			if int64(p.steps) != want {
				t.Errorf("run %d: steps=%d want=%d (usage bled across runs)", i, p.steps, want)
			}
		}(i)
	}
	wg.Wait()
}

func TestRunLimits_Stricter(t *testing.T) {
	base := planner.RunLimits{MaxDuration: 20 * time.Minute}
	got := base.Stricter(planner.RunLimits{MaxDuration: time.Hour, MaxTokens: 5000})
	if got.MaxDuration != 20*time.Minute || got.MaxTokens != 5000 {
		t.Fatalf("stricter = %+v; a request may only tighten", got)
	}
	got = base.Stricter(planner.RunLimits{MaxDuration: 5 * time.Minute})
	if got.MaxDuration != 5*time.Minute {
		t.Fatalf("stricter = %+v", got)
	}
	if (planner.RunLimits{}).Stricter(planner.RunLimits{}).IsZero() != true {
		t.Fatal("zero stays unlimited")
	}
}
