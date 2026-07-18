// Package engine holds Harbor's shared TaskRegistry state machine: the
// task/group/patch lifecycle, idempotency dedup, cascade-cancel,
// group governance (seal/resolve/fail-fast), retain-turn waiters, and
// WatchGroup fan-out. It is the single implementation every
// TaskRegistry driver wraps; drivers differ ONLY in how records are
// persisted, expressed through the [Backend] seam.
//
// The engine is persistence-agnostic: it never imports the StateStore.
// On construction it calls [Backend.Hydrate] to rebuild its in-memory
// indices, and on every mutation it writes through [Backend.SaveTask]
// / [Backend.SaveGroup] / [Backend.SavePatch]. The in-process driver
// supplies an ephemeral backend (no reload across restarts); the
// durable driver supplies a StateStore-backed backend whose records
// survive a restart. A backend implementation MAY block on I/O — the
// engine holds its lock across a Save, so a slow backend serializes
// mutations on that engine instance (acceptable for the
// single-instance durability the durable driver targets).
//
// Internal model:
//
//   - A primary `map[TaskID]*Task` holds the live task state.
//   - A secondary `map[idempotencyKey]idempotencyRecord` resolves
//     `(SessionID, IdempotencyKey)` lookups for `Spawn` dedup.
//   - A children index `map[TaskID][]TaskID` powers cascade-cancel
//     BFS without scanning the primary map.
//   - A single `sync.RWMutex` guards every map.
//   - Caller-controlled strings (Description, Query, Result.Value,
//     Error.Message) are run through the `audit.Redactor` BEFORE the
//     record is handed to the backend. The redactor's reflective walk
//     returns a `map[string]any`; we read the redacted strings back
//     and replace them on the Task before persisting.
//   - `Close(ctx)` flips an atomic flag; subsequent calls return
//     `ErrRegistryClosed`. There are no engine-owned goroutines to
//     join, so Close is fast.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// New constructs an Engine over the given event bus, redactor, and
// persistence backend, then hydrates its in-memory state from the
// backend. Bus, redactor, and backend are all mandatory; a nil
// argument fails loudly (CLAUDE.md §5).
//
// Hydration uses context.Background(): New runs at boot, before the
// engine is handed to any caller, so there is no request context to
// thread and no concurrent access to race. A backend whose Hydrate
// returns an error fails New loudly rather than booting with partial
// state.
func New(bus events.EventBus, redactor audit.Redactor, backend Backend) (*Engine, error) {
	if bus == nil {
		return nil, fmt.Errorf("tasks/engine: New requires a non-nil EventBus")
	}
	if redactor == nil {
		return nil, fmt.Errorf("tasks/engine: New requires a non-nil Redactor")
	}
	if backend == nil {
		return nil, fmt.Errorf("tasks/engine: New requires a non-nil Backend")
	}
	e := &Engine{
		backend:          backend,
		bus:              bus,
		redactor:         redactor,
		tasks:            map[tasks.TaskID]*tasks.Task{},
		idemIdx:          map[idempotencyKey]idempotencyRecord{},
		children:         map[tasks.TaskID][]tasks.TaskID{},
		groups:           map[tasks.TaskGroupID]*tasks.TaskGroup{},
		taskGroup:        map[tasks.TaskID]tasks.TaskGroupID{},
		groupSubs:        map[tasks.TaskGroupID][]*groupSubscriber{},
		groupCompletions: map[tasks.TaskGroupID]tasks.GroupCompletion{},
		retainWaiters:    map[string][]*retainWaiter{},
		patches:          map[patchKey]*tasks.Patch{},
		acknowledged:     map[tasks.TaskID]struct{}{},
		ulidEntropy:      ulid.Monotonic(rand.Reader, 0),
	}
	if err := e.hydrate(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}

// idempotencyKey scopes IdempotencyKey by SessionID. Spec'd by the
// plan: same key across different sessions creates two distinct
// tasks (the key is namespaced by SessionID).
type idempotencyKey struct {
	SessionID string
	Key       string
}

// idempotencyRecord captures the bookkeeping required to detect a
// divergent re-spawn under the same `(SessionID, IdempotencyKey)`.
// `TaskID` resolves the already-spawned handle; `ContentHash` is the
// SHA-256 of the pre-redaction `Description + Query` bytes (the only
// caller-controlled fields that pass through `audit.Redactor` before
// storage). Comparing hashes lets `spawnRequestsEqual` distinguish a
// genuine retry (same bytes → same hash) from a divergent payload
// (different bytes → different hash) without depending on
// post-redaction equality, which would false-positive whenever the
// redactor erases caller-controlled tokens.
type idempotencyRecord struct {
	TaskID      tasks.TaskID
	ContentHash [32]byte
}

type Engine struct {
	backend  Backend
	bus      events.EventBus
	redactor audit.Redactor

	mu       sync.RWMutex
	tasks    map[tasks.TaskID]*tasks.Task
	idemIdx  map[idempotencyKey]idempotencyRecord
	children map[tasks.TaskID][]tasks.TaskID

	// group + patch + retain-turn + watcher state. All
	// guarded by `mu`. See groups.go for the access patterns +
	// invariants.
	groups           map[tasks.TaskGroupID]*tasks.TaskGroup
	taskGroup        map[tasks.TaskID]tasks.TaskGroupID
	groupSubs        map[tasks.TaskGroupID][]*groupSubscriber
	groupCompletions map[tasks.TaskGroupID]tasks.GroupCompletion
	retainWaiters    map[string][]*retainWaiter // key = SessionID
	patches          map[patchKey]*tasks.Patch
	acknowledged     map[tasks.TaskID]struct{}

	closed      atomic.Bool
	ulidEntropy *ulid.MonotonicEntropy
}

// Spawn implements tasks.TaskRegistry.
func (e *Engine) Spawn(ctx context.Context, req tasks.SpawnRequest) (tasks.TaskHandle, error) {
	if e.closed.Load() {
		return tasks.TaskHandle{}, tasks.ErrRegistryClosed
	}
	if err := tasks.ValidateRequest(req); err != nil {
		return tasks.TaskHandle{}, err
	}
	propagate := req.PropagateOnCancel
	if propagate == "" {
		propagate = tasks.PropagateCascade
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Idempotency check: same (SessionID, IdempotencyKey) seen?
	// Empty IdempotencyKey disables dedup (every Spawn yields a fresh
	// handle).
	contentHash := spawnRequestContentHash(req)
	if req.IdempotencyKey != "" {
		idemK := idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey}
		if existing, ok := e.idemIdx[idemK]; ok {
			stored := e.tasks[existing.TaskID]
			if stored == nil {
				// Defensive: a dangling index entry whose task is absent
				// (a rolled-back spawn should never leave this, but never
				// dereference a nil here) is treated as a cache miss — drop
				// the stale entry and spawn fresh.
				delete(e.idemIdx, idemK)
			} else {
				if !spawnRequestsEqual(stored, existing.ContentHash, req, contentHash, propagate) {
					return tasks.TaskHandle{}, fmt.Errorf("%w: key=%q",
						tasks.ErrIdempotencyConflict, req.IdempotencyKey)
				}
				return tasks.TaskHandle{ID: existing.TaskID, Reused: true}, nil
			}
		}
	}

	// New task: assign ULID, persist, emit task.spawned.
	id := tasks.TaskID(ulid.MustNew(ulid.Now(), e.ulidEntropy).String())
	now := time.Now().UnixNano()

	// Caller-side audit redaction on the user-controlled strings.
	// Description / Query are the inputs at Spawn; Result /
	// Error are populated later by Mark*.
	redactedDesc, redactedQuery, err := e.redactSpawnFields(ctx, req.Description, req.Query)
	if err != nil {
		return tasks.TaskHandle{}, fmt.Errorf("tasks/engine: redact: %w", err)
	}

	// Defensive copy of the input-artifact ID slice — protects the
	// stored task from caller-side mutation of the SpawnRequest. Nil
	// stays nil so the JSON `omitempty` tag elides the field for
	// text-only spawns.
	var inputArtifactIDs []string
	if len(req.InputArtifactIDs) > 0 {
		inputArtifactIDs = append([]string(nil), req.InputArtifactIDs...)
	}
	// defensive copy of the per-attachment
	// disposition hint map, same caller-mutation rationale.
	var inputArtifactDispositions map[string]string
	if len(req.InputArtifactDispositions) > 0 {
		inputArtifactDispositions = make(map[string]string, len(req.InputArtifactDispositions))
		for k, v := range req.InputArtifactDispositions {
			inputArtifactDispositions[k] = v
		}
	}
	// Defensive copy of the output-schema bytes — the stored task must
	// not alias the caller's SpawnRequest slice. Nil stays nil so the
	// whole-record marshal elides nothing for schemaless spawns.
	var outputSchema []byte
	if len(req.OutputSchema) > 0 {
		outputSchema = append([]byte(nil), req.OutputSchema...)
	}
	t := &tasks.Task{
		ID:                id,
		Identity:          req.Identity,
		Kind:              req.Kind,
		Status:            tasks.StatusPending,
		Priority:          req.Priority,
		ParentTaskID:      req.ParentTaskID,
		Description:       redactedDesc,
		Query:             redactedQuery,
		PropagateOnCancel: propagate,
		NotifyOnComplete:  req.NotifyOnComplete,
		IdempotencyKey:    req.IdempotencyKey,
		CreatedAt:         now,
		UpdatedAt:         now,
		InputArtifactIDs:  inputArtifactIDs,
		// per-attachment disposition hints.
		InputArtifactDispositions: inputArtifactDispositions,
		// per-request output schema (raw JSON-Schema bytes).
		OutputSchema: outputSchema,
	}
	// Validate the requested group BEFORE persisting anything. A
	// missing / cross-session / sealed group must fail the spawn
	// without leaving a persisted orphan task behind: a durable backend
	// reloads records on open, so a rollback that only touched memory
	// would resurrect the task after a restart. All three checks here
	// are in-memory reads.
	var memberGroup *tasks.TaskGroup
	if req.GroupID != "" {
		g, ok := e.groups[req.GroupID]
		if !ok || !identitiesEqual(g.SessionID, req.Identity.Identity) {
			return tasks.TaskHandle{}, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, req.GroupID)
		}
		if g.Status != tasks.GroupOpen {
			return tasks.TaskHandle{}, tasks.ErrGroupSealed
		}
		memberGroup = g
	}

	if err := e.persistTaskLocked(ctx, t, contentHash); err != nil {
		return tasks.TaskHandle{}, err
	}
	e.tasks[id] = t
	if req.IdempotencyKey != "" {
		e.idemIdx[idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey}] = idempotencyRecord{
			TaskID:      id,
			ContentHash: contentHash,
		}
	}
	if req.ParentTaskID != nil && *req.ParentTaskID != "" {
		e.children[*req.ParentTaskID] = append(e.children[*req.ParentTaskID], id)
	}
	// Wire the new task into the requested group. The group was
	// pre-validated as Open above, so addMemberLocked is not expected to
	// fail; the error is still routed through compensation to keep the
	// path total. If the member-add or its persist fails, compensate
	// EVERY mutation — including the durable task record (via the
	// backend) — so a restart never resurrects a half-wired spawn the
	// caller was told failed.
	if memberGroup != nil {
		wireErr := e.addMemberLocked(memberGroup, id)
		added := wireErr == nil
		if added {
			wireErr = e.persistGroupLocked(ctx, memberGroup)
		}
		if wireErr != nil {
			if added {
				e.unwireMemberLocked(memberGroup, id)
			}
			delete(e.tasks, id)
			if req.IdempotencyKey != "" {
				delete(e.idemIdx, idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey})
			}
			if req.ParentTaskID != nil && *req.ParentTaskID != "" {
				e.detachChildLocked(*req.ParentTaskID, id)
			}
			if derr := e.backend.DeleteTask(ctx, t); derr != nil {
				return tasks.TaskHandle{}, fmt.Errorf("%w (task-record compensation failed: %w)", wireErr, derr)
			}
			return tasks.TaskHandle{}, wireErr
		}
	}

	parentForPayload := tasks.TaskID("")
	if req.ParentTaskID != nil {
		parentForPayload = *req.ParentTaskID
	}
	if err := e.publish(ctx, t, tasks.EventTypeTaskSpawned, tasks.TaskSpawnedPayload{
		TaskID:         id,
		Kind:           req.Kind,
		ParentTaskID:   parentForPayload,
		Priority:       req.Priority,
		IdempotencyKey: req.IdempotencyKey,
	}); err != nil {
		return tasks.TaskHandle{}, err
	}
	return tasks.TaskHandle{ID: id, Reused: false}, nil
}

