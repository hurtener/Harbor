# Phase 83a — react-prompt-structured-sections

## Summary

Refactor `internal/planner/react/prompt.go`'s `defaultBuilder` from a single flat string into the twelve XML-tagged sections inventoried in brief 13 §2.1 (`<identity>`, `<output_format>`, `<action_schema>`, `<finishing>`, `<tool_usage>`, `<parallel_execution>`, `<reasoning>`, `<tone>`, `<error_handling>`, `<available_tools>`, `<additional_guidance>`, `<planning_constraints>`). Introduce explicit injection points (`extra_guidance`, `current_date`) and a `PlannerConfig.ExtraGuidance` config key so operators can shape the prompt without forking the builder. The adapted prompt **drops the `reasoning` field from the action JSON** and ports the predecessor's `<tone>` CRITICAL clamp verbatim (`reasoning` is captured from the provider channel — Phase 83e — never required in structured output). The `<finishing>` block carries **only** `answer`; rich-output fields (`confidence` / `route` / `requires_followup` / `warnings`) are not reserved, not deferred — Harbor delivers rich UI via MCP-Apps tools (brief 13 §5). The downstream phases (83b/c/d) extend the sections this phase establishes.

## RFC anchor

- RFC §6.2

## Briefs informing this phase

- brief 13

## Brief findings incorporated

- brief 13 §2.1: "XML tags make the sections individually editable" — we adopt the predecessor's twelve-section layout in the same fixed order so per-section edits stay local.
- brief 13 §2.1: "The `extra` and `planning_hints` slots are explicit injection points" — this phase wires `extra_guidance`; 83c wires `planning_hints`.
- brief 13 §4: "`{{current_date}}` … Date-only is deliberate: it stays stable across a session, which helps KV-cache hit rates" — we adopt date-only injection, no time-of-day, to preserve cache stability.
- brief 13 §2.6 (revision 2026-05-19): the predecessor's `<tone>` CRITICAL clamp is ported verbatim — *"During intermediate steps, produce ONLY the JSON action object. Do not add commentary."* + *"Do not include a 'thought' or 'reasoning' field in the JSON."* The action schema in the rendered prompt is `{tool, args}` only. Reasoning capture lives on Phase 83e (runtime surface + Decision-side schema narrowing); this phase's contribution is **prompt-side** alignment: the model is told not to emit reasoning, and the rendered prompt's `<action_schema>` example omits the field.
- brief 13 §5: rich output is dropped from Harbor entirely. The `<finishing>` block carries only `answer`; the `<error_handling>` block guides "ask for clarification in `args.answer`" rather than via a `requires_followup` flag. Rich UI is delivered via MCP-Apps tools, not planner emission.

## Findings I'm departing from (if any)

- **Initial draft of this plan (committed 2026-05-18) reserved V2 finish-args fields and kept `reasoning` in the action schema.** The 2026-05-19 brief revision drops both. This plan now matches the revised brief.

## Goals

- The shipped prompt assembles in the same twelve-section order as brief 13 §2.1.
- Operators can inject domain-specific interpretation rules via a config key without writing Go.
- The prompt's `<action_schema>` example shows `{tool, args}` only — no `reasoning` field. The `<tone>` section carries the CRITICAL clamp instructing the model not to emit `thought` / `reasoning` in JSON.
- The `<finishing>` section instructs only `args.answer` (plain text). Rich-output fields are not reserved, not described as "future."
- The Decision shape (runtime-side) is **unchanged** in this phase — Phase 83e tightens it. 83a is prompt-content-only; the runtime still accepts a `reasoning` field if the model emits one (Phase 44 strips it, soon to be a no-op once Phase 83e lands).
- All existing `WithSystemPrompt(string)` callers keep working unchanged (backwards-compatible default).

## Non-goals

- Tool catalog rendering depth (no `args_schema`, no examples). That is Phase 83b.
- Dynamic per-turn augmentation (`planning_hints`, repair guidance). That is Phase 83c.
- Memory / skills injection. That is Phase 83d.
- Narrowing the runtime-side Decision sum (dropping `reasoning` from `Decision_CallTool`) + extending `CompleteResponse` with the captured reasoning trace + adding the replay knob. That is Phase 83e.
- Reserving any rich-output fields on `_finish`. Harbor does not build rich-output emission; rich UI is via MCP-Apps tools.

