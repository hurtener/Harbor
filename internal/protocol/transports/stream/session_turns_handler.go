// Package stream — addition: the `sessions.turns.*` read handler.
// Like the Sessions-page handler, these are one-shot request/response
// endpoints — POST JSON in, JSON out — sharing the same
// `resolveIdentity` + defence-in-depth body-identity machinery.
//
// Route shapes (both POST, pinned explicitly — nested session routes
// are NEVER derived generically):
//
//	POST /v1/sessions/turns/list — one newest-first keyset page of the
//	                              caller's exact session (consumer lane).
//	POST /v1/sessions/turns/get  — one (session, task) read on either
//	                              the consumer lane or the elevated
//	                              operations DTO lane.
//
// Both methods are identity-mandatory. The consumer lane is
// exact-session: a foreign-session read answers typed not-found
// (non-oracular). The operations lane (sessions.turns.get with the
// `operations` projection) is the elevated admin/fleet observation
// surface — the SERVICE enforces the closed two-scope admit set and
// emits the widened-read `audit.admin_scope_used` event before the
// projection read; the handler does not re-implement the gate.
//
// SessionTurnsHandler is a concurrency-safe compiled artifact — service /
// logger are set once at construction; ServeHTTP holds no per-request
// state.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	turns "github.com/hurtener/Harbor/internal/sessions/turns"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
)

// SessionTurnsRoutePattern is the http.ServeMux prefix pattern the
// session-turns handler registers under. The handler internally branches
// on the trailing path segment (`list` / `get`) to dispatch.
const SessionTurnsRoutePattern = "POST /v1/sessions/turns/"

// maxSessionTurnsBodyBytes bounds a session-turns request body. The wire
// payloads are small (an identity scope + a session id + a cursor + a
// limit); 64 KiB is comfortably over the realistic ceiling and fails
// closed on a client that streams an unbounded body at the edge.
const maxSessionTurnsBodyBytes = 64 << 10

// ErrSessionTurnsMisconfigured — NewSessionTurnsHandler was called with
// a nil turns/protocol.Service.
var ErrSessionTurnsMisconfigured = errors.New("stream: session-turns handler missing a mandatory dependency")

// SessionTurnsHandler serves the two `POST /v1/sessions/turns/*` routes.
// It is the wire adapter over a *turnsprotocol.Service: resolve
// identity, branch on the trailing path segment, decode the request,
// dispatch, encode.
type SessionTurnsHandler struct {
	service *turnsprotocol.Service
	logger  *slog.Logger
}

// SessionTurnsOption configures NewSessionTurnsHandler at construction.
type SessionTurnsOption func(*SessionTurnsHandler)

// WithSessionTurnsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithSessionTurnsLogger(l *slog.Logger) SessionTurnsOption {
	return func(h *SessionTurnsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewSessionTurnsHandler builds the session-turns handler over a
// *turnsprotocol.Service. service is mandatory — a nil fails loud with
// ErrSessionTurnsMisconfigured rather than building a handler that would
// nil-panic on the first request (CLAUDE.md §5).
//
// The returned *SessionTurnsHandler is immutable after construction
// and safe for concurrent use by N goroutines.
func NewSessionTurnsHandler(service *turnsprotocol.Service, opts ...SessionTurnsOption) (*SessionTurnsHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: turns/protocol.Service is nil", ErrSessionTurnsMisconfigured)
	}
	h := &SessionTurnsHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, branches on
// the trailing path segment (pinned explicitly), decodes the body,
// dispatches to the service, and encodes the response.
func (h *SessionTurnsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeSessionTurnsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"sessions.turns endpoints accept POST only")
		return
	}
	id, r, err := resolveIdentity(r)
	if err != nil {
		writeSessionTurnsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionTurnsBodyBytes))
	if err != nil {
		writeSessionTurnsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	// The routes are PINNED EXPLICITLY — nested session routes are never
	// derived generically (a generic `sessions/<suffix>` derivation would
	// map `sessions.turns.get` onto the Sessions-page `sessions.get`
	// route, which does not exist).
	switch strings.TrimPrefix(r.URL.Path, "/v1/sessions/turns/") {
	case "list":
		h.serveList(w, r, body, wireID)
	case "get":
		h.serveGet(w, r, body, wireID)
	default:
		writeSessionTurnsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown sessions.turns method route")
	}
}