// SpawnTool implements tasks.TaskRegistry. It reshapes the tool request
// onto the Spawn path so the FSM, idempotency, and parent-graph
// bookkeeping share one code path; the task persists at StatusPending
// and emits task.spawned. Tool arguments are not carried on the Task
// (that field is reserved for the tool-catalog wiring); the description
// captures the intent for the lifecycle log. A run-loop driver advances
// the task (MarkRunning / MarkComplete / MarkFailed) once it dispatches
// the tool.
func (e *Engine) SpawnTool(ctx context.Context, req tasks.SpawnToolRequest) (tasks.TaskHandle, error) {
	if err := tasks.ValidateToolRequest(req); err != nil {
		return tasks.TaskHandle{}, err
	}
	// Re-shape onto the SpawnRequest path so the FSM, idempotency,
	// and parent-graph bookkeeping share one code path. Tool args
	// are NOT carried on Task (the schema is reserved
	// for the tool catalog wiring); the description captures
	// the intent for the lifecycle log.
	desc := req.Description
	if desc == "" {
		desc = fmt.Sprintf("tool: %s", req.ToolName)
	}
	return e.Spawn(ctx, tasks.SpawnRequest{
		Identity:          req.Identity,
		Kind:              tasks.KindForeground,
		ParentTaskID:      req.ParentTaskID,
		Description:       desc,
		Query:             string(req.ToolArgs),
		Priority:          req.Priority,
		IdempotencyKey:    req.IdempotencyKey,
		PropagateOnCancel: req.PropagateOnCancel,
		NotifyOnComplete:  req.NotifyOnComplete,
		GroupID:           req.GroupID,
	})
}

