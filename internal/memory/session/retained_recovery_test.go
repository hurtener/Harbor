package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

func journalAction(t *testing.T, r *sessionmemory.RetainedRun, base planner.RunContext, settled bool) planner.Step {
	t.Helper()
	step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "save-one", Args: json.RawMessage(`{"id":"doc-a"}`)}}
	if err := r.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"id":"doc-a","version":9007199254740993,"more":false}`)
	if settled {
		if err := r.AfterDispatch(t.Context(), base, step); err != nil {
			t.Fatal(err)
		}
	}
	return step
}

func TestRetainedRecovery_SettledRestartFencesSource(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("source", "recovery")
			old, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := old.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err := old.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := journalAction(t, old, base, true)
			base.Trajectory.Steps = append(base.Trajectory.Steps, step)
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
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
				t.Fatal(err)
			}
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
				t.Fatalf("idempotent recovery: %v", err)
			}
			next := retainedBase("next", "recovery")
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&next); err != nil {
				t.Fatal(err)
			}
			body := encodeRetained(t, next)
			for _, want := range []string{"9007199254740993", "doc-a", `"more":false`, `"historical_run_outcome":"interrupted"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("recovery lost %s", want)
				}
			}
			for _, entry := range next.Trajectory.Steps {
				if entry.Action != nil {
					t.Fatal("recovery created executable history")
				}
			}
			for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000"} {
				if _, err := store.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("journal was not cleaned: %v", err)
				}
			}
			if driver == "inmem" {
				if err := old.BeforeDispatch(t.Context(), base, step); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("old execution was not fenced: %v", err)
				}
			}
		})
	}
}

func TestRetainedRecovery_PendingNeverBecomesFailedOrReplayable(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("pending", "s")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	journalAction(t, r, base, false)
	before, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnsettled) {
		t.Fatalf("pending action adopted: %v", err)
	}
	after, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
	if err != nil || after.ID != before.ID || string(after.Bytes) != string(before.Bytes) {
		t.Fatal("pending action changed on refusal")
	}
}

func TestRetainedRecovery_ExpiryAndIdentity(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	base := retainedBase("source", "s")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	journalAction(t, r, base, true)
	for _, q := range []identity.Quadruple{{}, {Identity: identity.Identity{TenantID: "other", UserID: "u", SessionID: "s"}, RunID: "source"}, {Identity: identity.Identity{TenantID: "t", UserID: "other", SessionID: "s"}, RunID: "source"}, {Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "other"}, RunID: "source"}} {
		if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, q, 4, clock); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
			t.Fatalf("wrong identity accepted: %v", err)
		}
	}
	now = now.Add(30 * time.Second)
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, clock); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, clock); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
		t.Fatalf("source TTL extended: %v", err)
	}
	next := retainedBase("next", "s")
	newRun, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := newRun.Apply(&next); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encodeRetained(t, next), "doc-a") {
		t.Fatal("recovered evidence outlived its source")
	}
}

func TestRetainedRecovery_CompetesWithDispatchWithoutDoubleAuthority(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := retainedBase("source", fmt.Sprint("race-", i))
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := r.Apply(&base); err != nil {
				t.Error(err)
				return
			}
			if err := r.Start(t.Context(), base); err != nil {
				t.Error(err)
				return
			}
			start := make(chan struct{})
			recovered := make(chan error, 1)
			go func() {
				<-start
				recovered <- sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil)
			}()
			close(start)
			dispatchErr := r.BeforeDispatch(t.Context(), base, planner.Step{Action: planner.CallTool{Tool: "write", CallID: "new"}})
			recoverErr := <-recovered
			if (dispatchErr == nil) == (recoverErr == nil) {
				t.Errorf("expected exactly one generation winner: dispatch=%v recover=%v", dispatchErr, recoverErr)
			}
		}()
	}
	wg.Wait()
}

