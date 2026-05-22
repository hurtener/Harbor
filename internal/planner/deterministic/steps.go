package deterministic

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// DecisionTreeStep is the operator-configurable step abstraction the
// deterministic planner walks per `Next` call. Each step's
// [Decide(ctx, rc)] returns:
//
//   - `(decision, true, nil)` — the step claims the call; the
//     planner returns the decision verbatim.
//   - `(nil, false, nil)` — the step is skipped; the planner
//     advances to the next configured step.
//   - `(_, _, err)` — fail-loudly; the planner wraps the error with
//     [planner.ErrDeterministicStep] and returns it. NO silent skip.
//
// Implementations MUST be safe for concurrent use. The
// [DeterministicPlanner] is a reusable artifact (D-025); the same
// step instance receives N concurrent invocations from N concurrent
// runs against the shared planner.
type DecisionTreeStep interface {
	Decide(ctx context.Context, rc planner.RunContext) (planner.Decision, bool, error)
}

// registryBinder is the unexported contract group-aware steps
// implement so the planner constructor can inject the
// [tasks.TaskRegistry] handle without requiring operators to thread
// it through each step value. Steps that do not need the registry
// (e.g. [CallToolStep] / [FinishStep] / [PauseStep]) do NOT implement
// this; the binder loop is a no-op for them.
type registryBinder interface {
	bindRegistry(reg tasks.TaskRegistry)
}

// requiresRegistry reports whether a step needs the registry to be
// supplied via [WithRegistry]. The constructor's validation path
// uses this to reject configurations missing the registry handle.
func requiresRegistry(step DecisionTreeStep) bool {
	switch step.(type) {
	case *SpawnAndAwaitStep, *WatchGroupStep:
		return true
	default:
		return false
	}
}

// bindRegistry attaches the registry handle to a step that needs it.
// Steps that do not implement [registryBinder] receive nothing; the
// no-op surface lets the constructor's binder loop be uniform.
func bindRegistry(step DecisionTreeStep, reg tasks.TaskRegistry) {
	if rb, ok := step.(registryBinder); ok {
		rb.bindRegistry(reg)
	}
}

// ---------------------------------------------------------------------------
// CallToolStep — emit a single CallTool decision.
// ---------------------------------------------------------------------------

// CallToolStep is the operator-configured single-tool dispatch step.
// When `When` is nil (or returns true), the step claims the call and
// returns a [planner.CallTool] decision built from the tool name,
// the args-builder closure, and the reasoning string.
//
// `ArgsBuilder` is required: a CallToolStep with a nil ArgsBuilder
// returns a step error from [Decide] (fail-loudly per §13 — a
// silent default-args behaviour would mask operator bugs).
type CallToolStep struct {
	// Tool is the tool name registered in the ToolCatalogView. The
	// runtime executor dispatches via Phase 26's catalog.
	Tool string
	// ArgsBuilder constructs the JSON-encoded args payload from the
	// run context. Required; nil → step error.
	ArgsBuilder func(planner.RunContext) (json.RawMessage, error)
	// Reasoning is the planner's free-text justification surfaced in
	// observability + audit. Capped by the runtime's payload bounds
	// before emit.
	Reasoning string
	// When is the optional guard. nil → always match. Non-nil → step
	// claims the call only when the guard returns true.
	When func(planner.RunContext) bool
}

// Decide implements [DecisionTreeStep].
func (s *CallToolStep) Decide(_ context.Context, rc planner.RunContext) (planner.Decision, bool, error) {
	if s.When != nil && !s.When(rc) {
		return nil, false, nil
	}
	if s.Tool == "" {
		return nil, false, fmt.Errorf("CallToolStep: Tool name required")
	}
	if s.ArgsBuilder == nil {
		return nil, false, fmt.Errorf("CallToolStep[%s]: ArgsBuilder required", s.Tool)
	}
	args, err := s.ArgsBuilder(rc)
	if err != nil {
		return nil, false, fmt.Errorf("CallToolStep[%s]: ArgsBuilder: %w", s.Tool, err)
	}
	return planner.CallTool{
		Tool:      s.Tool,
		Args:      args,
		Reasoning: s.Reasoning,
	}, true, nil
}

// ---------------------------------------------------------------------------
// FinishStep — emit a terminal Finish decision.
// ---------------------------------------------------------------------------

