package react

import (
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// Dynamic-augmentation pass — Phase 83c (D-145).
//
// Phase 44 catches and repairs malformed JSON *inside* one planner
// step. It does NOT tell the LLM "you have been doing this wrong" —
// without an across-step signal, the same misformatted-action bug
// repeats every step until the MaxSteps circuit breaker trips (brief
// 13 §2.2). Phase 83c closes that across-step feedback loop:
//
//   - The runtime carries a per-run [planner.RepairCounters] on the
//     [planner.RunContext] and increments the matching counter when
//     Phase 44's repair pipeline had to fix an output (the increment
//     sites live in internal/planner/repair). A clean turn resets the
//     relevant counter.
//   - The ReAct prompt builder reads the counters every turn and, for
//     each non-zero counter, merges an escalating guidance block into
//     the `<additional_guidance>` section for THAT TURN ONLY. Tiered:
//     count 1 → `reminder`, count 2 → `warning`, count >= 3 →
//     `critical`.
//   - Each rendered block emits a
//     [planner.EventTypePlannerRepairGuidanceInjected] event so the
//     Console / operator can see when the LLM is struggling.
//
// Repair guidance does NOT replicate Phase 44's per-step repair: it
// counts failures Phase 44 had to repair and escalates if the trend
// does not reverse. Parallel-branch failures (a `parallel` plan whose
// branches fail at tool execution) are tool-execution failures, not
// LLM-output-format failures — they do NOT increment these counters.
//
// **Concurrent-reuse (D-145 + D-025).** The counters live on the
// per-run [planner.RunContext], never on the `ReActPlanner` artifact.
// The render helpers below are pure functions of their `rc` argument;
// they read counters, never write them, and never touch package
// state. Two concurrent Build calls with disjoint RunContext values
// cannot cross-contaminate.

// RepairTier names an escalation level for repair guidance. The three
// tiers map to counter values: 1 → reminder, 2 → warning, >= 3 →
// critical. A zero counter has no tier (the empty string).
type RepairTier string

// Repair-guidance escalation tiers (Phase 83c — D-145).
const (
	// RepairTierNone is the absence of a tier — the counter is 0, so
	// no guidance block is rendered.
	RepairTierNone RepairTier = ""
	// RepairTierReminder is the first escalation level (counter == 1):
	// a gentle nudge.
	RepairTierReminder RepairTier = "reminder"
	// RepairTierWarning is the second escalation level (counter == 2):
	// a firmer correction.
	RepairTierWarning RepairTier = "warning"
	// RepairTierCritical is the top escalation level (counter >= 3):
	// the strongest correction copy.
	RepairTierCritical RepairTier = "critical"
)

// repairTierFor maps a raw counter value to its escalation tier. The
// mapping is the load-bearing tier policy (brief 13 §2.2):
//
//	count <= 0 → none      count == 1 → reminder
//	count == 2 → warning   count >= 3 → critical
func repairTierFor(count int) RepairTier {
	switch {
	case count <= 0:
		return RepairTierNone
	case count == 1:
		return RepairTierReminder
	case count == 2:
		return RepairTierWarning
	default:
		return RepairTierCritical
	}
}

// Counter-name constants — the operator-visible `counter` attribute on
// the planner.repair_guidance_injected event. They are also the keys
// the smoke script and tests grep for.
const (
	counterFinish      = "finish"
	counterArgs        = "args"
	counterMultiAction = "multi_action"
)

// Repair-guidance hint copy — Phase 83c (D-145). The copy lives in
// exported constants so operators can grep it and so a copy change
// shows up as a reviewable diff (the nine golden fixtures under
// testdata/repair_guidance/ pin the rendered bodies). Each tier opens
// with its own tier name so a copy-paste typo that reuses the wrong
// tier's text is caught by the smoke script.
//
// Copy-design note (brief 13 §2.2 risk): an over-aggressive `critical`
// hint can confuse the model. The copy escalates in firmness, not in
// volume — `critical` is direct and specific, not shouty.
const (
	// ReminderFinishGuidance — finish-repair counter == 1.
	ReminderFinishGuidance = "reminder: your previous `_finish` action failed validation. " +
		"When you finish, emit exactly `{\"tool\": \"_finish\", \"args\": {\"answer\": \"...\"}}` " +
		"with `answer` as the only field — plain text, no metadata."
	// WarningFinishGuidance — finish-repair counter == 2.
	WarningFinishGuidance = "warning: your `_finish` action has failed validation twice. " +
		"Re-read the <finishing> section. The `args` object must contain ONLY the `answer` key, " +
		"and `answer` must be a plain-text string. Do not add status, confidence, or route fields."
	// CriticalFinishGuidance — finish-repair counter >= 3.
	CriticalFinishGuidance = "critical: your `_finish` action has failed validation three or more times. " +
		"Stop and emit this exact shape, substituting only the answer text: " +
		"`{\"tool\": \"_finish\", \"args\": {\"answer\": \"<your full plain-text answer here>\"}}`. " +
		"No other keys. No code fence commentary."

	// ReminderArgsGuidance — args-repair counter == 1.
	ReminderArgsGuidance = "reminder: your previous tool call had arguments that failed the tool's schema. " +
		"Match every argument name and type to the tool's `args_schema` exactly before calling it again."
	// WarningArgsGuidance — args-repair counter == 2.
	WarningArgsGuidance = "warning: your tool arguments have failed schema validation twice. " +
		"Re-read the chosen tool's `args_schema` in <available_tools>. Include every required field, " +
		"use the exact field names, and match the declared types — strings quoted, numbers unquoted."
	// CriticalArgsGuidance — args-repair counter >= 3.
	CriticalArgsGuidance = "critical: your tool arguments have failed schema validation three or more times. " +
		"Pick the simplest tool that can make progress, copy its `args_schema` field-for-field, " +
		"and supply only the fields that schema declares. If no tool fits, `_finish` with an explanation."

	// ReminderMultiActionGuidance — multi-action counter == 1.
	ReminderMultiActionGuidance = "reminder: your previous response contained more than one JSON action block. " +
		"Emit exactly ONE JSON object per turn, inside a single ```json code fence."
	// WarningMultiActionGuidance — multi-action counter == 2.
	WarningMultiActionGuidance = "warning: you have emitted multiple JSON action blocks twice. " +
		"One turn = one action. To run tools concurrently, use a single `parallel` action — " +
		"never several separate JSON objects."
	// CriticalMultiActionGuidance — multi-action counter >= 3.
	CriticalMultiActionGuidance = "critical: you have emitted multiple JSON action blocks three or more times. " +
		"Respond with ONE and only one JSON object. If you need concurrent tool calls, " +
		"wrap them in a single `{\"tool\": \"parallel\", \"args\": {\"steps\": [...]}}` action."
)

// finishGuidance returns the finish-repair hint copy for the tier, or
// the empty string for RepairTierNone.
func finishGuidance(t RepairTier) string {
	switch t {
	case RepairTierReminder:
		return ReminderFinishGuidance
	case RepairTierWarning:
		return WarningFinishGuidance
	case RepairTierCritical:
		return CriticalFinishGuidance
	default:
		return ""
	}
}

// argsGuidance returns the args-repair hint copy for the tier.
func argsGuidance(t RepairTier) string {
	switch t {
	case RepairTierReminder:
		return ReminderArgsGuidance
	case RepairTierWarning:
		return WarningArgsGuidance
	case RepairTierCritical:
		return CriticalArgsGuidance
	default:
		return ""
	}
}

// multiActionGuidance returns the multi-action hint copy for the tier.
func multiActionGuidance(t RepairTier) string {
	switch t {
	case RepairTierReminder:
		return ReminderMultiActionGuidance
	case RepairTierWarning:
		return WarningMultiActionGuidance
	case RepairTierCritical:
		return CriticalMultiActionGuidance
	default:
		return ""
	}
}

// repairGuidanceBlock is one resolved guidance line: the counter it
// came from, the tier it escalated to, the counter value, and the
// rendered copy. The render helper builds one per non-zero counter.
type repairGuidanceBlock struct {
	counter string
	tier    RepairTier
	count   int
	copy    string
}

// resolveRepairGuidance maps a [planner.RepairCounters] to the ordered
// list of guidance blocks the current turn should render. A nil
// counters pointer or all-zero counters yields a nil slice (no
// augmentation). The ordering is fixed — finish, args, multi_action —
// so the rendered prompt is deterministic across calls (KV-cache
// stability when only one counter changes between turns).
func resolveRepairGuidance(c *planner.RepairCounters) []repairGuidanceBlock {
	if c == nil {
		return nil
	}
	var blocks []repairGuidanceBlock
	if t := repairTierFor(c.FinishRepair); t != RepairTierNone {
		blocks = append(blocks, repairGuidanceBlock{
			counter: counterFinish, tier: t, count: c.FinishRepair, copy: finishGuidance(t),
		})
	}
	if t := repairTierFor(c.ArgsRepair); t != RepairTierNone {
		blocks = append(blocks, repairGuidanceBlock{
			counter: counterArgs, tier: t, count: c.ArgsRepair, copy: argsGuidance(t),
		})
	}
	if t := repairTierFor(c.MultiAction); t != RepairTierNone {
		blocks = append(blocks, repairGuidanceBlock{
			counter: counterMultiAction, tier: t, count: c.MultiAction, copy: multiActionGuidance(t),
		})
	}
	return blocks
}

// renderRepairGuidance returns the merged repair-guidance text for the
// current turn — one line per tripped counter, in the fixed finish →
// args → multi_action order — or the empty string when no counter is
// non-zero. The result is content for the `<additional_guidance>`
// section (the builder appends it below operator-supplied guidance).
//
// The function is a pure read of `c`: it never mutates the counters
// and never touches package state, so it is safe under the D-025
// concurrent-reuse contract.
func renderRepairGuidance(c *planner.RepairCounters) string {
	blocks := resolveRepairGuidance(c)
	if len(blocks) == 0 {
		return ""
	}
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		lines = append(lines, b.copy)
	}
	return strings.Join(lines, "\n")
}

