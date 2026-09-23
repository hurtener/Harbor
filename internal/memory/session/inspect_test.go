package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

type inspectionFailSave struct{ state.StateStore }

func (inspectionFailSave) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return errors.New("injected inspection mutation failure")
}

type inspectionBeforeSave struct {
	state.StateStore
	once   sync.Once
	before func()
}

func (s *inspectionBeforeSave) SaveIf(ctx context.Context, expectations []state.SlotExpectation, record state.StateRecord) error {
	s.once.Do(s.before)
	return s.StateStore.SaveIf(ctx, expectations, record)
}

func TestSessionInspection_DeletePreservesConcurrentTail(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor := inspectionStore(t, driver)
			other := store
			if driver == "postgres" {
				// The competing publication uses an independently opened pool.
				other, _ = inspectionStore(t, driver)
			}
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: string(state.NewEventID())}}
			victim, err := sessionmemory.Put(t.Context(), store, redactor, id, "remove", "", 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			var survivor string
			competing := &inspectionBeforeSave{StateStore: store, before: func() {
				var err error
				survivor, err = sessionmemory.Put(t.Context(), other, redactor, id, "concurrent survivor", "", 4, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
			}}
			count, err := sessionmemory.Delete(t.Context(), competing, id, victim, nil)
			if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			view, err := sessionmemory.Inspect(t.Context(), other, id, nil)
			if err != nil || len(view.Items) != 1 || view.Items[0].Key != survivor {
				t.Fatalf("concurrent publication lost: %+v, %v", view, err)
			}
		})
	}
}

type inspectionRedactor func(context.Context, any) (any, error)

func (r inspectionRedactor) Redact(ctx context.Context, value any) (any, error) { return r(ctx, value) }

