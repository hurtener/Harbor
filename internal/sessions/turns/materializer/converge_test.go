package materializer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

// ---------------------------------------------------------------------------
// Deferred complete sealing — convergence without a new canonical event
// (P1: the deferred seal never reread the TaskSnapshotReader, so a late
// answer, an initially-missing task record, or a durable restart could
// leave a completed turn running/unsealed forever).
// ---------------------------------------------------------------------------

// TestMaterialize_DeferredCompleteConvergesWhenRecordAppears pins the
// convergence contract for an INITIALLY MISSING task record: the
// completion defers (the row honestly stays running), and when the
// record appears WITHOUT any new canonical event the next convergence
// pass rereads the exact snapshot under the original event identity /
// task id, attaches the newly available bounded answer / input / output
// data, and seals — no event replay, no manual rebuild. Components the
// projection's write surface cannot carry post-append (query / agent)
// stay honestly unavailable.
func TestMaterialize_DeferredCompleteConvergesWhenRecordAppears(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().markMissing("task-miss")
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-miss")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-miss", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-miss"))
	h.src.publish(t, completedEv(h.id, "task-miss"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.PendingComplete != 1 {
		t.Errorf("pending complete = %d, want 1", res.PendingComplete)
	}
	row := mustGetRow(t, h, "task-miss")
	if row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("row = status %q sealed %v, want running/unsealed (deferred)", row.Status, row.Sealed)
	}
	if row.Answer.State != turns.AnswerStateUnavailable {
		t.Errorf("answer = %q, want honest unavailable while the record is missing", row.Answer.State)
	}

	// The task record appears — WITHOUT any new canonical event. The
	// next convergence pass rereads it and seals.
	reader.unmarkMissing("task-miss")
	reader.set("task-miss", TaskSnapshot{
		QueryPresent: true, Query: "late query", QueryAt: time.Unix(1_700_000_300, 0),
		AgentPresent: true, AgentID: "agent-l", AgentName: "Lambda", AgentBindingSource: turns.AgentBindingExplicit,
		InputsPresent: true, Inputs: []turns.Attachment{{
			ID: "in-l", MimeType: "text/plain", Availability: turns.CompletenessComplete,
		}},
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "the late answer"},
		OutputsPresent: true, Outputs: []turns.Attachment{{
			ID: "out-l", MimeType: "text/markdown", Availability: turns.CompletenessComplete,
		}},
	})
	res, err = m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if res.PendingComplete != 0 {
		t.Errorf("pending complete = %d, want 0 after convergence", res.PendingComplete)
	}
	row = mustGetRow(t, h, "task-miss")
	if !row.Sealed || row.Status != turns.StatusComplete || row.FinishReason != turns.FinishGoal {
		t.Fatalf("row = status %q sealed %v finish %q, want complete sealed", row.Status, row.Sealed, row.FinishReason)
	}
	if row.Answer.State != turns.AnswerStateInline || row.Answer.Inline != "the late answer" {
		t.Errorf("answer = %+v, want the late inline answer", row.Answer)
	}
	// The input attachments (never seeded at spawn) and the output
	// attachments rode the convergence update.
	if len(row.Inputs) != 1 || row.Inputs[0].ID != "in-l" {
		t.Errorf("inputs = %+v, want the record's input in-l", row.Inputs)
	}
	if len(row.Outputs) != 1 || row.Outputs[0].ID != "out-l" {
		t.Errorf("outputs = %+v, want the record's output out-l", row.Outputs)
	}
	// Query / agent are append-only in the projection: the late record's
	// query / agent honestly stay unavailable — never fabricated.
	if row.Query.Complete != turns.CompletenessUnavailable || row.Agent.Complete != turns.CompletenessUnavailable {
		t.Errorf("query/agent must honestly stay unavailable (append-only): query=%+v agent=%+v", row.Query, row.Agent)
	}
}

