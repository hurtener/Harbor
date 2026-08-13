// Package tasks is the public SDK facade over Harbor's
// internal/tasks package — the unified foreground/background
// TaskRegistry, the spawn vocabulary, and the task-group join
// surface (RFC §3.6, §6.3). Alias-based re-exports only: no
// behavior lives here. Event payload structs, the patch mechanism,
// and Protocol wire projections are deliberately private.
package tasks

import (
	internal "github.com/hurtener/Harbor/internal/tasks"
)

// Registry + task vocabulary — aliases of the internal types.
type (
	// TaskRegistry is the identity-mandatory task registry interface.
	TaskRegistry = internal.TaskRegistry
	// Dependencies carries the registry's construction dependencies.
	Dependencies = internal.Dependencies
	// SpawnRequest describes one task spawn.
	SpawnRequest = internal.SpawnRequest
	// SpawnToolRequest describes one background tool-call spawn.
	SpawnToolRequest = internal.SpawnToolRequest
	// Task is the canonical task record.
	Task = internal.Task
	// TaskHandle is the spawn's returned handle.
	TaskHandle = internal.TaskHandle
	// TaskID identifies one task.
	TaskID = internal.TaskID
	// TaskKind discriminates foreground/background tasks.
	TaskKind = internal.TaskKind
	// TaskStatus is the task lifecycle state.
	TaskStatus = internal.TaskStatus
	// TaskResult is the terminal result envelope.
	TaskResult = internal.TaskResult
	// TaskError is the typed terminal error.
	TaskError = internal.TaskError
	// TaskProgressSnapshot is the canonical durable progress record.
	TaskProgressSnapshot = internal.TaskProgressSnapshot
	// ReportProgressRequest is the ReportProgress input shape.
	ReportProgressRequest = internal.ReportProgressRequest
	// ProgressReportResult reports what one ReportProgress call did.
	ProgressReportResult = internal.ProgressReportResult
	// ProgressPolicy bounds the ReportProgress coalescing/rate policy.
	ProgressPolicy = internal.ProgressPolicy
	// ProgressReporter is permanently bound to one task; it has no target
	// argument and is the normal SDK progress surface.
	ProgressReporter = internal.ProgressReporter
	// ProgressReporterRegistry issues reporters bound to a task identity.
	ProgressReporterRegistry = internal.ProgressReporterRegistry
	// TaskFilter scopes a List call.
	TaskFilter = internal.TaskFilter
	// TaskSummary is the compact list projection.
	TaskSummary = internal.TaskSummary
	// TaskGroup is one spawn group.
	TaskGroup = internal.TaskGroup
	// TaskGroupID identifies one task group.
	TaskGroupID = internal.TaskGroupID
	// TaskGroupStatus is the group lifecycle state.
	TaskGroupStatus = internal.TaskGroupStatus
	// GroupRequest describes one group creation.
	GroupRequest = internal.GroupRequest
	// GroupCompletion is the resolved group's typed completion.
	GroupCompletion = internal.GroupCompletion
	// MemberOutcome is one member's outcome inside a GroupCompletion.
	MemberOutcome = internal.MemberOutcome
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// TaskKind values.
const (
	// KindForeground — a foreground task.
	KindForeground = internal.KindForeground
	// KindBackground — a background task.
	KindBackground = internal.KindBackground
)

// TaskStatus values.
const (
	// StatusPending — spawned, not yet started.
	StatusPending = internal.StatusPending
	// StatusRunning — executing.
	StatusRunning = internal.StatusRunning
	// StatusPaused — parked on the pause primitive.
	StatusPaused = internal.StatusPaused
	// StatusComplete — terminal success.
	StatusComplete = internal.StatusComplete
	// StatusFailed — terminal failure.
	StatusFailed = internal.StatusFailed
	// StatusCancelled — terminal cancellation.
	StatusCancelled = internal.StatusCancelled
)

// TaskGroupStatus values.
const (
	// GroupOpen — accepting members.
	GroupOpen = internal.GroupOpen
	// GroupSealed — membership frozen.
	GroupSealed = internal.GroupSealed
	// GroupCompleted — terminal success.
	GroupCompleted = internal.GroupCompleted
	// GroupCancelled — terminal cancellation.
	GroupCancelled = internal.GroupCancelled
)

// Cancellation propagation policies.
const (
	// PropagateCascade — cancelling the parent cancels spawned children.
	PropagateCascade = internal.PropagateCascade
	// PropagateIsolate — children survive the parent's cancellation.
	PropagateIsolate = internal.PropagateIsolate
)

// ReportProgress field bounds — the registry validates the raw caller
// input against these before any state is touched.
const (
	// ProgressFractionMin is the inclusive lower bound of Fraction.
	ProgressFractionMin = internal.ProgressFractionMin
	// ProgressFractionMax is the inclusive upper bound of Fraction.
	ProgressFractionMax = internal.ProgressFractionMax
	// ProgressPhaseMaxLen bounds one Phase label in bytes.
	ProgressPhaseMaxLen = internal.ProgressPhaseMaxLen
	// ProgressMessageMaxLen bounds one Message in bytes.
	ProgressMessageMaxLen = internal.ProgressMessageMaxLen
	// ProgressMaxTags bounds the number of Tags entries.
	ProgressMaxTags = internal.ProgressMaxTags
	// ProgressTagMaxLen bounds one Tag entry in bytes.
	ProgressTagMaxLen = internal.ProgressTagMaxLen
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrNotFound — no task under that ID.
	ErrNotFound = internal.ErrNotFound
	// ErrInvalidTransition — an illegal lifecycle transition.
	ErrInvalidTransition = internal.ErrInvalidTransition
	// ErrIdempotencyConflict — an idempotency key reused divergently.
	ErrIdempotencyConflict = internal.ErrIdempotencyConflict
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrUnknownDriver — the named task driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrRegistryClosed — the registry has been closed.
	ErrRegistryClosed = internal.ErrRegistryClosed
	// ErrInvalidRequest — the spawn request failed validation.
	ErrInvalidRequest = internal.ErrInvalidRequest
	// ErrGroupNotFound — no group under that ID.
	ErrGroupNotFound = internal.ErrGroupNotFound
	// ErrGroupSealed — a sealed group's membership cannot mutate.
	ErrGroupSealed = internal.ErrGroupSealed
	// ErrGroupNotSealed — resolve requires a sealed group.
	ErrGroupNotSealed = internal.ErrGroupNotSealed
)

// Open resolves the configured task driver and opens it.
var Open = internal.Open

// OpenDriver opens a task driver by explicit name.
var OpenDriver = internal.OpenDriver

// ValidateRequest validates a SpawnRequest.
var ValidateRequest = internal.ValidateRequest

// ValidateProgressRequest validates a ReportProgressRequest (fraction
// bounds + phase/message/tag bounds) before any state is touched.
var ValidateProgressRequest = internal.ValidateProgressRequest

// DefaultProgressPolicy returns the bounded default coalescing/rate
// policy every registry applies unless overridden at construction.
var DefaultProgressPolicy = internal.DefaultProgressPolicy

// RegisteredDrivers lists the seated task driver names (blank-import
// sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers

// WithRegistry returns a child context carrying the registry.
var WithRegistry = internal.WithRegistry

// From extracts the registry from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the registry from ctx, panicking when absent.
var MustFrom = internal.MustFrom
