// trajectory_budget_test.go — phase 213 (D-358). The compaction payload
// budget is DERIVED from the LLM-context heavy-output threshold
// (threshold − one fragment cap), so the raise reaches it with no edit
// to trajectory.go. These arms prove the derivation held rather than
// re-typing the new number: they assert the payload can now GROW past
// the old 32 KiB ceiling on the default configuration, and that a
// configured threshold still wins.

package summarizer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

// budgetTrajectory returns a trajectory whose naive rendering far
// exceeds any plausible budget, so the builder must elide.
func budgetTrajectory(steps int) *planner.Trajectory {
	tr := trajFixture()
	tr.Steps = nil
	chunk := strings.Repeat("y", 3*1024)
	for range steps {
		tr.Steps = append(tr.Steps, planner.Step{
			Action:         map[string]any{"tool": "chatty_tool"},
			LLMObservation: chunk,
		})
	}
	return tr
}

// TestTrajectoryBudget_DefaultDerivesFromRaisedThreshold — with no
// option threaded, the payload budget follows the raised LLM-context
// threshold: the rendered payload lands ABOVE the old 32 KiB ceiling
// (proving the derivation moved) and stays strictly BELOW the guard the
// summariser's own Complete call would trip.
func TestTrajectoryBudget_DefaultDerivesFromRaisedThreshold(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	if _, err := s.Summarise(context.Background(), trajRC("r-budget-default"), budgetTrajectory(120)); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if len(payload) >= llm.DefaultHeavyOutputThreshold {
		t.Fatalf("payload is %d bytes — at/over the %d guard; the budget failed",
			len(payload), llm.DefaultHeavyOutputThreshold)
	}
	if len(payload) <= 32*1024 {
		t.Fatalf("payload is only %d bytes — the budget did NOT track the raised threshold "+
			"(it is derived from %d, so it should now admit far more than the old 32 KiB)",
			len(payload), llm.DefaultHeavyOutputThreshold)
	}
}

// TestTrajectoryBudget_OperatorThresholdStillWins — an operator who
// pins the old value gets the old budget back, with no edit to the
// derivation. This is the §10 override read through the summariser.
func TestTrajectoryBudget_OperatorThresholdStillWins(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client,
		summarizer.WithTrajectoryHeavyOutputThreshold(32*1024))
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	if _, err := s.Summarise(context.Background(), trajRC("r-budget-pinned"), budgetTrajectory(120)); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if len(payload) >= 32*1024 {
		t.Fatalf("payload is %d bytes — a configured 32 KiB threshold must bound it below 32768",
			len(payload))
	}
	if !strings.Contains(payload, "earlier steps elided to fit the compaction payload budget") {
		t.Error("the configured budget rendered no elision marker")
	}
}

// TestTrajectoryBudget_TracksTheLLMContextArmNotTheConsolePin — the
// summariser composes PROMPT bytes, so its budget derives from the
// LLM-context threshold and must never be re-pointed at the pinned
// Console inline-payload bound.
func TestTrajectoryBudget_TracksTheLLMContextArmNotTheConsolePin(t *testing.T) {
	t.Parallel()
	if llm.DefaultHeavyOutputThreshold != config.DefaultHeavyOutputThresholdBytes {
		t.Fatalf("llm.DefaultHeavyOutputThreshold = %d, want the LLM-context arm %d",
			llm.DefaultHeavyOutputThreshold, config.DefaultHeavyOutputThresholdBytes)
	}
	if llm.DefaultHeavyOutputThreshold == config.DefaultConsoleInlinePayloadBytes {
		t.Fatal("the compaction payload budget must not derive from the Console inline-payload bound")
	}
}
