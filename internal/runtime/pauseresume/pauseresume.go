// Package pauseresume ships Harbor's ONE pause/resume primitive — the
// unified Coordinator that HITL approval, tool-side OAuth, A2A
// AUTH_REQUIRED / INPUT_REQUIRED, and operator/Console PAUSE all
// converge on (CLAUDE.md §7 rule 4, RFC §3.3 + §6.3). There are not
// four parallel pause implementations; there is this one.
//
// # The shape
//
// The Coordinator interface is three methods — Request, Resume,
// Status — keyed on an opaque, runtime-owned Token. Planners never
// instantiate the Coordinator and never reach into it: a planner
// signals "I need a pause" by returning the planner.RequestPause
// Decision shape (Phase 42 / D-047), and the runtime executor (a
// later phase) drives the Coordinator. This package never imports the
// planner package — it consumes only the planner.PauseReason enum, via
// the byte-stable Reason typedef bridge below. That one-directional
// dependency on a pure enum keeps the swappable-planner property
// intact (the master-plan flags Phase 50 as a critical-path phase for
// exactly this reason).
//
// # Durability
//
// A pause is always recorded in a process-local registry keyed by
// Token. Durability — surviving a Runtime restart — rides on the
// existing state.StateStore (Phase 07), handed to the Coordinator as
// an OPTIONAL checkpoint store via WithCheckpointStore. When a
// checkpoint store is configured, Request serialises the pause record
// (including the trajectory, via trajectory.Trajectory.Serialize) and
// Saves it; a fresh Coordinator over the same store rehydrates the
// pause on demand. When NO checkpoint store is configured, pauses are
// process-local only and explicitly do NOT survive restart. This is
// the master-plan acceptance criterion verbatim: "pauses survive
// Runtime restart only when StateStore-backed checkpoint is
// configured."
//
// Phase 50 deliberately does NOT mint a second persistence-driver
// seam. state.StateStore is already the §4.4 persistence seam, with
// three V1 drivers (in-mem / SQLite / Postgres) at conformance parity;
// a parallel CheckpointStore interface would be the §13
// two-parallel-implementations smell. See D-067.
//
// # Fail loudly
//
// There is no silent-degradation path (RFC §3.4, CLAUDE.md §13).
// trajectory.ErrUnserializable from Trajectory.Serialize propagates
// verbatim out of Request — a pause whose trajectory cannot serialise
// is rejected loud, never half-persisted. trajectory.ErrToolContextLost
// from HandleRegistry.Get propagates verbatim out of Resume — a run is
// never resumed with a nil tool context. Missing identity fails closed
// with ErrIdentityRequired; an unknown token surfaces ErrPauseNotFound.
//
// # Concurrent reuse (D-025)
//
// The Coordinator is a compiled artifact: immutable after
// construction, with per-call state living in ctx + arguments and the
// pause registry behind a documented-invariant mutex. One Coordinator
// is safe to share across N concurrent goroutines; concurrent_test.go
// pins N≥100 under -race.
//
// # §13 primitive-with-consumer
//
// Phase 50 ships the primitive. The first end-to-end
// RequestPause-driven-through-the-Coordinator consumer is Phase 53
// (steering wiring) — same wave, Stage 3. Phase 51 (Stage 2) also
// consumes this package's surface (the pause-record serialise
// contract). See the phase-50 plan's "§13 primitive-with-consumer
// obligation" section and D-067.
package pauseresume

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// Reason is the pause-reason enum carried on a pause record. It is a
// byte-stable typedef bridge onto the planner-side planner.PauseReason
// (RFC §6.3, D-047): the planner package is the truth source for the
// four canonical values; this package re-exports them under the
// pauseresume namespace so runtime call sites do not import the
// planner package directly. The typedef (not a fresh type) keeps the
// two byte-identical.
type Reason = planner.PauseReason

