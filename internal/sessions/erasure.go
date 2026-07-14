package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

// erasureAuditSession is the reserved session slot the content-free
// `session.erased` audit event — and the durable erasure-ledger
// checkpoint described below — are published/persisted under. The
// erasure runs own-session-only, so the actor's verified triple IS the
// erased triple; publishing/persisting under the erased session would
// re-persist a durable StateRecord there (the durable event bus keys
// every event by its identity triple) and leave the erased session's
// `state.history` non-empty. Instead both ride the actor's
// (tenant, user) with this reserved observability session — the
// compliance sink — so the erased triple stays genuinely empty
// post-erasure (RFC §6.13 / §7). The angle-bracket shape does not occur
// in session ids Harbor's own surfaces mint, and upstream token /
// session-header validation is assumed to reject it in caller-supplied
// ids — identity.Validate itself does not enforce a charset, so a
// hostile session id equal to this sentinel would alias the compliance
// sink. That is the same standing assumption the session-discovery
// catalog's sentinel already rests on; an identity-layer charset guard
// is a possible cross-cutting follow-up, deliberately not added here.
const erasureAuditSession = "<erasure-audit>"

// erasureLedgerKindPrefix namespaces the durable erasure-ledger
// checkpoint StateStore records (see erasureLedgerRecord). One record
// per erased session id, keyed under the actor's observability scope
// (ledgerScope) so it survives the erased triple's own
// StateStore.DeleteScope clear.
const erasureLedgerKindPrefix = "session.erasure.pending."

// erasureTombstoneKindPrefix namespaces the durable, content-free erasure
// TOMBSTONE records. Unlike the pending ledger (deleted on erasure success),
// the tombstone is TERMINAL — written once per converged erasure and never
// removed — so reopen has an O(1) fail-closed point-Load to reject a
// reopen-after-erase even after the erasure fully converged and the pending
// ledger is gone. Keyed per session id under the
// actor's observability scope (the SAME scope as the ledger + the
// session.erased record-of-fact — it survives the erased triple's
// DeleteScope). Content-free: session id + timestamp + deletion counts, the
// same shape the already-retained session.erased event carries, adding no new
// retained information about deleted data (RFC §7 right-to-erasure).
const erasureTombstoneKindPrefix = "session.erasure.tombstone."

