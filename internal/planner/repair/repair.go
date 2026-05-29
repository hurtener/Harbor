// Package repair ships Harbor's reusable salvage / schema-repair /
// graceful-failure / multi-action-salvage ladder for planner steps
// (RFC §6.2, Phase 44 — see docs/plans/phase-44-schema-repair.md).
//
// The ladder runs in this order — the order is load-bearing (D-050):
//
//  1. **Salvage.** Parse the LLM's response into one or more
//     [planner.CallTool] actions. Tolerant of fenced JSON
//     (` ```json ... ``` `), prose-wrapped JSON, multiple JSON objects
//     in one response, and bare JSON arrays. When every parsed action
//     validates against its tool's schema, the loop returns a
//     [planner.CallTool] (single) or [planner.CallParallel]
//     (multi-action salvage — Step 4).
//
//  2. **Schema repair.** When a parsed [planner.CallTool]'s args fail
//     the tool's [tools.ToolDescriptor.Validate], the loop builds a
//     focused corrective sub-prompt ("argument `X` failed: <validator
//     error>; please re-emit with the corrected field") and re-asks
//     the LLM. Bounded by [Config.RepairAttempts] (default 3).
//
//  3. **Graceful failure.** After [Config.MaxConsecutiveArgFailures]
//     consecutive arg-validation failures — independent of the
//     [Config.RepairAttempts] budget so identical-shape failures
//     terminate quickly — the loop returns
//     [planner.Finish]{Reason: [planner.FinishNoPath]} with
//     `Metadata["followup"] = true` AND emits
//     [planner.EventTypePlannerRepairExhausted] so observability picks
//     up the failure loudly (§13 fail-loudly principle; the emit is
//     the surface that makes graceful failure NOT silent).
//
//  4. **Multi-action salvage.** When the parser returns more than one
//     well-formed [planner.CallTool] and every one validates, the loop
//     emits [planner.CallParallel] with the actions in their original
//     LLM-emitted order, joined via [planner.JoinAll]. Concretes that
//     want sequential salvage instead opt out by setting
//     [Config.ArgFillEnabled] = false.
//
// **Composition note.** The repair loop is OUTSIDE the LLM call (it
// consumes the response). The Phase 36 retry-with-feedback wrapper is
// INSIDE the LLM call (it wraps a single attempt). Both layers exist;
// they handle different concerns:
//
//   - retry wrapper: a single Complete attempt's [llm.CompleteResponse]
//     was malformed at the [llm.Validator] callback (an LLM-CALL
//     concern; Phase 36 owns the bound + re-ask shape).
//   - repair loop:   the response's parsed [planner.CallTool] failed
//     the tool's schema (an OUTPUT-SHAPE concern; Phase 44 owns the
//     ladder + graceful failure).
//
// The two-parallel-implementations rule (CLAUDE.md §13) bans embedding
// a second copy of the retry-with-feedback logic inside the repair
// package; the loop calls [llm.LLMClient].Complete (which already has
// retry composed at the registry edge) and operates on the response.
//
// **Concurrent-reuse contract (D-025).** [RepairLoop] is a reusable
// artifact: one constructed loop is safe to share across N concurrent
// runs. The receiver is read-only after construction; per-call state
// lives on the stack and in the run's [planner.RunContext]. The
// package's d025_test.go pins N=128 invocations under `-race`.
//
// **Identity is mandatory (§6 rule 9; D-001).** The loop refuses to
// run when [planner.RunContext.Quadruple] is incomplete. The LLM
// client's [llm.ErrIdentityMissing] is also surfaced verbatim when
// the supplied client rejects a Complete call for missing identity.
package repair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// Default knob values (RFC §6.2 + brief 02 §6 spec). The loop falls
// back to these whenever [Config] carries a non-positive value — a
// defensive default so a zero-value Config behaves correctly.
const (
	// DefaultRepairAttempts is the LLM re-ask budget when
	// [Config.RepairAttempts] is unset. Brief 07 §10's predecessor
	// default; the storm guard ([Config.MaxConsecutiveArgFailures])
	// is the load-bearing terminator.
	DefaultRepairAttempts = 3
	// DefaultMaxConsecutiveArgFailures is the consecutive-failure
	// counter cap that trips graceful failure when set to its default.
	// Strictly less than [DefaultRepairAttempts] so the storm guard
	// fires BEFORE the attempts budget runs out — brief 07 §10's
	// failure-mode-blind mitigation.
	DefaultMaxConsecutiveArgFailures = 2

	// reasonTruncateBytes caps individual entries in the
	// [planner.RepairExhaustedPayload] reasons slice to keep audit
	// payloads bounded.
	reasonTruncateBytes = 256
)

