// Package react ships Harbor's reference LLM-driven planner concrete
// (Phase 45 — RFC §6.2 + RFC §3.2 — the first concrete sitting on the
// `internal/planner.Planner` seam).
//
// Phase 107c (D-167) cut the planner over to provider-native tool-
// calling: each per-step [llm.CompleteRequest] carries the visible
// catalog in `req.Tools`, and the response's typed
// [llm.CompleteResponse.ToolCalls] slice drives [planner.Decision]
// directly via [projectResponse]. The Phase 44 [repair.RepairLoop]
// remains in-tree for the `declarative_action` escape-hatch meta-tool
// (step 10 wires the dispatch); the main React path no longer parses
// JSON envelopes out of `resp.Content` and no longer runs the
// salvage / schema-repair / multi-action-salvage ladder for native
// responses (AC-15 / AC-19 / AC-20c).
//
// Each [ReActPlanner.Next] call:
//
//  1. Honours ctx.Err() and the run's identity quadruple
//     (§6 rule 9 + D-001 — identity is mandatory; the runtime fails
//     closed).
//  2. Checks the [MaxSteps] circuit breaker. When the run's prior
//     trajectory carries ≥ MaxSteps recorded steps, the planner emits
//     [planner.EventTypePlannerMaxStepsExceeded] AND returns
//     [planner.Finish]{Reason: [planner.FinishNoPath],
//     Metadata["max_steps_exceeded"]=true}. Fail-loudly per §13.
//  3. Observes [planner.RunContext.Control.Cancelled]; returns
//     [planner.Finish]{Reason: [planner.FinishCancelled]} on a CANCEL
//     observation (the planner's step-boundary contract per RFC §6.3).
//  4. Drains [planner.RunContext.PendingToolCalls] (AC-19a). When the
//     prior step's projection accumulated extra ToolCalls (the multi-
//     ToolCall serialization fallback per AC-19), the planner emits
//     them one at a time BEFORE consulting the LLM again.
//  5. Derives the per-run discovered-tools set (AC-18) by walking
//     prior `tool_search` results in [planner.RunContext.Trajectory],
//     stamps the union into [planner.RunContext.DiscoveredTools] for
//     observability, and uses it as the deferred-loading surface for
//     this turn's `req.Tools`.
//  6. Builds the [llm.CompleteRequest] via the configured
//     [PromptBuilder]. The default builder (Phase 83a, reshaped by
//     Phase 107c) assembles the nine XML-tagged structured sections
//     — `<identity>`, `<tool_discovery>`, `<tool_usage>`,
//     `<reasoning>`, `<tone>`, `<error_handling>`,
//     `<available_tools>` (name + description quick-reference;
//     schemas live in `req.Tools`), and the optional
//     `<additional_guidance>` / `<planning_constraints>` injection
//     surfaces. The prompt asks for a final answer as plain
//     `resp.Content` and tool steps as native `resp.ToolCalls`; the
//     Phase 83e `{tool, args}` JSON envelope is RETIRED on the main
//     path (declarative_action keeps it for backward compat).
//  7. Stamps `req.Tools` from `rc.Catalog.List()` (which already
//     filters by the run's identity scope + `LoadingAlways` per
//     [runtimeCatalogView]) plus per-run discovered tools resolved
//     through `rc.Catalog.Resolve` (AC-17). Sets
//     `req.ParallelToolCalls = true` so native parallel tool-call
//     emission is enabled per turn.
//  8. Wires the per-step streaming callbacks
//     ([planner.RunContext.OnChunk]) into `req.Stream` /
//     `req.OnContent` / `req.OnReasoning` (Phase 107). The wiring
//     formerly lived inside the repair loop; under the cutover the
//     planner owns it (the repair loop is no longer called on the
//     native path).
//  9. Issues exactly ONE [llm.LLMClient.Complete] call. On error,
//     surfaces it verbatim (§13 fail-loudly).
//  10. Routes the response through [projectResponse] (AC-15 / AC-19):
//      - `len(resp.ToolCalls) == 1` → [planner.CallTool] (or
//        reserved-name translation to [planner.Finish] /
//        [planner.SpawnTask] / [planner.AwaitTask]).
//      - `len(resp.ToolCalls) > 1` → first call becomes [planner.CallTool];
//        remainder accumulates on `rc.PendingToolCalls` for serialised
//        dispatch on subsequent steps (AC-19 serialization fallback).
//      - `len(resp.ToolCalls) == 0 && resp.Content != ""` →
//        [planner.Finish]{Reason: [planner.FinishGoal], Payload: resp.Content}
//        (terminal answer as plain content).
//      - `len(resp.ToolCalls) == 0 && resp.Content == ""` →
//        [planner.Finish]{Reason: [planner.FinishNoPath]}.
//  11. Threads the captured `resp.Reasoning` through
//      [planner.RunContext.OnReasoning] (Phase 83m item 8) so the
//      runloop's trajectory-append path stamps
//      `trajectory.Step.ReasoningTrace` (Phase 83e — D-148 replay).
//  12. Emits [planner.EventTypePlannerDecision] carrying the resolved
//      Decision shape + the captured reasoning trace.
//  13. Resets `rc.RepairCounters` for the step (a clean native step
//      clears any prior turn's repair guidance — the declarative_action
//      path in step 10 is the only producer that will increment them).
//
// **Pause/resume.** Native tool-calling does not change the
// [planner.RequestPause] contract; ReAct still doesn't emit
// RequestPause directly in V1.3 (the unified pause primitive's first
// React consumer lands with the HITL-approval wave). Pause requests
// arrive via [planner.RunContext.Control] and are honoured at step
// boundary by the planner concrete that emits them.
//
// **Wake-on-resolution (D-032).** [ReActPlanner] implements
// [planner.WakeAware] returning [planner.WakePush]. Phase 47 wires the
// emission path end-to-end (D-056): a non-retain-turn `_spawn_task`
// emission returns control to the runtime; the runtime registers the
// planner against [tasks.TaskRegistry.WatchGroup]; on the
// [tasks.GroupCompletion] delivery the runtime re-invokes `Next` with
// the resolved `MemberOutcome` slice surfaced through
// `RunContext.Trajectory.Background`. The conformance pack (Phase 49)
// asserts the round-trip:
//
//	planner.ResolveWakeMode(reactPlanner) == planner.WakePush
//
// **Concurrent-reuse (D-025).** [ReActPlanner] is a reusable artifact:
// one constructed instance is safe to share across N concurrent
// runs. The receiver is read-only after construction; per-call state
// lives on the stack and in the run's [planner.RunContext].
// `d025_test.go` pins N=128 invocations under `-race`.
//
// **Import-graph contract (§13).** The react package MUST NOT import
// `internal/runtime/...`. The Phase 42
// [internal/planner/conformance.TestImportGraph_PlannerDoesNotImportRuntime]
// covers the new package by construction (it walks the whole planner
// subtree). The Phase 45 smoke script asserts the same via grep.
package react

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
	"github.com/hurtener/Harbor/internal/tasks"
)

