package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// serveApps is the MCP Apps host REST adapter. It decodes the body into
// the per-method MCP Apps request wire type, reconciles the body
// identity against the request's verified identity through the shared
// body-identity gate, and dispatches through the configured AppsSurface
// under the reconciled ctx.
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

	ctx, perr := reconcileAppsIdentity(r, req)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.appsSurface.Dispatch(ctx, method, req)
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

// reconcileAppsIdentity routes the MCP Apps request body through the
// shared body-identity gate under the MCP Apps surface's registered
// policy. The policy pins all three components: the surface's verb gate
// is identity-scoped and mints no claim that widens the tenant, so the
// body triple is the caller's own or the request is refused.
//
// It returns the ctx to dispatch under.
func reconcileAppsIdentity(r *http.Request, req any) (context.Context, *protoerrors.Error) {
	scope := appsIdentityScope(req)
	if scope == nil {
		return r.Context(), protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"MCP Apps request type is not recognised")
	}
	return bodyscope.Reconcile(r.Context(), bodyscope.ForIdentityScope(scope), bodyscope.SurfaceApps, nil)
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
