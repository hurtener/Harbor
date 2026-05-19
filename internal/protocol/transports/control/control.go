// Package control is the Harbor Protocol REST/JSON control transport —
// the client→server half of the wire binding RFC §5.4 resolves to (SSE
// for events + REST/JSON for control). It is a thin adapter over the
// transport-agnostic protocol.ControlSurface that Phase 54 shipped: a
// Handler decodes an HTTP request body into the Protocol wire request
// type the method expects, calls ControlSurface.Dispatch, and encodes
// the wire response — or maps a *protocol/errors.Error onto an HTTP
// status (status.go) and a JSON error body.
//
// # The route shape
//
// One route serves every task-control method:
//
//	POST /v1/control/{method}
//
// where {method} is one of the ten canonical method names
// (internal/protocol/methods). `start` carries a types.StartRequest;
// the nine steering controls carry a types.ControlRequest. The method
// name is read from the path, never hardcoded — the handler validates
// it against methods.IsValidMethod and the Phase 58 single-source lint
// forbids a method string literal anywhere under internal/protocol/
// outside the methods package.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// The identity triple (+ run for a control) lives in the request body's
// `identity` object — the flat types.IdentityScope. The handler does NOT
// re-validate it: ControlSurface.Dispatch already fails closed on an
// incomplete triple with CodeIdentityRequired, which status.go maps to
// 401. The edge structure — decode, hand the whole request to Dispatch,
// map the error — is the single choke point Phase 61 slots JWT
// validation into without reshaping the handler.
//
// # Concurrent reuse (D-025)
//
// Handler is a compiled artifact: the ControlSurface and the logger are
// set once at construction and never mutated. ServeHTTP holds no
// per-request state on the Handler — every request's data lives on the
// *http.Request and the response is written to its own
// http.ResponseWriter. One Handler serves N concurrent requests safely;
// internal/protocol/transports/concurrent_test.go pins it under -race.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// RoutePattern is the http.ServeMux pattern the control transport
// registers under. The {method} wildcard is read via r.PathValue.
// Exported so internal/protocol/transports can mount the handler under
// the same pattern it documents.
const RoutePattern = "POST /v1/control/{method}"

// maxBodyBytes bounds a control request body. The Protocol control
// payload's own RFC §6.3 bound is 16 KiB; 64 KiB leaves generous room
// for the surrounding JSON envelope while still failing closed on a
// client that streams an unbounded body at the edge.
const maxBodyBytes = 64 << 10

