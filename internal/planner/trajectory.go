package planner

import "github.com/hurtener/Harbor/internal/planner/trajectory"

// Trajectory and the associated types/sentinels are re-exported from
// the canonical home at internal/planner/trajectory. Phase 43 moves
// the load-bearing implementation into the subpackage; the aliases
// below keep existing planner-package consumers compiling without
// changes (RunContext.Trajectory *Trajectory, etc.).
//
// The fail-loudly Serialize contract (RFC §6.2 + §3.4) lives at
// internal/planner/trajectory/serialize.go; the handle registry lives
// at internal/planner/trajectory/registry.go. Pre-Phase-43 stub
// ErrTrajectoryNotImplemented is retired — the real contract replaces
// it. See D-049.

// Trajectory is re-exported from the canonical subpackage.
type Trajectory = trajectory.Trajectory

// Step is the trajectory's per-step shape (action + observation +
// failure + streams). Re-exported from the canonical subpackage.
//
// Note: pre-Phase-43, this type was named TrajectoryStep at the
// planner-package level. The Phase 43 rename to Step is part of the
// subpackage relocation; no external consumers of TrajectoryStep
// existed pre-Phase-43.
type Step = trajectory.Step

// Summary is the trajectory's compaction artefact (Phase 46
// summariser output). Pre-Phase-43 this was TrajectorySummary at the
// planner-package level; the subpackage rename to Summary is part of
// Phase 43.
type Summary = trajectory.Summary

// ToolContext is the split serialisable / handle-registry tool-handle
// bundle. Re-exported from the canonical subpackage.
type ToolContext = trajectory.ToolContext

// HandleID is the opaque key for a non-serialisable tool-context
// handle. Re-exported from the canonical subpackage.
type HandleID = trajectory.HandleID

// Source records a citation / provenance entry. Re-exported.
type Source = trajectory.Source

// SteeringInjection records a steering event the planner observed.
type SteeringInjection = trajectory.SteeringInjection

// BackgroundResult is the planner's projection of a resolved
// non-retain-turn task group.
type BackgroundResult = trajectory.BackgroundResult

// BackgroundMemberOutcome is the per-member outcome inside a
// BackgroundResult.
type BackgroundMemberOutcome = trajectory.BackgroundMemberOutcome

// ResumeHint signals a resume continuation.
type ResumeHint = trajectory.ResumeHint

// FailureRecord is the structured-failure projection (Phase 44 repair).
type FailureRecord = trajectory.FailureRecord

// StreamChunk captures one chunk of a streaming output.
type StreamChunk = trajectory.StreamChunk

// ErrUnserializable is the fail-loudly sentinel returned by
// Trajectory.Serialize on a non-JSON-encodable leaf. Re-exported
// from the canonical subpackage; use errors.As to extract the Field
// path.
type ErrUnserializable = trajectory.ErrUnserializable

// ErrToolContextLost is the fail-loudly sentinel returned by
// HandleRegistry.Get on a missing handle. Re-exported from the
// canonical subpackage.
type ErrToolContextLost = trajectory.ErrToolContextLost

// HandleRegistry holds the non-serialisable half of ToolContext.
// V1 ships a process-local driver; the distributed driver is a
// post-V1 RFC concern.
type HandleRegistry = trajectory.HandleRegistry

// NewProcessLocalRegistry constructs the V1 process-local
// HandleRegistry driver.
func NewProcessLocalRegistry() HandleRegistry {
	return trajectory.NewProcessLocalRegistry()
}
