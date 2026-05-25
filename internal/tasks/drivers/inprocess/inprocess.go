// Package inprocess is Harbor's V1 in-process TaskRegistry driver.
// It is the test reference for the conformance suite — every later
// driver (post-V1 durable queue, e.g. NATS or Postgres-as-queue at
// Phase 87) inherits the same suite verbatim.
//
// Internal model:
//
//   - A primary `map[TaskID]*Task` holds the live task state.
//   - A secondary `map[idempotencyKey]TaskID` resolves
//     `(SessionID, IdempotencyKey)` lookups for `Spawn` dedup.
//   - A children index `map[TaskID][]TaskID` powers cascade-cancel
//     BFS without scanning the primary map.
//   - A single `sync.RWMutex` guards all three maps. The driver does
//     no I/O so contention is bounded by Go's map throughput; a
//     finer-grained lock structure would be premature.
//   - Every lifecycle transition writes through `state.StateStore`
//     (the typed-wrapper-over-generic adapter, D-027) and emits a
//     typed `events.EventPayload` on the bus.
//   - Caller-controlled strings (Description, Query, Result.Value,
//     Error.Message) are run through the `audit.Redactor` BEFORE the
//     Save (D-020). The redactor's reflective walk returns a
//     `map[string]any`; we read the redacted strings back and replace
//     them on the Task before marshalling.
//   - `Close(ctx)` flips an atomic flag; subsequent calls return
//     `ErrRegistryClosed`. There are no driver-owned goroutines to
//     join, so Close is fast.
package inprocess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// New constructs a TaskRegistry directly. Exposed for tests that
// want to skip the registry; production callers go through
// `tasks.Open`.
func New(deps tasks.Dependencies) (tasks.TaskRegistry, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("tasks/inprocess: New requires a non-nil StateStore")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("tasks/inprocess: New requires a non-nil EventBus")
	}
	// Redactor is mandatory per D-020. The audit driver is loaded
	// alongside the tasks driver in cmd/harbor; tests that don't
	// care about redaction pass a no-op redactor (see the
	// conformance suite's helper).
	if deps.Redactor == nil {
		return nil, fmt.Errorf("tasks/inprocess: New requires a non-nil Redactor")
	}
	return &driver{
		store:            deps.Store,
		bus:              deps.Bus,
		redactor:         deps.Redactor,
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
	}, nil
}

