package engine

// group governance + retain-turn + patches + WatchGroup for the
// engine. The engine extends the per-task internal model with
// additional maps:
//
//   - `groups[TaskGroupID]*tasks.TaskGroup` — primary group store.
//   - `taskGroup[TaskID]TaskGroupID` — reverse index from member to
//     owning group. Set when a task is added to a group; consulted
//     by the per-task terminal-transition path so the engine knows
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
// All maps are guarded by the same `sync.RWMutex` the per-task state
// lives behind. Persistence is delegated to the [Backend]; a backend
// that blocks on I/O serializes mutations on this engine instance
// while the lock is held (see the package doc).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
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
func (e *Engine) ResolveOrCreateGroup(ctx context.Context, req tasks.GroupRequest) (*tasks.TaskGroup, error) {
	if e.closed.Load() {
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

	e.mu.Lock()
	defer e.mu.Unlock()

	if req.ID != "" {
		if existing, ok := e.groups[req.ID]; ok {
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
		id = tasks.TaskGroupID(ulid.MustNew(ulid.Now(), e.ulidEntropy).String())
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
	if err := e.persistGroupLocked(ctx, g); err != nil {
		return nil, err
	}
	e.groups[id] = g

	if err := e.publishGroup(ctx, g, tasks.EventTypeTaskGroupCreated, tasks.TaskGroupCreatedPayload{
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
func (e *Engine) SealGroup(ctx context.Context, id tasks.TaskGroupID) error {
	if e.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	g, ok := e.groups[id]
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
	priorStatus := g.Status
	priorUpdated := g.UpdatedAt
	g.Status = tasks.GroupSealed
	g.UpdatedAt = time.Now()
	if err := e.persistGroupLocked(ctx, g); err != nil {
		// Keep the in-memory group consistent with the store on a
		// persist failure (it stays Open and re-sealable).
		g.Status = priorStatus
		g.UpdatedAt = priorUpdated
		return err
	}
	if err := e.publishGroup(ctx, g, tasks.EventTypeTaskGroupSealed, tasks.TaskGroupSealedPayload{
		GroupID:  g.ID,
		Members:  append([]tasks.TaskID(nil), g.Members...),
		SealedAt: g.UpdatedAt.UnixNano(),
	}); err != nil {
		return err
	}
	// If sealing finds the group already has all members terminal
	// (e.g. members completed before the seal), resolve immediately.
	if e.allMembersTerminalLocked(g) {
		if err := e.resolveGroupLocked(ctx, g, tasks.GroupCompleted, ""); err != nil {
			return err
		}
	}
	return nil
}

// CancelGroup implements tasks.TaskRegistry.
func (e *Engine) CancelGroup(ctx context.Context, id tasks.TaskGroupID, reason string, propagate bool) error {
	if e.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	g, ok := e.groups[id]
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
			t, exists := e.tasks[tid]
			if !exists {
				continue
			}
			if isTerminal(t.Status) {
				continue
			}
			if cerr := e.cancelTaskLocked(ctx, t, reason, true); cerr != nil {
				return cerr
			}
		}
	}

	if err := e.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason); err != nil {
		return err
	}
	return nil
}

// ApplyGroup implements tasks.TaskRegistry. Convenience dispatch.
func (e *Engine) ApplyGroup(ctx context.Context, id tasks.TaskGroupID, action tasks.GroupAction) error {
	switch action {
	case tasks.ActionSeal:
		return e.SealGroup(ctx, id)
	case tasks.ActionCancel:
		return e.CancelGroup(ctx, id, "action:cancel", true)
	case tasks.ActionResolve:
		return e.applyResolveAction(ctx, id)
	default:
		return fmt.Errorf("%w: unknown action %q", tasks.ErrGroupInvalidTransition, action)
	}
}

// applyResolveAction handles ActionResolve. Errors with
// `ErrGroupNotSealed` when the group is still Open.
func (e *Engine) applyResolveAction(ctx context.Context, id tasks.TaskGroupID) error {
	if e.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	g, ok := e.groups[id]
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
		return e.resolveGroupLocked(ctx, g, tasks.GroupCompleted, "")
	default:
		return fmt.Errorf("%w: from=%q to=%q",
			tasks.ErrGroupInvalidTransition, g.Status, tasks.GroupCompleted)
	}
}

// ListGroups implements tasks.TaskRegistry.
func (e *Engine) ListGroups(ctx context.Context, sessionID identity.Identity, status *tasks.TaskGroupStatus) ([]tasks.TaskGroup, error) {
	if e.closed.Load() {
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

	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]tasks.TaskGroup, 0, 4)
	for _, g := range e.groups {
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
func (e *Engine) ApplyPatch(ctx context.Context, sessionID identity.Identity, patchID string, action tasks.PatchAction) (bool, error) {
	if e.closed.Load() {
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

	e.mu.Lock()
	defer e.mu.Unlock()

	key := patchKey{SessionID: sessionID.SessionID, PatchID: patchID}
	p, ok := e.patches[key]
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
	priorStatus := p.Status
	priorUpdated := p.UpdatedAt
	p.Status = target
	p.UpdatedAt = time.Now()
	if err := e.persistPatchLocked(ctx, p); err != nil {
		// Keep the in-memory patch consistent with the store on a
		// persist failure: it stays pending (and no event was emitted),
		// so a retry is honest rather than hitting the idempotent
		// already-applied short-circuit.
		p.Status = priorStatus
		p.UpdatedAt = priorUpdated
		return false, err
	}
	var payload events.EventPayload
	if action == tasks.PatchAccept {
		payload = tasks.TaskPatchAppliedPayload{PatchID: patchID}
	} else {
		payload = tasks.TaskPatchRejectedPayload{PatchID: patchID}
	}
	if err := e.bus.Publish(ctx, events.Event{
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
func (e *Engine) AcknowledgeBackground(ctx context.Context, sessionID identity.Identity, ids []tasks.TaskID) (int, error) {
	if e.closed.Load() {
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

	e.mu.Lock()
	// We collect events to emit AFTER releasing the lock to avoid
	// holding it while the bus.Publish potentially blocks. The
	// driver's other publish paths hold the lock; this method
	// emits N events, so the unlock-and-publish pattern reduces
	// hold time under load.
	type emit struct {
		ev events.Event
	}
	emits := make([]emit, 0, len(ids))
	count := 0
	for _, tid := range ids {
		t, ok := e.tasks[tid]
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
		if _, already := e.acknowledged[tid]; already {
			continue
		}
		e.acknowledged[tid] = struct{}{}
		count++
		emits = append(emits, emit{ev: events.Event{
			Type:     tasks.EventTypeTaskBackgroundAcknowledged,
			Identity: t.Identity,
			Payload:  tasks.TaskBackgroundAcknowledgedPayload{TaskID: tid},
		}})
	}
	e.mu.Unlock()

	// Emit EVERY collected event before returning, even if one fails.
	// Returning early on the first publish error left earlier acks
	// with events shipped + later acks recorded but never observable
	// — a silent split-brain between `acknowledged` map state and
	// subscriber visibility. Joining the errors keeps the count
	// honest (it always reflects the tasks the driver flipped to
	// acked) while surfacing the publish failures as a single
	// aggregate per AGENTS.md §5 "fail loudly."
	var publishErrs []error
	for _, em := range emits {
		if err := e.bus.Publish(ctx, em.ev); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("tasks/engine: publish %q for %v: %w",
				em.ev.Type, em.ev.Payload, err))
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
func (e *Engine) RegisterRetainTurnWaiter(sessionID identity.Identity) (<-chan tasks.TaskGroupID, func()) {
	w := &retainWaiter{ch: make(chan tasks.TaskGroupID, 1)}

	e.mu.Lock()
	e.retainWaiters[sessionID.SessionID] = append(e.retainWaiters[sessionID.SessionID], w)
	e.mu.Unlock()

	cancel := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		closeRetainWaiterLocked(w)
		// Remove from the slice.
		list := e.retainWaiters[sessionID.SessionID]
		for i, entry := range list {
			if entry == w {
				e.retainWaiters[sessionID.SessionID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(e.retainWaiters[sessionID.SessionID]) == 0 {
			delete(e.retainWaiters, sessionID.SessionID)
		}
	}
	return w.ch, cancel
}

// WatchGroup implements tasks.TaskRegistry. Returns a buffered
// (size 1) channel that the driver primes with `GroupCompletion`
// either now (resolved-but-still-tracked group) or at resolve time.
func (e *Engine) WatchGroup(sessionID identity.Identity, groupID tasks.TaskGroupID) (<-chan tasks.GroupCompletion, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	g, ok := e.groups[groupID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, groupID)
	}
	if !identitiesEqual(g.SessionID, sessionID) {
		return nil, nil, fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, groupID)
	}

	sub := &groupSubscriber{ch: make(chan tasks.GroupCompletion, 1)}

	// If the group is already resolved AND we have a cached
	// completion payload, deliver it immediately + close the channel.
	// This is the late-subscriber path (doc'd in the plan).
	if isGroupTerminal(g.Status) {
		if cached, has := e.groupCompletions[groupID]; has {
			sub.ch <- cached
		}
		close(sub.ch)
		sub.closed = true
		// Cancel is a no-op on an already-closed subscription.
		return sub.ch, func() {}, nil
	}

	e.groupSubs[groupID] = append(e.groupSubs[groupID], sub)

	cancel := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !sub.closed {
			close(sub.ch)
			sub.closed = true
		}
		list := e.groupSubs[groupID]
		for i, entry := range list {
			if entry == sub {
				e.groupSubs[groupID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(e.groupSubs[groupID]) == 0 {
			delete(e.groupSubs, groupID)
		}
	}
	return sub.ch, cancel, nil
}

// --- internal helpers (caller MUST hold e.mu when noted) -------------

// resolveGroupLocked transitions g to a terminal status (Completed
// or Cancelled), constructs the `GroupCompletion` payload, caches
// it, delivers it to every active WatchGroup subscriber, closes the
// retain-turn waiters for the owning session, persists the group,
// and emits the right bus event.
//
// Caller MUST hold e.mu. Returns the first persist / publish error
// encountered. The completion payload is cached and delivered to
// subscribers regardless of persist/publish failure so callers
// observing WatchGroup don't deadlock; the error surfaces the
// durable-record + bus-event gap to the public-method caller so
// retries can land at the right layer (fail-loudly per AGENTS.md §5).
func (e *Engine) resolveGroupLocked(ctx context.Context, g *tasks.TaskGroup, final tasks.TaskGroupStatus, reason string) error {
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
		Members:     e.collectMemberOutcomesLocked(g),
		Reason:      reason,
	}
	e.groupCompletions[g.ID] = completion

	// Persist + emit. The WatchGroup fan-out below runs regardless of
	// persist/publish outcome so a slow durable backend never wedges
	// the in-memory wake; resolveErr captures the first failure and
	// the caller surfaces it.
	var resolveErr error
	if err := e.persistGroupLocked(ctx, g); err != nil {
		resolveErr = fmt.Errorf("tasks/engine: persist resolved group %q: %w", g.ID, err)
	}

	evType := tasks.EventTypeTaskGroupResolved
	var payload events.EventPayload = tasks.TaskGroupResolvedPayload{Completion: completion}
	if final != tasks.GroupCompleted {
		evType = tasks.EventTypeTaskGroupCancelled
		payload = tasks.TaskGroupCancelledPayload{Completion: completion}
	}
	if err := e.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: g.SessionID},
		Payload:  payload,
	}); err != nil && resolveErr == nil {
		resolveErr = fmt.Errorf("tasks/engine: publish %q for group %q: %w", evType, g.ID, err)
	}

	// Fan out the completion payload to every active WatchGroup
	// subscriber. Each channel is buffered size 1 so the send never
	// blocks (unless a slow consumer holds onto a delivery from a
	// prior resolve — which doesn't happen here because subscriptions
	// are per-group; first delivery is also the last). Close-once is
	// guarded by the subscriber's `closed` flag.
	for _, sub := range e.groupSubs[g.ID] {
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
	delete(e.groupSubs, g.ID)

	// Wake the retain-turn waiters for this session, if any. We
	// deliver the resolved group's ID then close. Each waiter is
	// guarded by its `closed` flag for close-once.
	if g.RetainTurn {
		for _, w := range e.retainWaiters[g.SessionID.SessionID] {
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
		delete(e.retainWaiters, g.SessionID.SessionID)
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
// Caller MUST hold e.mu.
func (e *Engine) collectMemberOutcomesLocked(g *tasks.TaskGroup) []tasks.MemberOutcome {
	out := make([]tasks.MemberOutcome, 0, len(g.Members))
	for _, tid := range g.Members {
		t, ok := e.tasks[tid]
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
			errCopy := *t.Error
			mo.Error = &errCopy
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
// Caller MUST hold e.mu.
func (e *Engine) allMembersTerminalLocked(g *tasks.TaskGroup) bool {
	if len(g.Members) == 0 {
		return false
	}
	for _, tid := range g.Members {
		t, ok := e.tasks[tid]
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
// gate. Caller MUST hold e.mu. Returns the resolve-path error (if
// any) so `transitionLocked` can surface it to the Mark* caller.
func (e *Engine) onMemberTerminalLocked(ctx context.Context, t *tasks.Task) error {
	gid, ok := e.taskGroup[t.ID]
	if !ok {
		return nil
	}
	g, exists := e.groups[gid]
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
			m, exists := e.tasks[mid]
			if !exists {
				continue
			}
			if isTerminal(m.Status) {
				continue
			}
			if cerr := e.cancelTaskLocked(ctx, m, reason, true); cerr != nil && cancelErr == nil {
				cancelErr = fmt.Errorf("tasks/engine: fail-fast cascade cancel member %q: %w", mid, cerr)
			}
		}
		if rerr := e.resolveGroupLocked(ctx, g, tasks.GroupCancelled, reason); rerr != nil {
			if cancelErr == nil {
				return rerr
			}
			return fmt.Errorf("%w; resolve: %v", cancelErr, rerr) //nolint:errorlint // reason: aggregate; primary cause kept via %w
		}
		return cancelErr
	}

	// Normal resolve path: when the group is sealed AND all members
	// terminal, transition to Completed.
	if g.Status == tasks.GroupSealed && e.allMembersTerminalLocked(g) {
		return e.resolveGroupLocked(ctx, g, tasks.GroupCompleted, "")
	}
	return nil
}

// cancelTaskLocked cancels t under the held lock. Mirrors the public
// Cancel surface for a single task; no children walk (FailFast
// targets group siblings, not arbitrary children). `cascaded` is
// stamped on the emitted event so subscribers can tell operator
// cancel from cascade.
func (e *Engine) cancelTaskLocked(ctx context.Context, t *tasks.Task, reason string, cascaded bool) error {
	if isTerminal(t.Status) {
		return nil
	}
	if err := e.transitionLocked(ctx, t, tasks.StatusCancelled); err != nil {
		return err
	}
	if err := e.publish(ctx, t, tasks.EventTypeTaskCancelled, tasks.TaskCancelledPayload{
		TaskID:   t.ID,
		Reason:   reason,
		Cascaded: cascaded,
	}); err != nil {
		return err
	}
	// Cascade to t's own children per its PropagateOnCancel, sharing the
	// one descendant walk with the public Cancel path so an
	// isolate-marked descendant detaches identically here. The direct
	// target t was already transitioned above (unconditional).
	if t.PropagateOnCancel == tasks.PropagateCascade {
		if err := e.cascadeCancelDescendantsLocked(ctx, t.ID, reason); err != nil {
			return err
		}
	}
	return nil
}

// addMemberLocked adds tid to g.Members and to the reverse
// taskGroup index. Returns ErrGroupSealed when g is sealed or
// terminal. Caller MUST hold e.mu.
func (e *Engine) addMemberLocked(g *tasks.TaskGroup, tid tasks.TaskID) error {
	if g.Status != tasks.GroupOpen {
		return tasks.ErrGroupSealed
	}
	g.Members = append(g.Members, tid)
	g.UpdatedAt = time.Now()
	e.taskGroup[tid] = g.ID
	return nil
}

// unwireMemberLocked reverses addMemberLocked: it removes tid from
// g.Members and drops the reverse index. Used to roll back a spawn
// whose group persist failed. Caller MUST hold e.mu.
func (e *Engine) unwireMemberLocked(g *tasks.TaskGroup, tid tasks.TaskID) {
	for i, m := range g.Members {
		if m == tid {
			g.Members = append(g.Members[:i], g.Members[i+1:]...)
			break
		}
	}
	delete(e.taskGroup, tid)
}

// detachChildLocked removes child from parent's children index, used to
// roll back the parent linkage of a spawn that failed after the child
// entry was recorded. Caller MUST hold e.mu.
func (e *Engine) detachChildLocked(parent, child tasks.TaskID) {
	list := e.children[parent]
	for i, c := range list {
		if c == child {
			e.children[parent] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(e.children[parent]) == 0 {
		delete(e.children, parent)
	}
}

// AddMemberToGroup is a driver-internal (non-interface) helper the
// conformance suite uses to wire a freshly-spawned task into a
// group. The tool dispatcher will route this through the SpawnTool's GroupID
// parameter; until then the helper exposes the seam directly so
// the conformance subtests can exercise the resolve gate
// without waiting for the tool-dispatch wiring.
//
// Returns `ErrGroupNotFound` on an unknown group or a cross-session
// access attempt; `ErrGroupSealed` on a sealed or terminal group;
// `ErrNotFound` on an unknown task.
func (e *Engine) AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error {
	if e.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	ctxIdent, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	g, ok := e.groups[gid]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, gid)
	}
	if !identityVisible(ctxIdent, identity.Quadruple{Identity: g.SessionID}) {
		return fmt.Errorf("%w: id=%q", tasks.ErrGroupNotFound, gid)
	}
	t, ok := e.tasks[tid]
	if !ok {
		return fmt.Errorf("%w: id=%q", tasks.ErrNotFound, tid)
	}
	if !identityVisible(ctxIdent, t.Identity) {
		return fmt.Errorf("%w: id=%q", tasks.ErrNotFound, tid)
	}
	if err := e.addMemberLocked(g, tid); err != nil {
		return err
	}
	return e.persistGroupLocked(ctx, g)
}

// CreatePendingPatch is a driver-internal helper the conformance
// suite uses to seed a pending patch record. Planner code
// will land patches through a typed interface; Harbor ships the
// transition surface (ApplyPatch) and the helper that creates the
// pending record the planner would normally create.
func (e *Engine) CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, bytesPayload []byte) (*tasks.Patch, error) {
	if e.closed.Load() {
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

	e.mu.Lock()
	defer e.mu.Unlock()
	key := patchKey{SessionID: sessionID.SessionID, PatchID: patchID}
	if _, exists := e.patches[key]; exists {
		// Idempotent: return the existing record.
		p := e.patches[key]
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
	e.patches[key] = p
	if err := e.persistPatchLocked(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// persistGroupLocked writes the group through the backend. Caller
// MUST hold e.mu.
func (e *Engine) persistGroupLocked(ctx context.Context, g *tasks.TaskGroup) error {
	if err := e.backend.SaveGroup(ctx, g); err != nil {
		return fmt.Errorf("tasks/engine: persist group: %w", err)
	}
	return nil
}

// persistPatchLocked writes the patch through the backend. The patch
// bytes are opaque to the registry; the caller is responsible for any
// audit-redaction upstream.
func (e *Engine) persistPatchLocked(ctx context.Context, p *tasks.Patch) error {
	if err := e.backend.SavePatch(ctx, p); err != nil {
		return fmt.Errorf("tasks/engine: persist patch: %w", err)
	}
	return nil
}

// publishGroup wraps a group event with the right identity quadruple.
func (e *Engine) publishGroup(ctx context.Context, g *tasks.TaskGroup, evType events.EventType, payload events.EventPayload) error {
	return e.bus.Publish(ctx, events.Event{
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
