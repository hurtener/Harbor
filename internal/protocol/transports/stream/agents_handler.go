// Package stream — additions: the `agents.*` HTTP
// handler. Like `tools.*` and `memory.*`, the eight Agents-page methods
// are one-shot request/response — POST JSON in, JSON out — and the
// handler lives in the stream package because its identity + scope-claim
// gating reuses the same `resolveIdentity` / `auth.HasScope` machinery
// the subscription surface uses.
//
// Route shape:
//
//	POST /v1/agents/{method}
//
// where {method} is the canonical method's verb suffix:
//
//	list | get | tools | memory | governance | skills |
//	permissions | metrics
//
// The handler reads identity from r.Context() (auth.Middleware) or the
// X-Harbor-* carrier headers (fallback), decodes the JSON body
// into the method-specific wire request, dispatches into the
// agentsprotocol.Service, and encodes the response. On failure, a JSON
// error body with the canonical Protocol Code, identical in shape to the
// REST control transport's error body.
//
// All eight `agents.*` methods are READ-ONLY projections of the Agent
// Registry. The five agent-control verbs the Console exposes
// (Pause / Drain / Restart / Force-Stop / Deregister) are NOT served
// here — they are the EXISTING shipped `registry.*` control verbs
// and Harbor mints no control method (CLAUDE.md §13).
//
// Identity is mandatory: a missing / incomplete (tenant, user, session)
// triple fails closed with CodeIdentityRequired (401). `agent_id` is a
// registration identity, not an isolation filter — the runtime scopes
// by the tuple, never by `agent_id` (CLAUDE.md §6).
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
)

// AgentsRoutePattern is the http.ServeMux pattern the Agents handler
// registers under. The {method} wildcard is read via r.PathValue.
const AgentsRoutePattern = "POST /v1/agents/{method}"

// maxAgentsBodyBytes bounds the Agents request body. The wire payloads
// are small (an identity scope + a filter + an agent ID); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body.
const maxAgentsBodyBytes = 64 << 10

// ErrAgentsMisconfigured — NewAgentsHandler was called with a nil
// agentsprotocol.Service.
var ErrAgentsMisconfigured = errors.New("stream: agents handler missing a mandatory dependency")

// AgentsHandler serves `POST /v1/agents/{method}`. It is the wire
// adapter over an agentsprotocol.Service: resolve identity, decode the
// request, dispatch, encode. The handler is a concurrency-safe compiled
// artifact — every field is set once at construction; ServeHTTP holds
// no per-request state.
type AgentsHandler struct {
	service *agentsprotocol.Service
	logger  *slog.Logger
}

// AgentsOption configures NewAgentsHandler at construction.
type AgentsOption func(*AgentsHandler)

// WithAgentsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithAgentsLogger(l *slog.Logger) AgentsOption {
	return func(h *AgentsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewAgentsHandler builds the Agents handler over an
// agentsprotocol.Service. service is mandatory — a nil fails loud with
// ErrAgentsMisconfigured rather than building a handler that would
// nil-panic on the first request (CLAUDE.md §5). The returned
// *AgentsHandler is immutable after construction and safe for
// concurrent use by N goroutines.
func NewAgentsHandler(service *agentsprotocol.Service, opts ...AgentsOption) (*AgentsHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: registry/protocol.Service is nil", ErrAgentsMisconfigured)
	}
	h := &AgentsHandler{
		service: service,
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, decodes the
// method-specific wire request, dispatches into the
// agentsprotocol.Service, and encodes the response.
func (h *AgentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"agents surface accepts POST only")
		return
	}

	verb := r.PathValue("method")
	method := methods.Method("agents." + verb)
	if !methods.IsAgentsMethod(method) {
		writeAgentsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			fmt.Sprintf("unknown agents method %q", verb))
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9. A missing /
	// incomplete triple fails closed with CodeIdentityRequired (401).
	id, err := resolveIdentity(r)
	if err != nil {
		writeAgentsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAgentsBodyBytes))
	if err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}

	// Fold the resolved identity triple into the request context. The
	// Agent Registry's storage methods scope every read by the
	// (tenant, user, session) tuple read FROM the context (CLAUDE.md §6
	// rule 3) — NEVER by agent_id. The agents.* Service therefore needs
	// the verified triple on the ctx it dispatches with. resolveIdentity
	// already failed closed above on an incomplete triple, so identity.
	// With cannot fail here; a defensive error still fails loud rather
	// than dispatching with an identity-less context.
	idCtx, err := identity.With(r.Context(), id)
	if err != nil {
		writeAgentsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	r = r.WithContext(idCtx)

	wireScope := prototypes.IdentityScope{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	}

	// The fleet-control verbs MUTATE registry state and require the
	// verified `auth.ScopeAdmin` control claim — the same
	// admin gate the tools admin verbs use. The read methods skip the
	// check. The service is the authoritative gate (it fails closed
	// without the bool); this is the verified-JWT scope decision passed
	// into it.
	controlScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)

	switch method {
	case methods.MethodAgentsList:
		h.serveList(w, r, body, wireScope)
	case methods.MethodAgentsGet:
		h.serveGet(w, r, body, wireScope)
	case methods.MethodAgentsTools:
		h.serveTools(w, r, body, wireScope)
	case methods.MethodAgentsMemory:
		h.serveMemory(w, r, body, wireScope)
	case methods.MethodAgentsGovernance:
		h.serveGovernance(w, r, body, wireScope)
	case methods.MethodAgentsSkills:
		h.serveSkills(w, r, body, wireScope)
	case methods.MethodAgentsPermissions:
		h.servePermissions(w, r, body, wireScope)
	case methods.MethodAgentsMetrics:
		h.serveMetrics(w, r, body, wireScope)
	case methods.MethodAgentsPause:
		h.serveControl(w, r, body, wireScope, methods.MethodAgentsPause, controlScoped, h.service.Pause)
	case methods.MethodAgentsDrain:
		h.serveControl(w, r, body, wireScope, methods.MethodAgentsDrain, controlScoped, h.service.Drain)
	case methods.MethodAgentsRestart:
		h.serveControl(w, r, body, wireScope, methods.MethodAgentsRestart, controlScoped, h.service.Restart)
	case methods.MethodAgentsForceStop:
		h.serveControl(w, r, body, wireScope, methods.MethodAgentsForceStop, controlScoped, h.service.ForceStop)
	case methods.MethodAgentsDeregister:
		h.serveControl(w, r, body, wireScope, methods.MethodAgentsDeregister, controlScoped, h.service.Deregister)
	default:
		// Unreachable — IsAgentsMethod gated the switch above.
		writeAgentsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown agents method")
	}
}

// decodeAgentsBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value of req (the identity
// scope is then overlaid from the verified identity by each handler).
func decodeAgentsBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

func (h *AgentsHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentListRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.list request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.List(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsList, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentGetRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.get request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Get(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsGet, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveTools(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentToolsRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.tools request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Tools(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsTools, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveMemory(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentMemoryRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.memory request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Memory(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsMemory, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveGovernance(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentGovernanceRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.governance request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Governance(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsGovernance, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveSkills(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentSkillsRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.skills request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Skills(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsSkills, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) servePermissions(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentPermissionsRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.permissions request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Permissions(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsPermissions, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

func (h *AgentsHandler) serveMetrics(w http.ResponseWriter, r *http.Request, body []byte, scope prototypes.IdentityScope) {
	var req prototypes.AgentMetricsRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode agents.metrics request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := h.service.Metrics(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentsMetrics, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

// serveControl is the shared wire adapter for the five fleet-control
// verbs. It decodes the shared AgentControlRequest,
// overlays the verified identity, and dispatches into the matching
// Service control method, passing the verified-JWT control-scope
// decision. The Service is the authoritative gate — it fails closed with
// ErrControlScopeRequired when controlScoped is false (classified to 403
// below), so a leaked read-only token can never drive a control verb.
func (h *AgentsHandler) serveControl(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	scope prototypes.IdentityScope,
	method methods.Method,
	controlScoped bool,
	invoke func(context.Context, prototypes.AgentControlRequest, bool) (prototypes.AgentControlResponse, error),
) {
	var req prototypes.AgentControlRequest
	if err := decodeAgentsBody(body, &req); err != nil {
		writeAgentsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode "+string(method)+" request: "+err.Error())
		return
	}
	req.Identity = scope
	resp, err := invoke(r.Context(), req, controlScoped)
	if err != nil {
		h.writeServiceError(w, r, method, err)
		return
	}
	writeAgentsJSON(w, h.logger, r, resp)
}

// writeServiceError maps an agentsprotocol.Service sentinel error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
func (h *AgentsHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyAgentsError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "agents handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeAgentsError(w, code, status, msg)
}

// classifyAgentsError maps a Service error onto the canonical Protocol
// Code + HTTP status. The mapping is the single place the Agents wire
// surface translates a Go error into a Protocol error.
func classifyAgentsError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, agentsprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, agentsprotocol.ErrAgentNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			m + ": agent not found"
	case errors.Is(err, agentsprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": invalid request — " + err.Error()
	case errors.Is(err, agentsprotocol.ErrControlScopeRequired):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			m + ": fleet control requires the elevated admin control claim"
	case errors.Is(err, agentsprotocol.ErrControlUnsupported):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": fleet control is not wired on this runtime"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeAgentsJSON encodes resp as a 200 JSON body.
func writeAgentsJSON(w http.ResponseWriter, logger *slog.Logger, r *http.Request, resp any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.WarnContext(r.Context(), "agents handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeAgentsError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body so a client decodes both with the
// same Error wire type.
func writeAgentsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{ //nolint:errcheck // response status already committed — a write error cannot be recovered here.
		Code:    code,
		Message: message,
	})
}