// Config carries the three per-concrete repair-loop knobs the master
// plan / RFC §6.2 spec'd:
//
//   - [Config.ArgFillEnabled]: opt-in. When false the loop returns
//     the parser's first valid action even if its args don't validate,
//     letting the dispatcher reject. When true (default) the loop runs
//     the schema-repair sub-prompt path.
//   - [Config.RepairAttempts]: total LLM re-asks before graceful-
//     failure consideration. Default [DefaultRepairAttempts].
//   - [Config.MaxConsecutiveArgFailures]: storm-guard counter that is
//     INDEPENDENT of [Config.RepairAttempts]. When N consecutive arg-
//     validation failures land in a row, graceful failure fires
//     regardless of remaining attempts budget. Default
//     [DefaultMaxConsecutiveArgFailures].
type Config struct {
	ArgFillEnabled            bool
	RepairAttempts            int
	MaxConsecutiveArgFailures int
}

// ToolValidator looks up the [tools.ToolDescriptor].Validate function
// for a named tool and runs it against the candidate args. The
// [planner.ToolCatalogView] exposes [tools.Tool] (schemas only — never
// descriptors) per RFC §6.2; the descriptor-bound validator lives in
// the runtime catalog. The repair loop accepts a validator-lookup
// function so the planner package stays isolated from descriptors
// (Phase 42 import-graph contract).
//
// Callers (Phase 45 ReAct, Phase 48 Deterministic) wire this from the
// runtime engine they already have a handle on; tests pass a stub.
//
// Contract:
//
//   - Returning nil means "args validated; the action is dispatchable."
//   - Returning a non-nil error means "args failed validation"; the
//     loop counts this against [Config.MaxConsecutiveArgFailures] and
//     uses the error's [error.Error] string in the corrective sub-
//     prompt + the repair-exhausted event's Reasons slice.
//   - Returning [ErrToolUnknown] means "no such tool"; the loop treats
//     this as a non-recoverable validation failure (the model named a
//     tool that doesn't exist — re-asking won't change that without
//     prompt-level changes the loop doesn't own).
type ToolValidator func(toolName string, args json.RawMessage) error

// ErrToolUnknown is the signal a [ToolValidator] returns when the
// named tool is not in the catalog. The loop treats this as a non-
// recoverable validation failure (same as schema rejection).
var ErrToolUnknown = errors.New("repair: tool unknown to catalog")

// RepairLoop is the salvage → repair → graceful-failure → multi-action-
// salvage driver. Reusable artifact (D-025): one instance is safe to
// share across N concurrent runs; per-call state lives on the stack
// and in the [planner.RunContext].
//
// Construct via [New]; call [RepairLoop.Run] to execute one planner
// step's worth of repair work.
type RepairLoop struct {
	cfg    Config
	parser *ActionParser
}

// RunResult is the outcome of one [RepairLoop.Run] step. It bundles
// the resolved [planner.Decision] with the provider-side reasoning
// trace captured from the LLM response (Phase 83e — D-147). The caller
// (the ReAct planner) stamps `Reasoning` onto
// `trajectory.Step.ReasoningTrace`; the loop itself never replays it.
//
// `Reasoning` is the trace from the LLM response that PRODUCED the
// returned Decision — on the repair path (multiple LLM calls), it is
// the reasoning of the final, successful response. Empty when the
// provider surfaced no reasoning.
//
// `Repair` carries the Phase 83c across-step failure classification
// (D-145): the ReAct planner reads it to update the per-run
// [planner.RepairCounters] so the next turn's prompt builder can
// escalate repair guidance. The loop itself does NOT mutate the
// counters — it only classifies; the counters live on the per-run
// [planner.RunContext] and the planner owns the increment/reset call
// (D-145 + D-025: no mutable state on the shared loop artifact).
type RunResult struct {
	// Decision is the resolved planner decision for the step.
	Decision planner.Decision
	// Reasoning is the provider-side thinking trace from the LLM
	// response that produced Decision. Empty when none was surfaced.
	Reasoning string
	// Repair carries the across-step failure classification for the
	// step (Phase 83c — D-145).
	Repair RepairOutcome
}

