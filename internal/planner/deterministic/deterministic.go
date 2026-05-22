// Package deterministic ships Harbor's second concrete Planner
// (Phase 48 — RFC §6.2 + RFC §11 Q-6 — the iface-validation lens that
// proves the `internal/planner.Planner` seam is genuinely swappable).
//
// The same Runtime that drives the Phase 45 LLM-driven ReAct concrete
// drives this deterministic concrete via the identical `Planner`
// interface, the identical `RunContext` view, and the identical
// `Decision` sum. NO Runtime change. NO interface change. NO
// `Decision` shape change. CLAUDE.md §1 property 3 holds.
//
// # Decision-tree model
//
// The deterministic planner is configured with an ordered slice of
// [DecisionTreeStep] values. Each step has a `Decide(ctx, rc)
// (Decision, bool, error)` method; the boolean reports whether the
// step claimed the current `Next` call. On every `Next`, the walker:
//
//  1. Honours `ctx.Err()`.
//  2. Validates [planner.RunContext.Quadruple] (§6 rule 9 + D-001;
//     fail-loudly per §13 with wrapped [planner.ErrIdentityRequired]).
//  3. Observes [planner.RunContext.Control.Cancelled] — returns
//     `Finish{Cancelled}` at the step boundary per RFC §6.3.
//  4. Walks the step set in order. First step returning
//     `(decision, true, nil)` wins. Steps returning
//     `(nil, false, nil)` are skipped. A step returning
//     `(_, _, err)` propagates the error wrapped with
//     [planner.ErrDeterministicStep] (fail-loudly — no silent skip).
//  5. If no step claims the call, the walker returns
//     `Finish{NoPath, Metadata["deterministic"]="no_step_matched"}`.
//     A misconfigured tree surfaces as a typed terminal, NEVER a
//     silent loop (§13).
//
// # Wake-on-resolution (D-032)
//
// The deterministic planner declares [planner.WakePoll] via the
// [planner.WakeAware] interface. The [WatchGroupStep] (and the
// composed [SpawnAndAwaitStep] after its first invocation) perform a
// non-blocking receive against the channel returned by
// [tasks.TaskRegistry.WatchGroup]:
//
//   - Channel not yet readable → emit
//     `AwaitTask{TaskID: OwnerTaskID}`; the runtime will re-invoke
//     `Next` at the next deterministic boundary.
//   - Channel readable → consume the typed
//     [tasks.GroupCompletion]'s `Members` slice and invoke the
//     operator-supplied `OnResolved` callback whose return value is
//     the planner's decision.
//
// The TaskRegistry stays NEUTRAL (D-032): no `WakeMode` field on
// registry types, no `Supports*` capability protocol. The choice is
// the planner concrete's; the deterministic concrete picks `poll`
// because the WakePoll mode is the on-disk proof that the registry's
// mode-neutral surface accepts a poller.
//
// # Concurrent reuse (D-025)
//
// [DeterministicPlanner] is a reusable artifact: the receiver is
// read-only after construction. Per-run state lives on the stack and
// in the [planner.RunContext] argument. [SpawnAndAwaitStep] holds an
// internal `sync.Map` for per-`(SessionID, StepID)` spawn-tracking,
// keyed so concurrent reuse across runs is safe.
// `d025_test.go` pins N=128 invocations under `-race`.
//
// # Import-graph contract (§13)
//
// The deterministic package MUST NOT import `internal/runtime/...`
// or `internal/llm/...`. The Phase 42
// [internal/planner/conformance.TestImportGraph_PlannerDoesNotImportRuntime]
// covers the new package by construction; `scripts/smoke/phase-48.sh`
// asserts the same via grep on both forbidden prefixes.
package deterministic

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// DefaultName is the [DeterministicPlanner.Name] returned when
// [WithName] is not supplied.
const DefaultName = "deterministic"

// Option configures a [DeterministicPlanner] at construction time.
type Option func(*config)

// config is the internal aggregate of operator-supplied knobs. The
// constructor copies fields onto the [DeterministicPlanner] struct;
// the planner does NOT retain the *config (so operator mutation
// post-construction cannot affect runtime behaviour).
type config struct {
	registry tasks.TaskRegistry
	name     string
	steps    []DecisionTreeStep
}

// WithSteps sets the ordered decision-tree step set. At least one
// step is required at construction time; an empty set surfaces as
// wrapped [planner.ErrInvalidConfig] from [NewDeterministicPlanner].
func WithSteps(steps ...DecisionTreeStep) Option {
	return func(c *config) {
		c.steps = steps
	}
}

// WithRegistry sets the [tasks.TaskRegistry] handle that group-aware
// steps poll via [tasks.TaskRegistry.WatchGroup]. Required when any
// configured step is a [SpawnAndAwaitStep] or a [WatchGroupStep];
// [NewDeterministicPlanner] returns wrapped
// [planner.ErrInvalidConfig] if a group-aware step is configured
// without a registry.
func WithRegistry(reg tasks.TaskRegistry) Option {
	return func(c *config) {
		c.registry = reg
	}
}

// WithName sets the planner's human-readable identifier (audit +
// observability). Default: [DefaultName].
func WithName(name string) Option {
	return func(c *config) {
		if name != "" {
			c.name = name
		}
	}
}