## Acceptance criteria

- [ ] `defaultBuilder.buildSystemContent` returns a string with each of the twelve sections in the order documented in brief 13 §2.1, separated by `\n\n`.
- [ ] Empty / missing optional injections (`extra_guidance`, planning hints) omit their section entirely rather than emitting an empty `<additional_guidance></additional_guidance>` block.
- [ ] `current_date` is rendered as `YYYY-MM-DD` (UTC, date-only). A test asserts the format does not include time-of-day.
- [ ] `PromptBuilder` interface keeps its current signature; `defaultBuilder` keeps the `WithSystemPrompt(string)` override semantics (empty string → `DefaultSystemPrompt`).
- [ ] **`<action_schema>` and the example actions in the rendered prompt contain NO `reasoning` field.** A golden-fixture test asserts the rendered prompt does not contain the substring `"reasoning":` (case-sensitive).
- [ ] **`<tone>` ports the predecessor's CRITICAL clamp verbatim** (case-sensitive). Specifically the two lines (a) "During intermediate steps, produce ONLY the JSON action object. Do not add commentary." and (b) "Do not include a 'thought' or 'reasoning' field in the JSON." Golden-fixture test asserts both lines appear.
- [ ] **`<finishing>` block carries only `args.answer`** (no `confidence` / `route` / `requires_followup` / `warnings` / "optional fields you may include"). A golden-fixture test asserts none of those strings appear in the rendered prompt.
- [ ] **`<error_handling>` block does NOT reference `requires_followup`** — the guidance is "ask for clarification in `args.answer`."
- [ ] New Option `WithSystemPromptExtra(s string)` on `ReActPlanner` injects content into the `<additional_guidance>` section.
- [ ] New config key `planner.extra_guidance` on `PlannerConfig` flows from YAML through `internal/config` validation to the planner constructor.
- [ ] `DefaultSystemPrompt` is replaced with a structured template constant (or with per-section constants concatenated by `buildSystemContent`); the migration is **not silent** — the old single-string constant either is removed or is renamed to `legacyDefaultSystemPrompt` with a TODO comment + tracking issue, never left dangling.
- [ ] Concurrent-reuse test: 100+ concurrent `Build` calls against a single `defaultBuilder` instance pass under `-race` (D-025; carries over from Phase 45's contract).
- [ ] Golden test: assert the full system prompt with no tools and no extra_guidance matches a checked-in fixture (the fixture *is* the spec — review surface is one file).

## Files added or changed

- `internal/planner/react/prompt.go` — extend `defaultBuilder` to emit twelve sections; add `extraGuidance` field + helper functions per section.
- `internal/planner/react/react.go` — replace flat `DefaultSystemPrompt` constant; add `WithSystemPromptExtra(s string)` Option.
- `internal/planner/react/testdata/golden_default_prompt.txt` — golden fixture for the no-tools / no-extras default.
- `internal/planner/react/prompt_test.go` — extend to assert section order + presence + omission rules.
- `internal/config/config.go` — add `ExtraGuidance string` field to `PlannerConfig`.
- `internal/config/loader.go::Validate` — no validation rule beyond "string" (operator copy is operator copy).
- `examples/harbor.yaml` — document the new key with a commented-out example.
- `scripts/smoke/phase-83a.sh` — smoke script with `static-only` assertions per §4.2.
- `docs/glossary.md` — add `Repair guidance`, `Planning hints`, `UNTRUSTED memory framing`, `Tool example` (some glossary entries are placeholders for 83b/c/d — they land here because 83a is the foundation PR).
- `docs/plans/README.md` — flip the Phase 83a row's Status column from `Pending` to `Shipped` on merge.

## Public API surface

```go
// internal/planner/react/react.go

// WithSystemPromptExtra injects operator-supplied guidance into the
// <additional_guidance> section of the rendered system prompt.
// The string is rendered verbatim; the operator is responsible for content
// hygiene. Empty string is a no-op.
func WithSystemPromptExtra(s string) Option { /* ... */ }

// internal/config/config.go (delta)

type PlannerConfig struct {
    Driver        string
    MaxSteps      int
    ExtraGuidance string            // NEW — flows to WithSystemPromptExtra at construction.
    Extra         map[string]string // unchanged (free-form per-driver knobs)
}
```

`PromptBuilder` interface signature is unchanged.

## Test plan

- **Unit:**
  - Golden test for `defaultBuilder.Build` with empty catalog / no extras (fixture-driven).
  - Section-presence tests: parameterised — for each of the twelve tags, assert it appears exactly once.
  - Omission tests: empty `extra_guidance` does NOT emit `<additional_guidance>`.
  - Date format test: `current_date` matches `^\d{4}-\d{2}-\d{2}$` and contains no `T` / colon / space.
  - Config-layering test: YAML `planner.extra_guidance: "foo"` produces a planner that emits `<additional_guidance>\nfoo\n</additional_guidance>`.
- **Integration:** none required (Deps row is 45 only; the existing Phase 45 integration test set still covers end-to-end planner behaviour and re-runs on this PR).
- **Conformance:** the existing planner conformance pack (Phase 49) re-runs unchanged; no new conformance work.
- **Concurrency / leak:** existing `d025_test.go` concurrent-reuse test extended to cover the new builder configuration (single shared `defaultBuilder` + `WithSystemPromptExtra`, 100 parallel `Build` calls under `-race`).

## Smoke script additions

- `scripts/smoke/phase-83a.sh` (classification: `static-only`):
  - Assert `internal/planner/react/testdata/golden_default_prompt.txt` exists.
  - Assert the golden contains all twelve required XML tag openers, each on its own line.
  - Assert the file contains no `{{` template markers (catches an un-rendered placeholder regression).
  - `go test ./internal/planner/react/...` invocation is OUT of smoke scope (preflight runs `go test` separately); the smoke focuses on static fixture invariants.

## Coverage target

- `internal/planner/react`: 85% (unchanged from Phase 45's target).

## Dependencies

- 45 (reference ReAct planner — provides `defaultBuilder` + `PromptBuilder` interface).

## Risks / open questions

- **Migration risk.** Any existing operator who has memorised the previous default prompt text and asserts against it will see the assertion break. Mitigated by: (a) marking this as post-V1; (b) treating the prompt-content change as a deliberate API surface bump; (c) the golden fixture being the *normative* spec going forward.
- **Token budget.** The twelve-section layout is materially longer (~1.2–1.8k tokens before tool rendering) than the current flat prompt (~200 tokens). For tiny models or extreme cost regimes this is non-trivial. Mitigation: `WithPromptBuilder()` already exposes the full escape hatch; operators who need the lean prompt construct their own builder. No new knob required.
- **Trained-in-format drift on dropped fields.** Some models trained on prompts similar to the predecessor's will emit `reasoning` / `confidence` / `route` / `requires_followup` anyway. Phase 44's schema repair pipeline must tolerate (strip-and-warn) extra fields on incoming JSON. A test asserts the planner's Decision parser silently drops these extras and logs a soft event for telemetry. The CRITICAL clamp in `<tone>` is the format pressure that makes drift uncommon over many turns.

## Glossary additions

- **Repair guidance** — placeholder; full definition lands with Phase 83c.
- **Planning hints** — placeholder; full definition lands with Phase 83c.
- **UNTRUSTED memory framing** — placeholder; full definition lands with Phase 83d.
- **Tool example** — placeholder; full definition lands with Phase 83b.

(All four placeholder entries are filled with definitions in brief 13 §10. Linking them to the brief makes the glossary self-explanatory even before the dependent phases ship.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (prompt content is identity-agnostic; identity flows on the LLM request envelope, not the prompt body).
- [ ] **Concurrent-reuse test passes** — 100+ concurrent `Build` calls against a single shared `defaultBuilder` under `-race`. (Required: `defaultBuilder` is a reusable artifact.)
- [ ] Integration test — N/A. Dependencies list only Phase 45 and no new cross-subsystem seam is opened by a content-only refactor.
- [ ] If new vocabulary: glossary updated.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departures).