// Handler is the Protocol REST/JSON control transport. It is built once
// per Runtime process via NewHandler and shared across every control
// request; ServeHTTP is safe for concurrent use by N goroutines (D-025).
//
// Phase 72b extension: when `bus` AND `redactor` are wired via
// WithEventBus / WithRedactor, the handler accepts admin-impersonation
// requests (IdentityScope.Impersonating set) and emits a redacted
// `audit.admin_scope_used` event on the bus on every accepted
// impersonation. When either dependency is missing, impersonation
// requests are rejected loudly with `CodeRuntimeError` — never silently
// accepted without the audit emit (CLAUDE.md §5, §7 rule 6, §13 "Silent
// degradation").
type Handler struct {
	surface  *protocol.ControlSurface
	logger   *slog.Logger
	bus      events.EventBus // nil ⇒ impersonation accepted-path refused
	redactor audit.Redactor  // nil ⇒ impersonation accepted-path refused
	now      func() time.Time
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithLogger sets the slog.Logger the handler logs decode / dispatch
// failures to. A nil logger (the default) routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithEventBus wires the canonical events.EventBus into the handler so
// the Phase 72b admin-impersonation gate can publish a typed
// `audit.admin_scope_used` event onto the bus when an impersonation
// request is accepted. The bus is OPTIONAL — when not supplied, the
// handler refuses impersonation requests with CodeRuntimeError (the
// audit emit is the load-bearing accountability surface for
// impersonation; rejecting fails-closed rather than silently degrading).
// A nil bus is treated as "WithEventBus not supplied".
func WithEventBus(b events.EventBus) Option {
	return func(h *Handler) {
		if b != nil {
			h.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the handler runs the
// impersonation audit payload through before publishing onto the
// event bus (CLAUDE.md §7 rule 6: "every payload goes through
// audit.Redactor"). The redactor is OPTIONAL at the type level but
// MANDATORY in practice for impersonation: a handler without a
// redactor refuses impersonation requests with CodeRuntimeError,
// same as the missing-bus case. A nil redactor is treated as
// "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(h *Handler) {
		if r != nil {
			h.redactor = r
		}
	}
}

// WithClock overrides the handler's wall clock — used by tests so the
// `OccurredAt` field on the published audit event is deterministic. A
// nil clock keeps the default (time.Now).
func WithClock(now func() time.Time) Option {
	return func(h *Handler) {
		if now != nil {
			h.now = now
		}
	}
}

// NewHandler builds the Protocol REST/JSON control transport over the
// transport-agnostic ControlSurface. The surface is mandatory — a nil
// fails loud with ErrMisconfigured rather than building a handler that
// would nil-panic on the first request (CLAUDE.md §5).
//
// The bus + redactor are OPTIONAL at construction so existing tests
// that don't exercise the Phase 72b impersonation path can call
// `NewHandler(surface)` unchanged. Production callers (transports.NewMux)
// MUST wire both via WithEventBus + WithRedactor; an impersonation
// request without them is rejected with CodeRuntimeError (CLAUDE.md §13
// "Silent degradation" — no quiet accept).
//
// The returned *Handler is immutable after construction (D-025) and safe
// for concurrent use by N goroutines.
func NewHandler(surface *protocol.ControlSurface, opts ...Option) (*Handler, error) {
	if surface == nil {
		return nil, fmt.Errorf("%w: protocol.ControlSurface is nil", ErrMisconfigured)
	}
	h := &Handler{
		surface: surface,
		logger:  slog.Default(),
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ErrMisconfigured — NewHandler was called with a nil ControlSurface.
var ErrMisconfigured = errors.New("control: REST transport missing a mandatory dependency")

// ServeHTTP implements http.Handler. It decodes the request into the
// wire type the path's method expects, dispatches it through the
// ControlSurface, and writes the JSON wire response — or a JSON error
// body with the mapped HTTP status.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The ServeMux pattern already pins POST; defend anyway so a Handler
	// mounted bare (not via NewMux) still rejects a non-POST closed.
	if r.Method != http.MethodPost {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"control transport accepts POST only, got %s", r.Method))
		return
	}

	method := methods.Method(r.PathValue("method"))
	if !methods.IsValidMethod(method) {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical task-control method", string(method)))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		// A MaxBytesReader overflow or a transport read error — the
		// request body could not be read. Fail closed with
		// invalid_request rather than guessing at a partial body.
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, perr := decodeRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// Phase 61: when auth.Middleware ran before us, r.Context() carries
	// the verified identity (via identity.With). The body's
	// IdentityScope MUST match the verified one — defence in depth so a
	// caller cannot present a valid JWT for tenant T1 while submitting
	// a control body claiming tenant T2. Mismatch fails closed (401)
	// with CodeIdentityRequired before Dispatch is called.
	//
	// When NO middleware ran (Phase 60 trust-based posture), there is
	// no ctx-identity and the check is a no-op — Dispatch's existing
	// identity-from-body gate covers it.
	if perr := assertBodyMatchesAuthedIdentity(r, req); perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// Phase 72b: admin-impersonation gate. When the body's
	// IdentityScope carries `Impersonating`, the handler validates the
	// full triplet (Actor / Requester / Impersonating) and gates on
	// auth.ScopeAdmin on the verified JWT before allowing Dispatch.
	// The accepted-path emit (audit.admin_scope_used with a typed
	// AdminScopeUsedPayload) happens only AFTER Dispatch succeeds, so
	// a failed Dispatch never reaches the bus.
	impersonating, perr := h.assertImpersonationShape(r, method, req)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// Dispatch is the transport-agnostic surface; the wire transport is
	// a thin adapter over it. Identity-scope enforcement, scope checks,
	// payload validation all live inside Dispatch — the handler does not
	// re-implement any of them (CLAUDE.md §13 forbids a second
	// validator).
	resp, derr := h.surface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeDispatchError(w, r, method, derr)
		return
	}

	// Phase 72b: emit the audit.admin_scope_used event AFTER a
	// successful Dispatch. Identity is mandatory, capability (admin
	// scope) is mandatory, and the audit emit is mandatory whenever
	// impersonation was accepted (CLAUDE.md §5 + §7 rule 6). The
	// handler refuses impersonation paths up-front when bus/redactor
	// are not wired (assertImpersonationShape), so by the time we get
	// here the dependencies are present.
	if impersonating {
		if eerr := h.emitAdminScopeUsed(r.Context(), method, req); eerr != nil {
			// The emit failed *after* Dispatch succeeded — the run is
			// already in flight. We log loudly, but the response stays
			// 200 because the caller-visible action did succeed. The
			// operator MUST see the bus-side audit drift through the
			// log channel. CLAUDE.md §5 "fail loudly" — never silent.
			h.logger.ErrorContext(r.Context(), "control: impersonation accepted but audit emit failed",
				slog.String("method", string(method)),
				slog.String("error", eerr.Error()))
		}
	}

	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeRequest decodes a request body into the wire request type the
// method expects: types.StartRequest for `start`, types.ControlRequest
// for the nine steering controls. A decode failure surfaces as
// CodeInvalidRequest — never a silent zero-value request.
func decodeRequest(method methods.Method, body []byte) (any, *protoerrors.Error) {
	if method == methods.MethodStart {
		var sr types.StartRequest
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid StartRequest", string(method))
		}
		return &sr, nil
	}
	var cr types.ControlRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body is not a valid ControlRequest", string(method))
	}
	return &cr, nil
}

