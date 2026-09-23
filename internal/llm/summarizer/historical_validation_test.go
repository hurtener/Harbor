package summarizer_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestTrajectoryHistorical_RejectsInvalidEvidenceBeforeInference(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"action":{"Tool":"read"},"reasoning_trace":"PRIVATE-CONTENT"}`,
		`{"action":{"Tool":"read"},"observation":"PRIVATE-CONTENT"}`,
		`{"action":{"Tool":"read"},"streams":{"reasoning":[]}}`,
		`{"historical":{"version":1}}`,
		`{"action":{},"action":{"Tool":"read"}}`,
		`{"Action":{},"action":{"Tool":"read"}}`,
		`{"action":{},"authority":"PRIVATE-CONTENT"}`,
		`null`,
	} {
		t.Run(body, func(t *testing.T) {
			client := newStubClient()
			s, err := summarizer.NewTrajectorySummariser(client)
			if err != nil {
				t.Fatal(err)
			}
			history := &planner.HistoricalStep{Version: 1, SourceRun: "older", Kind: "call_tool", Body: json.RawMessage(body)}
			tr := &planner.Trajectory{Steps: []planner.Step{{Historical: history}}}
			got, err := s.Summarise(context.Background(), trajRC("invalid-history"), tr)
			if !errors.Is(err, planner.ErrInvalidHistoricalStep) || got != nil || len(client.seenCalls()) != 0 {
				t.Fatalf("invalid retained evidence reached inference: summary=%v err=%v calls=%d", got != nil, err, len(client.seenCalls()))
			}
			if strings.Contains(err.Error(), "PRIVATE-CONTENT") {
				t.Fatal("private content leaked through error")
			}
		})
	}
}