// FinishStep is the operator-configured terminal step. When `When`
// is nil (or returns true), the step claims the call and returns a
// [planner.Finish] decision built from the configured reason, the
// payload-builder closure, and the metadata-builder closure.
//
// `Reason` MUST be one of the canonical [planner.FinishReason]
// values; an invalid reason surfaces as a step error.
type FinishStep struct {
	// Reason is the terminal reason. MUST be canonical (see
	// planner.IsValidFinishReason).
	Reason planner.FinishReason
	// PayloadBuilder constructs the terminal payload from the run
	// context. nil → nil Payload.
	PayloadBuilder func(planner.RunContext) (any, error)
	// MetadataBuilder constructs the terminal metadata from the run
	// context. nil → nil Metadata (the step still stamps `run_id`
	// when the run has one — see Decide).
	MetadataBuilder func(planner.RunContext) (map[string]any, error)
	// When is the optional guard. nil → always match.
	When func(planner.RunContext) bool
}

// Decide implements [DecisionTreeStep].
func (s *FinishStep) Decide(_ context.Context, rc planner.RunContext) (planner.Decision, bool, error) {
	if s.When != nil && !s.When(rc) {
		return nil, false, nil
	}
	if !planner.IsValidFinishReason(s.Reason) {
		return nil, false, fmt.Errorf("FinishStep: invalid Reason %q (not in canonical set)", s.Reason)
	}

	var payload any
	if s.PayloadBuilder != nil {
		v, err := s.PayloadBuilder(rc)
		if err != nil {
			return nil, false, fmt.Errorf("FinishStep[%s]: PayloadBuilder: %w", s.Reason, err)
		}
		payload = v
	}

	var meta map[string]any
	if s.MetadataBuilder != nil {
		v, err := s.MetadataBuilder(rc)
		if err != nil {
			return nil, false, fmt.Errorf("FinishStep[%s]: MetadataBuilder: %w", s.Reason, err)
		}
		meta = v
	}
	if meta == nil {
		meta = map[string]any{}
	}
	// Stamp the per-call RunID into metadata so D-025 identity
	// round-trip is observable without requiring every operator to
	// remember to do it themselves. Operators that prefer pure
	// builder-driven metadata can clear the key in MetadataBuilder.
	if _, has := meta["run_id"]; !has && rc.Quadruple.RunID != "" {
		meta["run_id"] = rc.Quadruple.RunID
	}

	return planner.Finish{
		Reason:   s.Reason,
		Payload:  payload,
		Metadata: meta,
	}, true, nil
}

// ---------------------------------------------------------------------------
// PauseStep — emit a RequestPause decision.
// ---------------------------------------------------------------------------

// PauseStep is the operator-configured pause-request step. When
// `When` is nil (or returns true), the step claims the call and
// returns a [planner.RequestPause] decision built from the
// configured reason and the payload-builder closure.
//
// `Reason` MUST be one of the canonical [planner.PauseReason]
// values; an invalid reason surfaces as a step error.
type PauseStep struct {
	// Reason is the pause reason. MUST be canonical (see
	// planner.IsValidPauseReason).
	Reason planner.PauseReason
	// PayloadBuilder constructs the pause payload from the run
	// context. nil → empty Payload map.
	PayloadBuilder func(planner.RunContext) (map[string]any, error)
	// When is the optional guard. nil → always match.
	When func(planner.RunContext) bool
}

// Decide implements [DecisionTreeStep].
func (s *PauseStep) Decide(_ context.Context, rc planner.RunContext) (planner.Decision, bool, error) {
	if s.When != nil && !s.When(rc) {
		return nil, false, nil
	}
	if !planner.IsValidPauseReason(s.Reason) {
		return nil, false, fmt.Errorf("PauseStep: invalid Reason %q (not in canonical set)", s.Reason)
	}
	var payload map[string]any
	if s.PayloadBuilder != nil {
		v, err := s.PayloadBuilder(rc)
		if err != nil {
			return nil, false, fmt.Errorf("PauseStep[%s]: PayloadBuilder: %w", s.Reason, err)
		}
		payload = v
	}
	return planner.RequestPause{
		Reason:  s.Reason,
		Payload: payload,
	}, true, nil
}

// ---------------------------------------------------------------------------
// SpawnAndAwaitStep — emit SpawnTask once, then AwaitTask / OnResolved.
// ---------------------------------------------------------------------------

