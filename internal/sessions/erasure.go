package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

// erasureAuditSession is the reserved session slot the content-free
// `session.erased` audit event is published under. The erasure runs
// own-session-only, so the actor's verified triple IS the erased triple;
// publishing the audit event under the erased session would re-persist a
// durable StateRecord there (the durable event bus keys every event by
// its identity triple) and leave the erased session's `state.history`
// non-empty. Instead the event rides the actor's (tenant, user) with
// this reserved observability session — the compliance sink — so the
// erased triple stays genuinely empty post-erasure (RFC §6.13 / §7). The
// angle-bracket shape can never collide with a real conversation id (it
// is not a legal JWT claim or session-header value), mirroring the
// session-discovery catalog's sentinel.
const erasureAuditSession = "<erasure-audit>"

// CascadeEraser performs the ordered, fail-loud, idempotent session
// erasure cascade behind `sessions.delete`: it refuses fail-loud on a
// running task, then deletes the session's scoped Artifacts, Memory, and
// State (the kind-agnostic StateStore scope delete removes the durable
// session-lifecycle record + run-scoped trajectories + planner
// checkpoints + the durable event stream), clears the registry's
// in-memory catalogs, and emits a redacted, content-free `session.erased`
// audit event under the actor's observability scope.
//
// There is no ACID transaction across the three independent stores; the
// cascade is ordered and every per-store delete is idempotent, so a
// mid-cascade error surfaces loudly (never a partial silent success —
// CLAUDE.md §13) and the cascade is safe to re-invoke to convergence.
// The refuse-if-running pre-flight runs FIRST so a refusal touches
// nothing.
//
// A constructed *CascadeEraser is immutable after construction and safe
// to share across N concurrent goroutines, each erasing a distinct
// session: every method's per-call state lives in the call's arguments
// and locals; the shared stores + registry are each independently
// concurrency-safe.
type CascadeEraser struct {
	registry *Registry
	state    state.StateStore
	memory   memory.MemoryStore
	arts     artifacts.ArtifactStore
	bus      events.EventBus
	redactor audit.Redactor // optional — defence-in-depth before the emit
	clock    Clock
	logger   *slog.Logger
}

// CascadeEraserDeps bundles the seams the CascadeEraser drives. Registry,
// State, Memory, Artifacts, and Bus are mandatory; Redactor, Clock, and
// Logger are optional.
type CascadeEraserDeps struct {
	Registry  *Registry
	State     state.StateStore
	Memory    memory.MemoryStore
	Artifacts artifacts.ArtifactStore
	Bus       events.EventBus
	Redactor  audit.Redactor
	Clock     Clock
	Logger    *slog.Logger
}

// ErrEraserMisconfigured — NewCascadeEraser was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5) rather than building
// an eraser that would nil-panic on the first erasure.
var ErrEraserMisconfigured = errors.New("sessions: CascadeEraser missing a mandatory dependency")

// NewCascadeEraser builds the session-erasure cascade orchestrator.
// Registry / State / Memory / Artifacts / Bus are mandatory — a nil
// fails loud with a wrapped ErrEraserMisconfigured. The returned
// *CascadeEraser is immutable after construction and safe for concurrent
// use by N goroutines.
func NewCascadeEraser(deps CascadeEraserDeps) (*CascadeEraser, error) {
	switch {
	case deps.Registry == nil:
		return nil, fmt.Errorf("%w: Registry is nil", ErrEraserMisconfigured)
	case deps.State == nil:
		return nil, fmt.Errorf("%w: State is nil", ErrEraserMisconfigured)
	case deps.Memory == nil:
		return nil, fmt.Errorf("%w: Memory is nil", ErrEraserMisconfigured)
	case deps.Artifacts == nil:
		return nil, fmt.Errorf("%w: Artifacts is nil", ErrEraserMisconfigured)
	case deps.Bus == nil:
		return nil, fmt.Errorf("%w: Bus is nil", ErrEraserMisconfigured)
	}
	clock := deps.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &CascadeEraser{
		registry: deps.Registry,
		state:    deps.State,
		memory:   deps.Memory,
		arts:     deps.Artifacts,
		bus:      deps.Bus,
		redactor: deps.Redactor,
		clock:    clock,
		logger:   logger,
	}, nil
}