func init() {
	tasks.Register("inprocess", New)
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

type driver struct {
	store    state.StateStore
	bus      events.EventBus
	redactor audit.Redactor

	mu       sync.RWMutex
	tasks    map[tasks.TaskID]*tasks.Task
	idemIdx  map[idempotencyKey]idempotencyRecord
	children map[tasks.TaskID][]tasks.TaskID

	// Phase 21 — group + patch + retain-turn + watcher state. All
	// guarded by `mu`. See `drivers/inprocess/groups.go` for the
	// access patterns + invariants.
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
func (d *driver) Spawn(ctx context.Context, req tasks.SpawnRequest) (tasks.TaskHandle, error) {
	if d.closed.Load() {
		return tasks.TaskHandle{}, tasks.ErrRegistryClosed
	}
	if err := tasks.ValidateRequest(req); err != nil {
		return tasks.TaskHandle{}, err
	}
	propagate := req.PropagateOnCancel
	if propagate == "" {
		propagate = tasks.PropagateCascade
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Idempotency check: same (SessionID, IdempotencyKey) seen?
	// Empty IdempotencyKey disables dedup (every Spawn yields a fresh
	// handle).
	contentHash := spawnRequestContentHash(req)
	if req.IdempotencyKey != "" {
		idemK := idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey}
		if existing, ok := d.idemIdx[idemK]; ok {
			stored := d.tasks[existing.TaskID]
			if !spawnRequestsEqual(stored, existing.ContentHash, req, contentHash, propagate) {
				return tasks.TaskHandle{}, fmt.Errorf("%w: key=%q",
					tasks.ErrIdempotencyConflict, req.IdempotencyKey)
			}
			return tasks.TaskHandle{ID: existing.TaskID, Reused: true}, nil
		}
	}

	// New task: assign ULID, persist, emit task.spawned.
	id := tasks.TaskID(ulid.MustNew(ulid.Now(), d.ulidEntropy).String())
	now := time.Now().UnixNano()

	// Caller-side audit redaction on the user-controlled strings
	// (D-020). Description / Query are the inputs at Spawn; Result /
	// Error are populated later by Mark*.
	redactedDesc, redactedQuery, err := d.redactSpawnFields(ctx, req.Description, req.Query)
	if err != nil {
		return tasks.TaskHandle{}, fmt.Errorf("tasks/inprocess: redact: %w", err)
	}

	// Defensive copy of the input-artifact ID slice — protects the
	// stored task from caller-side mutation of the SpawnRequest. Nil
	// stays nil so the JSON `omitempty` tag elides the field for
	// text-only spawns.
	var inputArtifactIDs []string
	if len(req.InputArtifactIDs) > 0 {
		inputArtifactIDs = append([]string(nil), req.InputArtifactIDs...)
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
	}
	if err := d.persistLocked(ctx, t); err != nil {
		return tasks.TaskHandle{}, err
	}
	d.tasks[id] = t
	if req.IdempotencyKey != "" {
		d.idemIdx[idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey}] = idempotencyRecord{
			TaskID:      id,
			ContentHash: contentHash,
		}
	}
	if req.ParentTaskID != nil && *req.ParentTaskID != "" {
		d.children[*req.ParentTaskID] = append(d.children[*req.ParentTaskID], id)
	}
	// Phase 21: wire the new task into the requested group, if any.
	// Sealed / terminal groups reject with ErrGroupSealed. The error
	// rolls back the spawn: we delete the just-created task to keep
	// the registry consistent.
	if req.GroupID != "" {
		g, ok := d.groups[req.GroupID]
		if !ok || !identitiesEqual(g.SessionID, req.Identity.Identity) {
			delete(d.tasks, id)
			if req.IdempotencyKey != "" {
				delete(d.idemIdx, idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey})
			}
			return tasks.TaskHandle{}, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, req.GroupID)
		}
		if err := d.addMemberLocked(g, id); err != nil {
			delete(d.tasks, id)
			if req.IdempotencyKey != "" {
				delete(d.idemIdx, idempotencyKey{SessionID: req.Identity.SessionID, Key: req.IdempotencyKey})
			}
			return tasks.TaskHandle{}, err
		}
		if err := d.persistGroupLocked(ctx, g); err != nil {
			delete(d.tasks, id)
			delete(d.taskGroup, id)
			return tasks.TaskHandle{}, err
		}
	}

	parentForPayload := tasks.TaskID("")
	if req.ParentTaskID != nil {
		parentForPayload = *req.ParentTaskID
	}
	if err := d.publish(ctx, t, tasks.EventTypeTaskSpawned, tasks.TaskSpawnedPayload{
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

// SpawnTool implements tasks.TaskRegistry.
//
// Phase 20's body is a deliberate stub: we persist a foreground task
// at StatusPending and emit task.spawned. Tool dispatch wiring lands
// at Phase 26 — the runtime engine will drive MarkRunning /
// MarkComplete / MarkFailed once the dispatcher is wired. Documented
// inline + flagged in the smoke script's PR body.
func (d *driver) SpawnTool(ctx context.Context, req tasks.SpawnToolRequest) (tasks.TaskHandle, error) {
	if err := tasks.ValidateToolRequest(req); err != nil {
		return tasks.TaskHandle{}, err
	}
	// Re-shape onto the SpawnRequest path so the FSM, idempotency,
	// and parent-graph bookkeeping share one code path. Tool args
	// are NOT carried on Task at Phase 20 (the schema is reserved
	// for Phase 26's tool catalog wiring); the description captures
	// the intent for the lifecycle log.
	desc := req.Description
	if desc == "" {
		desc = fmt.Sprintf("tool: %s", req.ToolName)
	}
	return d.Spawn(ctx, tasks.SpawnRequest{
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
func (d *driver) Get(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.tasks[id]
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
		e := *t.Error
		cp.Error = &e
	}
	if t.ParentTaskID != nil {
		p := *t.ParentTaskID
		cp.ParentTaskID = &p
	}
	return &cp, nil
}

// List implements tasks.TaskRegistry. Filters by session and the
// optional fields on `f`. Returns task summaries in arbitrary order
// — Phase 20 does not promise iteration order; Phase 21 may.
func (d *driver) List(ctx context.Context, sessionID identity.Identity, f tasks.TaskFilter) ([]tasks.TaskSummary, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]tasks.TaskSummary, 0, 8)
	for _, t := range d.tasks {
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
			UpdatedAt: t.UpdatedAt,
		})
	}
	return out, nil
}

