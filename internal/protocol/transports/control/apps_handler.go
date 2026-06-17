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

// serveApps is the MCP Apps host REST adapter. It decodes the body into
// the per-method MCP Apps request wire type, backfills the body identity
// from the auth-verified identity in ctx when the body left it empty
// (same posture as serveMCP), and dispatches through the configured
// AppsSurface.
//
// The AppsSurface itself enforces identity-mandatory; the
// `mcp.apps.call_tool` proxy additionally re-enters the approval / OAuth
// tool-safety gates inside the invocation path. The transport does not
// re-implement those gates (CLAUDE.md §13 forbids a second validator).
func (h *Handler) serveApps(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, perr := decodeAppsRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	if perr := backfillAppsIdentity(r, req); perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.appsSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeAppsError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeAppsRequest decodes a request body into the wire request type the
// MCP Apps method expects. A decode failure surfaces as
// CodeInvalidRequest — never a silent zero-value request.
func decodeAppsRequest(method methods.Method, body []byte) (any, *protoerrors.Error) {
	var target any
	switch method {
	case methods.MethodMCPReadResource:
		target = &types.ReadMCPResourceRequest{}
	case methods.MethodMCPAppsCallTool:
		target = &types.MCPAppCallToolRequest{}
	case methods.MethodMCPAppsToolContext:
		target = &types.ToolContextRequest{}
	default:
		// Unreachable — serveApps is gated on IsMCPAppsMethod.
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol MCP Apps method", string(method))
	}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, target); jerr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid request", string(method))
		}
	}
	return target, nil
}

// appsIdentityScope returns a pointer to the IdentityScope field of any
// MCP Apps request type — so the backfill / consistency check can read
// and write it uniformly.
func appsIdentityScope(req any) *types.IdentityScope {
	switch v := req.(type) {
	case *types.ReadMCPResourceRequest:
		return &v.Identity
	case *types.MCPAppCallToolRequest:
		return &v.Identity
	case *types.ToolContextRequest:
		return &v.Identity
	default:
		return nil
	}
}

// backfillAppsIdentity threads the verified identity into the MCP Apps
// request body — same posture as backfillMCPIdentity. A body claiming a
// different (user, session) than the verified JWT is rejected.
func backfillAppsIdentity(r *http.Request, req any) *protoerrors.Error {
	scope := appsIdentityScope(req)
	if scope == nil {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"MCP Apps request type is not recognised")
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
			"MCP Apps request body identity (user/session) does not match the verified JWT identity")
	}
	return nil
}

// writeAppsError maps an AppsSurface error onto the wire. The AppsSurface
// is contracted to return *protoerrors.Error; a non-Protocol error is
// wrapped as CodeRuntimeError (CLAUDE.md §5 + §7).
func (h *Handler) writeAppsError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: MCP Apps Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: MCP Apps dispatch failed", string(method)))
}