// CascadeEraser performs the ordered, fail-loud, idempotent session
// erasure cascade behind `sessions.delete`: it refuses fail-loud on a
// running task, then deletes the session's scoped Artifacts, Memory, and
// State (the kind-agnostic StateStore scope delete removes the durable
// session-lifecycle record + run-scoped trajectories + planner
// checkpoints + the durable event stream), clears the registry's
// in-memory catalogs, and completes with a redacted, content-free
// `session.erased` audit event under the actor's observability scope.
//
// There is no ACID transaction across the three independent stores; the
// cascade is ordered and every per-store delete is idempotent, so a
// mid-cascade error surfaces loudly (never a partial silent success —
// CLAUDE.md §13) and the cascade is safe to re-invoke to convergence.
// The refuse-if-running pre-flight runs FIRST so a refusal touches
// nothing; the event-bus fence (see fenceSession) runs immediately after
// it and BEFORE any destructive step, and a present-but-failing fence
// capability fails the whole cascade loud right there — the same
// retry-safe posture as a mid-cascade store error, just earlier.
//
// # The audit record is part of the erasure's success criteria
//
// Two follow-ups (issues #409/#410) hardened this cascade's audit trail
// on top of the ordering + fencing shape above. Previously, the final
// `session.erased` emit was best-effort AFTER the destructive steps
// completed: a transient bus/redactor failure lost the ONLY audit record
// while Erase still reported success, and — because the destructive
// steps (specifically StateStore.DeleteScope) had already removed the
// session-lifecycle record — a re-invoke returned ErrSessionNotFound,
// leaving no second chance to emit. Separately, a mid-cascade retry
// re-ran idempotent deletes that found fewer records the second time, so
// the reported counts reflected only the final converging attempt.
//
// The BINDING ordering invariant: at no point may (data irrevocably
// gone) ∧ (no durable audit record) ∧ (success returned) hold — nor the
// inverse (record says erased, data present, success returned). The
// chosen mechanism is to persist a durable compliance checkpoint
// (erasureLedgerRecord, via the StateStore the cascade already drives)
// BEFORE each destructive step's result would otherwise become
// unrecoverable, rather than bounded-retrying the emit in-call.
// Concretely:
//
//  1. The ledger checkpoint is persisted under the actor's observability
//     scope (ledgerScope — same reserved (tenant, user, <erasure-audit>)
//     slot the final event publishes under), NOT the erased triple, so it
//     survives StateStore.DeleteScope's clear of the erased session's own
//     scope. It is tenant/user-scoped OPERATOR AUDIT DATA about a deleted
//     session — never user content — addressing the phase plan's
//     identity-scoping risk note head-on.
//  2. The ledger is checkpointed immediately after EVERY destructive step
//     (artifacts, memory, state), accumulating each step's own count onto
//     whatever a PRIOR interrupted attempt already contributed (loaded at
//     the top of Erase). This is what makes counts cumulative across
//     converging attempts (#410): a step whose data is already gone on a
//     retry contributes 0 locally, but the ledger already holds the
//     earlier attempt's non-zero contribution. The ledger carries the
//     session lifecycle's OpenedAt stamp so accumulation NEVER crosses a
//     session-id reuse boundary: when the target session still exists (a
//     fresh attempt, not a converging retry) and a leftover ledger's
//     stamp mismatches the live session's OpenedAt, the leftover belongs
//     to an ABANDONED prior lifecycle of the reused id — it is discarded
//     (with a loud Warn) and the cascade starts from zero, so a reused
//     session id's counts reflect only its own lifecycle. Discarding
//     necessarily forfeits the abandoned lifecycle's never-emitted
//     record-of-fact: the caller was told loudly (ErrErasureRecordFailed)
//     to re-invoke and instead reopened the id — inflating the NEW
//     lifecycle's compliance counts to preserve a stale checkpoint would
//     be the worse corruption.
//  3. Because the ledger holds the complete, accurate counts BEFORE
//     StateStore.DeleteScope ever runs (steps 1-2 checkpoint before step
//     3), the compliance record is durably persisted strictly before the
//     irreversible clear — satisfying the first half of the invariant.
//     The final record-of-fact emit (completeErasure/emitErased) reads
//     ONLY from the ledger, never recomputing counts, so it can't drift.
//  4. A re-invoke after the destructive steps completed but the final
//     emit failed finds the session already gone (ErrSessionNotFound from
//     the registry pre-flight) AND a still-present ledger checkpoint —
//     recognized as a CONVERGING retry: it skips every destructive step
//     (nothing left to do) and goes straight to completeErasure, which
//     re-attempts the record + emit using the ledger's cumulative counts.
//     Only a successful emit clears the ledger, so an emit failure keeps
//     the session "re-invokable" without ever touching a store again.
//  5. A redactor refusal or a bus-publish failure at the final emit both
//     fail Erase loud with the wrapped sentinel ErrErasureRecordFailed
//     (no `Error`-log-and-continue) — the same loud path, because both
//     mean the durable record-of-fact did not land.
//  6. The final emit is IDEMPOTENT per (session, lifecycle): before
//     publishing, completeErasure checks the observability scope's
//     retained history (when the bus supports events.HistoryReplayer)
//     for a `session.erased` record already carrying this session id +
//     lifecycle stamp, and on a hit skips the publish — converging
//     silently to success and still clearing the ledger. This closes the
//     duplicate-emit window when the publish succeeded but the ledger
//     cleanup failed and the caller re-invoked. The check is
//     capability-gated and bounded (see recordAlreadyEmitted): when it
//     cannot verify, it emits — a duplicate compliance record is the
//     acceptable failure direction, a lost one never is.
//  7. Concurrent Erase calls for the SAME session are serialized by a
//     striped IN-PROCESS lock (lockSession) so exactly one goroutine in
//     this process ever runs the cascade for a given session at a time:
//     the loser blocks until the winner's cascade (through ledger
//     cleanup) is fully done, then sees the genuine ErrSessionNotFound
//     with no ledger — "one wins, one gets the not-found path, never a
//     double event." HONESTY NOTE — the lock is per-process only: two
//     runtime REPLICAS racing sessions.delete for the same session are
//     NOT serialized (their ledger checkpoints are last-writer-wins
//     StateStore saves, and both may run destructive steps — each of
//     which is idempotent, so the data outcome converges). The
//     cross-replica residuals are count skew on the racing attempts and
//     a possible duplicate emit where the item-6 history check races the
//     other replica's publish; never a lost record, never resurrected
//     data. Cross-replica serialization would need a StateStore CAS/lease
//     primitive — the same recorded limitation as the agent-config
//     registry's per-process owner locks.
//
// Documented residual gap (accepted, not silently dropped): there is no
// ACID transaction spanning a destructive store (artifacts/memory/state)
// and the StateStore the ledger checkpoint itself is persisted through —
// a generalized transactional-outbox subsystem is an explicit non-goal
// of this design. A failure in the narrow window between a destructive
// step succeeding and its OWN checkpoint save committing means that
// step's contribution is not durably recorded; a converging retry's
// local count for that step is by then 0 (already deleted), so the
// final total under-counts by exactly that step's amount. This is
// strictly narrower than the earlier whole-cascade gap it replaces (a
// failure ANYWHERE previously lost the whole attempt's counts) and is
// pinned by TestCascadeEraser_LedgerSaveFailure_LoudAndRetrySafe rather
// than left silently untested. It never affects the fail-loud /
// no-lost-record invariant (CLAUDE.md §13) — only count exactness on
// this specific interleaving.
//
// A constructed *CascadeEraser is immutable after construction and safe
// to share across N concurrent goroutines: every method's per-call state
// lives in the call's arguments and locals, the shared stores + registry
// are each independently concurrency-safe, and the fixed-size striped
// lock array (eraseLocks) is the one internally-synchronized mutable
// field — it never grows with the number of distinct sessions ever
// erased.
type CascadeEraser struct {
	registry *Registry
	state    state.StateStore
	memory   memory.MemoryStore
	arts     artifacts.ArtifactStore
	bus      events.EventBus
	redactor audit.Redactor // optional — defence-in-depth before the emit
	clock    Clock
	logger   *slog.Logger

	// eraseLocks stripes a fixed-size array of mutexes over the
	// (tenant, user, session) key so concurrent Erase calls for the SAME
	// session serialize (see lockSession); distinct sessions run fully
	// concurrently except for the rare shard collision (benign — erasure
	// is a rare operation). Fixed size — never grows with the number of
	// distinct sessions ever erased (an internally-synchronized array,
	// not a growing map).
	eraseLocks [eraseLockShards]sync.Mutex
}

