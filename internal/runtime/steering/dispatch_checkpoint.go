package steering

import (
	"context"

	"github.com/hurtener/Harbor/internal/planner"
)

// DispatchCheckpoint commits intent before execution and a settled exchange
// before another decision may depend on it. Failures are fatal persistence
// errors, never tool errors for the planner to repair by repeating the action.
// Context updates use the same journal before subsequent decisions.
// Calls are serialized by the run loop. Implementations must not execute tools.
type DispatchCheckpoint interface {
	RecordContext(context.Context, planner.RunContext, planner.Step) error
	BeforeDispatch(context.Context, planner.RunContext, planner.Step) error
	AfterDispatch(context.Context, planner.RunContext, planner.Step) error
}