func TestRetainedRecovery_MissingCorruptOrErasedEvidenceFailsClosed(t *testing.T) {
	for _, scenario := range []string{"missing frame", "byte mismatch", "frame not settled", "frame wrong index", "private body", "erased", "expired during seal"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			now := time.Now()
			clock := func() time.Time { return now }
			base := retainedBase("source", scenario)
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err := r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			journalAction(t, r, base, true)
			head, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing frame":
				if _, err := store.DeleteIf(t.Context(), state.InternalSlotExpectation(base.Quadruple, frame.Kind, frame.ID)); err != nil {
					t.Fatal(err)
				}
			case "erased":
				if _, err := store.DeleteScope(t.Context(), base.Quadruple.Identity); err != nil {
					t.Fatal(err)
				}
			case "expired during seal":
				redactor = &recoveryRedactor{fn: func(_ context.Context, value any) (any, error) { now = now.Add(2 * time.Hour); return value, nil }}
			default:
				var h, f map[string]json.RawMessage
				if err := json.Unmarshal(head.Bytes, &h); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(frame.Bytes, &f); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "byte mismatch":
					h["bytes"] = json.RawMessage(`1`)
				case "frame not settled":
					f["settled"] = json.RawMessage(`false`)
				case "frame wrong index":
					f["index"] = json.RawMessage(`1`)
				case "private body":
					f["step"] = json.RawMessage(`{"action":{"Tool":"save"},"reasoning_trace":"private"}`)
				}
				fb, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "byte mismatch" {
					var query string
					if err := json.Unmarshal(h["query"], &query); err != nil {
						t.Fatal(err)
					}
					qb, err := json.Marshal(query)
					if err != nil {
						t.Fatal(err)
					}
					h["bytes"] = json.RawMessage(fmt.Sprint(len(qb) + len(fb)))
				}
				hb, err := json.Marshal(h)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveBatchIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(base.Quadruple, head.Kind, head.ID), state.InternalSlotExpectation(base.Quadruple, frame.Kind, frame.ID)}, []state.StateRecord{state.NewInternalRecord(state.NewEventID(), base.Quadruple, head.Kind, hb), state.NewInternalRecord(state.NewEventID(), base.Quadruple, frame.Kind, fb)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, clock); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
				t.Fatalf("invalid source accepted: %v", err)
			}
			window, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if scenario == "erased" {
				if !errors.Is(err, state.ErrNotFound) {
					t.Fatal("erased context resurrected")
				}
			} else if err != nil || strings.Contains(string(window.Bytes), `"status":"interrupted"`) {
				t.Fatal("invalid recovery published terminal evidence")
			}
		})
	}
}

type recoveryRedactor struct {
	fn func(context.Context, any) (any, error)
}

func (r *recoveryRedactor) Redact(ctx context.Context, value any) (any, error) {
	return r.fn(ctx, value)
}

type recoveryFaultStore struct {
	state.StateStore
	failSeal   bool
	failDelete bool
	deletes    int
}

func (s *recoveryFaultStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	if s.failSeal {
		return errors.New("injected seal failure")
	}
	return s.StateStore.SaveBatchIf(ctx, checks, records)
}
func (s *recoveryFaultStore) DeleteIf(ctx context.Context, check state.SlotExpectation) (bool, error) {
	s.deletes++
	if s.failDelete && s.deletes == 2 {
		return false, errors.New("injected cleanup failure")
	}
	return s.StateStore.DeleteIf(ctx, check)
}

func TestRetainedRecovery_RequiredSealAndIdempotentCleanup(t *testing.T) {
	for _, scenario := range []string{"seal", "redaction", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			base := retainedBase("source", scenario)
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err := r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			journalAction(t, r, base, true)
			journalAction(t, r, base, true)
			fault := &recoveryFaultStore{StateStore: store, failSeal: scenario == "seal", failDelete: scenario == "cleanup"}
			chosen := redactor
			if scenario == "redaction" {
				chosen = retainedFailRedactor{}
			}
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), fault, chosen, base.Quadruple, 4, nil); err == nil {
				t.Fatal("required operation failure acknowledged")
			}
			window, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			if sealed := strings.Contains(string(window.Bytes), `"status":"interrupted"`); sealed != (scenario == "cleanup") {
				t.Fatal("seal is not atomic")
			}
			// Cleanup failure is not failed execution: the source remains fenced.
			if scenario == "cleanup" {
				if err := r.BeforeDispatch(t.Context(), base, planner.Step{Action: planner.CallTool{Tool: "write"}}); err == nil {
					t.Fatal("cleanup failure unfenced source")
				}
			}
			fault.failSeal, fault.failDelete = false, false
			if err := sessionmemory.ReconcileRetainedRun(t.Context(), fault, redactor, base.Quadruple, 4, nil); err != nil {
				t.Fatalf("safe retry: %v", err)
			}
			for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000", journalHeadKind + "/action/001"} {
				if _, err := store.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatal("cleanup did not finish")
				}
			}
		})
	}
}

func TestRetainedRecovery_CannotSucceedByImmediatelyEvictingSource(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("old", "capacity")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	journalAction(t, r, base, true)
	newer := retainedBase("newer", "capacity")
	sibling, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, newer.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sibling.Apply(&newer); err != nil {
		t.Fatal(err)
	}
	if err := sibling.Finish(t.Context(), newer.Trajectory, newer.Query, "done", "complete"); err != nil {
		t.Fatal(err)
	}
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, nil); !errors.Is(err, sessionmemory.ErrRetainedContextCapacity) {
		t.Fatalf("missing source acknowledged: %v", err)
	}
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedRecovery_ConcurrentReconciliationPublishesOnce(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("source", "same-admission")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	journalAction(t, r, base, true)
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil)
			// A reader racing exact-generation cleanup can observe an unavailable
			// snapshot. It must not insert a second turn or adopt another source.
			if err != nil && !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
				t.Errorf("unexpected recovery error: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
		t.Fatal(err)
	}
	window, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(window.Bytes), `"status":"interrupted"`) != 1 {
		t.Fatal("reconciliation published duplicate turns")
	}
	base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: "late owner"})
	if err := r.Finish(t.Context(), base.Trajectory, base.Query, "late success", "complete"); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
		t.Fatal("late owner overwrote fenced outcome")
	}
}
