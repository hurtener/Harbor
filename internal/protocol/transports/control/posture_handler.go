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

// servePosture is the Phase 72f (D-111) runtime-posture REST adapter.
// It decodes the body into a `*types.RuntimeInfoRequest`, backfills the
// body identity from the auth-verified identity in ctx when the body
// left it empty (same posture as the control transport's
// `assertBodyMatchesAuthedIdentity`), and dispatches through the
// configured PostureSurface.
//
// The PostureSurface itself enforces:
//
//   - Identity-mandatory (CodeIdentityRequired / 401) — an incomplete
//     triple fails closed at the surface edge.
//   - Cross-tenant gating (CodeScopeMismatch / 403) — a request whose
//     body Tenant differs from the ctx-verified tenant requires the
//     admin (or console:fleet) scope claim per D-079.
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

	// Phase 61 defence-in-depth: when auth.Middleware ran, r.Context()
	// carries the verified identity. The body's IdentityScope MUST be
	// consistent with it — backfill an empty body identity from the
	// JWT, and reject a body claiming a different (tenant, user,
	// session) than the verified one. When no middleware ran (Phase 60
	// trust-based posture) the check is a no-op and the body identity
	// is authoritative — same posture as the control transport.
	if perr := backfillPostureIdentity(r, req); perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.postureSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writePostureError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// backfillPostureIdentity threads the Phase 61 verified identity into
// the posture request body. When ctx carries a verified identity:
//
//   - An empty body identity is backfilled from the JWT (the JWT is the
//     source of truth — the same backfill the control transport does).
//   - A populated body identity must match the JWT's (tenant, user,
//     session) on every non-empty component, EXCEPT the Tenant — a
//     cross-tenant posture read with a differing Tenant is a legitimate
//     admin request the PostureSurface gates on the admin scope. User
//     and Session mismatches are rejected here closed.
//
// When ctx carries no verified identity (no middleware ran), the body
// identity is authoritative and this is a no-op.
func backfillPostureIdentity(r *http.Request, req *types.RuntimeInfoRequest) *protoerrors.Error {
	authed, ok := identity.From(r.Context())
	if !ok {
		return nil
	}
	scope := req.Identity
	if scope.Tenant == "" && scope.User == "" && scope.Session == "" {
		scope.Tenant = authed.TenantID
		scope.User = authed.UserID
		scope.Session = authed.SessionID
		req.Identity = scope
		return nil
	}
	// The User / Session must match the verified identity — they are
	// not cross-tenant-elevatable. The Tenant deliberately may differ
	// (a cross-tenant posture read); the PostureSurface gates that on
	// the admin scope.
	if scope.User != authed.UserID || scope.Session != authed.SessionID {
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"posture request body identity (user/session) does not match the verified JWT identity")
	}
	return nil
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
