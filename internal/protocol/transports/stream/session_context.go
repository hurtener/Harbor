package stream

import (
	"net/http"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func (h *SessionsHandler) serveReconcileContext(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.SessionsReconcileContextRequest
	if err := decodeSessionsBody(body, &req); err != nil {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest, "invalid context reconciliation request")
		return
	}
	if perr := reconcileBodyScope(r, &req.Identity, bodyscope.SurfaceSessions); perr != nil {
		writeSessionsError(w, perr.Code, bodyScopeStatus(perr.Code), perr.Message)
		return
	}
	req.Identity = wireID
	resp, err := h.service.ReconcileContext(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionsReconcileContext, err)
		return
	}
	writeSessionsJSON(w, r, resp, h.logger)
}
