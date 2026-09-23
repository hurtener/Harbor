package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

// ErrRetainedContextUnsettled refuses recovery while an external action may be
// in flight or its outcome was never committed. Reconcile with the owning
// service; neither this error nor a restart is permission to repeat the action.
var ErrRetainedContextUnsettled = fmt.Errorf("%w: external dispatch outcome is unknown", ErrRetainedContextUnavailable)

// ReconcileRetainedRun explicitly seals an abandoned, fully settled journal as
// interrupted context. The caller must own the full identity triple. The atomic
// terminal write fences the source admission before any future dispatch can
// commit intent. No historical action, model request, or completion hook runs.
// Pending actions are refused; this is not a cold-run resume operation.
func ReconcileRetainedRun(ctx context.Context, store state.StateStore, redactor audit.Redactor, q identity.Quadruple, turns int, now func() time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil || redactor == nil || identity.Validate(q.Identity) != nil || q.RunID == "" || turns < 1 || turns > maxRetainedContextTurns {
		return ErrRetainedContextUnavailable
	}
	if now == nil {
		now = time.Now
	}
	r := &RetainedRun{store: store, redactor: redactor, q: q, turns: turns, now: now}
	window, _, err := r.load(ctx)
	if err != nil {
		return err
	}
	alreadySealed := false
	sealedStatus := ""
	for _, turn := range window.Turns {
		if turn.Admission.RunID == q.RunID {
			if !turn.ExpiresAt.After(now()) {
				return ErrRetainedContextUnavailable
			}
			r.admission, alreadySealed = turn.Admission, true
			sealedStatus = turn.Status
			break
		}
	}
	if !alreadySealed {
		for _, active := range window.Active {
			if active.RunID == q.RunID {
				r.admission = active
				break
			}
		}
		if r.admission.ID == "" {
			return ErrRetainedContextUnavailable
		}
	}
	record, err := store.Load(ctx, q, retainedJournalKind)
	if alreadySealed && errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil || record.Identity != q || record.Kind != retainedJournalKind || len(record.Bytes) > maxRetainedContextBytes {
		return ErrRetainedContextUnavailable
	}
	if err := decodeRetained(record.Bytes, &r.journal); err != nil {
		return err
	}
	head := r.journal
	if head.Version != retainedJournalVersion || head.Admission != r.admission || head.Count < 0 || head.Count > maxRetainedContextSteps || head.Bytes < 0 || head.Bytes > maxRetainedContextBytes || head.ExpiresAt.IsZero() {
		return ErrRetainedContextUnavailable
	}
	if head.Pending {
		return ErrRetainedContextUnsettled
	}
	if (alreadySealed && head.Terminal != sealedStatus) || (!alreadySealed && (head.Terminal != "" || !head.ExpiresAt.After(now()))) {
		return ErrRetainedContextUnavailable
	}
	r.journalID = record.ID
	query, err := json.Marshal(head.Query)
	if err != nil {
		return ErrRetainedContextUnavailable
	}
	totalBytes := len(query)
	tr := &planner.Trajectory{Query: head.Query}
	for index := range head.Count {
		frameRecord, err := store.Load(ctx, q, retainedFrameKind(index))
		if alreadySealed && errors.Is(err, state.ErrNotFound) {
			r.frameIDs = append(r.frameIDs, "") // a previous cleanup already removed it
			continue
		}
		if err != nil || frameRecord.Identity != q || frameRecord.Kind != retainedFrameKind(index) || len(frameRecord.Bytes) > maxRetainedContextBytes-totalBytes {
			return ErrRetainedContextUnavailable
		}
		var frame retainedFrame
		if err := decodeRetained(frameRecord.Bytes, &frame); err != nil {
			return err
		}
		if frame.Version != retainedJournalVersion || frame.Admission != r.admission || frame.Index != index || !frame.Settled || !frame.ExpiresAt.Equal(head.ExpiresAt) {
			return ErrRetainedContextUnavailable
		}
		// Untagged journal actions are not guessed into executable or native
		// tool calls. The shared historical validator rejects private fields
		// and ambiguous host envelopes; exact permitted evidence stays inert.
		step, err := planner.ReadHistoricalStep(planner.Step{Historical: &planner.HistoricalStep{Version: 1, SourceRun: q.RunID, Index: index, Kind: "context", Body: frame.Step}})
		if err != nil || (step.Action == nil) != frame.Context || (frame.Context && step.LLMObservation == nil) {
			return ErrRetainedContextUnavailable
		}
		tr.Steps = append(tr.Steps, step)
		r.frameIDs = append(r.frameIDs, frameRecord.ID)
		totalBytes += len(frameRecord.Bytes)
	}
	if alreadySealed {
		return r.cleanupJournal(ctx)
	}
	if totalBytes != head.Bytes || !head.ExpiresAt.After(now()) {
		return ErrRetainedContextUnavailable
	}
	// Preserve the source deadline, not a fresh TTL. Finish conditionally seals
	// this exact journal generation and removes the old active admission. A
	// concurrent new intent changes the journal ID and cannot be recovered.
	err = r.finishRetained(ctx, tr, head.Query, "", "interrupted", head.ExpiresAt, true)
	if err == nil || r.finished {
		return err
	}
	// Another terminal/reconciliation writer may have won. Treat that as an
	// idempotent success only after observing the same sealed admission; do not
	// retry an action or adopt a different run generation.
	latest, _, loadErr := r.load(ctx)
	if loadErr != nil {
		return err
	}
	for _, turn := range latest.Turns {
		if turn.Admission == r.admission && turn.ExpiresAt.After(now()) {
			return nil
		}
	}
	return err
}
