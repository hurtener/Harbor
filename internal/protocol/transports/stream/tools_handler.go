// Package stream — Wave 13 additions (Phase 73f): the `tools.*` HTTP
// handler. Like `pause.list` and `events.aggregate`, the seven Tools-
// page methods are one-shot request/response — POST JSON in, JSON out
// — and the handler lives in the stream package because its identity +
// cross-tenant scope-claim gating reuses the same `resolveIdentity` /
// `auth.HasScope` machinery the subscription surface uses.
//
// Route shape:
//
//	POST /v1/tools/{method}
//
// where {method} is the canonical method's verb suffix:
//
//	list | get | describe | metrics | content_stats |
//	set_approval_policy | revoke_oauth
//
// The handler reads identity from r.Context() (auth.Middleware) or the
// X-Harbor-* carrier headers (Phase 60 fallback), decodes the JSON body
// into the method-specific wire request, gates the two admin methods on
// the verified `auth.ScopeAdmin` claim (D-079 — there is NO `tools.admin`
// scope), dispatches into the toolsprotocol.Service, and encodes the
// response. On failure, a JSON error body with the canonical Protocol
// Code, identical in shape to the REST control transport's error body.
//
// The handler is READ-MOSTLY: five of the seven methods are pure reads.
// The two admin methods mutate runtime tool state and emit an
// `audit.admin_scope_used` event through the shipped audit.Redactor
// (the emit lives in toolsprotocol.Service — the handler never bypasses
// the audit redactor; CLAUDE.md §7 rule 6, §13).
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
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// ToolsRoutePattern is the http.ServeMux pattern the Tools handler
// registers under. The {method} wildcard is read via r.PathValue.
const ToolsRoutePattern = "POST /v1/tools/{method}"

// maxToolsBodyBytes bounds the Tools request body. The wire payloads
// are small (an identity scope + a filter + a tool ID); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body.
const maxToolsBodyBytes = 64 << 10

// ErrToolsMisconfigured — NewToolsHandler was called with a nil
// toolsprotocol.Service.
var ErrToolsMisconfigured = errors.New("stream: tools handler missing a mandatory dependency")

// ToolsHandler serves `POST /v1/tools/{method}`. It is the wire adapter
// over a toolsprotocol.Service: resolve identity, decode the request,
// gate admin methods on scope, dispatch, encode. The handler is a
// D-025-safe compiled artifact — every field is set once at
// construction; ServeHTTP holds no per-request state.
type ToolsHandler struct {
	service *toolsprotocol.Service
	logger  *slog.Logger
}

// ToolsOption configures NewToolsHandler at construction.
type ToolsOption func(*ToolsHandler)

// WithToolsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithToolsLogger(l *slog.Logger) ToolsOption {
	return func(h *ToolsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewToolsHandler builds the Tools handler over a toolsprotocol.Service.
// service is mandatory — a nil fails loud with ErrToolsMisconfigured
// rather than building a handler that would nil-panic on the first
// request (CLAUDE.md §5). The returned *ToolsHandler is immutable after
// construction (D-025) and safe for concurrent use by N goroutines.
func NewToolsHandler(service *toolsprotocol.Service, opts ...ToolsOption) (*ToolsHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: tools/protocol.Service is nil", ErrToolsMisconfigured)
	}
	h := &ToolsHandler{
		service: service,
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, decodes the
// method-specific wire request, gates admin methods on scope, dispatches
// into the toolsprotocol.Service, and encodes the response.
func (h *ToolsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"tools surface accepts POST only")
		return
	}

	// Resolve the canonical method from the {method} path verb.
	verb := r.PathValue("method")
	method := methods.Method("tools." + verb)
	if !methods.IsToolsMethod(method) {
		writeToolsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			fmt.Sprintf("unknown tools method %q", verb))
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9. A missing /
	// incomplete triple fails closed with CodeIdentityRequired (401).
	id, err := resolveIdentity(r)
	if err != nil {
		writeToolsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	// Decode the body — bounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxToolsBodyBytes))
	if err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}

	// adminScoped is the verified-JWT scope decision. The toolsprotocol
	// Service enforces the admin gate (D-079) — a false value on an
	// admin method fails closed with CodeIdentityScopeRequired (403).
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)

	wireScope := prototypes.IdentityScope{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	}

	switch method {
	case methods.MethodToolsList:
		h.serveList(w, r, body, wireScope)
	case methods.MethodToolsGet:
		h.serveGet(w, r, body, wireScope)
	case methods.MethodToolsDescribe:
		h.serveDescribe(w, r, body, wireScope)
	case methods.MethodToolsMetrics:
		h.serveMetrics(w, r, body, wireScope)
	case methods.MethodToolsContentStats:
		h.serveContentStats(w, r, body, wireScope)
	case methods.MethodToolsSetApprovalPolicy:
		h.serveSetApprovalPolicy(w, r, body, wireScope, adminScoped)
	case methods.MethodToolsRevokeOAuth:
		h.serveRevokeOAuth(w, r, body, wireScope, adminScoped)
	default:
		// Unreachable — IsToolsMethod gated the switch above.
		writeToolsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown tools method")
	}
}

// decodeToolsBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value of req (the identity
// scope is then overlaid from the verified identity by each handler).
func decodeToolsBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

func (h *ToolsHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.ToolListRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.list request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.List(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsList, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.ToolGetRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.get request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Get(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsGet, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveDescribe(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.ToolDescribeRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.describe request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Describe(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsDescribe, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveMetrics(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.ToolMetricsRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.metrics request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Metrics(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsMetrics, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveContentStats(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.ToolContentStatsRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.content_stats request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.ContentStats(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsContentStats, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveSetApprovalPolicy(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.ToolSetApprovalPolicyRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.set_approval_policy request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.SetApprovalPolicy(r.Context(), req, adminScoped)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsSetApprovalPolicy, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

func (h *ToolsHandler) serveRevokeOAuth(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.ToolRevokeOAuthRequest
	if err := decodeToolsBody(body, &req); err != nil {
		writeToolsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode tools.revoke_oauth request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.RevokeOAuth(r.Context(), req, adminScoped)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodToolsRevokeOAuth, err)
		return
	}
	writeToolsJSON(w, h.logger, r, resp)
}

// writeServiceError maps a toolsprotocol.Service sentinel error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
// method is the canonical methods.Method constant (single-sourced in
// internal/protocol/methods — never a hardcoded string).
func (h *ToolsHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyToolsError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "tools handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeToolsError(w, code, status, msg)
}

// classifyToolsError maps a Service error onto the canonical Protocol
// Code + HTTP status. The mapping is the single place the Tools wire
// surface translates a Go error into a Protocol error.
func classifyToolsError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, toolsprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, toolsprotocol.ErrAdminScopeRequired):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			m + ": requires the verified `admin` scope claim"
	case errors.Is(err, toolsprotocol.ErrToolNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			m + ": tool not found"
	case errors.Is(err, toolsprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": invalid request — " + err.Error()
	case errors.Is(err, toolsprotocol.ErrAdminUnsupported):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": admin backend not configured"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeToolsJSON encodes resp as a 200 JSON body.
func writeToolsJSON(w http.ResponseWriter, logger *slog.Logger, r *http.Request, resp any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.WarnContext(r.Context(), "tools handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeToolsError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body so a client decodes both with the
// same Error wire type.
func writeToolsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{
		Code:    code,
		Message: message,
	})
}
