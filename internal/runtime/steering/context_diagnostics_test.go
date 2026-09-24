package steering

import (
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

func TestContextDiagnostics_RuntimeSnapshot(t *testing.T) {
	t.Parallel()
	var mu sync.RWMutex
	tr := &planner.Trajectory{Query: "private query", Steps: []planner.Step{{LLMObservation: "old"}, {LLMObservation: "fresh"}}}
	spec, rc := RunSpec{TrajectoryMu: &mu}, planner.RunContext{Trajectory: tr}
	start := 1
	tr.UnseenFrom = &start
	digest, err := tr.PrefixDigest(1)
	if err != nil {
		t.Fatal(err)
	}
	tr.Summary = &planner.TrajectorySummary{Facts: []string{"private narrative"}, Coverage: &planner.SummaryCoverage{Version: 1, Generation: 3, ThroughStep: 1, PrefixDigest: digest}}
	h := contextHistory(spec, rc)
	if h == nil || h.ReplayStart != 1 || h.ReplayEnd != 2 || h.CheckpointGeneration != 3 || !h.UnseenKnown || h.UnseenFrom != 1 {
		t.Fatalf("snapshot=%+v", h)
	}
	h.ReplayStart = 999
	if contextHistory(spec, rc).ReplayStart != 1 {
		t.Fatal("snapshot mutated runtime")
	}
	tr.Summary.Coverage.PrefixDigest = "invalid"
	if contextHistory(spec, rc) != nil {
		t.Fatal("diagnostics trusted invalid coverage")
	}
	tr.Summary.Coverage = nil
	if got := contextHistory(spec, rc); got.ReplayStart != 0 || got.CheckpointGeneration != 0 {
		t.Fatal("legacy checkpoint assigned invented coverage")
	}
	tr.UnseenFrom = nil
	if contextHistory(spec, rc).UnseenKnown {
		t.Fatal("unknown exposure became zero")
	}
	if contextHistory(spec, planner.RunContext{}) != nil {
		t.Fatal("nil trajectory acquired diagnostics")
	}
}