// eraseLockShards bounds the per-session erase-serialization lock
// memory to a constant footprint regardless of how many distinct
// sessions are ever erased over the process's lifetime — the same
// striped-mutex pattern as
// internal/runtime/agentcfg/protocol/service.go's writeLocks.
const eraseLockShards = 256

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

// ErrErasureRecordFailed — the cascade's destructive steps completed (in
// this attempt or an earlier converging one) but the durable
// record-of-fact could not be completed: a redactor refusal or a
// bus-publish failure at the final `session.erased` emit.
// Erase fails the WHOLE call loud rather than reporting success with a
// missing/incomplete audit trail. The session's scoped data IS gone;
// the durable erasure-ledger checkpoint (persisted before the
// irreversible StateStore.DeleteScope clear — see erasureLedgerRecord)
// survives, so a re-invoke converges: it skips straight to re-attempting
// the record + emit from the checkpointed cumulative counts, never
// re-running a destructive step.
var ErrErasureRecordFailed = errors.New("sessions: erasure record-of-fact could not be durably completed")

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
// before Erase is reached. Order (RFC §6.9, hardened with the durable
// record-of-fact ordering + cumulative counts described above):
//
//  0. lock: serialize concurrent Erase calls for this exact session
//     (lockSession) so only one goroutine ever runs the steps below for
//     it at a time; load any durable erasure-ledger checkpoint a PRIOR
//     interrupted attempt left behind.
//  1. refuse-if-running: load+verify the session record under id and
//     probe the running-task seam. A refusal touches NO store — UNLESS a
//     ledger checkpoint already exists, in which case ErrSessionNotFound
//     here means the destructive steps already fully completed on a
//     prior attempt and only the final record-of-fact remains: Erase
//     converges via completeErasure without touching artifacts / memory
//     / state again. When the session EXISTS, the ledger's lifecycle
//     stamp (the session's OpenedAt) is verified: a leftover ledger from
//     an abandoned prior lifecycle of a reused session id is discarded
//     with a loud Warn so counts never inflate across a reuse boundary.
//  2. fence: mark the triple erased on the event bus, BEFORE any
//     destructive step. A present-but-failing Fence capability fails the
//     WHOLE erasure loud here — nothing has been touched yet, so this is
//     retry-safe. A bus that does not implement the capability at all is
//     a logged downgrade, not an error (see fenceSession).
//  3. artifacts: enumerate the session's artifacts and delete each, then
//     durably checkpoint the cumulative count (issue #410).
//  4. memory: flush the session's memory to a clean state, then
//     checkpoint.
//  5. state: kind-agnostic scope delete (the session-lifecycle record,
//     run-scoped trajectories, planner checkpoints, and the durable event
//     stream all live under the triple and all go) — THE irreversible
//     clear — then checkpoint the final cumulative count. Because steps
//     3-5 each checkpoint before the NEXT step runs, the ledger is
//     always durably complete strictly before this step's clear takes
//     effect ("record before the irreversible clear" ordering).
//  6. complete: clear the registry's in-memory catalogs, build the
//     redacted content-free `session.erased` payload from the ledger's
//     cumulative counts, publish it (skipping idempotently when a record
//     for this session+lifecycle already exists — recordAlreadyEmitted),
//     and — only once the record durably exists — remove the ledger
//     checkpoint (completeErasure).
//
// Returns ErrSessionRunning (refused — 409), ErrSessionNotFound (absent
// under the caller's identity AND no pending ledger — 404),
// ErrErasureRecordFailed (destructive steps done, record incomplete —
// re-invokable), or a wrapped store/fence error (loud, retry-safe: every
// step through state.DeleteScope is idempotent, and the fence step (2)
// runs before step 3 ever touches a store). The response carries
// non-sensitive deletion telemetry only.
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

	// 0. Serialize concurrent Erase calls for this exact session.
	unlock := e.lockSession(id)
	defer unlock()

	ledger, hasLedger, err := e.loadLedger(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("sessions: erase ledger load: %w", err)
	}

	// 1. Refuse-if-running pre-flight (load+verify + probe). Touches
	//    nothing on refusal — unless a pending ledger says the
	//    destructive work already finished (converging retry).
	sess, perr := e.registry.preflightErase(ctx, id.SessionID)
	if perr != nil {
		if errors.Is(perr, ErrSessionNotFound) && hasLedger {
			return e.completeErasure(ctx, id, ledger)
		}
		return zero, perr
	}

	// The session EXISTS, so this is a fresh attempt or a mid-cascade
	// retry of the CURRENT lifecycle — never a converging retry. Stamp /
	// verify the ledger's lifecycle discriminator: a leftover ledger
	// whose stamp mismatches the live session's OpenedAt belongs to an
	// ABANDONED prior lifecycle of a reused session id and MUST be
	// discarded, or its stale counts would inflate this lifecycle's
	// compliance totals (the count-inflation bug on session-id reuse).
	stamp := sess.OpenedAt.UnixNano()
	if hasLedger && ledger.SessionOpenedAt != stamp {
		e.logger.WarnContext(ctx, "sessions: discarding stale erasure-ledger checkpoint from an abandoned prior lifecycle of a reused session id — its never-emitted record-of-fact is forfeited; this lifecycle's counts start from zero",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.Int64("stale_lifecycle", ledger.SessionOpenedAt),
			slog.Int64("current_lifecycle", stamp))
		ledger = erasureLedgerRecord{}
	}
	ledger.SessionOpenedAt = stamp

	// 2. Fence the session on the event bus BEFORE the sweep. The
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
	//
	//     A Fence error (the capability IS present but the call failed)
	//     fails the ENTIRE erasure loud, here, before step 3 touches any
	//     store (CLAUDE.md §13 — silent degradation is forbidden). This is
	//     safe precisely because fenceSession runs before any destructive
	//     step: nothing has been deleted yet, so the cascade is a clean,
	//     retry-safe no-op on this error — the documented idempotent-retry
	//     contract converges once the transient fault clears.
	if err := e.fenceSession(ctx, id); err != nil {
		return zero, fmt.Errorf("sessions: erase fence: %w", err)
	}

	// 3. Artifacts — checkpoint immediately so an interruption before the
	//    next step never loses this attempt's contribution (#410).
	artifactsDeleted, err := e.eraseArtifacts(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("sessions: erase artifacts: %w", err)
	}
	ledger.ArtifactsDeleted += artifactsDeleted
	if err := e.saveLedger(ctx, id, ledger); err != nil {
		return zero, fmt.Errorf("sessions: erase ledger checkpoint (artifacts): %w", err)
	}

	// 4. Memory.
	if err := e.memory.Flush(ctx, identity.Quadruple{Identity: id}); err != nil {
		return zero, fmt.Errorf("sessions: erase memory: %w", err)
	}
	ledger.MemoryPurged = true
	if err := e.saveLedger(ctx, id, ledger); err != nil {
		return zero, fmt.Errorf("sessions: erase ledger checkpoint (memory): %w", err)
	}

	// 5. State scope delete (removes the session-lifecycle record + every
	//    kind/run under the triple, including the durable event stream) —
	//    the irreversible clear. The ledger already durably holds every
	//    count up to and including this step's result BEFORE Erase ever
	//    reports success. The clear runs through the registry's
	//    deleteScopeSerialized — the same r.mu the whole-record writers
	//    (Touch / Close / SetTitle) hold across their load→save — so a
	//    writer that loaded the record before this clear can never save
	//    it back AFTER it, re-persisting an erased session's lifecycle
	//    record. Lock ordering: this call site already holds the striped
	//    per-session erase lock; registry methods never acquire the
	//    striped locks, so striped → r.mu is acyclic.
	stateDeleted, err := e.registry.deleteScopeSerialized(ctx, e.state, id)
	if err != nil {
		return zero, fmt.Errorf("sessions: erase state: %w", err)
	}
	ledger.StateRecordsDeleted += stateDeleted
	if err := e.saveLedger(ctx, id, ledger); err != nil {
		return zero, fmt.Errorf("sessions: erase ledger checkpoint (state): %w", err)
	}

	// 6. Complete: registry clear + redacted record-of-fact emit + ledger
	//    cleanup.
	return e.completeErasure(ctx, id, ledger)
}

