package protocol

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/flow"
)

// LaunchFunc is the runtime-supplied launcher the FuncInvoker wraps. It
// launches a one-shot run of the named flow under the caller's identity
// and returns the accepted run's identifier + start time. The runtime
// binds this to its real task-registry `start` path (Phase 54) — the
// FuncInvoker never reaches the task registry directly, so the Flows-
// page surface stays decoupled from the task subsystem's concrete type.
//
// A LaunchFunc that targets an unknown flow returns an error wrapping
// ErrNotFound; a malformed input form returns an error wrapping
// ErrInvalidRequest. Any other failure is wrapped as ErrRuntime by the
// Surface's classifyCatalogErr.
type LaunchFunc func(ctx context.Context, id identity.Identity, flowID string, inputs map[string]any) (runID string, startedAt time.Time, err error)

// FuncInvoker is the production Invoker implementation. It adapts a
// runtime-supplied LaunchFunc onto the Invoker interface. It is NOT a
// test stub (CLAUDE.md §13): the binary binds the LaunchFunc to the
// real `start` path; the FuncInvoker is the thin adapter that keeps the
// Flows-page surface decoupled from the task subsystem.
//
// Concurrent reuse (D-025): the FuncInvoker is a compiled artifact —
// the LaunchFunc is set once at construction and never mutated.
type FuncInvoker struct {
	launch   LaunchFunc
	registry *flow.Registry
}

// NewFuncInvoker builds the production Invoker over a runtime-supplied
// LaunchFunc + the flow Registry. launch is mandatory — a nil fails
// loud with ErrMisconfigured. registry is mandatory: the invoker
// rejects a run targeting an unregistered flow before reaching the
// launcher, so an unknown flow id is a clean CodeNotFound rather than
// an opaque launcher failure.
//
// The returned *FuncInvoker is immutable after construction and safe
// for concurrent use by N goroutines.
func NewFuncInvoker(launch LaunchFunc, registry *flow.Registry) (*FuncInvoker, error) {
	if launch == nil {
		return nil, fmt.Errorf("%w: LaunchFunc is nil", ErrMisconfigured)
	}
	if registry == nil {
		return nil, fmt.Errorf("%w: flow.Registry is nil", ErrMisconfigured)
	}
	return &FuncInvoker{launch: launch, registry: registry}, nil
}

// Invoke launches a one-shot run of flowID under the caller's identity.
// It rejects an unregistered flow id with ErrNotFound before reaching
// the launcher.
func (i *FuncInvoker) Invoke(ctx context.Context, id identity.Identity, flowID string, inputs map[string]any) (prototypes.FlowRunResponse, error) {
	if _, _, ok := i.registry.Definition(flowID); !ok {
		return prototypes.FlowRunResponse{}, fmt.Errorf("%w: flow %q", ErrNotFound, flowID)
	}
	runID, startedAt, err := i.launch(ctx, id, flowID, inputs)
	if err != nil {
		return prototypes.FlowRunResponse{}, err
	}
	return prototypes.FlowRunResponse{
		RunID:     runID,
		Status:    prototypes.FlowRunRunning,
		StartedAt: startedAt,
	}, nil
}

// compile-time assertion that FuncInvoker satisfies Invoker.
var _ Invoker = (*FuncInvoker)(nil)
