package session_test

import (
	"fmt"
	"testing"
	"time"

	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/state"
)

// In a session several people take part in, each retained request keeps
// its writer's label, and the next turn's model context names who asked
// what — including the current request.
func TestRetainedContext_SpeakerLabels(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	session := string(state.NewEventID())
	first := retainedBase("first", session)
	first.Speaker = "Ana"
	run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, first.Quadruple, 1, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Apply(&first); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(t.Context(), first.Trajectory, first.Query, "done", "complete"); err != nil {
		t.Fatal(err)
	}

	second := retainedBase("second", session)
	second.Speaker = "Bruno"
	next, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, second.Quadruple, 1, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Apply(&second); err != nil {
		t.Fatal(err)
	}
	var historical, current string
	for _, step := range second.Trajectory.Steps {
		obs, ok := step.LLMObservation.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := obs["historical_user_request"]; ok {
			historical = fmt.Sprint(obs["speaker"])
		}
		if _, ok := obs["current_request"]; ok {
			current = fmt.Sprint(obs["speaker"])
		}
	}
	if historical != "Ana" || current != "Bruno" {
		t.Fatalf("speaker labels: historical %q current %q, want Ana / Bruno", historical, current)
	}
}