// TestMaterialize_DeferredCompleteConvergesWhenAnswerAppears pins the
// convergence contract for a PRESENT record without an answer: the
// completion defers, and when the answer lands on the record (no new
// event) the next pass converges and seals with the exact answer.
func TestMaterialize_DeferredCompleteConvergesWhenAnswerAppears(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().set("task-late", TaskSnapshot{TaskID: "task-late", RunID: "run-late"})
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-late")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-late", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-late"))
	h.src.publish(t, completedEv(h.id, "task-late"))

	if res, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if res.PendingComplete != 1 {
		t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
	}
	if row := mustGetRow(t, h, "task-late"); row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("row = status %q sealed %v, want running/unsealed", row.Status, row.Sealed)
	}

	// The answer becomes available — no new event.
	reader.set("task-late", TaskSnapshot{
		TaskID: "task-late", RunID: "run-late",
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateEmpty},
	})
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if res.PendingComplete != 0 {
		t.Errorf("pending complete = %d, want 0", res.PendingComplete)
	}
	row := mustGetRow(t, h, "task-late")
	if !row.Sealed || row.Status != turns.StatusComplete {
		t.Fatalf("row = status %q sealed %v, want complete sealed", row.Status, row.Sealed)
	}
	// A definite EMPTY answer is a legitimate complete seal.
	if row.Answer.State != turns.AnswerStateEmpty || row.Answer.Complete != turns.CompletenessComplete {
		t.Errorf("answer = %+v, want the definite empty envelope", row.Answer)
	}
}

// TestMaterialize_RunLoop_PendingCompleteConvergesOnUnchangedWatermarkPoll
// pins requirement 2: the lost-wake poll must converge a deferred
// complete seal even when the source watermark is UNCHANGED — no new
// canonical event, no wake notification — because the pending-work
// queue is served on every poll tick. Wakes stay the fast path; the
// poll's unchanged-watermark convergence is the bounded no-new-event
// path.
func TestMaterialize_RunLoop_PendingCompleteConvergesOnUnchangedWatermarkPoll(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().setSeq(
		TaskSnapshot{}, // spawn read — no answer
		TaskSnapshot{}, // completion read — no answer
	)
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader), WithPollInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Publish a lifecycle whose completion defers, and DELIBERATELY
	// drop the wake (h.src.notify() is never called).
	h.src.publish(t, spawnEv(h.id, "run-p", "task-p", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-p"))
	h.src.publish(t, completedEv(h.id, "task-p"))

	// Wait for the deferred state (both snapshot reads done, row
	// running/unsealed), then make the answer available WITHOUT
	// publishing any new event: only the poll's pending-queue check can
	// converge it (the source watermark is unchanged).
	if !eventually(t, func() bool {
		row, err := h.proj.Get(context.Background(), h.id, "task-p")
		return err == nil && !row.Sealed && reader.callCount() >= 2
	}) {
		t.Fatal("deferred state never established")
	}
	reader.set("task-p", TaskSnapshot{
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "poll-converged"},
	})

	if !eventually(t, func() bool {
		row, err := h.proj.Get(context.Background(), h.id, "task-p")
		return err == nil && row.Sealed && row.Status == turns.StatusComplete && row.Answer.Inline == "poll-converged"
	}) {
		t.Fatal("unchanged-watermark poll never converged the pending complete")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on cancellation")
	}
}