// spawnState tracks the per-`(SessionID, StepID)` lifecycle a
// [SpawnAndAwaitStep] traverses across multiple `Next` calls:
//   - empty: not yet spawned.
//   - spawned with groupID + ownerTaskID: emit AwaitTask on
//     subsequent calls until the group resolves.
//   - resolved: the OnResolved callback has fired; future calls of
//     the same `(SessionID, StepID)` SKIP (the step is "done").
type spawnState struct {
	// mu guards every field below. Decide locks it for the whole
	// transition so concurrent invocations sharing one
	// `(SessionID, StepID)` key are serialised — the D-025
	// concurrent-reuse contract is binary (§5): the artifact must be
	// race-free for shared concurrent use, not merely race-free when
	// callers happen to use distinct keys. Distinct keys still get
	// distinct *spawnState values, so cross-run calls never contend.
	mu          sync.Mutex
	spawned     bool
	resolved    bool
	groupID     tasks.TaskGroupID
	ownerTaskID tasks.TaskID
}

// SpawnAndAwaitStep is the operator-configured spawn-then-await
// step. The step ships the load-bearing scenario the §13 primitive-
// with-consumer policy demands for Phase 48: SpawnTask + AwaitTask
// are emitted by a real concrete planner against a real
// [tasks.TaskRegistry].
//
// Lifecycle (per `(SessionID, StepID)`):
//
//  1. First [Decide] call → claim, emit
//     [planner.SpawnTask] built from `SpecBuilder`. The step
//     resolves an ad-hoc group via
//     [tasks.TaskRegistry.ResolveOrCreateGroup] (when `GroupID` is
//     empty) and spawns a member task via
//     [tasks.TaskRegistry.Spawn]. The step persists the assigned
//     `(GroupID, TaskID)` in its internal sync.Map.
//  2. Subsequent [Decide] calls perform a non-blocking receive
//     against the group's [tasks.TaskRegistry.WatchGroup] channel:
//     - not yet ready → emit
//     [planner.AwaitTask]{TaskID: ownerTaskID} so the runtime
//     sleeps the step until the next deterministic boundary
//     (WakePoll semantics, D-032).
//     - ready → invoke `OnResolved(rc, members)`; the returned
//     decision flows through. Once OnResolved fires, the step is
//     MARKED resolved — future `Decide` calls with the same
//     `(SessionID, StepID)` SKIP (return `(nil, false, nil)`).
//
// `StepID` MUST be unique within the planner's configured step set.
// When empty, the step uses `"<spawn-and-await>"` — fine for a single
// instance but ambiguous if two `SpawnAndAwaitStep` values are
// configured; operators with multiple group-aware steps SHOULD set
// distinct StepIDs.
//
// `OnResolved` MUST be safe for concurrent use — the same step
// instance is shared across N concurrent runs against the planner.
type SpawnAndAwaitStep struct {
	// StepID is the step's unique identifier within the planner's
	// configured step set. Used as the per-(SessionID, StepID) key
	// for the spawn-state map. Default: "<spawn-and-await>".
	StepID string
	// Kind is the [tasks.TaskKind] for the spawned member task.
	// Typically [tasks.KindBackground].
	Kind tasks.TaskKind
	// SpecBuilder constructs the [planner.SpawnSpec] from the run
	// context. Required; nil → step error.
	SpecBuilder func(planner.RunContext) (planner.SpawnSpec, error)
	// GroupID is the optional pre-assigned group identifier. Empty
	// → the step creates an ad-hoc group keyed by
	// `(SessionID, StepID)`.
	GroupID tasks.TaskGroupID
	// OnResolved is invoked once the group reaches a terminal
	// state. The returned decision becomes the planner's next
	// decision. Required; nil → step error.
	OnResolved func(planner.RunContext, []tasks.MemberOutcome) (planner.Decision, error)
	// When is the optional guard. nil → always match (claim the
	// call until the step is resolved).
	When func(planner.RunContext) bool

	// Internal fields. The registry is bound by the planner's
	// constructor via bindRegistry. The state map is keyed by
	// `(SessionID, StepID)`; values are *spawnState.
	mu       sync.Mutex
	registry tasks.TaskRegistry
	states   sync.Map
}

// bindRegistry implements [registryBinder].
func (s *SpawnAndAwaitStep) bindRegistry(reg tasks.TaskRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = reg
}

func (s *SpawnAndAwaitStep) stepID() string {
	if s.StepID == "" {
		return "<spawn-and-await>"
	}
	return s.StepID
}

func (s *SpawnAndAwaitStep) stateKey(q identity.Quadruple) string {
	return q.SessionID + "::" + s.stepID()
}

func (s *SpawnAndAwaitStep) getState(q identity.Quadruple) *spawnState {
	key := s.stateKey(q)
	if existing, ok := s.states.Load(key); ok {
		st, _ := existing.(*spawnState) //nolint:errcheck // states map values are always *spawnState by construction
		return st
	}
	fresh := &spawnState{}
	actual, _ := s.states.LoadOrStore(key, fresh)
	st, _ := actual.(*spawnState) //nolint:errcheck // states map values are always *spawnState by construction
	return st
}

