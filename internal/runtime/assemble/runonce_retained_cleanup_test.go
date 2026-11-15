package assemble_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	retainedWindowKind  = state.InternalKindPrefix + "session-execution-context"
	retainedJournalKind = state.InternalKindPrefix + "session-execution-journal"
)

// journalFaultStore fails retained journal cleanup (as a timed-out delete) or
// the terminal window publication, leaving every other store call untouched.
type journalFaultStore struct {
	state.StateStore
	failCleanup  bool
	failTerminal bool
}

func (s *journalFaultStore) DeleteIf(ctx context.Context, check state.SlotExpectation) (bool, error) {
	if s.failCleanup && strings.HasPrefix(check.Kind, retainedJournalKind) {
		return false, context.DeadlineExceeded
	}
	return s.StateStore.DeleteIf(ctx, check)
}

func (s *journalFaultStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	for _, record := range records {
		if s.failTerminal && record.Kind == retainedWindowKind {
			return errors.New("injected terminal write failure")
		}
	}
	return s.StateStore.SaveBatchIf(ctx, checks, records)
}

// A run whose final turn is saved succeeds even when the later journal cleanup
// fails. The next turn sees the saved turn, and reconciling the run reaps the
// leftover journal.
func TestRunOnce_RetainedCleanupFailureAfterSavedTurnSucceeds(t *testing.T) {
	s, c, calls := retainedRecordingStack(t)
	inner := s.State
	fault := &journalFaultStore{StateStore: inner, failCleanup: true}
	s.State = fault
	defer func() { s.State = inner }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "cleanup"}
	q := identity.Quadruple{Identity: id, RunID: "first"}

	env, err := s.RunOnce(t.Context(), "read the document", id, assemble.WithRunID("first"))
	if err != nil || env.FinishReason != string(planner.FinishGoal) {
		t.Fatalf("run with a saved final turn reported failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("tool calls=%d", calls.Load())
	}
	for _, kind := range []string{retainedJournalKind, retainedJournalKind + "/action/000"} {
		if _, err := inner.Load(t.Context(), q, kind); err != nil {
			t.Fatalf("expected leftover %s after failed cleanup: %v", kind, err)
		}
	}

	fault.failCleanup = false
	if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID("second")); err != nil {
		t.Fatalf("next turn: %v", err)
	}
	if !strings.Contains(c.body(t, "cleanup/second"), "doc-a") {
		t.Fatal("next turn did not see the saved turn")
	}
	if calls.Load() != 1 {
		t.Fatal("historical action executed again")
	}

	if err := s.ReconcileRetainedContext(t.Context(), id, "first"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, kind := range []string{retainedJournalKind, retainedJournalKind + "/action/000"} {
		if _, err := inner.Load(t.Context(), q, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("reconciliation left %s: %v", kind, err)
		}
	}
}

// A failed terminal write before the final turn is durable still fails the run.
func TestRunOnce_RetainedTerminalWriteFailureFailsRun(t *testing.T) {
	s, _, _ := retainedRecordingStack(t)
	inner := s.State
	s.State = &journalFaultStore{StateStore: inner, failTerminal: true}
	defer func() { s.State = inner }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "terminal"}

	if _, err := s.RunOnce(t.Context(), "read the document", id, assemble.WithRunID("first")); err == nil {
		t.Fatal("run reported success without a saved final turn")
	}
	window, err := inner.Load(t.Context(), identity.Quadruple{Identity: id}, retainedWindowKind)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(window.Bytes), `"status":"complete"`) {
		t.Fatal("failed terminal write published a turn")
	}
}