// Get implements tasks.TaskRegistry.
//
// Identity-mandatory: the ctx Identity is read and verified against
// the stored task's Identity. Cross-tenant / cross-session reads are
// rejected with ErrNotFound (the task is invisible from outside its
// scope; we do NOT leak existence-without-access).
func (e *Engine) Get(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	if e.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if !identityVisible(ident, t.Identity) {
		// Cross-tenant / cross-session read attempted. Return
		// ErrNotFound (we do not surface existence-without-access).
		return nil, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	cp := *t
	if t.Result != nil {
		r := *t.Result
		cp.Result = &r
	}
	if t.Error != nil {
		errCopy := *t.Error
		cp.Error = &errCopy
	}
	if t.ParentTaskID != nil {
		p := *t.ParentTaskID
		cp.ParentTaskID = &p
	}
	return &cp, nil
}

// List implements tasks.TaskRegistry. Filters by session and the
// optional fields on `f`. Returns task summaries in arbitrary order
// does not promise iteration order; may.
func (e *Engine) List(ctx context.Context, sessionID identity.Identity, f tasks.TaskFilter) ([]tasks.TaskSummary, error) {
	if e.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return nil, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]tasks.TaskSummary, 0, 8)
	for _, t := range e.tasks {
		if t.Identity.TenantID != sessionID.TenantID {
			continue
		}
		if t.Identity.UserID != sessionID.UserID {
			continue
		}
		if t.Identity.SessionID != sessionID.SessionID {
			continue
		}
		if f.Status != nil && t.Status != *f.Status {
			continue
		}
		if f.Kind != nil && t.Kind != *f.Kind {
			continue
		}
		if f.ParentID != nil {
			if t.ParentTaskID == nil || *t.ParentTaskID != *f.ParentID {
				continue
			}
		}
		out = append(out, tasks.TaskSummary{
			ID:        t.ID,
			Status:    t.Status,
			Kind:      t.Kind,
			Priority:  t.Priority,
			CreatedAt: t.CreatedAt,
			UpdatedAt: t.UpdatedAt,
		})
	}
	return out, nil
}

