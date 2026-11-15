package session_test

import (
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

// deadlineProbeStore records whether each store write ran under a deadline and
// can fail the journal cleanup or the terminal publication on demand.
type deadlineProbeStore struct {
	state.StateStore
	failCleanup  bool
	failTerminal bool
	unbounded    []string
}

func (s *deadlineProbeStore) probe(ctx context.Context, op string) {
	if _, ok := ctx.Deadline(); !ok {
		s.unbounded = append(s.unbounded, op)
	}
}

func (s *deadlineProbeStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	s.probe(ctx, "load "+kind)
	return s.StateStore.Load(ctx, q, kind)
}

func (s *deadlineProbeStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	s.probe(ctx, "save batch")
	for _, record := range records {
		if s.failTerminal && record.Kind == retainedKind {
			return errors.New("injected terminal write failure")
		}
	}
	return s.StateStore.SaveBatchIf(ctx, checks, records)
}

func (s *deadlineProbeStore) DeleteIf(ctx context.Context, check state.SlotExpectation) (bool, error) {
	s.probe(ctx, "delete "+check.Kind)
	if s.failCleanup {
		return false, context.DeadlineExceeded
	}
	return s.StateStore.DeleteIf(ctx, check)
}

// deadlineProbeRedactor fails the test if redaction runs under a write deadline.
type deadlineProbeRedactor struct {
	t     *testing.T
	calls int
}

func (r *deadlineProbeRedactor) Redact(ctx context.Context, value any) (any, error) {
	r.calls++
	if _, ok := ctx.Deadline(); ok {
		r.t.Error("redaction ran under the store write deadline")
	}
	return value, nil
}

// The terminal turn's redaction and encoding run before the write bound starts;
// every store call that publishes the turn runs under it.
func TestRetainedRun_WriteBoundCoversOnlyStoreIO(t *testing.T) {
	inner, _, _ := retainedStore(t, "inmem")
	store := &deadlineProbeStore{StateStore: inner}
	redactor := &deadlineProbeRedactor{t: t}
	base := retainedBase("bounded", "write-bound")
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
	base.Trajectory.Steps = append(base.Trajectory.Steps, journalAction(t, r, base, true))
	store.unbounded, redactor.calls = nil, 0
	if err := r.Finish(context.Background(), base.Trajectory, base.Query, "done", "complete"); err != nil {
		t.Fatal(err)
	}
	if len(store.unbounded) != 0 {
		t.Fatalf("store I/O ran without the write bound: %v", store.unbounded)
	}
	if redactor.calls == 0 {
		t.Fatal("redaction was not exercised")
	}
}

// A cleanup failure after the terminal turn committed is not a failed run: the
// turn is durable, the source stays fenced, and reconciliation removes the rest.
func TestRetainedRun_CleanupFailureAfterCommittedTurn(t *testing.T) {
	inner, redactor, _ := retainedStore(t, "inmem")
	store := &deadlineProbeStore{StateStore: inner, failCleanup: true}
	base := retainedBase("source", "cleanup-after-commit")
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
	base.Trajectory.Steps = append(base.Trajectory.Steps, journalAction(t, r, base, true))
	err = r.Finish(context.Background(), base.Trajectory, base.Query, "done", "complete")
	if !errors.Is(err, sessionmemory.ErrRetainedCleanupIncomplete) {
		t.Fatalf("cleanup failure after the committed turn: %v", err)
	}
	window := loadHostRecord(t, inner, identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
	if !strings.Contains(string(window.Bytes), `"status":"complete"`) || !strings.Contains(string(window.Bytes), "9007199254740993") {
		t.Fatal("terminal turn was not committed")
	}
	if err := r.BeforeDispatch(t.Context(), base, planner.Step{Action: planner.CallTool{Tool: "write"}}); err == nil {
		t.Fatal("finished source accepted another dispatch")
	}
	for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000"} {
		if _, err := inner.Load(t.Context(), base.Quadruple, kind); err != nil {
			t.Fatalf("leftover %s missing before reconciliation: %v", kind, err)
		}
	}
	if err := sessionmemory.ReconcileRetainedRun(t.Context(), inner, redactor, base.Quadruple, 4, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000"} {
		if _, err := inner.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("reconciliation left %s: %v", kind, err)
		}
	}
}

// A failed terminal publication still fails Finish: nothing is masked before
// the turn is durable.
func TestRetainedRun_TerminalWriteFailureStillFails(t *testing.T) {
	inner, redactor, _ := retainedStore(t, "inmem")
	store := &deadlineProbeStore{StateStore: inner, failTerminal: true}
	base := retainedBase("source", "terminal-failure")
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
	base.Trajectory.Steps = append(base.Trajectory.Steps, journalAction(t, r, base, true))
	err = r.Finish(context.Background(), base.Trajectory, base.Query, "done", "complete")
	if err == nil || errors.Is(err, sessionmemory.ErrRetainedCleanupIncomplete) {
		t.Fatalf("terminal write failure was not reported as a failure: %v", err)
	}
	window := loadHostRecord(t, inner, identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
	if strings.Contains(string(window.Bytes), `"status":"complete"`) {
		t.Fatal("failed terminal write published a turn")
	}
}
