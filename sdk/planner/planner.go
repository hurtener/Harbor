// Package planner is the public SDK facade over Harbor's
// internal/planner package — the swappable Planner interface, the
// Decision sum, the RunContext view, the Trajectory record, and the
// driver registry external planner concretes register on (RFC §3.6,
// §6.2; D-204). Alias-based re-exports only: no behavior lives here.
// Event payload structs, prompt-materialisation helpers, and repair
// internals are deliberately private.
package planner

import (
	internal "github.com/hurtener/Harbor/internal/planner"
)

// The planner contract — aliases of the internal types.
type (
	// Planner is the swappable reasoning-policy interface (CLAUDE.md
	// §1 property 3). Concretes implement Next(ctx, rc) (Decision, error).
	Planner = internal.Planner
	// RunContext is the per-step view the runtime projects for the
	// planner (identity, goal, trajectory, catalog, memory, skills).
	RunContext = internal.RunContext
	// Decision is the sealed decision sum a Planner returns.
	Decision = internal.Decision
	// Finish is the terminal decision.
	Finish = internal.Finish
	// FinishReason is the canonical terminal reason.
	FinishReason = internal.FinishReason
	// CallTool dispatches one tool.
	CallTool = internal.CallTool
	// CallParallel dispatches N tool branches with a join spec.
	CallParallel = internal.CallParallel
	// SpawnTask spawns a background task (twinned with AwaitTask).
	SpawnTask = internal.SpawnTask
	// AwaitTask joins a previously spawned task.
	AwaitTask = internal.AwaitTask
	// RequestPause parks the run on the unified pause/resume primitive.
	RequestPause = internal.RequestPause
	// ToolCallDeferred records a deferred (approval-gated) tool call.
	ToolCallDeferred = internal.ToolCallDeferred
	// PauseReason is the canonical pause vocabulary.
	PauseReason = internal.PauseReason
	// SpawnSpec describes the task a SpawnTask decision spawns.
	SpawnSpec = internal.SpawnSpec
	// JoinSpec describes how CallParallel branches join.
	JoinSpec = internal.JoinSpec
	// JoinKind discriminates JoinSpec strategies.
	JoinKind = internal.JoinKind
)

// Trajectory + run-state vocabulary — aliases of the internal types.
type (
	// Trajectory is the append-only execution log of a run.
	Trajectory = internal.Trajectory
	// Step is one trajectory step.
	Step = internal.Step
	// Summary is the compaction artefact of a compressed trajectory.
	Summary = internal.Summary
	// Source records a step's provenance.
	Source = internal.Source
	// StreamChunk is one chunked-output fragment on the trajectory.
	StreamChunk = internal.StreamChunk
	// ChunkKind discriminates StreamChunk kinds.
	ChunkKind = internal.ChunkKind
	// SteeringInjection is an INJECT_CONTEXT record on the trajectory.
	SteeringInjection = internal.SteeringInjection
	// ResumeHint carries the pause's resume guidance.
	ResumeHint = internal.ResumeHint
	// FailureRecord records one repaired/observed failure.
	FailureRecord = internal.FailureRecord
	// BackgroundResult is a joined background task's outcome.
	BackgroundResult = internal.BackgroundResult
	// BackgroundMemberOutcome is one member of a joined group.
	BackgroundMemberOutcome = internal.BackgroundMemberOutcome
	// ToolContext is the tool-only handle bundle (serialisable half +
	// handle registry).
	ToolContext = internal.ToolContext
	// HandleID identifies one registered non-serialisable handle.
	HandleID = internal.HandleID
	// HandleRegistry stores non-serialisable handles across steps.
	HandleRegistry = internal.HandleRegistry
	// ErrUnserializable is the loud pause/resume serialization failure
	// (CLAUDE.md §5 fail-loudly; never a silent context drop).
	ErrUnserializable = internal.ErrUnserializable
	// ErrToolContextLost is the loud lost-handle failure on resume.
	ErrToolContextLost = internal.ErrToolContextLost
	// RepairCounters carries the per-run schema-repair counters.
	RepairCounters = internal.RepairCounters
	// ControlSignals is the steering snapshot the runtime projects.
	ControlSignals = internal.ControlSignals
	// PlanningNudges are caller-provided ordering/parallel/budget nudges.
	PlanningNudges = internal.PlanningNudges
	// PlanningHints are runtime-supplied planning constraints.
	PlanningHints = internal.PlanningHints
	// Budget carries the run's enforced budget caps.
	Budget = internal.Budget
	// BudgetHints carries advisory budget guidance.
	BudgetHints = internal.BudgetHints
	// ParallelObservation is the joined CallParallel observation.
	ParallelObservation = internal.ParallelObservation
	// ParallelBranchObservation is one branch's observation.
	ParallelBranchObservation = internal.ParallelBranchObservation
)