// writeDispatchError maps a Dispatch error onto the wire. Dispatch's
// contract is that every error case is a *protoerrors.Error; if a
// non-Protocol error ever surfaces, it is wrapped as CodeRuntimeError
// rather than leaked verbatim (CLAUDE.md §5 + §7 — no raw runtime detail
// on the wire).
func (h *Handler) writeDispatchError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: dispatch failed", string(method)))
}

// writeError encodes a *protoerrors.Error as a JSON body with the
// mapped HTTP status.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, perr *protoerrors.Error) {
	h.writeJSON(w, r, httpStatus(perr.Code), perr)
}

// assertBodyMatchesAuthedIdentity is the Phase 61 defence-in-depth
// check: when auth.Middleware ran before this handler, r.Context()
// carries the verified identity, and the request body's IdentityScope
// MUST match it. A mismatch is a malicious / buggy client trying to
// borrow another tenant's identity using a valid token — fail closed.
//
// When no middleware ran (Phase 60 trust-based posture), ctx carries
// no identity and the check returns nil — Dispatch's existing
// identity-from-body gate is authoritative.
//
// Phase 72b: when the body carries `Impersonating`, the top-level
// Tenant/User/Session components are *deliberately* the impersonated
// identity, not the JWT identity. The verified JWT identity is carried
// in `Actor` and checked separately by `assertImpersonationShape`. This
// helper therefore returns nil for impersonation-shaped bodies — the
// impersonation gate is the authoritative check for that shape.
//
// req is the decoded *types.StartRequest or *types.ControlRequest.
func assertBodyMatchesAuthedIdentity(r *http.Request, req any) *protoerrors.Error {
	authed, ok := identity.From(r.Context())
	if !ok {
		return nil
	}
	var bodyScope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		bodyScope = v.Identity
	case *types.ControlRequest:
		bodyScope = v.Identity
	default:
		return nil
	}
	// Phase 72b: impersonation-shaped bodies bypass this check; the
	// dedicated impersonation gate (assertImpersonationShape) owns the
	// verification of the impersonation triplet against the JWT.
	if bodyScope.IsImpersonating() {
		return nil
	}
	// An empty body identity is permitted when ctx carries one — the
	// body-side gate in Dispatch will see the JWT-derived identity via
	// the request body once we backfill it. To keep Phase 60's flat
	// IdentityScope-on-the-body contract, callers SHOULD echo the
	// JWT identity in the body; we backfill the body when the body's
	// IdentityScope is empty so the existing Dispatch gate sees a
	// matching identity. When the body DOES carry an identity, every
	// non-empty component MUST match the JWT-verified one.
	if bodyScope.Tenant == "" && bodyScope.User == "" && bodyScope.Session == "" {
		// Backfill — the JWT is the source of truth. The body's
		// `Run` and `Scope` fields are independent and stay as-is.
		bodyScope.Tenant = authed.TenantID
		bodyScope.User = authed.UserID
		bodyScope.Session = authed.SessionID
		switch v := req.(type) {
		case *types.StartRequest:
			v.Identity = bodyScope
		case *types.ControlRequest:
			v.Identity = bodyScope
		}
		return nil
	}
	if bodyScope.Tenant != authed.TenantID || bodyScope.User != authed.UserID || bodyScope.Session != authed.SessionID {
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"body identity scope does not match the verified JWT identity")
	}
	return nil
}

