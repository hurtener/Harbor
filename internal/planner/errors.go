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
)