// TestMaterialize_Restart_DeferredCompleteConvergesAtOrBelowCheckpoint
// pins requirement 3: after a durable restart the terminal event at or
// below the durable checkpoint reconstructs the deferred-complete state
// — but ONLY after reading the existing exact turn and proving it is
// the same unsealed incomplete row — and the convergence pass then
// rereads the snapshot and seals. It must not reapply ordinary history,
// regress sequence/checkpoint, resurrect an erased/retained-away turn,
// or overwrite sealed content; the sealed row is byte-stable.
func TestMaterialize_Restart_DeferredCompleteConvergesAtOrBelowCheckpoint(t *testing.T) {
	dsn := t.TempDir() + "/turns-deferred.sqlite"
	h := newHarness(t, dsn)
	reader := newFakeTaskReader().setSeq(
		TaskSnapshot{}, // spawn read — no answer
		TaskSnapshot{}, // completion read — no answer
	)
	m1 := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-d")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-d", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-d"))
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "d.tool", time.Now()))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "d.tool", 5))
	last := h.src.publish(t, completedEv(h.id, "task-d"))

	// Pass 1: the completion defers (no answer yet) but the durable
	// checkpoint advances PAST it — the exact shape that previously
	// stranded the row as running forever after a restart.
	res, err := m1.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize (pre-restart): %v", err)
	}
	if res.PendingComplete != 1 {
		t.Errorf("pending complete = %d, want 1", res.PendingComplete)
	}
	pre := mustGetRow(t, h, "task-d")
	if pre.Sealed || pre.Status != turns.StatusRunning {
		t.Fatalf("pre-restart row = status %q sealed %v, want running/unsealed", pre.Status, pre.Sealed)
	}
	preCP, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if preCP != last.Sequence {
		t.Errorf("checkpoint = %d, want %d (the deferred completion still advanced the durable checkpoint)", preCP, last.Sequence)
	}

	// The task record gains its answer — WITHOUT any new canonical
	// event — before the restart.
	reader.set("task-d", TaskSnapshot{
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "restart-converged"},
		OutputsPresent: true, Outputs: []turns.Attachment{{
			ID: "out-d", MimeType: "text/markdown", Availability: turns.CompletenessComplete,
		}},
	})

	// "Restart": close and reopen the store; a fresh materializer
	// re-pages the retained source. The completion event is AT/BELOW the
	// durable checkpoint: the catch-up must reconstruct the deferred
	// state from the exact unsealed row and converge it in the SAME
	// pass.
	h.closeStore()
	store2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	proj2, err := turns.New(store2)
	if err != nil {
		t.Fatalf("reopen projector: %v", err)
	}
	defer func() { _ = proj2.Close(context.Background()) }()
	h2 := &harness{id: h.id, store: store2, proj: proj2, src: h.src}
	m2, err := New(h2.src, h2.proj, WithTaskSnapshotReader(reader))
	if err != nil {
		t.Fatalf("new post-restart materializer: %v", err)
	}
	res2, err := m2.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize (post-restart): %v", err)
	}
	if res2.PendingComplete != 0 {
		t.Errorf("post-restart pending complete = %d, want 0 (converged in the restart pass)", res2.PendingComplete)
	}
	post := mustGetRow(t, h2, "task-d")
	if !post.Sealed || post.Status != turns.StatusComplete || post.Answer.Inline != "restart-converged" {
		t.Fatalf("post-restart row = status %q sealed %v answer %q, want complete sealed", post.Status, post.Sealed, post.Answer.Inline)
	}

	// No sequence / checkpoint regression: the convergence used the
	// projector's no-new-event semantics (EventSeq 0) and the
	// checkpoint never moved.
	postCP, err := h2.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("post-restart checkpoint: %v", err)
	}
	if postCP != preCP {
		t.Errorf("post-restart checkpoint = %d, want %d (never regressed)", postCP, preCP)
	}
	if post.LastAppliedEventSeq != pre.LastAppliedEventSeq {
		t.Errorf("last-applied sequence regressed %d→%d", pre.LastAppliedEventSeq, post.LastAppliedEventSeq)
	}

	// The ordinary pre-restart history (tool activity) survived the
	// restart catch-up untouched — ordinary history was NOT re-applied.
	if len(post.Activity.Rows) != 1 || post.Activity.Rows[0].Status != turns.ActivitySucceeded {
		t.Errorf("post-restart activity = %+v (pre-restart history must be preserved)", post.Activity.Rows)
	}

	// The sealed row is byte-stable across a further pass (no churn).
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("post-restart re-materialize: %v", err)
	}
	after := mustGetRow(t, h2, "task-d")
	if !reflect.DeepEqual(post, after) {
		t.Errorf("sealed row changed after a further pass:\nbefore: %+v\nafter:  %+v", post, after)
	}
	if after.Version != post.Version {
		t.Errorf("sealed row version bumped %d→%d", post.Version, after.Version)
	}
}

// TestMaterialize_DeferredConvergence_EveryRereadBindsExactIdentityAndTask
// pins requirement 1's binding clause: EVERY convergence reread — like
// the live projections — is bound to the exact session identity and
// root task id, and the answer arrives only after the reread.
func TestMaterialize_DeferredConvergence_EveryRereadBindsExactIdentityAndTask(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().setSeq(
		TaskSnapshot{TaskID: "task-bx", RunID: "run-bx"}, // spawn read
		TaskSnapshot{TaskID: "task-bx", RunID: "run-bx"}, // completion read
	)
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-bx")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-bx", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-bx"))
	h.src.publish(t, completedEv(h.id, "task-bx"))

	if res, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if res.PendingComplete != 1 {
		t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
	}

	// The answer converges without a new event; the convergence reread
	// is BOUND to the exact session identity and root task id.
	reader.set("task-bx", TaskSnapshot{
		TaskID: "task-bx", RunID: "run-bx",
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "bound answer"},
	})
	if res, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("re-materialize: %v", err)
	} else if res.PendingComplete != 0 {
		t.Fatalf("pending complete = %d, want 0", res.PendingComplete)
	}
	row := mustGetRow(t, h, "task-bx")
	if !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != "bound answer" {
		t.Fatalf("row = status %q sealed %v answer %q", row.Status, row.Sealed, row.Answer.Inline)
	}
	calls := reader.calls()
	if len(calls) < 3 {
		t.Fatalf("reader calls = %d, want >= 3 (spawn + completion + convergence reread)", len(calls))
	}
	for _, c := range calls {
		if c.taskID != "task-bx" || c.triple != h.id {
			t.Errorf("reader call = %+v, want task-bx under the exact event identity %+v", c, h.id)
		}
	}
}

