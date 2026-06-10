// Package runctx is the public SDK facade over Harbor's
// internal/runtime/runctx package — the RunContext-population
// projections a run-loop driver applies between "task spawned" and
// "planner.Next" (RFC §3.6, §6.2; D-204/D-195). Alias-based
// re-exports only: no behavior lives here.
package runctx

import (
	internal "github.com/hurtener/Harbor/internal/runtime/runctx"
)

// ExtractAssistantAnswer extracts the assistant answer string from a
// terminal Finish.
var ExtractAssistantAnswer = internal.ExtractAssistantAnswer

// ProjectMemoryBlocks projects a memory LLMContextPatch into the
// planner's MemoryBlocks view.
var ProjectMemoryBlocks = internal.ProjectMemoryBlocks

// ProjectSkillsContext projects ranked skills into the planner's
// skills-context entries.
var ProjectSkillsContext = internal.ProjectSkillsContext

// ProjectSkillsDirectory projects the skills Directory view into the
// planner's skills-context entries (the canonical producer, D-201).
var ProjectSkillsDirectory = internal.ProjectSkillsDirectory

// ResolveInputArtifacts resolves the run's input artifact refs into
// planner-visible InputArtifactViews.
var ResolveInputArtifacts = internal.ResolveInputArtifacts
