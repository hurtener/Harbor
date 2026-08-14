package materializer

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// pendingWork is one deferred-complete entry in the materializer's
// bounded pending-work queue: the session identity and root task id of
// a turn whose complete seal is deferred until its answer source
// (the injected TaskSnapshotReader) converges.
type pendingWork struct {
	id     identity.Identity
	taskID string
}

// enqueuePending records the turn's deferred complete seal in the
// bounded pending-work queue. It is called exactly once per deferral
// (the pendingComplete marker dedupes), so a turn never occupies more
// than one queue slot. Callers hold the materializer's mu.
func (m *Materializer) enqueuePending(sess *sessionState, ts *turnState) {
	if ts.pendingComplete {
		return
	}
	ts.pendingComplete = true
	m.pending = append(m.pending, pendingWork{id: sess.id, taskID: ts.taskID})
}

// clearPending clears the turn's deferred-complete marker and removes
// its queue entry — used when a REAL terminal event seals the row (a
// failed / cancelled seal needs no answer source and converges
// immediately) or the row is retired. Callers hold the materializer's
// mu.
func (m *Materializer) clearPending(sess *sessionState, ts *turnState) {
	if !ts.pendingComplete {
		return
	}
	ts.pendingComplete = false
	for i, pw := range m.pending {
		if pw.id == sess.id && pw.taskID == ts.taskID {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return
		}
	}
}

// convergePending processes up to m.convergenceBudget pending-complete
// entries in stable FIFO / round-robin order: each live entry
// REREADS the turn's exact task snapshot under the session identity
// and root task id, re-runs the accepted TaskID / RunID /
// answer-envelope agreement checks, attaches the newly available
// bounded answer / output / input data, and seals only after the
// answer source has converged. An entry whose answer source is STILL
// unavailable is re-enqueued at the tail, so every pending turn is
// attempted in stable round-robin order across passes and no single
// deferred seal starves the queue.
//
// The pass is BOUNDED: at most convergenceBudget attempts per call,
// and it never scans all sessions — only the queue. A hard failure (a
// transient snapshot error, a binding mismatch, a store failure)
// aborts the pass loud WITHOUT losing the pending work (the entry is
// re-enqueued and the next pass converges); an erasure fence that
// converged between the deferral and the convergence fenced the
// session and drops the entry (never a resurrection, never a hard
// failure). Stale entries (the turn was sealed or retired by a real
// terminal event) and fenced sessions are dropped.
//
// Callers hold the materializer's mu (Materialize holds it for the
// whole pass; convergeQueued takes it for the poll / wake path).
func (m *Materializer) convergePending(ctx context.Context, res *Result) error {
	if res == nil {
		res = &Result{}
	}
	for attempts := 0; attempts < m.convergenceBudget && len(m.pending) > 0; attempts++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		pw := m.pending[0]
		m.pending = m.pending[1:]
		sess := m.sessions[pw.id]
		if sess == nil {
			// The session state no longer exists (a fenced / evicted
			// entry must never wedge the queue): drop the stale entry.
			continue
		}
		if sess.fenced {
			res.FencedSessions++
			continue
		}
		ts := sess.turns[turns.TurnID(pw.taskID)]
		if ts == nil || !ts.pendingComplete || ts.terminal() {
			// A stale entry: the turn was sealed or retired by a real
			// terminal event (or the entry never was live). Dropped.
			continue
		}
		if m.snap == nil {
			// No reader: the answer is honestly unavailable and can
			// never converge on this instance. The turn stays queued
			// (its row honestly stays mutable) — the bounded budget and
			// the poll / wake cadence keep this from ever spinning, and
			// a later instance with a wired reader converges it.
			m.pending = append(m.pending, pw)
			continue
		}
		sealed, stillPending, err := m.completeSealProjection(ctx, sess, ts, "", 0)
		if err != nil {
			if errors.Is(err, turns.ErrErasureFenced) {
				// Erasure converged between the deferral and this
				// convergence: the session is permanently fenced — drop
				// the entry (never resurrect it), not a hard pass
				// failure.
				sess.fenced = true
				res.FencedSessions++
				continue
			}
			// A hard failure (a transient snapshot error, a binding
			// mismatch, a store failure): the entry is re-enqueued so
			// the next pass retries it — pending work is never lost and
			// the pass fails loud.
			m.pending = append(m.pending, pw)
			return err
		}
		switch {
		case sealed:
			// The durable row is now sealed: the deferred seal
			// converged. The entry was already consumed.
			ts.sealed = true
			ts.pendingComplete = false
		case stillPending:
			// The answer source has not converged yet: re-enqueue at
			// the tail so every pending turn is attempted in stable
			// FIFO / round-robin order — no deferred seal starves.
			m.pending = append(m.pending, pw)
		default:
			// retired: the durable row was evicted past retention — an
			// HONEST TERMINAL PROJECTION GAP. The deferred seal retires
			// with the routing state, never resurrecting the row.
			ts.pendingComplete = false
		}
	}
	return nil
}

