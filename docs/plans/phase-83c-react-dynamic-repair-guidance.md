# Phase 83c — react-dynamic-repair-guidance

## Summary

Add the per-turn dynamic augmentation pass described in brief 13 §2.2: planner-level **failure counters** (finish-repair, args-repair, multi-action) drive escalating system-prompt **repair guidance** (`reminder` → `warning` → `critical`) merged into the system prompt for one turn at a time. Wire `RunContext.PlanningHints` into the `<planning_constraints>` section established by Phase 83a. Together these close the across-step feedback loop that today depends entirely on `MaxSteps` as a circuit breaker.

## RFC anchor

- RFC §6.2

## Briefs informing this phase

- brief 13
- brief 02
- brief 03

## Brief findings incorporated

- brief 13 §2.2: "Without it the same misformatted-action bug repeats every step until `MaxSteps` trips — there is no signal to the LLM that 'you've been doing this wrong'." — closing this loop is the core goal.
- brief 13 §2.2: "Phase 44 catches and repairs malformed JSON *inside* one step; this dynamic guidance closes the *across*-step feedback loop." — explicit coordination contract with Phase 44. Repair guidance does NOT replicate Phase 44's per-step repair; it counts failures Phase 44 had to repair and escalates if the trend doesn't reverse.
- brief 13 §2.5: "The runtime can swap planning_hints per session — useful for tenant-specific policy or for guiding the planner around a known-bad path." — operators get a deliberate steering knob without writing Go.
- brief 02 §2 (silent context loss): the failure-counter pattern is the runtime-level analogue: explicit signal, explicit response.

## Findings I'm departing from (if any)

- The predecessor stores failure counters on the planner instance, persisting across runs ("no orchestrator wiring required" — brief 13 §2.2). Harbor scopes counters to the run via `RunContext` instead, because Harbor's `ReActPlanner` is a **shared compiled artifact** (D-025) — mutable per-run state on the planner struct violates the concurrent-reuse contract (AGENTS.md §5). Departure recorded as **D-145** in `docs/decisions.md`.

## Goals

- A run that emits a finish action which fails Phase 44 validation increments a per-run `finishRepairCount`. The next turn renders a `reminder`-tier guidance block in `<additional_guidance>` (or a dedicated `<repair_guidance>` block — implementation choice; see §3 risks).
- Same for args validation (`argsRepairCount`) and multi-action / multi-JSON-block emission (`multiActionCount`).
- Counters reset on a successful turn at that surface (a successful finish resets `finishRepairCount`; a successful single-action with valid args resets the other two).
- Tiered hints: count 1 → `reminder` copy, count 2 → `warning` copy, count ≥ 3 → `critical` copy. Hint copy lives in `internal/planner/react/repair_guidance.go` as exported constants so operators can grep them.
- `RunContext.PlanningHints` (new field) rendered into `<planning_constraints>` when present; omitted entirely otherwise. Fields: `Constraints`, `PreferredOrder []string`, `ParallelGroups [][]string`, `DisallowTools []string`, `PreferredTools []string`, `Budget *BudgetHints`.
- Counters and hints emit `planner.repair_guidance_injected` events (event taxonomy — Phase 05) with tier + counter-name attributes so the Console / operator can see when the LLM is struggling.

## Non-goals

- Cross-run learning (persisting counters across sessions). Not in scope; per-run scope is the chosen contract.
- Auto-tightening JSON output via `structured_output` (Phase 35). Coordination is documented (see §3 risks) but Phase 83c is prompt-side only.
- Tool-call-failure feedback (a tool failed, so suggest alternative tools). The reference design tracks tool-execution errors separately; we leave that for a later phase if measurement says it's needed. Phase 83c covers only LLM-output-format failures.
- Render-component failure tracking. Deferred along with rich-output (V2).

## Acceptance criteria

