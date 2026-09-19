package runctx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

type retainedSummaryRecorder struct{ inputs []*planner.Trajectory }

func (s *retainedSummaryRecorder) Summarise(_ context.Context, _ planner.RunContext, tr *planner.Trajectory) (*planner.TrajectorySummary, error) {
	s.inputs = append(s.inputs, tr)
	return &planner.TrajectorySummary{Facts: []string{"Keep the approved navigation"}, Pending: []string{"verify remaining edit"}}, nil
}

func TestRetainedCheckpoint_ReusesCoverageAcrossTurns(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("first", "checkpoint")
			first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Apply(&base); err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"COVERED-ONE", "COVERED-TWO", "EXACT-FRESH-THREE"} {
				base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: value, Args: json.RawMessage(`{}`)}, LLMObservation: value})
			}
			recorder := &retainedSummaryRecorder{}
			compactor := planner.NewCompressionRunner(recorder)
			base.Budget.TokenBudget = 1
			if err := compactor.MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Fatal(err)
			}
			if base.Trajectory.Summary == nil {
				t.Fatal("fixture did not compact")
			}
			if err := first.Finish(t.Context(), base.Trajectory, base.Query, "first complete", "complete"); err != nil {
				t.Fatal(err)
			}
			if driver == "sqlite" {
				if err := store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			next := retainedBase("second", "checkpoint")
			next.Query, next.Trajectory.Query = "Now change the footer", "Now change the footer"
			second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Apply(&next); err != nil {
				t.Fatal(err)
			}
			if next.Trajectory.Summary == nil {
				t.Fatal("retained checkpoint was lost between turns")
			}
			start, err := next.Trajectory.ReplayStart()
			if err != nil || start != 3 {
				t.Fatalf("restored coverage=%d error=%v, want query plus two covered exchanges", start, err)
			}
			tail, err := json.Marshal(next.Trajectory.Steps[start:])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(tail), "EXACT-FRESH-THREE") || !strings.Contains(string(tail), "Now change the footer") || strings.Contains(string(tail), "COVERED-ONE") {
				t.Fatal("restored coverage hid fresh evidence or replayed covered history")
			}
			next.Trajectory.Steps = append(next.Trajectory.Steps, planner.Step{LLMObservation: "second older"}, planner.Step{LLMObservation: "second latest"})
			next.Budget.TokenBudget = 1
			if err := compactor.MaybeCompress(t.Context(), next, next.Trajectory); err != nil {
				t.Fatal(err)
			}
			if len(recorder.inputs) != 2 || recorder.inputs[1].Summary == nil {
				t.Fatal("new compaction lost previous narrative")
			}
			input, _ := json.Marshal(recorder.inputs[1])
			if strings.Contains(string(input), "COVERED-ONE") || !strings.Contains(string(input), "EXACT-FRESH-THREE") {
				t.Fatal("new compaction reprocessed covered history or lost uncovered evidence")
			}
			if err := second.Finish(t.Context(), next.Trajectory, next.Query, "second complete", "complete"); err != nil {
				t.Fatal(err)
			}
			thirdBase := retainedBase("third", "checkpoint")
			third, err := runctx.BeginRetainedRun(t.Context(), store, redactor, thirdBase.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := third.Apply(&thirdBase); err != nil {
				t.Fatal(err)
			}
			if thirdBase.Trajectory.Summary == nil || thirdBase.Trajectory.Summary.Coverage.Generation != 2 {
				t.Fatal("rolling checkpoint generation did not survive")
			}
			if _, err := thirdBase.Trajectory.ReplayStart(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainedCheckpoint_InvalidationAndCorruption(t *testing.T) {
	for _, scenario := range []string{"expiry", "eviction", "changed source", "bad generation", "unversioned injection"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			base := retainedBase("first", scenario)
			first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Apply(&base); err != nil {
				t.Fatal(err)
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: "older"}, planner.Step{LLMObservation: "latest"})
			base.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Fatal(err)
			}
			if err := first.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Fatal(err)
			}
			q := identity.Quadruple{Identity: base.Quadruple.Identity}
			before, err := store.Load(t.Context(), q, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(before.Bytes, &object); err != nil {
				t.Fatal(err)
			}
			if len(object["checkpoint"]) == 0 {
				t.Fatal("fixture lost checkpoint")
			}
			switch scenario {
			case "expiry":
				now = now.Add(2 * time.Hour)
			case "eviction":
				secondBase := retainedBase("second", scenario)
				second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, secondBase.Quadruple, 1, time.Hour, clock)
				if err != nil {
					t.Fatal(err)
				}
				if err := second.Apply(&secondBase); err != nil {
					t.Fatal(err)
				}
				if err := second.Finish(t.Context(), secondBase.Trajectory, secondBase.Query, "newest", "complete"); err != nil {
					t.Fatal(err)
				}
			default:
				var cp map[string]any
				if err := json.Unmarshal(object["checkpoint"], &cp); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed source" {
					cp["source_digest"] = "altered"
				}
				if scenario == "bad generation" {
					cp["generation"] = 0
				}
				if scenario == "unversioned injection" {
					object["version"] = json.RawMessage(`2`)
				}
				object["checkpoint"], err = json.Marshal(cp)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(q, retainedKind, before.ID)}, state.NewInternalRecord(state.NewEventID(), q, retainedKind, body)); err != nil {
					t.Fatal(err)
				}
			}
			next := retainedBase("next", scenario)
			last, err := runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, clock)
			if scenario == "changed source" || scenario == "bad generation" || scenario == "unversioned injection" {
				if !errors.Is(err, runctx.ErrRetainedContextUnavailable) || last != nil {
					t.Fatal("corrupt checkpoint admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := last.Apply(&next); err != nil {
				t.Fatal(err)
			}
			if next.Trajectory.Summary != nil {
				t.Fatal("checkpoint outlived its sources")
			}
		})
	}
}

