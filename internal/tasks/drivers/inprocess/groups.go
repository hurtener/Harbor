package inprocess

// Phase 21 — group governance + retain-turn + patches + WatchGroup
// for the in-process driver. The driver extends the Phase 20
// per-task internal model with three additional maps:
//
//   - `groups[TaskGroupID]*tasks.TaskGroup` — primary group store.
//   - `taskGroup[TaskID]TaskGroupID` — reverse index from member to
//     owning group. Set when a task is added to a group; consulted
//     by the per-task terminal-transition path so the driver knows
//     to check the group's resolve gate.
//   - `groupSubs[TaskGroupID][]chan tasks.GroupCompletion` — list of
//     active `WatchGroup` subscriber channels per group. Cleared on
//     resolve.
//   - `groupCompletions[TaskGroupID]tasks.GroupCompletion` — cached
//     completion payload for resolved-but-still-tracked groups so
//     late `WatchGroup` subscribers receive an already-primed channel.
//   - `retainWaiters[SessionID][]retainWaiter` — per-session
//     retain-turn waiter channels + their group filter.
//   - `patches[patchKey]*tasks.Patch` — primary patch store, keyed
//     by `(SessionID, PatchID)` so the same `patchID` can be reused
//     across sessions without collision.
//   - `acknowledged[TaskID]struct{}` — set of background-task IDs
//     the user has explicitly acknowledged.
//
// All five maps are guarded by the same `sync.RWMutex` the per-task
// state lives behind. The driver does no I/O so contention is
// bounded by Go's map throughput; a finer-grained lock structure
// would be premature.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// patchKey scopes patch IDs by session so the same caller-shaped ID
// can be reused across sessions without collision.
type patchKey struct {
	SessionID string
	PatchID   string
}

// retainWaiter is a single registration on the per-session
// retain-turn waiter list. `ch` is the buffered (size 1) delivery
// channel; `closed` is the close-once guard.
type retainWaiter struct {
	ch     chan tasks.TaskGroupID
	closed bool
}

// groupSubscriber is a single `WatchGroup` registration. `ch` is the
// buffered (size 1) delivery channel; `closed` is the close-once
// guard. The cancel func zeroes the entry's `ch` so the resolve path
// skips delivery.
type groupSubscriber struct {
	ch     chan tasks.GroupCompletion
	closed bool
}

// ResolveOrCreateGroup implements tasks.TaskRegistry. Idempotent on
// (SessionID, GroupID): if a group with the same ID already exists
// AND belongs to the ctx session, returns the existing record
// unchanged. Otherwise creates a fresh group.
func (d *driver) ResolveOrCreateGroup(ctx context.Context, req tasks.GroupRequest) (*tasks.TaskGroup, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if err := validateGroupRequest(req); err != nil {
		return nil, err
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: req.SessionID}) {
		// The session in the request doesn't match the ctx — refuse
		// (cross-session group creation is forbidden).
		return nil, tasks.ErrIdentityRequired
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if req.ID != "" {
		if existing, ok := d.groups[req.ID]; ok {
			if !identitiesEqual(existing.SessionID, req.SessionID) {
				// Existing group belongs to a different session;
				// surface as not-found (we don't leak
				// existence-without-access).
				return nil, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, req.ID)
			}
			cp := *existing
			cp.Members = append([]tasks.TaskID(nil), existing.Members...)
			return &cp, nil
		}
	}

	id := req.ID
	if id == "" {
		id = tasks.TaskGroupID(ulid.MustNew(ulid.Now(), d.ulidEntropy).String())
	}
	now := time.Now()
	g := &tasks.TaskGroup{
		ID:          id,
		SessionID:   req.SessionID,
		OwnerTaskID: req.OwnerTaskID,
		Status:      tasks.GroupOpen,
		RetainTurn:  req.RetainTurn,
		FailFast:    req.FailFast,
		Description: req.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := d.persistGroupLocked(ctx, g); err != nil {
		return nil, err
	}
	d.groups[id] = g

	if err := d.publishGroup(ctx, g, tasks.EventTypeTaskGroupCreated, tasks.TaskGroupCreatedPayload{
		GroupID:     id,
		OwnerTaskID: req.OwnerTaskID,
		RetainTurn:  req.RetainTurn,
		FailFast:    req.FailFast,
		Description: req.Description,
	}); err != nil {
		return nil, err
	}

	cp := *g
	cp.Members = append([]tasks.TaskID(nil), g.Members...)
	return &cp, nil
}