// Erase runs the full erasure cascade for the verified identity. The
// identity is the caller's own verified `(tenant, user, session)` — the
// own-session-only scope contract is enforced at the wire / service edge
// before Erase is reached. Order (RFC §6.9):
//
//  1. refuse-if-running: load+verify the session record under id and
//     probe the running-task seam. A refusal touches NO store.
//  2. artifacts: enumerate the session's artifacts and delete each.
//  3. memory: flush the session's memory to a clean state.
//  4. state: kind-agnostic scope delete (the session-lifecycle record,
//     run-scoped trajectories, planner checkpoints, and the durable event
//     stream all live under the triple and all go).
//  5. clear the registry's in-memory catalogs + discovery-catalog entry.
//  6. emit the redacted, content-free `session.erased` audit event under
//     the actor's observability scope.
//
// Returns ErrSessionRunning (refused — 409), ErrSessionNotFound (absent
// under the caller's identity — 404), or a wrapped store error (loud,
// retry-safe). The response carries non-sensitive deletion telemetry
// only.
func (e *CascadeEraser) Erase(ctx context.Context, id identity.Identity) (prototypes.SessionsDeleteResponse, error) {
	var zero prototypes.SessionsDeleteResponse
	if err := identity.Validate(id); err != nil {
		return zero, err
	}
	// Thread the verified identity onto ctx so the registry pre-flight's
	// ctx-identity check is satisfied even when Erase is reached off a
	// path that did not carry it (the handler normally already has).
	ctx, err := identity.With(ctx, id)
	if err != nil {
		return zero, err
	}

	// 1. Refuse-if-running pre-flight (load+verify + probe). Touches
	//    nothing on refusal.
	if _, perr := e.registry.preflightErase(ctx, id.SessionID); perr != nil {
		return zero, perr
	}

	// 1b. Fence the session on the event bus BEFORE the sweep. The
	//     running-task probe cannot see a task that the asynchronous
	//     `control.start` path has not yet registered, so a task can still be
	//     finishing concurrently and emit lifecycle events to the durable
	//     event log AFTER the State sweep below — re-creating retained
	//     history under the just-erased triple. The fence closes that window:
	//     it synchronises with the bus's publish path (an in-flight Publish
	//     either completes before Fence returns, and is then swept by
	//     DeleteScope, or observes the fence and is dropped) and makes the
	//     erased triple read as empty history regardless. A bus that lacks
	//     the capability still gets the primary sweep below; the gap-closing
	//     fence is logged-loud-skipped, never silently assumed.
	e.fenceSession(ctx, id)

	// 2. Artifacts.
	artifactsDeleted, err := e.eraseArtifacts(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("sessions: erase artifacts: %w", err)
	}

	// 3. Memory.
	if err := e.memory.Flush(ctx, identity.Quadruple{Identity: id}); err != nil {
		return zero, fmt.Errorf("sessions: erase memory: %w", err)
	}

	// 4. State scope delete (removes the session-lifecycle record + every
	//    kind/run under the triple, including the durable event stream).
	stateDeleted, err := e.state.DeleteScope(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("sessions: erase state: %w", err)
	}

	// 5. Clear the registry's in-memory catalogs + discovery catalog.
	if err := e.registry.clearErased(ctx, id); err != nil {
		return zero, fmt.Errorf("sessions: erase registry clear: %w", err)
	}

	// 6. Emit the content-free audit event under the actor's
	//    observability scope (never the erased triple).
	resp := prototypes.SessionsDeleteResponse{
		SessionID:           id.SessionID,
		Deleted:             true,
		StateRecordsDeleted: stateDeleted,
		ArtifactsDeleted:    artifactsDeleted,
		MemoryPurged:        true,
	}
	e.emitErased(ctx, id, resp)
	return resp, nil
}