// Decide implements [DecisionTreeStep]. See type godoc for the
// lifecycle semantics.
func (s *SpawnAndAwaitStep) Decide(ctx context.Context, rc planner.RunContext) (planner.Decision, bool, error) {
	if s.When != nil && !s.When(rc) {
		return nil, false, nil
	}
	if s.SpecBuilder == nil {
		return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: SpecBuilder required", s.stepID())
	}
	if s.OnResolved == nil {
		return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: OnResolved required", s.stepID())
	}
	if s.registry == nil {
		return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: registry not bound (constructor bug)", s.stepID())
	}

	state := s.getState(rc.Quadruple)

	// Per-`(SessionID, StepID)` state transitions forward only:
	//   empty → spawned → resolved.
	// Concurrent reuse across runs uses distinct map keys (the
	// `(SessionID, StepID)` tuple) → distinct *spawnState values.
	// state.mu serialises concurrent invocations that DO share a
	// key, so the field reads/writes below are race-free under the
	// D-025 contract regardless of how callers key their runs.
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.resolved {
		return nil, false, nil
	}

	// First call: emit SpawnTask. The step also actually spawns the
	// task in the registry so the WatchGroup the subsequent calls
	// poll has a real group to resolve.
	if !state.spawned {
		spec, err := s.SpecBuilder(rc)
		if err != nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: SpecBuilder: %w", s.stepID(), err)
		}

		groupID := s.GroupID
		groupReq := tasks.GroupRequest{
			ID:          groupID,
			SessionID:   rc.Quadruple.Identity,
			OwnerTaskID: tasks.TaskID(rc.Quadruple.RunID),
			RetainTurn:  spec.RetainTurn,
			FailFast:    spec.FailFast,
			Description: spec.Description,
		}
		spawnCtx := ctxWithIdentity(ctx, rc.Quadruple)
		group, err := s.registry.ResolveOrCreateGroup(spawnCtx, groupReq)
		if err != nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: ResolveOrCreateGroup: %w", s.stepID(), err)
		}
		groupID = group.ID

		spawnReq := tasks.SpawnRequest{
			Identity:       rc.Quadruple,
			Kind:           s.kindOrBackground(),
			Description:    spec.Description,
			Query:          spec.Query,
			Priority:       spec.Priority,
			IdempotencyKey: s.stateKey(rc.Quadruple),
			GroupID:        groupID,
		}
		handle, err := s.registry.Spawn(spawnCtx, spawnReq)
		if err != nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: Spawn: %w", s.stepID(), err)
		}

		// Seal the ad-hoc group so it resolves when the member
		// reaches a terminal state. (Open groups never resolve
		// automatically because more members might join.)
		if err := s.registry.SealGroup(spawnCtx, groupID); err != nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: SealGroup: %w", s.stepID(), err)
		}

		state.spawned = true
		state.groupID = groupID
		state.ownerTaskID = handle.ID

		decision := planner.SpawnTask{
			Kind:    s.kindOrBackground(),
			Spec:    spec,
			GroupID: groupID,
		}
		return decision, true, nil
	}

	// Subsequent calls: poll the group via WatchGroup.
	ch, cancel, err := s.registry.WatchGroup(rc.Quadruple.Identity, state.groupID)
	if err != nil {
		return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: WatchGroup: %w", s.stepID(), err)
	}
	defer cancel()

	// Non-blocking receive (WakePoll semantics, D-032). The
	// runtime's foreground turn never blocks here.
	select {
	case completion, ok := <-ch:
		if !ok {
			// Channel closed without a delivery — the group was
			// cancelled before resolving; surface as AwaitTask so
			// the runtime decides whether to retry / give up. The
			// alternative (emit Finish{NoPath}) would prevent the
			// runtime engine's escalation policy from running.
			return planner.AwaitTask{TaskID: state.ownerTaskID}, true, nil
		}
		dec, err := s.OnResolved(rc, completion.Members)
		if err != nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: OnResolved: %w", s.stepID(), err)
		}
		state.resolved = true
		if dec == nil {
			return nil, false, fmt.Errorf("SpawnAndAwaitStep[%s]: OnResolved returned nil decision", s.stepID())
		}
		return dec, true, nil
	default:
		// Group not yet resolved — emit AwaitTask. The runtime
		// engine sleeps the step until the next deterministic
		// boundary; the next `Next` call re-polls.
		return planner.AwaitTask{TaskID: state.ownerTaskID}, true, nil
	}
}

