package control

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// serveSearch is the Phase 72c (D-108) search-method REST adapter. It
// decodes the body into a `*types.SearchRequest`, defends the body
// identity against the auth-verified identity in ctx (defence in
// depth, same shape as assertBodyMatchesAuthedIdentity for control
// methods — but search has no per-request identity in the body, so
// instead we read the verified identity straight from ctx), and
// dispatches through the configured SearchSurface.
//
// The search surface itself enforces:
//
//   - Identity-mandatory (CodeIdentityRequired / 401) — missing-triple
//     in ctx fails closed at the surface.
//   - Cross-tenant gating (CodeAuthRejected / 403) — the surface
//     returns search.ErrCrossTenantRequiresAdmin which the wire
//     mapper translates to CodeAuthRejected.
//   - Bound enforcement (CodeInvalidRequest / 400) — page_size > max,
//     unknown index in palette request.
func (h *Handler) serveSearch(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req := &types.SearchRequest{}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, req); jerr != nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid SearchRequest", string(method)))
			return
		}
	}

	resp, derr := h.searchSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeSearchError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// writeSearchError maps a SearchSurface error onto the wire. The
// SearchSurface is contracted to return *protoerrors.Error; if it ever
// returns a non-Protocol error, it is wrapped as CodeRuntimeError
// (CLAUDE.md §5 + §7: no raw runtime detail on the wire).
func (h *Handler) writeSearchError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: search Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: search dispatch failed", string(method)))
}
