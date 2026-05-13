package planner

import "errors"

// Sentinel errors. Callers compare via errors.Is.
//
// Trajectory-shaped sentinels (ErrUnserializable, ErrToolContextLost)
// live in the canonical subpackage internal/planner/trajectory and
// are re-exported as type aliases at trajectory.go in this package.
// Pre-Phase-43 stub ErrTrajectoryNotImplemented is retired — Phase 43
// ships the real fail-loudly Serialize contract that replaces it.
var (
	// ErrPlannerClosed — operations against a planner whose Close()
	// has been called. Reserved for future planner concretes that
	// hold lifecycle resources (HTTP client pools, LLM driver
	// sessions, etc.).
	ErrPlannerClosed = errors.New("planner: planner closed")

	// ErrInvalidDecision — the planner returned a Decision the
	// Runtime cannot dispatch (unknown FinishReason / PauseReason,
	// CallTool with empty Tool name, CallParallel with zero
	// branches). The Runtime rejects with this sentinel before
	// dispatching the decision.
	ErrInvalidDecision = errors.New("planner: invalid decision")

	// ErrRepairExhausted (Phase 44) — surfaced by the
	// `internal/planner/repair.RepairLoop` for callers that want to
	// inspect the graceful-failure path before the loop's terminal
	// `Finish{NoPath}` is dispatched. The loop's `Run` returns
	// (Decision, nil) on graceful failure — Finish IS the success path
	// — but the wrapped sentinel is available via the Finish.Metadata
	// `repair_error` slot for observability sinks that prefer error-
	// shaped reads. Compare via `errors.Is`. The fail-loudly emit
	// (`planner.repair_exhausted`) is the canonical observability
	// surface; this sentinel is the secondary read surface.
	ErrRepairExhausted = errors.New("planner: schema repair exhausted")

	// ErrIdentityRequired (Phase 48) — a concrete planner observed a
	// `RunContext.Quadruple` missing one of the four scope components
	// (tenant / user / session / run). Identity is mandatory at every
	// planner boundary (§6 rule 9 + D-001). Concrete planners SHOULD
	// wrap this sentinel with their context so the runtime executor
	// can surface a precise failure (e.g. `fmt.Errorf("%w (missing
	// run_id)", planner.ErrIdentityRequired)`). The deterministic
	// planner is the first emitter; future concretes that enforce
	// identity at Next boundary consume the same sentinel.
	ErrIdentityRequired = errors.New("planner: identity required (tenant/user/session/run)")

	// ErrInvalidConfig (Phase 48) — a concrete planner's constructor
	// rejected the supplied configuration (empty step set, missing
	// dependency, contradictory options). The fail-loudly contract:
	// a malformed configuration MUST surface at construction time,
	// NEVER at Next time. Wrapping per-concrete with structural
	// context (`fmt.Errorf("%w: WithSteps required at least one
	// step", planner.ErrInvalidConfig)`) is the recommended pattern.
	ErrInvalidConfig = errors.New("planner: invalid configuration")

	// ErrDeterministicStep (Phase 48) — a `DecisionTreeStep` inside
	// the deterministic planner's walker returned a non-nil error.
	// The planner wraps the step's error with this sentinel so
	// callers can distinguish a structural step failure from a
	// transport-level error returned by other concretes. Fail-loudly
	// per §13 — the walker does NOT skip a failing step (a silent
	// skip would mask operator bugs in the tree).
	ErrDeterministicStep = errors.New("planner/deterministic: step returned error")
	// ErrParallelCapExceeded (Phase 47, D-056) — a CallParallel was
	// emitted with more branches than AbsoluteMaxParallel. The runtime
	// parallel executor rejects with this sentinel BEFORE any branch
	// dispatches — atomic-setup-validation discipline per RFC §6.2.
	// Fail-loudly per §13; never silently truncates the branch list.
	ErrParallelCapExceeded = errors.New("planner: CallParallel branch count exceeds absolute_max_parallel")

	// ErrParallelInvalidJoin (Phase 47, D-056) — a CallParallel was
	// emitted with a JoinSpec whose shape is malformed (e.g. JoinN
	// with N ≤ 0 or N > len(Branches), or an unknown JoinKind). The
	// executor rejects at setup time, before any branch dispatches.
	ErrParallelInvalidJoin = errors.New("planner: CallParallel join spec invalid")

	// ErrParallelBranchInvalidArgs (Phase 47, D-056) — atomic-setup
	// validation: ANY branch's args fail the descriptor's validator,
	// the whole CallParallel fails before execution. The wrapped error
	// names the offending branch index + the upstream validator error.
	ErrParallelBranchInvalidArgs = errors.New("planner: CallParallel branch failed atomic-setup validation")

	// ErrParallelPauseUnsupported (Phase 47, D-056) — a branch requested
	// a pause mid-execution. The unified pause/resume primitive lands
	// at Phase 50; until then, the parallel executor MUST fail loud
	// per RFC §6.2's parallel-pause-atomicity contract. Phase 50
	// upgrades this path to a checkpointed atomic pause.
	ErrParallelPauseUnsupported = errors.New("planner: CallParallel pause-mid-execution not supported until Phase 50 unified pause primitive")
)

// AbsoluteMaxParallel is the system cap on CallParallel branch counts
// (RFC §6.2 — settled). Any CallParallel with more branches fails the
// whole call with ErrParallelCapExceeded BEFORE execution. The cap is
// defence in depth — the planner's PlanningHints.MaxParallel is the
// soft cap; this constant is the hard cap that protects the runtime
// from a runaway emission (a buggy LLM emitting 1000 branches).
//
// D-056 — Phase 47 settles the value at 50.
const AbsoluteMaxParallel = 50
