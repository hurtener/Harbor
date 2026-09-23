package summarizer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestTrajectoryChronological_ReceiptAndPriorNarrativeRemainExact(t *testing.T) {
	t.Parallel()
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("x", 14580)
	receipt := `{"source":"` + source + `","id":"doc-a","version":7,"more":false}`
	tr := trajFixture()
	tr.Summary = &planner.TrajectorySummary{Facts: []string{"Keep the approved navigation"}, Coverage: &planner.SummaryCoverage{Version: 1, Generation: 1, ThroughStep: 25, PrefixDigest: "not-for-model"}}
	tr.Steps = []planner.Step{{Action: planner.CallTool{Tool: "read", CallID: "read-7", Args: json.RawMessage(`{"id":"doc-a"}`)}, LLMObservation: json.RawMessage(receipt), ReasoningTrace: "PRIVATE-REASONING"}}
	before, _ := json.Marshal(tr)
	got, err := s.Summarise(context.Background(), trajRC("receipt"), tr)
	if err != nil {
		t.Fatal(err)
	}
	payload := *client.seenCalls()[0].req.Messages[1].Content.Text
	if !strings.Contains(payload, receipt) || !strings.Contains(payload, "Keep the approved navigation") {
		t.Fatal("source, trailing metadata or previous constraints were omitted")
	}
	if strings.Contains(payload, "PRIVATE-REASONING") || strings.Contains(payload, "not-for-model") || got.Coverage != nil {
		t.Fatal("runtime/private metadata leaked to generated narrative")
	}
	after, _ := json.Marshal(tr)
	if string(before) != string(after) {
		t.Fatal("summarization mutated source")
	}
}

func TestTrajectoryChronological_RejectsIncompleteOrInvalidNarrative(t *testing.T) {
	t.Parallel()
	cases := map[string]llm.CompleteResponse{
		"length despite valid JSON": {Content: goodSummaryJSON, FinishReason: "length"},
		"tool emission":             {Content: goodSummaryJSON, ToolCalls: []llm.ToolCallStructured{{Name: "forbidden"}}},
		"filtered":                  {Content: goodSummaryJSON, FinishReason: "content_filter"},
		"note only":                 {Content: `{"goals":[],"facts":[],"pending":[],"last_output_digest":"","note":"looks good"}`},
		"whitespace only":           {Content: `{"goals":[" "],"facts":[],"pending":[],"last_output_digest":" ","note":"x"}`},
		"missing":                   {Content: `{"facts":["x"]}`},
		"extra":                     {Content: `{"unexpected":"PRIVATE-CONTENT"}`},
		"duplicate":                 {Content: strings.Replace(goodSummaryJSON, `"facts":`, `"facts":["PRIVATE-CONTENT"],"facts":`, 1)},
		"coverage injection":        {Content: strings.Replace(goodSummaryJSON, `"note":`, `"coverage":{"through_step":999},"note":`, 1)},
		"null entry":                {Content: strings.Replace(goodSummaryJSON, `["find the access code"]`, `[null,"valid"]`, 1)},
		"bad encoding":              {Content: string([]byte{255})},
		"null array":                {Content: strings.Replace(goodSummaryJSON, `["find the access code"]`, `null`, 1)},
		"trailing object":           {Content: goodSummaryJSON + `{}`},
		"unclosed fence":            {Content: "```json\n" + goodSummaryJSON},
		"oversized":                 {Content: strings.Repeat("PRIVATE-CONTENT", 2000)},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := summarizer.NewTrajectorySummariser(&stubClient{response: response})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Summarise(context.Background(), trajRC("invalid"), trajFixture())
			if err == nil || got != nil {
				t.Fatal("invalid candidate accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE-CONTENT") {
				t.Fatal("response content leaked through error")
			}
		})
	}
}