// RepairOutcome is the Phase 83c (D-145) across-step failure
// classification of one [RepairLoop.Run] step. The ReAct planner maps
// it onto the per-run [planner.RepairCounters]:
//
//   - ArgsRepaired   — at least one args-validation failure occurred
//     during the step (the schema-repair path fired). Drives the
//     ArgsRepair counter.
//   - MultiAction    — the parser salvaged more than one well-formed
//     action from a single LLM response (multi-action / multi-JSON
//     emission). Drives the MultiAction counter.
//
// Both fields are false on a clean step (a single well-formed action
// whose args validated first try) — which signals the planner to
// RESET the corresponding counters. The finish-repair counter is
// classified by the ReAct planner itself (it owns `_finish` reserved-
// name semantics — the repair loop sees only [planner.CallTool]).
type RepairOutcome struct {
	// ArgsRepaired is true when the schema-repair path fired at least
	// once during the step.
	ArgsRepaired bool
	// MultiAction is true when the parser salvaged > 1 action from a
	// single response.
	MultiAction bool
}

// New constructs a [RepairLoop] from the supplied [Config]. A zero-
// value Config gets [DefaultRepairAttempts] / [DefaultMaxConsecutive
// ArgFailures]; [Config.ArgFillEnabled] is taken as-is (zero value =
// false, so the schema-repair path is opt-in by struct contract).
//
// The returned loop is goroutine-safe.
func New(cfg Config) *RepairLoop {
	if cfg.RepairAttempts <= 0 {
		cfg.RepairAttempts = DefaultRepairAttempts
	}
	if cfg.MaxConsecutiveArgFailures <= 0 {
		cfg.MaxConsecutiveArgFailures = DefaultMaxConsecutiveArgFailures
	}
	return &RepairLoop{
		cfg:    cfg,
		parser: NewParser(),
	}
}

// Config returns the loop's effective configuration (post-default
// application). Useful for tests + observability.
func (l *RepairLoop) Config() Config {
	return l.cfg
}