// FinishToolName is the reserved tool name the LLM emits to signal
// completion. The planner intercepts this BEFORE returning the
// Decision; `"_finish"` never reaches the runtime as a real tool
// call. The leading underscore is a documented convention; future
// runtime catalog registration MAY reject `_`-prefixed tool names.
// D-051.
const FinishToolName = "_finish"

// SpawnTaskToolName is the reserved tool name the LLM emits to spawn
// a background task. The planner intercepts this in mapDecision and
// translates it to a typed [planner.SpawnTask] Decision before
// returning — the runtime never sees `"_spawn_task"` as a real tool
// call. D-056 — Phase 47 adds the third reserved emission shape.
const SpawnTaskToolName = "_spawn_task"

// AwaitTaskToolName is the reserved tool name the LLM emits to block
// the foreground turn on a previously-spawned task. The planner
// intercepts this in mapDecision and translates it to a typed
// [planner.AwaitTask] Decision. D-056 — Phase 47.
const AwaitTaskToolName = "_await_task"

// DefaultMaxSteps is the planner-side circuit-breaker default for the
// observed trajectory step count. Set small enough to surface bugs
// quickly; large enough to leave 3-step scenarios headroom. The
// runtime's hop / cost budget (Phase 47+) is the authoritative gate;
// the planner-side cap is defence in depth (§13 + D-051).
const DefaultMaxSteps = 12

// DefaultSystemPrompt is the sentinel value the planner sends as the
// leading system-prompt argument when [WithSystemPrompt] is not set.
//
// Phase 83a (RFC §6.2, brief 13 §2.1) replaced the former flat-string
// prompt with the twelve XML-tagged structured sections assembled by
// `defaultBuilder.buildSystemContent`. The structured sections ARE the
// default prompt content; this constant is the routing sentinel the
// builder compares against to decide whether to emit the structured
// twelve-section layout (sentinel matched → structured) or to honour
// an operator's verbatim [WithSystemPrompt] override (any other value
// → verbatim). The constant value is intentionally a stable
// non-empty string, never empty: `New` seeds `systemPrompt` with it,
// `WithSystemPrompt("")` falls back to it, and `buildSystemContent`
// branches on identity-equality with it.
//
// The old single-string Phase 45/47 prompt constant is intentionally
// removed (not renamed to `legacyDefaultSystemPrompt`) — the golden
// fixture `testdata/golden_default_prompt.txt` is the normative spec
// for the rendered default prompt going forward, and a dangling
// legacy constant would be dead code (CLAUDE.md §13).
const DefaultSystemPrompt = "harbor.react.default-system-prompt"

// PromptBuilder constructs the [llm.CompleteRequest] from a
// [planner.RunContext]. Default implementation ships in-package as
// [defaultBuilder]; operators may inject their own via
// [WithPromptBuilder] per RFC §6.2 (the planner's small set of
// genuinely policy-shaped knobs).
//
// Implementations MUST be safe for concurrent use (the planner is a
// reusable artifact per D-025; the prompt builder is read on every
// Next call).
type PromptBuilder interface {
	// Build returns the LLM request to send for the current step.
	// The builder reads from rc; it MUST NOT mutate rc. The returned
	// request carries Model = "" (the LLM client / wrapper chain
	// resolves the configured model at registry edge); callers that
	// need to pin a model override can wrap a default builder.
	Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest
}

// Option configures a [ReActPlanner] at construction time. Options
// are applied in order; later options override earlier ones.
type Option func(*ReActPlanner)

// WithMaxSteps overrides the [DefaultMaxSteps] circuit-breaker cap.
// Values ≤ 0 fall back to [DefaultMaxSteps]. The breaker fires when
// `len(rc.Trajectory.Steps) >= MaxSteps`; the planner emits
// [planner.EventTypePlannerMaxStepsExceeded] AND returns
// `Finish{NoPath, Metadata["max_steps_exceeded"]=true}`.
func WithMaxSteps(n int) Option {
	return func(p *ReActPlanner) {
		if n > 0 {
			p.maxSteps = n
		}
	}
}

// WithRepairAttempts passes the [repair.Config.RepairAttempts] knob
// through to Phase 44's loop. Default [repair.DefaultRepairAttempts]
// (3).
func WithRepairAttempts(n int) Option {
	return func(p *ReActPlanner) {
		p.repairCfg.RepairAttempts = n
	}
}

