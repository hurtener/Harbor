// Package stream — addition: the admin-scoped `agent_config.*`
// agent-config control-plane HTTP handler. Like the governance handler,
// this is a one-shot request/response endpoint — POST JSON in, JSON out —
// sharing the same `resolveIdentity` + defence-in-depth body-identity
// machinery.
//
// Route shape (POST):
//
//	POST /v1/agent_config/get             — read the active revision
//	POST /v1/agent_config/set_revision    — write a new revision
//	POST /v1/agent_config/list_revisions  — the revision chain
//	POST /v1/agent_config/diff            — compare two revisions
//	POST /v1/agent_config/rollback        — repoint the active pointer
//	POST /v1/agent_config/set_tool_exposure — set MCP pause/resume + per-tool policy
//	POST /v1/agent_config/set_prompt_layers — set the layered system prompt (base + user)
//	POST /v1/agent_config/add_mcp_connection — add a NEW MCP server connection (dial + handshake + OAuth)
//	POST /v1/agent_config/skills/list     — list the agent's skills
//	POST /v1/agent_config/skills/upsert   — upsert a skill (records a rev)
//	POST /v1/agent_config/skills/delete   — delete a skill (records a rev)
//
// Every route is identity-mandatory AND admin-scoped (the whole family
// changes/reads what an agent can do — a capability surface): the
// verified `auth.ScopeAdmin` claim is required. A non-admin caller is
// rejected with CodeScopeMismatch (403) before the request reaches the
// service. Authority derives from the verified ctx, never the request body
// (authority from the verified ctx). The handler overlays the verified identity onto the request
// body.
//
// AgentConfigHandler is a concurrency-safe compiled artifact — service /
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

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
)

// AgentConfigRoutePattern is the http.ServeMux prefix pattern the
// agent-config admin handler registers under. The handler internally
// branches on the trailing path segment to dispatch.
const AgentConfigRoutePattern = "POST /v1/agent_config/"

// maxAgentConfigBodyBytes bounds an agent-config admin request body. The
// wire payloads are small (an identity scope + an agent id + a skills
// membership / a single skill input); 1 MiB is comfortably over the
// realistic ceiling and fails closed on a client that streams an
// unbounded body.
const maxAgentConfigBodyBytes = 1 << 20

// ErrAgentConfigMisconfigured — NewAgentConfigHandler was called with a
// nil service.
var ErrAgentConfigMisconfigured = errors.New("stream: agent-config handler missing a mandatory dependency")

// AgentConfigHandler serves the `POST /v1/agent_config/*` admin routes.
type AgentConfigHandler struct {
	service *agentcfgprotocol.Service
	logger  *slog.Logger
}

// AgentConfigOption configures NewAgentConfigHandler.
type AgentConfigOption func(*AgentConfigHandler)