// completeErasure finishes the cascade's final leg. It (re-)clears the
// registry's in-memory catalogs — idempotent, safe whether or not a
// prior attempt already ran it — builds the redacted, content-free
// `session.erased` payload from the durably checkpointed ledger counts,
// publishes it UNLESS a record for this session+lifecycle already
// exists in the observability scope's retained history (the idempotent
// re-emit guard, recordAlreadyEmitted), writes the durable, terminal
// erasure TOMBSTONE (UNCONDITIONAL — outside the re-emit guard —
// success-critical, and strictly BEFORE the ledger cleanup so reopen's
// isErased guard never sees a gap), and — only once the
// record + tombstone durably exist — removes the ledger checkpoint.
// Reached either fresh (from Erase's step 6, right after DeleteScope) or
// via a converging retry (Erase's pre-flight found the session already
// gone but a ledger checkpoint still pending).
func (e *CascadeEraser) completeErasure(ctx context.Context, id identity.Identity, ledger erasureLedgerRecord) (prototypes.SessionsDeleteResponse, error) {
	var zero prototypes.SessionsDeleteResponse
	if err := e.registry.clearErased(ctx, id); err != nil {
		return zero, fmt.Errorf("sessions: erase registry clear: %w", err)
	}

	resp := prototypes.SessionsDeleteResponse{
		SessionID:           id.SessionID,
		Deleted:             true,
		StateRecordsDeleted: ledger.StateRecordsDeleted,
		ArtifactsDeleted:    ledger.ArtifactsDeleted,
		MemoryPurged:        ledger.MemoryPurged,
	}
	if !e.recordAlreadyEmitted(ctx, id, ledger.SessionOpenedAt) {
		if err := e.emitErased(ctx, id, resp, ledger.SessionOpenedAt); err != nil {
			return zero, err
		}
	}

	// Write the durable erasure TOMBSTONE — UNCONDITIONAL per terminal
	// completeErasure invocation, OUTSIDE the recordAlreadyEmitted emit-skip
	// guard above. If it were co-located inside the
	// !recordAlreadyEmitted block, a converging retry where a prior attempt
	// emitted the event but died before the tombstone write would see
	// recordAlreadyEmitted==true → skip tombstone → deleteLedger → leave
	// NEITHER ledger nor tombstone → a reopen-after-erase would then silently
	// resurrect the id. The Save is idempotent (overwrite) and SUCCESS-CRITICAL:
	// it runs BEFORE deleteLedger (write-happens-before-delete — at every
	// instant isErased == ledger∨tombstone with no gap), and a failed Save
	// fails the erasure loud (wrapped ErrErasureRecordFailed) and MUST NOT
	// proceed to deleteLedger, so a tombstone-fails + ledger-deleted interleave
	// can never open a converged-erasure resurrection gap.
	if err := e.saveTombstone(ctx, id, resp); err != nil {
		e.logger.ErrorContext(ctx, "sessions: erasure tombstone write failed — erasure fails loud; the pending ledger is retained so a re-invoke converges",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("error", err.Error()))
		return zero, fmt.Errorf("sessions: erase tombstone: %w: %w", ErrErasureRecordFailed, err)
	}

	// The record-of-fact AND the terminal tombstone are durably persisted at
	// this point — the invariant this cascade defends is already satisfied. A
	// failure removing the now-superfluous ledger checkpoint is logged loudly
	// rather than failing an already-successful erasure: isErased still reads
	// the tombstone, so the only residual effect is a stray durable ledger
	// record (harmless) — never a lost record or a reopenable erased id
	// (CLAUDE.md §13 concerns itself with losing the record, not an
	// over-cautious extra one).
	if err := e.deleteLedger(ctx, id); err != nil {
		e.logger.WarnContext(ctx, "sessions: erasure ledger checkpoint cleanup failed after a successful record-of-fact emit — a stray durable record may remain",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("error", err.Error()))
	}
	return resp, nil
}