// WithMaxConsecutiveArgFailures passes the
// [repair.Config.MaxConsecutiveArgFailures] storm-guard counter
// through to Phase 44's loop. Default
// [repair.DefaultMaxConsecutiveArgFailures] (2).
func WithMaxConsecutiveArgFailures(n int) Option {
	return func(p *ReActPlanner) {
		p.repairCfg.MaxConsecutiveArgFailures = n
	}
}

// WithArgFillEnabled toggles Phase 44's schema-repair path. When
// false, the loop surfaces the parser's first action verbatim and
// lets the dispatcher reject misshaped args. Default true.
func WithArgFillEnabled(b bool) Option {
	return func(p *ReActPlanner) {
		p.repairCfg.ArgFillEnabled = b
	}
}

// WithPromptBuilder injects a custom [PromptBuilder]. Default: the
// in-package builder. A nil builder is rejected (the option is a
// no-op).
func WithPromptBuilder(b PromptBuilder) Option {
	return func(p *ReActPlanner) {
		if b != nil {
			p.builder = b
		}
	}
}

// WithReasoningReplay sets the agent-configured reasoning-replay mode
// (Phase 83e — D-148). The runtime wires this from
// `config.PlannerConfig.ReasoningReplay`. The default — and the value
// for an empty / unset mode — is [planner.ReasoningReplayNever]: a
// prior step's captured reasoning is NEVER re-injected into the next
// prompt. [planner.ReasoningReplayText] opts the agent into prepending
// each prior step's captured `ReasoningTrace` as a text block above
// the action JSON. A per-run `RunContext.ReasoningReplay` override
// wins over this configured value at render time.
//
// An invalid mode is rejected (the option is a no-op) — config
// validation already rejects bad values pre-boot.
func WithReasoningReplay(mode planner.ReasoningReplayMode) Option {
	return func(p *ReActPlanner) {
		if planner.IsValidReasoningReplayMode(mode) {
			p.reasoningReplay = mode
		}
	}
}

// WithMaxToolExamplesPerTool caps how many curated examples each tool
// renders in the `<available_tools>` section of the system prompt
// (Phase 83b — D-144). The runtime wires this from
// `config.PlannerConfig.MaxToolExamplesPerTool`. A value ≤ 0 (the
// default) resolves to [defaultMaxToolExamples] (3) at render time.
// Examples are ranked `minimal` > `common` > `edge-case` > untagged;
// the renderer keeps the top N.
//
// The option applies only when the default prompt builder is in use;
// an operator-supplied [WithPromptBuilder] owns its own prompt
// assembly and ignores this value.
func WithMaxToolExamplesPerTool(n int) Option {
	return func(p *ReActPlanner) {
		p.maxToolExamples = n
	}
}

// WithSystemPrompt overrides the [DefaultSystemPrompt]. An empty
// string falls back to [DefaultSystemPrompt].
//
// A non-default, non-empty string is honoured verbatim by the default
// prompt builder: it REPLACES the twelve-section structured layout
// (the structured sections ARE the default prompt content). The
// optional injection sections (`<available_tools>`,
// `<additional_guidance>`, `<planning_constraints>`) still append, so
// tool rendering and [WithSystemPromptExtra] guidance survive a custom
// base prompt.
func WithSystemPrompt(s string) Option {
	return func(p *ReActPlanner) {
		if s != "" {
			p.systemPrompt = s
		}
	}
}

// WithSystemPromptExtra injects operator-supplied guidance into the
// `<additional_guidance>` section of the rendered system prompt
// (Phase 83a, RFC §6.2, brief 13 §2.1 section 11). The string is
// rendered verbatim; the operator is responsible for content hygiene.
// An empty (or whitespace-only) string is a no-op — the
// `<additional_guidance>` section is then omitted from the prompt
// entirely rather than emitted as an empty tag pair.
//
// The guidance applies only when the default prompt builder is in
// use; an operator-supplied [WithPromptBuilder] owns its own prompt
// assembly and ignores this option. `internal/config`'s
// `PlannerConfig.ExtraGuidance` key flows to this option at
// construction (see `internal/planner/react/init.go`).
func WithSystemPromptExtra(s string) Option {
	return func(p *ReActPlanner) {
		p.extraGuidance = s
	}
}

