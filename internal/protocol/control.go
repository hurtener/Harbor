package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// methodToControlType maps each of the nine steering-control Protocol
// method names onto its steering.ControlType. MethodStart is NOT in this
// map — it routes to the task registry, not the steering inbox. The map
// is the single place the Protocol method vocabulary (lowercase
// snake_case, client-facing) is bridged to the steering taxonomy
// (uppercase, runtime-internal). Keeping the two namespaces distinct is
// deliberate (see internal/protocol/methods doc).
var methodToControlType = map[methods.Method]steering.ControlType{
	methods.MethodCancel:        steering.ControlCancel,
	methods.MethodPause:         steering.ControlPause,
	methods.MethodResume:        steering.ControlResume,
	methods.MethodRedirect:      steering.ControlRedirect,
	methods.MethodInjectContext: steering.ControlInjectContext,
	methods.MethodApprove:       steering.ControlApprove,
	methods.MethodReject:        steering.ControlReject,
	methods.MethodPrioritize:    steering.ControlPrioritize,
	methods.MethodUserMessage:   steering.ControlUserMessage,
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// task-control method call. A Phase 60 HTTP/SSE handler decodes a
// request, calls Dispatch, and encodes the response — Dispatch IS the
// surface; the wire transport is a thin adapter over it.
//
// method selects the handler. req MUST be the wire request type the
// method expects:
//
//   - MethodStart        → *types.StartRequest,  returns *types.StartResponse
//   - the nine controls  → *types.ControlRequest, returns *types.ControlResponse
//
// Dispatch fails closed with a *errors.Error in every error case (the
// caller reaches a stable errors.Code via errors.As):
//
//   - CodeUnknownMethod   — method is not one of the ten canonical methods.
//   - CodeInvalidRequest  — req is nil or the wrong wire type for method.
//   - CodeIdentityRequired — the request's identity scope is incomplete
//     (RFC §5.5: the Protocol rejects any request without an identity
//     scope); for a control method, a missing run id also lands here.
//   - CodeScopeMismatch   — the caller's steering scope is below the
//     control method's RFC §6.3 minimum (mapped from steering.CheckScope).
//   - CodePayloadInvalid  — the control payload violated an RFC §6.3
//     bound (mapped from steering.ValidatePayload).
//   - CodeNotFound        — the target run has no live steering inbox.
//   - CodeRuntimeError    — an unclassified runtime-side failure.
//
// Dispatch holds no per-call state on the ControlSurface — it reads
// everything from ctx + req (D-025). One ControlSurface serves N
// concurrent Dispatch goroutines safely.
func (s *ControlSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsValidMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol method", string(method))
	}

	if method == methods.MethodStart {
		return s.dispatchStart(ctx, req)
	}
	// Wave 13 (Phase 72 / 72a — D-105 / D-106): the streaming-events
	// methods are served by their own transports (SSE for subscribe;
	// POST /v1/events/aggregate for aggregate), NOT by the REST control
	// surface. A caller that hits Dispatch with one of them is using
	// the wrong transport for the wrong vocabulary — surface it loud
	// rather than silently routing onto the steering inbox.
	if method == methods.MethodEventsSubscribe {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a streaming-events method; open the SSE transport at GET /v1/events instead", string(method))
	}
	if method == methods.MethodEventsAggregate {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a streaming-events method; POST to /v1/events/aggregate instead", string(method))
	}
	// Phase 72c (D-108): the five `search.*` methods are dispatched by
	// SearchSurface, not ControlSurface — a caller that hits the REST
	// control surface with a search method is using the wrong transport
	// for the wrong vocabulary. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsSearchMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a search method; dispatch through the SearchSurface (POST /v1/search) instead", string(method))
	}
	// Phase 72f (D-111): the five `runtime.*` / `metrics.*` posture
	// methods are dispatched by PostureSurface, not ControlSurface — a
	// caller that hits the task-control Dispatch with a posture method
	// is using the wrong surface. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsPostureMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a posture method; dispatch through the PostureSurface instead", string(method))
	}
	// Phase 72e (D-110): `pause.list` is a read-only snapshot over the
	// pauseresume.Coordinator — it routes through its own HTTP handler
	// (POST /v1/pause/list), not the task-control ControlSurface. A
	// caller that hits the control Dispatch with the pause method is
	// using the wrong surface. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsPauseMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a pause-snapshot method; POST to /v1/pause/list instead", string(method))
	}
	return s.dispatchControl(ctx, method, req)
}