// lockSession acquires the striped per-session lock serializing
// concurrent Erase calls for the SAME (tenant, user, session) so exactly
// one goroutine ever runs the cascade for a given session at a time — a
// second caller blocks until the first's cascade (including the final
// record-of-fact emit + ledger cleanup) has fully finished. By the time
// the loser acquires the lock the session and its ledger are gone, so
// its own pre-flight returns the genuine ErrSessionNotFound: "one wins,
// one gets the not-found path, never a double event" (CLAUDE.md §11).
// Distinct sessions that happen to hash to the same shard serialize
// occasionally too — benign over-locking (erasure is rare), never a
// correctness loss.
//
// The lock is PER-PROCESS. Two runtime replicas racing sessions.delete
// for the same session are not serialized by it: their ledger
// checkpoints are last-writer-wins StateStore saves and both replicas
// may run the (idempotent) destructive steps, so the data outcome still
// converges but the racing attempts can skew counts and the
// recordAlreadyEmitted history check can race the other replica's
// publish into a duplicate emit — never a lost record. Cross-replica
// serialization would need a StateStore CAS/lease primitive; the
// CascadeEraser type doc records this limitation.
func (e *CascadeEraser) lockSession(id identity.Identity) func() {
	key := id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
	m := &e.eraseLocks[eraseLockShard(key)]
	m.Lock()
	return m.Unlock
}

// eraseLockShard maps a session lock key to a stable shard index in
// [0, eraseLockShards). FNV-1a over the key bytes — deterministic and
// allocation-free — so the same session key always resolves to the same
// shard.
func eraseLockShard(key string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := range len(key) {
		h ^= uint32(key[i])
		h *= prime
	}
	return h % eraseLockShards
}

