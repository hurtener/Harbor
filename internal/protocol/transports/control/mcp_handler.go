package control

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// serveMCP is the Phase 73k (D-119) MCP-Connections REST adapter. It
// decodes the body into the per-method `mcp.servers.*` request wire
// type, backfills the body identity from the auth-verified identity in
// ctx when the body left it empty (same posture as servePosture), and
// dispatches through the configured MCPSurface.
//
// The MCPSurface itself enforces identity-mandatory + the admin-scope
// gate (D-079) on the three admin verbs and the two control-plane verbs
// (refresh_discovery / probe). The transport does not re-implement those
// gates (CLAUDE.md §13 forbids a second validator).
func (h *Handler) serveMCP(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, perr := decodeMCPRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// Phase 61 defence-in-depth: when auth.Middleware ran, r.Context()
	// carries the verified identity; backfill an empty body identity
	// from it, and reject a body claiming a different (user, session).
	// The Tenant deliberately may differ — a cross-tenant MCP read is an
	// admin operation the MCPSurface gates on the admin scope.
	if perr := backfillMCPIdentity(r, req); perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.mcpSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeMCPError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeMCPRequest decodes a request body into the wire request type the
// `mcp.servers.*` method expects. A decode failure surfaces as
// CodeInvalidRequest — never a silent zero-value request.
func decodeMCPRequest(method methods.Method, body []byte) (any, *protoerrors.Error) {
	var target any
	switch method {
	case methods.MethodMCPServersList:
		target = &types.MCPServersListRequest{}
	case methods.MethodMCPServersGet:
		target = &types.MCPServerGetRequest{}
	case methods.MethodMCPServersResources:
		target = &types.MCPServerResourcesRequest{}
	case methods.MethodMCPServersPrompts:
		target = &types.MCPServerPromptsRequest{}
	case methods.MethodMCPServersRefreshDiscovery:
		target = &types.MCPServerRefreshDiscoveryRequest{}
	case methods.MethodMCPServersProbe:
		target = &types.MCPServerProbeRequest{}
	case methods.MethodMCPServersHealth:
		target = &types.MCPServerHealthRequest{}
	case methods.MethodMCPServersBindingsList:
		target = &types.MCPServerBindingsListRequest{}
	case methods.MethodMCPServersPolicy:
		target = &types.MCPServerPolicyRequest{}
	case methods.MethodMCPServersRefreshBinding:
		target = &types.MCPServerRefreshBindingRequest{}
	case methods.MethodMCPServersRevokeBinding:
		target = &types.MCPServerRevokeBindingRequest{}
	case methods.MethodMCPServersSetRawHTMLTrust:
		target = &types.MCPServerSetRawHTMLTrustRequest{}
	default:
		// Unreachable — serveMCP is gated on IsMCPServersMethod.
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol MCP method", string(method))
	}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, target); jerr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid request", string(method))
		}
	}
	return target, nil
}

// mcpIdentityScope returns a pointer to the IdentityScope field of any
// MCP request type — so the backfill / consistency check can read and
// write it uniformly.
func mcpIdentityScope(req any) *types.IdentityScope {
	switch v := req.(type) {
	case *types.MCPServersListRequest:
		return &v.Identity
	case *types.MCPServerGetRequest:
		return &v.Identity
	case *types.MCPServerResourcesRequest:
		return &v.Identity
	case *types.MCPServerPromptsRequest:
		return &v.Identity
	case *types.MCPServerRefreshDiscoveryRequest:
		return &v.Identity
	case *types.MCPServerProbeRequest:
		return &v.Identity
	case *types.MCPServerHealthRequest:
		return &v.Identity
	case *types.MCPServerBindingsListRequest:
		return &v.Identity
	case *types.MCPServerPolicyRequest:
		return &v.Identity
	case *types.MCPServerRefreshBindingRequest:
		return &v.Identity
	case *types.MCPServerRevokeBindingRequest:
		return &v.Identity
	case *types.MCPServerSetRawHTMLTrustRequest:
		return &v.Identity
	default:
		return nil
	}
}

// backfillMCPIdentity threads the Phase 61 verified identity into the
// MCP request body — same posture as backfillPostureIdentity.
func backfillMCPIdentity(r *http.Request, req any) *protoerrors.Error {
	scope := mcpIdentityScope(req)
	if scope == nil {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"MCP request type is not recognised")
	}
	authed, ok := identity.From(r.Context())
	if !ok {
		return nil
	}
	if scope.Tenant == "" && scope.User == "" && scope.Session == "" {
		scope.Tenant = authed.TenantID
		scope.User = authed.UserID
		scope.Session = authed.SessionID
		return nil
	}
	if scope.User != authed.UserID || scope.Session != authed.SessionID {
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"MCP request body identity (user/session) does not match the verified JWT identity")
	}
	return nil
}

// writeMCPError maps an MCPSurface error onto the wire. The MCPSurface
// is contracted to return *protoerrors.Error; a non-Protocol error is
// wrapped as CodeRuntimeError (CLAUDE.md §5 + §7).
func (h *Handler) writeMCPError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: MCP Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: MCP dispatch failed", string(method)))
}