// DeterministicPlanner is Harbor's second concrete Planner. The
// receiver is read-only after construction; per-call state lives in
// `ctx` + [planner.RunContext]. See package godoc for the wake-mode
// contract and the decision-tree walker semantics.
type DeterministicPlanner struct {
	registry tasks.TaskRegistry
	name     string
	steps    []DecisionTreeStep
}

// Compile-time assertions: DeterministicPlanner satisfies both
// [planner.Planner] and [planner.WakeAware].
var (
	_ planner.Planner   = (*DeterministicPlanner)(nil)
	_ planner.WakeAware = (*DeterministicPlanner)(nil)
)

// NewDeterministicPlanner constructs a [DeterministicPlanner].
// Returns wrapped [planner.ErrInvalidConfig] when:
//
//   - the configured step set is empty;
//   - any configured step is group-aware ([SpawnAndAwaitStep] /
//     [WatchGroupStep]) and [WithRegistry] was not supplied;
//   - any configured step is nil.
//
// Fail-loudly per §13: configuration errors surface at construction
// time, NEVER at `Next` time.
func NewDeterministicPlanner(opts ...Option) (*DeterministicPlanner, error) {
	cfg := config{
		name: DefaultName,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if len(cfg.steps) == 0 {
		return nil, fmt.Errorf("%w: WithSteps must supply at least one step", planner.ErrInvalidConfig)
	}

	needsRegistry := false
	for i, step := range cfg.steps {
		if step == nil {
			return nil, fmt.Errorf("%w: step[%d] is nil", planner.ErrInvalidConfig, i)
		}
		if requiresRegistry(step) {
			needsRegistry = true
		}
	}
	if needsRegistry && cfg.registry == nil {
		return nil, fmt.Errorf(
			"%w: group-aware step configured (SpawnAndAwaitStep / WatchGroupStep) but WithRegistry was not supplied",
			planner.ErrInvalidConfig,
		)
	}

	// Wire each group-aware step's registry handle. The step types
	// hold an unexported registry pointer so the operator never has
	// to thread it through Decide.
	for _, step := range cfg.steps {
		bindRegistry(step, cfg.registry)
	}

	return &DeterministicPlanner{
		steps:    cfg.steps,
		registry: cfg.registry,
		name:     cfg.name,
	}, nil
}

// Name returns the planner's human-readable identifier.
func (p *DeterministicPlanner) Name() string {
	return p.name
}

// WakeMode declares the planner's wake-on-resolution strategy
// (D-032 + Phase 48 spec). Deterministic ships the `poll` mode: each
// `Next` invocation performs a non-blocking receive on its
// outstanding group's [tasks.TaskRegistry.WatchGroup] channel; not
// ready → emit `AwaitTask`, the runtime sleeps the step until the
// next deterministic boundary; ready → consume `MemberOutcome` and
// proceed. No LLM, no eager wake — a clean deterministic shape that
// proves the registry's `WatchGroup` surface is mode-neutral.
func (p *DeterministicPlanner) WakeMode() planner.WakeMode {
	return planner.WakePoll
}

// Next implements [planner.Planner]. The flow is documented in the
// package godoc.
func (p *DeterministicPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := assertIdentity(rc); err != nil {
		return nil, err
	}

	// Steering: a CANCEL observation at the step boundary returns
	// Finish{Cancelled} before any step is walked (RFC §6.3 —
	// steering is drained between steps; the planner observes
	// Control signals).
	if rc.Control.Cancelled {
		return planner.Finish{
			Reason: planner.FinishCancelled,
			Metadata: map[string]any{
				"steering": "cancelled",
				"run_id":   rc.Quadruple.RunID,
			},
		}, nil
	}

	// Walk the decision tree. The first claiming step wins; any
	// step returning a non-nil error propagates loudly (§13).
	for i, step := range p.steps {
		decision, claimed, err := step.Decide(ctx, rc)
		if err != nil {
			return nil, fmt.Errorf("%w: step[%d]: %w", planner.ErrDeterministicStep, i, err)
		}
		if claimed {
			if decision == nil {
				// A step that claims the call but returns a nil
				// decision is structurally broken — surface it as
				// a step error rather than letting the runtime
				// receive a nil Decision.
				return nil, fmt.Errorf(
					"%w: step[%d] claimed call with nil decision (structural bug)",
					planner.ErrDeterministicStep, i,
				)
			}
			return decision, nil
		}
	}

	// No step claimed the call. Fail-loudly: a tree that exhausts
	// every step is misconfigured for the current RunContext state.
	// The Finish surfaces it as a typed terminal, NEVER a silent
	// loop. (§13)
	return planner.Finish{
		Reason: planner.FinishNoPath,
		Metadata: map[string]any{
			"deterministic": "no_step_matched",
			"run_id":        rc.Quadruple.RunID,
		},
	}, nil
}

// assertIdentity rejects calls whose [planner.RunContext.Quadruple]
// is missing any of the four scope components. Returns wrapped
// [planner.ErrIdentityRequired] (§6 rule 9 + D-001). Fail-loudly per
// §13 — a planner with missing identity fails closed, never silently
// degrades.
func assertIdentity(rc planner.RunContext) error {
	q := rc.Quadruple
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" || q.RunID == "" {
		return fmt.Errorf(
			"%w (deterministic planner refuses missing-identity Next: tenant=%q user=%q session=%q run=%q)",
			planner.ErrIdentityRequired,
			q.TenantID, q.UserID, q.SessionID, q.RunID,
		)
	}
	return nil
}