// RunContext view interfaces + projections — aliases of the internal types.
type (
	// MemoryView is the read-only memory view a planner sees.
	MemoryView = internal.MemoryView
	// MemoryBlocks is the projected memory block set.
	MemoryBlocks = internal.MemoryBlocks
	// ToolCatalogView is the identity-filtered catalog view.
	ToolCatalogView = internal.ToolCatalogView
	// SkillLookup is the read-only skill lookup a planner sees.
	SkillLookup = internal.SkillLookup
	// Skill is the planner-visible skill projection.
	Skill = internal.Skill
	// SkillResult is one planner-visible skill search hit.
	SkillResult = internal.SkillResult
	// InputArtifactView is one planner-visible input artifact.
	InputArtifactView = internal.InputArtifactView
	// AnswerEnvelope is the canonical terminal answer shape.
	AnswerEnvelope = internal.AnswerEnvelope
)

// Registry + compression vocabulary — aliases of the internal types.
type (
	// PlannerConfig is the resolved planner configuration.
	PlannerConfig = internal.PlannerConfig
	// Factory constructs a planner concrete from config + deps.
	Factory = internal.Factory
	// FactoryDeps carries the dependencies a Factory may consume.
	FactoryDeps = internal.FactoryDeps
	// WakeMode declares a planner's wake-on-resolution strategy.
	WakeMode = internal.WakeMode
	// WakeAware is the optional wake-mode declaration interface.
	WakeAware = internal.WakeAware
	// ReasoningReplayMode controls reasoning-trace replay in prompts.
	ReasoningReplayMode = internal.ReasoningReplayMode
	// CompressionRunner runs trajectory compression at step boundaries.
	CompressionRunner = internal.CompressionRunner
	// CompressionOption customises NewCompressionRunner.
	CompressionOption = internal.CompressionOption
	// TokenEstimator estimates a trajectory's token footprint.
	TokenEstimator = internal.TokenEstimator
	// Summariser produces the five-field trajectory Summary.
	Summariser = internal.Summariser
)

// FinishReason values.
const (
	// FinishGoal — the goal was achieved.
	FinishGoal = internal.FinishGoal
	// FinishNoPath — no step/decision path remains.
	FinishNoPath = internal.FinishNoPath
	// FinishCancelled — the run was cancelled.
	FinishCancelled = internal.FinishCancelled
	// FinishDeadlineExceeded — the run ran out of time/steps.
	FinishDeadlineExceeded = internal.FinishDeadlineExceeded
	// FinishConstraintsConflict — constraints are unsatisfiable.
	FinishConstraintsConflict = internal.FinishConstraintsConflict
)

// PauseReason values.
const (
	// PauseApprovalRequired — HITL approval is required.
	PauseApprovalRequired = internal.PauseApprovalRequired
	// PauseAwaitInput — the run awaits user input.
	PauseAwaitInput = internal.PauseAwaitInput
	// PauseExternalEvent — the run awaits an external event.
	PauseExternalEvent = internal.PauseExternalEvent
	// PauseConstraintsConflict — a conflict requires a human decision.
	PauseConstraintsConflict = internal.PauseConstraintsConflict
)

// JoinKind values.
const (
	// JoinAll — wait for every branch.
	JoinAll = internal.JoinAll
	// JoinFirstSuccess — first successful branch wins.
	JoinFirstSuccess = internal.JoinFirstSuccess
	// JoinKeyed — join on a keyed subset.
	JoinKeyed = internal.JoinKeyed
	// JoinN — join when N branches resolved.
	JoinN = internal.JoinN
)