// SealGroup implements tasks.TaskRegistry.
func (d *driver) SealGroup(ctx context.Context, id tasks.TaskGroupID) error {
	if d.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[id]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: g.SessionID}) {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	if !isValidGroupTransition(g.Status, tasks.GroupSealed) {
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrGroupInvalidTransition, g.Status, tasks.GroupSealed)
	}
	g.Status = tasks.GroupSealed
	g.UpdatedAt = time.Now()
	if err := d.persistGroupLocked(ctx, g); err != nil {
		return err
	}
	if err := d.publishGroup(ctx, g, tasks.EventTypeTaskGroupSealed, tasks.TaskGroupSealedPayload{
		GroupID:  g.ID,
		Members:  append([]tasks.TaskID(nil), g.Members...),
		SealedAt: g.UpdatedAt.UnixNano(),
	}); err != nil {
		return err
	}
	// If sealing finds the group already has all members terminal
	// (e.g. members completed before the seal), resolve immediately.
	if d.allMembersTerminalLocked(g) {
		if err := d.resolveGroupLocked(ctx, g, tasks.GroupCompleted, ""); err != nil {
			return err
		}
	}
	return nil
}

// CancelGroup implements tasks.TaskRegistry.
func (d *driver) CancelGroup(ctx context.Context, id tasks.TaskGroupID, reason string, propagate bool) error {
	if d.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[id]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: g.SessionID}) {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	if isGroupTerminal(g.Status) {
		// Idempotent on already-terminal groups.
		return nil
	}

	if propagate {
		// Cancel each non-terminal member. We bypass the per-task
		// Cancel surface (which would re-grab the lock) by walking
		// members directly under the held lock.
		for _, tid := range g.Members {
			t, exists := d.tasks[tid]
			if !exists {
				continue
			}
			if isTerminal(t.Status) {
				continue
			}
			if cerr := d.cancelTaskLocked(ctx, t, reason, true); cerr != nil {
				return cerr
			}
		}
	}

	if err := d.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason); err != nil {
		return err
	}
	return nil
}

// ApplyGroup implements tasks.TaskRegistry. Convenience dispatch.
func (d *driver) ApplyGroup(ctx context.Context, id tasks.TaskGroupID, action tasks.GroupAction) error {
	switch action {
	case tasks.ActionSeal:
		return d.SealGroup(ctx, id)
	case tasks.ActionCancel:
		return d.CancelGroup(ctx, id, "action:cancel", true)
	case tasks.ActionResolve:
		return d.applyResolveAction(ctx, id)
	default:
		return fmt.Errorf("%w: unknown action %q", tasks.ErrGroupInvalidTransition, action)
	}
}

// applyResolveAction handles ActionResolve. Errors with
// `ErrGroupNotSealed` when the group is still Open.
func (d *driver) applyResolveAction(ctx context.Context, id tasks.TaskGroupID) error {
	if d.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[id]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: g.SessionID}) {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, id)
	}
	switch g.Status {
	case tasks.GroupOpen:
		return fmt.Errorf("%w: id=%q (still open)", tasks.ErrGroupNotSealed, id)
	case tasks.GroupSealed:
		return d.resolveGroupLocked(ctx, g, tasks.GroupCompleted, "")
	default:
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrGroupInvalidTransition, g.Status, tasks.GroupCompleted)
	}
}

// ListGroups implements tasks.TaskRegistry.
func (d *driver) ListGroups(ctx context.Context, sessionID identity.Identity, status *tasks.TaskGroupStatus) ([]tasks.TaskGroup, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return nil, err
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	// Cross-session list is forbidden — the ctx identity must match the
	// requested session.
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: sessionID}) {
		return nil, tasks.ErrIdentityRequired
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]tasks.TaskGroup, 0, 4)
	for _, g := range d.groups {
		if !identitiesEqual(g.SessionID, sessionID) {
			continue
		}
		if status != nil && g.Status != *status {
			continue
		}
		cp := *g
		cp.Members = append([]tasks.TaskID(nil), g.Members...)
		out = append(out, cp)
	}
	return out, nil
}