// Cancel implements tasks.TaskRegistry. Walks the children index per
// the target task's PropagateOnCancel:
//
//   - "cascade" (default): BFS through descendants, transitioning
//     each non-terminal child to StatusCancelled.
//   - "isolate": only the target transitions.
//
// Already-terminal target → (false, nil). Missing target →
// ErrNotFound.
func (d *driver) Cancel(ctx context.Context, id tasks.TaskID, reason string) (bool, error) {
	if d.closed.Load() {
		return false, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return false, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tasks[id]
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
	if err := d.transitionLocked(ctx, t, tasks.StatusCancelled); err != nil {
		return false, err
	}
	if err := d.publish(ctx, t, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
		TaskID:   t.ID,
		Reason:   reason,
		Cascaded: false,
	}); err != nil {
		return false, err
	}

	// Cascade to descendants per the target's policy.
	if t.PropagateOnCancel == tasks.PropagateCascade {
		// BFS: queue starts with the target's direct children.
		queue := append([]tasks.TaskID(nil), d.children[t.ID]...)
		for len(queue) > 0 {
			childID := queue[0]
			queue = queue[1:]
			child, ok := d.tasks[childID]
			if !ok {
				continue
			}
			if isTerminal(child.Status) {
				continue
			}
			if err := d.transitionLocked(ctx, child, tasks.StatusCancelled); err != nil {
				return true, err
			}
			if err := d.publish(ctx, child, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
				TaskID:   child.ID,
				Reason:   reason,
				Cascaded: true,
			}); err != nil {
				return true, err
			}
			// Enqueue grandchildren regardless of their own
			// PropagateOnCancel — once a parent cascade has reached
			// them, the cascade stops at terminal-status checks, not
			// at policy boundaries. (Brief 05 §4: cascade is a parent-
			// initiated walk; child policy governs the child's own
			// Cancel call, not its parent's cascade.)
			queue = append(queue, d.children[childID]...)
		}
	}

	return true, nil
}