// updateRepairCounters applies one planner step's outcome to the
// per-run [planner.RepairCounters] (Phase 83c — D-145). It is the
// runtime-side increment/reset call the plan assigns to "the runtime"
// — the ReAct planner owns it because the planner subtree cannot
// import internal/runtime (§13 import-graph contract) and the planner
// is the only component with both `rc` and the resolved decision in
// hand.
//
// A nil `rc.RepairCounters` means the runtime opted out of dynamic
// augmentation; the call is then a no-op.
//
// Reset / increment semantics (per the phase plan acceptance
// criteria):
//
//   - FinishRepair — incremented when `final` is a graceful-failure
//     [planner.Finish] (Reason [planner.FinishNoPath]): the repair
//     pipeline could not extract a usable action, the across-step
//     finish-quality signal. RESET to 0 on a clean
//     [planner.FinishGoal] finish.
//   - ArgsRepair  — incremented when `outcome.ArgsRepaired` (the
//     schema-repair path fired). RESET to 0 on a clean step (a single
//     well-formed action whose args validated first try).
//   - MultiAction — incremented when `outcome.MultiAction` (> 1
//     action salvaged). RESET to 0 on a clean single-action step.
//
// A clean tool-call step (no args repair, single action) resets BOTH
// ArgsRepair and MultiAction; it does NOT touch FinishRepair (a clean
// tool call is not a finish — finish quality is unchanged). A clean
// finish resets FinishRepair AND, since a finish is a single
// well-formed action, ArgsRepair + MultiAction too.
//
// The counters are the per-run [planner.RunContext] pointee — mutating
// them here is per-run-scoped state, NOT mutable state on the shared
// planner artifact (D-145 + D-025).
func updateRepairCounters(rc planner.RunContext, final planner.Decision, outcome repair.RepairOutcome) {
	c := rc.RepairCounters
	if c == nil {
		return
	}

	// ArgsRepair + MultiAction are classified directly from the
	// repair loop's outcome — increment on the signal, reset otherwise.
	if outcome.ArgsRepaired {
		c.ArgsRepair++
	} else {
		c.ArgsRepair = 0
	}
	if outcome.MultiAction {
		c.MultiAction++
	} else {
		c.MultiAction = 0
	}

	// FinishRepair is keyed to the resolved decision shape.
	if fin, ok := final.(planner.Finish); ok {
		switch fin.Reason {
		case planner.FinishGoal:
			// A clean, goal-reaching finish — the LLM produced a
			// usable terminal. Reset the finish-quality counter.
			c.FinishRepair = 0
		case planner.FinishNoPath:
			// Graceful-failure finish from the repair pipeline: the
			// runtime could not extract a usable action this step.
			// Escalate the across-step finish-quality signal.
			c.FinishRepair++
		default:
			// Cancelled / deadline / constraints — terminal for
			// reasons unrelated to output-format quality. Leave the
			// finish counter untouched.
		}
	}
	// A non-Finish decision (CallTool / CallParallel / SpawnTask /
	// AwaitTask) does not change FinishRepair — finish quality is
	// only observable on a finish.
}

// emitRepairGuidanceInjected publishes one
// [planner.EventTypePlannerRepairGuidanceInjected] event per rendered
// guidance block. Best-effort: a nil Emit closure (tests without
// observability) is a no-op. The function reads `rc.RepairCounters`
// via [resolveRepairGuidance] so the emitted events match exactly
// what [renderRepairGuidance] put into the prompt.
//
// The builder calls this from Build AFTER assembling the system
// content, so the emit reflects the guidance the LLM will actually
// see this turn.
func emitRepairGuidanceInjected(rc planner.RunContext) {
	if rc.Emit == nil {
		return
	}
	blocks := resolveRepairGuidance(rc.RepairCounters)
	if len(blocks) == 0 {
		return
	}
	now := nowFromRC(rc)
	for _, b := range blocks {
		rc.Emit(events.Event{
			Type:       planner.EventTypePlannerRepairGuidanceInjected,
			Identity:   rc.Quadruple,
			OccurredAt: now,
			Payload: planner.RepairGuidanceInjectedPayload{
				Identity:   rc.Quadruple,
				Tier:       string(b.tier),
				Counter:    b.counter,
				Count:      b.count,
				OccurredAt: now,
			},
		})
	}
}