// TestMaterialize_DeferredConvergence_CorruptRereadFailsLoud pins that
// a corrupt snapshot on the CONVERGENCE reread fails the pass loudly
// (the identity/task/run binding is re-enforced on every reread), leaves
// the row untouched, and does NOT lose the pending work — healed, the
// next pass converges.
func TestMaterialize_DeferredConvergence_CorruptRereadFailsLoud(t *testing.T) {
	t.Run("foreign task id on the reread", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().setSeq(
			TaskSnapshot{TaskID: "task-cx", RunID: "run-cx"}, // spawn read
			TaskSnapshot{TaskID: "task-cx", RunID: "run-cx"}, // completion read
		)
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-cx")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-cx", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-cx"))
		h.src.publish(t, completedEv(h.id, "task-cx"))
		if res, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		} else if res.PendingComplete != 1 {
			t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
		}

		// The convergence reread serves a snapshot claiming a FOREIGN
		// task id: the reread's TaskID binding must be re-enforced — the
		// pass fails loud and the row stays unsealed.
		reader.set("task-cx", TaskSnapshot{
			TaskID:        "task-other",
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "must not land"},
		})
		if _, err := m.Materialize(context.Background()); !errors.Is(err, ErrSnapshotTaskIDMismatch) {
			t.Fatalf("corrupt reread = %v, want ErrSnapshotTaskIDMismatch", err)
		}
		row := mustGetRow(t, h, "task-cx")
		if row.Sealed || row.Status != turns.StatusRunning {
			t.Fatalf("row = status %q sealed %v, want untouched running", row.Status, row.Sealed)
		}

		// Healed, the next pass converges — the pending work was not
		// lost by the failed pass.
		reader.set("task-cx", TaskSnapshot{
			TaskID: "task-cx", RunID: "run-cx",
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "the answer"},
		})
		res, err := m.Materialize(context.Background())
		if err != nil {
			t.Fatalf("healed materialize: %v", err)
		}
		if res.PendingComplete != 0 {
			t.Errorf("pending complete = %d, want 0", res.PendingComplete)
		}
		row = mustGetRow(t, h, "task-cx")
		if !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != "the answer" {
			t.Fatalf("healed row = status %q sealed %v answer %q", row.Status, row.Sealed, row.Answer.Inline)
		}
	})

	t.Run("run rebind on the reread", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().setSeq(
			TaskSnapshot{TaskID: "task-rx", RunID: "run-1"}, // spawn read — establishes the binding
			TaskSnapshot{TaskID: "task-rx", RunID: "run-1"}, // completion read
		)
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-1")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-rx", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-rx"))
		h.src.publish(t, completedEv(h.id, "task-rx"))
		if res, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		} else if res.PendingComplete != 1 {
			t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
		}

		// The convergence reread attempts to MOVE the established run
		// binding: the reread's run agreement must be re-enforced — the
		// pass fails loud, the row keeps its binding and stays unsealed.
		reader.set("task-rx", TaskSnapshot{
			TaskID: "task-rx", RunID: "run-2",
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "must not land"},
		})
		if _, err := m.Materialize(context.Background()); !errors.Is(err, ErrSnapshotRunIDMismatch) {
			t.Fatalf("run rebind reread = %v, want ErrSnapshotRunIDMismatch", err)
		}
		row := mustGetRow(t, h, "task-rx")
		if row.Sealed || row.RunID != "run-1" {
			t.Fatalf("row = sealed %v run %q, want the established binding run-1 untouched", row.Sealed, row.RunID)
		}
	})
}