// The four canonical pause reasons (RFC §6.3 — settled). Re-exported
// from the planner package via the Reason typedef bridge so callers
// in the runtime tree reach them under the pauseresume namespace.
const (
	// ReasonApprovalRequired — a human needs to approve a
	// planner-chosen tool call before execution (HITL).
	ReasonApprovalRequired = planner.PauseApprovalRequired
	// ReasonAwaitInput — the run needs additional input from the
	// user / supervisor before continuing.
	ReasonAwaitInput = planner.PauseAwaitInput
	// ReasonExternalEvent — the run is waiting on an external event
	// (webhook, scheduled trigger, A2A callback, tool-side OAuth
	// completion).
	ReasonExternalEvent = planner.PauseExternalEvent
	// ReasonConstraintsConflict — a constraint conflict (budget vs.
	// tool requirement, identity scope mismatch) requires operator
	// resolution.
	ReasonConstraintsConflict = planner.PauseConstraintsConflict
)

// IsValidReason reports whether r is one of the four canonical pause
// reasons. Delegates to planner.IsValidPauseReason — the planner
// package owns the enum, this package owns the bridge.
func IsValidReason(r Reason) bool {
	return planner.IsValidPauseReason(r)
}

// Token is the opaque, runtime-issued handle for a paused run. Clients
// never construct or parse a Token — the runtime owns the encoding
// (Phase 50 uses a ULID source). A Token is unique per Request call.
type Token string

// State is the lifecycle state of a pause record.
type State string

const (
	// StatusPaused — the run is paused; Resume has not been called.
	StatusPaused State = "paused"
	// StatusResumed — Resume has been called for this Token; the
	// record is terminal (a second Resume returns ErrAlreadyResumed).
	StatusResumed State = "resumed"
)

// Pause is the value returned by Coordinator.Request: the opaque
// Token plus the sanitised reason / payload / timestamp / identity of
// the paused run. Payload is depth/size-bounded by the caller (the
// Protocol edge enforces the RFC §6.3 steering-payload bounds before
// a pause request ever reaches the Coordinator); this package treats
// Payload as opaque sanitised data.
type Pause struct {
	// Token is the opaque runtime-issued handle for this pause.
	Token Token
	// Reason is one of the four canonical pause reasons.
	Reason Reason
	// Payload is the sanitised, bounded pause payload (auth URL +
	// scopes for OAuth, approval context for HITL, etc.).
	Payload map[string]any
	// PausedAt is the wall-clock time the pause was recorded.
	PausedAt time.Time
	// Identity is the (tenant, user, session) triple the pause was
	// recorded under. Resume validates the resuming scope against it.
	Identity identity.Identity
}

// PauseRequest is the input to Coordinator.Request.
type PauseRequest struct {
	Payload    map[string]any
	Trajectory *trajectory.Trajectory
	Identity   identity.Identity
	Reason     Reason
}

// Status is the value returned by Coordinator.Status: a read-only
// snapshot of a pause record's lifecycle without mutating it.
type Status struct {
	PausedAt  time.Time
	ResumedAt time.Time
	State     State
	Reason    Reason
}