// ApplyPatch implements tasks.TaskRegistry.
func (d *driver) ApplyPatch(ctx context.Context, sessionID identity.Identity, patchID string, action tasks.PatchAction) (bool, error) {
	if d.closed.Load() {
		return false, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return false, err
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return false, err
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: sessionID}) {
		return false, tasks.ErrIdentityRequired
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	key := patchKey{SessionID: sessionID.SessionID, PatchID: patchID}
	p, ok := d.patches[key]
	if !ok {
		return false, fmt.Errorf("%w: id=%q", tasks.ErrPatchNotFound, patchID)
	}
	target := "applied"
	evType := tasks.EventTypeTaskPatchApplied
	if action == tasks.PatchReject {
		target = "rejected"
		evType = tasks.EventTypeTaskPatchRejected
	}
	if p.Status == target {
		// Idempotent re-apply.
		return false, nil
	}
	if p.Status != "pending" {
		return false, fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrGroupInvalidTransition, p.Status, target)
	}
	p.Status = target
	p.UpdatedAt = time.Now()
	if err := d.persistPatchLocked(ctx, p); err != nil {
		return false, err
	}
	var payload events.EventPayload
	if action == tasks.PatchAccept {
		payload = tasks.TaskPatchAppliedPayload{PatchID: patchID}
	} else {
		payload = tasks.TaskPatchRejectedPayload{PatchID: patchID}
	}
	if err := d.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: sessionID},
		Payload:  payload,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// AcknowledgeBackground implements tasks.TaskRegistry. Marks the
// given tasks as user-acknowledged. Returns the count of tasks that
// transitioned; emits one `task.background_acknowledged` event per
// transition. Unknown task IDs are silently skipped.
func (d *driver) AcknowledgeBackground(ctx context.Context, sessionID identity.Identity, ids []tasks.TaskID) (int, error) {
	if d.closed.Load() {
		return 0, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return 0, err
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return 0, err
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: sessionID}) {
		return 0, tasks.ErrIdentityRequired
	}

	d.mu.Lock()
	// We collect events to emit AFTER releasing the lock to avoid
	// holding it while the bus.Publish potentially blocks. The
	// driver's other publish paths hold the lock; this method
	// emits N events, so the unlock-and-publish pattern reduces
	// hold time under load.
	type emit struct {
		ev events.Event
	}
	var emits []emit
	count := 0
	for _, tid := range ids {
		t, ok := d.tasks[tid]
		if !ok {
			continue
		}
		if !identityVisible(ctxIdent, t.Identity) {
			continue
		}
		if t.Kind != tasks.KindBackground {
			continue
		}
		if !isTerminal(t.Status) {
			continue
		}
		if _, already := d.acknowledged[tid]; already {
			continue
		}
		d.acknowledged[tid] = struct{}{}
		count++
		emits = append(emits, emit{ev: events.Event{
			Type:     tasks.EventTypeTaskBackgroundAcknowledged,
			Identity: t.Identity,
			Payload:  tasks.TaskBackgroundAcknowledgedPayload{TaskID: tid},
		}})
	}
	d.mu.Unlock()

	// Emit EVERY collected event before returning, even if one fails.
	// Returning early on the first publish error left earlier acks
	// with events shipped + later acks recorded but never observable
	// — a silent split-brain between `acknowledged` map state and
	// subscriber visibility. Joining the errors keeps the count
	// honest (it always reflects the tasks the driver flipped to
	// acked) while surfacing the publish failures as a single
	// aggregate per AGENTS.md §5 "fail loudly."
	var publishErrs []error
	for _, e := range emits {
		if err := d.bus.Publish(ctx, e.ev); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("tasks/inprocess: publish %q for %v: %w",
				e.ev.Type, e.ev.Payload, err))
		}
	}
	if len(publishErrs) > 0 {
		return count, errors.Join(publishErrs...)
	}
	return count, nil
}