// WithAgentConfigLogger sets the slog.Logger. A nil logger routes to
// slog.Default().
func WithAgentConfigLogger(l *slog.Logger) AgentConfigOption {
	return func(h *AgentConfigHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewAgentConfigHandler builds the agent-config admin handler over a
// *agentcfgprotocol.Service. service is mandatory — a nil fails loud with
// ErrAgentConfigMisconfigured (CLAUDE.md §5).
//
// The returned *AgentConfigHandler is immutable after construction and
// safe for concurrent use by N goroutines.
func NewAgentConfigHandler(service *agentcfgprotocol.Service, opts ...AgentConfigOption) (*AgentConfigHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: agentcfg/protocol.Service is nil", ErrAgentConfigMisconfigured)
	}
	h := &AgentConfigHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *AgentConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentConfigError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"agent_config endpoints accept POST only")
		return
	}
	id, err := resolveIdentity(r)
	if err != nil {
		writeAgentConfigError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	// Per-route authorization tier (the authorization scope matrix). The
	// `session/*` routes are the SAFE SUBSET (the non-admin lower tier): a
	// session-scoped caller may set the user prompt layer, narrow-only
	// source/tool disable, and manage ephemeral personal skills — these
	// require a valid identity but NOT the admin scope. EVERY other
	// `agent_config.*` route reads/mutates the agent-level capability surface
	// and requires the verified `auth.ScopeAdmin` claim. Authority derives
	// from the verified ctx, never the request body; a non-admin caller on an
	// admin route is rejected with CodeScopeMismatch before any dispatch
	// (fail-closed).
	route := strings.TrimPrefix(r.URL.Path, "/v1/agent_config/")
	if !agentConfigSessionSafeRoutes[route] {
		if !auth.HasScope(r.Context(), auth.ScopeAdmin) {
			writeAgentConfigError(w, protoerrors.CodeScopeMismatch, http.StatusForbidden,
				"agent_config methods require the admin scope claim")
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAgentConfigBodyBytes))
	if err != nil {
		writeAgentConfigError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	switch route {
	case "get":
		h.serveGet(w, r, body, wireID)
	case "set_revision":
		h.serveSetRevision(w, r, body, wireID)
	case "list_revisions":
		h.serveListRevisions(w, r, body, wireID)
	case "diff":
		h.serveDiff(w, r, body, wireID)
	case "rollback":
		h.serveRollback(w, r, body, wireID)
	case "set_tool_exposure":
		h.serveSetToolExposure(w, r, body, wireID)
	case "set_prompt_layers":
		h.serveSetPromptLayers(w, r, body, wireID)
	case "add_mcp_connection":
		h.serveAddMCPConnection(w, r, body, wireID)
	case "skills/list":
		h.serveSkillsList(w, r, body, wireID)
	case "skills/upsert":
		h.serveSkillsUpsert(w, r, body, wireID)
	case "skills/delete":
		h.serveSkillsDelete(w, r, body, wireID)
	case "session/set_user_prompt":
		h.serveSessionSetUserPrompt(w, r, body, wireID)
	case "session/set_source_disables":
		h.serveSessionSetSourceDisables(w, r, body, wireID)
	case "session/skills/list":
		h.serveSessionSkillsList(w, r, body, wireID)
	case "session/skills/upsert":
		h.serveSessionSkillsUpsert(w, r, body, wireID)
	case "session/skills/delete":
		h.serveSessionSkillsDelete(w, r, body, wireID)
	default:
		writeAgentConfigError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown agent_config method route")
	}
}

// agentConfigSessionSafeRoutes is the closed set of trailing path segments
// that are the SAFE SUBSET (the non-admin lower tier of the authorization
// matrix). A session-scoped caller is permitted on EXACTLY these routes
// (identity-mandatory, no admin scope required); every other
// `agent_config.*` route is admin-gated. The set mirrors
// methods.canonicalAgentConfigSessionMethods — adding a session-safe verb
// extends BOTH.
var agentConfigSessionSafeRoutes = map[string]bool{
	"session/set_user_prompt":     true,
	"session/set_source_disables": true,
	"session/skills/list":         true,
	"session/skills/upsert":       true,
	"session/skills/delete":       true,
}

func (h *AgentConfigHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigGetRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigGet) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.Get(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigGet, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSetRevision(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSetRevisionRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSetRevision) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SetRevision(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSetRevision, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveListRevisions(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigListRevisionsRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigListRevisions) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.ListRevisions(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigListRevisions, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveDiff(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigDiffRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigDiff) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.Diff(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigDiff, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveRollback(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigRollbackRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigRollback) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.Rollback(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigRollback, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSetToolExposure(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSetToolExposureRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSetToolExposure) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SetToolExposure(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSetToolExposure, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSetPromptLayers(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSetPromptLayersRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSetPromptLayers) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SetPromptLayers(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSetPromptLayers, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveAddMCPConnection(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigAddMCPConnectionRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigAddMCPConnection) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.AddMCPConnection(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigAddMCPConnection, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSkillsList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSkillsListRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSkillsList) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SkillsList(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSkillsList, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSkillsUpsert(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSkillsUpsertRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSkillsUpsert) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SkillsUpsert(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSkillsUpsert, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSkillsDelete(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSkillsDeleteRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSkillsDelete) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SkillsDelete(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSkillsDelete, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

// --- session-safe (non-admin) routes ---

func (h *AgentConfigHandler) serveSessionSetUserPrompt(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSessionSetUserPromptRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSessionSetUserPrompt) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SessionSetUserPrompt(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSessionSetUserPrompt, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSessionSetSourceDisables(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSessionSetSourceDisablesRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSessionSetSourceDisables) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SessionSetSourceDisables(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSessionSetSourceDisables, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSessionSkillsList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSessionSkillsListRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSessionSkillsList) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SessionSkillsList(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSessionSkillsList, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSessionSkillsUpsert(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSessionSkillsUpsertRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSessionSkillsUpsert) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SessionSkillsUpsert(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSessionSkillsUpsert, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

func (h *AgentConfigHandler) serveSessionSkillsDelete(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.AgentConfigSessionSkillsDeleteRequest
	if !h.decode(w, body, &req, methods.MethodAgentConfigSessionSkillsDelete) {
		return
	}
	if !h.assertIdentity(w, req.Identity, wireID) {
		return
	}
	req.Identity = wireID
	resp, err := h.service.SessionSkillsDelete(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodAgentConfigSessionSkillsDelete, err)
		return
	}
	writeAgentConfigJSON(w, r, resp, h.logger)
}

// decode decodes the JSON body into req (rejecting unknown fields) and
// writes a CodeInvalidRequest error on failure, returning false.
func (h *AgentConfigHandler) decode(w http.ResponseWriter, body []byte, req any, method methods.Method) bool {
	if err := decodeGovernanceBody(body, req); err != nil {
		writeAgentConfigError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode "+string(method)+" request: "+err.Error())
		return false
	}
	return true
}

// assertIdentity is the defence-in-depth body-identity check. Returns
// false (and writes the error) when the body identity disagrees with the
// verified identity.
func (h *AgentConfigHandler) assertIdentity(w http.ResponseWriter, body, verified prototypes.IdentityScope) bool {
	if perr := assertGovernanceIdentity(body, verified); perr != "" {
		writeAgentConfigError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return false
	}
	return true
}

// writeServiceError maps a service error onto a canonical Protocol Code +
// HTTP status + safe operator-facing message.
func (h *AgentConfigHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyAgentConfigError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "agent-config handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeAgentConfigError(w, code, status, msg)
}

// classifyAgentConfigError maps a service / registry / skills error onto
// the canonical Protocol Code + HTTP status — the single place the
// agent-config wire surface translates a Go error into a Protocol error.
func classifyAgentConfigError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, agentcfgprotocol.ErrIdentityRequired),
		errors.Is(err, agentcfg.ErrIdentityRequired),
		errors.Is(err, sessionoverlay.ErrIdentityRequired),
		errors.Is(err, skills.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, sessionoverlay.ErrInvalidInput):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, agentcfgprotocol.ErrSkillsUnavailable):
		return protoerrors.CodeUnknownMethod, http.StatusNotImplemented,
			m + ": skills control is not wired on this runtime"
	case errors.Is(err, agentcfgprotocol.ErrSessionOverlayUnavailable):
		return protoerrors.CodeUnknownMethod, http.StatusNotImplemented,
			m + ": session safe-subset control is not wired on this runtime"
	case errors.Is(err, agentcfgprotocol.ErrConnectionAttachUnavailable):
		return protoerrors.CodeUnknownMethod, http.StatusNotImplemented,
			m + ": mcp connection attach is not wired on this runtime"
	case errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed):
		// The most privileged action's fail-closed RCE gate — a stdio add of
		// an un-allowlisted command is an authorization failure (CodeScopeMismatch).
		return protoerrors.CodeScopeMismatch, http.StatusForbidden,
			m + ": " + err.Error()
	case errors.Is(err, agentcfgprotocol.ErrInvalidConnection):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, agentcfgprotocol.ErrCoordinatorUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": " + err.Error()
	case errors.Is(err, agentcfg.ErrRevisionNotFound), errors.Is(err, skills.ErrSkillNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			m + ": " + err.Error()
	case errors.Is(err, skills.ErrPackOverwriteRefused):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, skills.ErrInvalidSkill), errors.Is(err, agentcfg.ErrInvalidPayload):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, agentcfg.ErrClosed), errors.Is(err, skills.ErrStoreClosed),
		errors.Is(err, sessionoverlay.ErrClosed):
		return protoerrors.CodeRuntimeError, http.StatusServiceUnavailable,
			m + ": agent-config subsystem closed"
	case errors.Is(err, agentcfg.ErrStateUnavailable), errors.Is(err, sessionoverlay.ErrStateUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": state store unavailable"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeAgentConfigJSON encodes a successful response.
func writeAgentConfigJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "agent-config handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeAgentConfigError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status.
func writeAgentConfigError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