// Prioritize implements tasks.TaskRegistry. Stores the new value;
// does NOT preempt or reorder execution (D-001's "scheduling is
// runtime engine's concern" line). Emits task.prioritised.
func (d *driver) Prioritize(ctx context.Context, id tasks.TaskID, priority int) (bool, error) {
	if d.closed.Load() {
		return false, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return false, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tasks[id]
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
	t.Priority = priority
	t.UpdatedAt = time.Now().UnixNano()
	if err := d.persistLocked(ctx, t); err != nil {
		return false, err
	}
	if err := d.publish(ctx, t, tasks.EventTypeTaskPrioritised, tasks.TaskPrioritisedPayload{
		TaskID:        t.ID,
		PriorPriority: prior,
		NewPriority:   priority,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// MarkRunning implements tasks.TaskRegistry.
func (d *driver) MarkRunning(ctx context.Context, id tasks.TaskID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	prior := t.Status
	if err := d.transitionLocked(ctx, t, tasks.StatusRunning); err != nil {
		return err
	}
	return d.publish(ctx, t, tasks.EventTypeTaskStarted, tasks.TaskStartedPayload{
		TaskID:     t.ID,
		PriorState: prior,
	})
}

// MarkPaused implements tasks.TaskRegistry.
func (d *driver) MarkPaused(ctx context.Context, id tasks.TaskID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if err := d.transitionLocked(ctx, t, tasks.StatusPaused); err != nil {
		return err
	}
	return d.publish(ctx, t, tasks.EventTypeTaskPaused, tasks.TaskPausedPayload{TaskID: t.ID})
}

// MarkResumed implements tasks.TaskRegistry. Distinct method (vs
// MarkRunning) so the bus event can be `task.resumed` rather than
// `task.started`. The FSM transition is identical to MarkRunning's
// Paused → Running edge.
func (d *driver) MarkResumed(ctx context.Context, id tasks.TaskID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != tasks.StatusPaused {
		return fmt.Errorf("%w: from=%q to=%q (resume only valid from paused)",
			tasks.ErrInvalidTransition, t.Status, tasks.StatusRunning)
	}
	if err := d.transitionLocked(ctx, t, tasks.StatusRunning); err != nil {
		return err
	}
	return d.publish(ctx, t, tasks.EventTypeTaskResumed, tasks.TaskResumedPayload{TaskID: t.ID})
}

// MarkComplete implements tasks.TaskRegistry. Persists `result` on
// the Task record (after caller-side redaction; per D-020 the caller
// is responsible for redacting Value before passing it in, but we
// run it through the redactor again as a safety net).
func (d *driver) MarkComplete(ctx context.Context, id tasks.TaskID, result tasks.TaskResult) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
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
	redactedValue, err := d.redactRawJSON(ctx, result.Value)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: redact result: %w", err)
	}
	t.Result = &tasks.TaskResult{Value: redactedValue}
	if err := d.transitionLocked(ctx, t, tasks.StatusComplete); err != nil {
		return err
	}
	return d.publish(ctx, t, tasks.EventTypeTaskCompleted, tasks.TaskCompletedPayload{TaskID: t.ID})
}

// MarkFailed implements tasks.TaskRegistry. Persists `err` on the
// Task record (after caller-side redaction on the message).
func (d *driver) MarkFailed(ctx context.Context, id tasks.TaskID, taskErr tasks.TaskError) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	// Pre-flight the FSM transition before mutating Error.
	if !isValidTransition(t.Status, tasks.StatusFailed) {
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrInvalidTransition, t.Status, tasks.StatusFailed)
	}
	redactedMsg, err := d.redactString(ctx, taskErr.Message)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: redact error message: %w", err)
	}
	t.Error = &tasks.TaskError{Code: taskErr.Code, Message: redactedMsg}
	if err := d.transitionLocked(ctx, t, tasks.StatusFailed); err != nil {
		return err
	}
	return d.publish(ctx, t, tasks.EventTypeTaskFailed, tasks.TaskFailedPayload{
		TaskID:    t.ID,
		ErrorCode: taskErr.Code,
	})
}

