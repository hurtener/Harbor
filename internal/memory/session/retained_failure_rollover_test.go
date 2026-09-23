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

// A failed compactor must not leave a settled run permanently active merely
// because the recent-detail target is full. Recovery never runs a tool/model.
func TestRetainedCumulative_FailureCanSettleBeforeNextCompaction(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, mode := range []string{"interrupted", "cancelled", "explicit recovery"} {
			t.Run(driver+"/"+mode, func(t *testing.T) {
				store, redactor, cfg := retainedStore(t, driver)
				admit := func(run string) (*sessionmemory.RetainedRun, planner.RunContext) {
					t.Helper()
					base := retainedBase(run, "failure-rollover")
					r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := r.Apply(&base); err != nil {
						t.Fatal(err)
					}
					if err := r.Start(t.Context(), base); err != nil {
						t.Fatal(err)
					}
					return r, base
				}
				seed, base := admit("seed")
				step := journalAction(t, seed, base, true)
				base.Trajectory.Steps = append(base.Trajectory.Steps, step)
				if err := seed.Finish(t.Context(), base.Trajectory, base.Query, "ORIGINAL-CONSTRAINT", "complete"); err != nil {
					t.Fatal(err)
				}
				failed, interrupted := admit("failed")
				q := identity.Quadruple{Identity: base.Quadruple.Identity}
				before := loadHostRecord(t, store, q, retainedKind)
				if mode == "explicit recovery" {
					if err := failed.Finish(t.Context(), interrupted.Trajectory, interrupted.Query, "unsaved answer", "complete"); !errors.Is(err, sessionmemory.ErrRetainedContextCapacity) {
						t.Fatalf("expected original refusal: %v", err)
					}
					if after := loadHostRecord(t, store, q, retainedKind); string(after.Bytes) != string(before.Bytes) {
						t.Fatal("failed publication changed committed state")
					}
					if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, interrupted.Quadruple, 1, nil); err != nil {
						t.Fatalf("settled recovery deadlocked behind full window: %v", err)
					}
				} else if err := failed.Finish(t.Context(), interrupted.Trajectory, interrupted.Query, "", mode); err != nil {
					t.Fatalf("interrupted terminal record deadlocked behind full window: %v", err)
				}
				committed := loadHostRecord(t, store, q, retainedKind)
				var window struct {
					Active []json.RawMessage `json:"active"`
					Turns  []json.RawMessage `json:"turns"`
				}
				if err := json.Unmarshal(committed.Bytes, &window); err != nil {
					t.Fatal(err)
				}
				if len(window.Active) != 0 || len(window.Turns) != 2 || !strings.Contains(string(committed.Bytes), "ORIGINAL-CONSTRAINT") {
					t.Fatal("settlement dropped prior detail or left a phantom active run")
				}
				if _, err := store.Load(t.Context(), interrupted.Quadruple, journalHeadKind); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("settled journal not cleaned: %v", err)
				}
				if err := failed.BeforeDispatch(t.Context(), interrupted, step); err == nil {
					t.Fatal("settled source can dispatch again")
				}
				if driver == "sqlite" {
					if err := store.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					var err error
					store, err = state.Open(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = store.Close(context.Background()) }()
				}
				next, resumed := admit("resumed")
				if !next.CompactionRequired() {
					t.Fatal("preserved overflow did not request compaction")
				}
				recorder := &retainedSummaryRecorder{}
				resumed.Budget.TokenBudget = 1
				if err := planner.NewCompressionRunner(recorder).MaybeCompress(t.Context(), resumed, resumed.Trajectory); err != nil {
					t.Fatal(err)
				}
				if len(recorder.inputs) != 1 {
					t.Fatal("expected one compaction")
				}
				input, err := json.Marshal(recorder.inputs[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"ORIGINAL-CONSTRAINT", "9007199254740993", `"more":false`, "failed"} {
					if !strings.Contains(string(input), want) {
						t.Fatalf("compaction lost %s", want)
					}
				}
				if err := next.Finish(t.Context(), resumed.Trajectory, resumed.Query, "recovered", "complete"); err != nil {
					t.Fatalf("successful compaction still cannot settle: %v", err)
				}
				committed = loadHostRecord(t, store, q, retainedKind)
				window.Active, window.Turns = nil, nil
				if err := json.Unmarshal(committed.Bytes, &window); err != nil {
					t.Fatal(err)
				}
				if len(window.Active) != 0 || len(window.Turns) != 1 || !strings.Contains(string(committed.Bytes), "9007199254740993") {
					t.Fatal("rollover failed to restore target or preserve exact evidence")
				}
			})
		}
	}
}

func TestRetainedCumulative_RepeatedInterruptionsRemainLoadable(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			for i := range 34 { // Cross the retired hardcoded 32-turn ceiling.
				base := retainedBase(fmt.Sprintf("failed-%d", i), "repeated-interruptions")
				r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := r.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				if err := r.Finish(t.Context(), base.Trajectory, base.Query, "", "interrupted"); err != nil {
					t.Fatalf("interruption %d cannot settle: %v", i, err)
				}
			}
			base := retainedBase("healthy", "repeated-interruptions")
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			base.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
				t.Fatal(err)
			}
			if err := r.Finish(t.Context(), base.Trajectory, base.Query, "healthy", "complete"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
