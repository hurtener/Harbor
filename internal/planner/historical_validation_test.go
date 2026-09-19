package planner_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

func TestHistoricalValidation_ClosedEnvelope(t *testing.T) {
	t.Parallel()
	valid := func() planner.Step {
		return planner.Step{Historical: &planner.HistoricalStep{Version: 1, SourceRun: "source", Kind: "call_tool", Body: json.RawMessage(`{"action":{"Tool":"read","Args":{}},"llm_observation":{"more":false}}`)}}
	}
	cases := map[string]func(*planner.Step){
		"no envelope":      func(s *planner.Step) { s.Historical = nil },
		"bad version":      func(s *planner.Step) { s.Historical.Version = 2 },
		"unknown kind":     func(s *planner.Step) { s.Historical.Kind = "invented" },
		"missing origin":   func(s *planner.Step) { s.Historical.SourceRun = "" },
		"negative ordinal": func(s *planner.Step) { s.Historical.Index = -1 },
		"ambiguous action": func(s *planner.Step) { s.Action = planner.CallTool{Tool: "write"} },
		"ambiguous result": func(s *planner.Step) { s.LLMObservation = "PRIVATE" },
		"outer reasoning":  func(s *planner.Step) { s.ReasoningTrace = "PRIVATE" },
		"outer failure":    func(s *planner.Step) { s.Error = "PRIVATE" },
		"large body":       func(s *planner.Step) { s.Historical.Body = json.RawMessage(strings.Repeat(" ", 512*1024+1)) },
	}
	bodies := []string{
		`null`, `[]`, `{`, `{}`, `{} {}`, `{"action":null}`,
		`{"action":{},"observation":"PRIVATE"}`,
		`{"action":{},"reasoning_trace":"PRIVATE"}`,
		`{"action":{},"streams":{}}`, `{"action":{},"historical":{}}`,
		`{"Action":{},"action":{}}`, `{"action":{},"action":{}}`,
		`{"action":{},"unexpected":"PRIVATE"}`, `{"action":{},"failure":{"unknown":true}}`,
		`{"action":{},"latency_ms":"invalid"}`,
	}
	for _, body := range bodies {
		cases[body] = func(s *planner.Step) { s.Historical.Body = json.RawMessage(body) }
	}
	cases["bad UTF8"] = func(s *planner.Step) { s.Historical.Body = json.RawMessage{'{', '"', 255, '"', ':', '1', '}'} }
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			step := valid()
			mutate(&step)
			_, err := planner.ReadHistoricalStep(step)
			if !errors.Is(err, planner.ErrInvalidHistoricalStep) {
				t.Fatalf("invalid envelope accepted: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("error exposed historical payload")
			}
		})
	}
	if _, err := planner.ReadHistoricalStep(valid()); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalValidation_ConcurrentExactData(t *testing.T) {
	t.Parallel()
	live := planner.Step{Action: planner.CallTool{Tool: "read", Args: json.RawMessage(`{"version":9007199254740993}`)},
		LLMObservation: map[string]any{"version": json.Number("9007199254740993"), "source": "café 東京", "more": false, "reasoning_trace": "opaque tool field"},
		ReasoningTrace: "PRIVATE-THOUGHT", Observation: "PRIVATE-RAW"}
	retained, err := planner.RetainStep(live, "source", 3)
	if err != nil {
		t.Fatal(err)
	}
	before := string(retained.Historical.Body)
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := planner.ReadHistoricalStep(retained)
			if err != nil {
				t.Error(err)
				return
			}
			if _, executable := got.Action.(planner.Decision); executable {
				t.Error("history became executable")
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Error(err)
				return
			}
			for _, want := range []string{"9007199254740993", "café 東京", `"more":false`, "opaque tool field"} {
				if !strings.Contains(string(data), want) {
					t.Error("permitted data changed")
				}
			}
			if strings.Contains(string(data), "PRIVATE-") {
				t.Error("private data survived capture")
			}
			// The decode result is detached even under a shared source envelope.
			if obs, ok := got.LLMObservation.(map[string]any); ok {
				obs["source"] = "changed"
			}
		}()
	}
	wg.Wait()
	if before != string(retained.Historical.Body) {
		t.Fatal("shared history mutated")
	}
}