// ListTenant implements tasks.TaskRegistry — the admin-widened fleet
// read. It scans the engine's in-memory task map and returns FULL Task
// copies for every task whose tenant matches `tenantID`, across ALL
// (user, session) scopes, honouring the optional `f` facets.
//
// The scan is COMPLETE without a StateStore scan: the engine's in-memory
// `tasks` map holds every live task in this process (both drivers hydrate
// their backend into it on open), so a tenant filter over the map yields
// the whole per-runtime fleet view. Cross-RUNTIME federation is the
// coordinator's job over per-runtime reads (the same division as sessions
// / events) — Harbor ships the per-runtime widened read.
//
// Unlike List, ListTenant takes an EXPLICIT tenant argument rather than
// reading a mandatory triple from ctx: it is the admin-gated fleet read
// and the Protocol service is its only production caller (it gates on the
// verified `auth.ScopeAdmin` claim before invoking). An empty `tenantID`
// fails loud with tasks.ErrInvalidRequest — a fleet read never dumps the
// whole store.
func (e *Engine) ListTenant(_ context.Context, tenantID string, f tasks.TaskFilter) ([]*tasks.Task, error) {
	if e.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: ListTenant requires a non-empty tenant id", tasks.ErrInvalidRequest)
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*tasks.Task, 0, 8)
	for _, t := range e.tasks {
		if t.Identity.TenantID != tenantID {
			continue
		}
		if f.Status != nil && t.Status != *f.Status {
			continue
		}
		if f.Kind != nil && t.Kind != *f.Kind {
			continue
		}
		if f.ParentID != nil {
			if t.ParentTaskID == nil || *t.ParentTaskID != *f.ParentID {
				continue
			}
		}
		out = append(out, copyTask(t))
	}
	return out, nil
}

