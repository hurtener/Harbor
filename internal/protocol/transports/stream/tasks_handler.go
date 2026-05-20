// Package stream — Wave 13 additions (Phase 73d): the `tasks.*` HTTP
// handler. Like `tools.*` and `pause.list`, the two Tasks-page read
// methods are one-shot request/response — POST JSON in, JSON out —
// and the handler lives in the stream package because its identity +
// cross-tenant scope-claim gating reuses the same `resolveIdentity` /
// `auth.HasScope` machinery the subscription surface uses.
//
// Route shape:
//
//	POST /v1/tasks/{method}
//
// where {method} is the canonical method's verb suffix:
//
//	list | get
//
// The handler reads identity from r.Context() (auth.Middleware) or the
// X-Harbor-* carrier headers (Phase 60 fallback), decodes the JSON body
// into the method-specific wire request, computes the verified
// `auth.ScopeAdmin` claim (consulted by `tasks.list` only for a
// cross-tenant fan-in — D-079), dispatches into the tasksprotocol.Service,
// and encodes the response. On failure, a JSON error body with the
// canonical Protocol Code, identical in shape to the REST control
// transport's error body.
//
// The handler is READ-ONLY: both methods are pure reads. The Console
// Tasks page consumes the EXISTING Phase 54 task-control verbs
// (`cancel` / `pause` / `resume` / `prioritize` / `approve` / `reject`)
// for mutation through the control transport — there is NO `tasks.*`
// mutating method (CLAUDE.md §13 "no parallel implementations").

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
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// TasksRoutePattern is the http.ServeMux pattern the Tasks handler
// registers under. The {method} wildcard is read via r.PathValue.
const TasksRoutePattern = "POST /v1/tasks/{method}"

// maxTasksBodyBytes bounds the Tasks request body. The wire payloads
// are small (an identity scope + a filter + a task ID); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body.
const maxTasksBodyBytes = 64 << 10

// ErrTasksMisconfigured — NewTasksHandler was called with a nil
// tasksprotocol.Service.
var ErrTasksMisconfigured = errors.New("stream: tasks handler missing a mandatory dependency")

// TasksHandler serves `POST /v1/tasks/{method}`. It is the wire adapter
// over a tasksprotocol.Service: resolve identity, decode the request,
// compute the admin-scope claim, dispatch, encode. The handler is a
// D-025-safe compiled artifact — every field is set once at
// construction; ServeHTTP holds no per-request state.
type TasksHandler struct {
	service *tasksprotocol.Service
	logger  *slog.Logger
}

// TasksOption configures NewTasksHandler at construction.
type TasksOption func(*TasksHandler)

// WithTasksLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithTasksLogger(l *slog.Logger) TasksOption {
	return func(h *TasksHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewTasksHandler builds the Tasks handler over a tasksprotocol.Service.
// service is mandatory — a nil fails loud with ErrTasksMisconfigured
// rather than building a handler that would nil-panic on the first
// request (CLAUDE.md §5). The returned *TasksHandler is immutable after
// construction (D-025) and safe for concurrent use by N goroutines.
func NewTasksHandler(service *tasksprotocol.Service, opts ...TasksOption) (*TasksHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: tasks/protocol.Service is nil", ErrTasksMisconfigured)
	}
	h := &TasksHandler{
		service: service,
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, decodes the
// method-specific wire request, dispatches into the tasksprotocol.Service,
// and encodes the response.
func (h *TasksHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTasksError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"tasks surface accepts POST only")
		return
	}

	// Resolve the canonical method from the {method} path verb.
	verb := r.PathValue("method")
	method := methods.Method("tasks." + verb)
	if !methods.IsTasksMethod(method) {
		writeTasksError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			fmt.Sprintf("unknown tasks method %q", verb))
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9. A missing /
	// incomplete triple fails closed with CodeIdentityRequired (401).
	id, err := resolveIdentity(r)
	if err != nil {
		writeTasksError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	// Decode the body — bounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTasksBodyBytes))
	if err != nil {
		writeTasksError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}

	// adminScoped is the verified-JWT scope decision. `tasks.list`
	// consults it only for a cross-tenant fan-in (D-079); `tasks.get`
	// ignores it.
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)

	wireScope := prototypes.IdentityScope{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	}

	switch method {
	case methods.MethodTasksList:
		h.serveList(w, r, body, wireScope, adminScoped)
	case methods.MethodTasksGet:
		h.serveGet(w, r, body, wireScope)
	default:
		// Unreachable — IsTasksMethod gated the switch above.
		writeTasksError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown tasks method")
	}
}

// decodeTasksBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value of req (the identity
// scope is then overlaid from the verified identity by each handler).
func decodeTasksBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

func (h *TasksHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.TaskListRequest
	if err := decodeTasksBody(body, &req); err != nil {
		writeTasksError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tasks.list request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.List(r.Context(), req, adminScoped)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodTasksList, err)
		return
	}
	writeTasksJSON(w, h.logger, r, resp)
}

func (h *TasksHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.TaskGetRequest
	if err := decodeTasksBody(body, &req); err != nil {
		writeTasksError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tasks.get request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Get(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodTasksGet, err)
		return
	}
	writeTasksJSON(w, h.logger, r, resp)
}

// writeServiceError maps a tasksprotocol.Service sentinel error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
func (h *TasksHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyTasksError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "tasks handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeTasksError(w, code, status, msg)
}

// classifyTasksError maps a Service error onto the canonical Protocol
// Code + HTTP status. The mapping is the single place the Tasks wire
// surface translates a Go error into a Protocol error.
func classifyTasksError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, tasksprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, tasksprotocol.ErrScopeMismatch):
		return protoerrors.CodeScopeMismatch, http.StatusForbidden,
			m + ": cross-tenant query requires the verified `admin` scope claim"
	case errors.Is(err, tasksprotocol.ErrTaskNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			m + ": task not found"
	case errors.Is(err, tasksprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": invalid request — " + err.Error()
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeTasksJSON encodes resp as a 200 JSON body.
func writeTasksJSON(w http.ResponseWriter, logger *slog.Logger, r *http.Request, resp any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.WarnContext(r.Context(), "tasks handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeTasksError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body so a client decodes both with the
// same Error wire type.
func writeTasksError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{
		Code:    code,
		Message: message,
	})
}
