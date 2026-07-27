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

// artifactsMaxBodyBytes bounds an artifacts-method request body at the
// transport edge. artifacts.put carries an upload payload, so this cap
// is generous (8 MiB — covers a moderate-size upload plus the JSON
// envelope). The ArtifactsSurface separately enforces the configured
// `MaxRequestBytes` against the decoded payload bytes, so an oversize
// upload is rejected with the canonical CodeRequestTooLarge / HTTP 413
// rather than a transport-level read error; this cap is the outer
// fail-closed guard against an unbounded stream.
const artifactsMaxBodyBytes = 8 << 20

// serveArtifacts is the artifacts-method REST adapter.
// It decodes the body into the wire request type the method expects,
// reconciles the body scope against the request's verified identity
// through the shared body-identity gate, and dispatches through the
// configured ArtifactsSurface under the reconciled ctx.
//
// The ArtifactsSurface itself enforces:
//
//   - Identity-mandatory (CodeIdentityRequired / 401) — an incomplete
//     triple fails closed at the surface edge.
//   - Cross-tenant gating (CodeScopeMismatch / 403) — a request whose
//     scope Tenant differs from the ctx-verified tenant requires the
//     admin scope claim per the closed admin-scope set.
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

	ctx, perr := h.reconcileArtifactsIdentity(r, method, scopePtr)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.artifactsSurface.Dispatch(ctx, method, req)
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
	case methods.MethodArtifactsGet:
		req := &types.ArtifactsGetRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return nil, nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: request body is not a valid ArtifactsGetRequest", string(method))
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
	case methods.MethodArtifactsDelete:
		req := &types.ArtifactsDeleteRequest{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return nil, nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: request body is not a valid ArtifactsDeleteRequest", string(method))
			}
		}
		return req, &req.Scope, nil
	default:
		return nil, nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol artifacts method", string(method))
	}
}

// reconcileArtifactsIdentity routes the artifacts request body's
// ArtifactScope through the shared body-identity gate under the posture
// the METHOD carries. The artifacts cluster holds four postures across
// five methods, and the registry states each: a listing crosses tenants
// under either admin-tier claim, an upload or a delete takes the
// administrative claim alone, and the two CONTENT reads — the
// driver-independent byte read and the presigned reference — cross for
// nobody, sharing one posture row because they share one reason.
//
// Selecting per method matters beyond correctness: it keeps the gate
// from recording a crossing that the surface one layer down will refuse,
// so an audit trail of granted crossings stays a trail of crossings that
// were actually taken.
//
// It returns the ctx to dispatch under, which carries the audited
// crossing when one was granted.
func (h *Handler) reconcileArtifactsIdentity(r *http.Request, method methods.Method, scope *types.ArtifactScope) (context.Context, *protoerrors.Error) {
	surface := bodyscope.SurfaceArtifacts
	switch method {
	case methods.MethodArtifactsPut:
		surface = bodyscope.SurfaceArtifactsPut
	case methods.MethodArtifactsDelete:
		surface = bodyscope.SurfaceArtifactsDelete
	case methods.MethodArtifactsGet, methods.MethodArtifactsGetRef:
		surface = bodyscope.SurfaceArtifactsRef
	}
	return bodyscope.Reconcile(r.Context(), bodyscope.ForArtifactScope(scope),
		surface, h.bodyScopeAuditor)
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
