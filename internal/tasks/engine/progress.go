package engine

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/hurtener/Harbor/internal/tasks"
)

// ReportProgress implements tasks.TaskRegistry. It durably replaces
// the task's latest progress snapshot and, subject to the
// coalescing/rate policy, publishes the ordered redacted
// `task.progress` event. Full contract in the interface godoc; the
// enforcement points here are:
//
//   - Validation runs BEFORE the lock (fail fast on malformed input).
//   - Identity + existence run under the lock via lookupLocked
//     (cross-tenant / cross-session reports return ErrNotFound).
//   - The non-terminal check runs under the SAME lock Mark* holds, so
//     a progress event can never be ordered after the task's terminal
//     event: a report racing MarkComplete either lands first (and the
//     terminal event follows it) or is rejected with
//     ErrInvalidTransition after the terminal transition persisted.
//   - The snapshot is redacted before persistence; the persisted +
//     published forms are identical, so the SafePayload claim on
//     TaskProgressPayload holds by construction.
//   - A persistence failure rolls the in-memory snapshot back and
//     returns the error; a publication failure returns the error with
//     the durable snapshot standing (at-most-once, same contract as
//     Mark* publish failures).
func (e *Engine) ReportProgress(ctx context.Context, id tasks.TaskID, req tasks.ReportProgressRequest) (tasks.ProgressReportResult, error) {
	if e.closed.Load() {
		return tasks.ProgressReportResult{}, tasks.ErrRegistryClosed
	}
	if err := tasks.ValidateProgressRequest(req); err != nil {
		return tasks.ProgressReportResult{}, err
	}
	tags := normalizeProgressTags(req.Tags)

	e.mu.Lock()
	defer e.mu.Unlock()

	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return tasks.ProgressReportResult{}, err
	}
	if isTerminal(t.Status) {
		return tasks.ProgressReportResult{}, fmt.Errorf(
			"%w: progress on terminal task (status=%q)", tasks.ErrInvalidTransition, t.Status)
	}

	now := e.clock().UnixNano()
	snap := &tasks.TaskProgressSnapshot{
		Fraction:   cloneFraction(req.Fraction),
		Phase:      req.Phase,
		Message:    req.Message,
		Tags:       tags,
		ReportedAt: now,
	}
	redacted, err := e.redactProgressSnapshot(ctx, snap)
	if err != nil {
		return tasks.ProgressReportResult{}, fmt.Errorf("tasks/engine: redact progress: %w", err)
	}

	// No-op: the post-redaction snapshot is unchanged. Nothing is
	// persisted, nothing is published, and no success is claimed —
	// the identical retry path (an idempotent re-report).
	if progressSnapshotsEqual(t.Progress, redacted) {
		return tasks.ProgressReportResult{}, nil
	}

	// Coalescing/rate decision. A real phase/fraction change always
	// publishes (the window is bypassed); a message/tags-only update
	// publishes once the window since the task's last PUBLISHED event
	// has elapsed. Either way the snapshot is recorded.
	emit := progressIsRealChange(t.Progress, redacted, e.progressPolicy.FractionEpsilon)
	if !emit {
		last, hasLast := e.lastProgressEmit[id]
		if !hasLast || now-last >= int64(e.progressPolicy.MinInterval) {
			emit = true
		}
	}

	prior := t.Progress
	priorUpdated := t.UpdatedAt
	t.Progress = redacted
	t.UpdatedAt = redacted.ReportedAt // progress is observable activity
	if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		t.Progress = prior
		t.UpdatedAt = priorUpdated
		return tasks.ProgressReportResult{}, err
	}

	result := tasks.ProgressReportResult{Recorded: true, Emitted: false}
	if emit {
		if err := e.publish(ctx, t, tasks.EventTypeTaskProgress, progressPayload(t, redacted)); err != nil {
			// The snapshot is durably recorded; the event is dropped
			// at-most-once. Surface the failure — the caller cannot
			// claim success — exactly as Mark* publish failures do.
			return tasks.ProgressReportResult{}, err
		}
		e.lastProgressEmit[id] = redacted.ReportedAt
		result.Emitted = true
	}
	return result, nil
}