// fenceSession marks the session's triple erased on the event bus so a
// task still finishing concurrently cannot re-create retained history under
// the just-erased triple (see events.Fencer). It runs BEFORE the State
// sweep so the sweep removes anything an in-flight Publish persisted before
// the fence took hold, and the fence drops everything after.
//
// The bus is the canonical events.EventBus; the fence is an optional
// capability, and the two failure shapes have DIFFERENT postures:
//
//   - Capability absent (the bus does not implement events.Fencer): a
//     documented, logged-loud DOWNGRADE, not an error. Erase proceeds on
//     the primary State sweep alone (which removes all already-retained
//     history); the un-closable late-event window is logged loudly so the
//     degraded posture is observable, never silently assumed.
//   - Capability present but Fence(ctx, id) returns an error: fenceSession
//     returns the error and Erase fails the WHOLE erasure loud, BEFORE any
//     destructive step runs (step 3 is the first store mutation). A
//     half-applied fence would leave the late-event window open while the
//     caller believes erasure succeeded — the exact silent-degradation
//     shape CLAUDE.md §13 forbids. Because nothing has been deleted yet,
//     failing here is retry-safe: the documented idempotent-retry
//     contract converges once the transient fault clears.
func (e *CascadeEraser) fenceSession(ctx context.Context, id identity.Identity) error {
	fencer, ok := e.bus.(events.Fencer)
	if !ok {
		e.logger.WarnContext(ctx, "sessions: event bus does not support fencing — erasure proceeds on the State sweep alone; late events from an in-flight task may briefly re-appear in state.history",
			slog.String("session_id", id.SessionID),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID))
		return nil
	}
	if err := fencer.Fence(ctx, id); err != nil {
		// No "sessions:" prefix here — Erase wraps this error with
		// "sessions: erase fence:", and a doubled package prefix reads
		// as noise in the operator-facing error chain.
		return fmt.Errorf("event-bus fence failed for session %q: %w", id.SessionID, err)
	}
	return nil
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

// erasureLedgerRecord is the durable checkpoint the cascade persists via
// the StateStore's ordinary Save/Load/Delete surface — NOT the erasure
// scope delete — under the actor's observability scope (ledgerScope),
// keyed per erased session id. It exists solely to survive across
// interrupted/retried Erase attempts for ONE session: its cumulative
// counts are the source of truth completeErasure reads from, and its
// presence is what tells a later Erase call "the destructive steps
// already ran; only the record-of-fact remains." It carries no user
// content — bounded counts and a bool, the same shape as
// SessionErasedPayload minus the timestamp.
type erasureLedgerRecord struct {
	ArtifactsDeleted    int  `json:"artifacts_deleted"`
	MemoryPurged        bool `json:"memory_purged"`
	StateRecordsDeleted int  `json:"state_records_deleted"`
	// SessionOpenedAt is the erased session lifecycle's OpenedAt stamp
	// (unix nanoseconds) — the lifecycle discriminator. A leftover ledger
	// whose stamp mismatches the LIVE session's OpenedAt belongs to an
	// abandoned prior lifecycle of a reused session id and is discarded
	// rather than accumulated (see Erase), and the same stamp keys the
	// idempotent re-emit guard (recordAlreadyEmitted).
	SessionOpenedAt int64 `json:"session_opened_at"`
}

// ledgerScope returns the StateStore identity the erasure ledger for id's
// session is persisted under: the actor's (tenant, user) with the
// reserved erasureAuditSession slot — the SAME scope the final
// `session.erased` event publishes under. It is deliberately NOT the
// erased triple: StateStore.DeleteScope(ctx, id) removes every record
// under the erased triple, and a ledger stored there would vanish at
// exactly the moment it is needed to survive.
func ledgerScope(id identity.Identity) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: erasureAuditSession,
	}}
}

// ledgerKind returns the StateStore Kind the erasure ledger for
// sessionID is keyed under. Namespaced per session id so concurrent
// erasures of DISTINCT sessions belonging to the same actor never share
// a slot.
func ledgerKind(sessionID string) string {
	return erasureLedgerKindPrefix + sessionID
}

// loadLedger returns the durable erasure checkpoint for id's session, if
// one exists from a prior attempt (interrupted mid-cascade, or between
// the irreversible clear and a successful record-of-fact emit). Returns
// the zero ledger and hasLedger=false when none exists — a fresh
// erasure attempt with no prior progress to accumulate onto.
func (e *CascadeEraser) loadLedger(ctx context.Context, id identity.Identity) (erasureLedgerRecord, bool, error) {
	rec, err := e.state.Load(ctx, ledgerScope(id), ledgerKind(id.SessionID))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return erasureLedgerRecord{}, false, nil
		}
		return erasureLedgerRecord{}, false, err
	}
	var ledger erasureLedgerRecord
	if err := json.Unmarshal(rec.Bytes, &ledger); err != nil {
		return erasureLedgerRecord{}, false, fmt.Errorf("corrupt erasure ledger for session %q: %w", id.SessionID, err)
	}
	return ledger, true, nil
}

