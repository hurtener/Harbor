package pauseresume

import "errors"

// Sentinel errors. Callers compare via errors.Is.
//
// Two fail-loudly errors are NOT redefined here: trajectory.ErrUnserializable
// (raised by trajectory.Trajectory.Serialize when a pause request's
// trajectory carries a non-JSON-encodable leaf) and
// trajectory.ErrToolContextLost (raised by trajectory.HandleRegistry.Get
// when a handle cannot be re-attached on resume). The Coordinator
// propagates both verbatim; callers reach them via errors.As against
// the trajectory package's struct sentinels. Redefining them here
// would fork the fail-loudly contract Phase 43 already owns.
var (
	// ErrIdentityRequired — Request / Resume / Status was called with
	// an identity triple missing one of (tenant, user, session). The
	// Coordinator fails closed (CLAUDE.md §6 rule 9 + D-001); there is
	// no identity-downgrading knob.
	ErrIdentityRequired = errors.New("pauseresume: identity triple incomplete")

	// ErrPauseNotFound — Resume / Status was called for a Token with
	// no live pause record (and, when a checkpoint store is configured,
	// no persisted checkpoint either). Typical cause: an already-cleared
	// resume, or a token from a different Runtime process with no
	// shared checkpoint store.
	ErrPauseNotFound = errors.New("pauseresume: pause token not found")

	// ErrAlreadyResumed — Resume was called for a Token whose pause
	// record is already in StatusResumed. Resume is idempotent: the
	// second call is rejected loud rather than re-applying side
	// effects.
	ErrAlreadyResumed = errors.New("pauseresume: pause already resumed")

	// ErrScopeMismatch — Resume was called with an identity triple
	// whose (tenant, user, session) does not match the triple the
	// pause was Requested under. Authentication on resume is checked
	// against the original pause's identity scope (RFC §3.3).
	ErrScopeMismatch = errors.New("pauseresume: resume identity scope does not match pause")

	// ErrInvalidReason — Request was called with a Reason that is not
	// one of the four canonical pause reasons (RFC §6.3). Fails closed
	// rather than recording a malformed pause record.
	ErrInvalidReason = errors.New("pauseresume: invalid pause reason")

	// ErrCheckpointStoreRequired — an operation that needs a
	// configured checkpoint store was attempted on a Coordinator
	// constructed without one. Surfaces a misconfiguration loud
	// instead of silently degrading to process-local behaviour.
	ErrCheckpointStoreRequired = errors.New("pauseresume: operation requires a configured checkpoint store")

	// ErrCheckpointCorrupt — a checkpoint loaded from the StateStore
	// failed to decode into a pause record. Surfaces store corruption
	// loud rather than resuming with a half-decoded record.
	ErrCheckpointCorrupt = errors.New("pauseresume: checkpoint record is corrupt")
)
