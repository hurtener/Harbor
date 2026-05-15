package control

import (
	"net/http"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// httpStatus maps a canonical Protocol error Code onto a stable HTTP
// status. The mapping is part of the wire contract: a Protocol client
// branches on the JSON body's `code`, but an intermediary (a proxy, a
// load balancer, a browser's network panel) branches on the HTTP status,
// so the two must agree and stay stable across a Runtime refactor.
//
// The Code set is single-sourced in internal/protocol/errors (CLAUDE.md
// §8); this function is the one place the Protocol wire transport binds
// each Code to a status. A Code with no explicit entry falls through to
// 500 — fail loud rather than silently returning a misleading 200.
func httpStatus(code protoerrors.Code) int {
	switch code {
	case protoerrors.CodeInvalidRequest:
		// Structurally malformed request — the client must fix the
		// request shape.
		return http.StatusBadRequest // 400
	case protoerrors.CodeIdentityRequired:
		// No / incomplete identity scope. RFC §5.5: the Protocol rejects
		// any request without an identity scope. 401 — the request is
		// unauthenticated at the Protocol edge (Phase 61 makes this a
		// real JWT check; the status is stable across that change).
		return http.StatusUnauthorized // 401
	case protoerrors.CodeScopeMismatch:
		// The caller is identified but the steering scope claim is below
		// the control method's RFC §6.3 minimum — authenticated but not
		// authorized.
		return http.StatusForbidden // 403
	case protoerrors.CodePayloadInvalid:
		// The request was well-formed JSON but the control payload
		// violated an RFC §6.3 bound — semantically unprocessable.
		return http.StatusUnprocessableEntity // 422
	case protoerrors.CodeUnknownMethod:
		// The method name is not one of the ten canonical methods — the
		// route does not exist.
		return http.StatusNotFound // 404
	case protoerrors.CodeNotFound:
		// The request's target (a run with no live inbox, a missing
		// parent task) does not exist.
		return http.StatusNotFound // 404
	case protoerrors.CodeRuntimeError:
		// An unclassified runtime-side failure — the catch-all.
		return http.StatusInternalServerError // 500
	case protoerrors.CodeAuthRejected:
		// Phase 61 — the request carried a JWT bearer that failed
		// cryptographic / structural verification. Distinct from
		// CodeIdentityRequired (which signals no identity at all): the
		// client supplied a token but it did not verify. 401 — the
		// request is unauthenticated at the Protocol edge.
		return http.StatusUnauthorized // 401
	default:
		// An unmapped Code is a Protocol-surface bug, not a client
		// error. Surface it loud as a 500 rather than masking it
		// (CLAUDE.md §5: fail loudly, no silent degradation).
		return http.StatusInternalServerError // 500
	}
}
