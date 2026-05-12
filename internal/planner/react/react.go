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
//     [PromptBuilder]. The default builder asks the LLM for a JSON
//     envelope `{"tool":"<name>","args":{...},"reasoning":"..."}` per
//     step OR `{"tool":"_finish","args":{"answer":"..."}}` to signal
//     completion.
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
//     - `CallTool` with another tool name → return verbatim.
//     - `CallParallel` (multi-action salvage from Phase 44) → reduce
//     to the first branch. V1 minimum viable per the master plan's
//     Phase 45 detail block ("single tool call per step"); Phase 47
//     revisits when the parallel executor exists (D-051).
//
// **Wake-on-resolution (D-032).** [ReActPlanner] implements
// [planner.WakeAware] returning [planner.WakePush]. The conformance
// pack (Phase 49) asserts the round-trip:
//
//	planner.ResolveWakeMode(reactPlanner) == planner.WakePush
//
// Phase 45 ships the declaration; the SpawnTask emission path is
// deferred to a later concrete-planner upgrade (V1 minimum viable
// emits only CallTool / Finish — D-051).
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
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// FinishToolName is the reserved tool name the LLM emits to signal
// completion. The planner intercepts this BEFORE returning the
// Decision; `"_finish"` never reaches the runtime as a real tool
// call. The leading underscore is a documented convention; future
// runtime catalog registration MAY reject `_`-prefixed tool names.
// D-051.
const FinishToolName = "_finish"

// DefaultMaxSteps is the planner-side circuit-breaker default for the
// observed trajectory step count. Set small enough to surface bugs
// quickly; large enough to leave 3-step scenarios headroom. The
// runtime's hop / cost budget (Phase 47+) is the authoritative gate;
// the planner-side cap is defence in depth (§13 + D-051).
const DefaultMaxSteps = 12

// DefaultSystemPrompt is the prompt the planner sends as the leading
// system message when [WithSystemPrompt] is not set. The prompt
// instructs the LLM on the JSON action envelope, names the reserved
// `_finish` tool, and asks for one action per step. The actual tool
// catalog is rendered in-line by the prompt builder.
const DefaultSystemPrompt = `You are Harbor's ReAct planner. Each step, choose exactly ONE action and respond with a JSON object of the form:

  {"tool": "<tool name>", "args": {...}, "reasoning": "<why>"}

When you have enough information to satisfy the user's goal, emit:

  {"tool": "_finish", "args": {"answer": "<final answer>"}, "reasoning": "<why>"}

Constraints:
- Always respond with valid JSON (no prose around it).
- Pick exactly one tool per step. Do not emit JSON arrays of actions.
- Use only the tools listed below; never invent a tool name.
- Keep "reasoning" short — one or two sentences.`

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

// WithSystemPrompt overrides the [DefaultSystemPrompt]. An empty
// string falls back to [DefaultSystemPrompt].
func WithSystemPrompt(s string) Option {
	return func(p *ReActPlanner) {
		if s != "" {
			p.systemPrompt = s
		}
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
		client:       client,
		maxSteps:     DefaultMaxSteps,
		builder:      defaultBuilder{},
		systemPrompt: DefaultSystemPrompt,
		repairCfg: repair.Config{
			ArgFillEnabled:            true,
			RepairAttempts:            repair.DefaultRepairAttempts,
			MaxConsecutiveArgFailures: repair.DefaultMaxConsecutiveArgFailures,
		},
	}
	for _, opt := range opts {
		opt(p)
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
	req := p.builder.Build(rc, p.systemPrompt)

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
	dec, err := loop.Run(ctx, rc, p.client, req, nil)
	if err != nil {
		return nil, err
	}

	// Map the loop's Decision to the planner's final Decision shape.
	final := p.mapDecision(dec)

	p.stepsTaken.Add(1)
	return final, nil
}

// mapDecision converts the repair loop's Decision into the planner's
// final Decision. Three transforms:
//
//   - [planner.Finish] (graceful failure from the loop) → verbatim.
//   - [planner.CallTool] with Tool == [FinishToolName] → translate
//     to [planner.Finish]{Reason: [planner.FinishGoal], Payload: ...}.
//   - [planner.CallTool] with another name → verbatim.
//   - [planner.CallParallel] → reduce to the first [planner.CallTool]
//     (V1 single-tool-call-per-step; D-051).
//
// Future phases extend this mapping (e.g. SpawnTask emission lands
// in a later concrete-planner upgrade — V1 minimum viable is
// CallTool / Finish only).
func (p *ReActPlanner) mapDecision(dec planner.Decision) planner.Decision {
	switch d := dec.(type) {
	case planner.Finish:
		// Graceful-failure terminal from the loop — pass through.
		// (planner.repair_exhausted already emitted by the loop's
		// gracefulFailure; the planner does NOT re-emit.)
		return d
	case planner.CallTool:
		if d.Tool == FinishToolName {
			return p.translateFinishCall(d)
		}
		return d
	case planner.CallParallel:
		return p.reduceToSingleAction(d)
	default:
		// Unreachable in V1 — the repair loop only ever returns
		// CallTool / CallParallel / Finish. A future planner concrete
		// that swaps in a richer loop MUST extend the mapping; for V1
		// surface the Decision verbatim and let the runtime executor
		// reject via planner.ErrInvalidDecision (§13 fail-loudly).
		return d
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
		_ = json.Unmarshal(call.Args, &args)
	}
	metadata := map[string]any{
		"reasoning":  call.Reasoning,
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

// reduceToSingleAction collapses a [planner.CallParallel] from the
// Phase 44 repair loop's multi-action salvage path into the first
// [planner.CallTool] branch. The rest are dropped — V1 minimum viable
// per the Phase 45 master-plan detail block ("single tool call per
// step. No parallel"). Phase 47 ships the parallel executor and will
// revisit this override (the method is the single unwind point).
//
// If the parallel has zero branches (defensive — the loop's
// `promote()` guards against this), the planner returns
// `Finish{NoPath, Metadata["reduction_error"]="empty_parallel"}`
// instead of nil (planner contract: always return a well-shaped
// Decision; §13 fail-loudly).
func (p *ReActPlanner) reduceToSingleAction(par planner.CallParallel) planner.Decision {
	if len(par.Branches) == 0 {
		return planner.Finish{
			Reason: planner.FinishNoPath,
			Metadata: map[string]any{
				"reduction_error": "empty_parallel",
				"via":             "react.reduceToSingleAction",
			},
		}
	}
	first := par.Branches[0]
	// Annotate the reasoning so observers see the reduction happened
	// (the Phase 49 conformance pack reads this in the multi-action
	// scenario).
	if first.Reasoning == "" {
		first.Reasoning = fmt.Sprintf(
			"react: reduced %d-way CallParallel to first action (V1 single-tool-call-per-step; D-051)",
			len(par.Branches),
		)
	}
	// Special case: the LLM emitted multiple actions and the first
	// was the `_finish` marker. Translate that to a Finish Decision
	// the same way the single-action path does (the reduction must
	// not change the completion semantics).
	if first.Tool == FinishToolName {
		return p.translateFinishCall(first)
	}
	return first
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

// errFinishNotTranslated is a defensive sentinel used by tests that
// want to assert the Finish-translation path is reachable; never
// surfaced through the planner's public contract.
var errFinishNotTranslated = errors.New("react: finish tool name not translated to Finish Decision")