func (h *SessionTurnsHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.SessionTurnsListRequest
	if err := decodeSessionTurnsBody(body, &req); err != nil {
		writeSessionTurnsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode sessions.turns.list request: "+err.Error())
		return
	}
	if perr := reconcileSessionTurnsScope(r, &req.Identity); perr != nil {
		writeSessionTurnsError(w, perr.Code, bodyScopeStatus(perr.Code), perr.Message)
		return
	}
	req.Identity = wireID
	resp, err := h.service.List(r.Context(), turnsprotocol.ListRequest{
		SessionID:   req.SessionID,
		OlderCursor: req.OlderCursor,
		Limit:       req.Limit,
		Projection:  turnsprotocol.Projection(req.Projection),
	})
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionTurnsList, err)
		return
	}
	writeSessionTurnsJSON(w, r, projectSessionTurnsList(resp), h.logger)
}

func (h *SessionTurnsHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.SessionTurnsGetRequest
	if err := decodeSessionTurnsBody(body, &req); err != nil {
		writeSessionTurnsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode sessions.turns.get request: "+err.Error())
		return
	}
	if perr := reconcileSessionTurnsScope(r, &req.Identity); perr != nil {
		writeSessionTurnsError(w, perr.Code, bodyScopeStatus(perr.Code), perr.Message)
		return
	}
	req.Identity = wireID
	resp, err := h.service.Get(r.Context(), turnsprotocol.GetRequest{
		SessionID:  req.SessionID,
		TaskID:     req.TaskID,
		Projection: turnsprotocol.Projection(req.Projection),
	})
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionTurnsGet, err)
		return
	}
	writeSessionTurnsJSON(w, r, projectSessionTurnsGet(resp), h.logger)
}

// reconcileSessionTurnsScope routes the request body's identity scope
// through the shared body-identity gate under the session-turns
// surface's registered policy — the same defence-in-depth machinery the
// Sessions-page handler uses.
func reconcileSessionTurnsScope(r *http.Request, scope *prototypes.IdentityScope) *protoerrors.Error {
	_, perr := bodyscope.Reconcile(r.Context(), bodyscope.ForIdentityScope(scope), bodyscope.SurfaceSessions, nil)
	return perr
}

// decodeSessionTurnsBody strictly decodes a session-turns request body.
// A decode failure surfaces as CodeInvalidRequest — never a silent
// zero-value request.
func decodeSessionTurnsBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	return decodeGovernanceBody(body, req)
}

// writeServiceError maps a service error onto a canonical Protocol Code +
// HTTP status + safe operator-facing message.
func (h *SessionTurnsHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifySessionTurnsError(err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "session-turns handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeSessionTurnsError(w, code, status, msg)
}

// classifySessionTurnsError maps a turns service / projection error onto
// a canonical Protocol Code + HTTP status — the single place the
// session-turns wire surface translates a Go error into a Protocol
// error. An otherwise-current App (or turn) never collapses to a generic
// runtime error: not-found stays not-found, scope stays scope.
func classifySessionTurnsError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, turnsprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete"
	case errors.Is(err, turnsprotocol.ErrInvalidRequest),
		errors.Is(err, turns.ErrInvalidCursor):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"sessions.turns request invalid: " + err.Error()
	case errors.Is(err, turnsprotocol.ErrTurnNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			"sessions.turns: turn not found"
	case errors.Is(err, turnsprotocol.ErrOperationsScopeDenied):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"sessions.turns operations projection requires a verified admin or console:fleet scope"
	case errors.Is(err, turnsprotocol.ErrSessionReachDenied):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"sessions.turns: session outside the caller's verified session reach"
	case errors.Is(err, turnsprotocol.ErrMisconfigured):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"sessions.turns: session-turns service is misconfigured"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"sessions.turns request failed: " + err.Error()
	}
}

// writeSessionTurnsJSON encodes a successful response.
func writeSessionTurnsJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "session-turns handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeSessionTurnsError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status.
func writeSessionTurnsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
