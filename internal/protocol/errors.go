package protocol

import (
	stderrors "errors"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
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

// mapTopologyError translates an engine Topology() error into a stable
// *protoerrors.Error (Phase 74 / D-114). The engine's identity-rejection
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
