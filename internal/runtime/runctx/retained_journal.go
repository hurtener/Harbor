package runctx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	retainedJournalKind    = state.InternalKindPrefix + "session-execution-journal"
	retainedJournalVersion = 1
)

type retainedJournal struct {
	Version   int               `json:"version"`
	Admission retainedAdmission `json:"admission"`
	Query     string            `json:"query"`
	Count     int               `json:"count"`
	Bytes     int               `json:"bytes"`
	Pending   bool              `json:"pending"`
	Terminal  string            `json:"terminal,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type retainedFrame struct {
	Version   int               `json:"version"`
	Admission retainedAdmission `json:"admission"`
	Index     int               `json:"index"`
	Settled   bool              `json:"settled"`
	Context   bool              `json:"context,omitempty"`
	Step      json.RawMessage   `json:"step"`
	ExpiresAt time.Time         `json:"expires_at"`
}

func retainedFrameKind(index int) string {
	return fmt.Sprintf("%s/action/%03d", retainedJournalKind, index)
}

// Start commits the admitted query before model or tool work. It creates a
// private run-local journal, not another public transcript. Each later dispatch
// writes only one bounded frame plus this small head through SaveBatchIf.
func (r *RetainedRun) Start(ctx context.Context, base planner.RunContext) (err error) {
	defer func() { r.rememberJournalFailure(err) }()
	if base.Quadruple != r.q || base.Trajectory == nil || r.finished || r.journalID != "" {
		return ErrRetainedContextUnavailable
	}
	query, err := r.redactJournalValue(ctx, base.Query)
	if err != nil {
		return err
	}
	var safeQuery string
	if err := decodeRetained(query, &safeQuery); err != nil {
		return err
	}
	head := retainedJournal{
		Version: retainedJournalVersion, Admission: r.admission,
		Query: safeQuery, Bytes: len(query), ExpiresAt: r.now().Add(r.ttl),
	}
	return r.commitJournal(ctx, head, nil, "")
}

// BeforeDispatch commits intent before any external invocation. A frame without
// a settlement means outcome unknown, not failed. A parallel decision is one
// complete exchange: partial branch execution remains unknown after a crash.
func (r *RetainedRun) BeforeDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) (err error) {
	defer func() { r.rememberJournalFailure(err) }()
	if err := r.checkJournal(rc); err != nil {
		return err
	}
	if r.journal.Pending || step.Action == nil {
		return ErrRetainedContextUnavailable
	}
	if r.journal.Count >= maxRetainedContextSteps {
		return ErrRetainedContextCapacity
	}
	frame, err := r.makeFrame(ctx, r.journal.Count, false, step)
	if err != nil {
		return err
	}
	head := r.journal
	head.Count++
	head.Pending = true
	head.Bytes += len(frame)
	return r.commitJournal(ctx, head, frame, "")
}

// AfterDispatch commits the entire permitted result/error before a dependent
// model decision. Its context may be a bounded post-cancellation persistence
// context, but it can never start or retry an external action.
func (r *RetainedRun) AfterDispatch(ctx context.Context, rc planner.RunContext, step planner.Step) (err error) {
	defer func() { r.rememberJournalFailure(err) }()
	if err := r.checkJournal(rc); err != nil {
		return err
	}
	if !r.journal.Pending || len(r.frameIDs) != r.journal.Count {
		return ErrRetainedContextUnavailable
	}
	index := r.journal.Count - 1
	old, err := r.store.Load(ctx, r.q, retainedFrameKind(index))
	if err != nil {
		return fmt.Errorf("%w: load dispatch intent: %w", ErrRetainedContextUnavailable, err)
	}
	if old.ID != r.frameIDs[index] || old.Identity != r.q || old.Kind != retainedFrameKind(index) {
		return ErrRetainedContextUnavailable
	}
	frame, err := r.makeFrame(ctx, index, true, step)
	if err != nil {
		return err
	}
	var before, after retainedFrame
	if err := decodeRetained(old.Bytes, &before); err != nil {
		return err
	}
	if err := decodeRetained(frame, &after); err != nil {
		return err
	}
	var intentStep, settledStep planner.Step
	if err := decodeRetained(before.Step, &intentStep); err != nil {
		return err
	}
	if err := decodeRetained(after.Step, &settledStep); err != nil {
		return err
	}
	intentAction, intentErr := json.Marshal(intentStep.Action)
	settledAction, settledErr := json.Marshal(settledStep.Action)
	if intentErr != nil || settledErr != nil || before.Version != retainedJournalVersion ||
		before.Admission != r.admission || before.Index != index || before.Settled || before.Context ||
		!bytes.Equal(intentAction, settledAction) {
		return ErrRetainedContextUnavailable
	}
	head := r.journal
	head.Pending = false
	head.Bytes += len(frame) - len(old.Bytes)
	return r.commitJournal(ctx, head, frame, old.ID)
}

// RecordContext commits one applied, non-executable context update. It shares
// the ordered bounded journal with dispatches; a failed write cannot be repaired
// by continuing with forgotten instructions. The caller appends the same step
// to the live trajectory only after this write succeeds.
func (r *RetainedRun) RecordContext(ctx context.Context, rc planner.RunContext, step planner.Step) (err error) {
	defer func() { r.rememberJournalFailure(err) }()
	if err := r.checkJournal(rc); err != nil {
		return err
	}
	if r.journal.Pending || step.Action != nil || step.LLMObservation == nil || step.Historical != nil ||
		step.Observation != nil || step.ReasoningTrace != "" || step.AssistantPreamble != "" || step.Streams != nil || step.Failure != nil || step.Error != "" {
		return ErrRetainedContextUnavailable
	}
	if r.journal.Count >= maxRetainedContextSteps {
		return ErrRetainedContextCapacity
	}
	frame, err := r.makeFrame(ctx, r.journal.Count, true, step)
	if err != nil {
		return err
	}
	head := r.journal
	head.Count++
	head.Bytes += len(frame)
	return r.commitJournal(ctx, head, frame, "")
}

func (r *RetainedRun) checkJournal(rc planner.RunContext) error {
	if rc.Quadruple != r.q || r.finished || r.journalID == "" || r.journalFailure != nil {
		return ErrRetainedContextUnavailable
	}
	return nil
}

func (r *RetainedRun) rememberJournalFailure(err error) {
	if err != nil && r.journalFailure == nil {
		r.journalFailure = err
	}
}

func (r *RetainedRun) makeFrame(ctx context.Context, index int, settled bool, step planner.Step) ([]byte, error) {
	evidence, err := r.redactJournalValue(ctx, trajectory.ModelStep(step))
	if err != nil {
		return nil, err
	}
	checked, err := planner.ReadHistoricalStep(planner.Step{Historical: &planner.HistoricalStep{
		Version: 1, SourceRun: r.q.RunID, Index: index, Kind: "context", Body: evidence,
	}})
	if err != nil {
		return nil, ErrRetainedContextUnavailable
	}
	if (checked.Action == nil) != (step.Action == nil) || (checked.Action == nil && (!settled || checked.LLMObservation == nil)) {
		return nil, ErrRetainedContextUnavailable
	}
	return json.Marshal(retainedFrame{
		Version: retainedJournalVersion, Admission: r.admission, Index: index,
		Settled: settled, Context: checked.Action == nil, Step: evidence, ExpiresAt: r.journal.ExpiresAt,
	})
}

func (r *RetainedRun) redactJournalValue(ctx context.Context, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrRetainedContextUnavailable
	}
	if len(encoded) > maxRetainedContextBytes {
		return nil, ErrRetainedContextCapacity
	}
	var tree any
	if err := decodeRetained(encoded, &tree); err != nil {
		return nil, err
	}
	safe, err := r.redactor.Redact(ctx, tree)
	if err != nil {
		return nil, fmt.Errorf("%w: dispatch redaction: %w", ErrRetainedContextUnavailable, err)
	}
	if safe == nil {
		return nil, ErrRetainedContextUnavailable
	}
	encoded, err = json.Marshal(safe)
	if err != nil {
		return nil, ErrRetainedContextUnavailable
	}
	if len(encoded) > maxRetainedContextBytes {
		return nil, ErrRetainedContextCapacity
	}
	return encoded, nil
}

func (r *RetainedRun) commitJournal(ctx context.Context, head retainedJournal, frame []byte, previousFrame state.EventID) error {
	body, err := json.Marshal(head)
	if err != nil || head.Bytes < 0 || head.Bytes+len(body) > maxRetainedContextBytes {
		return ErrRetainedContextCapacity
	}
	next := state.NewInternalRecord(state.NewEventID(), r.q, retainedJournalKind, body)
	var nextFrame state.StateRecord
	if frame != nil {
		nextFrame = state.NewInternalRecord(state.NewEventID(), r.q, retainedFrameKind(head.Count-1), frame)
	}
	for range retainedContextAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.prefixExpiresAt.IsZero() && !r.prefixExpiresAt.After(r.now()) {
			return ErrRetainedContextUnavailable
		}
		window, windowID, err := r.load(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, active := range window.Active {
			if active == r.admission {
				found = true
				break
			}
		}
		if !found {
			return ErrRetainedContextUnavailable
		}
		predicates, err := r.erasurePredicates()
		if err != nil {
			return err
		}
		predicates = append(predicates,
			state.InternalSlotExpectation(identity.Quadruple{Identity: r.q.Identity}, retainedContextKind, windowID),
			state.InternalSlotExpectation(r.q, retainedJournalKind, r.journalID))
		writes := []state.StateRecord{next}
		if frame != nil {
			predicates = append(predicates, state.InternalSlotExpectation(r.q, nextFrame.Kind, previousFrame))
			writes = append(writes, nextFrame)
		}
		if err := r.store.SaveBatchIf(ctx, predicates, writes); errors.Is(err, state.ErrConditionFailed) {
			continue
		} else if err != nil {
			return fmt.Errorf("%w: commit dispatch checkpoint: %w", ErrRetainedContextUnavailable, err)
		}
		r.journal, r.journalID = head, next.ID
		if frame != nil {
			if previousFrame == "" {
				r.frameIDs = append(r.frameIDs, nextFrame.ID)
			} else {
				r.frameIDs[head.Count-1] = nextFrame.ID
			}
		}
		return nil
	}
	return ErrRetainedContextUnavailable
}

// Seal the journal and publish the terminal window in one transaction. Never
// acknowledge a terminal turn over a pending or uncommitted dispatch outcome.
func (r *RetainedRun) saveTerminal(ctx context.Context, previous state.EventID, window retainedWindow, status string) error {
	if r.journalID == "" {
		return r.save(ctx, previous, window)
	}
	data, err := json.Marshal(window)
	if err != nil || len(data) > maxRetainedContextBytes {
		return ErrRetainedContextCapacity
	}
	head := r.journal
	head.Terminal = status
	body, err := json.Marshal(head)
	if err != nil {
		return ErrRetainedContextUnavailable
	}
	predicates, err := r.erasurePredicates()
	if err != nil {
		return err
	}
	q := identity.Quadruple{Identity: r.q.Identity}
	predicates = append(predicates,
		state.InternalSlotExpectation(q, retainedContextKind, previous),
		state.InternalSlotExpectation(r.q, retainedJournalKind, r.journalID))
	next := state.NewInternalRecord(state.NewEventID(), r.q, retainedJournalKind, body)
	if err := r.store.SaveBatchIf(ctx, predicates, []state.StateRecord{
		state.NewInternalRecord(state.NewEventID(), q, retainedContextKind, data), next,
	}); err != nil {
		return err
	}
	r.journal, r.journalID = head, next.ID
	return nil
}

// Committed terminal evidence now lives in the bounded session window. Remove
// transient frames by exact generation; erasure is allowed to have removed them
// already. A cleanup error is explicit and never permission to replay actions.
func (r *RetainedRun) cleanupJournal(ctx context.Context) error {
	if r.journalID == "" {
		return nil
	}
	for i, id := range r.frameIDs {
		if id == "" {
			continue // an idempotent reconciliation observed prior cleanup
		}
		if _, err := r.store.DeleteIf(ctx, state.InternalSlotExpectation(r.q, retainedFrameKind(i), id)); err != nil {
			return fmt.Errorf("%w: cleanup dispatch frame: %w", ErrRetainedContextUnavailable, err)
		}
	}
	if _, err := r.store.DeleteIf(ctx, state.InternalSlotExpectation(r.q, retainedJournalKind, r.journalID)); err != nil {
		return fmt.Errorf("%w: cleanup dispatch head: %w", ErrRetainedContextUnavailable, err)
	}
	return nil
}
