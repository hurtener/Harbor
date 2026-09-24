package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
)

type terminalShapeRedactor struct {
	shape string
}

func (r terminalShapeRedactor) Redact(_ context.Context, value any) (any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}
	steps, ok := object["steps"].([]any)
	if !ok || len(steps) == 0 {
		return value, nil
	}
	outer, ok := steps[0].(map[string]any)
	if !ok {
		return value, nil
	}
	historical, ok := outer["historical"].(map[string]any)
	if !ok {
		return value, nil
	}
	body, ok := historical["body"].(map[string]any)
	if !ok {
		return value, nil
	}
	action, ok := body["action"].(map[string]any)
	if !ok {
		return value, nil
	}
	switch r.shape {
	case "tool":
		action["Tool"] = "different"
	case "call id":
		action["CallID"] = "different-call"
	case "envelope source":
		historical["source_run"] = "different-run"
	case "envelope kind":
		historical["kind"] = "context"
	case "missing historical":
		delete(outer, "historical")
	case "missing action":
		delete(body, "action")
	case "invalid body":
		body["reasoning_trace"] = "PRIVATE-TRACE"
	case "parallel branches":
		branches, _ := action["Branches"].([]any)
		action["Branches"] = branches[:1]
	case "batch branches":
		tools, _ := action["Tools"].([]any)
		action["Tools"] = tools[:1]
	case "control authority":
		action["TaskID"] = "foreign-task"
	case "content":
		action["Args"] = map[string]any{"redacted": true}
		body["llm_observation"] = "[redacted result]"
		object["query"] = "[redacted query]"
		object["answer"] = "[redacted answer]"
	}
	return object, nil
}

func TestRetainedContext_TerminalRedactorCannotChangeActionIdentity(t *testing.T) {
	parallel := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "read-a", CallID: "parallel-a", Args: json.RawMessage(`{"id":"a"}`)},
			{Tool: "read-b", CallID: "parallel-b", Args: json.RawMessage(`{"id":"b"}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}
	batch, err := planner.NewBatch(
		[]planner.CallTool{
			{Tool: "read-a", CallID: "batch-a", Args: json.RawMessage(`{"id":"a"}`)},
			{Tool: "read-b", CallID: "batch-b", Args: json.RawMessage(`{"id":"b"}`)},
		},
		nil,
		&planner.JoinSpec{Kind: planner.JoinAll},
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		shape  string
		action any
	}{
		{name: "tool", shape: "tool", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "call-id", shape: "call id", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "envelope-source", shape: "envelope source", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "envelope-kind", shape: "envelope kind", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "missing-historical", shape: "missing historical", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "missing-action", shape: "missing action", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "invalid-body", shape: "invalid body", action: planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"value"}`)}},
		{name: "parallel-branches", shape: "parallel branches", action: parallel},
		{name: "batch-branches", shape: "batch branches", action: batch},
		{name: "control-authority", shape: "control authority", action: planner.CancelTask{TaskID: "owned-task", Reason: "no longer needed"}},
	}
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, tc := range cases {
			t.Run(driver+"/"+tc.name, func(t *testing.T) {
				store, _, _ := retainedStore(t, driver)
				base := retainedBase("source", "terminal-redactor-"+tc.name)
				run, err := sessionmemory.BeginRetainedRun(t.Context(), store, terminalShapeRedactor{shape: tc.shape}, base.Quadruple, 2, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = run.Apply(&base); err != nil {
					t.Fatal(err)
				}
				base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: tc.action, LLMObservation: "settled result"})
				if err = run.Finish(t.Context(), base.Trajectory, "request", "answer", "complete"); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("terminal action mutation accepted: %v", err)
				}
				record, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
				if err != nil {
					t.Fatal(err)
				}
				var window struct {
					Turns []json.RawMessage `json:"turns"`
				}
				if err := json.Unmarshal(record.Bytes, &window); err != nil {
					t.Fatal(err)
				}
				if len(window.Turns) != 0 {
					t.Fatal("rejected terminal action was persisted")
				}
			})
		}
	}
}

func TestRetainedContext_TerminalRedactorAllowsContentRedaction(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, _, _ := retainedStore(t, driver)
			base := retainedBase("source", "terminal-content-redaction")
			run, err := sessionmemory.BeginRetainedRun(t.Context(), store, terminalShapeRedactor{shape: "content"}, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = run.Apply(&base); err != nil {
				t.Fatal(err)
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{
				Action:         planner.CallTool{Tool: "read", CallID: "call-exact", Args: json.RawMessage(`{"secret":"SECRET-ARG"}`)},
				LLMObservation: "SECRET-RESULT",
			})
			if err = run.Finish(t.Context(), base.Trajectory, "SECRET-QUERY", "SECRET-ANSWER", "complete"); err != nil {
				t.Fatalf("content redaction rejected: %v", err)
			}
			record, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range [][]byte{[]byte(`"Tool":"read"`), []byte(`"CallID":"call-exact"`), []byte(`"redacted":true`), []byte("[redacted result]"), []byte("[redacted query]"), []byte("[redacted answer]")} {
				if !bytes.Contains(record.Bytes, want) {
					t.Fatalf("retained terminal record omitted %q", want)
				}
			}
			if bytes.Contains(record.Bytes, []byte("SECRET")) {
				t.Fatal("retained terminal record kept unredacted content")
			}
		})
	}
}
