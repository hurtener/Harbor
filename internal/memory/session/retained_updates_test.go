package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

func TestRetainedUpdates_ContextFrameRecovery(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("steered", "context-frames")
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := planner.Step{LLMObservation: json.RawMessage(`{"applied_steering":"INJECT_CONTEXT","content":{"version":9007199254740993,"constraint":"Keep approved navigation"}}`)}
			if err = r.RecordContext(t.Context(), base, step); err != nil {
				t.Fatal(err)
			}
			if driver == "sqlite" {
				if err = store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			// The old live trajectory never received the context step: recovery
			// must use its committed journal, not process-local state.
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
				t.Fatal(err)
			}
			next := retainedBase("next", "context-frames")
			recovered, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = recovered.Apply(&next); err != nil {
				t.Fatal(err)
			}
			encoded := encodeRetained(t, next)
			if !strings.Contains(encoded, "9007199254740993") || !strings.Contains(encoded, "Keep approved navigation") {
				t.Fatal("context frame lost on recovery")
			}
			for _, step := range next.Trajectory.Steps {
				if step.Action != nil {
					t.Fatal("context became an executable decision")
				}
			}
		})
	}
}

func TestRetainedUpdates_RefusesInvalidOrPendingContext(t *testing.T) {
	for _, scenario := range []string{"pending", "action", "historical", "private", "empty", "oversized", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			base := retainedBase("run", scenario)
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := planner.Step{LLMObservation: "correction"}
			ctx := t.Context()
			switch scenario {
			case "pending":
				err = r.BeforeDispatch(ctx, base, planner.Step{Action: planner.CallTool{Tool: "write"}})
				if err != nil {
					t.Fatal(err)
				}
			case "action":
				step.Action = planner.CallTool{Tool: "write"}
			case "historical":
				step.Historical = &planner.HistoricalStep{Version: 1}
			case "private":
				step.ReasoningTrace = "private"
			case "empty":
				step.LLMObservation = nil
			case "oversized":
				step.LLMObservation = strings.Repeat("x", 512*1024)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := r.RecordContext(ctx, base, step); err == nil {
				t.Fatal("invalid context admitted")
			}
			if err := r.Finish(t.Context(), base.Trajectory, "query", "complete", "complete"); err == nil {
				t.Fatal("required context failure became successful terminal retention")
			}
		})
	}
}

func TestRetainedUpdates_ContextFrameCannotHideAnAction(t *testing.T) {
	for _, contextFrame := range []bool{false, true} {
		store, redactor, _ := retainedStore(t, "inmem")
		base := retainedBase("run", "corrupt-frame")
		r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Apply(&base); err != nil {
			t.Fatal(err)
		}
		if err = r.Start(t.Context(), base); err != nil {
			t.Fatal(err)
		}
		if err = r.RecordContext(t.Context(), base, planner.Step{LLMObservation: "context"}); err != nil {
			t.Fatal(err)
		}
		record, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000")
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]json.RawMessage
		if err = json.Unmarshal(record.Bytes, &value); err != nil {
			t.Fatal(err)
		}
		value["context"] = json.RawMessage(`false`)
		if contextFrame {
			value["context"] = json.RawMessage(`true`)
			value["step"] = json.RawMessage(`{"action":{"Tool":"write"},"llm_observation":"context"}`)
		}
		record.Bytes, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(base.Quadruple, record.Kind, record.ID)}, state.NewInternalRecord(state.NewEventID(), base.Quadruple, record.Kind, record.Bytes)); err != nil {
			t.Fatal(err)
		}
		if err = sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
			t.Fatalf("ambiguous frame accepted: %v", err)
		}
	}
}