// TestMaterialize_DeferredConvergence_ErasedRetainedSealedRefusal pins
// requirement 3's refusal clause: the convergence reread never
// resurrects an erased / retained-away turn and never overwrites sealed
// content.
func TestMaterialize_DeferredConvergence_ErasedRetainedSealedRefusal(t *testing.T) {
	t.Run("sealed row is never overwritten by a stale entry", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().setSeq(
			TaskSnapshot{TaskID: "task-s1", RunID: "run-s1"}, // spawn read
			TaskSnapshot{TaskID: "task-s1", RunID: "run-s1"}, // completion read
		)
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-s1")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-s1", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-s1"))
		h.src.publish(t, completedEv(h.id, "task-s1"))
		if res, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		} else if res.PendingComplete != 1 {
			t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
		}

		// A REAL terminal event seals the row and eagerly clears the
		// queue.
		h.src.publish(t, failedEv(h.id, "task-s1", "timeout"))
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize (real terminal): %v", err)
		}
		sealedRow := mustGetRow(t, h, "task-s1")
		if !sealedRow.Sealed || sealedRow.Status != turns.StatusFailed {
			t.Fatalf("row = status %q sealed %v, want failed sealed", sealedRow.Status, sealedRow.Sealed)
		}
		if got := len(m.pending); got != 0 {
			t.Fatalf("pending queue = %d, want 0 (the real seal cleared it)", got)
		}

		// A stale queue entry survives (a defensive shape — e.g. a
		// concurrent materializer sealed the row): the convergence must
		// refuse to touch the sealed row and drop the entry without any
		// write.
		reader.set("task-s1", TaskSnapshot{
			TaskID: "task-s1", RunID: "run-s1",
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "must never land"},
		})
		m.mu.Lock()
		ts := m.sessions[h.id].turns[turns.TurnID("task-s1")]
		ts.pendingComplete = true
		m.pending = append(m.pending, pendingWork{id: h.id, taskID: "task-s1"})
		m.mu.Unlock()
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize with stale entry: %v", err)
		}
		after := mustGetRow(t, h, "task-s1")
		if !reflect.DeepEqual(sealedRow, after) {
			t.Errorf("sealed row was overwritten by a stale convergence entry:\nbefore: %+v\nafter:  %+v", sealedRow, after)
		}
		if after.Version != sealedRow.Version {
			t.Errorf("sealed row version bumped %d→%d", sealedRow.Version, after.Version)
		}
		if got := len(m.pending); got != 0 {
			t.Errorf("stale entry not consumed: pending = %d, want 0", got)
		}
	})

	t.Run("evicted row retires, never resurrects", func(t *testing.T) {
		store, err := sqlite.New(sqlite.Config{DSN: ":memory:", Retention: 1})
		if err != nil {
			t.Fatalf("open sqlite turns store: %v", err)
		}
		proj, err := turns.New(store)
		if err != nil {
			_ = store.Close(context.Background())
			t.Fatalf("new projector: %v", err)
		}
		h := &harness{
			id:    identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
			store: store,
			proj:  proj,
			src:   &fakeSource{},
			closeStore: func() {
				_ = proj.Close(context.Background())
			},
		}
		defer h.closeStore()
		reader := newFakeTaskReader().setSeq(
			TaskSnapshot{}, // turn 1 spawn read
			TaskSnapshot{}, // turn 1 completion read
			TaskSnapshot{}, // turn 2 spawn read
		)
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad1 := testQuad(h.id, "run-v1")
		h.src.publish(t, spawnEv(h.id, quad1.RunID, "task-v1", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-v1"))
		h.src.publish(t, completedEv(h.id, "task-v1"))
		// Turn 2's append evicts turn 1's row (retention bound 1).
		quad2 := testQuad(h.id, "run-v2")
		h.src.publish(t, spawnEv(h.id, quad2.RunID, "task-v2", tasks.KindForeground, ""))

		// The convergence pass meets the EVICTED pending turn: the
		// deferred seal retires (an honest terminal projection gap) and
		// the pass continues — never a resurrection, never a hard
		// failure, never a wedged queue.
		res, err := m.Materialize(context.Background())
		if err != nil {
			t.Fatalf("materialize: %v (an evicted deferred turn must never fail or wedge the pass)", err)
		}
		if res.PendingComplete != 0 {
			t.Errorf("pending = %d, want 0 (the evicted deferred seal retired)", res.PendingComplete)
		}
		if _, err := h.proj.Get(context.Background(), h.id, "task-v1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("evicted turn = %v, want ErrTurnNotFound (no resurrection)", err)
		}
		// Turn 2's row survives (it is the retained newest row).
		row2, err := h.proj.Get(context.Background(), h.id, "task-v2")
		if err != nil {
			t.Fatalf("turn 2: %v", err)
		}
		if row2.Sealed {
			t.Errorf("turn 2 must stay mutable (its terminal never arrived)")
		}
	})

	t.Run("erased session is fenced, never resurrected", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().setSeq(
			TaskSnapshot{}, // spawn read
			TaskSnapshot{}, // completion read
		)
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-e2")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-e2", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-e2"))
		h.src.publish(t, completedEv(h.id, "task-e2"))
		if res, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		} else if res.PendingComplete != 1 {
			t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
		}

		// Erase the session mid-deferral: the convergence pass now hits
		// the gone/fenced row — the entry is dropped (never a
		// resurrection) and the pass is NOT a hard failure.
		if _, err := h.proj.Erase(context.Background(), h.id); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize after erase = %v (must be a refusal, never a hard failure)", err)
		}
		if _, err := h.proj.Get(context.Background(), h.id, "task-e2"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("erased turn = %v, want ErrTurnNotFound (no resurrection)", err)
		}
		if got := len(m.pending); got != 0 {
			t.Errorf("erased pending entry not consumed: pending = %d, want 0", got)
		}
	})
}

