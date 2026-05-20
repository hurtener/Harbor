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

// artifactsMaxBodyBytes bounds an artifacts-method request body at the
// transport edge. artifacts.put carries an upload payload, so this cap
// is generous (8 MiB — covers a moderate-size upload plus the JSON
// envelope). The ArtifactsSurface separately enforces the configured
// `MaxRequestBytes` against the decoded payload bytes, so an oversize
// upload is rejected with the canonical CodeRequestTooLarge / HTTP 413
// rather than a transport-level read error; this cap is the outer
// fail-closed guard against an unbounded stream.
const artifactsMaxBodyBytes = 8 << 20

// serveArtifacts is the Phase 73l (D-120) artifacts-method REST adapter.
// It decodes the body into the wire request type the method expects,
// backfills the body identity from the auth-verified identity in ctx
// when the body left it empty (same posture as servePosture's
// backfillPostureIdentity), and dispatches through the configured
// ArtifactsSurface.
//
// The ArtifactsSurface itself enforces:
//
//   - Identity-mandatory (CodeIdentityRequired / 401) — an incomplete
//     triple fails closed at the surface edge.
//   - Cross-tenant gating (CodeScopeMismatch / 403) — a request whose
//     scope Tenant differs from the ctx-verified tenant requires the
//     admin scope claim per D-079.
//   - Body bounds (CodeRequestTooLarge / 413) — an artifacts.put body
//     above the configured MaxRequestBytes.
//   - Presigner capability (CodePresignUnsupported / 501) — an
//     artifacts.get_ref against a non-S3 driver.
func (h *Handler) serveArtifacts(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, artifactsMaxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, scopePtr, perr := decodeArtifactsRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// Phase 61 defence-in-depth: when auth.Middleware ran, backfill an
	// empty body identity from the verified JWT, and reject a body whose
	// user/session disagree with it. The Tenant deliberately may differ
	// (a cross-tenant artifacts.list is a legitimate admin request the
	// ArtifactsSurface gates on the admin scope).
	if perr := backfillArtifactsIdentity(r, scopePtr); perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.artifactsSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeArtifactsError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeArtifactsRequest decodes an artifacts request body into the wire
// type the method expects and returns a pointer to its embedded
// ArtifactScope so the identity-backfill helper can mutate it in place.
func decodeArtifactsRequest(method methods.Method, body []byte) (any, *types.ArtifactScope, *protoerrors.Error) {
	switch method {
	case methods.MethodArtifactsList:
		req := &types.ArtifactsListRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return nil, nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: request body is not a valid ArtifactsListRequest", string(method))
			}
		}
		return req, &req.Scope, nil
	case methods.MethodArtifactsPut:
		req := &types.ArtifactsPutRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return nil, nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: request body is not a valid ArtifactsPutRequest", string(method))
			}
		}
		return req, &req.Scope, nil
	case methods.MethodArtifactsGetRef:
		req := &types.ArtifactsGetRefRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return nil, nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: request body is not a valid ArtifactsGetRefRequest", string(method))
			}
		}
		return req, &req.Scope, nil
	default:
		return nil, nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol artifacts method", string(method))
	}
}

// backfillArtifactsIdentity threads the Phase 61 verified identity into
// the artifacts request body's ArtifactScope. When ctx carries a
// verified identity:
//
//   - An empty body scope is backfilled from the JWT triple.
//   - A populated body scope must match the JWT's user/session; the
//     Tenant may differ (a cross-tenant artifacts.list — the
//     ArtifactsSurface gates that on the admin scope).
//
// When ctx carries no verified identity (no middleware ran), the body
// scope is authoritative and this is a no-op.
func backfillArtifactsIdentity(r *http.Request, scope *types.ArtifactScope) *protoerrors.Error {
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
	// User / Session must match the verified identity — artifacts are
	// not an impersonation surface. The Tenant may differ.
	if scope.User != "" && scope.User != authed.UserID {
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"artifacts request body identity (user) does not match the verified JWT identity")
	}
	if scope.Session != "" && scope.Session != authed.SessionID {
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"artifacts request body identity (session) does not match the verified JWT identity")
	}
	return nil
}

// writeArtifactsError maps an ArtifactsSurface error onto the wire. The
// surface is contracted to return *protoerrors.Error; a non-Protocol
// error is wrapped as CodeRuntimeError (CLAUDE.md §5 + §7: no raw
// runtime detail on the wire).
func (h *Handler) writeArtifactsError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: artifacts Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: artifacts dispatch failed", string(method)))
}