// ReActPlanner is Harbor's reference LLM-driven planner. Reusable
// artifact (D-025): the receiver is read-only after construction;
// per-call state lives on the stack and in the [planner.RunContext].
//
// All fields are set at construction by [New] (with [Option] applied);
// none are mutated by [Next].
type ReActPlanner struct {
	// client is the LLM client. Composed by the LLM registry's
	// [llm.Open] with retry + downgrade + corrections + safety +
	// governance per D-043; the planner consumes the composed
	// surface and adds NO parallel layers (§13).
	client llm.LLMClient

	// repairCfg is the Phase 44 repair loop configuration the
	// planner applies on every Next. The loop is constructed once
	// per Next call (cheap — the loop's only state is the cfg + the
	// parser, both immutable).
	repairCfg repair.Config

	// maxSteps is the planner-side circuit breaker. Set via
	// [WithMaxSteps]; defaults to [DefaultMaxSteps].
	maxSteps int

	// builder constructs the LLM request from the RunContext. Set
	// via [WithPromptBuilder]; defaults to [defaultBuilder].
	builder PromptBuilder

	// systemPrompt is the leading system message every prompt
	// build starts with. Set via [WithSystemPrompt]; defaults to
	// [DefaultSystemPrompt].
	systemPrompt string

	// extraGuidance is operator-supplied content for the rendered
	// prompt's <additional_guidance> section. Set via
	// [WithSystemPromptExtra]; empty by default. Applied to the
	// in-package [defaultBuilder] at construction (`New`); an operator-
	// supplied [WithPromptBuilder] owns its own assembly and ignores
	// this field. Read-only after construction (D-025).
	extraGuidance string

	// reasoningReplay is the agent-configured reasoning-replay mode
	// (Phase 83e — D-148). Set via [WithReasoningReplay] from
	// `config.PlannerConfig.ReasoningReplay`; defaults to
	// [planner.ReasoningReplayNever]. Applied to the default prompt
	// builder at construction; a per-run RunContext override wins at
	// render time.
	reasoningReplay planner.ReasoningReplayMode

	// maxToolExamples is the agent-configured per-tool curated-example
	// cap for the rendered <available_tools> section (Phase 83b —
	// D-144). Set via [WithMaxToolExamplesPerTool] from
	// `config.PlannerConfig.MaxToolExamplesPerTool`; a value ≤ 0
	// resolves to [defaultMaxToolExamples] (3) at render time. Applied
	// to the default prompt builder at construction; read-only
	// thereafter (D-025).
	maxToolExamples int

	// stepsTaken is a process-wide diagnostic counter. NOT used
	// for any per-call semantics (those are derived from the
	// RunContext + ctx); maintained as `atomic.Int64` so the
	// D-025 reuse contract isn't broken by a bare int field. The
	// field is read-only from the outside (no exported accessor at
	// V1 — observability flows through events.Event).
	stepsTaken atomic.Int64
}

// Compile-time assertions: ReActPlanner satisfies both
// [planner.Planner] and [planner.WakeAware].
var (
	_ planner.Planner   = (*ReActPlanner)(nil)
	_ planner.WakeAware = (*ReActPlanner)(nil)
)