// OldestRetainedAt reports the OBSERVED oldest task-creation instant
// across the WHOLE retained task set — every (tenant, user, session) in
// this runtime — as the identity-free runtime-wide retention horizon a
// fleet-observe caller reads on `runtime.health`. It is the runtime-wide
// analogue of events.RetentionReporter, discovered by type assertion at
// the posture wiring seam (no `Supports*` capability protocol — CLAUDE.md
// §4.4). The returned instant carries no per-task content — only the bare
// oldest CreatedAt — so it is safe to expose runtime-wide, never a
// cross-tenant enumeration.
//
// Returns (zero, false, nil) when the engine retains no tasks (present is
// false), and (zero, false, ErrRegistryClosed) after Close. The scan is
// COMPLETE without a StateStore scan: both drivers hydrate their backend
// into the in-memory `tasks` map on open, so the map holds every live
// task in this process. The read is an O(N) oldest-head fold under the
// RLock, never a wire-crossing enumeration.
func (e *Engine) OldestRetainedAt(_ context.Context) (time.Time, bool, error) {
	if e.closed.Load() {
		return time.Time{}, false, tasks.ErrRegistryClosed
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var oldest int64
	for _, t := range e.tasks {
		if t.CreatedAt == 0 {
			continue
		}
		if oldest == 0 || t.CreatedAt < oldest {
			oldest = t.CreatedAt
		}
	}
	if oldest == 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(0, oldest).UTC(), true, nil
}

// copyTask returns a deep copy of t so a caller cannot mutate the
// engine's live record (mirrors Get's copy discipline). The pointer
// fields (Result / Error / ParentTaskID) are cloned; the immutable-slice
// fields alias the stored slices (the registry never mutates them
// in-place after Spawn).
func copyTask(t *tasks.Task) *tasks.Task {
	cp := *t
	if t.Result != nil {
		r := *t.Result
		cp.Result = &r
	}
	if t.Error != nil {
		errCopy := *t.Error
		cp.Error = &errCopy
	}
	if t.ParentTaskID != nil {
		p := *t.ParentTaskID
		cp.ParentTaskID = &p
	}
	return &cp
}

// Cancel implements tasks.TaskRegistry. The target transitions
// unconditionally (a direct cancel is never gated on the target's own
// PropagateOnCancel); the descendant walk then depends on the target's
// PropagateOnCancel:
//
//   - "cascade" (default): BFS through descendants, transitioning each
//     non-terminal descendant to StatusCancelled — EXCEPT an
//     isolate-marked descendant, whose whole subtree detaches from the
//     cascade and keeps running (see cascadeCancelDescendantsLocked).
//   - "isolate": only the target transitions; no descendant walk.
//
// So an isolate-marked task survives an ANCESTOR's cascade but is always
// reachable by a direct Cancel (the operator's last word, and the
// spawning run's own CancelTask).
//
// Already-terminal target → (false, nil). Missing target →
// ErrNotFound.
func (e *Engine) Cancel(ctx context.Context, id tasks.TaskID, reason string) (bool, error) {
	if e.closed.Load() {
		return false, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return false, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return false, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if !identityVisible(ident, t.Identity) {
		return false, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if isTerminal(t.Status) {
		return false, nil
	}

	// Cancel target.
	if err := e.transitionLocked(ctx, t, tasks.StatusCancelled); err != nil {
		return false, err
	}
	if err := e.publish(ctx, t, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
		TaskID:   t.ID,
		Reason:   reason,
		Cascaded: false,
	}); err != nil {
		return false, err
	}

	// Cascade to descendants per the target's policy. The direct target
	// was already transitioned above (a direct cancel is never gated on
	// the target's own flag); the descendant walk honours EACH
	// descendant's own PropagateOnCancel so an isolate-marked subtree
	// detaches from this ancestor's cascade.
	if t.PropagateOnCancel == tasks.PropagateCascade {
		if err := e.cascadeCancelDescendantsLocked(ctx, t.ID, reason); err != nil {
			return true, err
		}
	}

	return true, nil
}

// cascadeCancelDescendantsLocked walks the descendant subtree rooted at
// rootID in BFS order, cancelling each descendant whose own
// PropagateOnCancel is cascade (the stored default) and enqueuing that
// descendant's children. A descendant marked PropagateIsolate DETACHES
// from the ancestor's cascade: it is neither cancelled NOR are its
// children enqueued, so the whole isolate-marked subtree survives the
// ancestor's cancellation.
//
// This governs ONLY descendants reached mid-walk. The direct cancel
// target (rootID) is transitioned unconditionally by the caller BEFORE
// the walk starts, and a direct Cancel on an isolate-marked task still
// transitions it — isolate detaches a task from an ANCESTOR's cascade,
// never from a direct cancel by the operator or by the run that spawned
// it. Both Cancel and the group-cancel path share this one walk so the
// isolate-detaches invariant holds identically at every cascade site.
//
// Caller MUST hold e.mu. A transition/publish error stops the walk and
// is returned; the walk is bounded by the finite task set (terminal and
// isolate nodes are never re-enqueued).
func (e *Engine) cascadeCancelDescendantsLocked(ctx context.Context, rootID tasks.TaskID, reason string) error {
	queue := append([]tasks.TaskID(nil), e.children[rootID]...)
	for len(queue) > 0 {
		childID := queue[0]
		queue = queue[1:]
		child, ok := e.tasks[childID]
		if !ok {
			continue
		}
		if isTerminal(child.Status) {
			continue
		}
		// An isolate-marked descendant detaches from the ancestor's
		// cascade: skip it AND its whole subtree (do not enqueue its
		// children). A direct Cancel on this task still reaches it via
		// the public Cancel path, which transitions the direct target
		// unconditionally.
		if child.PropagateOnCancel == tasks.PropagateIsolate {
			continue
		}
		if err := e.transitionLocked(ctx, child, tasks.StatusCancelled); err != nil {
			return err
		}
		if err := e.publish(ctx, child, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
			TaskID:   child.ID,
			Reason:   reason,
			Cascaded: true,
		}); err != nil {
			return err
		}
		queue = append(queue, e.children[childID]...)
	}
	return nil
}

// Prioritize implements tasks.TaskRegistry. Stores the new value;
// does NOT preempt or reorder execution (the "scheduling is
// runtime engine's concern" line). Emits task.prioritised.
func (e *Engine) Prioritize(ctx context.Context, id tasks.TaskID, priority int) (bool, error) {
	if e.closed.Load() {
		return false, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return false, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return false, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if !identityVisible(ident, t.Identity) {
		return false, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if t.Priority == priority {
		return false, nil
	}
	prior := t.Priority
	priorUpdated := t.UpdatedAt
	t.Priority = priority
	t.UpdatedAt = time.Now().UnixNano()
	if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		// Keep the in-memory record consistent with the store on a
		// persist failure.
		t.Priority = prior
		t.UpdatedAt = priorUpdated
		return false, err
	}
	if err := e.publish(ctx, t, tasks.EventTypeTaskPrioritised, tasks.TaskPrioritisedPayload{
		TaskID:        t.ID,
		PriorPriority: prior,
		NewPriority:   priority,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// MarkRunning implements tasks.TaskRegistry.
func (e *Engine) MarkRunning(ctx context.Context, id tasks.TaskID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	prior := t.Status
	if err := e.transitionLocked(ctx, t, tasks.StatusRunning); err != nil {
		return err
	}
	return e.publish(ctx, t, tasks.EventTypeTaskStarted, tasks.TaskStartedPayload{
		TaskID:     t.ID,
		PriorState: prior,
	})
}

// MarkPaused implements tasks.TaskRegistry.
func (e *Engine) MarkPaused(ctx context.Context, id tasks.TaskID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if err := e.transitionLocked(ctx, t, tasks.StatusPaused); err != nil {
		return err
	}
	return e.publish(ctx, t, tasks.EventTypeTaskPaused, tasks.TaskPausedPayload{TaskID: t.ID})
}

// MarkResumed implements tasks.TaskRegistry. Distinct method (vs
// MarkRunning) so the bus event can be `task.resumed` rather than
// `task.started`. The FSM transition is identical to MarkRunning's
// Paused → Running edge.
func (e *Engine) MarkResumed(ctx context.Context, id tasks.TaskID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != tasks.StatusPaused {
		return fmt.Errorf("%w: from=%q to=%q (resume only valid from paused)",
			tasks.ErrInvalidTransition, t.Status, tasks.StatusRunning)
	}
	if err := e.transitionLocked(ctx, t, tasks.StatusRunning); err != nil {
		return err
	}
	return e.publish(ctx, t, tasks.EventTypeTaskResumed, tasks.TaskResumedPayload{TaskID: t.ID})
}

// MarkComplete implements tasks.TaskRegistry. Persists `result` on
// the Task record (after caller-side redaction; the caller
// is responsible for redacting Value before passing it in, but we
// run it through the redactor again as a safety net).
func (e *Engine) MarkComplete(ctx context.Context, id tasks.TaskID, result tasks.TaskResult) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	// Pre-flight the FSM transition before mutating any persisted
	// fields. Mutating Result before the FSM check would leave the
	// task in an inconsistent state if the transition is invalid.
	if !isValidTransition(t.Status, tasks.StatusComplete) {
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrInvalidTransition, t.Status, tasks.StatusComplete)
	}
	redactedValue, err := e.redactRawJSON(ctx, result.Value)
	if err != nil {
		return fmt.Errorf("tasks/engine: redact result: %w", err)
	}
	priorResult := t.Result
	t.Result = &tasks.TaskResult{Value: redactedValue}
	if err := e.transitionLocked(ctx, t, tasks.StatusComplete); err != nil {
		// transitionLocked already rolled back status on a persist
		// failure; restore the result it doesn't know about so the
		// in-memory record stays consistent with the store.
		t.Result = priorResult
		return err
	}
	return e.publish(ctx, t, tasks.EventTypeTaskCompleted, tasks.TaskCompletedPayload{TaskID: t.ID})
}

// MarkFailed implements tasks.TaskRegistry. Persists `err` on the
// Task record (after caller-side redaction on the message).
func (e *Engine) MarkFailed(ctx context.Context, id tasks.TaskID, taskErr tasks.TaskError) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	// Pre-flight the FSM transition before mutating Error.
	if !isValidTransition(t.Status, tasks.StatusFailed) {
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrInvalidTransition, t.Status, tasks.StatusFailed)
	}
	redactedMsg, err := e.redactString(ctx, taskErr.Message)
	if err != nil {
		return fmt.Errorf("tasks/engine: redact error message: %w", err)
	}
	priorError := t.Error
	t.Error = &tasks.TaskError{Code: taskErr.Code, Message: redactedMsg}
	if err := e.transitionLocked(ctx, t, tasks.StatusFailed); err != nil {
		t.Error = priorError
		return err
	}
	return e.publish(ctx, t, tasks.EventTypeTaskFailed, tasks.TaskFailedPayload{
		TaskID:    t.ID,
		ErrorCode: taskErr.Code,
	})
}

// IncrementToolCount implements tasks.TaskRegistry.
//
// Atomically increments `t.ToolCount` by 1 under the FSM lock and
// persists the updated record. The lock ensures
// N concurrent calls against the same task yield the correct final
// count — N — without torn writes.
//
// Does NOT advance the FSM status and does NOT emit a bus event:
// per-tool-dispatch traffic is already covered by `task.spawned` /
// `tool.*` events; ToolCount is the cheap rollup the Console reads
// without subscribing to the per-tool stream.
func (e *Engine) IncrementToolCount(ctx context.Context, id tasks.TaskID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	priorUpdated := t.UpdatedAt
	t.ToolCount++
	t.UpdatedAt = time.Now().UnixNano()
	if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		// Keep the in-memory record consistent with the store on a
		// persist failure.
		t.ToolCount--
		t.UpdatedAt = priorUpdated
		return err
	}
	return nil
}

