package control

import (
	"net/http"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// HTTPStatus maps a canonical Protocol error Code onto a stable HTTP
// status. The mapping is part of the wire contract: a Protocol client
// branches on the JSON body's `code`, but an intermediary (a proxy, a
// load balancer, a browser's network panel) branches on the HTTP status,
// so the two must agree and stay stable across a Runtime refactor.
//
// The Code set is single-sourced in internal/protocol/errors (CLAUDE.md
// §8); this function is the one place the Protocol wire transport binds
// each Code to a status. A Code with no explicit entry falls through to
// 500 — fail loud rather than silently returning a misleading 200.
//
// Exported since then: `cmd/harbor-gen-protocol-docs`
// renders the published error reference from the SAME binding the wire
// transport serves, so the docs cannot drift from this function.
func HTTPStatus(code protoerrors.Code) int {
	switch code {
	case protoerrors.CodeInvalidRequest:
		// Structurally malformed request — the client must fix the
		// request shape.
		return http.StatusBadRequest // 400
	case protoerrors.CodeIdentityRequired:
		// No / incomplete identity scope. RFC §5.5: the Protocol rejects
		// any request without an identity scope. 401 — the request is
		// unauthenticated at the Protocol edge (makes this a
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
	case protoerrors.CodeRestartUnavailable:
		return http.StatusConflict // 409
	case protoerrors.CodeRuntimeError:
		// An unclassified runtime-side failure — the catch-all.
		return http.StatusInternalServerError // 500
	case protoerrors.CodeAuthRejected:
		// the request carried a JWT bearer that failed
		// cryptographic / structural verification. Distinct from
		// CodeIdentityRequired (which signals no identity at all): the
		// client supplied a token but it did not verify. 401 — the
		// request is unauthenticated at the Protocol edge.
		return http.StatusUnauthorized // 401
	case protoerrors.CodeIdentityScopeRequired:
		// the request is
		// authenticated AND identified, but the caller's scope set is
		// insufficient for the requested fan-in. Returned when an
		// `events.subscribe` request asks for `?admin=1`, or an
		// `events.aggregate` filter names a tenant other than the
		// caller's, without the `auth.ScopeAdmin` or
		// `auth.ScopeConsoleFleet` claim. Distinct from
		// CodeIdentityRequired (no identity — 401), CodeAuthRejected
		// (token invalid — 401), and CodeScopeMismatch (reserved for
		// the steering-control scope path per RFC §6.3). 403 —
		// authenticated but not authorised. Maps from
		// events.ErrIdentityScopeRequired and
		// events.ErrAdminScopeRequired at the wire edge.
		return http.StatusForbidden // 403
	case protoerrors.CodePresignUnsupported:
		// an `artifacts.get_ref` request
		// reached an ArtifactStore driver without the `Presigner`
		// capability. The request is well-formed and the surface is
		// real, but the configured driver cannot satisfy it. 501 Not
		// Implemented — distinct from a 404 (the route exists) and a 400
		// (the request was valid).
		return http.StatusNotImplemented // 501
	case protoerrors.CodeRequestTooLarge:
		// an `artifacts.put` body exceeded
		// the configured MaxRequestBytes bound. 413 Payload Too Large.
		return http.StatusRequestEntityTooLarge // 413
	case protoerrors.CodeSessionRunning:
		// a `sessions.delete` erasure was refused because the target
		// session has a RUNNING task (mirroring the GC never-reap-running
		// invariant). The request is well-formed and authorised, but the
		// session's current state forbids the operation. 409 Conflict —
		// distinct from a 404 (the session does not exist) and a 400 (a
		// malformed body).
		return http.StatusConflict // 409
	case protoerrors.CodeSessionErased:
		// a `start` named a session id permanently deleted by
		// `sessions.delete` (right-to-erasure). The request is well-formed
		// and authorised, but the session is terminal — its data is gone and
		// it can never be reopened. 409 Conflict — the same state-forbids
		// posture as CodeSessionRunning; distinct from a 404 (a closed but
		// reopenable session exists and resumes) and a 400 (a malformed
		// body).
		return http.StatusConflict // 409
	case protoerrors.CodeRevisionConflict:
		// an agent-config write declared an `expected_content_hash` and the
		// agent's active revision no longer carries it (or there is none).
		// The request was well-formed and authorised; the target's current
		// state forbids the operation, and nothing was persisted. 409
		// Conflict — the same state-forbids posture as CodeSessionRunning;
		// distinct from a 400 (the body was valid) and a 500 (the server
		// did not fault). The client re-reads `agent_config.get` and
		// retries.
		return http.StatusConflict // 409
	case protoerrors.CodeSessionSkillCutoverPending:
		// A session-personal mutation reached a tenant still in the declared
		// dual-read migration mode. The request is valid, but state_only has
		// not yet been durably authorized.
		return http.StatusConflict // 409
	case protoerrors.CodeSessionSkillReadUnstable:
		// The bounded session-skill read observed lifecycle or erasure fences
		// changing on every attempt. No partial result is returned; retry
		// after the concurrent transition settles.
		return http.StatusConflict // 409
	case protoerrors.CodeAgentRetired, protoerrors.CodeAgentRetirementConflict:
		return http.StatusConflict // 409
	case protoerrors.CodeRenderAdmissionMissing:
		// MCP Apps: an app-tool-call request referenced a render
		// admission but supplied none. The request is malformed — the
		// App must re-read the resource with the opt-in flag.
		return http.StatusBadRequest // 400
	case protoerrors.CodeRenderAdmissionUnavailable:
		// MCP Apps: the render-admission token could not be opened
		// (bad base64url / oversize / envelope tamper / wrong
		// key-replica). The presented authority is unusable — 400, never
		// a misleading 404 (the App exists; its admission is bad).
		return http.StatusBadRequest // 400
	case protoerrors.CodeRenderAdmissionInvalid:
		// MCP Apps: the render-admission token opened but its sealed
		// claims are structurally invalid.
		return http.StatusBadRequest // 400
	case protoerrors.CodeRenderAdmissionExpired:
		// MCP Apps: the render-admission token is well-formed but past
		// its expiry. The App re-reads the resource for a fresh one —
		// typed, never collapsed into a generic not-found.
		return http.StatusBadRequest // 400
	case protoerrors.CodeRenderAdmissionMismatch:
		// MCP Apps: the render-admission token does not match the
		// requested render tuple (identity / agent / server / resource /
		// generation).
		return http.StatusBadRequest // 400
	case protoerrors.CodeRenderAuthorityAmbiguous:
		// MCP Apps: the request supplied BOTH the legacy binding and the
		// fresh render admission. Refused as ambiguous — 400.
		return http.StatusBadRequest // 400
	case protoerrors.CodeSkillImportProposalInvalid:
		// agent-config import: the echoed proposal token is unknown /
		// consumed / foreign / stale. The caller re-runs
		// import_validate. 400 — a bad token is a bad request input,
		// never an ambiguous not-found.
		return http.StatusBadRequest // 400
	case protoerrors.CodeSkillImportProposalExpired:
		// agent-config import: the proposal's review window elapsed.
		// 400 — the caller re-validates for a fresh proposal.
		return http.StatusBadRequest // 400
	case protoerrors.CodeSkillImportPackageInvalid:
		// agent-config import: the artifact is not a valid complete
		// skill package. 400 — the request was well formed, the package
		// was not.
		return http.StatusBadRequest // 400
	case protoerrors.CodeSkillImportReplaceRequired:
		// agent-config import: a different package already wins the
		// target key and no replacement consent was given. 409 — the
		// target's current state forbids the write.
		return http.StatusConflict // 409
	case protoerrors.CodeQueryBudgetExceeded:
		// observability: the query exceeds a result budget (too many
		// buckets, page limit too high). 400 — the caller narrows the
		// window or coarsens the bucket; never a truncated response.
		return http.StatusBadRequest // 400
	case protoerrors.CodeInvalidCursor:
		// observability: the page cursor is malformed or foreign to the
		// query shape. 400 — the caller restarts from the first page.
		return http.StatusBadRequest // 400
	default:
		// An unmapped Code is a Protocol-surface bug, not a client
		// error. Surface it loud as a 500 rather than masking it
		// (CLAUDE.md §5: fail loudly, no silent degradation).
		return http.StatusInternalServerError // 500
	}
}
