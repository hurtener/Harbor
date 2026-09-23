package session_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

func TestRetainedCumulative_EvidenceExceedsLegacyTurnCount(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			sessionID := string(state.NewEventID())
			compactor := planner.NewCompressionRunner(&retainedSummaryRecorder{})
			for turn := range 260 {
				base := retainedBase(fmt.Sprintf("turn-%d", turn), sessionID)
				r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatalf("admission %d: %v", turn, err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				base.Budget.TokenBudget = 1
				if err := compactor.MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
					t.Fatal(err)
				}
				base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: fmt.Sprintf("exact-receipt-%d", turn)})
				if err := r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
					t.Fatalf("terminal %d: %v", turn, err)
				}
			}
			q := identity.Quadruple{Identity: retainedBase("inspect", sessionID).Quadruple.Identity}
			record := loadHostRecord(t, store, q, retainedKind)
			var window struct {
				Evidence []struct{ Steps []json.RawMessage } `json:"evidence"`
				Turns    []struct{ Steps []json.RawMessage } `json:"turns"`
			}
			if err := json.Unmarshal(record.Bytes, &window); err != nil {
				t.Fatal(err)
			}
			if len(window.Evidence) != 259 || len(window.Turns) != 1 {
				t.Fatalf("evidence silently lost: older=%d recent=%d", len(window.Evidence), len(window.Turns))
			}
			for index, entry := range window.Evidence {
				if len(entry.Steps) != 1 {
					t.Fatal("receipt count changed")
				}
				var retained planner.Step
				if err := json.Unmarshal(entry.Steps[0], &retained); err != nil {
					t.Fatal(err)
				}
				step, err := planner.ReadHistoricalStep(retained)
				if err != nil || step.LLMObservation != fmt.Sprintf("exact-receipt-%d", index) {
					t.Fatalf("receipt %d changed: %v", index, err)
				}
			}
		})
	}
}

func TestRetainedJournal_ExceedsLegacyStepCount(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		for _, recoverRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovery=%t", driver, recoverRun), func(t *testing.T) {
				store, redactor, _ := retainedStore(t, driver)
				base := retainedBase("long-run", string(state.NewEventID()))
				r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 100, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := r.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				for i := range 300 {
					step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: fmt.Sprint("call-", i), Args: json.RawMessage(`{}`)}}
					if err := r.BeforeDispatch(t.Context(), base, step); err != nil {
						t.Fatalf("intent %d: %v", i, err)
					}
					step.LLMObservation = json.RawMessage(`{"version":9007199254740993127,"more":false}`)
					if err := r.AfterDispatch(t.Context(), base, step); err != nil {
						t.Fatalf("settlement %d: %v", i, err)
					}
					base.Trajectory.Steps = append(base.Trajectory.Steps, step)
				}
				if recoverRun {
					err = sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 100, nil)
				} else {
					err = r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete")
				}
				if err != nil {
					t.Fatalf("terminal publication: %v", err)
				}
				next := retainedBase("next", base.Quadruple.SessionID)
				r, err = sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 100, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&next); err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, step := range next.Trajectory.Steps {
					if step.Historical != nil {
						count++
					}
				}
				if count != 300 {
					t.Fatalf("retained %d steps, want 300", count)
				}
			})
		}
	}
}

func TestRetainedContext_ConfiguredRecentWindowAboveThirtyTwo(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			sessionID := string(state.NewEventID())
			for i := range 40 {
				base := retainedBase(fmt.Sprint("run-", i), sessionID)
				r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 40, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := r.Finish(t.Context(), base.Trajectory, base.Query, fmt.Sprint("answer-", i), "complete"); err != nil {
					t.Fatal(err)
				}
			}
			base := retainedBase("next", sessionID)
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 40, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if len(base.Trajectory.Steps) != 81 || !r.CompactionRequired() {
				t.Fatal("configured detail window was clipped or rollover not requested")
			}
		})
	}
}
