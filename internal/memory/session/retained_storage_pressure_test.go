package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

// Compaction bounds model input, not the size of exact persisted evidence.
// Crossing the former 512 KiB storage ceiling must neither fail nor drop receipts.
func TestRetainedCumulative_LargeReceiptsExceedLegacyByteLimitAfterCompaction(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			sessionID := string(state.NewEventID())
			recorder := &retainedSummaryRecorder{}
			compactor := planner.NewCompressionRunner(recorder)
			for turn := 1; turn <= 12; turn++ {
				base := retainedBase(fmt.Sprintf("turn-%d", turn), sessionID)
				run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 8, time.Hour, nil)
				if err != nil {
					t.Fatalf("turn %d admission: %v", turn, err)
				}
				if err := run.Apply(&base); err != nil {
					t.Fatal(err)
				}
				base.Budget.TokenBudget = 1
				if err := compactor.MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
					t.Fatalf("turn %d compaction: %v", turn, err)
				}
				base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{
					Action: planner.CallTool{Tool: "read", CallID: fmt.Sprintf("read-%d", turn)},
					LLMObservation: map[string]any{
						"source":      strings.Repeat("x", 64*1024),
						"resource_id": fmt.Sprintf("document-%d", turn),
						"version":     "9007199254740993127", "more": false,
					},
				})
				if err := run.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
					t.Fatalf("turn %d terminal write after %d successful summaries: %v", turn, len(recorder.inputs), err)
				}
			}
			if len(recorder.inputs) < 5 {
				t.Fatalf("only %d summaries; fixture did not exercise repeated compaction", len(recorder.inputs))
			}
			base := retainedBase("next", sessionID)
			record := loadHostRecord(t, store, identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if len(record.Bytes) <= 512*1024 {
				t.Fatal("fixture did not cross the former storage ceiling")
			}
			var window struct {
				Evidence []struct{ Steps []json.RawMessage } `json:"evidence"`
				Turns    []struct{ Steps []json.RawMessage } `json:"turns"`
			}
			if err := json.Unmarshal(record.Bytes, &window); err != nil {
				t.Fatal(err)
			}
			var steps []json.RawMessage
			for _, entry := range window.Evidence {
				steps = append(steps, entry.Steps...)
			}
			for _, entry := range window.Turns {
				steps = append(steps, entry.Steps...)
			}
			if len(steps) != 12 {
				t.Fatalf("exact receipts lost: %d", len(steps))
			}
			for i, raw := range steps {
				var historical planner.Step
				if err := json.Unmarshal(raw, &historical); err != nil {
					t.Fatal(err)
				}
				step, err := planner.ReadHistoricalStep(historical)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(step.LLMObservation)
				if err != nil {
					t.Fatal(err)
				}
				var source string
				var got map[string]json.RawMessage
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(got["source"], &source); err != nil {
					t.Fatal(err)
				}
				if string(got["resource_id"]) != fmt.Sprintf("\"document-%d\"", i+1) || string(got["version"]) != `"9007199254740993127"` || string(got["more"]) != "false" || source != strings.Repeat("x", 64*1024) {
					t.Fatalf("receipt %d changed", i+1)
				}
			}
			run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 8, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Apply(&base); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainedJournal_LargeEvidenceSurvivesExplicitRecovery(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("source", string(state.NewEventID()))
			base.Query = "query:" + strings.Repeat("q", 512*1024+1)
			run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 8, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err := run.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			preamble := "preamble:" + strings.Repeat("p", 512*1024+1)
			step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "large-read", Args: json.RawMessage(`{}`)}, AssistantPreamble: preamble}
			if err := run.BeforeDispatch(t.Context(), base, step); err != nil {
				t.Fatal(err)
			}
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 8, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnsettled) {
				t.Fatalf("large pending intent no longer unknown: %v", err)
			}
			receipt := "receipt:" + strings.Repeat("r", 512*1024+1)
			step.LLMObservation = map[string]any{"source": receipt, "version": json.Number("9007199254740993127"), "more": false}
			if err := run.AfterDispatch(t.Context(), base, step); err != nil {
				t.Fatal(err)
			}
			correction := "steering:" + strings.Repeat("s", 512*1024+1)
			if err := run.RecordContext(t.Context(), base, planner.Step{LLMObservation: correction}); err != nil {
				t.Fatal(err)
			}
			if driver == "sqlite" {
				if err := store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 8, nil); err != nil {
				t.Fatal(err)
			}
			next := retainedBase("next", base.Quadruple.SessionID)
			restored, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 8, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.Apply(&next); err != nil {
				t.Fatal(err)
			}
			body := encodeRetained(t, next)
			for _, exact := range []string{base.Query, preamble, receipt, correction, `"version":9007199254740993127`, `"more":false`, `"historical_run_outcome":"interrupted"`} {
				if !strings.Contains(body, exact) {
					t.Fatal("recovery changed exact large evidence")
				}
			}
			for _, entry := range next.Trajectory.Steps {
				if entry.Action != nil {
					t.Fatal("recovery created executable history")
				}
			}
		})
	}
}

func TestSessionInspection_LargeNoteIsNotClipped(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			base := retainedBase("unused", string(state.NewEventID()))
			id := identity.Quadruple{Identity: base.Quadruple.Identity}
			query, answer := strings.Repeat("q", 512*1024+1), strings.Repeat("a", 512*1024+1)
			key, err := sessionmemory.Put(t.Context(), store, redactor, id, query, answer, 8, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			view, err := sessionmemory.Inspect(t.Context(), store, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Items) != 1 || view.Items[0].Key != key {
				t.Fatal("large note missing")
			}
			var got struct{ Query, Answer string }
			if err := json.Unmarshal(view.Items[0].Value, &got); err != nil {
				t.Fatal(err)
			}
			if got.Query != query || got.Answer != answer {
				t.Fatal("large note clipped")
			}
		})
	}
}