// saveLedger durably persists ledger's current cumulative counts for
// id's session. Called after every destructive step so an interruption
// before the NEXT step never loses this attempt's contribution (#410).
// A fresh EventID is minted per call: this is a new logical version of
// the checkpoint slot, not a retried write of the same one, so Save's
// same-EventID-different-Bytes conflict rule does not apply — Save
// simply overwrites the (Identity, Kind) slot, exactly as intended.
func (e *CascadeEraser) saveLedger(ctx context.Context, id identity.Identity, ledger erasureLedgerRecord) error {
	bytes, err := json.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("marshal erasure ledger: %w", err)
	}
	return e.state.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: ledgerScope(id),
		Kind:     ledgerKind(id.SessionID),
		Bytes:    bytes,
	})
}

// deleteLedger removes the erasure-ledger checkpoint for id's session.
// Idempotent (state.StateStore.Delete on an absent key is a no-op).
func (e *CascadeEraser) deleteLedger(ctx context.Context, id identity.Identity) error {
	return e.state.Delete(ctx, ledgerScope(id), ledgerKind(id.SessionID))
}

// erasureTombstoneRecord is the durable, content-free TERMINAL marker a
// converged erasure leaves so reopen can fail loud (ErrReopenAfterErase) even
// after the pending ledger and the session.lifecycle record are both gone. It
// carries the same non-sensitive shape as SessionErasedPayload (session id +
// counts + timestamp) — NO user content — so it adds no new retained
// information about deleted data beyond what the already-retained
// session.erased record-of-fact holds.
type erasureTombstoneRecord struct {
	SessionID           string `json:"session_id"`
	StateRecordsDeleted int    `json:"state_records_deleted"`
	ArtifactsDeleted    int    `json:"artifacts_deleted"`
	MemoryPurged        bool   `json:"memory_purged"`
	ErasedAt            int64  `json:"erased_at"`
}

// tombstoneKind returns the StateStore Kind the erasure tombstone for
// sessionID is keyed under. Namespaced per session id, under the same
// observability scope (ledgerScope) as the pending ledger.
func tombstoneKind(sessionID string) string {
	return erasureTombstoneKindPrefix + sessionID
}

// saveTombstone durably persists the terminal erasure tombstone for id's
// session. Idempotent (overwrites the (Identity, Kind) slot on a re-invoke).
// A fresh EventID per call — a new logical version of the slot, not a retried
// same-EventID write. Called from completeErasure BEFORE deleteLedger, and its
// failure fails the erasure loud (never proceeding to deleteLedger).
func (e *CascadeEraser) saveTombstone(ctx context.Context, id identity.Identity, resp prototypes.SessionsDeleteResponse) error {
	rec := erasureTombstoneRecord{
		SessionID:           id.SessionID,
		StateRecordsDeleted: resp.StateRecordsDeleted,
		ArtifactsDeleted:    resp.ArtifactsDeleted,
		MemoryPurged:        resp.MemoryPurged,
		ErasedAt:            e.clock.Now().UnixNano(),
	}
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal erasure tombstone: %w", err)
	}
	return e.state.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: ledgerScope(id),
		Kind:     tombstoneKind(id.SessionID),
		Bytes:    bytes,
	})
}

// isErased is the reopen-facing terminality guard: it reports whether
// the session named by ident is (being) erased, so Open / EnsureOpen can fail
// loud with ErrReopenAfterErase rather than reviving deleted data or minting a
// fresh empty session on a converged erasure. It is an O(1) point-Load of the
// pending erasure LEDGER (an in-flight / interrupted erasure) OR the terminal
// TOMBSTONE (a converged erasure) — never a bounded history scan.
//
// It FAILS CLOSED: it returns (false, nil) ONLY when BOTH Loads return
// state.ErrNotFound; any other (non-NotFound) Load error propagates, and the
// caller then mints/re-activates NOTHING (mirroring loadLedger). A fail-OPEN
// collapse to "not erased" on a transient StateStore fault would permit
// resurrection, which is exactly the direction the erasure contract forbids.
//
// Called under the registry's r.mu (Open holds it), so it serialises against
// the erasure cascade's r.mu-held record clear (deleteScopeSerialized) and
// catalog clear (clearErased). The tombstone is written by completeErasure
// strictly BEFORE the pending ledger is deleted, so at every instant
// isErased == (ledger present) ∨ (tombstone present) with no gap.
func (r *Registry) isErased(ctx context.Context, ident identity.Identity) (bool, error) {
	scope := ledgerScope(ident)
	// Pending ledger (in-flight / interrupted erasure).
	if _, err := r.store.Load(ctx, scope, ledgerKind(ident.SessionID)); err == nil {
		return true, nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return false, fmt.Errorf("sessions: isErased ledger load: %w", err)
	}
	// Terminal tombstone (converged erasure).
	if _, err := r.store.Load(ctx, scope, tombstoneKind(ident.SessionID)); err == nil {
		return true, nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return false, fmt.Errorf("sessions: isErased tombstone load: %w", err)
	}
	return false, nil
}