// TestMaterialize_DeferredConvergence_BoundedManyPendingFairness pins
// requirement 2's bounded per-pass budget + stable queue discipline: a
// large pending queue is served a bounded number of entries per pass in
// stable FIFO / round-robin order, so every pending turn is attempted
// within a bounded pass count, and a turn whose answer never converges
// does NOT starve the others (they all seal; it stays honestly pending).
func TestMaterialize_DeferredConvergence_BoundedManyPendingFairness(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	const n = 4
	var snaps []TaskSnapshot
	for range n {
		// spawn read + completion read per turn: no answer.
		snaps = append(snaps, TaskSnapshot{}, TaskSnapshot{})
	}
	reader := newFakeTaskReader().setSeq(snaps...)
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader), WithConvergenceBudget(1))

	quad := testQuad(h.id, "run-f")
	for i := range n {
		taskID := fmt.Sprintf("task-f%d", i)
		h.src.publish(t, spawnEv(h.id, quad.RunID, taskID, tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, taskID))
		h.src.publish(t, completedEv(h.id, taskID))
	}

	// Pass 1: every completion defers — the queue holds all n turns.
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.PendingComplete != n {
		t.Errorf("pending = %d, want %d", res.PendingComplete, n)
	}

	// The first three records gain their answers — WITHOUT new events.
	for i := range 3 {
		reader.set(fmt.Sprintf("task-f%d", i), TaskSnapshot{
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: fmt.Sprintf("answer %d", i)},
		})
	}

	// With budget 1, the FIFO / round-robin discipline converges every
	// answer-available turn within a bounded pass count while the
	// never-answer turn keeps cycling to the tail — it never starves
	// the others and never blocks the queue.
	converged := false
	for pass := 2; pass <= 2*n+2; pass++ {
		res, err = m.Materialize(context.Background())
		if err != nil {
			t.Fatalf("materialize (pass %d): %v", pass, err)
		}
		if res.PendingComplete == 1 {
			converged = true
			break
		}
		if res.PendingComplete < 1 || res.PendingComplete > n {
			t.Fatalf("pass %d pending = %d, want 1..%d", pass, res.PendingComplete, n)
		}
	}
	if !converged {
		t.Fatal("fairness: the answer-available turns never converged within a bounded pass count")
	}

	// Exactly the three answer-available turns are sealed; the
	// never-answer turn stays honestly mutable.
	for i := range 3 {
		row := mustGetRow(t, h, fmt.Sprintf("task-f%d", i))
		if !row.Sealed || row.Status != turns.StatusComplete {
			t.Errorf("turn f%d = status %q sealed %v, want complete sealed", i, row.Status, row.Sealed)
		}
	}
	rowLast := mustGetRow(t, h, "task-f3")
	if rowLast.Sealed || rowLast.Status != turns.StatusRunning {
		t.Errorf("never-answer turn = status %q sealed %v, want honestly running", rowLast.Status, rowLast.Sealed)
	}
}