func TestRetainedCheckpoint_ConcurrentSiblingCannotInventCoverage(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	a, b := retainedBase("a", "siblings"), retainedBase("b", "siblings")
	first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, a.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Apply(&a); err != nil {
		t.Fatal(err)
	}
	second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, b.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Apply(&b); err != nil {
		t.Fatal(err)
	}
	for _, base := range []*planner.RunContext{&a, &b} {
		base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: "older-" + base.Quadruple.RunID}, planner.Step{LLMObservation: "latest-" + base.Quadruple.RunID})
		base.Budget.TokenBudget = 1
		if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), *base, base.Trajectory); err != nil {
			t.Fatal(err)
		}
	}
	if err := second.Finish(t.Context(), b.Trajectory, b.Query, "b done", "complete"); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(t.Context(), a.Trajectory, a.Query, "a done", "complete"); err != nil {
		t.Fatal(err)
	}
	// A may publish a checkpoint for A only (its admission precedes B). B's
	// concurrent view cannot claim it observed A. B remains outside A's coverage.
	next := retainedBase("next", "siblings")
	r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&next); err != nil {
		t.Fatal(err)
	}
	start, err := next.Trajectory.ReplayStart()
	if err != nil {
		t.Fatal(err)
	}
	tail, _ := json.Marshal(next.Trajectory.Steps[start:])
	if !strings.Contains(string(tail), "latest-a") || !strings.Contains(string(tail), "older-b") || !strings.Contains(string(tail), "latest-b") {
		t.Fatal("sibling coverage hid unseen evidence")
	}
}

func TestRetainedCheckpoint_SharedStoreReuse(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("checkpoint-%d", i)
			base := retainedBase("first", session)
			r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := r.Apply(&base); err != nil {
				t.Error(err)
				return
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: session}, planner.Step{LLMObservation: "fresh-" + session})
			base.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Error(err)
				return
			}
			if err := r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Error(err)
				return
			}
			next := retainedBase("next", session)
			r, err = runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := r.Apply(&next); err != nil {
				t.Error(err)
				return
			}
			if next.Trajectory.Summary == nil {
				t.Error("lost checkpoint")
				return
			}
			start, err := next.Trajectory.ReplayStart()
			if err != nil {
				t.Error(err)
				return
			}
			body, _ := json.Marshal(next.Trajectory.Steps[start:])
			if !strings.Contains(string(body), "fresh-"+session) {
				t.Error("lost scoped tail")
			}
		}()
	}
	wg.Wait()
}

type checkpointScrubber struct{}

func (checkpointScrubber) Redact(_ context.Context, value any) (any, error) {
	var scrub func(any) any
	scrub = func(v any) any {
		switch x := v.(type) {
		case string:
			return strings.ReplaceAll(x, "SECRET-VALUE", "[redacted]")
		case map[string]any:
			for k, item := range x {
				x[k] = scrub(item)
			}
		case []any:
			for i, item := range x {
				x[i] = scrub(item)
			}
		}
		return v
	}
	return scrub(value), nil
}

type privateCheckpointSummary struct{}

func (privateCheckpointSummary) Summarise(context.Context, planner.RunContext, *planner.Trajectory) (*planner.TrajectorySummary, error) {
	return &planner.TrajectorySummary{Facts: []string{"SECRET-VALUE"}}, nil
}
func TestRetainedCheckpoint_RedactionAndChangedSource(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			store, _, _ := retainedStore(t, "inmem")
			base := retainedBase("first", "redaction")
			first, err := runctx.BeginRetainedRun(t.Context(), store, checkpointScrubber{}, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Apply(&base); err != nil {
				t.Fatal(err)
			}
			old := "ordinary source"
			if changed {
				old = "SECRET-VALUE"
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: old}, planner.Step{LLMObservation: "fresh"})
			base.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(privateCheckpointSummary{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Fatal(err)
			}
			if err := first.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Fatal(err)
			}
			record, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(record.Bytes), "SECRET-VALUE") {
				t.Fatal("checkpoint bypassed redaction")
			}
			next := retainedBase("second", "redaction")
			r, err := runctx.BeginRetainedRun(t.Context(), store, checkpointScrubber{}, next.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&next); err != nil {
				t.Fatal(err)
			}
			if changed && next.Trajectory.Summary != nil {
				t.Fatal("checkpoint claimed coverage over changed evidence")
			}
			if !changed && (next.Trajectory.Summary == nil || next.Trajectory.Summary.Facts[0] != "[redacted]") {
				t.Fatal("redacted narrative did not restore")
			}
		})
	}
}