// Run executes one planner step of repair work. The flow:
//
//  1. Issue [llm.LLMClient.Complete] for `req`.
//  2. Parse the response via [ActionParser.Parse].
//  3. Validate each parsed [planner.CallTool] via `validateTool`.
//  4. On all-valid: return [planner.CallTool] (single) or
//     [planner.CallParallel] (multi-action salvage).
//  5. On any args failure: build a corrective sub-prompt, append to
//     `req.Messages`, loop. Bounded by [Config.RepairAttempts] AND
//     [Config.MaxConsecutiveArgFailures].
//  6. On exhaustion: emit [planner.EventTypePlannerRepairExhausted]
//     and return [planner.Finish]{Reason: NoPath,
//     Metadata["followup"]=true}.
//
// `validateTool` is the descriptor-bound validator lookup (see
// [ToolValidator] godoc). When nil, the loop SKIPS the schema-repair
// path entirely — equivalent to [Config.ArgFillEnabled] = false. This
// is a safety hatch for callers (e.g. tests) that want pure salvage.
//
// Identity is mandatory (§6 rule 9): missing
// [planner.RunContext.Quadruple] identity returns
// [llm.ErrIdentityMissing]. The supplied `client` is the canonical
// identity gate (it rejects ctxes without identity); this method's
// pre-check exists so the loop fails closed even with a stub client
// that doesn't enforce.
//
// Returns a [RunResult] bundling the resolved Decision with the
// provider-side reasoning trace captured from the LLM response that
// produced it (Phase 83e — D-147). The caller stamps the trace onto
// `trajectory.Step.ReasoningTrace`.
func (l *RepairLoop) Run(
	ctx context.Context,
	rc planner.RunContext,
	client llm.LLMClient,
	req llm.CompleteRequest,
	validateTool ToolValidator,
) (RunResult, error) {
	if client == nil {
		return RunResult{}, errors.New("repair: nil llm.LLMClient")
	}
	if err := assertIdentity(rc); err != nil {
		return RunResult{}, err
	}

	// Step counter. Each iteration burns one LLM call.
	var (
		attempts            = 0
		consecutiveArgFails = 0
		current             = req
		reasons             []string
		// lastReasoning holds the reasoning trace of the most recent
		// LLM response — the trace that accompanies the Decision the
		// loop ultimately returns.
		lastReasoning string
		// argsRepaired records whether ANY args-validation failure
		// fired during the step — the Phase 83c (D-145) across-step
		// classification the ReAct planner maps onto the ArgsRepair
		// counter. Distinct from `consecutiveArgFails`, which is the
		// storm-guard counter reset semantics never touch.
		argsRepaired bool
	)

	for attempts < l.cfg.RepairAttempts {
		if err := ctx.Err(); err != nil {
			return RunResult{}, err
		}

		// Phase 107 — per-step streaming. When rc.OnChunk is set, flip
		// Stream=true and wire the OnContent / OnReasoning callbacks to
		// forward deltas through rc.OnChunk. Per D-025, the callback
		// closures are per-run on the stack, not on the shared artifact.
		if rc.OnChunk != nil {
			current.Stream = true
			current.OnContent = func(delta string, done bool) {
				rc.OnChunk(delta, done, planner.ChunkContent)
			}
			current.OnReasoning = func(delta string, done bool) {
				rc.OnChunk(delta, done, planner.ChunkReasoning)
			}
		}

		// Step 0: issue the LLM call.
		resp, err := client.Complete(ctx, current)
		if err != nil {
			// LLM-call errors bubble verbatim — retry-with-feedback
			// (Phase 36) is composed INSIDE the client, not here.
			// We never silently swallow upstream errors.
			return RunResult{}, err
		}
		attempts++
		lastReasoning = resp.Reasoning

		// Step 1: Salvage — parse the response.
		actions, parseErr := l.parser.Parse(resp.Content)
		// Phase 83e (D-147): the action schema is `{tool, args}`. A
		// model that still emits `reasoning` / `thought` has those
		// fields silently stripped; emit one telemetry event per
		// dropped field so the drift is observable without failing the
		// step. Best-effort — runs only on a parseable response.
		if parseErr == nil && len(actions) > 0 {
			emitExtraFieldsDropped(rc, resp.Content)
		}
		if parseErr != nil || len(actions) == 0 {
			reason := "parser failed: " + parserErrorReason(parseErr)
			reasons = append(reasons, truncate(reason))
			consecutiveArgFails++
			// A parser failure is an LLM-output-format failure — count
			// it toward the Phase 83c args-repair classification so the
			// next turn's prompt builder escalates guidance (D-145).
			argsRepaired = true
			if l.tripped(consecutiveArgFails) {
				return RunResult{
					Decision:  l.gracefulFailure(ctx, rc, attempts, consecutiveArgFails, reasons),
					Reasoning: lastReasoning,
					Repair:    RepairOutcome{ArgsRepaired: argsRepaired},
				}, nil
			}
			// Build a corrective sub-prompt for the next attempt.
			current = appendCorrectiveTurn(req, resp, parserCorrection(parseErr))
			continue
		}

		// Steps 2 + 4: validate every parsed CallTool. When
		// ArgFillEnabled is false OR validateTool is nil, skip the
		// schema-repair path and surface the parser's first action(s)
		// verbatim — letting the dispatcher reject if args are wrong.
		if !l.cfg.ArgFillEnabled || validateTool == nil {
			return RunResult{
				Decision:  promote(actions),
				Reasoning: lastReasoning,
				Repair: RepairOutcome{
					ArgsRepaired: argsRepaired,
					MultiAction:  len(actions) > 1,
				},
			}, nil
		}

		var firstBadIdx = -1
		var firstBadErr error
		for i, a := range actions {
			if verr := validateTool(a.Tool, a.Args); verr != nil {
				firstBadIdx = i
				firstBadErr = verr
				break
			}
		}
		if firstBadIdx == -1 {
			// All actions validate. Step 4: multi-action salvage.
			return RunResult{
				Decision:  promote(actions),
				Reasoning: lastReasoning,
				Repair: RepairOutcome{
					ArgsRepaired: argsRepaired,
					MultiAction:  len(actions) > 1,
				},
			}, nil
		}

		// At least one action failed validation. Record + decide
		// whether to repair or graceful-fail.
		bad := actions[firstBadIdx]
		reason := fmt.Sprintf("tool=%s arg-validation: %s",
			safeName(bad.Tool), firstBadErr.Error())
		reasons = append(reasons, truncate(reason))
		consecutiveArgFails++
		// A schema-repair failure is the canonical Phase 83c args-
		// repair signal (D-145).
		argsRepaired = true

		if l.tripped(consecutiveArgFails) {
			return RunResult{
				Decision:  l.gracefulFailure(ctx, rc, attempts, consecutiveArgFails, reasons),
				Reasoning: lastReasoning,
				Repair:    RepairOutcome{ArgsRepaired: argsRepaired},
			}, nil
		}

		// Step 2: build the corrective sub-prompt and loop.
		current = appendCorrectiveTurn(req, resp,
			formatArgsCorrection(bad.Tool, firstBadErr))
	}

	// RepairAttempts exhausted without a clean parse + validate.
	// Same graceful-failure terminal as the storm-guard path; the
	// payload distinguishes via ConsecutiveArgFailures vs Attempts.
	return RunResult{
		Decision:  l.gracefulFailure(ctx, rc, attempts, consecutiveArgFails, reasons),
		Reasoning: lastReasoning,
		Repair:    RepairOutcome{ArgsRepaired: argsRepaired},
	}, nil
}

