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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
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
type Handler struct {
	surface *protocol.ControlSurface
	logger  *slog.Logger
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

// NewHandler builds the Protocol REST/JSON control transport over the
// transport-agnostic ControlSurface. The surface is mandatory — a nil
// fails loud with ErrMisconfigured rather than building a handler that
// would nil-panic on the first request (CLAUDE.md §5).
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