// RegisterRetainTurnWaiter implements tasks.TaskRegistry. Returns a
// channel that the driver closes when the session's earliest-active
// retain-turn group resolves. Buffered size 1 + close-once
// invariant.
func (d *driver) RegisterRetainTurnWaiter(sessionID identity.Identity) (<-chan tasks.TaskGroupID, func()) {
	w := &retainWaiter{ch: make(chan tasks.TaskGroupID, 1)}

	d.mu.Lock()
	d.retainWaiters[sessionID.SessionID] = append(d.retainWaiters[sessionID.SessionID], w)
	d.mu.Unlock()

	cancel := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		closeRetainWaiterLocked(w)
		// Remove from the slice.
		list := d.retainWaiters[sessionID.SessionID]
		for i, entry := range list {
			if entry == w {
				d.retainWaiters[sessionID.SessionID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(d.retainWaiters[sessionID.SessionID]) == 0 {
			delete(d.retainWaiters, sessionID.SessionID)
		}
	}
	return w.ch, cancel
}

// WatchGroup implements tasks.TaskRegistry. Returns a buffered
// (size 1) channel that the driver primes with `GroupCompletion`
// either now (resolved-but-still-tracked group) or at resolve time.
func (d *driver) WatchGroup(sessionID identity.Identity, groupID tasks.TaskGroupID) (<-chan tasks.GroupCompletion, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[groupID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, groupID)
	}
	if !identitiesEqual(g.SessionID, sessionID) {
		return nil, nil, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, groupID)
	}

	sub := &groupSubscriber{ch: make(chan tasks.GroupCompletion, 1)}

	// If the group is already resolved AND we have a cached
	// completion payload, deliver it immediately + close the channel.
	// This is the late-subscriber path (D-022 doc'd in the plan).
	if isGroupTerminal(g.Status) {
		if cached, has := d.groupCompletions[groupID]; has {
			sub.ch <- cached
		}
		close(sub.ch)
		sub.closed = true
		// Cancel is a no-op on an already-closed subscription.
		return sub.ch, func() {}, nil
	}

	d.groupSubs[groupID] = append(d.groupSubs[groupID], sub)

	cancel := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !sub.closed {
			close(sub.ch)
			sub.closed = true
		}
		list := d.groupSubs[groupID]
		for i, entry := range list {
			if entry == sub {
				d.groupSubs[groupID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(d.groupSubs[groupID]) == 0 {
			delete(d.groupSubs, groupID)
		}
	}
	return sub.ch, cancel, nil
}

// --- internal helpers (caller MUST hold d.mu when noted) -------------

// resolveGroupLocked transitions g to a terminal status (Completed
// or Cancelled), constructs the `GroupCompletion` payload, caches
// it, delivers it to every active WatchGroup subscriber, closes the
// retain-turn waiters for the owning session, persists the group,
// and emits the right bus event.
//
// Caller MUST hold d.mu. Returns the first persist / publish error
// encountered. The completion payload is cached and delivered to
// subscribers regardless of persist/publish failure so callers
// observing WatchGroup don't deadlock; the error surfaces the
// durable-record + bus-event gap to the public-method caller so
// retries can land at the right layer (fail-loudly per AGENTS.md §5).
func (d *driver) resolveGroupLocked(ctx context.Context, g *tasks.TaskGroup, final tasks.TaskGroupStatus, reason string) error {
	now := time.Now()
	g.Status = final
	g.UpdatedAt = now
	g.ResolvedAt = &now
	completion := tasks.GroupCompletion{
		GroupID:     g.ID,
		SessionID:   g.SessionID,
		OwnerTaskID: g.OwnerTaskID,
		FinalStatus: final,
		ResolvedAt:  now,
		Members:     d.collectMemberOutcomesLocked(g),
		Reason:      reason,
	}
	d.groupCompletions[g.ID] = completion

	// Persist + emit. The WatchGroup fan-out below runs regardless of
	// persist/publish outcome so a slow durable backend never wedges
	// the in-memory wake; resolveErr captures the first failure and
	// the caller surfaces it.
	var resolveErr error
	if err := d.persistGroupLocked(ctx, g); err != nil {
		resolveErr = fmt.Errorf("tasks/inprocess: persist resolved group %q: %w", g.ID, err)
	}

	evType := tasks.EventTypeTaskGroupResolved
	var payload events.EventPayload = tasks.TaskGroupResolvedPayload{Completion: completion}
	if final != tasks.GroupCompleted {
		evType = tasks.EventTypeTaskGroupCancelled
		payload = tasks.TaskGroupCancelledPayload{Completion: completion}
	}
	if err := d.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: g.SessionID},
		Payload:  payload,
	}); err != nil && resolveErr == nil {
		resolveErr = fmt.Errorf("tasks/inprocess: publish %q for group %q: %w", evType, g.ID, err)
	}

	// Fan out the completion payload to every active WatchGroup
	// subscriber. Each channel is buffered size 1 so the send never
	// blocks (unless a slow consumer holds onto a delivery from a
	// prior resolve — which doesn't happen here because subscriptions
	// are per-group; first delivery is also the last). Close-once is
	// guarded by the subscriber's `closed` flag.
	for _, sub := range d.groupSubs[g.ID] {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- completion:
		default:
			// Channel was somehow already at capacity; skip the send,
			// still close. Defensive — shouldn't happen given the
			// per-group close-on-first-delivery contract.
		}
		close(sub.ch)
		sub.closed = true
	}
	delete(d.groupSubs, g.ID)

	// Wake the retain-turn waiters for this session, if any. We
	// deliver the resolved group's ID then close. Each waiter is
	// guarded by its `closed` flag for close-once.
	if g.RetainTurn {
		for _, w := range d.retainWaiters[g.SessionID.SessionID] {
			if w.closed {
				continue
			}
			select {
			case w.ch <- g.ID:
			default:
			}
			close(w.ch)
			w.closed = true
		}
		delete(d.retainWaiters, g.SessionID.SessionID)
	}
	return resolveErr
}