// IncrementToolCount implements tasks.TaskRegistry.
//
// Atomically increments `t.ToolCount` by 1 under the FSM lock and
// persists the updated record (Phase 83m item 7). The lock ensures
// N concurrent calls against the same task yield the correct final
// count — N — without torn writes.
//
// Does NOT advance the FSM status and does NOT emit a bus event:
// per-tool-dispatch traffic is already covered by `task.spawned` /
// `tool.*` events; ToolCount is the cheap rollup the Console reads
// without subscribing to the per-tool stream.
func (d *driver) IncrementToolCount(ctx context.Context, id tasks.TaskID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	t.ToolCount++
	t.UpdatedAt = time.Now().UnixNano()
	if err := d.persistLocked(ctx, t); err != nil {
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
func (d *driver) Close(_ context.Context) error {
	d.mu.Lock()
	if d.closed.Load() {
		d.mu.Unlock()
		return nil
	}
	for gid, subs := range d.groupSubs {
		for _, sub := range subs {
			if sub.closed {
				continue
			}
			close(sub.ch)
			sub.closed = true
		}
		delete(d.groupSubs, gid)
	}
	for sess, waiters := range d.retainWaiters {
		for _, w := range waiters {
			if w.closed {
				continue
			}
			close(w.ch)
			w.closed = true
		}
		delete(d.retainWaiters, sess)
	}
	d.closed.Store(true)
	d.mu.Unlock()
	return nil
}

// --- Helpers ----------------------------------------------------------

// lookupLocked finds the task and verifies ctx-identity visibility.
// Caller MUST hold d.mu.
func (d *driver) lookupLocked(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	ident, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	t, ok := d.tasks[id]
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
// group, the Phase 21 group resolve gate is checked (`onMemberTerminalLocked`).
// Caller MUST hold d.mu.
func (d *driver) transitionLocked(ctx context.Context, t *tasks.Task, to tasks.TaskStatus) error {
	if !isValidTransition(t.Status, to) {
		return fmt.Errorf("%w: from=%q to=%q", tasks.ErrInvalidTransition, t.Status, to)
	}
	t.Status = to
	t.UpdatedAt = time.Now().UnixNano()
	if err := d.persistLocked(ctx, t); err != nil {
		return err
	}
	if isTerminal(to) {
		if err := d.onMemberTerminalLocked(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// persistLocked writes the task through the StateStore as a
// `task.lifecycle` record. Caller MUST hold d.mu (the Task object
// is mutated by the caller before the call; the lock keeps the
// marshalled bytes in agreement with the in-memory snapshot).
func (d *driver) persistLocked(ctx context.Context, t *tasks.Task) error {
	bytesPayload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal task: %w", err)
	}
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: t.Identity,
		Kind:     tasks.LifecycleKind,
		Bytes:    bytesPayload,
		Version:  0,
	}
	if err := d.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("tasks/inprocess: state save: %w", err)
	}
	return nil
}

// publish emits a typed lifecycle event. Returns the bus error
// directly so callers can propagate the failure to the operator.
func (d *driver) publish(ctx context.Context, t *tasks.Task, evType events.EventType, payload events.EventPayload) error {
	return d.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: t.Identity,
		Payload:  payload,
	})
}

// redactSpawnFields redacts Description and Query through the
// configured Redactor. Each field is redacted independently so that
// secret-shaped substrings in either are caught.
func (d *driver) redactSpawnFields(ctx context.Context, desc, query string) (string, string, error) {
	rd, err := d.redactString(ctx, desc)
	if err != nil {
		return "", "", err
	}
	rq, err := d.redactString(ctx, query)
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
func (d *driver) redactString(ctx context.Context, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	out, err := d.redactor.Redact(ctx, map[string]any{"v": s})
	if err != nil {
		return "", err
	}
	m, ok := out.(map[string]any)
	if !ok {
		// Defensive: a redactor returning a non-map shape on a
		// map input is a driver-shape bug. Surface loudly.
		return "", fmt.Errorf("tasks/inprocess: redactor returned %T, want map[string]any", out)
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
func (d *driver) redactRawJSON(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
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
		// emits raw text alongside JSON in a future Phase 26 wiring).
		_ = jerr // intentionally swallow; defensive fall-through is documented above.
		redacted, rerr := d.redactString(ctx, string(raw))
		if rerr != nil {
			return nil, rerr
		}
		quoted, qerr := json.Marshal(redacted)
		if qerr != nil {
			return nil, fmt.Errorf("tasks/inprocess: requote redacted value: %w", qerr)
		}
		return quoted, nil
	}
	out, err := d.redactor.Redact(ctx, decoded)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("tasks/inprocess: marshal redacted value: %w", err)
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
	// Round-7 F11 / D-166 — input artifact attachments are part of the
	// task's content identity. Same key, different attachments → conflict.
	if !stringSliceEqual(existing.InputArtifactIDs, req.InputArtifactIDs) {
		return false
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
	// Round-7 F11 / D-166 — fold InputArtifactIDs into the hash so
	// "same key, different attachments" surfaces as ErrIdempotencyConflict.
	for _, id := range req.InputArtifactIDs {
		h.Write([]byte{0x1F})
		h.Write([]byte(id))
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
// triple is mandatory at every API boundary (D-001).
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
var _ tasks.TaskRegistry = (*driver)(nil)