// Close implements tasks.TaskRegistry. Idempotent.
//
// Drains every still-open `WatchGroup` subscriber and every
// `RegisterRetainTurnWaiter` channel before flipping `closed`. A
// caller blocked on either channel observes the close-on-shutdown
// instead of leaking forever — the public contract is "channel
// closes exactly once: either on resolve or on registry teardown."
// Identity is irrelevant here: we close everything we own.
func (e *Engine) Close(_ context.Context) error {
	e.mu.Lock()
	if e.closed.Load() {
		e.mu.Unlock()
		return nil
	}
	for gid, subs := range e.groupSubs {
		for _, sub := range subs {
			if sub.closed {
				continue
			}
			close(sub.ch)
			sub.closed = true
		}
		delete(e.groupSubs, gid)
	}
	for sess, waiters := range e.retainWaiters {
		for _, w := range waiters {
			if w.closed {
				continue
			}
			close(w.ch)
			w.closed = true
		}
		delete(e.retainWaiters, sess)
	}
	e.closed.Store(true)
	e.mu.Unlock()
	return nil
}

// --- Helpers ----------------------------------------------------------

// lookupLocked finds the task and verifies ctx-identity visibility.
// Caller MUST hold e.mu.
func (e *Engine) lookupLocked(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	if e.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	t, ok := e.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	if !identityVisible(ident, t.Identity) {
		return nil, fmt.Errorf("%w: id=%q", tasks.ErrNotFound, id)
	}
	return t, nil
}

// transitionLocked performs an FSM transition and persists the task.
// When the destination is a terminal state AND the task belongs to a
// group, the group resolve gate is checked (`onMemberTerminalLocked`).
// Caller MUST hold e.mu.
func (e *Engine) transitionLocked(ctx context.Context, t *tasks.Task, to tasks.TaskStatus) error {
	if !isValidTransition(t.Status, to) {
		return fmt.Errorf("%w: from=%q to=%q", tasks.ErrInvalidTransition, t.Status, to)
	}
	// Snapshot the fields we are about to mutate so a persist failure
	// leaves the in-memory record in agreement with the store. Without
	// this, a failed Save would advance in-memory status while the
	// durable record stayed behind — on a durable backend the task
	// would then be un-completable (a retry hits ErrInvalidTransition)
	// and a restart would recover a divergent state.
	priorStatus := t.Status
	priorUpdated := t.UpdatedAt
	t.Status = to
	t.UpdatedAt = time.Now().UnixNano()
	if err := e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		t.Status = priorStatus
		t.UpdatedAt = priorUpdated
		return err
	}
	if isTerminal(to) {
		if err := e.onMemberTerminalLocked(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// persistTaskLocked writes the task through the backend. Caller MUST
// hold e.mu (the Task object is mutated by the caller before the
// call; the lock keeps the persisted bytes in agreement with the
// in-memory snapshot).
//
// hash is the SHA-256 of the task's pre-redaction content (see
// [spawnRequestContentHash]); it is carried on the persisted record
// so a durable backend can rebuild the idempotency index across a
// restart without re-deriving it from the (post-redaction) stored
// fields, which would be lossy whenever the redactor erased
// caller-controlled tokens. A task with no IdempotencyKey carries the
// zero hash (it is never deduped).
func (e *Engine) persistTaskLocked(ctx context.Context, t *tasks.Task, hash [32]byte) error {
	if err := e.backend.SaveTask(ctx, TaskRecord{Task: t, ContentHash: hash}); err != nil {
		return fmt.Errorf("tasks/engine: persist task: %w", err)
	}
	return nil
}

// contentHashLocked returns the idempotency content hash recorded for
// t at Spawn, or the zero hash when t has no IdempotencyKey (and is
// therefore never deduped). Caller MUST hold e.mu.
func (e *Engine) contentHashLocked(t *tasks.Task) [32]byte {
	if t.IdempotencyKey == "" {
		return [32]byte{}
	}
	if rec, ok := e.idemIdx[idempotencyKey{SessionID: t.Identity.SessionID, Key: t.IdempotencyKey}]; ok {
		return rec.ContentHash
	}
	return [32]byte{}
}

// publish emits a typed lifecycle event. Returns the bus error
// directly so callers can propagate the failure to the operator.
func (e *Engine) publish(ctx context.Context, t *tasks.Task, evType events.EventType, payload events.EventPayload) error {
	return e.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: t.Identity,
		Payload:  payload,
	})
}