func TestTrajectoryChronological_CapacityNeverClips(t *testing.T) {
	t.Parallel()
	for _, threshold := range []int{1, 64, 4096, 16384} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			c := newStubClient()
			s, err := summarizer.NewTrajectorySummariser(c, summarizer.WithTrajectoryHeavyOutputThreshold(threshold))
			if err != nil {
				t.Fatal(err)
			}
			tr := trajFixture()
			tr.Steps[0].LLMObservation = strings.Repeat("x", threshold+1)
			got, err := s.Summarise(context.Background(), trajRC("capacity"), tr)
			if !errors.Is(err, summarizer.ErrTrajectorySummaryCapacity) || got != nil || len(c.seenCalls()) != 0 {
				t.Fatalf("got=%v err=%v calls=%d", got, err, len(c.seenCalls()))
			}
		})
	}
}

func TestTrajectoryChronological_CallBoundAndNoPartialCandidate(t *testing.T) {
	t.Parallel()
	c := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(c, summarizer.WithTrajectoryHeavyOutputThreshold(8192))
	if err != nil {
		t.Fatal(err)
	}
	tr := budgetTrajectory(100)
	prior := &planner.TrajectorySummary{Facts: []string{"original"}}
	tr.Summary = prior
	got, err := s.Summarise(context.Background(), trajRC("bound"), tr)
	if !errors.Is(err, summarizer.ErrTrajectorySummaryCapacity) || got != nil || len(c.seenCalls()) != 16 || !reflect.DeepEqual(tr.Summary, prior) {
		t.Fatalf("got=%v err=%v calls=%d", got, err, len(c.seenCalls()))
	}
}

func TestTrajectoryChronological_LaterFailureKeepsInstalledCheckpoint(t *testing.T) {
	t.Parallel()
	boom := errors.New("scripted later failure")
	count := 0
	scopes := map[string]bool{}
	model := "selected-model"
	c := &funcClient{fn: func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		count++
		scope, ok := llm.AttemptScopeFrom(ctx)
		if !ok || scope.CallID == "" || scopes[scope.CallID] {
			t.Error("maintenance reused invocation identity")
		}
		scopes[scope.CallID] = true
		if req.Model != model || len(req.Tools) != 0 {
			t.Error("effective model or tools changed")
		}
		if count == 2 {
			return llm.CompleteResponse{}, boom
		}
		return llm.CompleteResponse{Content: goodSummaryJSON, FinishReason: "stop"}, nil
	}}
	s, err := summarizer.NewTrajectorySummariser(c, summarizer.WithTrajectoryHeavyOutputThreshold(8192))
	if err != nil {
		t.Fatal(err)
	}
	tr := budgetTrajectory(20)
	original := &planner.TrajectorySummary{Facts: []string{"legacy constraints"}}
	tr.Summary = original
	rc := trajRC("later-failure")
	rc.LLMOverrides = &planner.LLMOverrides{Model: &model}
	got, err := s.Summarise(context.Background(), rc, tr)
	if !errors.Is(err, boom) || count != 2 || got != nil || tr.Summary != original {
		t.Fatalf("got=%v err=%v calls=%d", got, err, count)
	}
}

func TestTrajectoryChronological_EncodingFailureAndLateCancellation(t *testing.T) {
	t.Parallel()
	t.Run("non JSON result", func(t *testing.T) {
		c := newStubClient()
		s, _ := summarizer.NewTrajectorySummariser(c)
		tr := trajFixture()
		tr.Steps[0].LLMObservation = make(chan int)
		got, err := s.Summarise(context.Background(), trajRC("encoding"), tr)
		var target planner.ErrUnserializable
		if got != nil || !errors.As(err, &target) || len(c.seenCalls()) != 0 {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})
	t.Run("cancel during completion", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &funcClient{fn: func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
			cancel()
			return llm.CompleteResponse{Content: goodSummaryJSON}, nil
		}}
		s, _ := summarizer.NewTrajectorySummariser(c)
		got, err := s.Summarise(ctx, trajRC("cancel"), trajFixture())
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})
}
