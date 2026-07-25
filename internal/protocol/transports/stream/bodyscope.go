package stream

import (
	"net/http"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// reconcileBodyScope routes a decoded request body's identity scope
// through the shared body-identity gate under the surface's registered
// policy.
//
// The streaming transport's page handlers each own their own error
// writer, so this returns the Protocol error rather than writing it: the
// caller renders it with its page's writer. The reconciled ctx is
// discarded for the surfaces whose policy pins every component — there
// is no crossing to carry — and returned by reconcileBodyScopeCtx for
// the ones that can cross.
func reconcileBodyScope(r *http.Request, scope *prototypes.IdentityScope, surface bodyscope.Surface) *protoerrors.Error {
	_, perr := bodyscope.Reconcile(r.Context(), bodyscope.ForIdentityScope(scope), surface, nil)
	return perr
}

// bodyScopeCodes is the closed set of Protocol codes the shared
// body-identity gate can return. It is the universe bodyScopeStatus maps
// and the lockstep test in bodyscope_status_test.go walks.
func bodyScopeCodes() []protoerrors.Code {
	return []protoerrors.Code{
		protoerrors.CodeInvalidRequest,
		protoerrors.CodeIdentityRequired,
		protoerrors.CodeScopeMismatch,
		protoerrors.CodeNotFound,
		protoerrors.CodeRuntimeError,
	}
}

// bodyScopeStatus maps the Protocol codes the shared body-identity gate
// returns onto the HTTP status the streaming transport writes.
//
// The binding is the transport-wide one; a lockstep test pins this
// function against the control transport's HTTPStatus for every code in
// bodyScopeCodes, so the two transports cannot answer the same refusal
// with different statuses.
func bodyScopeStatus(code protoerrors.Code) int {
	switch code {
	case protoerrors.CodeInvalidRequest:
		return http.StatusBadRequest
	case protoerrors.CodeIdentityRequired:
		return http.StatusUnauthorized
	case protoerrors.CodeScopeMismatch:
		return http.StatusForbidden
	case protoerrors.CodeNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