// collectMemberOutcomesLocked returns one `MemberOutcome` per
// member, snapshotting the member's terminal Result / Error. Members
// that are still non-terminal at resolve time (only possible on the
// cancel path with `propagate=false` — every other path waits for
// terminality before resolving) are recorded with their current
// status and nil Result/Error.
//
// Caller MUST hold d.mu.
func (d *driver) collectMemberOutcomesLocked(g *tasks.TaskGroup) []tasks.MemberOutcome {
	out := make([]tasks.MemberOutcome, 0, len(g.Members))
	for _, tid := range g.Members {
		t, ok := d.tasks[tid]
		if !ok {
			out = append(out, tasks.MemberOutcome{TaskID: tid, Status: tasks.StatusCancelled})
			continue
		}
		mo := tasks.MemberOutcome{TaskID: tid, Status: t.Status}
		if t.Result != nil {
			r := *t.Result
			mo.Result = &r
		}
		if t.Error != nil {
			e := *t.Error
			mo.Error = &e
		}
		out = append(out, mo)
	}
	return out
}

// allMembersTerminalLocked reports whether every member of g is in
// a terminal state. An empty member list returns false — a group
// with zero members is degenerate (the planner sealed before
// spawning anything), and auto-resolving it on seal would surprise
// callers. The explicit `ApplyGroup(ActionResolve)` path still works
// on a sealed empty group to make the resolution intent visible.
// Caller MUST hold d.mu.
func (d *driver) allMembersTerminalLocked(g *tasks.TaskGroup) bool {
	if len(g.Members) == 0 {
		return false
	}
	for _, tid := range g.Members {
		t, ok := d.tasks[tid]
		if !ok {
			continue
		}
		if !isTerminal(t.Status) {
			return false
		}
	}
	return true
}

