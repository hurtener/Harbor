package react

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

func TestHistoricalProjection_ReusesLiveNativeRendering(t *testing.T) {
	t.Parallel()
	call := planner.CallTool{Tool: "read", CallID: "reused-provider-id", Args: json.RawMessage(`{"version":9007199254740993}`)}
	cases := []planner.Step{
		{Action: call, LLMObservation: json.RawMessage(`{"source":"日本語 🌿\nexact text","version":18446744073709551615,"more":false}`), AssistantPreamble: "Inspect the exact source.", ReasoningTrace: "PRIVATE"},
		{Action: call, Error: "version conflict", LLMObservation: map[string]any{"error": "conflict", "result": "retry only with a fresh version"}},
		{Action: planner.CallParallel{Branches: []planner.CallTool{call, call}}, LLMObservation: planner.ParallelObservation{Branches: []planner.ParallelBranchObservation{
			{Index: 0, Value: json.RawMessage(`{"value":9007199254740993}`)}, {Index: 1, Error: "not found"},
		}}},
		{Action: planner.Batch{Tools: []planner.CallTool{call}, Spawns: []planner.SpawnTask{{CallID: "spawn", Spec: planner.SpawnSpec{Query: "check"}}},
			Progress: []planner.TaskProgress{{CallID: "progress", Message: "checking"}}},
			LLMObservation: planner.BatchObservation{
				Tools:    []planner.ParallelBranchObservation{{Index: 0, Value: "source"}},
				Spawns:   []planner.BatchSpawnObservation{{Index: 0, TaskID: "child", GroupID: "group"}},
				Progress: []planner.BatchProgressObservation{{Index: 0, Recorded: true, Emitted: true}},
			}},
		{Action: planner.TaskProgress{CallID: "progress", Message: "checking"}, LLMObservation: "reported"},
		{Action: planner.SpawnTask{CallID: "spawn", Spec: planner.SpawnSpec{Query: "check"}}, LLMObservation: "child registered"},
		{Action: planner.AwaitTask{TaskID: "child"}, LLMObservation: "child result"},
		{Action: planner.TaskStatusQuery{}, LLMObservation: "running"},
		{Action: planner.CancelTask{TaskID: "child"}, LLMObservation: "cancelled"},
		{Action: planner.SteerTask{TaskID: "child", Directive: "stop"}, LLMObservation: "steered"},
		{Action: planner.PauseTask{TaskID: "child"}, LLMObservation: "paused"},
		{Action: planner.ResumeTask{TaskID: "child"}, LLMObservation: "resumed"},
	}
	for index, step := range cases {
		t.Run(fmt.Sprintf("%T", step.Action), func(t *testing.T) {
			before, err := json.Marshal(step)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := planner.RetainStep(step, "origin", index)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Action != nil || stored.AssistantPreamble != "" || stored.ReasoningTrace != "" {
				t.Fatal("historical record became active execution")
			}
			data, err := (&planner.Trajectory{Steps: []planner.Step{stored}}).Serialize()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("PRIVATE")) {
				t.Fatal("private reasoning retained")
			}
			restored, err := trajectory.Deserialize(data)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := renderHistoricalStep(restored.Steps[0].Historical)
			if err != nil {
				t.Fatal(err)
			}
			live := renderStepMessages(step, planner.ReasoningReplayNever, index)
			if len(messages) != len(live) {
				t.Fatal("native exchange size changed")
			}
			ids := map[string]bool{}
			for i, call := range messages[0].ToolCalls {
				if ids[call.ID] || messages[i+1].ToolCallID == nil || *messages[i+1].ToolCallID != call.ID {
					t.Fatal("missing or duplicate historical correlation")
				}
				ids[call.ID] = true
				// Names, arguments, content and order must match the existing
				// native renderer; only the stable cross-turn ID changes.
				live[0].ToolCalls[i].ID = call.ID
				id := call.ID
				live[i+1].ToolCallID = &id
			}
			for i := 1; i < len(live); i++ {
				if live[i].Content.Text != nil && messages[i].Content.Text != nil {
					a, b := *live[i].Content.Text, *messages[i].Content.Text
					if json.Valid([]byte(a)) && json.Valid([]byte(b)) {
						a, b = canonicalHistoricalJSON(t, a), canonicalHistoricalJSON(t, b)
						live[i].Content.Text, messages[i].Content.Text = &a, &b
					}
				}
			}
			if !reflect.DeepEqual(live, messages) {
				a, _ := json.Marshal(live)
				b, _ := json.Marshal(messages)
				t.Fatalf("historical/native rendering diverged\nlive=%s\nhistory=%s", a, b)
			}
			after, err := json.Marshal(step)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("capture mutated live execution")
			}
		})
	}
}

