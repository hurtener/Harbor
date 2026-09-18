package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

type compactionFunc func(context.Context, planner.RunContext, *planner.Trajectory) (*planner.TrajectorySummary, error)

func (f compactionFunc) Summarise(ctx context.Context, rc planner.RunContext, tr *planner.Trajectory) (*planner.TrajectorySummary, error) {
	return f(ctx, rc, tr)
}

func TestPortableCompaction_RepeatedCoverageAndFailure(t *testing.T) {
	t.Parallel()
	tr := bigTrajectory(10000)
	tr.Steps = append(tr.Steps, planner.Step{LLMObservation: "latest"})
	rc := rcWith(fixedQuadruple("rolling"), 10, nil)
	var inputs []*planner.Trajectory
	fail := false
	runner := planner.NewCompressionRunner(compactionFunc(func(_ context.Context, got planner.RunContext, input *planner.Trajectory) (*planner.TrajectorySummary, error) {
		if got.Trajectory != input {
			t.Fatal("summarizer received live trajectory")
		}
		inputs = append(inputs, input)
		if fail {
			return nil, errors.New("unavailable")
		}
		return cannedSummary(), nil
	}))
	if err := runner.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	if start, err := tr.ReplayStart(); err != nil || start != 2 {
		t.Fatalf("start=%d err=%v", start, err)
	}
	if len(inputs[0].Steps) != 2 {
		t.Fatal("fresh exchange entered summarizer")
	}
	tr.Steps = append(tr.Steps, planner.Step{LLMObservation: "new result"})
	if err := runner.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Summary.Coverage.Generation != 2 || tr.Summary.Coverage.ThroughStep != 3 {
		t.Fatal("coverage did not advance")
	}
	if len(inputs[1].Steps) != 1 || inputs[1].Summary == nil {
		t.Fatal("rolling input omitted prior summary or reprocessed covered steps")
	}
	old := tr.Summary
	tr.Steps = append(tr.Steps, planner.Step{Error: "edit version conflict"})
	fail = true
	if err := runner.MaybeCompress(t.Context(), rc, tr); err == nil {
		t.Fatal("failure swallowed")
	}
	if tr.Summary != old {
		t.Fatal("failed compaction replaced checkpoint")
	}
}

func TestPortableCompaction_UnseenGroupIsProtected(t *testing.T) {
	t.Parallel()
	s := &staticSummariser{summary: cannedSummary()}
	r := planner.NewCompressionRunner(s)
	tr := bigTrajectory(10000)
	tr.Steps = append(tr.Steps, planner.Step{LLMObservation: "unseen last"})
	unseen := 0
	tr.UnseenFrom = &unseen
	rc := rcWith(fixedQuadruple("pending"), 10, nil)
	if err := r.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Summary != nil || s.calls.Load() != 0 {
		t.Fatal("unseen results compacted before decision")
	}
	unseen = 1
	if err := r.MaybeCompress(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Summary.Coverage.ThroughStep != 1 {
		t.Fatal("coverage crossed unseen boundary")
	}
}

func TestPortableCompaction_RejectsBadCandidatesWithoutMutatingSource(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"note-only", "cancel", "changed-prefix", "detached-input"} {
		t.Run(name, func(t *testing.T) {
			tr := bigTrajectory(10000)
			obs := map[string]any{"body": "original"}
			tr.Steps[0].LLMObservation = obs
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := planner.NewCompressionRunner(compactionFunc(func(_ context.Context, _ planner.RunContext, input *planner.Trajectory) (*planner.TrajectorySummary, error) {
				switch name {
				case "note-only":
					return &planner.TrajectorySummary{Note: "nothing", Goals: []string{" "}}, nil
				case "cancel":
					cancel()
				case "changed-prefix":
					tr.Steps[0].Error = "changed"
				case "detached-input":
					input.Steps[0].LLMObservation.(map[string]any)["body"] = "mutated"
				}
				return cannedSummary(), nil
			}))
			err := r.MaybeCompress(ctx, rcWith(fixedQuadruple(name), 10, nil), tr)
			if name == "detached-input" {
				if err != nil || obs["body"] != "original" {
					t.Fatalf("source changed: %v %v", obs, err)
				}
			} else if err == nil || tr.Summary != nil {
				t.Fatalf("bad candidate accepted: %v", err)
			}
		})
	}
}

func TestPortableCompaction_CoverageRoundTripAndTampering(t *testing.T) {
	t.Parallel()
	tr := bigTrajectory(10000)
	tr.Steps[0].Action = planner.CallTool{Tool: "read"}
	r := planner.NewCompressionRunner(&staticSummariser{summary: cannedSummary()})
	if err := r.MaybeCompress(t.Context(), rcWith(fixedQuadruple("roundtrip"), 10, nil), tr); err != nil {
		t.Fatal(err)
	}
	b, err := tr.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := trajectory.Deserialize(b)
	if err != nil {
		t.Fatal(err)
	}
	if start, err := restored.ReplayStart(); err != nil || start != 1 {
		t.Fatalf("roundtrip cursor %d: %v", start, err)
	}
	restored.Steps[0].LLMObservation = "tampered"
	if _, err := restored.ReplayStart(); !errors.Is(err, trajectory.ErrInvalidCoverage) {
		t.Fatal("changed evidence accepted")
	}
}

func TestPortableCompaction_LegacyDoesNotGuessCoverage(t *testing.T) {
	t.Parallel()
	tr := bigTrajectory(10000)
	tr.Summary = &planner.TrajectorySummary{Facts: []string{"stale legacy"}}
	if tr.ActiveSummary() != nil {
		t.Fatal("legacy narrative overrides available original steps")
	}
	if start, err := tr.ReplayStart(); start != 0 || err != nil {
		t.Fatal("invented legacy coverage")
	}
}

func TestPortableCompaction_EstimateExcludesRawAndCoveredDuplicates(t *testing.T) {
	t.Parallel()
	tr := bigTrajectory(10000)
	tr.LLMContext = nil
	before, err := planner.DefaultTokenEstimator(tr)
	if err != nil {
		t.Fatal(err)
	}
	tr.Steps[0].Observation = strings.Repeat("never model-facing", 10000)
	after, err := planner.DefaultTokenEstimator(tr)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("diagnostic duplicate affects active estimate")
	}
}
