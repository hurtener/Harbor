// Package runctx is the public SDK facade over Harbor's
// internal/runtime/runctx package — the RunContext-population
// projections a run-loop driver applies between "task spawned" and
// "planner.Next" (RFC §3.6, §6.2). Alias-based
// re-exports only: no behavior lives here.
package runctx

import (
	internal "github.com/hurtener/Harbor/internal/runtime/runctx"
)

// NewRunContext projects stack-derived subsystem handles into a
// fully-formed planner.RunContext, composing the same memory / skills /
// artifact / streaming projection helpers the run-loop drivers use. The
// shared factory a one-call runner or a headless RunSpec builder
// composes.
var NewRunContext = internal.NewRunContext

// Sources carries the stack-derived subsystem handles NewRunContext
// projects into a planner.RunContext. Every field is optional except an
// identity-complete quadruple.
type Sources = internal.Sources

// Option configures a NewRunContext call (functional-option shape).
type Option = internal.Option

// WithInputArtifacts pre-resolves operator-uploaded artifact IDs into
// the run's first-turn multimodal inputs.
var WithInputArtifacts = internal.WithInputArtifacts

// WithInputArtifactDispositions supplies the per-attachment disposition
// hint map (the top precedence layer).
var WithInputArtifactDispositions = internal.WithInputArtifactDispositions

// WithDispositionPolicy supplies the per-agent disposition policy map
// (the middle precedence layer).
var WithDispositionPolicy = internal.WithDispositionPolicy

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
// planner's skills-context entries (the canonical producer).
var ProjectSkillsDirectory = internal.ProjectSkillsDirectory

// ResolveInputArtifacts resolves the run's input artifact refs into
// planner-visible InputArtifactViews, resolving each attachment's
// disposition (caller hint > agent policy > runtime default) via the
// planner-homed pure resolver.
var ResolveInputArtifacts = internal.ResolveInputArtifacts

// InputArtifactOptions carries the disposition inputs (hints / policy
// / catalog / emit) to ResolveInputArtifacts. The zero value
// reproduces the prior default behaviour exactly.
type InputArtifactOptions = internal.InputArtifactOptions

// DispositionHints converts a task's string-typed per-attachment hint
// map into the typed map InputArtifactOptions.Hints expects.
var DispositionHints = internal.DispositionHints

// InputDispositionResolvedPayload is the
// `task.input_disposition.resolved` event payload.
type InputDispositionResolvedPayload = internal.InputDispositionResolvedPayload

// EventTypeInputDispositionResolved is published once per input
// artifact when the disposition is resolved.
const EventTypeInputDispositionResolved = internal.EventTypeInputDispositionResolved
