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
// planner-visible InputArtifactViews, resolving each attachment's
// disposition (caller hint > agent policy > runtime default) via the
// planner-homed pure resolver (Phase 84b — D-189).
var ResolveInputArtifacts = internal.ResolveInputArtifacts

// InputArtifactOptions carries the disposition inputs (hints / policy
// / catalog / emit) to ResolveInputArtifacts. The zero value
// reproduces the pre-84b behaviour exactly.
type InputArtifactOptions = internal.InputArtifactOptions

// DispositionHints converts a task's string-typed per-attachment hint
// map into the typed map InputArtifactOptions.Hints expects.
var DispositionHints = internal.DispositionHints

// InputDispositionResolvedPayload is the
// `task.input_disposition.resolved` event payload.
type InputDispositionResolvedPayload = internal.InputDispositionResolvedPayload

// EventTypeInputDispositionResolved is published once per input
// artifact when the disposition is resolved (Phase 84b — D-189).
const EventTypeInputDispositionResolved = internal.EventTypeInputDispositionResolved
