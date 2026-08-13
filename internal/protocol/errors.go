package protocol

import (
	stderrors "errors"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// mapSteeringError translates a steering-subsystem error (from
// Inbox.Enqueue / Registry.Lookup) into a stable *protoerrors.Error. The
// steering sentinels are the contract; this is the one place the
// Protocol surface maps them onto Protocol error codes (CLAUDE.md §8 —
// the codes are single-sourced; the *mapping* is the surface's job).
//
// The Message is built from the stable steering sentinel's text + the
// method context — never from raw control-payload data (CLAUDE.md §7
// rule 7: no caller payload in error messages).
func mapSteeringError(method string, err error) *protoerrors.Error {
	switch {
	case err == nil:
		return nil

	case stderrors.Is(err, steering.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete", method)

	case stderrors.Is(err, steering.ErrScopeMismatch),
		stderrors.Is(err, steering.ErrInvalidScope):
		return protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: caller scope insufficient", method)

	case stderrors.Is(err, steering.ErrPayloadInvalid),
		stderrors.Is(err, steering.ErrUnsupportedPayloadValue):
		return protoerrors.Newf(protoerrors.CodePayloadInvalid,
			"method %q: control payload failed validation", method)

	case stderrors.Is(err, steering.ErrUnknownControlType):
		// A control method whose steering.ControlType is not canonical
		// is a Protocol-surface bug, not a client error — the method
		// table is fixed. Surface it loud as a runtime error rather
		// than masking it.
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: mapped to a non-canonical control type", method)

	case stderrors.Is(err, steering.ErrInboxNotFound):
		return protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: no live run for the requested run id", method)
	case stderrors.Is(err, pauseresume.ErrRestartUnavailable):
		return protoerrors.Newf(protoerrors.CodeRestartUnavailable,
			"method %q: exact restart redrive is unavailable", method)

	default:
		// An unclassified steering error — surface it loud as a runtime
		// error (the catch-all). Never swallowed (CLAUDE.md §5).
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: steering enqueue failed", method)
	}
}

// mapTaskError translates a tasks-subsystem error (from
// TaskRegistry.Spawn) into a stable *protoerrors.Error.
func mapTaskError(method string, err error) *protoerrors.Error {
	switch {
	case err == nil:
		return nil

	case stderrors.Is(err, tasks.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete", method)

	case stderrors.Is(err, tasks.ErrNotFound):
		return protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: task target not found", method)

	case stderrors.Is(err, tasks.ErrIdempotencyConflict):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: idempotency key reused with a divergent request", method)

	case stderrors.Is(err, tasks.ErrInvalidRequest):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request failed validation", method)

	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: task spawn failed", method)
	}
}

// Session-ensure sentinels. The SessionEnsurer seam is
// error-only and the protocol package does not import the sessions
// package, so the adapter that wraps a concrete sessions.Registry
// translates the registry's sentinels into THESE before returning them
// to dispatchStart. Keeping the mapping vocabulary here means the
// Protocol surface owns its own error-code contract (CLAUDE.md §8)
// without coupling to the sessions package.
var (
	// ErrSessionReopenAfterErase — `start` named a session id that was
	// permanently deleted by `sessions.delete` (right-to-erasure). An erased
	// session is terminal — its data is gone — so `start` fails loud rather
	// than silently minting a fresh empty session (RFC §6.9 amended / §7). A
	// closed (but not erased) session, by contrast, RE-ACTIVATES on `start`.
	// Maps to the dedicated machine-branchable CodeSessionErased (HTTP 409)
	// so a consumer-chat client can route "this conversation was deleted —
	// start a new one" off the stable Code.
	ErrSessionReopenAfterErase = stderrors.New("protocol: session reopen-after-erase forbidden")
	// ErrSessionIDReuse — `start` named a session id already opened under
	// a different (tenant, user). Cross-principal session-id reuse is
	// rejected. Maps to CodeInvalidRequest.
	ErrSessionIDReuse = stderrors.New("protocol: session id reused across principals")
)

// mapSessionEnsureError translates a SessionEnsurer error into a stable
// *protoerrors.Error. The adapter is responsible for translating the
// concrete registry sentinels into the protocol-side sentinels above;
// an unclassified error surfaces loud as a runtime error (never
// swallowed — CLAUDE.md §5).
func mapSessionEnsureError(method string, err error) *protoerrors.Error {
	switch {
	case err == nil:
		return nil

	case stderrors.Is(err, identity.ErrIdentityIncomplete):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete", method)

	case stderrors.Is(err, ErrSessionReopenAfterErase):
		return protoerrors.Newf(protoerrors.CodeSessionErased,
			"method %q: session was permanently deleted and cannot be reopened — start a new conversation with a fresh session id", method)

	case stderrors.Is(err, ErrSessionIDReuse):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: session id is already in use by a different tenant/user", method)

	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: session create-on-first-use failed", method)
	}
}

// mapTopologyError translates an engine Topology() error into a stable
// *protoerrors.Error. The engine's identity-rejection
// path wraps identity.ErrIdentityIncomplete; anything else is an
// unclassified runtime failure. Never swallowed (CLAUDE.md §5).
func mapTopologyError(method string, err error) *protoerrors.Error {
	switch {
	case err == nil:
		return nil

	case stderrors.Is(err, identity.ErrIdentityIncomplete):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete", method)

	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: topology projection failed", method)
	}
}