// WakeMode values.
const (
	// WakePush — the runtime wakes the planner on resolution.
	WakePush = internal.WakePush
	// WakePoll — the planner polls at deterministic boundaries.
	WakePoll = internal.WakePoll
	// WakeHybrid — push with a poll fallback.
	WakeHybrid = internal.WakeHybrid
)

// Task-error codes the runtime maps non-goal finishes to.
const (
	// TaskErrorCodeRunLoopError — the run loop failed.
	TaskErrorCodeRunLoopError = internal.TaskErrorCodeRunLoopError
	// TaskErrorCodeCancelled — the run was cancelled.
	TaskErrorCodeCancelled = internal.TaskErrorCodeCancelled
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrPlannerClosed — the planner has been closed.
	ErrPlannerClosed = internal.ErrPlannerClosed
	// ErrInvalidDecision — the decision failed runtime validation.
	ErrInvalidDecision = internal.ErrInvalidDecision
	// ErrRepairExhausted — the schema-repair budget ran out.
	ErrRepairExhausted = internal.ErrRepairExhausted
	// ErrIdentityRequired — the run's identity quadruple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrInvalidConfig — the planner configuration is invalid.
	ErrInvalidConfig = internal.ErrInvalidConfig
	// ErrDeterministicStep — a deterministic step returned an error.
	ErrDeterministicStep = internal.ErrDeterministicStep
	// ErrParallelCapExceeded — CallParallel exceeded the absolute cap.
	ErrParallelCapExceeded = internal.ErrParallelCapExceeded
	// ErrParallelInvalidJoin — the CallParallel join spec is invalid.
	ErrParallelInvalidJoin = internal.ErrParallelInvalidJoin
	// ErrMemoryBlockUnserializable — a memory block is not JSON-serialisable.
	ErrMemoryBlockUnserializable = internal.ErrMemoryBlockUnserializable
	// ErrDriverUnknown — the named planner driver is not registered.
	ErrDriverUnknown = internal.ErrDriverUnknown
	// ErrNilTrajectory — compression refuses a nil trajectory.
	ErrNilTrajectory = internal.ErrNilTrajectory
	// ErrEmptySummary — the summariser violated its contract.
	ErrEmptySummary = internal.ErrEmptySummary
)

// Resolve resolves the configured planner concrete via the driver
// registry (D-103).
var Resolve = internal.Resolve

// Register registers a planner concrete by name (the seam external
// planner authors plug into).
var Register = internal.Register

// MustRegister registers a planner concrete, panicking on conflict
// (init()-time registration).
var MustRegister = internal.MustRegister

// RegisteredDrivers lists the seated planner driver names
// (blank-import sdk/planner/react or sdk/drivers/prod to seat the
// production set).
var RegisteredDrivers = internal.RegisteredDrivers

// ConfigFromOperator projects the operator config block into the
// resolved PlannerConfig.
var ConfigFromOperator = internal.ConfigFromOperator

// HintsFromConfig projects the operator hints block into
// PlanningHints (nil when the block is empty).
var HintsFromConfig = internal.HintsFromConfig

// IsValidFinishReason reports whether r is canonical.
var IsValidFinishReason = internal.IsValidFinishReason

// IsValidPauseReason reports whether r is canonical.
var IsValidPauseReason = internal.IsValidPauseReason

// TaskErrorCodeForFinish maps a non-goal FinishReason to its
// task-error code.
var TaskErrorCodeForFinish = internal.TaskErrorCodeForFinish

// ResolveWakeMode resolves a planner's effective WakeMode (WakePush
// unless the concrete declares otherwise via WakeAware).
var ResolveWakeMode = internal.ResolveWakeMode

// NewCompressionRunner constructs the trajectory-compression runner
// over a Summariser.
var NewCompressionRunner = internal.NewCompressionRunner

// WithTokenEstimator overrides the runner's token estimator.
var WithTokenEstimator = internal.WithTokenEstimator

// DefaultTokenEstimator is the chars/4 trajectory token estimate.
var DefaultTokenEstimator = internal.DefaultTokenEstimator

// NewProcessLocalRegistry constructs the process-local handle
// registry for non-serialisable tool handles.
var NewProcessLocalRegistry = internal.NewProcessLocalRegistry
