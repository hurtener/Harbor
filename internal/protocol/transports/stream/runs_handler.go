// Package stream — Wave 13 addition (Phase 73n / D-130): the Console
// Playground-page `runs.set_overrides` HTTP handler. Like the Sessions /
// Tasks / Flows page handlers, this is a one-shot request/response
// endpoint — POST JSON in, JSON out. It lives in the stream package
// because its identity resolution + defence-in-depth body-identity
// check are the same `resolveIdentity` machinery the other Console-page
// handlers use.
//
// Route shape (POST):
//
//	POST /v1/runs/set_overrides — record the next-message override
//
// The route is identity-mandatory. The override's target session
// (`overrides.session_id`) MUST equal the caller's verified session;
// the runs/protocol.Service rejects a cross-session target with
// CodeScopeMismatch (403). `runs.set_overrides` is NOT an admin method
// — it records an override for the operator's OWN session, so no
// cross-tenant scope claim is consulted.
//
// RunsHandler is a D-025-safe compiled artifact — service / logger are
// set once at construction; ServeHTTP holds no per-request state.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// RunsRoutePattern is the http.ServeMux prefix pattern the Runs-page
// handler registers under. The handler internally branches on the
// trailing path segment to dispatch.
const RunsRoutePattern = "POST /v1/runs/"

// maxRunsBodyBytes bounds a Runs-page request body. The wire payload is
// small (an identity scope + four optional override fields, one of them
// a system-prompt string); 256 KiB is comfortably over the realistic
// ceiling — a system prompt is bounded text, never heavy content — and
// fails closed on a client that streams an unbounded body at the edge.
const maxRunsBodyBytes = 256 << 10

// ErrRunsMisconfigured — NewRunsHandler was called with a nil
// runs/protocol.Service.
var ErrRunsMisconfigured = errors.New("stream: runs handler missing a mandatory dependency")

// RunsHandler serves the `POST /v1/runs/*` routes. It is the wire
// adapter over a *runsprotocol.Service: resolve identity, branch on the
// trailing path segment, decode the request, dispatch, encode.
type RunsHandler struct {
	service *runsprotocol.Service
	logger  *slog.Logger
}

// RunsOption configures NewRunsHandler at construction.
type RunsOption func(*RunsHandler)

// WithRunsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithRunsLogger(l *slog.Logger) RunsOption {
	return func(h *RunsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewRunsHandler builds the Runs-page handler over a
// *runsprotocol.Service. service is mandatory — a nil fails loud with
// ErrRunsMisconfigured rather than building a handler that would
// nil-panic on the first request (CLAUDE.md §5).
//
// The returned *RunsHandler is immutable after construction (D-025) and
// safe for concurrent use by N goroutines.
func NewRunsHandler(service *runsprotocol.Service, opts ...RunsOption) (*RunsHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: runs/protocol.Service is nil", ErrRunsMisconfigured)
	}
	h := &RunsHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, branches on
// the trailing path segment, decodes the body, dispatches to the
// service, and encodes the response.
func (h *RunsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeRunsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"runs endpoints accept POST only")
		return
	}
	id, err := resolveIdentity(r)
	if err != nil {
		writeRunsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunsBodyBytes))
	if err != nil {
		writeRunsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	switch strings.TrimPrefix(r.URL.Path, "/v1/runs/") {
	case "set_overrides":
		h.serveSetOverrides(w, r, body, wireID)
	default:
		writeRunsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown runs method route")
	}
}

func (h *RunsHandler) serveSetOverrides(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.RunSetOverridesRequest
	if err := decodeRunsBody(body, &req); err != nil {
		writeRunsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode runs.set_overrides request: "+err.Error())
		return
	}
	// Defence-in-depth: a body identity, when present, must match the
	// verified identity. A caller cannot present a valid JWT for one
	// identity while submitting a body claiming another.
	if perr := assertRunsIdentity(req.Identity, wireID); perr != "" {
		writeRunsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	// If the override omitted session_id, default it to the verified
	// session — the Playground always records for the operator's own
	// session, so an absent session_id is the common shape.
	if req.Overrides.SessionID == "" {
		req.Overrides.SessionID = wireID.Session
	}
	resp, err := h.service.SetOverrides(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodRunsSetOverrides, err)
		return
	}
	writeRunsJSON(w, r, resp, h.logger)
}

// decodeRunsBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value (the identity scope
// is then overlaid from the verified identity).
func decodeRunsBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

// assertRunsIdentity is the defence-in-depth check: when the request
// body carries an identity, every non-empty component MUST match the
// verified identity. Returns "" when the body identity is consistent
// (or empty).
func assertRunsIdentity(body, verified prototypes.IdentityScope) string {
	if (body.Tenant != "" && body.Tenant != verified.Tenant) ||
		(body.User != "" && body.User != verified.User) ||
		(body.Session != "" && body.Session != verified.Session) {
		return "body identity scope does not match the verified identity"
	}
	return ""
}

// writeServiceError maps a runs/protocol.Service error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message.
func (h *RunsHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyRunsError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "runs handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeRunsError(w, code, status, msg)
}

// classifyRunsError maps a Service error onto the canonical Protocol
// Code + HTTP status. The mapping is the single place the Runs wire
// surface translates a Go error into a Protocol error.
func classifyRunsError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, runsprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, runsprotocol.ErrCrossSessionScope):
		return protoerrors.CodeScopeMismatch, http.StatusForbidden,
			m + ": override targets a session outside the caller's verified scope"
	case errors.Is(err, runsprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": invalid request — " + err.Error()
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeRunsJSON encodes a successful Runs-page response.
func writeRunsJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "runs handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeRunsError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body.
func writeRunsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
