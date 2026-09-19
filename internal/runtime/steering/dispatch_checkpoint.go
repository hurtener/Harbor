package steering

import (
	"context"

	"github.com/hurtener/Harbor/internal/planner"
)

// DispatchCheckpoint commits intent before execution and a settled exchange
// before another decision may depend on it. Failures are fatal persistence
// errors, never tool errors for the planner to repair by repeating the action.
// Calls are serialized by the run loop. Implementations must not execute tools.
type DispatchCheckpoint interface {
	BeforeDispatch(context.Context, planner.RunContext, planner.Step) error
	AfterDispatch(context.Context, planner.RunContext, planner.Step) error
}