// tripped reports whether the storm-guard threshold has fired.
func (l *RepairLoop) tripped(consecutiveArgFails int) bool {
	return consecutiveArgFails >= l.cfg.MaxConsecutiveArgFailures
}

// gracefulFailure builds the terminal [planner.Finish] AND emits the
// [planner.EventTypePlannerRepairExhausted] event. Identity-stamped
// via the run's quadruple; payload truncated per `reasonTruncateBytes`.
//
// This is the load-bearing fail-loudly emit (§13): every graceful-
// failure path MUST run through this function so the observability
// trail is complete. A direct `return planner.Finish{...}` without
// the emit would be a silent-degradation bug.
func (l *RepairLoop) gracefulFailure(
	ctx context.Context,
	rc planner.RunContext,
	attempts, consecutiveArgFails int,
	reasons []string,
) planner.Finish {
	now := nowFromRC(rc)

	emitRepairExhausted(ctx, rc, attempts, consecutiveArgFails, reasons, now)

	// Build the terminal Finish. Metadata carries:
	//   - "followup": true — brief 02 §6 spec'd value; signals the
	//     runtime/UX should ask the user for a retry / clarification.
	//   - "repair_attempts": int — the attempts the loop burned.
	//   - "repair_consecutive_arg_failures": int — storm-guard count.
	//   - "repair_chain": string — semicolon-joined truncated reasons.
	//   - "repair_error": "planner: schema repair exhausted" — the
	//     [planner.ErrRepairExhausted] sentinel string for sinks that
	//     prefer error-shaped reads.
	chain := strings.Join(reasons, "; ")
	if len(chain) > 1024 {
		chain = chain[:1024] + "…"
	}
	metadata := map[string]any{
		"followup":                        true,
		"repair_attempts":                 attempts,
		"repair_consecutive_arg_failures": consecutiveArgFails,
		"repair_chain":                    chain,
		"repair_error":                    planner.ErrRepairExhausted.Error(),
	}
	return planner.Finish{
		Reason:   planner.FinishNoPath,
		Metadata: metadata,
	}
}

// emitRepairExhausted publishes the planner.repair_exhausted event.
// Best-effort; never blocks on the bus (subscribers handle their own
// drop policies per Phase 05).
func emitRepairExhausted(
	ctx context.Context,
	rc planner.RunContext,
	attempts, consecutiveArgFails int,
	reasons []string,
	now time.Time,
) {
	if rc.Emit == nil {
		// The loop fails loud at the API surface; an absent Emit
		// closure means the host did not wire observability. Tests
		// pass a recording closure; production runtime always wires
		// one. The audit-redactor + bus take it from there.
		return
	}
	// Copy the reasons slice — the caller may continue mutating its
	// own slice after the emit returns (defensive immutability for the
	// event payload).
	reasonsCopy := append([]string(nil), reasons...)

	rc.Emit(events.Event{
		Type:       planner.EventTypePlannerRepairExhausted,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: planner.RepairExhaustedPayload{
			Identity:               rc.Quadruple,
			Attempts:               attempts,
			ConsecutiveArgFailures: consecutiveArgFails,
			Reasons:                reasonsCopy,
			OccurredAt:             now,
		},
	})
	_ = ctx // ctx is reserved for future cancellation-aware emits.
}

