package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/state"
)

type terminalPreparationRedactor struct {
	audit.Redactor
	prepare func(context.Context) error
}

func (r terminalPreparationRedactor) Redact(ctx context.Context, value any) (any, error) {
	if object, ok := value.(map[string]any); ok && object["admission"] != nil {
		if err := r.prepare(ctx); err != nil {
			return nil, err
		}
	}
	return r.Redactor.Redact(ctx, value)
}

type terminalStageStore struct {
	state.StateStore
	observe func(context.Context)
}

func (s *terminalStageStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.observe != nil {
		s.observe(ctx)
	}
	return s.StateStore.Load(ctx, q, kind)
}

func (s *terminalStageStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	if s.observe != nil {
		s.observe(ctx)
	}
	return s.StateStore.SaveBatchIf(ctx, checks, records)
}

func (s *terminalStageStore) SaveIf(ctx context.Context, checks []state.SlotExpectation, record state.StateRecord) error {
	if s.observe != nil {
		s.observe(ctx)
	}
	return s.StateStore.SaveIf(ctx, checks, record)
}

func (s *terminalStageStore) DeleteIf(ctx context.Context, check state.SlotExpectation) (bool, error) {
	if s.observe != nil {
		s.observe(ctx)
	}
	return s.StateStore.DeleteIf(ctx, check)
}

func TestRetainedContext_TerminalStagesHaveIndependentBounds(t *testing.T) {
	type contextKey struct{}
	for _, parentBound := range []bool{false, true} {
		name := "independent"
		if parentBound {
			name = "earlier parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			observed := &terminalStageStore{StateStore: store}
			ctx := context.WithValue(t.Context(), contextKey{}, "preserved")
			var parentDeadline time.Time
			if parentBound {
				var cancel context.CancelFunc
				parentDeadline = time.Now().Add(4 * time.Second)
				ctx, cancel = context.WithDeadline(ctx, parentDeadline)
				defer cancel()
			}
			var prepareCtx, persistCtx context.Context
			var prepareDeadline time.Time
			redactor = terminalPreparationRedactor{Redactor: redactor, prepare: func(ctx context.Context) error {
				prepareCtx = ctx
				var ok bool
				prepareDeadline, ok = ctx.Deadline()
				if !ok || time.Until(prepareDeadline) > 5*time.Second || ctx.Value(contextKey{}) != "preserved" {
					t.Error("preparation lost its five-second bound or parent context")
				}
				return nil
			}}
			base := retainedBase("source", name)
			run, err := sessionmemory.BeginRetainedRun(ctx, observed, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = run.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = run.Start(ctx, base); err != nil {
				t.Fatal(err)
			}
			step := journalAction(t, run, base, true)
			base.Trajectory.Steps = append(base.Trajectory.Steps, step)
			calls := 0
			observed.observe = func(ctx context.Context) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second || ctx.Value(contextKey{}) != "preserved" || ctx.Err() != nil {
					t.Error("persistence lost its live five-second bound or parent context")
				}
				if prepareCtx == nil || !errors.Is(prepareCtx.Err(), context.Canceled) {
					t.Error("storage started before preparation ended")
				}
				if parentBound {
					if !deadline.Equal(parentDeadline) || !prepareDeadline.Equal(parentDeadline) {
						t.Error("terminal stage extended its parent's deadline")
					}
				} else if !deadline.After(prepareDeadline) {
					t.Error("preparation consumed the persistence budget")
				}
				if persistCtx == nil {
					persistCtx = ctx
				} else if persistCtx != ctx {
					t.Error("persistence renewed its budget between store operations")
				}
			}
			if err = run.Finish(ctx, base.Trajectory, base.Query, "saved", "complete"); err != nil {
				t.Fatal(err)
			}
			if calls == 0 || persistCtx == nil || !errors.Is(persistCtx.Err(), context.Canceled) {
				t.Fatal("persistence was skipped or left its context active")
			}
		})
	}
}

func TestRetainedContext_TerminalPreparationFailureDoesNotTouchStore(t *testing.T) {
	refused := errors.New("injected terminal preparation refusal")
	for _, scenario := range []string{"redaction", "cancel before", "cancel during", "expired parent"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			observed := &terminalStageStore{StateStore: store}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			redactor = terminalPreparationRedactor{Redactor: redactor, prepare: func(context.Context) error {
				if scenario == "redaction" {
					return refused
				}
				if scenario == "cancel during" {
					cancel()
				}
				return nil
			}}
			base := retainedBase("source", scenario)
			run, err := sessionmemory.BeginRetainedRun(ctx, observed, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = run.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = run.Start(ctx, base); err != nil {
				t.Fatal(err)
			}
			step := journalAction(t, run, base, true)
			base.Trajectory.Steps = append(base.Trajectory.Steps, step)
			calls := 0
			observed.observe = func(context.Context) { calls++ }
			want := context.Canceled
			switch scenario {
			case "redaction":
				want = refused
			case "cancel before":
				cancel()
			case "expired parent":
				var expire context.CancelFunc
				ctx, expire = context.WithDeadline(ctx, time.Unix(1, 0))
				defer expire()
				want = context.DeadlineExceeded
			}
			if err = run.Finish(ctx, base.Trajectory, base.Query, "saved", "complete"); !errors.Is(err, want) {
				t.Fatalf("preparation failure changed: %v", err)
			}
			if calls != 0 {
				t.Fatalf("failed preparation touched storage %d times", calls)
			}
		})
	}
}

func TestRetainedContext_TerminalPreparationDoesNotFreezeLifetimeAuthority(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, erase := range []bool{false, true} {
			name := "expiry"
			if erase {
				name = "erasure"
			}
			t.Run(driver+"/"+name, func(t *testing.T) {
				store, redactor, _ := retainedStore(t, driver)
				base := retainedBase("source", name)
				now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
				redactor = terminalPreparationRedactor{Redactor: redactor, prepare: func(ctx context.Context) error {
					if erase {
						_, err := store.DeleteScope(ctx, base.Quadruple.Identity)
						return err
					}
					now = now.Add(2 * time.Hour)
					return nil
				}}
				run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				if err = run.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err = run.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				step := journalAction(t, run, base, true)
				base.Trajectory.Steps = append(base.Trajectory.Steps, step)
				q := identity.Quadruple{Identity: base.Quadruple.Identity}
				before, err := store.Load(t.Context(), q, retainedKind)
				if err != nil {
					t.Fatal(err)
				}
				if err = run.Finish(t.Context(), base.Trajectory, base.Query, "saved", "complete"); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("preparation froze stale lifetime authority: %v", err)
				}
				after, err := store.Load(t.Context(), q, retainedKind)
				if erase {
					if !errors.Is(err, state.ErrNotFound) {
						t.Fatalf("preparation resurrected erased state: %v", err)
					}
				} else if err != nil || after.ID != before.ID {
					t.Fatalf("preparation published expired evidence: %v", err)
				}
			})
		}
	}
}

func TestRetainedContext_TerminalCancellationPreservesRequiredFailure(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("source", "required-failure")
	run, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = run.Start(t.Context(), base); !errors.Is(err, state.ErrStoreClosed) {
		t.Fatalf("required journal write did not fail: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = run.Finish(ctx, base.Trajectory, base.Query, "", "cancelled"); !errors.Is(err, state.ErrStoreClosed) {
		t.Fatalf("cancellation hid the required persistence failure: %v", err)
	}
}