// dispatchStart handles the `start` method: it spawns a foreground task
// via the Phase 20 tasks.TaskRegistry. A `start` request carries the
// identity triple (RunID is ignored — Spawn assigns the TaskID) and no
// steering scope (task creation is not a steering control).
//
// The method name is read from methods.MethodStart, never hardcoded —
// the Phase 58 single-source lint forbids a Protocol method string
// literal anywhere under internal/protocol/ outside the methods package
// (CLAUDE.md §8; D-075).
func (s *ControlSurface) dispatchStart(ctx context.Context, req any) (*types.StartResponse, error) {
	method := methods.MethodStart

	sr, ok := req.(*types.StartRequest)
	if !ok || sr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.StartRequest", string(method))
	}

	// Identity-mandatory at the Protocol edge (RFC §5.5). The triple is
	// required; RunID is not (a `start` mints the run).
	id := identity.Identity{
		TenantID:  sr.Identity.Tenant,
		UserID:    sr.Identity.User,
		SessionID: sr.Identity.Session,
	}
	if err := identity.Validate(id); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}

	handle, err := s.tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity:       identity.Quadruple{Identity: id},
		Kind:           tasks.KindForeground,
		Description:    sr.Description,
		Query:          sr.Query,
		Priority:       sr.Priority,
		IdempotencyKey: sr.IdempotencyKey,
	})
	if err != nil {
		return nil, mapTaskError(string(method), err)
	}

	return &types.StartResponse{
		TaskID:          string(handle.ID),
		Reused:          handle.Reused,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// dispatchControl handles the nine steering-control methods. It builds a
// steering.ControlEvent from the Protocol request and enqueues it on the
// run's steering.Inbox — the Phase 52 Inbox.Enqueue does the validation
// (RFC §6.3 payload bounds), the per-event scope check (steering.
// CheckScope), and the identity gate. Phase 54 does NOT re-implement any
// of that (CLAUDE.md §13 forbids a second validator); it constructs the
// event, hands it to Enqueue, and maps the steering sentinel onto a
// Protocol error code.
func (s *ControlSurface) dispatchControl(ctx context.Context, method methods.Method, req any) (*types.ControlResponse, error) {
	_ = ctx // the steering enqueue path is synchronous; ctx is held for the Phase 60 transport adapter's cancellation seam.

	cr, ok := req.(*types.ControlRequest)
	if !ok || cr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.ControlRequest", string(method))
	}

	ctrlType, ok := methodToControlType[method]
	if !ok {
		// Unreachable: Dispatch already rejected non-canonical methods,
		// and MethodStart is routed away before here. Fail loud rather
		// than silently no-op (CLAUDE.md §5).
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no steering control type mapping (Protocol-surface invariant violated)", string(method))
	}

	// Build the run quadruple. The full quadruple — triple + run — is
	// mandatory for a steering control: it targets a specific run's
	// inbox. An incomplete quadruple fails closed here, before the
	// steering Registry is ever touched.
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  cr.Identity.Tenant,
			UserID:    cr.Identity.User,
			SessionID: cr.Identity.Session,
		},
		RunID: cr.Identity.Run,
	}
	if err := identity.Validate(q.Identity); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	if q.RunID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: run id is required for a steering control", string(method))
	}

	// Resolve the caller's steering scope. An empty / unrecognised scope
	// is rejected by steering.CheckScope inside Enqueue — but resolve it
	// here so an obviously-malformed scope string fails with a clear
	// Protocol error rather than being passed through as the raw string.
	scope := steering.Scope(cr.Identity.Scope)
	if !steering.IsValidScope(scope) {
		return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: caller scope %q is not a canonical steering scope", string(method), cr.Identity.Scope)
	}

	// Look up the run's live inbox. A run with no inbox (never started,
	// or already ended) fails closed with CodeNotFound.
	inbox, err := s.steering.Lookup(q)
	if err != nil {
		return nil, mapSteeringError(string(method), err)
	}

	// Construct the control event and hand it to Enqueue. Enqueue runs
	// the full Phase 52 gauntlet: identity match, canonical-type check,
	// CheckScope (per-event scope + cross-tenant-requires-admin),
	// ValidatePayload (the RFC §6.3 bounds). Any failure surfaces as a
	// steering sentinel, mapped onto a Protocol error code.
	ev := steering.ControlEvent{
		Type:         ctrlType,
		Identity:     q,
		CallerScope:  scope,
		CallerTenant: q.TenantID, // the caller authenticated under the run's tenant; cross-tenant steering arrives with a differing CallerTenant once Phase 61 auth lands. Until then the trust-based scope claim carries the elevation.
		Payload:      cr.Payload,
		EventID:      cr.EventID,
	}
	if err := inbox.Enqueue(ev); err != nil {
		return nil, mapSteeringError(string(method), err)
	}

	return &types.ControlResponse{
		Accepted:        true,
		Method:          string(method),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// compile-time assertion: every steering-control method
// (IsControlMethod=true) has a steering.ControlType mapping. MethodStart,
// the Wave 13 streaming-events methods (MethodEventsSubscribe /
// MethodEventsAggregate), and the Phase 72c `search.*` cluster are NOT
// control methods and route through their own surfaces (the task
// registry for Start; the SSE handler + events-aggregate handler for
// the streaming-events two; the search dispatcher in
// internal/protocol/search.go for the search cluster) — IsControlMethod
// gates the exhaustive check so a new non-control method does NOT need
// a mapping. If a new steering-control method is added to
// internal/protocol/methods without a mapping here, this fails —
// keeping the Protocol method table and the steering bridge in lockstep.
func init() {
	for _, m := range methods.Methods() {
		if !methods.IsControlMethod(m) {
			// MethodStart, the Wave 13 streaming-events methods, the
			// Phase 72c search cluster, and any future non-control
			// method are routed elsewhere — no steering.ControlType is
			// expected for them.
			continue
		}
		if _, ok := methodToControlType[m]; !ok {
			panic(fmt.Sprintf("protocol: steering-control method %q has no steering.ControlType mapping — methodToControlType is out of sync with internal/protocol/methods", m))
		}
	}
}