// emitExtraFieldsDropped publishes one
// [planner.EventTypePlannerActionExtraFieldDropped] event per
// Phase 83e-narrowed extra field (`reasoning` / `thought` — D-147) the
// LLM response carried. The narrowed action schema is `{tool, args}`;
// extra fields are stripped, NOT errored — this is a soft telemetry
// signal, not a fail-loudly surface. Best-effort; never blocks on the
// bus. A nil Emit closure (tests without observability) is a no-op.
func emitExtraFieldsDropped(rc planner.RunContext, responseContent string) {
	if rc.Emit == nil {
		return
	}
	dropped := DroppedExtraFields(responseContent)
	if len(dropped) == 0 {
		return
	}
	now := nowFromRC(rc)
	for _, field := range dropped {
		rc.Emit(events.Event{
			Type:       planner.EventTypePlannerActionExtraFieldDropped,
			Identity:   rc.Quadruple,
			OccurredAt: now,
			Payload: planner.ActionExtraFieldDroppedPayload{
				Identity:   rc.Quadruple,
				Field:      field,
				OccurredAt: now,
			},
		})
	}
}

// nowFromRC reads the [planner.RunContext.Clock] when present, else
// falls back to wall-clock. Tests fix the clock to make event-payload
// timestamp assertions deterministic.
func nowFromRC(rc planner.RunContext) time.Time {
	if rc.Clock != nil {
		return rc.Clock()
	}
	return time.Now()
}

// promote converts a non-empty []CallTool into the planner.Decision
// the loop returns to the caller. A single action returns a
// [planner.CallTool]; multiple actions return a [planner.CallParallel]
// with [planner.JoinAll] (multi-action salvage; Step 4 of the ladder).
func promote(actions []planner.CallTool) planner.Decision {
	switch len(actions) {
	case 0:
		// Should be unreachable — the caller guards this. Return a
		// no-arg fallback rather than nil so the runtime executor
		// always sees a well-shaped Decision.
		return planner.Finish{
			Reason:   planner.FinishNoPath,
			Metadata: map[string]any{"followup": true, "repair_error": "no actions"},
		}
	case 1:
		return actions[0]
	default:
		branches := make([]planner.CallTool, len(actions))
		copy(branches, actions)
		return planner.CallParallel{
			Branches: branches,
			Join:     &planner.JoinSpec{Kind: planner.JoinAll},
		}
	}
}

// assertIdentity rejects calls whose [planner.RunContext.Quadruple]
// is missing any of the four scope components. Returns
// [llm.ErrIdentityMissing] for parity with the LLM-client edge — the
// repair loop fails closed with the same sentinel the rest of the
// runtime uses (§6 rule 9 + D-001).
func assertIdentity(rc planner.RunContext) error {
	q := rc.Quadruple
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" || q.RunID == "" {
		return fmt.Errorf("%w (repair loop refuses missing-identity Run)", llm.ErrIdentityMissing)
	}
	return nil
}

// safeName escapes whitespace + control chars in a tool name to keep
// the corrective sub-prompt + audit payload well-formed when a model
// emits a junk tool name.
func safeName(s string) string {
	if s == "" {
		return "<empty>"
	}
	// Cap length so a malicious model can't blow up the audit payload.
	const cap = 64
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	// Replace control chars + newlines with spaces.
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncate caps a string at reasonTruncateBytes appending an ellipsis.
func truncate(s string) string {
	if len(s) <= reasonTruncateBytes {
		return s
	}
	return s[:reasonTruncateBytes] + "…"
}

// parserErrorReason extracts a one-line reason from a parser error
// for the corrective sub-prompt + the event payload.
func parserErrorReason(err error) string {
	if err == nil {
		return "empty actions list"
	}
	return err.Error()
}