// Coordinator is Harbor's unified pause/resume primitive. One
// Coordinator is built per Runtime process and shared across all runs;
// it is safe for concurrent use by N goroutines (D-025).
//
// Implementations MUST:
//   - mint an opaque, unique Token per Request;
//   - fail closed on a missing identity triple (ErrIdentityRequired);
//   - propagate trajectory.ErrUnserializable verbatim out of Request
//     and trajectory.ErrToolContextLost verbatim out of Resume — no
//     silent-degradation path;
//   - validate the resuming identity scope against the pause's scope
//     (ErrScopeMismatch);
//   - treat Resume as idempotent — a second Resume of the same Token
//     returns ErrAlreadyResumed, never a double-apply.
type Coordinator interface {
	// Request records a pause and returns its opaque Token. When a
	// checkpoint store is configured the pause record (including the
	// optional trajectory) is persisted; a non-serialisable trajectory
	// fails loud with trajectory.ErrUnserializable and nothing is
	// persisted.
	Request(ctx context.Context, req PauseRequest) (Pause, error)

	// Resume terminates a pause: it validates the resuming identity
	// scope, re-attaches any tool-context handles via the
	// HandleRegistry (propagating trajectory.ErrToolContextLost on a
	// lost handle), marks the record resumed, and clears the
	// checkpoint. A second Resume of the same Token returns
	// ErrAlreadyResumed.
	//
	// `decision` is the typed marker carried on the emitted
	// `pause.resumed` event so wire consumers (the Console, third-party
	// clients, integration tests) can distinguish approve from reject
	// from generic resume from timeout without parsing free-form
	// `Reason` strings. An invalid Decision is rejected loud with
	// ErrInvalidDecision — there is no untyped default (§13 fail-loudly,
	// D-096).
	Resume(ctx context.Context, token Token, decision Decision, payload map[string]any) error

	// Status returns a read-only snapshot of a pause record's
	// lifecycle. When the Token is absent from the in-memory registry
	// but a checkpoint store is configured, Status falls back to a
	// checkpoint load (the restart-survival path). An unknown Token
	// returns ErrPauseNotFound.
	Status(ctx context.Context, token Token) (Status, error)

	// List returns a paginated snapshot of pause records visible under
	// the caller's identity scope (Phase 72e — the `pause.list`
	// Protocol surface). Read-only: it does NOT mutate the registry,
	// does NOT call Resume, and does NOT clear checkpoints.
	//
	// Identity-mandatory: a missing (tenant, user, session) triple on
	// req.Identity returns wrapped ErrIdentityRequired. Pagination is
	// mandatory — a 0 / negative / over-max PageSize, or a negative
	// Page, returns wrapped ErrInvalidPage; the snapshot is never
	// silently clamped.
	//
	// Cross-tenant visibility: when req.Filter.TenantIDs names a tenant
	// other than req.Identity.TenantID (or more than one tenant), the
	// caller MUST set req.AdminScoped — otherwise List returns wrapped
	// ErrCrossTenantScope. The Coordinator does NOT read the scope from
	// ctx; the Protocol-edge handler is responsible for verifying the
	// `auth.ScopeAdmin` claim and setting AdminScoped (separation of
	// concerns — D-079).
	List(ctx context.Context, req ListRequest) (ListResponse, error)
}

// ListRequest is the input to Coordinator.List — the runtime-internal
// projection of the Protocol-edge types.PauseListRequest.
type ListRequest struct {
	Identity    identity.Identity
	Filter      ListFilter
	Page        int
	PageSize    int
	AdminScoped bool
}

// ListFilter is the runtime-internal filter shape for Coordinator.List.
// Empty slices are wildcards; a zero Since / Until is "no bound".
type ListFilter struct {
	Since      time.Time
	Until      time.Time
	States     []State
	TenantIDs  []string
	UserIDs    []string
	SessionIDs []string
	RunIDs     []string
	Reasons    []Reason
}

// ListResponse is the value returned by Coordinator.List.
type ListResponse struct {
	// Snapshots is the page of pause records, ordered PausedAt
	// descending (newest first).
	Snapshots []Pause
	// Statuses is parallel to Snapshots — Statuses[i] is the lifecycle
	// status of Snapshots[i].
	Statuses []Status
	// Page is the 1-based page number this response covers.
	Page int
	// PageSize is the per-page row count applied.
	PageSize int
	// PageCount is the total number of pages over the filtered set.
	PageCount int
	// TotalRows is the total filtered row count across all pages.
	TotalRows int
	// Truncated is true when a status=resumed filter was requested but
	// the resumed slice has aged out of the in-memory registry — see
	// the Coordinator's destructive-on-resume contract (coordinator.go).
	Truncated bool
}