// convergeQueued runs one bounded convergence pass over the
// deferred-complete queue when the event source has no new events
// behind the cursor — the unchanged-watermark convergence path (the
// lost-wake poll's job; also served on a spurious wake). A nil reader
// or an empty queue is a cheap no-op, so a caught-up loop never spins
// and never scans sessions beyond the bounded queue. Cancellation is
// honoured through the pass and stops the poll / timers (Run's exit
// paths are unchanged).
func (m *Materializer) convergeQueued(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return nil
	}
	return m.convergePending(ctx, &Result{})
}

// completeSealProjection rereads the exact turn and its task snapshot
// for a COMPLETE-seal candidate under the session identity and the
// turn's root task id, re-runs the accepted TaskID / RunID /
// answer-envelope agreement checks, attaches the newly available
// bounded answer / output / input-attachment data, and seals the turn
// — but only after the answer source has converged. The exact turn is
// read and PROVEN first (same authoritative task id, not sealed, not
// evicted): a sealed row is already terminal, an evicted / erased row
// is an honest terminal projection gap, and neither is ever written.
// It is the SINGLE complete-seal path shared by:
//
//   - the live completion projection (eventRunID = the completion
//     event envelope's run id, attachSeq = the completion event's
//     sequence), and
//   - the deferred-convergence reread (eventRunID = "" — the
//     established turn binding stands — and attachSeq = 0, the
//     projector's explicit NO-NEW-EVENT convergence semantics: an
//     observation with EventSeq 0 applies without advancing the row's
//     LastAppliedEventSeq, so convergence never fabricates a newer
//     canonical event sequence and never regresses the checkpoint).
//
// The snapshot read is BOUNDED and BOUND to the exact requested
// identity and task id (readTaskSnapshot), and the run observations
// must agree (bindRunID) — a later snapshot can never move an
// established turn/run binding. The query / agent components are
// append-only in the projection (the ops Update surface carries no
// such fields by design); when the record's query / agent were absent
// at spawn and appear later they honestly stay unavailable — the
// convergence attaches every component the write surface can carry
// (the answer envelope, the output attachments, and — when the spawn
// projection never seeded them — the record's input attachments).
//
// Returns sealed=true when the durable row is now sealed;
// stillPending=true when the answer source is still unavailable (the
// turn stays queued and the next pass rereads); or a wrapped error for
// a hard failure (a transient snapshot error, a binding mismatch, a
// store failure) that aborts the pass WITHOUT losing the pending work.
// A retired row (evicted past retention / erased) reports sealed=false,
// stillPending=false — an honest terminal projection gap.
func (m *Materializer) completeSealProjection(ctx context.Context, sess *sessionState, ts *turnState, eventRunID string, attachSeq uint64) (sealed, stillPending bool, err error) {
	// Read the exact turn FIRST — the convergence refuses to resurrect
	// an evicted / erased row, overwrite sealed content, or chase a
	// vanished row, exactly like the projector's monotonic guards. A
	// sealed row is already terminal (the entry is dropped without a
	// write); an evicted / erased row (ErrTurnNotFound) retires the
	// routing state — an HONEST TERMINAL PROJECTION GAP, never a
	// resurrection and never a hard failure.
	current, err := m.proj.Get(ctx, sess.id, turns.TurnID(ts.taskID))
	if err != nil {
		if errors.Is(err, turns.ErrTurnNotFound) {
			ts.retired = true
			return false, false, nil
		}
		return false, false, err
	}
	if current.Sealed {
		ts.sealed = true
		return true, false, nil
	}
	if current.TaskID != "" && current.TaskID != ts.taskID {
		// The durable row under this turn id belongs to a DIFFERENT
		// task — a corrupt store. Never converge against it.
		return false, true, nil
	}
	snap, err := m.readTaskSnapshot(ctx, sess.id, ts.taskID)
	if err != nil {
		return false, false, err
	}
	// Run binding on the reread: the event envelope (live path only),
	// the turn's established binding, and the snapshot must all agree;
	// a snapshot may fill the run only when no binding exists yet —
	// never move an established one. Enforced BEFORE any mutation so a
	// mismatch leaves the row untouched.
	bound, err := bindRunID(eventRunID, ts.runID, snap.RunID)
	if err != nil {
		return false, false, err
	}
	if bound != ts.runID {
		ts.runID = bound
	}
	if !snap.AnswerPresent {
		// The answer source has not converged: the row honestly stays
		// mutable and the seal is deferred to the next pass.
		return false, true, nil
	}
	// The answer envelope has converged: attach it (with the output
	// attachment metadata, and the record's input attachments when the
	// spawn projection never seeded them) BEFORE the seal, so the
	// complete seal's required answer source is present on the row. The
	// seal below carries EventSeq 0 ON PURPOSE (the projector's
	// no-new-event convergence semantics): on the live path the attach
	// just stamped the completion observation's sequence, and a seal
	// carrying the same sequence would be a monotonic no-op; on the
	// deferred path there is NO canonical event to stamp, so EventSeq 0
	// is the only honest sequence — it never fabricates one.
	ans := snap.Answer
	u := turns.Update{
		Answer:   &ans,
		Outputs:  snapshotOutputs(snap),
		EventSeq: attachSeq,
	}
	if snap.InputsPresent && len(ts.inputs) == 0 {
		// The record now reports input attachments that the spawn
		// projection never saw (the record was absent at spawn): seed
		// the in-memory accumulator and attach them so the row
		// converges with the record. Only the never-seeded gap is
		// filled — disposition-derived inputs are never clobbered.
		ts.inputs = append([]turns.Attachment(nil), snap.Inputs...)
		u.Inputs = ts.inputs
	}
	if _, err := m.updateTurn(ctx, sess, ts, u); err != nil {
		return false, false, err
	}
	row, err := m.sealTurn(ctx, sess, ts, turns.Seal{
		Status:       turns.StatusComplete,
		FinishReason: turns.FinishGoal,
		EventSeq:     0,
	})
	if err != nil {
		if errors.Is(err, turns.ErrSealIncomplete) {
			return false, true, nil
		}
		return false, false, err
	}
	if ts.retired {
		// The row was evicted past retention: the seal is an honest
		// terminal projection gap — retire the routing state, never
		// resurrect.
		return false, false, nil
	}
	if !row.Sealed {
		// Defensive (EventSeq 0 is never a monotonic no-op, so this is
		// unreachable in practice): a seal that returned an unsealed
		// row is NOT a successful seal — the turn stays pending and the
		// next pass rereads.
		return false, true, nil
	}
	return true, false, nil
}
