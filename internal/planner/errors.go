package planner

import "errors"

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrTrajectoryNotImplemented is returned by Trajectory.Serialize
	// at Phase 42 — the fail-loudly contract closes at Phase 43.
	// Callers fail loudly rather than receiving an empty payload.
	ErrTrajectoryNotImplemented = errors.New("planner: Trajectory.Serialize not implemented at Phase 42 (Phase 43 closes the contract)")

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