func (s *SpawnAndAwaitStep) kindOrBackground() tasks.TaskKind {
	if s.Kind == "" {
		return tasks.KindBackground
	}
	return s.Kind
}

// ---------------------------------------------------------------------------
// WatchGroupStep — wait on a pre-existing group.
// ---------------------------------------------------------------------------

// WatchGroupStep is the operator-configured "I am waiting on this
// pre-existing group" step. Distinct from [SpawnAndAwaitStep]: this
// step does NOT spawn — it expects the group to exist already (the
// runtime engine, or another planner step earlier in the tree,
// created it). The step performs the WakePoll non-blocking receive
// per `Decide` call:
//
//   - not yet ready → emit
//     [planner.AwaitTask]{TaskID: OwnerTaskID}.
//   - ready → invoke `OnResolved(rc, members)`; once it fires the
//     step is marked resolved and SKIPS future calls of the same
//     `(SessionID, OwnerTaskID)`.
type WatchGroupStep struct {
	// GroupID is the pre-existing group identifier.
	GroupID tasks.TaskGroupID
	// OwnerTaskID is the task whose lifecycle the planner reports
	// as the AwaitTask target. Typically the spawn handle's TaskID
	// surfaced by an earlier SpawnTask emission.
	OwnerTaskID tasks.TaskID
	// OnResolved is invoked once the group reaches a terminal
	// state. Required; nil → step error.
	OnResolved func(planner.RunContext, []tasks.MemberOutcome) (planner.Decision, error)
	// When is the optional guard. nil → always match.
	When func(planner.RunContext) bool

	mu       sync.Mutex
	registry tasks.TaskRegistry
	resolved sync.Map // map[string]bool keyed by SessionID
}

// bindRegistry implements [registryBinder].
func (s *WatchGroupStep) bindRegistry(reg tasks.TaskRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = reg
}

// Decide implements [DecisionTreeStep].
func (s *WatchGroupStep) Decide(ctx context.Context, rc planner.RunContext) (planner.Decision, bool, error) {
	if s.When != nil && !s.When(rc) {
		return nil, false, nil
	}
	if s.GroupID == "" {
		return nil, false, fmt.Errorf("WatchGroupStep: GroupID required")
	}
	if s.OnResolved == nil {
		return nil, false, fmt.Errorf("WatchGroupStep[%s]: OnResolved required", s.GroupID)
	}
	if s.registry == nil {
		return nil, false, fmt.Errorf("WatchGroupStep[%s]: registry not bound (constructor bug)", s.GroupID)
	}

	key := rc.Quadruple.SessionID + "::" + string(s.GroupID)
	if v, has := s.resolved.Load(key); has {
		if done, _ := v.(bool); done { //nolint:errcheck // resolved map values are always bool by construction
			return nil, false, nil
		}
	}

	ch, cancel, err := s.registry.WatchGroup(rc.Quadruple.Identity, s.GroupID)
	if err != nil {
		return nil, false, fmt.Errorf("WatchGroupStep[%s]: WatchGroup: %w", s.GroupID, err)
	}
	defer cancel()

	select {
	case completion, ok := <-ch:
		if !ok {
			return planner.AwaitTask{TaskID: s.OwnerTaskID}, true, nil
		}
		dec, err := s.OnResolved(rc, completion.Members)
		if err != nil {
			return nil, false, fmt.Errorf("WatchGroupStep[%s]: OnResolved: %w", s.GroupID, err)
		}
		s.resolved.Store(key, true)
		if dec == nil {
			return nil, false, fmt.Errorf("WatchGroupStep[%s]: OnResolved returned nil decision", s.GroupID)
		}
		return dec, true, nil
	default:
		return planner.AwaitTask{TaskID: s.OwnerTaskID}, true, nil
	}
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// ctxWithIdentity attaches the run's identity (the triple) to ctx
// so the TaskRegistry's `identity.From(ctx)` pathway sees the
// planner's quadruple. Returns the bare ctx when the identity is
// empty (the planner's identity-mandatory pre-check would have
// already rejected such a call).
func ctxWithIdentity(ctx context.Context, q identity.Quadruple) context.Context {
	if q.SessionID == "" {
		return ctx
	}
	withID, err := identity.With(ctx, q.Identity)
	if err != nil {
		// The identity-mandatory pre-check should have caught any
		// shape that fails here. Surface the bare ctx — the
		// registry will reject with ErrIdentityRequired and the
		// planner will wrap it as a step error.
		return ctx
	}
	return withID
}