// redactSpawnFields redacts Description and Query through the
// configured Redactor. Each field is redacted independently so that
// secret-shaped substrings in either are caught.
func (e *Engine) redactSpawnFields(ctx context.Context, desc, query string) (string, string, error) {
	rd, err := e.redactString(ctx, desc)
	if err != nil {
		return "", "", err
	}
	rq, err := e.redactString(ctx, query)
	if err != nil {
		return "", "", err
	}
	return rd, rq, nil
}

// redactString runs a string through the configured Redactor by
// wrapping it in a `map[string]any` (the redactor's reflective walk
// returns map[string]any for structs; passing a map directly skips
// the struct-to-map step). Empty strings short-circuit; the
// redactor MUST NOT replace empty input.
func (e *Engine) redactString(ctx context.Context, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	out, err := e.redactor.Redact(ctx, map[string]any{"v": s})
	if err != nil {
		return "", err
	}
	m, ok := out.(map[string]any)
	if !ok {
		// Defensive: a redactor returning a non-map shape on a
		// map input is a driver-shape bug. Surface loudly.
		return "", fmt.Errorf("tasks/engine: redactor returned %T, want map[string]any", out)
	}
	v, ok := m["v"].(string)
	if !ok {
		// The redactor replaced the value with a non-string (e.g.
		// the canonical "[REDACTED]" placeholder is still a string,
		// but a buggy redactor could return an int). Coerce via fmt.
		return fmt.Sprintf("%v", m["v"]), nil
	}
	return v, nil
}

// redactRawJSON runs a JSON-encoded value through the Redactor by
// decoding it, walking it, then re-encoding. The redactor's
// reflective walk handles map[string]any natively.
//
// Empty / nil RawMessage short-circuits.
func (e *Engine) redactRawJSON(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var decoded any
	if jerr := json.Unmarshal(raw, &decoded); jerr != nil {
		// If the value isn't JSON, treat it as an opaque string and
		// run it through the redactor. This is defensive — the
		// caller's contract is that Value is JSON, but we'd rather
		// redact-then-pass than fail loudly on malformed bytes that
		// the caller may have intentionally sent (e.g. a tool that
		// emits raw text alongside JSON in a future wiring).
		_ = jerr // intentionally swallow; defensive fall-through is documented above.
		redacted, rerr := e.redactString(ctx, string(raw))
		if rerr != nil {
			return nil, rerr
		}
		quoted, qerr := json.Marshal(redacted)
		if qerr != nil {
			return nil, fmt.Errorf("tasks/engine: requote redacted value: %w", qerr)
		}
		return quoted, nil
	}
	out, err := e.redactor.Redact(ctx, decoded)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("tasks/engine: marshal redacted value: %w", err)
	}
	return encoded, nil
}

