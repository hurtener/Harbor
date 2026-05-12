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
)
