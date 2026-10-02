package steering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// UserMessageText validates the one supported clarification shape. Attachments
// and other controls do not acquire text-input receipt semantics by accident.
func UserMessageText(payload map[string]any) (string, error) {
	if err := ValidatePayload(payload); err != nil {
		return "", err
	}
	message, ok := stringFromPayload(payload, "message")
	if !ok || message == "" || len(payload) != 1 {
		return "", fmt.Errorf("%w: USER_MESSAGE requires only a nonempty message string; send attachments with a new turn", ErrPayloadInvalid)
	}
	return message, nil
}

// EnqueueInput persists admission while holding the same lock as terminal
// decision admission. A successful receipt cannot race past a sealed execution.
// Exact retries wake at most once per live inbox and never interrupt twice.
func (in *Inbox) EnqueueInput(ctx context.Context, registry tasks.TaskRegistry, ev ControlEvent) (tasks.InputReceipt, error) {
	if registry == nil || ev.Type != ControlUserMessage || ev.EventID == "" {
		return tasks.InputReceipt{}, tasks.ErrInvalidRequest
	}
	if err := in.validateEvent(ev); err != nil {
		return tasks.InputReceipt{}, err
	}
	message, validationErr := UserMessageText(ev.Payload)
	if validationErr != nil {
		return tasks.InputReceipt{}, validationErr
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var expected []uint64
	if ev.ExpectedInputRevision != nil {
		expected = []uint64{*ev.ExpectedInputRevision}
	}
	var record tasks.InputRecord
	var err error
	if in.closed || in.executionFinished || in.hardCancellation != nil {
		record, err = registry.RefuseInput(ctx, tasks.TaskID(ev.Identity.RunID), ev.EventID, message, "run_not_active", expected...)
		return record.Receipt, err
	}
	record, err = registry.AcceptInput(ctx, tasks.TaskID(ev.Identity.RunID), ev.EventID, message, expected...)
	if err != nil {
		return tasks.InputReceipt{}, err
	}
	if record.Receipt.Status != tasks.InputAccepted {
		return record.Receipt, nil
	}
	if in.inputEvents == nil {
		in.inputEvents = make(map[string]struct{})
	}
	if _, exists := in.inputEvents[ev.EventID]; exists {
		return record.Receipt, nil
	}
	ev.InputRevision = record.Receipt.Revision
	ev.Payload = map[string]any{"message": record.Message}
	if err := in.enqueueLocked(ev); err != nil {
		return tasks.InputReceipt{}, err
	}
	in.inputEvents[ev.EventID] = struct{}{}
	return record.Receipt, nil
}

// inputProjection is per-run state. Applied receipts can reconstruct data on a
// paused-task redrive, but never execute a control again or allocate a revision.
type inputProjection struct {
	revision  uint64
	projected map[string]struct{}
	pending   []ControlEvent
}

func (rl *RunLoop) restoreInputs(ctx context.Context, spec *RunSpec, inbox *Inbox) (inputProjection, error) {
	out := inputProjection{projected: make(map[string]struct{})}
	if rl.applier.taskRegistry == nil {
		return out, nil
	}
	task, err := rl.applier.taskRegistry.Get(ctx, spec.TaskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return out, nil
	} // embedded RunOnce has no task record
	if err != nil {
		return out, err
	}
	if string(task.ID) != spec.Base.Quadruple.RunID || task.Identity.Identity != spec.Base.Quadruple.Identity {
		return out, tasks.ErrNotFound
	}
	for _, record := range task.InputReceipts {
		r := record.Receipt
		if r.TaskID != task.ID || r.EventID == "" || r.Revision > task.InputRevision {
			return out, tasks.ErrInvalidRequest
		}
		ev := ControlEvent{Type: ControlUserMessage, Identity: spec.Base.Quadruple, CallerScope: ScopeOwnerUser, CallerTenant: spec.Base.Quadruple.TenantID, EventID: r.EventID, InputRevision: r.Revision, Payload: map[string]any{"message": record.Message}}
		if (r.Status == tasks.InputAccepted || r.Status == tasks.InputApplied) && (r.Revision == 0 || record.Message == "") {
			return out, tasks.ErrInvalidRequest
		}
		switch r.Status {
		case tasks.InputApplied:
			if spec.Base.Trajectory == nil {
				spec.Base.Trajectory = &planner.Trajectory{}
			}
			if !hasInputProjection(spec.Base.Trajectory, spec.Base.Quadruple.RunID, r.EventID) {
				step, err := steeringContextStep(ev)
				if err != nil {
					return out, err
				}
				if spec.TrajectoryMu != nil {
					spec.TrajectoryMu.Lock()
				}
				spec.Base.Trajectory.Steps = append(spec.Base.Trajectory.Steps, step)
				if spec.TrajectoryMu != nil {
					spec.TrajectoryMu.Unlock()
				}
			}
			out.revision = max(out.revision, r.Revision)
			out.projected[r.EventID] = struct{}{}
		case tasks.InputAccepted:
			// Requeue accepted data through the same live-inbox fence. Its
			// retained raw hash still binds retries; this is not a new request.
			inbox.mu.Lock()
			if inbox.inputEvents == nil {
				inbox.inputEvents = make(map[string]struct{})
			}
			if _, exists := inbox.inputEvents[r.EventID]; !exists {
				if err := inbox.enqueueLocked(ev); err != nil {
					inbox.mu.Unlock()
					return out, err
				}
				inbox.inputEvents[r.EventID] = struct{}{}
			}
			inbox.mu.Unlock()
		case tasks.InputDeclined, tasks.InputTerminal:
			// Retained outcome only; never project or execute it.
		default:
			return out, tasks.ErrInvalidRequest
		}
	}
	return out, nil
}

func hasInputProjection(trajectory *planner.Trajectory, taskID, eventID string) bool {
	if trajectory == nil {
		return false
	}
	for _, step := range trajectory.Steps {
		var observation struct {
			EventID string `json:"input_event_id"`
			TaskID  string `json:"input_task_id"`
		}
		if raw, err := json.Marshal(step.LLMObservation); err == nil && json.Unmarshal(raw, &observation) == nil && observation.EventID == eventID && observation.TaskID == taskID {
			return true
		}
	}
	return false
}

func (p *inputProjection) consumed(ctx context.Context, registry tasks.TaskRegistry, taskID tasks.TaskID) error {
	if len(p.pending) == 0 {
		return nil
	}
	if registry == nil {
		return tasks.ErrInvalidRequest
	}
	sort.Slice(p.pending, func(i, j int) bool { return p.pending[i].InputRevision < p.pending[j].InputRevision })
	for _, ev := range p.pending {
		if _, err := registry.MarkInputApplied(ctx, taskID, ev.EventID, ev.InputRevision); err != nil {
			return err
		}
	}
	p.pending = nil
	return nil
}

// A restarted paused inbox may restore an older accepted input after a newer
// live arrival. Reorder only durable input slots; other controls keep their
// original batch positions and cancellation arbitration remains unchanged.
func orderInputEvents(events []ControlEvent) {
	var inputs []ControlEvent
	for _, event := range events {
		if event.InputRevision != 0 {
			inputs = append(inputs, event)
		}
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].InputRevision < inputs[j].InputRevision })
	i := 0
	for j := range events {
		if events[j].InputRevision != 0 {
			events[j] = inputs[i]
			i++
		}
	}
}
