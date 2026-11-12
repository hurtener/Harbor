package planner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem" // driver conformance: reduction retains exact observations in a real store
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

func reduceRC() planner.RunContext {
	return planner.RunContext{Quadruple: identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}}
}

func reduceTrajectory(t *testing.T) *planner.Trajectory {
	t.Helper()
	big := strings.Repeat("o", 20*1024)
	historical, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: "old_read"}, LLMObservation: map[string]any{"text": "HIST-" + big}}, "earlier", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &planner.Trajectory{Query: "q", Steps: []planner.Step{
		historical,
		{Action: planner.CallTool{Tool: "small"}, LLMObservation: "tiny"},
		{Action: planner.CallTool{Tool: "failed"}, LLMObservation: map[string]any{"error": big}, Error: "boom"},
		{Action: planner.CallTool{Tool: "web_search"}, Observation: "RAW-" + big, LLMObservation: "LIVE-" + big},
		{Action: planner.CallTool{Tool: "latest"}, LLMObservation: "LATEST-" + big},
	}}
}

func TestReduceOldestObservations_OldestFirstWithRetainedReferences(t *testing.T) {
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	runner := planner.NewCompressionRunner(&staticSummariser{}, planner.WithCompressionArtifactStore(store))
	tr := reduceTrajectory(t)
	var order []int
	n, err := runner.ReduceOldestObservations(t.Context(), reduceRC(), tr, func(index int, reduced planner.Step) (bool, error) {
		order = append(order, index)
		tr.Steps[index] = reduced
		return false, nil
	})
	if err != nil || n != 2 || len(order) != 2 || order[0] != 0 || order[1] != 3 {
		t.Fatalf("reduced=%d order=%v err=%v; want historical then live, skipping small, failed and latest", n, order, err)
	}
	hist, err := planner.ReadHistoricalStep(tr.Steps[0])
	if err != nil {
		t.Fatalf("reduced historical step no longer valid: %v", err)
	}
	for _, obs := range []any{hist.LLMObservation, tr.Steps[3].LLMObservation} {
		m, ok := obs.(map[string]any)
		if !ok || m["truncated"] != true || m["artifact_ref"] == "" || len(m["preview"].(string)) > planner.ReducedObservationPreviewBytes {
			t.Fatalf("reduced observation has wrong shape: %#v", obs)
		}
		body, found, err := store.Get(t.Context(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, m["artifact_ref"].(string))
		if err != nil || !found || (!strings.Contains(string(body), "HIST-") && !strings.Contains(string(body), "LIVE-")) {
			t.Fatal("exact observation was not retained under the session triple")
		}
	}
	if tr.Steps[3].Observation != "RAW-"+strings.Repeat("o", 20*1024) {
		t.Fatal("raw observation was modified")
	}
	if !strings.HasPrefix(tr.Steps[4].LLMObservation.(string), "LATEST-") {
		t.Fatal("latest exchange was reduced")
	}
	// Idempotent: already-reduced observations are not reduced again.
	again, err := runner.ReduceOldestObservations(t.Context(), reduceRC(), tr, func(int, planner.Step) (bool, error) { return false, nil })
	if err != nil || again != 0 {
		t.Fatalf("second pass reduced %d (err=%v)", again, err)
	}
}

func TestReduceOldestObservations_StopsWhenInstallReportsFit(t *testing.T) {
	runner := planner.NewCompressionRunner(&staticSummariser{})
	tr := reduceTrajectory(t)
	n, err := runner.ReduceOldestObservations(t.Context(), reduceRC(), tr, func(index int, reduced planner.Step) (bool, error) {
		m := reduced.LLMObservation
		if reduced.Historical != nil {
			view, _ := planner.ReadHistoricalStep(reduced)
			m = view.LLMObservation
		}
		if m.(map[string]any)["remainder_not_retained"] != true {
			t.Fatal("reduction without a store must say the remainder was not retained")
		}
		return true, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("reduced=%d err=%v; want stop after the first fitting install", n, err)
	}
}

func TestReduceOldestObservations_RequiresIdentity(t *testing.T) {
	runner := planner.NewCompressionRunner(&staticSummariser{})
	if _, err := runner.ReduceOldestObservations(t.Context(), planner.RunContext{}, reduceTrajectory(t), nil); err == nil {
		t.Fatal("missing identity accepted")
	}
}
