package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Invocation is one descriptor invocation, including its ordinary retry policy.
type Invocation = func(context.Context, json.RawMessage) (ToolResult, error)

// InvocationDecorator adds runtime bookkeeping at the actual invocation edge.
type InvocationDecorator func(Invocation) Invocation

type invocationBoundaryKey struct{}
type invocationDecoration struct {
	name string
	wrap InvocationDecorator
}

// DecorateInvocation always executes the descriptor's actual Invoke chain.
// A copied descriptor may have acquired an external gate or a simulation since
// registration; reconstructing an older factory would silently bypass it.
func DecorateInvocation(d ToolDescriptor, wrap InvocationDecorator) ToolDescriptor {
	d = WithInvocationBoundary(d)
	outer := d.Invoke
	name := d.Tool.Name
	out := d
	out.Invoke = func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
		return outer(context.WithValue(ctx, invocationBoundaryKey{}, invocationDecoration{name, wrap}), args)
	}
	return out
}

// WithInvocationBoundary seats a leaf boundary once. Existing outer wrappers
// remain intact. Drivers with local admission use DeclareInvocationBoundary and
// call InvokeAtBoundary only after those preconditions succeed.
func WithInvocationBoundary(d ToolDescriptor) ToolDescriptor {
	if d.invocationBoundary || d.Invoke == nil {
		return d
	}
	inner := d.Invoke
	name := d.Tool.Name
	d.invocationBoundary = true
	d.Invoke = func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
		return InvokeAtBoundary(ctx, name, inner, args)
	}
	return d
}

// DeclareInvocationBoundary marks a driver-owned boundary inside Invoke.
func DeclareInvocationBoundary(d ToolDescriptor) ToolDescriptor {
	d.invocationBoundary = true
	return d
}

// InvokeAtBoundary applies one per-call decorator after local admission.
// The decorator is consumed in the child context; helpers never inherit it.
func InvokeAtBoundary(ctx context.Context, name string, invoke Invocation, args json.RawMessage) (ToolResult, error) {
	decoration, ok := ctx.Value(invocationBoundaryKey{}).(invocationDecoration)
	if !ok || decoration.wrap == nil {
		return invoke(ctx, args)
	}
	if decoration.name != name {
		return ToolResult{}, fmt.Errorf("%w: invocation boundary descriptor changed", ErrInvocationCleanupFailed)
	}
	return decoration.wrap(invoke)(context.WithValue(ctx, invocationBoundaryKey{}, invocationDecoration{}), args)
}

// WrapInvocationGate composes admission/observability around a descriptor while
// preserving the runtime boundary inside it. It never mutates shared state.
func WrapInvocationGate(d ToolDescriptor, gate InvocationDecorator) ToolDescriptor {
	d = WithInvocationBoundary(d)
	out := d
	out.Invoke = gate(d.Invoke)
	return out
}