// onMemberTerminalLocked is the hook the per-task Mark* /
// transitionLocked path invokes when a task that belongs to a group
// reaches terminal. It implements the FailFast cascade + resolve
// gate. Caller MUST hold d.mu. Returns the resolve-path error (if
// any) so `transitionLocked` can surface it to the Mark* caller.
func (d *driver) onMemberTerminalLocked(ctx context.Context, t *tasks.Task) error {
	gid, ok := d.taskGroup[t.ID]
	if !ok {
		return nil
	}
	g, exists := d.groups[gid]
	if !exists {
		return nil
	}
	if isGroupTerminal(g.Status) {
		return nil
	}

	// FailFast: a member failure cancels remaining members AND
	// transitions the group to Cancelled. The cancel reason is
	// derived from the failing member's error code.
	if g.FailFast && t.Status == tasks.StatusFailed {
		reason := "fail-fast"
		if t.Error != nil && t.Error.Code != "" {
			reason = "fail-fast:" + t.Error.Code
		}
		var cancelErr error
		for _, mid := range g.Members {
			if mid == t.ID {
				continue
			}
			m, exists := d.tasks[mid]
			if !exists {
				continue
			}
			if isTerminal(m.Status) {
				continue
			}
			if cerr := d.cancelTaskLocked(ctx, m, reason, true); cerr != nil && cancelErr == nil {
				cancelErr = fmt.Errorf("tasks/inprocess: fail-fast cascade cancel member %q: %w", mid, cerr)
			}
		}
		if rerr := d.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason); rerr != nil {
			if cancelErr == nil {
				return rerr
			}
			return fmt.Errorf("%w; resolve: %v", cancelErr, rerr) //nolint:errorlint // reason: aggregate; primary cause kept via %w
		}
		return cancelErr
	}

	// Normal resolve path: when the group is sealed AND all members
	// terminal, transition to Completed.
	if g.Status == tasks.GroupSealed && d.allMembersTerminalLocked(g) {
		return d.resolveGroupLocked(ctx, g, tasks.GroupCompleted, "")
	}
	return nil
}

// cancelTaskLocked cancels t under the held lock. Mirrors the public
// Cancel surface for a single task; no children walk (FailFast
// targets group siblings, not arbitrary children). `cascaded` is
// stamped on the emitted event so subscribers can tell operator
// cancel from cascade.
func (d *driver) cancelTaskLocked(ctx context.Context, t *tasks.Task, reason string, cascaded bool) error {
	if isTerminal(t.Status) {
		return nil
	}
	if err := d.transitionLocked(ctx, t, tasks.StatusCancelled); err != nil {
		return err
	}
	if err := d.publish(ctx, t, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
		TaskID:   t.ID,
		Reason:   reason,
		Cascaded: cascaded,
	}); err != nil {
		return err
	}
	// Cascade to t's own children per its PropagateOnCancel.
	if t.PropagateOnCancel == tasks.PropagateCascade {
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
				return err
			}
			if err := d.publish(ctx, child, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
				TaskID:   child.ID,
				Reason:   reason,
				Cascaded: true,
			}); err != nil {
				return err
			}
			queue = append(queue, d.children[childID]...)
		}
	}
	return nil
}

// addMemberLocked adds tid to g.Members and to the reverse
// taskGroup index. Returns ErrGroupSealed when g is sealed or
// terminal. Caller MUST hold d.mu.
func (d *driver) addMemberLocked(g *tasks.TaskGroup, tid tasks.TaskID) error {
	if g.Status != tasks.GroupOpen {
		return tasks.ErrGroupSealed
	}
	g.Members = append(g.Members, tid)
	g.UpdatedAt = time.Now()
	d.taskGroup[tid] = g.ID
	return nil
}

// AddMemberToGroup is a driver-internal (non-interface) helper the
// conformance suite uses to wire a freshly-spawned task into a
// group. Phase 26+ will route this through the SpawnTool's GroupID
// parameter; until then the helper exposes the seam directly so
// Phase 21's conformance subtests can exercise the resolve gate
// without waiting for the tool-dispatch wiring.
//
// Returns `ErrGroupNotFound` on an unknown group or a cross-session
// access attempt; `ErrGroupSealed` on a sealed or terminal group;
// `ErrNotFound` on an unknown task.
func (d *driver) AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error {
	if d.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[gid]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, gid)
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: g.SessionID}) {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, gid)
	}
	t, ok := d.tasks[tid]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrNotFound, tid)
	}
	if !identityVisible(ctxIdent, t.Identity) {
		return fmt.Errorf("%w: id=%q", tasks.ErrNotFound, tid)
	}
	if err := d.addMemberLocked(g, tid); err != nil {
		return err
	}
	return d.persistGroupLocked(ctx, g)
}

