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
	"context"
	stderrors "errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TopologyAccessor is the narrow read-only contract the ControlSurface
// calls into for the Phase 74 `topology.snapshot` method. The Runtime
// engine satisfies it structurally — the engine package never imports
// the Protocol package; the wiring at cmd/harbor injects the engine as
// a TopologyAccessor. Keeping the interface here (not in the engine
// package) is what keeps the engine Protocol-free (no import cycle).
//
// A nil TopologyAccessor is permitted at construction (a Runtime that
// hosts no engine — e.g. validate-only mode): a `topology.snapshot`
// call against a nil-accessor surface fails closed with
// CodeUnknownMethod (the route effectively does not exist on that
// Runtime), which the smoke script's 404 → SKIP convention picks up.
type TopologyAccessor interface {
	// Topology builds the engine's canonical TopologyProjection.
	// Identity-mandatory; pure read.
	Topology(ctx context.Context) (types.TopologyProjection, error)
	// TenantID is the tenant the engine runs under. The
	// admin-cross-tenant gate compares it against the caller's tenant:
	// a caller whose tenant differs needs the verified auth.ScopeAdmin
	// claim (D-079).
	TenantID() string
}

// ScopeChecker reports whether ctx carries a given auth scope. It is
// the seam the admin-cross-tenant gate consults; the production
// implementation is auth.HasScope. Injecting it (rather than calling
// auth.HasScope directly) keeps the gate unit-testable without an
// auth.Middleware in front.
type ScopeChecker func(ctx context.Context, s auth.Scope) bool

// ControlSurface is the transport-agnostic Harbor Protocol task-control
// handler. It is built once per Runtime process and shared across every
// Protocol request; Dispatch is safe for concurrent use by N goroutines
// (D-025).
//
// Construct a ControlSurface via NewControlSurface; do not construct one
// directly.
type ControlSurface struct {
	tasks      tasks.TaskRegistry
	steering   *steering.Registry
	topology   TopologyAccessor // Phase 74 — may be nil (Runtime hosts no engine)
	adminScope ScopeChecker     // Phase 74 — the admin-cross-tenant gate; defaults to auth.HasScope
	bus        events.EventBus  // Phase 74 — optional; the audit.admin_scope_used emit on a cross-tenant topology read
	sessions   SessionEnsurer   // D-171 — optional; create-on-first-use on `start`
}

// SessionEnsurer is the create-on-first-use seam the `start` method
// calls (D-171). The session id is the per-request session the client
// chose (carried in the request's IdentityScope, sourced from the
// X-Harbor-Session header by auth.Middleware). When a `start` names a
// session id that has no registry row yet, EnsureSession materialises it
// under the verified (tenant, user); a later turn in the same session
// is a no-op. A closed session id fails loud (the runtime never
// silently revives a GC-reaped conversation).
//
// Defined here (consumer side, error-only) so the protocol package does
// not import the sessions package; the concrete *sessions.Registry is
// adapted to this interface in cmd/harbor / harbortest.
type SessionEnsurer interface {
	EnsureSession(ctx context.Context, ident identity.Identity) error
}

// Option configures a ControlSurface at construction time. Reserved for
// later Protocol-surface phases (a clock override, a metrics hook); the
// Phase 54 surface has no options yet, but the variadic seam means a
// later phase adds one without a signature break.
type Option func(*ControlSurface)

// NewControlSurface builds the Protocol task-control surface. Two
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
// The Phase 74 `topology` accessor is OPTIONAL — a nil topology builds
// a surface that rejects `topology.snapshot` with CodeUnknownMethod (a
// Runtime hosting no engine, e.g. validate-only mode). It is wired via
// the WithTopologyAccessor option so existing two-arg callers compile
// unchanged.
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
		tasks:      taskRegistry,
		steering:   steeringRegistry,
		adminScope: auth.HasScope,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// WithTopologyAccessor wires the Phase 74 engine topology accessor into
// the ControlSurface so `topology.snapshot` returns a real projection.
// A surface built WITHOUT it rejects `topology.snapshot` with
// CodeUnknownMethod — the explicit "this Runtime hosts no engine"
// posture (CLAUDE.md §13 — no silent degradation; the route simply
// does not exist on an engine-less Runtime). A nil accessor passed
// here is treated as "not supplied".
func WithTopologyAccessor(t TopologyAccessor) Option {
	return func(s *ControlSurface) {
		// A typed-nil interface value (a nil *engine boxed into a
		// non-nil interface) is still a nil accessor for our purposes;
		// callers pass a real accessor or omit the option.
		if t != nil {
			s.topology = t
		}
	}
}

// WithScopeChecker overrides the admin-cross-tenant scope predicate.
// Production leaves it at the default (auth.HasScope); tests inject a
// deterministic checker to exercise the admin path without standing up
// an auth.Middleware.
func WithScopeChecker(c ScopeChecker) Option {
	return func(s *ControlSurface) {
		if c != nil {
			s.adminScope = c
		}
	}
}

// WithEventBus wires the canonical events.EventBus the ControlSurface
// publishes an `audit.admin_scope_used` event onto when a cross-tenant
// `topology.snapshot` read is granted under the admin scope (RFC §6.13
// — admin-scope use is retroactively auditable). The bus is OPTIONAL:
// a surface built without it still gates the cross-tenant read on the
// admin scope, but a successful admin read emits no audit event — the
// production wiring (cmd/harbor / harbortest devstack) wires the bus so
// the audit trail is complete. A nil bus is treated as "not supplied".
func WithEventBus(b events.EventBus) Option {
	return func(s *ControlSurface) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithSessionEnsurer wires the create-on-first-use seam (D-171) the
// `start` method calls so a brand-new conversation's session row
// materialises in the SessionRegistry on the first turn. A surface
// built WITHOUT it does NOT create sessions on start — the explicit
// "this Runtime has no session registry" posture (e.g. a control-only
// surface). A nil ensurer passed here is treated as "not supplied"
// (no silent panic). The production wiring (cmd/harbor, harbortest)
// always supplies it so `harbor dev` gets create-on-first-use sessions
// out of the box.
func WithSessionEnsurer(e SessionEnsurer) Option {
	return func(s *ControlSurface) {
		if e != nil {
			s.sessions = e
		}
	}
}

// ErrMisconfigured — NewControlSurface was called with a nil dependency.
// Both the TaskRegistry and the steering Registry are mandatory: the
// former is where `start` lands, the latter is where the nine control
// methods land. Fails closed (CLAUDE.md §5) rather than building a
// surface that would nil-panic on the first Dispatch.
var ErrMisconfigured = stderrors.New("protocol: ControlSurface missing a mandatory dependency")
