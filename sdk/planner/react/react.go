// Package react is the public SDK facade over Harbor's
// internal/planner/react package — the reference LLM-driven ReAct
// planner concrete (RFC §3.6, §6.2). Importing this package
// (including blank-importing it) seats the "react" driver
// registration, so `planner.driver: react` resolves through the
// registry; sdk/drivers/prod seats it too. Alias-based re-exports
// only: no behavior lives here. Prompt construction, projector, and
// repair-guidance internals are deliberately private.
package react

import (
	internal "github.com/hurtener/Harbor/internal/planner/react"
)

// Planner vocabulary — aliases of the internal types.
type (
	// ReActPlanner is the reference ReAct planner concrete.
	ReActPlanner = internal.ReActPlanner
	// Option customises New.
	Option = internal.Option
	// PromptBuilder is the swappable prompt-construction seam.
	PromptBuilder = internal.PromptBuilder
)

// DriverName is the registry name the package's init() registers.
const DriverName = internal.DriverName

// DefaultMaxSteps is the default planner-step cap.
const DefaultMaxSteps = internal.DefaultMaxSteps

// DefaultSystemPrompt is the sentinel selecting the built-in system
// prompt.
const DefaultSystemPrompt = internal.DefaultSystemPrompt

// Planner-reserved tool names the ReAct action grammar uses.
const (
	// FinishToolName — the terminal action.
	FinishToolName = internal.FinishToolName
	// SpawnTaskToolName — the background-spawn action.
	SpawnTaskToolName = internal.SpawnTaskToolName
	// AwaitTaskToolName — the background-join action.
	AwaitTaskToolName = internal.AwaitTaskToolName
	// DeclarativeActionToolName — the declarative-action envelope tool.
	DeclarativeActionToolName = internal.DeclarativeActionToolName
)

// New constructs a ReActPlanner over an LLM client.
var New = internal.New

// Constructor options (see internal/planner/react Option docs).
var (
	// WithArgFillEnabled toggles schema-guided argument filling.
	WithArgFillEnabled = internal.WithArgFillEnabled
	// WithMaxConsecutiveArgFailures caps consecutive arg failures.
	WithMaxConsecutiveArgFailures = internal.WithMaxConsecutiveArgFailures
	// WithMaxSteps caps the planner-step count.
	WithMaxSteps = internal.WithMaxSteps
	// WithMaxToolExamplesPerTool caps rendered examples per tool.
	WithMaxToolExamplesPerTool = internal.WithMaxToolExamplesPerTool
	// WithParallelToolCalls toggles CallParallel emission.
	WithParallelToolCalls = internal.WithParallelToolCalls
	// WithPromptBuilder swaps the prompt-construction seam.
	WithPromptBuilder = internal.WithPromptBuilder
	// WithReasoningReplay sets the reasoning-replay mode.
	WithReasoningReplay = internal.WithReasoningReplay
	// WithRepairAttempts sets the schema-repair budget.
	WithRepairAttempts = internal.WithRepairAttempts
	// WithSystemPrompt replaces the system prompt.
	WithSystemPrompt = internal.WithSystemPrompt
	// WithSystemPromptExtra appends to the system prompt.
	WithSystemPromptExtra = internal.WithSystemPromptExtra
)
