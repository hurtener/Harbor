package steering

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

var errDecisionSuperseded = errors.New("superseded before dispatch; action was not executed")

func (in *Inbox) fenceInvocation(ctx context.Context, generation uint64) (context.Context, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.generationCurrentLocked(generation) {
		return ctx, errDecisionSuperseded
	}
	if in.invocationInvalidated == nil {
		in.invocationInvalidated = make(chan struct{})
	}
	return tools.WithInvocationFence(ctx, in.invocationInvalidated), nil
}

// A concurrent correction cannot mask a persistence/accounting failure joined
// with cancellation. Only a pure cancellation chain is safe to re-plan.
func onlyAttemptCancellation(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyAttemptCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return onlyAttemptCancellation(wrapped)
	}
	return errors.Is(err, context.Canceled)
}

func (in *Inbox) generationCurrentLocked(generation uint64) bool {
	return !in.closed && !in.executionFinished && in.hardCancellation == nil && generation == in.steerGeneration
}

// Only the active planning attempt is interrupted by a user correction.
// Already-admitted tool execution keeps its outcome and settles normally.
func (in *Inbox) beginAttempt(generation uint64, cancel context.CancelFunc) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.generationCurrentLocked(generation) {
		return false
	}
	in.cancelAttempt = cancel
	return true
}

func (in *Inbox) endAttempt(generation uint64) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.cancelAttempt = nil
	return in.generationCurrentLocked(generation)
}

// Normal completion still allows accepted callbacks to reach the existing
// terminal chunk seal. Superseded attempts and hard Stop do not.
func (in *Inbox) acceptsOutput(generation uint64) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return !in.closed && in.hardCancellation == nil && generation == in.steerGeneration
}

// Admission and steering share a lock. A correction accepted before this
// boundary invalidates the decision; after tool admission it applies to the
// next decision. Terminal admission closes controls immediately.
func (in *Inbox) admitDecision(generation uint64, terminal bool) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.generationCurrentLocked(generation) {
		return false
	}
	if terminal {
		in.executionFinished = true
	}
	return true
}

// Preserve inputs consumed by an interrupted attempt, not the obsolete plan.
// Applied controls remain in the trajectory; they are not applied again.
func carryInterruptedAttempt(base *planner.RunContext, rc planner.RunContext) {
	base.Control = rc.Control
	base.InputArtifacts = rc.InputArtifacts
	base.PendingToolCalls = nil
}