func TestHistoricalProjection_FailedArgumentsNeverReachStorageOrSummary(t *testing.T) {
	t.Parallel()
	failed := planner.CallTool{Tool: "write", CallID: "failed", Args: json.RawMessage(`{"key":"DO-NOT-REPLAY-SECRET"}`)}
	good := planner.CallTool{Tool: "read", CallID: "good", Args: json.RawMessage(`{"id":"keep-this-id"}`)}
	step := planner.Step{
		Action: planner.CallParallel{Branches: []planner.CallTool{failed, good}},
		LLMObservation: planner.ParallelObservation{Branches: []planner.ParallelBranchObservation{
			{Index: 0, Error: "denied"}, {Index: 1, Value: "complete source"},
		}},
	}
	stored, err := planner.RetainStep(step, "origin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Historical.Body, []byte("DO-NOT-REPLAY-SECRET")) ||
		!bytes.Contains(stored.Historical.Body, []byte("keep-this-id")) {
		t.Fatal("failed arguments retained or successful arguments dropped")
	}
	messages, err := renderHistoricalStep(stored.Historical)
	if err != nil {
		t.Fatal(err)
	}
	if string(messages[0].ToolCalls[0].Args) != "{}" ||
		!bytes.Contains(messages[0].ToolCalls[1].Args, []byte("keep-this-id")) {
		t.Fatal("branch argument policy changed")
	}
}

func TestHistoricalProjection_InvalidEnvelopeFailsClosed(t *testing.T) {
	t.Parallel()
	valid, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: "read"}, LLMObservation: "source"}, "origin", 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*planner.HistoricalStep){
		"version":       func(h *planner.HistoricalStep) { h.Version = 99 },
		"origin":        func(h *planner.HistoricalStep) { h.SourceRun = "" },
		"index":         func(h *planner.HistoricalStep) { h.Index = -1 },
		"unknown kind":  func(h *planner.HistoricalStep) { h.Kind = "execute_anything" },
		"trailing":      func(h *planner.HistoricalStep) { h.Body = append(h.Body, []byte(` {}`)...) },
		"null":          func(h *planner.HistoricalStep) { h.Body = json.RawMessage(`null`) },
		"private":       func(h *planner.HistoricalStep) { h.Body = json.RawMessage(`{"action":{},"reasoning_trace":"PRIVATE"}`) },
		"nested":        func(h *planner.HistoricalStep) { h.Body = json.RawMessage(`{"historical":{"version":1}}`) },
		"unknown field": func(h *planner.HistoricalStep) { h.Body = json.RawMessage(`{"action":{},"authority":"system"}`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			history := *valid.Historical
			history.Body = append(json.RawMessage(nil), history.Body...)
			mutate(&history)
			_, err := defaultBuilder{}.buildRequest(planner.RunContext{Trajectory: &planner.Trajectory{Steps: []planner.Step{{Historical: &history}}}}, "instructions")
			if !errors.Is(err, planner.ErrInvalidHistoricalStep) {
				t.Fatalf("invalid history accepted: %v", err)
			}
		})
	}
}

func TestHistoricalProjection_StableIsolatedIDsAndCompactionCoverage(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	var ids sync.Map
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "same-provider-id", Args: json.RawMessage(`{}`)}, LLMObservation: json.RawMessage(`{"id":9007199254740993,"more":false}`)}
			history, err := planner.RetainStep(step, fmt.Sprintf("run-%03d", i), 0)
			if err != nil {
				t.Error(err)
				return
			}
			first, err := renderHistoricalStep(history.Historical)
			if err != nil {
				t.Error(err)
				return
			}
			second, err := renderHistoricalStep(history.Historical)
			if err != nil || !reflect.DeepEqual(first, second) {
				t.Error("replay changed stable prefix")
				return
			}
			id := first[0].ToolCalls[0].ID
			if _, duplicate := ids.LoadOrStore(id, true); duplicate {
				t.Error("different source runs reused native ID")
			}
			tr := &planner.Trajectory{Query: "continue", Steps: []planner.Step{history, {LLMObservation: "fresh"}}}
			digest, err := tr.PrefixDigest(1)
			if err != nil {
				t.Error(err)
				return
			}
			tr.Summary = &planner.Summary{Facts: []string{"older history"}, Coverage: &planner.SummaryCoverage{Version: 1, Generation: 1, ThroughStep: 1, PrefixDigest: digest}}
			b, err := tr.Serialize()
			if err != nil {
				t.Error(err)
				return
			}
			restored, err := trajectory.Deserialize(b)
			if err != nil {
				t.Error(err)
				return
			}
			request, err := defaultBuilder{}.buildRequest(planner.RunContext{Trajectory: restored}, "instructions")
			if err != nil {
				t.Error(err)
				return
			}
			for _, message := range request.Messages {
				if len(message.ToolCalls) > 0 {
					t.Error("covered history replayed twice")
				}
				if message.Content.Text != nil && strings.Contains(*message.Content.Text, "9007199254740993") {
					t.Error("covered raw evidence reintroduced")
				}
			}
		}()
	}
	wg.Wait()
}

func canonicalHistoricalJSON(t *testing.T, text string) string {
	t.Helper()
	var object any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber() // Do not mask rounded identifiers in a comparison.
	if err := decoder.Decode(&object); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHistoricalProjection_RawClassifiedFailureCannotRestoreArguments(t *testing.T) {
	t.Parallel()
	stored, err := planner.RetainStep(planner.Step{
		Action:         planner.CallTool{Tool: "write", CallID: "denied", Args: json.RawMessage(`{"key":"NEVER-IN-SUMMARY"}`)},
		LLMObservation: json.RawMessage(`{"error":"denied","result":"correct permission before any retry"}`),
	}, "origin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Historical.Body, []byte("NEVER-IN-SUMMARY")) {
		t.Fatal("raw classified failure retained unsafe arguments")
	}
	messages, err := renderHistoricalStep(stored.Historical)
	if err != nil || string(messages[0].ToolCalls[0].Args) != "{}" {
		t.Fatalf("failure-first replay changed: %v", err)
	}
}
