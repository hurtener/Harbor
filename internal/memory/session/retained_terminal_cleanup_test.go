package session_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

type terminalCancelStore struct {
	state.StateStore
	cancel      context.CancelFunc
	afterFrame  bool
	armed       bool
	deleteCalls int
}

func (s *terminalCancelStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	err := s.StateStore.SaveBatchIf(ctx, checks, records)
	if err == nil && s.armed && !s.afterFrame {
		s.cancel()
	}
	return err
}

func (s *terminalCancelStore) DeleteIf(ctx context.Context, check state.SlotExpectation) (bool, error) {
	deleted, err := s.StateStore.DeleteIf(ctx, check)
	s.deleteCalls++
	if err == nil && s.armed && s.afterFrame && strings.HasPrefix(check.Kind, journalHeadKind+"/action/") {
		s.cancel()
	}
	return deleted, err
}

// Expiry of the finalization context after the atomic publication is a cleanup
// failure, not a lost terminal outcome. Keep reporting the failure, fence the
// source, and recover the exact committed evidence without repeating execution.
func TestRetainedContext_TerminalCleanupFailureKeepsSealedEvidence(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, afterFrame := range []bool{false, true} {
			stage := "frame"
			wantDeletes := 1
			if afterFrame {
				stage = "head"
				wantDeletes = 2
			}
			t.Run(driver+"/"+stage, func(t *testing.T) {
				store, redactor, cfg := retainedStore(t, driver)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fault := &terminalCancelStore{StateStore: store, cancel: cancel, afterFrame: afterFrame}
				base := retainedBase("source", "terminal-cleanup")
				run, err := sessionmemory.BeginRetainedRun(t.Context(), fault, redactor, base.Quadruple, 4, time.Hour, nil)
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
				fault.armed = true
				if err = run.Finish(ctx, base.Trajectory, base.Query, "saved", "complete"); !errors.Is(err, context.Canceled) || !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("cleanup failure was hidden: %v", err)
				}
				if fault.deleteCalls != wantDeletes {
					t.Fatalf("cleanup stopped at wrong boundary: %d", fault.deleteCalls)
				}
				if err = run.BeforeDispatch(t.Context(), base, planner.Step{Action: planner.CallTool{Tool: "save"}}); err == nil {
					t.Fatal("cleanup failure authorized another source dispatch")
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
				q := identity.Quadruple{Identity: base.Quadruple.Identity}
				before, err := store.Load(t.Context(), q, retainedKind)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(before.Bytes, []byte(`"status":"complete"`)) || bytes.Contains(before.Bytes, []byte(`"active"`)) {
					t.Fatal("cleanup failure changed the terminal publication")
				}
				if _, err = sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("sealed source was reacquired: %v", err)
				}
				if err = sessionmemory.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
					t.Fatalf("cleanup could not be reconciled: %v", err)
				}
				after, err := store.Load(t.Context(), q, retainedKind)
				if err != nil || after.ID != before.ID || !bytes.Equal(after.Bytes, before.Bytes) {
					t.Fatalf("cleanup rewrote committed evidence: %v", err)
				}
				for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000"} {
					if _, err = store.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
						t.Fatalf("transient journal remained: %v", err)
					}
				}
				next := retainedBase("next", base.Quadruple.SessionID)
				restored, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = restored.Apply(&next); err != nil {
					t.Fatal(err)
				}
				body := encodeRetained(t, next)
				for _, exact := range []string{`"version":9007199254740993`, `"more":false`, `"historical_run_outcome":"complete"`, `"assistant_answer":"saved"`} {
					if !strings.Contains(body, exact) {
						t.Fatalf("sealed terminal evidence lost %s", exact)
					}
				}
			})
		}
	}
}
