package control

import (
	"io"
	"net/http"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// serveSkillPublications is the HA-68 REST adapter. It decodes one of the
// publication request envelopes and delegates all identity, admin-scope, and
// signed effective-Agent reach decisions to the transport-agnostic surface.
// In particular, it deliberately does not reconcile the request body's
// IdentityScope into context: body fields are caller input and cannot grant
// authority.
func (h *Handler) serveSkillPublications(w http.ResponseWriter, r *http.Request, method methods.Method) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, perr := decodeSkillPublicationsRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	resp, derr := h.skillPublicationsSurface.Dispatch(r.Context(), method, req)
	if derr != nil {
		h.writeDispatchError(w, r, method, derr)
		return
	}
	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeSkillPublicationsRequest decodes a strict HA-68 request envelope.
// Empty bodies are accepted for the identity-only list/available/reference
// operations, matching the other read-only Protocol handlers; the surface
// still rejects requests without a verified identity.
func decodeSkillPublicationsRequest(method methods.Method, body []byte) (any, *protoerrors.Error) {
	var target any
	switch method {
	case methods.MethodSkillsPublicationsPublish:
		target = &types.SkillPublicationPublishRequest{}
	case methods.MethodSkillsPublicationsList:
		target = &types.SkillPublicationListRequest{}
	case methods.MethodSkillsPublicationsGet:
		target = &types.SkillPublicationGetRequest{}
	case methods.MethodSkillsPublicationsSuccessor:
		target = &types.SkillPublicationSuccessorRequest{}
	case methods.MethodSkillsPublicationsRetire:
		target = &types.SkillPublicationRetireRequest{}
	case methods.MethodSkillsPublicationsAvailable:
		target = &types.SkillPublicationAvailableRequest{}
	case methods.MethodSkillsPublicationsInstall:
		target = &types.SkillPublicationInstallRequest{}
	case methods.MethodSkillsPublicationsUpdate:
		target = &types.SkillPublicationUpdateRequest{}
	case methods.MethodSkillsPublicationsRemove:
		target = &types.SkillPublicationRemoveRequest{}
	case methods.MethodSkillsPublicationsReferencesList:
		target = &types.SkillPublicationReferencesListRequest{}
	default:
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical skill-publication method", string(method))
	}
	if len(body) > 0 {
		if err := decodeStrict(body, target); err != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid skill-publication request: %s", string(method), decodeDetail(err))
		}
	}
	return target, nil
}
