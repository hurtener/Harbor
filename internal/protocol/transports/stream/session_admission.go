package stream

import (
	"net/http"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func (h *SessionsHandler) serveSetAdmission(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.SessionsSetAdmissionRequest
	if err := decodeSessionsBody(body, &req); err != nil {
		writeSessionsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest, "invalid session admission request")
		return
	}
	if perr := reconcileBodyScope(r, &req.Identity, bodyscope.SurfaceSessions); perr != nil {
		writeSessionsError(w, perr.Code, bodyScopeStatus(perr.Code), perr.Message)
		return
	}
	req.Identity = wireID
	resp, err := h.service.SetAdmission(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodSessionsSetAdmission, err)
		return
	}
	writeSessionsJSON(w, r, resp, h.logger)
}
