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
// would fork the fail-loudly contract already owns.
var (
	// ErrIdentityRequired — Request / Resume / Status was called with
	// an identity triple missing one of (tenant, user, session). The
	// Coordinator fails closed (CLAUDE.md §6 rule 9); there is
	// no identity-downgrading knob.
	ErrIdentityRequired = errors.New("pauseresume: identity triple incomplete")

	// ErrPauseNotFound — Resume / Status was called for a Token with
	// no live pause record (and, when a checkpoint store is configured,
	// no persisted checkpoint either). Typical cause: an already-cleared
	// resume, or a token from a different Runtime process with no
	// shared checkpoint store.
	ErrPauseNotFound = errors.New("pauseresume: pause token not found")

	// ErrRestartUnavailable reports a durable tranche checkpoint whose
	// original in-process run loop is gone. A trajectory checkpoint alone is
	// not an exact redrive implementation, so resume fails closed rather than
	// pretending to continue the run as a new task.
	ErrRestartUnavailable = errors.New("pauseresume: exact restart redrive unavailable")

	// ErrAlreadyResumed — Resume was called for a Token whose pause
	// record is already in StatusResumed. Resume is idempotent: the
	// second call is rejected loud rather than re-applying side
	// effects.
	ErrAlreadyResumed           = errors.New("pauseresume: pause already resumed")
	ErrTrancheCancellerRequired = errors.New("pauseresume: coordinator cannot cancel live tranche pauses")

	ErrNotTranchePause = errors.New("pauseresume: pause is not a step-tranche pause")

	// ErrScopeMismatch — Resume was called with an identity triple
	// whose (tenant, user, session) does not match the triple the
	// pause was Requested under. Authentication on resume is checked
	// against the original pause's identity scope (RFC §3.3).
	ErrScopeMismatch = errors.New("pauseresume: resume identity scope does not match pause")

	// ErrInvalidReason — Request was called with a Reason that is not
	// one of the four canonical pause reasons (RFC §6.3). Fails closed
	// rather than recording a malformed pause record.
	ErrInvalidReason = errors.New("pauseresume: invalid pause reason")

	// ErrCheckpointCorrupt — a checkpoint loaded from the StateStore
	// failed to decode into a pause record. Surfaces store corruption
	// loud rather than resuming with a half-decoded record.
	ErrCheckpointCorrupt = errors.New("pauseresume: checkpoint record is corrupt")

	// ErrInvalidDecision — Resume was called with a Decision that is
	// not one of the four canonical values (approve / reject / resume
	// / timeout). Fails closed rather than emitting a `pause.resumed`
	// event with an untyped Decision — the §13 fail-loudly contract
	// the typed Decision marker exists to enforce (issue #113).
	ErrInvalidDecision = errors.New("pauseresume: invalid resume decision")

	// ErrUnsupportedFormatVersion — a pause record loaded from the
	// StateStore carries a format_version this Runtime does not
	// recognise (a zero/absent version, or a higher version written by
	// a newer Runtime). The load-side half of the RFC §6.3 "JSON with
	// format_version: 1" contract: rather than silently mis-decoding a
	// forward-incompatible record against the current schema, the load
	// fails loud.
	ErrUnsupportedFormatVersion = errors.New("pauseresume: unsupported pause-record format_version")

	// ErrInvalidPage — Coordinator.List was called with a pagination
	// shape outside the accepted bounds: a negative Page, a negative
	// PageSize, or a PageSize above MaxListPageSize. The List path
	// fails closed rather than silently clamping — a silent clamp would
	// defeat the per-row identity boundary the snapshot guarantees.
	ErrInvalidPage = errors.New("pauseresume: invalid pause-list pagination")
	// ErrInvalidContinuation reports an empty/malformed continuation identity.
	ErrInvalidContinuation = errors.New("pauseresume: invalid continuation")
	// ErrContinuationHandlerMissing keeps a durable pause retriable when its
	// runtime has not registered the handler named by the checkpoint.
	ErrContinuationHandlerMissing = errors.New("pauseresume: continuation handler missing")
	// ErrContinuationKindRegistered prevents two consumers from racing to own
	// one durable continuation kind.
	ErrContinuationKindRegistered = errors.New("pauseresume: continuation kind already registered")
	// ErrResumeInProgress reports a concurrent claim of the same token while its
	// continuation is running. The handler still executes exactly once.
	ErrResumeInProgress = errors.New("pauseresume: resume already in progress")

	// ErrCrossTenantScope — Coordinator.List was called with a
	// ListFilter naming a tenant other than the caller's own (or more
	// than one tenant) without ListRequest.AdminScoped set. Cross-tenant
	// pause visibility requires the verified auth.ScopeAdmin claim;
	// the Coordinator fails closed rather than leaking
	// foreign-tenant pause records.
	ErrCrossTenantScope = errors.New("pauseresume: cross-tenant pause-list requires the admin scope claim")

	// ErrSweeperMisconfigured — RunSweeper was started against a
	// Coordinator the sweeper cannot maintain: a foreign Coordinator
	// implementation (the maintenance scan needs this package's
	// concrete registry), or one constructed without
	// WithMaxParkDuration (nothing would ever expire — a sweeper that
	// silently spins forever reaping nothing is the §13
	// silent-degradation shape).
	ErrSweeperMisconfigured = errors.New("pauseresume: sweeper misconfigured")
)