// progressPayload builds the SafePayload task.progress event body for
// a durably recorded snapshot. The parent-task id is read from the
// live Task record; the runtime-owned Event envelope metadata
// (identity quadruple, OccurredAt, Sequence) is added by publish.
func progressPayload(t *tasks.Task, snap *tasks.TaskProgressSnapshot) tasks.TaskProgressPayload {
	p := tasks.TaskProgressPayload{
		TaskID:     t.ID,
		Fraction:   cloneFraction(snap.Fraction),
		Phase:      snap.Phase,
		Message:    snap.Message,
		ReportedAt: snap.ReportedAt,
	}
	if t.ParentTaskID != nil {
		p.ParentTaskID = *t.ParentTaskID
	}
	if len(snap.Tags) > 0 {
		p.Tags = append([]string(nil), snap.Tags...)
	}
	return p
}

// redactProgressSnapshot runs the caller-controlled Phase / Message /
// Tags through the configured audit redactor — the same redaction
// Description / Query take at Spawn. The snapshot is stored AND
// published in this redacted form, so the TaskProgressPayload's
// SafePayload claim holds by construction.
//
// Redaction happens BEFORE the no-op comparison, so a report whose
// raw content differs only in secret-shaped tokens (which redact to
// the same placeholder) is honestly a no-op: the observable persisted
// + published form did not change.
func (e *Engine) redactProgressSnapshot(ctx context.Context, snap *tasks.TaskProgressSnapshot) (*tasks.TaskProgressSnapshot, error) {
	out := &tasks.TaskProgressSnapshot{
		Fraction:   cloneFraction(snap.Fraction),
		Phase:      snap.Phase,
		Message:    snap.Message,
		ReportedAt: snap.ReportedAt,
	}
	if snap.Phase != "" {
		phase, err := e.redactString(ctx, snap.Phase)
		if err != nil {
			return nil, fmt.Errorf("phase: %w", err)
		}
		out.Phase = phase
	}
	if snap.Message != "" {
		msg, err := e.redactString(ctx, snap.Message)
		if err != nil {
			return nil, fmt.Errorf("message: %w", err)
		}
		out.Message = msg
	}
	if len(snap.Tags) > 0 {
		out.Tags = make([]string, 0, len(snap.Tags))
		for _, tag := range snap.Tags {
			redacted, err := e.redactString(ctx, tag)
			if err != nil {
				return nil, fmt.Errorf("tag: %w", err)
			}
			out.Tags = append(out.Tags, redacted)
		}
	}
	return out, nil
}

// normalizeProgressTags trims whitespace, drops empty entries, and
// collapses duplicates in first-seen order — the normalization the
// "uniqueness" leg of the change detection runs on. A report whose
// only difference from the current snapshot is tag reordering or
// duplication is therefore a no-op, never a change.
func normalizeProgressTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, tag := range in {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cloneFraction copies the pointee so a stored snapshot never aliases
// the caller's request float (or the engine's live record).
func cloneFraction(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

// progressSnapshotsEqual reports whether two post-redaction snapshots
// are observably identical. It is the no-op predicate for
// ReportProgress: equality means "record nothing, publish nothing".
//
// ReportedAt is deliberately EXCLUDED from the comparison — it is
// runtime bookkeeping (the record instant), not an observable caller
// field, so an identical-content re-report is an idempotent retry,
// never a fresh record (the Prioritize same-value precedent).
func progressSnapshotsEqual(a, b *tasks.TaskProgressSnapshot) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Phase != b.Phase || a.Message != b.Message {
		return false
	}
	if !fractionEqual(a.Fraction, b.Fraction) {
		return false
	}
	return progressTagsEqual(a.Tags, b.Tags)
}

// fractionEqual reports equality of two optional fraction pointers.
func fractionEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// progressTagsEqual reports set-equality of two normalized tag slices
// (both are normalized first-seen-order, so order + length equality
// is sufficient).
func progressTagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// progressIsRealChange reports whether next differs from prev in a way
// the rate policy must never coalesce: a phase change or a fraction
// change of at least `epsilon` (or a fraction-presence change). The
// first report on a task is always real.
func progressIsRealChange(prev, next *tasks.TaskProgressSnapshot, epsilon float64) bool {
	if prev == nil {
		return true
	}
	if next == nil {
		// Defensive: ReportProgress never passes a nil next; treat it
		// as a real change rather than silently dropping it.
		return true
	}
	if prev.Phase != next.Phase {
		return true
	}
	return fractionDeltaIsReal(prev.Fraction, next.Fraction, epsilon)
}

// fractionDeltaIsReal reports whether the fraction moved enough to be
// a real change: |Δ| ≥ epsilon, or a nil↔non-nil presence change.
// Two nils are never a real change.
func fractionDeltaIsReal(a, b *float64, epsilon float64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true // presence changed
	}
	return math.Abs(*a-*b) >= epsilon
}