// erasureDedupeScanLimit bounds the recordAlreadyEmitted history scan:
// the record for THIS session+lifecycle, if it exists at all, was
// published by a recent attempt of this same erasure, so it sits among
// the actor's most recent observability-scope events. The bound keeps
// the guard O(recent-window) instead of O(full-history); a record older
// than the window is simply not found, and the guard then re-emits — a
// duplicate compliance record is the acceptable failure direction, a
// lost one never is.
const erasureDedupeScanLimit = 512

// recordAlreadyEmitted reports whether a `session.erased` record for
// this exact session id + lifecycle stamp already exists in the actor's
// observability-scope retained history — the idempotent re-emit guard
// completeErasure consults before publishing. It closes the
// duplicate-emit window when a prior attempt's publish succeeded but its
// ledger cleanup failed and the caller re-invoked, and narrows the
// cross-replica duplicate window (see lockSession).
//
// Best-effort by design, in the SAFE direction only: a bus without the
// events.HistoryReplayer capability, a scan error, or a record beyond
// the bounded window all report false — completeErasure then publishes,
// risking at worst a duplicate record, never a lost one. The lifecycle
// stamp is matched via the event's Extra metadata (exact string
// compare), and the session id via the payload — handling both the
// typed in-process payload and the durable log's generic RedactedMap
// rehydration.
func (e *CascadeEraser) recordAlreadyEmitted(ctx context.Context, id identity.Identity, stamp int64) bool {
	hr, ok := e.bus.(events.HistoryReplayer)
	if !ok {
		return false
	}
	filter := events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: erasureAuditSession,
		Types:   []events.EventType{EventTypeSessionErased},
	}
	evs, err := hr.Window(ctx, 0, erasureDedupeScanLimit, filter)
	if err != nil {
		e.logger.WarnContext(ctx, "sessions: erasure re-emit guard could not scan the observability history — emitting (a duplicate record beats a lost one)",
			slog.String("session_id", id.SessionID),
			slog.String("error", err.Error()))
		return false
	}
	want := strconv.FormatInt(stamp, 10)
	for _, ev := range evs {
		if ev.Extra[erasureLifecycleExtraKey] != want {
			continue
		}
		switch p := ev.Payload.(type) {
		case SessionErasedPayload:
			if p.SessionID == id.SessionID {
				return true
			}
		case events.RedactedMap:
			if s, ok := p.Data["SessionID"].(string); ok && s == id.SessionID {
				return true
			}
		}
	}
	return false
}

// erasureLifecycleExtraKey is the Event.Extra metadata key carrying the
// erased session lifecycle's OpenedAt stamp (unix nanoseconds, decimal
// string) on the `session.erased` record. It exists solely so the
// idempotent re-emit guard (recordAlreadyEmitted) can distinguish "this
// lifecycle's record already exists" from "an OLDER lifecycle of the
// same reused session id was erased before" — matching on session id
// alone would wrongly suppress the newer lifecycle's record. A bounded,
// low-cardinality label per the Extra slot's contract; the string form
// round-trips exactly through the durable log's generic JSON
// rehydration (an int64 payload field would decode as float64).
const erasureLifecycleExtraKey = "erased_session_opened_at"

// emitErased builds, redacts, and publishes the redacted, content-free
// `session.erased` audit event under the actor's observability scope.
// The erased session id rides as a payload field; the event Identity is
// the actor's (tenant, user) with the reserved erasureAuditSession slot,
// so the emit never re-persists durable state under the erased triple.
// The lifecycle stamp rides as Extra metadata (erasureLifecycleExtraKey)
// so a re-invoke can recognize the record idempotently.
//
// This is now part of Erase's SUCCESS CRITERIA, not best-effort
// observability. The SafePayload is run through the audit.Redactor when
// one is wired (defence-in-depth); a redactor refusal returns a wrapped
// ErrErasureRecordFailed rather than logging and continuing (CLAUDE.md
// §7 rule 6 / §13), and so does a bus.Publish failure. Either failure
// means the durable record-of-fact did not land — completeErasure
// propagates the error as Erase's own, and the ledger checkpoint
// (already durably persisted before this call ever runs, across
// whichever attempt got the destructive steps done) survives so a
// re-invoke converges without re-running any destructive step.
func (e *CascadeEraser) emitErased(ctx context.Context, actor identity.Identity, resp prototypes.SessionsDeleteResponse, stamp int64) error {
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
			e.logger.ErrorContext(ctx, "sessions: session.erased redaction failed — erasure fails loud; a re-invoke converges",
				append(logAttrs, slog.String("error", err.Error()))...)
			return fmt.Errorf("sessions: erase record: %w: redaction failed: %w", ErrErasureRecordFailed, err)
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
		Extra:      map[string]string{erasureLifecycleExtraKey: strconv.FormatInt(stamp, 10)},
	}
	if err := e.bus.Publish(ctx, ev); err != nil {
		e.logger.ErrorContext(ctx, "sessions: session.erased emit failed — erasure fails loud; a re-invoke converges",
			append(logAttrs, slog.String("error", err.Error()))...)
		return fmt.Errorf("sessions: erase record: %w: publish failed: %w", ErrErasureRecordFailed, err)
	}
	return nil
}
