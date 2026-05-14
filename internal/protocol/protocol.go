// Package protocol is the Harbor Protocol layer's runtime-side surface —
// the transport-agnostic handlers that translate a Protocol method call
// into a runtime action. Phase 54 ships the **task control surface**:
// the ControlSurface type, which maps the ten canonical task-control
// methods (internal/protocol/methods) onto the already-shipped runtime.
//
// # The decoupling rule (RFC §5.1, CLAUDE.md §8)
//
// The Console is a Protocol client; it never reads Runtime internals.
// The Protocol surface is the contract. ControlSurface is the
// runtime-side half of the task-control contract: it accepts the
// Protocol wire types (internal/protocol/types — flat, Protocol-owned
// structs, never re-exports of runtime Go types), reaches the runtime
// ONLY through the public Phase 20 tasks.TaskRegistry + Phase 52/53
// steering.Registry surfaces, and returns Protocol wire types. A
// Protocol method that mapped 1:1 onto an internal Go signature would be
// the RFC §5.1 reject-on-sight smell — the control methods deliberately
// take a flat IdentityScope + payload map, and ControlSurface does the
// translation.
//
// # Transport-agnostic — the wire transport is Phase 60
//
// RFC §5.4 leaves the wire transport (SSE+REST-leaning) not-yet-locked,
// and says "the relevant phase blocks until it resolves." Phase 54 takes
// the explicit consequence: it ships the transport-AGNOSTIC surface now.
// ControlSurface.Dispatch(ctx, method, req) is a plain Go entry point —
// a Phase 60 HTTP/SSE handler is a thin adapter that decodes a request,
// calls Dispatch, and encodes the response (or maps a *errors.Error onto
// an HTTP status). The whole surface is in-process-invocable and
// testable today, which is what lets the Wave 9 E2E exercise it as a
// real §13 consumer.
//
// # Identity scope is enforced at the edge (RFC §5.5, CLAUDE.md §6)
//
// Every method fails closed on an incomplete identity triple — RFC §5.5:
// "the Protocol rejects any request without an identity scope." The nine
// steering-control methods additionally require a run id (they target a
// specific run's inbox) and run the Phase 52 per-event scope check via
// steering.Inbox.Enqueue → steering.CheckScope. The IdentityScope.Scope
// claim is trust-based until Phase 61 Protocol auth — exactly the posture
// events.Filter.Admin holds until then.
//
// # Single source for types / methods / errors (CLAUDE.md §8)
//
// Every Protocol message struct is in internal/protocol/types; every
// method name is in internal/protocol/methods; every error code is in
// internal/protocol/errors. This package defines NONE of those — it only
// consumes them. Phase 58 formalises the lint that enforces this; Phase
// 54 lays the foundation correctly so Phase 58 is a no-op formalisation.
//
// # Concurrent reuse (D-025)
//
// ControlSurface is a compiled artifact: every field is set once at
// construction (the TaskRegistry, the steering Registry, the clock — all
// immutable after NewControlSurface returns). There is NO per-call state
// on the struct: Dispatch reads its request-specific data from ctx + the
// request argument, never from the surface. One ControlSurface is safe
// to share across N concurrent Dispatch goroutines; concurrent_test.go
// pins N≥100 under -race.
package protocol

import (
	stderrors "errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// ControlSurface is the transport-agnostic Harbor Protocol task-control
// handler. It is built once per Runtime process and shared across every
// Protocol request; Dispatch is safe for concurrent use by N goroutines
// (D-025).
//
// Construct a ControlSurface via NewControlSurface; do not construct one
// directly.
type ControlSurface struct {
	tasks    tasks.TaskRegistry
	steering *steering.Registry
}

// Option configures a ControlSurface at construction time. Reserved for
// later Protocol-surface phases (a clock override, a metrics hook); the
// Phase 54 surface has no options yet, but the variadic seam means a
// later phase adds one without a signature break.
type Option func(*ControlSurface)

// NewControlSurface builds the Protocol task-control surface. Both
// dependencies are mandatory:
//
//   - taskRegistry — the Phase 20 task registry the `start` method maps
//     onto (tasks.TaskRegistry.Spawn).
//   - steeringRegistry — the Phase 52/53 process-wide steering inbox
//     registry the nine control methods map onto (a control event is
//     enqueued on the run's steering.Inbox).
//
// A nil either fails loud with a wrapped ErrMisconfigured — there is no
// silent-degradation path (CLAUDE.md §5).
//
// The returned ControlSurface is immutable after construction (D-025)
// and safe for concurrent use by N goroutines.
func NewControlSurface(taskRegistry tasks.TaskRegistry, steeringRegistry *steering.Registry, opts ...Option) (*ControlSurface, error) {
	if taskRegistry == nil {
		return nil, fmt.Errorf("%w: tasks.TaskRegistry is nil", ErrMisconfigured)
	}
	if steeringRegistry == nil {
		return nil, fmt.Errorf("%w: steering.Registry is nil", ErrMisconfigured)
	}
	s := &ControlSurface{
		tasks:    taskRegistry,
		steering: steeringRegistry,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// ErrMisconfigured — NewControlSurface was called with a nil dependency.
// Both the TaskRegistry and the steering Registry are mandatory: the
// former is where `start` lands, the latter is where the nine control
// methods land. Fails closed (CLAUDE.md §5) rather than building a
// surface that would nil-panic on the first Dispatch.
var ErrMisconfigured = stderrors.New("protocol: ControlSurface missing a mandatory dependency")
