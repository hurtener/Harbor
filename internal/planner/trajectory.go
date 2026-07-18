package planner

import "github.com/hurtener/Harbor/internal/planner/trajectory"

// Trajectory and the associated types/sentinels are re-exported from
// the canonical home at internal/planner/trajectory. Harbor moves
// the load-bearing implementation into the subpackage; the aliases
// below keep existing planner-package consumers compiling without
// changes (RunContext.Trajectory *Trajectory, etc.).
//
// The fail-loudly Serialize contract (RFC §6.2 + §3.4) lives at
// internal/planner/trajectory/serialize.go; the handle registry lives
// at internal/planner/trajectory/registry.go. The earlier stub
// ErrTrajectoryNotImplemented is retired — the real contract replaces
// it.

// Trajectory is re-exported from the canonical subpackage.
type Trajectory = trajectory.Trajectory

// Step is the trajectory's per-step shape (action + observation +
// failure + streams). Re-exported from the canonical subpackage.
//
// Note: earlier, this type was named TrajectoryStep at the
// planner-package level. The rename to Step is part of the
// subpackage relocation; no external consumers of TrajectoryStep
// existed in an earlier revision.
type Step = trajectory.Step

// Summary is the trajectory's compaction artefact (
// summariser output). Earlier this was TrajectorySummary at the
// planner-package level; the subpackage rename to Summary is part of
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

// FailureRecord is the structured-failure projection (repair).
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

// CountToolInvocations counts the tool invocations ATTEMPTED across
// t's steps — the counting rule behind AnswerEnvelope.ToolCallsSeen.
// A CallTool step counts as one invocation; a CallParallel step counts
// as len(Branches) (the runtime dispatches N tools concurrently, not
// one); a Batch step counts as len(Tools) (its spawns are not tool
// dispatches); SpawnTask, AwaitTask, and any other step value count as
// zero — they are runtime decisions the planner made, never tool
// dispatches. A nil Trajectory counts as zero.
//
// The Console Tasks-page tool_count uses the SAME per-decision counting
// rule (DecisionInvocationCount) but differs on the failure axis: the
// run loop skips its dispatch hook when the executor errors, so
// tool_count counts SUCCESSFUL dispatches only, while a failed dispatch
// still appends its step to the trajectory and is counted here. A run
// with three CallTool steps of which one failed reports ToolCallsSeen
// == 3 and tool_count == 2 — by design, not drift.
//
// Producers call this on a LIVE, in-process trajectory, whose
// Step.Action values are the concrete Decision shapes the run loop
// appended. A trajectory rehydrated from serialized bytes carries
// map-shaped Actions (see Trajectory.Serialize) and would count 0 —
// any future cross-process-resume producer must re-type Actions or
// count before serialization, deliberately, not by accident.
//
// This replaces an earlier len(t.Steps) reading, which silently
// miscounted in both directions: one CallParallel step holding N
// branches undercounted (reported 1 instead of N), and a SpawnTask /
// AwaitTask step overcounted (reported a dispatch that never happened).
func CountToolInvocations(t *Trajectory) int {
	if t == nil {
		return 0
	}
	n := 0
	for _, step := range t.Steps {
		n += DecisionInvocationCount(step.Action)
	}
	return n
}

// DecisionInvocationCount returns the number of tool invocations a
// single decision represents: 1 for CallTool, len(Branches) for
// CallParallel, len(Tools) for Batch (its Spawns contribute zero,
// matching SpawnTask's rule — a spawn is never a tool invocation), 0
// for SpawnTask / AwaitTask / TaskStatusQuery / CancelTask /
// RequestPause / Finish / any other value
// (including a trajectory step restored from JSON without being
// re-typed through the Decision sum — e.g. a bare map[string]any).
// Typed-nil pointers count as zero.
//
// action is typed `any`, not Decision: trajectory.Step.Action is `any`
// to avoid a planner<->trajectory import cycle (see Step's godoc); the
// type switch here is the seam that recovers the concrete Decision
// shape when the caller has one (a live, in-process Step.Action, or a
// Decision passed directly from a run-loop dispatch site).
func DecisionInvocationCount(action any) int {
	switch d := action.(type) {
	case CallTool:
		return 1
	case *CallTool:
		if d == nil {
			return 0
		}
		return 1
	case CallParallel:
		return len(d.Branches)
	case *CallParallel:
		if d == nil {
			return 0
		}
		return len(d.Branches)
	case Batch:
		return len(d.Tools)
	case *Batch:
		if d == nil {
			return 0
		}
		return len(d.Tools)
	default:
		return 0
	}
}