// CreatePendingPatch is a driver-internal helper the conformance
// suite uses to seed a pending patch record. Phase 42+ planner code
// will land patches through a typed interface; Phase 21 ships the
// transition surface (ApplyPatch) and the helper that creates the
// pending record the planner would normally create.
func (d *driver) CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, bytesPayload []byte) (*tasks.Patch, error) {
	if d.closed.Load() {
		return nil, tasks.ErrRegistryClosed
	}
	if err := validateListIdentity(sessionID); err != nil {
		return nil, err
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: sessionID}) {
		return nil, tasks.ErrIdentityRequired
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	key := patchKey{SessionID: sessionID.SessionID, PatchID: patchID}
	if _, exists := d.patches[key]; exists {
		// Idempotent: return the existing record.
		p := d.patches[key]
		return p, nil
	}
	now := time.Now()
	p := &tasks.Patch{
		ID:        patchID,
		SessionID: sessionID,
		Status:    "pending",
		Bytes:     append([]byte(nil), bytesPayload...),
		CreatedAt: now,
		UpdatedAt: now,
	}
	d.patches[key] = p
	if err := d.persistPatchLocked(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// persistGroupLocked writes the group through the StateStore. Caller
// MUST hold d.mu.
func (d *driver) persistGroupLocked(ctx context.Context, g *tasks.TaskGroup) error {
	payload, err := marshalGroup(g)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal group: %w", err)
	}
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: g.SessionID},
		Kind:     tasks.GroupKind,
		Bytes:    payload,
		Version:  0,
	}
	if err := d.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("tasks/inprocess: state save (group): %w", err)
	}
	return nil
}

// persistPatchLocked writes the patch through the StateStore. The
// patch bytes are opaque to the registry; the caller is responsible
// for any audit-redaction upstream (D-020).
func (d *driver) persistPatchLocked(ctx context.Context, p *tasks.Patch) error {
	payload, err := marshalPatch(p)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal patch: %w", err)
	}
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: p.SessionID},
		Kind:     tasks.PatchKind,
		Bytes:    payload,
		Version:  0,
	}
	if err := d.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("tasks/inprocess: state save (patch): %w", err)
	}
	return nil
}

// publishGroup wraps a group event with the right identity quadruple.
func (d *driver) publishGroup(ctx context.Context, g *tasks.TaskGroup, evType events.EventType, payload events.EventPayload) error {
	return d.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: g.SessionID},
		Payload:  payload,
	})
}

// closeRetainWaiterLocked closes a retain-turn waiter exactly once.
func closeRetainWaiterLocked(w *retainWaiter) {
	if w.closed {
		return
	}
	close(w.ch)
	w.closed = true
}

// isValidGroupTransition is the group FSM table.
//
// Allowed edges:
//
//	Open      → Sealed, Cancelled
//	Sealed    → Completed, Cancelled
//	Completed (terminal — no edges out)
//	Cancelled (terminal — no edges out)
//
// Same-state transitions are invalid.
func isValidGroupTransition(from, to tasks.TaskGroupStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case tasks.GroupOpen:
		return to == tasks.GroupSealed || to == tasks.GroupCancelled
	case tasks.GroupSealed:
		return to == tasks.GroupCompleted || to == tasks.GroupCancelled
	case tasks.GroupCompleted, tasks.GroupCancelled:
		return false
	default:
		return false
	}
}

// isGroupTerminal reports whether g is in a terminal status.
func isGroupTerminal(s tasks.TaskGroupStatus) bool {
	return s == tasks.GroupCompleted || s == tasks.GroupCancelled
}

// identitiesEqual compares two Identity values component-wise.
func identitiesEqual(a, b identity.Identity) bool {
	return a.TenantID == b.TenantID &&
		a.UserID == b.UserID &&
		a.SessionID == b.SessionID
}

// validateGroupRequest gates the GroupRequest shape used by
// ResolveOrCreateGroup. Identity is mandatory; the rest of the
// fields have no structural constraints.
func validateGroupRequest(req tasks.GroupRequest) error {
	if req.SessionID.TenantID == "" || req.SessionID.UserID == "" || req.SessionID.SessionID == "" {
		return tasks.ErrIdentityRequired
	}
	return nil
}

// marshalGroup is JSON-encoding for the group record. The group
// payload is opaque bytes from StateStore's POV (D-027 typed-wrapper
// pattern).
func marshalGroup(g *tasks.TaskGroup) ([]byte, error) {
	return json.Marshal(g)
}

// marshalPatch is JSON-encoding for the patch record.
func marshalPatch(p *tasks.Patch) ([]byte, error) {
	return json.Marshal(p)
}