func TestSessionInspection_NoteRedactorAndCancellation(t *testing.T) {
	store, redactor := inspectionStore(t, "inmem")
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "redactor"}}
	for _, shape := range []string{`null`, `{}`, `{"query":null,"answer":""}`, `{"query":"q","answer":false}`, `{"query":"q","answer":"","expires_at":"2099-01-01"}`, `{"query":"q","query":"duplicate","answer":""}`} {
		t.Run(shape, func(t *testing.T) {
			r := inspectionRedactor(func(context.Context, any) (any, error) { return json.RawMessage(shape), nil })
			if _, err := sessionmemory.Put(t.Context(), store, r, id, "query", "answer", 4, time.Hour, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
				t.Fatalf("malformed redaction admitted: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sessionmemory.Put(ctx, store, redactor, id, "query", "", 4, time.Hour, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := sessionmemory.Delete(ctx, store, id, "key", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := sessionmemory.Inspect(ctx, store, id, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	view, err := sessionmemory.Inspect(t.Context(), store, id, nil)
	if err != nil || len(view.Items) != 0 || view.EstimatedTokens != 0 {
		t.Fatalf("rejected mutations published: %+v %v", view, err)
	}
	r := inspectionRedactor(func(_ context.Context, value any) (any, error) {
		v := value.(map[string]any)
		if len(v) != 2 {
			t.Fatal("redactor received host metadata")
		}
		v["query"] = "redacted"
		return v, nil
	})
	if _, err := sessionmemory.Put(t.Context(), store, r, id, "secret", "", 4, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	view, err = sessionmemory.Inspect(t.Context(), store, id, nil)
	if err != nil || len(view.Items) != 1 || bytes.Contains(view.Items[0].Value, []byte("secret")) || !bytes.Contains(view.Items[0].Value, []byte("redacted")) {
		t.Fatalf("redaction not applied: %+v %v", view, err)
	}
}

func TestSessionInspection_ConcurrentIdentityIsolation(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor := inspectionStore(t, driver)
			const n = 128
			errs := make(chan error, n)
			var wg sync.WaitGroup
			prefix := string(state.NewEventID())
			for i := range n {
				wg.Go(func() {
					id := identity.Quadruple{Identity: identity.Identity{TenantID: fmt.Sprintf("t%d", i%4), UserID: fmt.Sprintf("u%d", i/4%4), SessionID: fmt.Sprintf("%s-%d", prefix, i/16)}}
					note := fmt.Sprintf("private-note-%d", i)
					key, err := sessionmemory.Put(t.Context(), store, redactor, id, note, "", 4, time.Hour, nil)
					if err != nil {
						errs <- err
						return
					}
					view, err := sessionmemory.Inspect(t.Context(), store, id, nil)
					if err != nil {
						errs <- err
						return
					}
					if len(view.Items) != 1 || view.Items[0].Key != key || !bytes.Contains(view.Items[0].Value, []byte(`"query":"`+note+`"`)) {
						errs <- fmt.Errorf("identity %d saw wrong source", i)
						return
					}
					if _, err := sessionmemory.Delete(t.Context(), store, id, key, nil); err != nil {
						errs <- err
						return
					}
					view, err = sessionmemory.Inspect(t.Context(), store, id, nil)
					if err != nil {
						errs <- err
						return
					}
					if len(view.Items) != 0 {
						errs <- fmt.Errorf("identity %d deletion failed", i)
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

func inspectionStore(t *testing.T, driver string) (state.StateStore, audit.Redactor) {
	t.Helper()
	if driver != "postgres" {
		store, redactor, _ := retainedStore(t, driver)
		return store, redactor
	}
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_PG_DSN required for service-backed memory inspection")
	}
	store, err := state.Open(t.Context(), config.StateConfig{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	redactor, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return store, redactor
}

func TestSessionInspection_CumulativeDeletionFencesExecution(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		for _, duringInference := range []bool{false, true} {
			t.Run(driver+"/"+map[bool]string{false: "before", true: "during"}[duringInference], func(t *testing.T) {
				store, redactor := inspectionStore(t, driver)
				base := retainedBase("first", string(state.NewEventID()))
				first, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := first.Apply(&base); err != nil {
					t.Fatal(err)
				}
				base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "exact", Args: json.RawMessage(`{}`)}, LLMObservation: json.RawMessage(`{"version":9007199254740993,"more":false,"value":"ERASE-THIS"}`)})
				if err := first.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
					t.Fatal(err)
				}
				initial, err := sessionmemory.Inspect(t.Context(), store, base.Quadruple, nil)
				if err != nil || len(initial.Items) != 1 {
					t.Fatalf("initial=%+v err=%v", initial, err)
				}
				victim := initial.Items[0].Key
				secondBase := retainedBase("second", base.Quadruple.SessionID)
				second, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, secondBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := second.Apply(&secondBase); err != nil {
					t.Fatal(err)
				}
				secondBase.Budget.TokenBudget = 1
				if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), secondBase, secondBase.Trajectory); err != nil {
					t.Fatal(err)
				}
				if err := second.Finish(t.Context(), secondBase.Trajectory, secondBase.Query, "second done", "complete"); err != nil {
					t.Fatal(err)
				}
				view, err := sessionmemory.Inspect(t.Context(), store, secondBase.Quadruple, nil)
				if err != nil || view.Summary == "" || view.RecentTurns != 1 || len(view.Items) != 3 {
					t.Fatalf("rollover=%+v err=%v", view, err)
				}
				found := false
				for _, item := range view.Items {
					if item.Key == victim {
						found = bytes.Contains(item.Value, []byte(`9007199254740993`)) && bytes.Contains(item.Value, []byte(`"more":false`))
					}
				}
				if !found {
					t.Fatal("source key or exact evidence lost across rollover")
				}
				activeBase := retainedBase("active", base.Quadruple.SessionID)
				active, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, activeBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := active.Apply(&activeBase); err != nil {
					t.Fatal(err)
				}
				activeBase.Query = "PRIVATE-ACTIVE-QUERY"
				if err := active.Start(t.Context(), activeBase); err != nil {
					t.Fatal(err)
				}
				inspect, err := sessionmemory.Inspect(t.Context(), store, activeBase.Quadruple, nil)
				encoded, _ := json.Marshal(inspect)
				if err != nil || bytes.Contains(encoded, []byte(activeBase.Query)) {
					t.Fatal("inspection exposed an unsettled journal")
				}
				foreignBase := retainedBase("foreign", string(state.NewEventID()))
				foreign, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, foreignBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := foreign.Apply(&foreignBase); err != nil {
					t.Fatal(err)
				}
				if _, err := sessionmemory.Delete(t.Context(), store, foreignBase.Quadruple, victim, nil); !errors.Is(err, sessionmemory.ErrItemNotFound) {
					t.Fatalf("foreign delete=%v", err)
				}
				remove := func() {
					t.Helper()
					n, err := sessionmemory.Delete(t.Context(), store, activeBase.Quadruple, victim, nil)
					if err != nil || n != 1 {
						t.Fatalf("delete remaining=%d err=%v", n, err)
					}
				}
				called := false
				p := retainedDecisionFunc(func(context.Context, planner.RunContext) (planner.Decision, error) {
					called = true
					if duringInference {
						remove()
					}
					return planner.Finish{Reason: planner.FinishGoal}, nil
				})
				if !duringInference {
					remove()
				}
				decision, err := active.GuardPlanner(p, nil).Next(t.Context(), activeBase)
				if !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) || decision != nil || called != duringInference {
					t.Fatalf("stale decision=%v err=%v called=%v", decision, err, called)
				}
				if err := active.Finish(t.Context(), activeBase.Trajectory, activeBase.Query, "late", "complete"); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("late settlement=%v", err)
				}
				if err := sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, activeBase.Quadruple, 1, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("recovery restored a fenced journal: %v", err)
				}
				after, err := sessionmemory.Inspect(t.Context(), store, base.Quadruple, nil)
				if err != nil || after.Summary != "" || len(after.Items) != 1 || after.Items[0].Key == victim {
					t.Fatalf("after=%+v err=%v", after, err)
				}
				if err := foreign.Finish(t.Context(), foreignBase.Trajectory, foreignBase.Query, "foreign done", "complete"); err != nil {
					t.Fatalf("other session fenced: %v", err)
				}
			})
		}
	}
}

func TestSessionInspection_NoteCapacityExpiryAndFailurePreserveState(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor := inspectionStore(t, driver)
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: string(state.NewEventID())}}
			now := time.Now()
			clock := func() time.Time { return now }
			key, err := sessionmemory.Put(t.Context(), store, redactor, id, "first constraint", "recorded", 1, time.Hour, clock)
			if err != nil || key == "" {
				t.Fatal(err)
			}
			before := loadHostRecord(t, store, id, retainedKind)
			if _, err := sessionmemory.Put(t.Context(), store, redactor, id, "must not evict", "", 1, time.Hour, clock); !errors.Is(err, sessionmemory.ErrRetainedContextCapacity) {
				t.Fatalf("capacity=%v", err)
			}
			failed := inspectionFailSave{StateStore: store}
			if _, err := sessionmemory.Delete(t.Context(), failed, id, key, clock); err == nil {
				t.Fatal("failed persistence reported deletion")
			}
			after := loadHostRecord(t, store, id, retainedKind)
			if before.ID != after.ID || !bytes.Equal(before.Bytes, after.Bytes) {
				t.Fatal("failed operation changed committed memory")
			}
			view, err := sessionmemory.Inspect(t.Context(), store, id, clock)
			if err != nil || len(view.Items) != 1 || !strings.Contains(string(view.Items[0].Value), "first constraint") {
				t.Fatalf("view=%+v err=%v", view, err)
			}
			now = now.Add(2 * time.Hour)
			view, err = sessionmemory.Inspect(t.Context(), store, id, clock)
			if err != nil || len(view.Items) != 0 {
				t.Fatalf("expired view=%+v err=%v", view, err)
			}
			if _, err := sessionmemory.Delete(t.Context(), store, id, key, clock); !errors.Is(err, sessionmemory.ErrItemNotFound) {
				t.Fatalf("expired delete=%v", err)
			}
			after = loadHostRecord(t, store, id, retainedKind)
			if before.ID != after.ID {
				t.Fatal("inspection renewed or rewrote retention")
			}
		})
	}
}
