// Package react ships Harbor's reference LLM-driven planner concrete
// (Phase 45 — RFC §6.2 + RFC §3.2 — the first concrete sitting on the
// `internal/planner.Planner` seam).
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
//  4. Builds the [llm.CompleteRequest] via the configured
//     [PromptBuilder]. The default builder (Phase 83a — brief 13
//     §2.1) assembles the twelve XML-tagged structured sections and
//     asks the LLM for a JSON envelope `{"tool":"<name>","args":{...}}`
//     per step OR `{"tool":"_finish","args":{"answer":"..."}}` to
//     signal completion. The envelope carries NO `reasoning` field —
//     reasoning is captured from the provider channel, never required
//     in the structured output (brief 13 §2.6).
//  5. Delegates the response → [planner.Decision] mapping to Phase
//     44's [repair.RepairLoop.Run] (salvage → schema repair →
//     graceful failure → multi-action salvage). The repair loop is
//     OUTSIDE the LLM call; the Phase 36 retry-with-feedback wrapper
//     is INSIDE — composition stays at the registry edge (D-043 +
//     D-050).
//  6. Maps the repair loop's [planner.Decision] to the planner's
//     final shape:
//     - `Finish{NoPath}` from the loop → propagate verbatim
//     ([planner.EventTypePlannerRepairExhausted] already emitted
//     by the loop's [repair.RepairLoop.gracefulFailure]).
//     - `CallTool` with `Tool == "_finish"` → translate to
//     `Finish{Reason: FinishGoal, Payload: <args.answer>}`. The
//     reserved name is a prompt-time convention, NOT a magic-
//     string opcode in the Decision sum (D-047 + D-051; the
//     predecessor's `next_node` anti-pattern is rejected).
//     - `CallTool` with `Tool == "_spawn_task"` → translate to
//     [planner.SpawnTask] with the args decoded into [planner.SpawnSpec]
//     (Phase 47, D-056). Wake mode is push: a non-retain-turn
//     SpawnTask returns control to the runtime; on
//     [tasks.GroupCompletion] the runtime re-enters Next with the
//     resolved MemberOutcome surfaced through `RunContext.Trajectory.Background`.
//     - `CallTool` with `Tool == "_await_task"` → translate to
//     [planner.AwaitTask] keyed on the args' `task_id` (Phase 47, D-056).
//     - `CallTool` with another tool name → return verbatim.
//     - `CallParallel` (multi-action salvage from Phase 44) → pass
//     through verbatim. Phase 47 (D-056) ships the runtime parallel
//     executor that consumes this shape; the V1 single-tool-call-
//     per-step collapse override (the Phase 45 D-051 stop-gap) is
//     DELETED — Harbor's V1 ceiling lifts here.
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

	// Phase 44's loop owns the salvage / repair / graceful-failure /
	// multi-action-salvage ladder. The loop calls
	// llm.LLMClient.Complete internally; the planner does NOT call
	// Complete directly (composition stays clean — §13 + D-050).
	//
	// validateTool is nil for Phase 45 V1: the planner does NOT have a
	// descriptor-bound validator surface at this point (the runtime
	// engine wires the Phase 42 ToolCatalogView in later phases). The
	// loop short-circuits the schema-repair path with a nil validator
	// (Phase 44 contract) and surfaces the parser's first action(s)
	// verbatim; the dispatcher rejects misshaped args downstream.
	loop := repair.New(p.repairCfg)
	result, err := loop.Run(ctx, rc, p.client, req, nil)
	if err != nil {
		return nil, err
	}

	// Map the loop's Decision to the planner's final Decision shape.
	// The mapper may surface a typed error when translating reserved
	// names (`_spawn_task`, `_await_task`) whose args are malformed —
	// silent degradation is forbidden per §13.
	final, mapErr := p.mapDecision(result.Decision)
	if mapErr != nil {
		return nil, mapErr
	}

	// Phase 83c (D-145): update the per-run RepairCounters from this
	// step's outcome so the NEXT turn's prompt builder can escalate
	// repair guidance. The counters live on rc (the per-run scope),
	// never on the planner struct — D-145 + D-025. A nil
	// rc.RepairCounters means the runtime opted out of augmentation;
	// the update is then a no-op.
	updateRepairCounters(rc, final, result.Repair)

	// Phase 83e (D-147): emit planner.decision carrying the captured
	// provider-side reasoning trace. The runtime's trajectory-append
	// path stamps `trajectory.Step.ReasoningTrace` from the same
	// `RunResult.Reasoning`; the event is the observability surface
	// `harbor inspect-runs` replays to reconstruct the reasoning
	// channel. Reasoning is captured content — it is NOT replayed into
	// the next prompt unless the agent's ReasoningReplay mode is `text`
	// (D-148; the trajectory renderer enforces this).
	p.emitDecision(rc, final, result.Reasoning)

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