// assertImpersonationShape is the Phase 72b admin-impersonation gate.
// It returns (impersonating, error):
//
//   - impersonating == false, err == nil → no impersonation on the
//     body; existing behaviour is preserved.
//   - impersonating == true,  err == nil → the body's impersonation
//     triplet (Actor / Requester / Impersonating) passed every
//     structural + scope check; Dispatch is safe to call.
//   - impersonating == ?,     err != nil → the body is malformed
//     (incomplete triple, mismatched Actor, missing scope claim,
//     etc.) — the handler writes the protoerror and never calls
//     Dispatch.
//
// The checks are layered in fail-loud order:
//
//  1. Body carries Impersonating? → if not, return (false, nil).
//  2. Bus + Redactor wired? → if not, refuse with CodeRuntimeError
//     (no silent accept without the audit emit — CLAUDE.md §13).
//  3. JWT carries auth.ScopeAdmin? → if not, refuse with
//     CodeScopeMismatch (impersonation is an admin-only feature per
//     Brief 11 §CC-2).
//  4. Impersonating triple complete? → if not, refuse with
//     CodeIdentityRequired (identity is mandatory; the impersonated
//     triple is identity too — CLAUDE.md §6 rule 9).
//  5. Actor + Requester present + complete? → if not, refuse with
//     CodeIdentityRequired.
//  6. Actor matches the verified JWT triple? → if not, refuse with
//     CodeScopeMismatch (the actor is the audit anchor; faking it is
//     a privilege-escalation attempt).
//  7. Requester == Actor (V1 invariant)? → if not, refuse with
//     CodeScopeMismatch (delegated impersonation is post-V1).
//  8. Top-level Tenant/User/Session == Impersonating triple? → if
//     not, refuse with CodeIdentityRequired (the run must execute
//     under the impersonated identity).
func (h *Handler) assertImpersonationShape(r *http.Request, method methods.Method, req any) (bool, *protoerrors.Error) {
	var bodyScope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		bodyScope = v.Identity
	case *types.ControlRequest:
		bodyScope = v.Identity
	default:
		return false, nil
	}
	if !bodyScope.IsImpersonating() {
		return false, nil
	}

	// (2) Audit emit is a hard precondition for accepting an
	// impersonation request — otherwise we'd silently accept the
	// admin-on-behalf-of action without bus-visible accountability.
	// CLAUDE.md §13 forbids silent degradation.
	if h.bus == nil || h.redactor == nil {
		h.logger.ErrorContext(r.Context(), "control: impersonation request received but transport not wired for audit emit",
			slog.String("method", string(method)))
		return false, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: impersonation accepted-path requires audit emit (bus + redactor) on the transport; refusing fail-closed", string(method))
	}

	// (3) admin scope is mandatory — read from the verified scope set
	// on ctx (auth.Middleware injected it). HasScope returns false
	// when no scopes are attached (Phase 60 trust-based posture or
	// non-admin token), which is the safe default for a privilege
	// check.
	if !auth.HasScope(r.Context(), auth.ScopeAdmin) {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation requires the admin scope claim", string(method))
	}

	// (4) The impersonated triple must be complete — identity is
	// mandatory; the impersonated triple IS identity (CLAUDE.md §6
	// rule 9). No "anonymous impersonation" mode.
	imp := bodyScope.Impersonating
	if err := identity.Validate(identity.Identity{
		TenantID:  imp.Tenant,
		UserID:    imp.User,
		SessionID: imp.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation target identity incomplete: %v", string(method), err)
	}

	// (5) Actor + Requester must both be present and complete. They
	// are the audit anchor — a request without them cannot be
	// audited, so refuse.
	if bodyScope.Actor == nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the actor field", string(method))
	}
	if bodyScope.Requester == nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the requester field", string(method))
	}
	actor := bodyScope.Actor
	requester := bodyScope.Requester
	if err := identity.Validate(identity.Identity{
		TenantID:  actor.Tenant,
		UserID:    actor.User,
		SessionID: actor.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation actor identity incomplete: %v", string(method), err)
	}
	if err := identity.Validate(identity.Identity{
		TenantID:  requester.Tenant,
		UserID:    requester.User,
		SessionID: requester.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requester identity incomplete: %v", string(method), err)
	}

	// (6) Actor MUST equal the verified JWT identity — the audit
	// trail's accountability stands or falls here. When the middleware
	// has run (production posture), we have the verified identity on
	// ctx and assert against it. When no middleware ran (test-only
	// posture WITHOUT WithoutValidator's escape hatch wired through),
	// we cannot verify the actor, so the impersonation gate refuses
	// fail-closed.
	authed, ok := identity.From(r.Context())
	if !ok {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation requires a verified identity in context (auth middleware must run)", string(method))
	}
	if actor.Tenant != authed.TenantID || actor.User != authed.UserID || actor.Session != authed.SessionID {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation actor does not match the verified JWT identity", string(method))
	}

	// (7) Requester == Actor at V1 (delegated impersonation is
	// post-V1 — the field is reserved for future use, not for V1
	// divergence).
	if requester.Tenant != actor.Tenant || requester.User != actor.User || requester.Session != actor.Session {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: V1 invariant requires impersonation requester == actor (delegated impersonation is post-V1)", string(method))
	}

	// (8) The top-level Tenant/User/Session MUST equal the
	// Impersonating triple — the run executes as the impersonated
	// identity. A mismatch is a malformed shape (the caller asked to
	// impersonate user A but the body wants the run to execute as
	// user B); refuse closed.
	if bodyScope.Tenant != imp.Tenant || bodyScope.User != imp.User || bodyScope.Session != imp.Session {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the top-level identity to equal the impersonated triple", string(method))
	}

	return true, nil
}

// emitAdminScopeUsed publishes the typed
// `audit.admin_scope_used` event with an AdminScopeUsedPayload (Phase
// 72b) onto the wired event bus. The payload runs through the wired
// audit.Redactor BEFORE the publish so any redaction rule that would
// rewrite a string field (e.g. a configurable PII rule) takes effect at
// the audit boundary, per CLAUDE.md §7 rule 6 ("every payload goes
// through audit.Redactor") + D-020 (Audit owns redaction).
//
// The event's Identity is the IMPERSONATED triple so a Console
// subscribing to events for the impersonated session sees the audit
// emit alongside the run's own events. The Actor / Requester /
// Impersonating fields on the payload carry the full triplet for
// audit-side correlation. This shape is by-design redundant with the
// Identity quadruple — the payload is the source of truth for
// "who impersonated whom"; the event Identity is the bus-side scope.
//
// A publish failure is returned to the caller, which logs loudly but
// returns 200 to the client (the user-visible action already succeeded
// by the time emit runs). CLAUDE.md §5 "fail loudly" — never silent.
func (h *Handler) emitAdminScopeUsed(ctx context.Context, method methods.Method, req any) error {
	var scope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		scope = v.Identity
	case *types.ControlRequest:
		scope = v.Identity
	default:
		return fmt.Errorf("control: emitAdminScopeUsed called with unsupported request type %T", req)
	}
	if !scope.IsImpersonating() || scope.Actor == nil || scope.Requester == nil {
		return fmt.Errorf("control: emitAdminScopeUsed called with non-impersonation scope (gate ordering bug)")
	}

	// CLAUDE.md §7 rule 6 + D-020 — run the impersonation fields
	// through the audit redactor BEFORE building the typed payload.
	// The redactor walks a flat `map[string]any` of the fields the
	// payload carries; we extract redacted strings back (with the
	// pre-redaction fallback) and assemble the typed
	// `AdminScopeUsedPayload`. This mirrors the Phase 61 audit
	// pattern in `internal/protocol/auth/auth.go::audit` (D-082): the
	// redactor IS run, but the published payload is the typed
	// `SafePayload` so subscribers see the canonical shape (no
	// `RedactedMap` ambiguity for an audit type the operator's
	// payload-shape contract explicitly carved out as flat).
	auditView := map[string]any{
		"actor_tenant":         scope.Actor.Tenant,
		"actor_user":           scope.Actor.User,
		"actor_session":        scope.Actor.Session,
		"requester_tenant":     scope.Requester.Tenant,
		"requester_user":       scope.Requester.User,
		"requester_session":    scope.Requester.Session,
		"impersonating_tenant": scope.Impersonating.Tenant,
		"impersonating_user":   scope.Impersonating.User,
		"impersonating_session": scope.Impersonating.Session,
		"reason":               auth.AdminImpersonationReason,
		"method":               string(method),
	}
	redacted, err := h.redactor.Redact(ctx, auditView)
	if err != nil {
		// Fail loud — never emit unredacted. CLAUDE.md §13 forbids
		// silent fall-through to the unredacted payload.
		return fmt.Errorf("control: redactor refused admin_scope_used payload: %w", err)
	}
	redactedMap, ok := redacted.(map[string]any)
	if !ok {
		// Defensive: production redactors return a map (the
		// patterns driver's reflective walk shape). A redactor
		// returning a non-map shape on a map input is a contract
		// violation; refuse rather than emit a half-redacted view.
		return fmt.Errorf("control: redactor returned non-map shape %T on map input (cannot extract redacted fields)", redacted)
	}

	emitPayload := auth.AdminScopeUsedPayload{
		Actor: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "actor_tenant", scope.Actor.Tenant),
			User:    redactedString(redactedMap, "actor_user", scope.Actor.User),
			Session: redactedString(redactedMap, "actor_session", scope.Actor.Session),
		},
		Requester: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "requester_tenant", scope.Requester.Tenant),
			User:    redactedString(redactedMap, "requester_user", scope.Requester.User),
			Session: redactedString(redactedMap, "requester_session", scope.Requester.Session),
		},
		Impersonating: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "impersonating_tenant", scope.Impersonating.Tenant),
			User:    redactedString(redactedMap, "impersonating_user", scope.Impersonating.User),
			Session: redactedString(redactedMap, "impersonating_session", scope.Impersonating.Session),
		},
		Reason: redactedString(redactedMap, "reason", auth.AdminImpersonationReason),
		Method: redactedString(redactedMap, "method", string(method)),
	}

	// Identity for the event: use the IMPERSONATED triple so a
	// Console subscribing to events for the impersonated session
	// sees the audit emit alongside the run's own events. The Actor
	// is on the payload for audit correlation.
	ev := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  scope.Impersonating.Tenant,
				UserID:    scope.Impersonating.User,
				SessionID: scope.Impersonating.Session,
			},
		},
		OccurredAt: h.now(),
		Payload:    emitPayload,
	}
	if err := h.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("control: publish admin_scope_used: %w", err)
	}
	return nil
}

// redactedString returns key's string value from the redactor's
// output when present (and a string); falls back to the original
// fallback value otherwise. Mirrors the pattern in
// `internal/protocol/auth/auth.go::redactedString`: production
// redactors usually pass through string values unchanged, but a
// custom redactor that rewrites a field returns the rewritten value
// here; an unexpected shape (non-string at the key) is treated as a
// pass-through to the pre-redaction value rather than crashing the
// emit.
func redactedString(red map[string]any, key, fallback string) string {
	if v, ok := red[key].(string); ok {
		return v
	}
	return fallback
}

// writeJSON encodes v as a JSON body with the given status. A marshal
// failure is logged and degraded to a bare 500 — never a partial body.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "control transport: response marshal failed",
			slog.Int("status", status))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// The client hung up mid-write — nothing recoverable, but log it
		// rather than swallowing silently.
		h.logger.DebugContext(r.Context(), "control transport: response write failed",
			slog.Int("status", status))
	}
}
