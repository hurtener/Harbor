package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/tasks"
)

func inputHash(eventID, message string, expectedRevision ...uint64) (string, error) {
	if eventID == "" || !utf8.ValidString(eventID) || len(eventID) > 128 || strings.TrimSpace(eventID) != eventID || message == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > 4096 {
		return "", fmt.Errorf("%w: bounded event id and nonempty text are required", tasks.ErrInvalidRequest)
	}
	if len(expectedRevision) > 1 {
		return "", tasks.ErrInvalidRequest
	}
	var expected *uint64
	if len(expectedRevision) == 1 {
		value := expectedRevision[0]
		expected = &value
	}
	payload, err := json.Marshal(struct {
		Message          string
		ExpectedRevision *uint64
	}{message, expected})
	if err != nil {
		return "", fmt.Errorf("encode input identity: %w", err)
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

// AcceptInput persists one exact text input before inbox admission acknowledges it.
func (e *Engine) AcceptInput(ctx context.Context, id tasks.TaskID, eventID, message string, expectedRevision ...uint64) (tasks.InputRecord, error) {
	return e.recordInput(ctx, id, eventID, message, "", expectedRevision...)
}

// RefuseInput records an unaccepted request when its exact execution is unavailable.
// A known receipt wins, including an accepted request whose acknowledgement was lost.
func (e *Engine) RefuseInput(ctx context.Context, id tasks.TaskID, eventID, message, reason string, expectedRevision ...uint64) (tasks.InputRecord, error) {
	if reason != "run_not_active" {
		return tasks.InputRecord{}, tasks.ErrInvalidRequest
	}
	return e.recordInput(ctx, id, eventID, message, reason, expectedRevision...)
}

func (e *Engine) recordInput(ctx context.Context, id tasks.TaskID, eventID, message, refusal string, expectedRevision ...uint64) (tasks.InputRecord, error) {
	if err := ctx.Err(); err != nil {
		return tasks.InputRecord{}, err
	}
	hash, err := inputHash(eventID, message, expectedRevision...)
	if err != nil {
		return tasks.InputRecord{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return tasks.InputRecord{}, err
	}
	for _, record := range t.InputReceipts {
		if record.Receipt.EventID == eventID {
			if record.Receipt.PayloadHash != hash {
				return tasks.InputRecord{}, tasks.ErrIdempotencyConflict
			}
			return record, nil
		}
	}
	if len(expectedRevision) == 1 && t.InputRevision != expectedRevision[0] {
		return tasks.InputRecord{}, tasks.ErrInputRevisionConflict
	}
	if len(t.InputReceipts) >= tasks.MaxInputReceipts {
		return tasks.InputRecord{}, tasks.ErrInputReceiptCapacity
	}
	redacted, err := e.redactString(ctx, message)
	if err != nil {
		return tasks.InputRecord{}, fmt.Errorf("tasks: redact input: %w", err)
	}
	now := time.Now().UnixNano()
	r := tasks.InputRecord{Message: redacted, Receipt: tasks.InputReceipt{EventID: eventID, TaskID: id, PayloadHash: hash}}
	switch {
	case isTerminal(t.Status):
		r.Receipt.Status, r.Receipt.Reason, r.Receipt.TerminalAt = tasks.InputTerminal, string(t.Status), now
	case refusal != "" || (t.Status != tasks.StatusRunning && t.Status != tasks.StatusPaused):
		r.Receipt.Status, r.Receipt.Reason, r.Receipt.TerminalAt = tasks.InputDeclined, "run_not_active", now
	default:
		if t.InputRevision == ^uint64(0) {
			return tasks.InputRecord{}, tasks.ErrInputReceiptCapacity
		}
		r.Receipt.Status, r.Receipt.Revision, r.Receipt.AcceptedAt = tasks.InputAccepted, t.InputRevision+1, now
	}
	if r.Receipt.Status != tasks.InputAccepted {
		r.Message = ""
	} else if redacted == "" || !utf8.ValidString(redacted) || utf8.RuneCountInString(redacted) > 4096 {
		return tasks.InputRecord{}, fmt.Errorf("%w: redacted input is not bounded nonempty text", tasks.ErrInvalidRequest)
	}
	prior := *t
	t.InputReceipts = append(append([]tasks.InputRecord(nil), t.InputReceipts...), r)
	if r.Receipt.Revision != 0 {
		t.InputRevision = r.Receipt.Revision
	}
	t.UpdatedAt = now
	if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		*t = prior
		return tasks.InputRecord{}, err
	}
	return r, nil
}

// GetInputReceipt is available after completion, cancellation, and restart.
func (e *Engine) GetInputReceipt(ctx context.Context, id tasks.TaskID, eventID string) (tasks.InputReceipt, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return tasks.InputReceipt{}, err
	}
	for _, record := range t.InputReceipts {
		if record.Receipt.EventID == eventID {
			return record.Receipt, nil
		}
	}
	return tasks.InputReceipt{}, tasks.ErrInputReceiptNotFound
}

// MarkInputApplied commits consumption only for the accepted immutable revision.
// The run loop calls it after a planner invocation, before executing its decision.
func (e *Engine) MarkInputApplied(ctx context.Context, id tasks.TaskID, eventID string, revision uint64) (tasks.InputReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return tasks.InputReceipt{}, err
	}
	for i, record := range t.InputReceipts {
		if record.Receipt.EventID != eventID {
			continue
		}
		if revision == 0 || record.Receipt.Revision != revision {
			return tasks.InputReceipt{}, tasks.ErrIdempotencyConflict
		}
		if record.Receipt.Status == tasks.InputApplied {
			return record.Receipt, nil
		}
		if record.Receipt.Status != tasks.InputAccepted || isTerminal(t.Status) {
			return record.Receipt, tasks.ErrInvalidTransition
		}
		for _, prior := range t.InputReceipts {
			if prior.Receipt.Status == tasks.InputAccepted && prior.Receipt.Revision < revision {
				return tasks.InputReceipt{}, tasks.ErrInvalidTransition
			}
		}
		prior := *t
		t.InputReceipts = append([]tasks.InputRecord(nil), t.InputReceipts...)
		record.Receipt.Status, record.Receipt.AppliedAt = tasks.InputApplied, time.Now().UnixNano()
		t.InputReceipts[i] = record
		t.AppliedInputRevision = max(t.AppliedInputRevision, revision)
		t.UpdatedAt = record.Receipt.AppliedAt
		if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
			*t = prior
			return tasks.InputReceipt{}, err
		}
		return record.Receipt, nil
	}
	return tasks.InputReceipt{}, tasks.ErrInputReceiptNotFound
}

func terminalInputReceipts(records []tasks.InputRecord, status tasks.TaskStatus, now int64) []tasks.InputRecord {
	out := append([]tasks.InputRecord(nil), records...)
	for i := range out {
		if out[i].Receipt.Status == tasks.InputAccepted {
			out[i].Receipt.Status, out[i].Receipt.Reason, out[i].Receipt.TerminalAt = tasks.InputTerminal, string(status), now
		}
	}
	return out
}
