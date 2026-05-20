// Package stream — Wave 13 additions (Phase 73c / D-122): the Console
// Sessions-page HTTP handler. Like `events.aggregate`, `pause.list`,
// and the Flows-page handler, these are one-shot request/response
// endpoints — POST JSON in, JSON out. They live in the stream package
// because their identity + cross-tenant scope-claim gating is the same
// `resolveIdentity` / `auth.HasScope` machinery the subscription
// surface uses.
//
// Route shapes (both POST):
//
//	POST /v1/sessions/list     — paginated, filtered session catalog
//	POST /v1/sessions/inspect  — a single session's full snapshot
//
// Both routes are read-only and identity-mandatory. A cross-tenant
// `sessions.list` filter (a `tenant_ids` entry outside the caller's
// verified tenant) is gated on the verified `auth.ScopeAdmin` claim
// (D-079 closed two-scope set — NO new scope is minted); the
// sessions/protocol.Service emits an `audit.admin_scope_used` event on
// every successful admin-scope query.
//
// SessionsHandler is a D-025-safe compiled artifact — service / logger
// are set once at construction; ServeHTTP holds no per-request state.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// SessionsRoutePattern is the http.ServeMux prefix pattern the
// Sessions-page handler registers under. The handler internally
// branches on the trailing path segment to dispatch the two methods.
const SessionsRoutePattern = "POST /v1/sessions/"

// maxSessionsBodyBytes bounds a Sessions-page request body. The wire
// payloads are small (an identity scope + a filter + pagination); 64
// KiB is comfortably over the realistic ceiling and fails closed on a
// client that streams an unbounded body at the edge.
const maxSessionsBodyBytes = 64 << 10

// ErrSessionsMisconfigured — NewSessionsHandler was called with a nil
// sessions/protocol.Service.
var ErrSessionsMisconfigured = errors.New("stream: sessions handler missing a mandatory dependency")

// SessionsHandler serves the two `POST /v1/sessions/*` routes. It is
// the wire adapter over a *sessionsprotocol.Service: resolve identity,
// branch on the trailing path segment, decode the request, dispatch,
// encode.
type SessionsHandler struct {
	service *sessionsprotocol.Service
	logger  *slog.Logger
}

// SessionsOption configures NewSessionsHandler at construction.
type SessionsOption func(*SessionsHandler)

// WithSessionsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithSessionsLogger(l *slog.Logger) SessionsOption {
	return func(h *SessionsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewSessionsHandler builds the Sessions-page handler over a
// *sessionsprotocol.Service. service is mandatory — a nil fails loud
// with ErrSessionsMisconfigured rather than building a handler that
// would nil-panic on the first request (CLAUDE.md §5).
//
// The returned *SessionsHandler is immutable after construction
// (D-025) and safe for concurrent use by N goroutines.
func NewSessionsHandler(service *sessionsprotocol.Service, opts ...SessionsOption) (*SessionsHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: sessions/protocol.Service is nil", ErrSessionsMisconfigured)
	}
	h := &SessionsHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, branches on
// the trailing path segment, decodes the body, dispatches to the
// service, and encodes the response.
func (h *SessionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"sessions endpoints accept POST only")
		return
	}
	id, err := resolveIdentity(r)
	if err != nil {
		writeSessionsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionsBodyBytes))
	if err != nil {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	// adminScoped is the verified-JWT scope decision. The Service
	// enforces the D-079 cross-tenant gate — a false value on a
	// cross-tenant filter fails closed with CodeScopeMismatch (403).
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin) ||
		auth.HasScope(r.Context(), auth.ScopeConsoleFleet)
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	switch strings.TrimPrefix(r.URL.Path, "/v1/sessions/") {
	case "list":
		h.serveList(w, r, body, wireID, adminScoped)
	case "inspect":
		h.serveInspect(w, r, body, wireID, adminScoped)
	default:
		writeSessionsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown sessions method route")
	}
}

func (h *SessionsHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.SessionsListRequest
	if err := decodeSessionsBody(body, &req); err != nil {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode sessions.list request: "+err.Error())
		return
	}
	if perr := assertSessionsIdentity(req.Identity, wireID); perr != "" {
		writeSessionsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	resp, err := h.service.List(r.Context(), req, adminScoped)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionsList, err)
		return
	}
	writeSessionsJSON(w, r, resp, h.logger)
}

func (h *SessionsHandler) serveInspect(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.SessionsInspectRequest
	if err := decodeSessionsBody(body, &req); err != nil {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode sessions.inspect request: "+err.Error())
		return
	}
	if perr := assertSessionsIdentity(req.Identity, wireID); perr != "" {
		writeSessionsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	resp, err := h.service.Inspect(r.Context(), req, adminScoped)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionsInspect, err)
		return
	}
	writeSessionsJSON(w, r, resp, h.logger)
}

// decodeSessionsBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value (the identity scope
// is then overlaid from the verified identity by each handler).
func decodeSessionsBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

// assertSessionsIdentity is the defence-in-depth check: when the
// request body carries an identity, every non-empty component MUST
// match the verified identity — a caller cannot present a valid JWT
// for tenant T1 while submitting a body claiming tenant T2. Returns
// "" when the body identity is consistent (or empty).
func assertSessionsIdentity(body, verified prototypes.IdentityScope) string {
	if (body.Tenant != "" && body.Tenant != verified.Tenant) ||
		(body.User != "" && body.User != verified.User) ||
		(body.Session != "" && body.Session != verified.Session) {
		return "body identity scope does not match the verified identity"
	}
	return ""
}

// writeServiceError maps a sessions/protocol.Service error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
// method is the canonical methods.Method constant (single-sourced in
// internal/protocol/methods — never a hardcoded string).
func (h *SessionsHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifySessionsError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "sessions handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeSessionsError(w, code, status, msg)
}

// classifySessionsError maps a Service error onto the canonical
// Protocol Code + HTTP status. The mapping is the single place the
// Sessions wire surface translates a Go error into a Protocol error.
func classifySessionsError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, sessionsprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, sessionsprotocol.ErrCrossTenantScope):
		return protoerrors.CodeScopeMismatch, http.StatusForbidden,
			m + ": cross-tenant query requires the verified `admin` scope claim"
	case errors.Is(err, sessionsprotocol.ErrSessionNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			m + ": session not found"
	case errors.Is(err, sessionsprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": invalid request — " + err.Error()
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeSessionsJSON encodes a successful Sessions-page response.
func writeSessionsJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "sessions handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeSessionsError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status. The body shape matches the
// REST control transport's error body so a client decodes both with
// the same Error wire type.
func writeSessionsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message})
}