// fenceSession marks the session's triple erased on the event bus so a
// task still finishing concurrently cannot re-create retained history under
// the just-erased triple (see events.Fencer). It runs BEFORE the State
// sweep so the sweep removes anything an in-flight Publish persisted before
// the fence took hold, and the fence drops everything after.
//
// The bus is the canonical events.EventBus; the fence is an optional
// capability. When the configured bus implements it (both V1 drivers do),
// a Fence error fails the erasure loud — a half-applied fence would leave
// the late-event window open and silently degrade the erasure guarantee
// (CLAUDE.md §13). When the bus does NOT implement it, the erasure proceeds
// on the primary State sweep alone (which removes all already-retained
// history); the un-closable late-event window is logged loudly so the
// degraded posture is observable, never silently assumed.
func (e *CascadeEraser) fenceSession(ctx context.Context, id identity.Identity) {
	fencer, ok := e.bus.(events.Fencer)
	if !ok {
		e.logger.WarnContext(ctx, "sessions: event bus does not support fencing — erasure proceeds on the State sweep alone; late events from an in-flight task may briefly re-appear in state.history",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID))
		return
	}
	if err := fencer.Fence(ctx, id); err != nil {
		// Loud, but non-fatal to the cascade: the primary sweep still
		// erases all already-retained history. Surface so the gap-closing
		// failure is observable rather than silently swallowed.
		e.logger.ErrorContext(ctx, "sessions: event-bus fence failed — erasure proceeds on the State sweep alone",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("error", err.Error()))
	}
}

// eraseArtifacts enumerates the session's artifacts (across every task)
// and deletes each. Both List and Delete are idempotent; the count is
// the number of artifacts that existed before delete.
func (e *CascadeEraser) eraseArtifacts(ctx context.Context, id identity.Identity) (int, error) {
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
		// TaskID empty ⇒ wildcard: every artifact under the session.
	}
	refs, err := e.arts.List(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	deleted := 0
	for _, ref := range refs {
		existed, derr := e.arts.Delete(ctx, ref.Scope, ref.ID)
		if derr != nil {
			return deleted, fmt.Errorf("delete %q: %w", ref.ID, derr)
		}
		if existed {
			deleted++
		}
	}
	return deleted, nil
}

// emitErased publishes the redacted, content-free `session.erased` audit
// event under the actor's observability scope. The erased session id
// rides as a payload field; the event Identity is the actor's
// (tenant, user) with the reserved erasureAuditSession slot, so the emit
// never re-persists durable state under the erased triple.
//
// The emit is best-effort observability — the erasure has already
// completed durably, so a bus failure is logged loudly rather than
// failing the (successful) erasure. The SafePayload is run through the
// audit.Redactor when one is wired (defence-in-depth); a redactor refusal
// logs loudly and skips the publish — never an unredacted emit
// (CLAUDE.md §7 rule 6 / §13).
func (e *CascadeEraser) emitErased(ctx context.Context, actor identity.Identity, resp prototypes.SessionsDeleteResponse) {
	now := e.clock.Now()
	payload := SessionErasedPayload{
		SessionID:           resp.SessionID,
		StateRecordsDeleted: resp.StateRecordsDeleted,
		ArtifactsDeleted:    resp.ArtifactsDeleted,
		MemoryPurged:        resp.MemoryPurged,
		ErasedAt:            now.UnixNano(),
	}
	logAttrs := []any{
		slog.String("session_id", resp.SessionID),
		slog.String("tenant_id", actor.TenantID),
		slog.String("user_id", actor.UserID),
	}
	if e.redactor != nil {
		if _, err := e.redactor.Redact(ctx, payload); err != nil {
			e.logger.ErrorContext(ctx, "sessions: session.erased redaction failed — event NOT published",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}
	observability := identity.Quadruple{Identity: identity.Identity{
		TenantID:  actor.TenantID,
		UserID:    actor.UserID,
		SessionID: erasureAuditSession,
	}}
	ev := events.Event{
		Type:       EventTypeSessionErased,
		Identity:   observability,
		OccurredAt: now,
		Payload:    payload,
	}
	if err := e.bus.Publish(ctx, ev); err != nil {
		e.logger.WarnContext(ctx, "sessions: session.erased emit failed",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