// TestMaterialize_DeferredConvergence_TransientErrorRetriesWithoutLosingPending
// pins requirement 5: a transient snapshot failure on the CONVERGENCE
// reread fails the pass loud WITHOUT losing the pending work (the entry
// stays queued, the row is never written, never falsely sealed), and
// the healed next pass converges.
func TestMaterialize_DeferredConvergence_TransientErrorRetriesWithoutLosingPending(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().setSeq(
		TaskSnapshot{}, // spawn read
		TaskSnapshot{}, // completion read
	)
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-t")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-t", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-t"))
	h.src.publish(t, completedEv(h.id, "task-t"))

	if res, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if res.PendingComplete != 1 {
		t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
	}

	// The task store becomes transiently unavailable: the convergence
	// reread fails loud and the pass fails WITHOUT losing the pending
	// work (the entry stays queued).
	reader.fail(errors.New("task store transient failure"))
	if _, err := m.Materialize(context.Background()); err == nil {
		t.Fatal("pass must fail loud on the transient convergence read failure")
	}
	row := mustGetRow(t, h, "task-t")
	if row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("row = status %q sealed %v (the failed convergence wrote nothing)", row.Status, row.Sealed)
	}
	if got := len(m.pending); got != 1 {
		t.Errorf("pending queue = %d, want 1 (the transient failure must not lose the pending work)", got)
	}

	// Healed (and the answer now available), the next pass converges —
	// the pending work was never lost.
	reader.fail(nil)
	reader.set("task-t", TaskSnapshot{
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "healed answer"},
	})
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("healed materialize: %v", err)
	}
	if res.PendingComplete != 0 {
		t.Errorf("pending complete = %d, want 0", res.PendingComplete)
	}
	row = mustGetRow(t, h, "task-t")
	if !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != "healed answer" {
		t.Fatalf("row = status %q sealed %v answer %q", row.Status, row.Sealed, row.Answer.Inline)
	}
}

// TestMaterialize_DeferredConvergence_NoSequenceRegressionByteStableSealed
// pins requirement 4 + 6: the convergence uses the projector's
// no-new-event semantics (EventSeq 0), so the sealed row's
// LastAppliedEventSeq never regresses and the checkpoint stays on the
// last canonical event; the sealed row is byte-stable across further
// passes.
func TestMaterialize_DeferredConvergence_NoSequenceRegressionByteStableSealed(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().setSeq(
		TaskSnapshot{}, // spawn read
		TaskSnapshot{}, // completion read
	)
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-s")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-s", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-s"))
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "s.tool", time.Now()))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "s.tool", 5))
	last := h.src.publish(t, completedEv(h.id, "task-s"))

	if res, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if res.PendingComplete != 1 {
		t.Fatalf("pending complete = %d, want 1", res.PendingComplete)
	}
	before := mustGetRow(t, h, "task-s")
	if before.Sealed || before.Status != turns.StatusRunning {
		t.Fatalf("row = status %q sealed %v, want running/unsealed", before.Status, before.Sealed)
	}

	// The answer converges without a new event.
	reader.set("task-s", TaskSnapshot{
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "converged answer"},
	})
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("convergence materialize: %v", err)
	}
	if res.PendingComplete != 0 {
		t.Errorf("pending complete = %d, want 0", res.PendingComplete)
	}
	sealed := mustGetRow(t, h, "task-s")
	if !sealed.Sealed || sealed.Status != turns.StatusComplete || sealed.Answer.Inline != "converged answer" {
		t.Fatalf("row = status %q sealed %v answer %q", sealed.Status, sealed.Sealed, sealed.Answer.Inline)
	}
	// No sequence regression: the convergence attached and sealed with
	// EventSeq 0, so the row's LastAppliedEventSeq is unchanged and the
	// checkpoint stays on the last canonical event.
	if sealed.LastAppliedEventSeq != before.LastAppliedEventSeq {
		t.Errorf("last-applied sequence regressed %d→%d", before.LastAppliedEventSeq, sealed.LastAppliedEventSeq)
	}
	cp, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != last.Sequence {
		t.Errorf("checkpoint = %d, want %d (never regressed)", cp, last.Sequence)
	}

	// Byte-stable sealed row: a further pass mutates nothing.
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	after := mustGetRow(t, h, "task-s")
	if !reflect.DeepEqual(sealed, after) {
		t.Errorf("sealed row changed after a further pass:\nbefore: %+v\nafter:  %+v", sealed, after)
	}
	if after.Version != sealed.Version {
		t.Errorf("sealed row version bumped %d→%d", sealed.Version, after.Version)
	}
}
