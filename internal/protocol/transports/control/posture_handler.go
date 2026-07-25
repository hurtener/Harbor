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

// servePosture is the runtime-posture REST adapter.
// It decodes the body into a `*types.RuntimeInfoRequest`, reconciles the
// body identity against the request's verified identity through the
// shared body-identity gate, and dispatches through the configured
// PostureSurface under the reconciled ctx.
//
// The PostureSurface itself enforces:
//
//   - Identity-mandatory (CodeIdentityRequired / 401) — an incomplete
//     triple fails closed at the surface edge.
//   - Cross-tenant gating (CodeScopeMismatch / 403) — a request whose
//     body Tenant differs from the ctx-verified tenant requires the
//     admin (or console:fleet) scope claim per the closed admin-scope set.
func (h *Handler) servePosture(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req := &types.RuntimeInfoRequest{}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, req); jerr != nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid RuntimeInfoRequest", string(method)))
			return
		}
	}

	ctx, perr := h.reconcilePostureIdentity(r, req)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.postureSurface.Dispatch(ctx, method, req)
	if derr != nil {
		h.writePostureError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// reconcilePostureIdentity routes the posture request body through the
// shared body-identity gate under the posture surface's registered
// policy. The policy pins the user and the session and declares the
// tenant admin-scoped: a fleet operator reads another tenant's posture
// under the admin claim, and the gate records the crossing before
// granting it.
//
// It returns the ctx to dispatch under, which carries the audited
// crossing when one was granted.
func (h *Handler) reconcilePostureIdentity(r *http.Request, req *types.RuntimeInfoRequest) (context.Context, *protoerrors.Error) {
	return bodyscope.Reconcile(r.Context(), bodyscope.ForIdentityScope(&req.Identity),
		bodyscope.SurfacePosture, h.bodyScopeAuditor)
}

// writePostureError maps a PostureSurface error onto the wire. The
// PostureSurface is contracted to return *protoerrors.Error; a
// non-Protocol error is wrapped as CodeRuntimeError (CLAUDE.md §5 + §7:
// no raw runtime detail on the wire).
func (h *Handler) writePostureError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: posture Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: posture dispatch failed", string(method)))
}