// spawnRequestsEqual reports whether an existing Task and a fresh
// SpawnRequest agree on every field that contributes to the task's
// identity. Used to detect ErrIdempotencyConflict.
//
// `propagate` is the resolved (default-applied) value the caller is
// asking for; `existing` is compared against it directly because the
// stored Task already has the resolved value.
//
// `existingHash` is the SHA-256 captured at the original Spawn (from
// `idempotencyRecord.ContentHash`); `reqHash` is the SHA-256 of the
// current request's pre-redaction Description+Query bytes. Comparing
// hashes catches the divergence the redactor would have erased — a
// caller that resends the same key with new bytes hits the conflict
// path even when the redactor produces identical post-redaction
// strings (e.g. both inputs contain only secret-shaped tokens).
func spawnRequestsEqual(existing *tasks.Task, existingHash [32]byte, req tasks.SpawnRequest, reqHash [32]byte, propagate string) bool {
	if existing.Identity != req.Identity {
		return false
	}
	if existing.Kind != req.Kind {
		return false
	}
	if existing.Priority != req.Priority {
		return false
	}
	if !taskIDPtrEqual(existing.ParentTaskID, req.ParentTaskID) {
		return false
	}
	if existingHash != reqHash {
		return false
	}
	if existing.PropagateOnCancel != propagate {
		return false
	}
	if existing.NotifyOnComplete != req.NotifyOnComplete {
		return false
	}
	if existing.IdempotencyKey != req.IdempotencyKey {
		return false
	}
	// input artifact attachments are part of the
	// task's content identity. Same key, different attachments → conflict.
	if !stringSliceEqual(existing.InputArtifactIDs, req.InputArtifactIDs) {
		return false
	}
	// disposition hints are part of the task's
	// content identity too. Same key, same attachments, different
	// dispositions → conflict.
	if !stringMapEqual(existing.InputArtifactDispositions, req.InputArtifactDispositions) {
		return false
	}
	// the output schema is part of the task's content identity: a reused
	// idempotency key carrying a DIFFERENT output_schema is caller misuse
	// and must surface as a loud conflict, not silently adopt the original
	// schema. Same key, same body, same schema → a safe genuine retry.
	if !bytes.Equal(existing.OutputSchema, req.OutputSchema) {
		return false
	}
	return true
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// spawnRequestContentHash returns a SHA-256 of the pre-redaction
// caller-controlled string fields on req (Description + Query). The
// hash is computed BEFORE `audit.Redactor.Redact` runs so divergent
// inputs whose redacted forms collide (e.g. both consist entirely of
// secret-shaped tokens that the redactor erases identically) are
// still detected as conflicts under the same IdempotencyKey.
//
// The separator byte (0x1F — ASCII Unit Separator) prevents
// preimage attacks where two distinct (Description, Query) pairs
// concatenate to the same byte sequence ("ab" + "c" vs "a" + "bc").
func spawnRequestContentHash(req tasks.SpawnRequest) [32]byte {
	h := sha256.New()
	h.Write([]byte(req.Description))
	h.Write([]byte{0x1F})
	h.Write([]byte(req.Query))
	// fold InputArtifactIDs into the hash so
	// "same key, different attachments" surfaces as ErrIdempotencyConflict.
	for _, id := range req.InputArtifactIDs {
		h.Write([]byte{0x1F})
		h.Write([]byte(id))
	}
	// fold the disposition hints in deterministic
	// (sorted-key) order so "same attachments, different dispositions"
	// also surfaces as ErrIdempotencyConflict.
	if len(req.InputArtifactDispositions) > 0 {
		keys := make([]string, 0, len(req.InputArtifactDispositions))
		for k := range req.InputArtifactDispositions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte{0x1F})
			h.Write([]byte(k))
			h.Write([]byte{0x1E})
			h.Write([]byte(req.InputArtifactDispositions[k]))
		}
	}
	// fold the output schema in so "same key, different output_schema"
	// surfaces as ErrIdempotencyConflict.
	if len(req.OutputSchema) > 0 {
		h.Write([]byte{0x1F})
		h.Write(req.OutputSchema)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func taskIDPtrEqual(a, b *tasks.TaskID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// isValidTransition is the FSM table. Each (from, to) pair is checked
// before any state mutation; invalid transitions return
// ErrInvalidTransition.
//
// Allowed edges:
//
//	Pending  → Running, Cancelled
//	Running  → Paused,  Complete, Failed, Cancelled
//	Paused   → Running, Cancelled
//	Complete (terminal — no edges out)
//	Failed   (terminal — no edges out)
//	Cancelled(terminal — no edges out)
//
// Same-state transitions are invalid (no idempotent self-edges; the
// runtime engine knows whether a transition is real before calling).
func isValidTransition(from, to tasks.TaskStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case tasks.StatusPending:
		return to == tasks.StatusRunning || to == tasks.StatusCancelled
	case tasks.StatusRunning:
		return to == tasks.StatusPaused || to == tasks.StatusComplete ||
			to == tasks.StatusFailed || to == tasks.StatusCancelled
	case tasks.StatusPaused:
		return to == tasks.StatusRunning || to == tasks.StatusCancelled
	case tasks.StatusComplete, tasks.StatusFailed, tasks.StatusCancelled:
		// Terminal — no transitions out.
		return false
	default:
		return false
	}
}

func isTerminal(s tasks.TaskStatus) bool {
	return s == tasks.StatusComplete || s == tasks.StatusFailed || s == tasks.StatusCancelled
}

// identityFromCtx pulls the Identity off ctx and validates it. The
// triple is mandatory at every API boundary.
func identityFromCtx(ctx context.Context) (identity.Identity, error) {
	ident, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}, tasks.ErrIdentityRequired
	}
	if ident.TenantID == "" || ident.UserID == "" || ident.SessionID == "" {
		return identity.Identity{}, tasks.ErrIdentityRequired
	}
	return ident, nil
}

// validateListIdentity gates the List call's session-identity
// argument. List is intentionally explicit (vs. implicit ctx) so
// the caller surfaces the session under inspection — a planner may
// have multiple session contexts available simultaneously.
func validateListIdentity(s identity.Identity) error {
	if s.TenantID == "" || s.UserID == "" || s.SessionID == "" {
		return tasks.ErrIdentityRequired
	}
	return nil
}

// identityVisible reports whether a request from `ctxIdent` can see
// a task scoped to `taskIdent`. Cross-tenant + cross-session reads
// return false; same-triple returns true regardless of RunID.
func identityVisible(ctxIdent identity.Identity, taskIdent identity.Quadruple) bool {
	return ctxIdent.TenantID == taskIdent.TenantID &&
		ctxIdent.UserID == taskIdent.UserID &&
		ctxIdent.SessionID == taskIdent.SessionID
}

// Compile-time assertion that driver satisfies tasks.TaskRegistry.
var _ tasks.TaskRegistry = (*Engine)(nil)
