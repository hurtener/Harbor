// Package errors is the single source of truth for Harbor Protocol error
// codes (CLAUDE.md §8: "Error codes live in
// internal/protocol/errors/errors.go. Add new codes there and only
// there."). The Phase 58 lint formalises this — Phase 54 lays the
// foundation so that lint is a no-op formalisation.
//
// # The shape
//
// A Protocol error is a stable, low-cardinality, client-facing Code plus
// a human-readable Message. The Code is what a Protocol client branches
// on — it is part of the versioned Protocol surface, so the set of codes
// is stable across a Runtime refactor (RFC §5.3). The Message is
// advisory: it carries enough context for a human operator but is never
// the thing a client switches on.
//
// `*Error` implements the `error` interface, so a ControlSurface handler
// returns a `*Error` and a transport adapter (Phase 60) type-asserts it
// to map the Code onto an HTTP status. Until the wire transport lands,
// the in-process caller reaches the Code via errors.As.
//
// # Why these codes
//
// The Phase 54 task-control surface needs exactly the codes below: a
// malformed request, a missing identity scope (RFC §5.5: "the Protocol
// rejects any request without an identity scope"), a steering scope
// mismatch (RFC §6.3 per-event scopes), an out-of-bounds control payload
// (RFC §6.3 payload bounds), an unknown method name, a not-found target
// (a `start` for a nonexistent parent, a control for a run with no
// inbox), and a catch-all for a runtime-side failure the surface could
// not classify. Later Protocol surfaces add their own codes here in
// their own phases.
package errors

import "fmt"

// Code is a stable, client-facing Protocol error code. It is part of the
// versioned Protocol surface — the set of codes is stable across a
// Runtime refactor, and a Protocol client branches on the Code.
type Code string

// The Phase 54 task-control surface error codes.
const (
	// CodeInvalidRequest — the request was structurally malformed: a
	// wrong wire type for the method, a nil request, a request body the
	// surface could not decode into the method's expected shape.
	CodeInvalidRequest Code = "invalid_request"
	// CodeIdentityRequired — the request carried an incomplete identity
	// scope (a missing tenant / user / session, or a missing run on a
	// steering-control method). RFC §5.5: "the Protocol rejects any
	// request without an identity scope." Fails closed — there is no
	// identity-downgrading knob (CLAUDE.md §6 rule 9).
	CodeIdentityRequired Code = "identity_required"
	// CodeScopeMismatch — the caller's steering scope claim is below the
	// control method's RFC §6.3 minimum, or a cross-tenant steering
	// submission was made without the admin scope. Maps from
	// steering.ErrScopeMismatch / steering.ErrInvalidScope.
	CodeScopeMismatch Code = "scope_mismatch"
	// CodePayloadInvalid — the control payload violated an RFC §6.3
	// bound (depth > 6, > 64 keys, > 50 list items, a string > 4096
	// chars, > 16 KiB total) or carried a leaf of an unsupported type.
	// Maps from steering.ErrPayloadInvalid /
	// steering.ErrUnsupportedPayloadValue.
	CodePayloadInvalid Code = "payload_invalid"
	// CodeUnknownMethod — the method name is not one of the ten
	// canonical task-control methods.
	CodeUnknownMethod Code = "unknown_method"
	// CodeNotFound — the request's target does not exist: a steering
	// control for a run with no live inbox (the run never started or
	// already ended), a `start` referencing a nonexistent parent task.
	// Maps from steering.ErrInboxNotFound / tasks.ErrNotFound.
	CodeNotFound Code = "not_found"
	// CodeRuntimeError — a runtime-side failure the surface could not
	// classify into a more specific code. The catch-all; a transport
	// adapter maps it to a 500.
	CodeRuntimeError Code = "runtime_error"
	// CodeAuthRejected — Phase 61 Protocol auth: the request carried a
	// JWT bearer that failed cryptographic / structural verification —
	// a malformed token, an `alg` outside the asymmetric allowlist
	// (CLAUDE.md §7 rule 1), an invalid signature, an expired or
	// not-yet-valid token, an unknown `kid`, an audience / issuer
	// mismatch. Distinct from CodeIdentityRequired (which signals an
	// absent identity scope, not a present-but-invalid one) — a client
	// that gets CodeIdentityRequired needs to *attach* a token; a
	// client that gets CodeAuthRejected has one but it failed to
	// verify. Maps to HTTP 401.
	CodeAuthRejected Code = "auth_rejected"
)

// canonicalCodes is the registered set — a fixed package-level map. A
// new Protocol error code is a new phase that declares a constant +
// extends this map; there is no registration escape hatch.
var canonicalCodes = map[Code]struct{}{
	CodeInvalidRequest:   {},
	CodeIdentityRequired: {},
	CodeScopeMismatch:    {},
	CodePayloadInvalid:   {},
	CodeUnknownMethod:    {},
	CodeNotFound:         {},
	CodeRuntimeError:     {},
	CodeAuthRejected:     {},
}

// IsValidCode reports whether c is one of the canonical Protocol error
// codes.
func IsValidCode(c Code) bool {
	_, ok := canonicalCodes[c]
	return ok
}

// Error is the Protocol error wire type: a stable client-facing Code
// plus a human-readable Message. It implements the `error` interface so
// a ControlSurface handler can return it directly and a caller can reach
// the Code via errors.As.
type Error struct {
	// Code is the stable, client-facing error code.
	Code Code `json:"code"`
	// Message is the human-readable explanation. Advisory — never the
	// thing a client branches on. The Message MUST NOT carry caller
	// payload data verbatim (CLAUDE.md §7 rule 7); the ControlSurface
	// builds Messages from stable strings + the offending method /
	// identity scope, never from raw control-payload values.
	Message string `json:"message"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("protocol: %s: %s", e.Code, e.Message)
}

// New builds a *Error with the given code and message. The message is
// caller-controlled — callers MUST NOT pass raw control-payload data
// (CLAUDE.md §7 rule 7).
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds a *Error with a printf-formatted message. As with New,
// the format + args MUST NOT include raw control-payload data.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