// New constructs a [ReActPlanner] backed by the supplied
// [llm.LLMClient] with the given options applied. Nil client panics —
// composition error caught at boot.
func New(client llm.LLMClient, opts ...Option) *ReActPlanner {
	if client == nil {
		panic("react.New: nil llm.LLMClient")
	}
	p := &ReActPlanner{
		client:          client,
		maxSteps:        DefaultMaxSteps,
		builder:         defaultBuilder{},
		systemPrompt:    DefaultSystemPrompt,
		reasoningReplay: planner.ReasoningReplayNever,
		repairCfg: repair.Config{
			ArgFillEnabled:            true,
			RepairAttempts:            repair.DefaultRepairAttempts,
			MaxConsecutiveArgFailures: repair.DefaultMaxConsecutiveArgFailures,
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	// Finalise the in-package builder with operator-supplied
	// <additional_guidance> content and the agent-configured
	// reasoning-replay mode (Phase 83a + 83e — D-148). Skipped when an
	// operator injected their own builder via WithPromptBuilder — a
	// custom builder owns its own prompt assembly and replay handling
	// (the option order is "later overrides earlier", so a
	// WithPromptBuilder after a WithSystemPromptExtra is the operator's
	// deliberate choice). The builder is rebuilt as a fresh value so it
	// stays an immutable compiled artifact (D-025).
	if _, ok := p.builder.(defaultBuilder); ok {
		p.builder = defaultBuilder{
			extraGuidance:    p.extraGuidance,
			configuredReplay: p.reasoningReplay,
			maxToolExamples:  p.maxToolExamples,
		}
	}
	return p
}

// WakeMode declares the planner's wake-on-resolution strategy (D-032
// + Phase 45 spec). ReAct ships the `push` mode: a non-retain-turn
// SpawnTask emission (deferred to a later phase) would return control
// to the runtime; the runtime would register the planner against
// [tasks.TaskRegistry.WatchGroup]; on `GroupCompletion` the runtime
// would re-invoke `Next` with the resolved `MemberOutcome` surfaced
// through `RunContext.Trajectory.Background`.
func (p *ReActPlanner) WakeMode() planner.WakeMode {
	return planner.WakePush
}

// StepsTaken returns the process-wide count of [ReActPlanner.Next]
// invocations served. Used by tests; not part of the planner contract.
// Atomic load — safe across goroutines.
func (p *ReActPlanner) StepsTaken() int64 {
	return p.stepsTaken.Load()
}

// Next implements [planner.Planner]. The flow is documented in the
// package godoc.
//
// **Native tool-calling path (Phase 107c — D-167).** Next issues
// exactly ONE [llm.LLMClient.Complete] and routes the response through
// [projectResponse]. The Phase 44 [repair.RepairLoop] is not called on
// the main path; the declarative_action escape-hatch (step 10) is the
// only consumer that re-enters the loop. A response with no
// ToolCalls and no Content maps to [planner.Finish]{NoPath}; non-empty
// Content with no ToolCalls is a natural-language terminal answer
// ([planner.Finish]{Goal, Payload: resp.Content}).
func (p *ReActPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := assertIdentity(rc); err != nil {
		return nil, err
	}

	// Steering: a CANCEL observation at step boundary returns
	// Finish{Cancelled} before any LLM call (RFC §6.3 — steering is
	// drained between steps; planner observes Control signals).
	if rc.Control.Cancelled {
		return planner.Finish{
			Reason: planner.FinishCancelled,
			Metadata: map[string]any{
				"steering": "cancelled",
				"run_id":   rc.Quadruple.RunID,
			},
		}, nil
	}

	// Circuit breaker: the planner-side step cap. The breaker fires
	// BEFORE the LLM call so a runaway never burns an additional
	// completion. The emit is the fail-loudly surface per §13.
	if rc.Trajectory != nil && len(rc.Trajectory.Steps) >= p.maxSteps {
		return p.maxStepsExceeded(ctx, rc), nil
	}

	// AC-13 / AC-20c (Phase 107c step 10 — D-167). When the previous
	// step dispatched `declarative_action`, the meta-tool's structured
	// observation carries a `repair_outcome` field classifying the
	// dispatch's failure mode (args validation, multi-action,
	// finish-via-wrong-channel). Bump the per-run RepairCounters BEFORE
	// the prompt is built so this turn's `<repair_guidance>` section
	// escalates the matching tier. The bump fires at most once per
	// declarative_action observation (the trajectory step is
	// timestamp-stable; subsequent steps that don't follow a
	// declarative_action call are no-ops).
	//
	// The signal is observation-driven (not decision-driven) because
	// the meta-tool's outcome only materialises AFTER the runloop
	// dispatches it — the planner's prior step's `updateRepairCounters`
	// fired with an empty outcome (correct at that moment; no
	// information about the dispatch yet). Here we close the loop by
	// reading the dispatch result that the runloop captured.
	declarativeBumped := applyDeclarativeOutcome(rc)

	// AC-19a: drain any PendingToolCalls accumulated by the prior
	// step's projection (the multi-ToolCall serialization fallback per
	// AC-19) BEFORE consulting the LLM again. Each Next call dispatches
	// at most one CallTool from the pending queue. When the runtime
	// later wires cross-step persistence of `rc.PendingToolCalls`, this
	// drain becomes the primary mechanism for serialising native
	// parallel tool-call responses; today's per-Next-call rc-value
	// copy limits persistence to within a single planner step.
	if pending := drainPending(&rc); pending != nil {
		// Emit planner.decision and bump the step counter so the
		// drain path looks identical to a normal CallTool emission
		// from the observer's perspective.
		final := *pending
		p.emitDecision(rc, final, "")
		// Same suppression rule as the main path below: a drained
		// declarative_action call that follows an outcome-bump in the
		// prior step must preserve the bump for the next step to
		// compound. Drains of other tool names reset as usual.
		if !(declarativeBumped && isDeclarativeActionDispatch(final)) {
			updateRepairCounters(rc, final, repair.RepairOutcome{})
		}
		// AC-19a: surface the shrunken queue to the runloop so the
		// next step's value-copy sees one fewer entry. drainPending
		// removed the head; the tail is what survives.
		if rc.OnPendingToolCalls != nil {
			rc.OnPendingToolCalls(rc.PendingToolCalls)
		}
		p.stepsTaken.Add(1)
		return final, nil
	}

	// AC-18: derive the per-run discovered-tools set from the prior
	// `tool_search` results in the trajectory. The runtime pre-clears
	// `rc.DiscoveredTools` at run start; the planner re-derives the
	// union per step (cross-step persistence via rc-by-value is the
	// runtime's concern — re-derivation is the V1.3 mechanism that
	// keeps the planner stateless per D-025). The stamped slice is
	// informational on `rc.DiscoveredTools`; the same set drives this
	// turn's `req.Tools` construction.
	rc.DiscoveredTools = mergeDiscovered(rc.DiscoveredTools, deriveDiscoveredFromTrajectory(rc.Trajectory))

	// Build the LLM request via the configured prompt builder. The
	// builder reads from rc; it MUST NOT mutate rc.
	//
	// Phase 83d (D-146): the in-package [defaultBuilder] can fail
	// loudly when a `RunContext.MemoryBlocks` tier or a `SkillsContext`
	// entry is not JSON-serialisable. The [PromptBuilder] interface
	// signature is fixed, so the planner drives the default builder via
	// the error-returning [defaultBuilder.buildRequest] and surfaces
	// [planner.ErrMemoryBlockUnserializable] from `Next` — never a
	// silently dropped memory tier. An operator-supplied builder owns
	// its own assembly and uses the interface `Build` (custom builders
	// do not render the Phase 83d wrappers).
	var req llm.CompleteRequest
	if db, ok := p.builder.(defaultBuilder); ok {
		var buildErr error
		req, buildErr = db.buildRequest(rc, p.systemPrompt)
		if buildErr != nil {
			return nil, buildErr
		}
	} else {
		req = p.builder.Build(rc, p.systemPrompt)
	}

	// AC-17: populate the per-turn native tool-calling surface.
	// `rc.Catalog.List()` already returns the always-loaded subset
	// filtered by the run's identity + GrantedScopes (the
	// runtimeCatalogView's filter defaults to LoadingAlways when
	// `LoadingModes` is empty). Meta-tools registered with
	// LoadingAlways flow through this path naturally. Per-run
	// discovered tools (AC-18) are resolved by name and appended
	// without duplication.
	req.Tools = buildToolDeclarations(rc, rc.DiscoveredTools)
	// V1.3 default: enable provider-side parallel tool-calling. The
	// runtime executor's CallParallel dispatch is post-V1.3, so the
	// projector's serialization fallback (AC-19) handles N>1 ToolCalls
	// by emitting the first and queueing the rest on rc.PendingToolCalls.
	req.ParallelToolCalls = true

	// Phase 107 streaming wiring. Under the Phase 44 era the repair
	// loop owned the OnContent/OnReasoning fan-out; the Phase 107c
	// cutover moved the Complete call into the planner, so the
	// streaming hooks are wired here. A nil rc.OnChunk skips the
	// streaming path (the LLM driver returns the full response on
	// unary Complete). Per D-025, the closures capture rc (per-run
	// stack value), never planner state.
	if rc.OnChunk != nil {
		req.Stream = true
		req.OnContent = func(delta string, done bool) {
			rc.OnChunk(delta, done, planner.ChunkContent)
		}
		req.OnReasoning = func(delta string, done bool) {
			rc.OnChunk(delta, done, planner.ChunkReasoning)
		}
	}

	// Single LLM call — the Phase 44 salvage/repair ladder is BYPASSED
	// on the native path (AC-20c). The projector reads `resp.ToolCalls`
	// directly; `resp.Content` is treated as the model's preamble (when
	// ToolCalls are present) or its terminal answer (when ToolCalls are
	// empty). LLM-call errors propagate verbatim (§13 fail-loudly).
	resp, err := p.client.Complete(ctx, req)
	if err != nil {
		return nil, err
	}

	final, projErr := projectResponse(resp, &rc)
	if projErr != nil {
		return nil, projErr
	}

	// AC-19 + AC-19a: when the projector saw N>1 ToolCalls, it
	// appended the remainder to rc.PendingToolCalls. Surface the
	// updated queue to the runloop so the next step's value-copy
	// drains it (the projector's append on the local rc value would
	// otherwise be dead — see the OnPendingToolCalls godoc).
	if rc.OnPendingToolCalls != nil {
		rc.OnPendingToolCalls(rc.PendingToolCalls)
	}

	// Phase 83c (D-145) + Phase 107c step 10 (D-167): clear the per-run
	// RepairCounters on a clean native step. The native path bypasses
	// the schema-repair pipeline entirely, so an empty RepairOutcome
	// reflects what actually happened — no parser/args failures, no
	// multi-action salvage. The declarative_action escape hatch is the
	// only producer that sets ArgsRepaired / MultiAction / FinishRepair;
	// its signals arrive on the NEXT step (via applyDeclarativeOutcome
	// at top-of-Next reading the trajectory). When THIS step's decision
	// is a `CallTool{declarative_action}` dispatch AND a prior-step
	// bump already landed via applyDeclarativeOutcome, suppress the
	// reset: the bump must persist so the next step's outcome can
	// compound the escalation on repeated failures. Without this
	// suppression, every consecutive declarative_action call would
	// zero the counters here and the next-step bump would only ever
	// reach tier 1 (reminder).
	if !(declarativeBumped && isDeclarativeActionDispatch(final)) {
		updateRepairCounters(rc, final, repair.RepairOutcome{})
	}

	// Phase 83e (D-147): emit planner.decision carrying the captured
	// provider-side reasoning trace. The event is the observability
	// surface `harbor inspect-runs` replays to reconstruct a run's
	// reasoning channel; the audit redactor processes the payload on
	// the bus before any sink persists it (§7 — reasoning can be
	// sensitive).
	p.emitDecision(rc, final, resp.Reasoning)

	// Phase 83m item 8: hand the captured reasoning trace to the
	// runloop via the per-step `RunContext.OnReasoning` callback. The
	// runloop copies it onto `trajectory.Step.ReasoningTrace` when it
	// appends the step — without this, `ReasoningReplay=text` mode is
	// structurally ineffective in production because the trajectory
	// append leaves the field empty. A nil callback (tests without
	// observability) is a no-op; an empty reasoning string is still
	// delivered so the runloop's append is consistent.
	if rc.OnReasoning != nil {
		rc.OnReasoning(resp.Reasoning)
	}
	// Preserve the assistant's preamble prose across trajectory
	// replay so the model retains its narrative thread. See
	// RunContext.OnAssistantContent + trajectory.Step.AssistantPreamble.
	if rc.OnAssistantContent != nil {
		rc.OnAssistantContent(resp.Content)
	}

	p.stepsTaken.Add(1)
	return final, nil
}

// emitDecision publishes a [planner.EventTypePlannerDecision] event
// carrying the resolved Decision shape + the captured reasoning trace
// (Phase 83e — D-147). Best-effort; a nil Emit closure (tests without
// observability) is a no-op. The event is the load-bearing surface
// `harbor inspect-runs` replays to reconstruct a run's reasoning
// channel; the audit redactor processes the payload on the bus before
// any sink persists it (CLAUDE.md §7 — reasoning can be sensitive).
func (p *ReActPlanner) emitDecision(rc planner.RunContext, dec planner.Decision, reasoning string) {
	if rc.Emit == nil {
		return
	}
	kind, tool := decisionKindAndTool(dec)
	now := nowFromRC(rc)
	rc.Emit(events.Event{
		Type:       planner.EventTypePlannerDecision,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: planner.DecisionPayload{
			Identity:       rc.Quadruple,
			DecisionKind:   kind,
			Tool:           tool,
			ReasoningChars: len([]rune(reasoning)),
			ReasoningTrace: reasoning,
			OccurredAt:     now,
		},
	})
}

// decisionKindAndTool returns the Decision shape name and — for a
// CallTool — its tool name. A nil or unrecognised Decision yields
// ("unknown", "").
func decisionKindAndTool(dec planner.Decision) (kind, tool string) {
	switch d := dec.(type) {
	case planner.CallTool:
		return "CallTool", d.Tool
	case planner.CallParallel:
		return "CallParallel", ""
	case planner.Finish:
		return "Finish", ""
	case planner.SpawnTask:
		return "SpawnTask", ""
	case planner.AwaitTask:
		return "AwaitTask", ""
	case planner.RequestPause:
		return "RequestPause", ""
	default:
		return "unknown", ""
	}
}

// mapDecision converts the repair loop's Decision into the planner's
// final Decision. Five transforms (Phase 47, D-056):
//
//   - [planner.Finish] (graceful failure from the loop) → verbatim.
//   - [planner.CallTool] with Tool == [FinishToolName] → translate
//     to [planner.Finish]{Reason: [planner.FinishGoal], Payload: ...}.
//   - [planner.CallTool] with Tool == [SpawnTaskToolName] → translate
//     to [planner.SpawnTask]{Kind, Spec} (Phase 47, D-056). Malformed
//     args fail the call with a wrapped error — silent degradation
//     is forbidden per §13.
//   - [planner.CallTool] with Tool == [AwaitTaskToolName] → translate
//     to [planner.AwaitTask]{TaskID} (Phase 47, D-056).
//   - [planner.CallTool] with another name → verbatim.
//   - [planner.CallParallel] → verbatim. Phase 47 (D-056) ships the
//     runtime parallel executor; the V1 single-tool-call-per-step
//     collapse override is DELETED.
//
// Returns (Decision, error). The error path covers translation
// failures on the reserved names: malformed args, missing required
// fields. Fail-loudly per §13 — the planner refuses to dispatch a
// reserved-name shape with broken args (rather than silently emitting
// a literal CallTool that the dispatcher would reject downstream).
func (p *ReActPlanner) mapDecision(dec planner.Decision) (planner.Decision, error) {
	switch d := dec.(type) {
	case planner.Finish:
		// Graceful-failure terminal from the loop — pass through.
		// (planner.repair_exhausted already emitted by the loop's
		// gracefulFailure; the planner does NOT re-emit.)
		return d, nil
	case planner.CallTool:
		switch d.Tool {
		case FinishToolName:
			return p.translateFinishCall(d), nil
		case SpawnTaskToolName:
			return p.translateSpawnCall(d)
		case AwaitTaskToolName:
			return p.translateAwaitCall(d)
		default:
			return d, nil
		}
	case planner.CallParallel:
		// Phase 47 (D-056): pass CallParallel through unchanged.
		// The runtime parallel executor (internal/runtime/parallel)
		// validates branch count vs. absolute_max_parallel + atomic
		// setup before dispatching any branch. The planner's job is
		// emission — it does NOT reduce, validate caps, or call
		// branches itself (the planner subtree cannot import
		// internal/runtime/... per the §13 import-graph contract).
		//
		// Defensive: even though we pass CallParallel through, we
		// translate _finish/_spawn/_await reserved names embedded in
		// the FIRST branch so a multi-action emission whose first
		// shape is a completion marker still terminates cleanly. The
		// pattern is rare (the LLM emitted a multi-action array whose
		// first entry is _finish); the planner-side translation
		// matches the single-action semantics. Branches after the
		// first are left verbatim — the executor enforces the
		// atomic-setup-validation contract.
		if len(d.Branches) > 0 {
			first := d.Branches[0]
			switch first.Tool {
			case FinishToolName:
				return p.translateFinishCall(first), nil
			case SpawnTaskToolName:
				return p.translateSpawnCall(first)
			case AwaitTaskToolName:
				return p.translateAwaitCall(first)
			}
		}
		return d, nil
	default:
		// Unreachable in V1 — the repair loop only ever returns
		// CallTool / CallParallel / Finish. A future planner concrete
		// that swaps in a richer loop MUST extend the mapping; for V1
		// surface the Decision verbatim and let the runtime executor
		// reject via planner.ErrInvalidDecision (§13 fail-loudly).
		return d, nil
	}
}

// translateFinishCall converts a `_finish` reserved-name CallTool
// into a [planner.Finish]{Reason: [planner.FinishGoal]} Decision.
// The args' "answer" field becomes the Payload; the reasoning + the
// raw args end up in Metadata for observability.
func (p *ReActPlanner) translateFinishCall(call planner.CallTool) planner.Finish {
	type finishArgs struct {
		Answer any `json:"answer"`
	}
	var args finishArgs
	// Best-effort decode: the parser already validated this is JSON;
	// missing/non-string answer surfaces as a nil Payload (the runtime
	// executor's task.completed payload will carry the same nil).
	if len(call.Args) > 0 {
		_ = json.Unmarshal(call.Args, &args) //nolint:errcheck // best-effort decode; a missing answer surfaces as nil Payload (see doc above)
	}
	metadata := map[string]any{
		"raw_args":   string(call.Args),
		"via":        "react._finish",
		"tool":       FinishToolName,
		"goal_reach": true,
	}
	return planner.Finish{
		Reason:   planner.FinishGoal,
		Payload:  args.Answer,
		Metadata: metadata,
	}
}

// translateSpawnCall converts a `_spawn_task` reserved-name CallTool
// into a typed [planner.SpawnTask] Decision (Phase 47, D-056).
//
// Expected args envelope:
//
//	{"kind":"background"|"foreground", "spec":{<SpawnSpec fields>}, "group_id":"<optional>"}
//
// Missing fields fall back to documented defaults:
//   - `kind` defaults to `tasks.KindBackground` (the typical spawn-and-resume use).
//   - `spec.priority` defaults to 0.
//   - `spec.retain_turn` defaults to false (push wake-on-resolution per D-032).
//   - `spec.fail_fast` defaults to false.
//
// Malformed JSON in `args` returns a wrapped [planner.ErrInvalidDecision]
// — silent degradation is forbidden per §13. The Reasoning is preserved
// on the Decision's Spec.Description so audit + observability sinks see
// it.
func (p *ReActPlanner) translateSpawnCall(call planner.CallTool) (planner.SpawnTask, error) {
	type spawnArgsEnvelope struct {
		Kind    string `json:"kind"`
		GroupID string `json:"group_id"`
		Spec    struct {
			Description string `json:"description"`
			Query       string `json:"query"`
			Priority    int    `json:"priority"`
			RetainTurn  bool   `json:"retain_turn"`
			FailFast    bool   `json:"fail_fast"`
		} `json:"spec"`
	}
	var env spawnArgsEnvelope
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &env); err != nil {
			return planner.SpawnTask{}, fmt.Errorf(
				"%w: react._spawn_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(call.Args),
			)
		}
	}
	kind := tasks.TaskKind(env.Kind)
	switch kind {
	case "":
		// Default to background — the typical wake-on-resolution shape
		// per D-032 + Phase 47 spec (RunContext.Trajectory.Background
		// surfaces the resolved MemberOutcome on the planner's next
		// step).
		kind = tasks.KindBackground
	case tasks.KindForeground, tasks.KindBackground:
		// Valid.
	default:
		return planner.SpawnTask{}, fmt.Errorf(
			"%w: react._spawn_task kind %q not in {foreground, background}",
			planner.ErrInvalidDecision, env.Kind,
		)
	}
	return planner.SpawnTask{
		Kind: kind,
		Spec: planner.SpawnSpec{
			Description: env.Spec.Description,
			Query:       env.Spec.Query,
			Priority:    env.Spec.Priority,
			RetainTurn:  env.Spec.RetainTurn,
			FailFast:    env.Spec.FailFast,
		},
		GroupID: tasks.TaskGroupID(env.GroupID),
	}, nil
}