- [ ] `RunContext` (`internal/planner/planner.go`) gains `RepairCounters *RepairCounters` and `PlanningHints *PlanningHints` fields. Both are pointers — `nil` means "no augmentation."
- [ ] `RepairCounters{FinishRepair int, ArgsRepair int, MultiAction int}` — counter type lives in `internal/planner` (used by the runtime, read by the React planner).
- [ ] `PlanningHints` type lives in `internal/planner` with the fields enumerated in Goals. `BudgetHints{MaxSteps *int, MaxCostUSD *float64, MaxLatencyMS *int64}`.
- [ ] The runtime updates `RepairCounters` between steps (in the Phase 44 repair path + the multi-action detection path).
- [ ] `defaultBuilder.Build` reads the counters; for each non-zero counter that has a tier-matching hint, the hint is merged into the system prompt for *this turn only*.
- [ ] A successful step resets the relevant counter (the runtime's responsibility; tested via integration test).
- [ ] Hint tier copy lives in exported constants (`ReminderFinishGuidance`, `WarningFinishGuidance`, `CriticalFinishGuidance`, …); copy itself is reviewable as part of this PR.
- [ ] Event emission: `planner.repair_guidance_injected` with `{tier: "reminder"|"warning"|"critical", counter: "finish"|"args"|"multi_action", count: N}`.
- [ ] Concurrent-reuse: counter mutation is keyed to `RunContext`, not the planner struct (D-145). Test: 100+ concurrent runs against a shared `ReActPlanner`, each with its own `RunContext`, assert no cross-run counter bleed.
- [ ] Golden fixtures for tier copy: three text files per counter type (9 total) under `internal/planner/react/testdata/repair_guidance/` so copy changes show up as a reviewable diff.

## Files added or changed

- `internal/planner/planner.go` — add `RepairCounters`, `PlanningHints`, `BudgetHints` types + `RunContext` fields.
- `internal/planner/react/repair_guidance.go` (new) — hint constants + `renderRepairGuidance(c *RepairCounters) string` helper.
- `internal/planner/react/planning_hints.go` (new) — `renderPlanningHints(h *PlanningHints) string` helper.
- `internal/planner/react/prompt.go` — call both helpers in `buildSystemContent`; document the dynamic-augmentation pass at the top of the file.
- `internal/runtime/engine/...` (or wherever the planner loop drives Phase 44 repair) — increment / reset counter calls.
- `internal/events/taxonomy.go` (or equivalent) — add `EventTypePlannerRepairGuidanceInjected`.
- `internal/planner/react/testdata/repair_guidance/{finish,args,multi_action}_{reminder,warning,critical}.txt` — nine golden files.
- `internal/planner/react/repair_guidance_test.go` — extensive table-driven tests.
- `internal/planner/react/integration_test.go` (existing) — extend with end-to-end finish-failure-recovery flow.
- `docs/decisions.md` — **D-145** "Repair counters live in `RunContext`, not on the `ReActPlanner` struct" (≥10 lines: context, decision, consequences).
- `scripts/smoke/phase-83c.sh` — static-only assertions on the nine golden files + the event-taxonomy enum.
- `docs/glossary.md` — fill in `Repair guidance` and `Planning hints` entries (placeholders added in Phase 83a).
- `docs/plans/README.md` — Status column flip on merge.

## Public API surface

```go
// internal/planner/planner.go (delta)

type RepairCounters struct {
    FinishRepair int
    ArgsRepair   int
    MultiAction  int
}

type BudgetHints struct {
    MaxSteps     *int
    MaxCostUSD   *float64
    MaxLatencyMS *int64
}

type PlanningHints struct {
    Constraints    string
    PreferredOrder []string
    ParallelGroups [][]string
    DisallowTools  []string
    PreferredTools []string
    Budget         *BudgetHints
}

type RunContext struct {
    // ...existing fields...
    RepairCounters *RepairCounters // NEW
    PlanningHints  *PlanningHints  // NEW
}
```

## Test plan

- **Unit:**
  - Tier-mapping tests: each counter value 0/1/2/3 produces the expected tier (`none`/`reminder`/`warning`/`critical`).
  - Golden tests against the nine fixture files.
  - `renderPlanningHints` with empty / partial / full hints — partial omits absent fields entirely, never emits empty lines.
  - Race test: two parallel `defaultBuilder.Build` calls with disjoint `RunContext` instances do not cross-contaminate counters or hints (the test creates two `RunContext` instances with different counter values and asserts the rendered output reflects each).
- **Integration:**
  - End-to-end finish-recovery: drive a planner with a stub LLM that emits an invalid finish on turn 1, then a valid finish on turn 2. Assert the turn-2 system prompt contains `ReminderFinishGuidance`, the run completes, and `planner.repair_guidance_injected` event was published with `tier=reminder`.
  - Escalation: same but stub fails finishes 3 times then succeeds on turn 4. Assert tiers `reminder`, `warning`, `critical`, then no hint on turn 4 after reset.
  - Cross-run isolation: two concurrent runs share a `ReActPlanner`; run A has 3 finish failures, run B has 0. Assert run B's prompts never contain repair guidance.
- **Conformance:** existing planner conformance pack re-runs. Add a new optional conformance test for repair-counter wiring that all `Planner` implementations may opt into (this matters if Phase 90 brings additional planner concretes — they should share the same counter contract).
- **Concurrency / leak:** **mandatory** — D-025 + the explicit cross-run isolation contract make this the highest-risk surface in 83a–d. N≥100 concurrent runs against a single shared `ReActPlanner`; each run has its own `RunContext` with random counter values; assert renders are correctly scoped.

## Smoke script additions

- `scripts/smoke/phase-83c.sh` (classification: `static-only`):
  - Assert the nine repair-guidance golden files exist.
  - Grep each tier file for the tier name itself in the body (defensive: a typo that re-uses the same copy across tiers shows up here).
  - Assert `EventTypePlannerRepairGuidanceInjected` appears in the events taxonomy file (`internal/events/...`).

## Coverage target

- `internal/planner/react`: 85%.
- `internal/planner`: 90% (the new types + counter wiring sit in the planner ifaces package; isolation tests carry weight).

## Dependencies

- 83a (structured-section builder).
- 44 (schema repair pipeline — the runtime hook where we increment counters).
- 05 (event taxonomy — new event type registered here).

## Risks / open questions

- **Coordination with Phase 35 (Structured Output Strategies).** If a provider supports structured output / function-calling natively, the repair counters may never trip in the first place. Documented contract: repair guidance is **additive** and ignored by providers that natively constrain output. The counters still increment if Phase 44 had to repair, regardless of provider — that signal is real and worth surfacing.
- **Hint copy needs care.** Bad copy is worse than no hint (an aggressive `critical` tier hint can confuse the model). The constants live in code precisely so copy changes are reviewed; the 9 golden files make the diff legible at PR time.
- **Counter-reset semantics on `parallel` decisions.** A parallel plan that has 3 branches and 2 succeed + 1 fails — does that count as a finish/args failure? Decision: parallel branch failures do **not** increment the finish/args counters (they are tool-execution failures, not LLM-output-format failures). Documented in `repair_guidance.go` and in **D-145**.
- **D-145 numbering.** Phase 83c is pre-assigned **D-145** by the Wave 15 dispatch (the prior speculative "D-105" in earlier drafts is superseded). D-148 is the highest number already merged (Phase 83e); D-145 is free. The README's preflight gate prevents two PRs landing the same D-NNN.

## Glossary additions

- **Repair guidance** — A per-turn system-prompt augmentation emitted by the React planner's prompt builder when a `RunContext.RepairCounters` field is non-zero. Tiered: `reminder` (count 1), `warning` (count 2), `critical` (count ≥ 3). Counters are incremented by the runtime when Phase 44's schema repair pipeline had to fix an output, and reset when a clean turn lands. Closes the across-step feedback loop that Phase 44 (per-step repair) leaves open.
- **Planning hints** — Runtime-supplied constraints rendered into the `<planning_constraints>` section of the system prompt. Fields: ordering preferences, allowed parallel groups, disallowed tools, preferred tools, budget caps. Populated on `RunContext.PlanningHints`; nil means "no hints, omit the section." Useful for tenant-specific policy or for guiding the planner around a known-bad path.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — **required** (cross-run counter scope is the headline guarantee). Passes.
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent runs against a single shared `ReActPlanner` + disjoint `RunContext.RepairCounters`. The d025_test extension is mandatory.
- [ ] Integration test — required (Deps lists 44 + 05). Real Phase 44 + real Phase 05 bus assertion.
- [ ] Glossary updated (Repair guidance + Planning hints).
- [ ] `docs/decisions.md` D-145 entry filed.
