package steering

import "errors"

// Sentinel errors. Callers compare via errors.Is. The Protocol-edge
// projection (Phase 54) maps ErrScopeMismatch to a 403 + an audit
// emit, and ErrPayloadInvalid / ErrUnknownControlType to a 400.
var (
	// ErrIdentityRequired — a steering operation was called with an
	// identity quadruple missing one of (tenant, user, session, run).
	// The inbox fails closed (CLAUDE.md §6 rule 9 + D-001); there is
	// no identity-downgrading knob. The run component is mandatory
	// too: the inbox is per-run.
	ErrIdentityRequired = errors.New("steering: identity quadruple incomplete")

	// ErrUnknownControlType — a ControlEvent carried a Type that is
	// not one of the nine canonical control types (RFC §6.3). Rejected
	// at the edge rather than enqueued.
	ErrUnknownControlType = errors.New("steering: unknown control type")

	// ErrPayloadInvalid — a ControlEvent payload violated one of the
	// RFC §6.3 bounds (depth > 6, > 64 keys, > 50 list items, a string
	// > 4096 chars, or > 16 KiB total). The payload is REJECTED loud,
	// never silently truncated to fit (CLAUDE.md §5 "fail loudly").
	// The wrapped message names which bound was exceeded.
	ErrPayloadInvalid = errors.New("steering: control payload failed validation")

	// ErrUnsupportedPayloadValue — a ControlEvent payload carried a
	// leaf value of a type the JSON-shaped steering surface does not
	// accept (a channel, a func, a complex number, etc.). Distinct
	// from ErrPayloadInvalid (a bound exceeded): this is a structural
	// rejection. Fails loud rather than coercing.
	ErrUnsupportedPayloadValue = errors.New("steering: control payload carries an unsupported value type")

	// ErrScopeMismatch — the caller's presented Scope is below the
	// minimum scope the control Type requires (RFC §6.3 per-event
	// scopes), or a cross-tenant steering attempt was made without the
	// admin scope. Fails closed; the Protocol edge maps this to 403 +
	// audit.
	ErrScopeMismatch = errors.New("steering: caller scope insufficient for control type")

	// ErrInvalidScope — a Scope value outside the three canonical
	// scopes was presented. Rejected rather than treated as the
	// weakest scope.
	ErrInvalidScope = errors.New("steering: invalid caller scope")

	// ErrInboxNotFound — a Registry lookup / drain / retire was asked
	// for a run quadruple with no live inbox. Typical cause: the run
	// already ended and its inbox was retired, or it never started.
	ErrInboxNotFound = errors.New("steering: no inbox for run")

	// ErrInboxExists — Registry.Open was called for a run quadruple
	// that already has a live inbox. Opening twice would orphan the
	// first inbox's queued events; the second call is rejected loud.
	ErrInboxExists = errors.New("steering: inbox already open for run")
)