// translateAwaitCall converts a `_await_task` reserved-name CallTool
// into a typed [planner.AwaitTask] Decision (Phase 47, D-056).
//
// Expected args envelope: `{"task_id":"<id>"}`. Empty task_id fails
// loudly with [planner.ErrInvalidDecision] — the planner refuses to
// dispatch an empty-id Await (the runtime executor would reject
// downstream anyway; we surface the error at translation time).
func (p *ReActPlanner) translateAwaitCall(call planner.CallTool) (planner.AwaitTask, error) {
	type awaitArgs struct {
		TaskID string `json:"task_id"`
	}
	var args awaitArgs
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return planner.AwaitTask{}, fmt.Errorf(
				"%w: react._await_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(call.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.AwaitTask{}, fmt.Errorf(
			"%w: react._await_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(call.Args),
		)
	}
	return planner.AwaitTask{TaskID: tasks.TaskID(args.TaskID)}, nil
}

// maxStepsExceeded builds the terminal [planner.Finish] AND emits the
// [planner.EventTypePlannerMaxStepsExceeded] event. Same fail-loudly
// shape as Phase 44's `gracefulFailure` — every breaker path runs
// through this function so the observability trail is complete.
func (p *ReActPlanner) maxStepsExceeded(ctx context.Context, rc planner.RunContext) planner.Finish {
	now := nowFromRC(rc)
	stepsObserved := 0
	lastTool := ""
	if rc.Trajectory != nil {
		stepsObserved = len(rc.Trajectory.Steps)
		// Extract LastTool from the most recent CallTool action.
		// Steps[].Action is typed as `any` (trajectory package
		// avoids importing planner — see trajectory.Step godoc), so
		// we type-assert through the canonical Decision shapes.
		if n := len(rc.Trajectory.Steps); n > 0 {
			if call, ok := rc.Trajectory.Steps[n-1].Action.(planner.CallTool); ok {
				lastTool = call.Tool
			}
		}
	}

	// Emit FIRST so a panic in the Finish-construction path can't
	// silently drop the breaker observation. (The Finish construction
	// is pure value-shaping, but defence in depth matches the Phase 44
	// pattern.)
	emitMaxStepsExceeded(ctx, rc, p.maxSteps, stepsObserved, lastTool, now)

	return planner.Finish{
		Reason: planner.FinishNoPath,
		Metadata: map[string]any{
			"max_steps_exceeded": true,
			"max_steps":          p.maxSteps,
			"steps_observed":     stepsObserved,
			"last_tool":          lastTool,
			"run_id":             rc.Quadruple.RunID,
			"via":                "react.maxStepsExceeded",
		},
	}
}

