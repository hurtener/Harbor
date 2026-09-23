package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns" // The real redactor is the only extra driver needed on this persistence seam.
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

const journalHeadKind = state.InternalKindPrefix + "session-execution-journal"

// Two independent production store pools share only a per-test PostgreSQL
// schema. This exercises database generation fences, not an in-process lock.
func retainedPostgresStores(t *testing.T) (state.StateStore, state.StateStore, audit.Redactor) {
	t.Helper()
	dsn := freshSchema(t, requireDSN(t))
	storeConfig := config.StateConfig{Driver: "postgres", DSN: dsn}
	open := func() state.StateStore {
		t.Helper()
		s, err := state.Open(t.Context(), storeConfig)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		return s
	}
	first, second := open(), open()
	redactor, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return first, second, redactor
}

func TestPostgres_RetainedContext_SettledRecoveryAndPendingRefusal(t *testing.T) {
	first, second, redactor := retainedPostgresStores(t)
	base := retainedBase("source", "postgres-recovery")
	base.InputArtifacts = []planner.InputArtifactView{{ID: "attachment-one", Bytes: []byte("PRIVATE-BINARY")}}
	r, err := runctx.BeginRetainedRun(t.Context(), first, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	update := planner.Step{LLMObservation: map[string]any{"user_message": "Keep the approved navigation"}}
	if err := r.RecordContext(t.Context(), base, update); err != nil {
		t.Fatal(err)
	}
	base.Trajectory.Steps = append(base.Trajectory.Steps, update)
	step := journalAction(t, r, base)
	before, err := second.Load(t.Context(), base.Quadruple, journalHeadKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := runctx.ReconcileRetainedRun(t.Context(), second, redactor, base.Quadruple, 4, nil); !errors.Is(err, runctx.ErrRetainedContextUnsettled) {
		t.Fatalf("pending operation became replayable: %v", err)
	}
	after, err := second.Load(t.Context(), base.Quadruple, journalHeadKind)
	if err != nil || before.ID != after.ID || string(before.Bytes) != string(after.Bytes) {
		t.Fatal("refusal mutated the pending journal")
	}
	if err := r.AfterDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	// Close the source connection before reconciling: no runtime handle from
	// the writer is needed to restore committed evidence.
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runctx.ReconcileRetainedRun(t.Context(), second, redactor, base.Quadruple, 4, nil); err != nil {
		t.Fatal(err)
	}
	if err := runctx.ReconcileRetainedRun(t.Context(), second, redactor, base.Quadruple, 4, nil); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
	next := retainedBase("next", base.Quadruple.SessionID)
	restored, err := runctx.BeginRetainedRun(t.Context(), second, redactor, next.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Apply(&next); err != nil {
		t.Fatal(err)
	}
	body := encodeRetained(t, next)
	for _, want := range []string{"attachment-one", "Keep the approved navigation", "9007199254740993", `"more":false`, `"historical_run_outcome":"interrupted"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("restored evidence lost %q", want)
		}
	}
	if strings.Contains(body, "PRIVATE-BINARY") {
		t.Fatal("attachment body copied into private execution context")
	}
	for _, item := range next.Trajectory.Steps {
		if item.Action != nil {
			t.Fatal("recovery constructed an executable historical action")
		}
	}
	for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000", journalHeadKind + "/action/001", journalHeadKind + "/action/002"} {
		if _, err := second.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("unsealed journal record: %s: %v", kind, err)
		}
	}
}

func TestPostgres_RetainedContext_CheckpointAndSourceExpiry(t *testing.T) {
	first, second, redactor := retainedPostgresStores(t)
	now := time.Now()
	clock := func() time.Time { return now }
	base := retainedBase("source", "postgres-checkpoint")
	r, err := runctx.BeginRetainedRun(t.Context(), first, redactor, base.Quadruple, 4, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"covered-one", "covered-two", "fresh-9007199254740993"} {
		base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: marker, Args: json.RawMessage(`{}`)}, LLMObservation: marker})
	}
	base.Budget.TokenBudget = 1
	compactor := planner.NewCompressionRunner(&retainedSummaryRecorder{})
	if err := compactor.MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
		t.Fatal(err)
	}
	if base.Trajectory.Summary != nil {
		t.Fatal("unobserved execution evidence was compacted")
	}
	// This direct-compactor fixture bypasses the run loop's observation
	// acknowledgement. Mark only the earlier prefix observed, leaving the
	// newest result protected until it reaches the next decision request.
	seen := len(base.Trajectory.Steps) - 1
	base.Trajectory.UnseenFrom = &seen
	if err := compactor.MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(t.Context(), base.Trajectory, base.Query, "saved", "complete"); err != nil {
		t.Fatal(err)
	}
	next := retainedBase("next", base.Quadruple.SessionID)
	next.Query, next.Trajectory.Query = "Change the footer", "Change the footer"
	restored, err := runctx.BeginRetainedRun(t.Context(), second, redactor, next.Quadruple, 4, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Apply(&next); err != nil {
		t.Fatal(err)
	}
	start, err := next.Trajectory.ReplayStart()
	if err != nil || start != 3 || next.Trajectory.Summary == nil {
		t.Fatalf("checkpoint not rebound to next request: %d %v", start, err)
	}
	tail, err := json.Marshal(next.Trajectory.Steps[start:])
	if err != nil || strings.Contains(string(tail), "covered-one") || !strings.Contains(string(tail), "fresh-9007199254740993") {
		t.Fatal("coverage lost exact recent evidence")
	}
	now = now.Add(2 * time.Minute)
	later := retainedBase("later", base.Quadruple.SessionID)
	expired, err := runctx.BeginRetainedRun(t.Context(), first, redactor, later.Quadruple, 4, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := expired.Apply(&later); err != nil {
		t.Fatal(err)
	}
	if later.Trajectory.Summary != nil || strings.Contains(encodeRetained(t, later), "fresh-9007199254740993") {
		t.Fatal("derived checkpoint outlived its source")
	}
	// A stale writer cannot publish into a session erased on another pool.
	if _, err := second.DeleteScope(t.Context(), later.Quadruple.Identity); err != nil {
		t.Fatal(err)
	}
	if err := expired.Finish(t.Context(), later.Trajectory, later.Query, "must not return", "complete"); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
		t.Fatalf("erased admission resurrected: %v", err)
	}
}

func TestPostgres_RetainedContext_TwoPoolDispatchReconciliationRace(t *testing.T) {
	first, second, redactor := retainedPostgresStores(t)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := retainedBase("source", fmt.Sprintf("postgres-race-%03d", i))
			r, err := runctx.BeginRetainedRun(t.Context(), first, redactor, base.Quadruple, 4, time.Hour, nil)
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
				recovered <- runctx.ReconcileRetainedRun(t.Context(), second, redactor, base.Quadruple, 4, nil)
			}()
			close(start)
			dispatchErr := r.BeforeDispatch(t.Context(), base, planner.Step{Action: planner.CallTool{Tool: "save", CallID: "next"}})
			recoverErr := <-recovered
			if (dispatchErr == nil) == (recoverErr == nil) {
				t.Errorf("scope %d has no unique generation winner: dispatch=%v recovery=%v", i, dispatchErr, recoverErr)
			}
			// A different tenant cannot recover this session's execution.
			other := base.Quadruple
			other.Identity = identity.Identity{TenantID: "other", UserID: other.UserID, SessionID: other.SessionID}
			if err := runctx.ReconcileRetainedRun(t.Context(), second, redactor, other, 4, nil); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
				t.Errorf("scope %d crossed tenant authority: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

func retainedBase(run, session string) planner.RunContext {
	return planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: session}, RunID: run}, Query: "edit the document", Trajectory: &planner.Trajectory{Query: "edit the document"}}
}

func encodeRetained(t *testing.T, base planner.RunContext) string {
	t.Helper()
	data, err := base.Trajectory.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func journalAction(t *testing.T, r *runctx.RetainedRun, base planner.RunContext) planner.Step {
	t.Helper()
	step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "save-one", Args: json.RawMessage(`{"id":"doc-a"}`)}}
	if err := r.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"id":"doc-a","version":9007199254740993,"more":false}`)
	return step
}

type retainedSummaryRecorder struct{}

func (*retainedSummaryRecorder) Summarise(context.Context, planner.RunContext, *planner.Trajectory) (*planner.TrajectorySummary, error) {
	return &planner.TrajectorySummary{Facts: []string{"Keep the approved navigation"}, Pending: []string{"verify remaining edit"}}, nil
}
