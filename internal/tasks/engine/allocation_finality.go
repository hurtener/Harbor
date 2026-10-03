package engine

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
)

// InferenceAllocationFinality advertises the built-in terminal funding barrier.
// Storage durability remains a separate negotiated/runtime property.
func (e *Engine) InferenceAllocationFinality() bool { return e.allocations != nil }

// closeTerminalAllocationLocked treats candidate as about to become terminal.
// Root completion alone cannot cut off an accepted background descendant: every
// accepted member of the exact identity/funding root must also be terminal.
// Provider calls already admitted before Close retain their complete envelope.
// Caller holds e.mu, which also serializes new descendant acceptance.
func (e *Engine) closeTerminalAllocationLocked(ctx context.Context, candidate *tasks.Task) error {
	if e.allocations == nil || candidate.InferenceAllocation == nil {
		return nil
	}
	rootID := tasks.TaskID(candidate.AllocationTaskID)
	if rootID == "" {
		rootID = candidate.ID
	}
	root, ok := e.tasks[rootID]
	if !ok || !identitiesEqual(root.Identity.Identity, candidate.Identity.Identity) || !llm.EqualInferenceAllocation(root.InferenceAllocation, candidate.InferenceAllocation) {
		return llm.ErrAllocationInvalid
	}
	if root.ID != candidate.ID && !isTerminal(root.Status) {
		return nil
	}
	for _, member := range e.tasks {
		if member.ID == candidate.ID || !identitiesEqual(member.Identity.Identity, root.Identity.Identity) {
			continue
		}
		if member.ID != rootID && member.AllocationTaskID != string(rootID) {
			continue
		}
		if !llm.EqualInferenceAllocation(member.InferenceAllocation, root.InferenceAllocation) {
			return llm.ErrAllocationInvalid
		}
		if !isTerminal(member.Status) {
			return nil
		}
	}
	q := root.Identity
	q.RunID = string(root.ID)
	if err := e.allocations.Close(ctx, q, *root.InferenceAllocation); err != nil {
		return fmt.Errorf("close terminal task allocation: %w", err)
	}
	return nil
}
