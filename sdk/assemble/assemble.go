// Package assemble is the public SDK facade over Harbor's
// internal/runtime/assemble package — the ONE composition fan-out
// that turns a validated config into a running headless stack
// (RFC §3.6, §6.4). Alias-based re-exports only: no
// behavior lives here. Blank-import sdk/drivers/prod alongside this
// package so every Open the assembly performs resolves the
// production driver set; see docs/recipes/embed-harbor-headless.md
// for the full embedding walkthrough.
package assemble

import (
	internal "github.com/hurtener/Harbor/internal/runtime/assemble"
)

// Stack is the composed runtime bundle Assemble returns (stores,
// bus, LLM, memory, skills, tasks, catalog, executor, planner,
// run loop) with reverse-order Close.
type Stack = internal.Stack

// Options carries Assemble's injection points (logger, LLM snapshot
// override, planner override, skip knobs).
type Options = internal.Options

// DefaultMCPIdentity is the fallback transport-event identity for
// attached MCP servers.
var DefaultMCPIdentity = internal.DefaultMCPIdentity

// Assemble composes the full dependency-ordered runtime from a
// validated config, with partial-failure cleanup (a failed Assemble
// returns the partial Stack so the caller can drain it via Close).
var Assemble = internal.Assemble

// RunOption configures a Stack.RunOnce invocation. The functional-option
// shape keeps RunOnce's signature stable as new per-run knobs land.
type RunOption = internal.RunOption

// WithRunID pins the run's RunID instead of synthesising a fresh ULID.
var WithRunID = internal.WithRunID

// WithInputArtifacts pre-resolves operator-uploaded artifact IDs into
// the run's first-turn multimodal inputs.
var WithInputArtifacts = internal.WithInputArtifacts

// WithOutputSchema opts a RunOnce call into run-level structured output:
// the terminal answer is validated against the supplied JSON Schema and
// delivered as the envelope's additive AnswerPayload key (raw validated
// JSON). A nil/empty schema is a loud config error. On a schema-invalid
// answer after the correction budget the runner returns
// planner.ErrOutputInvalid — never a silent fallback to unvalidated
// text. Composing with WithStream: assistant-content token deltas are
// suppressed; the validated answer arrives once, in the envelope.
var WithOutputSchema = internal.WithOutputSchema

// StreamEvent is one observation a WithStream sink receives while
// RunOnce drives a run — a token delta, a planner-step boundary, or a
// tool dispatch. Minimal by design: progress signals only, never raw
// tool arguments or results.
type StreamEvent = internal.StreamEvent

// StreamEventKind is the sealed enum discriminating a StreamEvent
// (token / tool_dispatched / step).
type StreamEventKind = internal.StreamEventKind

// Streaming-sink event kinds: StreamToken (incremental output delta),
// StreamToolDispatched (a tool was dispatched), StreamStep (a
// planner-step boundary).
const (
	StreamToken          = internal.StreamToken
	StreamToolDispatched = internal.StreamToolDispatched
	StreamStep           = internal.StreamStep
)

// WithStream registers a per-run streaming sink on a RunOnce
// invocation. RunOnce still blocks and returns the terminal answer
// envelope; the sink receives StreamEvents synchronously on the run
// goroutine, so every event arrives before RunOnce returns.
var WithStream = internal.WithStream

// ErrNotRunnable is returned by Stack.RunOnce when the stack was
// assembled without a planner/run loop (no LLM driver, or
// SkipSteering/SkipRunLoop). Compare via errors.Is.
var ErrNotRunnable = internal.ErrNotRunnable