// emitMaxStepsExceeded publishes the planner.max_steps_exceeded event.
// Best-effort; never blocks on the bus (subscribers handle their own
// drop policies per Phase 05).
func emitMaxStepsExceeded(
	ctx context.Context,
	rc planner.RunContext,
	maxSteps, stepsObserved int,
	lastTool string,
	now time.Time,
) {
	if rc.Emit == nil {
		// Absent Emit closure means the host did not wire
		// observability. Tests pass a recording closure; production
		// runtime always wires one. The audit-redactor + bus take it
		// from there.
		return
	}
	rc.Emit(events.Event{
		Type:       planner.EventTypePlannerMaxStepsExceeded,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: planner.MaxStepsExceededPayload{
			Identity:      rc.Quadruple,
			MaxSteps:      maxSteps,
			StepsObserved: stepsObserved,
			LastTool:      lastTool,
			OccurredAt:    now,
		},
	})
	_ = ctx // ctx reserved for future cancellation-aware emits.
}

// nowFromRC reads [planner.RunContext.Clock] when present, else falls
// back to wall-clock. Tests fix the clock to make event-payload
// timestamp assertions deterministic.
func nowFromRC(rc planner.RunContext) time.Time {
	if rc.Clock != nil {
		return rc.Clock()
	}
	return time.Now()
}

// assertIdentity rejects calls whose [planner.RunContext.Quadruple]
// is missing any of the four scope components. Returns wrapped
// [llm.ErrIdentityMissing] for parity with the LLM-client edge (and
// the Phase 44 repair loop) — the planner fails closed with the same
// sentinel the rest of the runtime uses (§6 rule 9 + D-001).
func assertIdentity(rc planner.RunContext) error {
	q := rc.Quadruple
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" || q.RunID == "" {
		return fmt.Errorf("%w (react planner refuses missing-identity Next)", llm.ErrIdentityMissing)
	}
	return nil
}
