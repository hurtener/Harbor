# Phase 44 — Schema repair pipeline

## Summary

Land Harbor's reusable salvage / schema-repair / graceful-failure /
multi-action-salvage ladder under `internal/planner/repair/`. When an
LLM returns malformed `CallTool` arguments, the loop (a) salvages what
it can from sloppy JSON / fenced blocks / prose-wrapped output, (b)
runs each parsed `CallTool` through the tool's `Validate` schema, (c)
on validation failure builds a focused corrective sub-prompt and re-
asks the LLM up to `repair_attempts` times, and (d) after
`max_consecutive_arg_failures` consecutive arg-failures forces
`Finish{Reason: NoPath, Followup: true}` and emits
`planner.repair_exhausted` so observability picks up the failure
loudly (D-013 fail-loudly principle; D-025 concurrent-reuse). Multi-
action salvage emits a `CallParallel` when the LLM produced more than
one well-formed `CallTool`. The package houses both the `ActionParser`
(LLM-text → typed actions) and the `RepairLoop` (per-step repair
driver) — glossary lines 927 + 930 place both in this phase.

## RFC anchor

- RFC §6.2

## Briefs informing this phase

- brief 02
- brief 07
- brief 08

## Brief findings incorporated

- **brief 02 §6 (salvage / schema repair / graceful failure / multi-action salvage — `planner/repair/`, opt-in per concrete).** "1. **Salvage** — extract first valid JSON object from a malformed string; retry parse. 2. **Schema repair** — if action validates but tool args fail, emit a focused 'fix these missing/invalid fields' sub-prompt instead of regenerating the whole action. Configurable: `arg_fill_enabled`, `repair_attempts`, `max_consecutive_arg_failures`. 3. **Graceful failure** — after N consecutive arg-validation failures, force a `Finish{Reason: NoPath, Followup: true}` to avoid infinite repair loops on small models. 4. **Multi-action salvage** — if the LLM emitted several JSON objects in one response, queue the additional read-only tool calls for sequential execution without another LLM hop (configurable)." Phase 44 ships the ladder in exactly that order, configurable via the three named knobs.
- **brief 07 §3 (parser stack).** "`_extract_json_from_text` strips ` ```json ` fences, then falls back to 'first `{` to last `}`'." + "`normalize_action_with_debug` tries `json.loads` first, then a real-decoder-based scanner that can find multiple JSON objects in mixed prose." Harbor's `ActionParser` ships both passes: an `encoding/json` greedy decode for the happy path, fence-strip + decoder scan for the salvage path, and explicit error wrapping on every fall-through so the loop can build a focused corrective sub-prompt.
- **brief 07 §8 (parser+loop both belong with the planner subsystem).** "`ActionParser` (in `internal/runtime/planner/parser/`)" + "`RepairLoop`: drives parser → validator → planner-prompt-on-failure cycles up to `RepairAttempts` (default 3). Loud on exhaust." Master-plan glossary lines 927 + 930 confirm both ship in this phase under `internal/planner/repair/`. The brief's "internal/runtime/planner/parser/" path is superseded by the planner subsystem's RFC-stable home under `internal/planner/...` (Phase 42 settled this — planners do NOT import `internal/runtime/...`).
- **brief 07 §10 ("No retry storm guard on the repair loop").** "Harbor: the `RepairLoop` phase plan must include a per-session budget gate that aborts to a `final_response` with an error answer rather than spinning." Phase 44's `max_consecutive_arg_failures` is the storm guard; on exhaust the loop returns `Finish{Reason: NoPath, Followup: true}` — never an error, never an infinite spin. The `planner.repair_exhausted` emit is the loud failure surface.
- **brief 07 §10 ("Schema-repair loop is bounded but failure-mode-blind").** "If the model's response is *consistently* malformed, `_repair_attempts` (default 3) of identical-shape feedback may never converge." The loop bounds with `repair_attempts` AND a separate `max_consecutive_arg_failures` counter so identical-shape failures still terminate. Once the consecutive-failure counter trips, graceful failure fires regardless of remaining `repair_attempts` budget.
- **brief 08 §"How bifrost maps onto Harbor's LLMClient" — schema repair is runtime-side.** "Harbor's runtime owns the `ActionParser` / `Dispatcher` / `ObservationRenderer` / `RepairLoop` / `SchemaSanitizer`." The repair loop calls the supplied `LLMClient` (which has the Phase 36 retry-with-feedback wrapper composed already); the loop never adds a parallel retry layer. The two-parallel-implementations rule (CLAUDE.md §13) bans it. Repair = OUTPUT-shape concern (the loop owns the response). Phase 36 retry = LLM-CALL concern (the wrapper owns the call). Composition stays clean.

## Findings I'm departing from (if any)

- **brief 07 §8 puts `ActionParser` under `internal/runtime/planner/parser/`.** Departed: Harbor ships the parser under `internal/planner/repair/` alongside the `RepairLoop`. **Why:** Phase 42 settled that planner-package files MUST NOT import `internal/runtime/...` (the import-graph lint in `internal/planner/conformance/importgraph_test.go` is the §13 gate). The parser is a planner-side utility — it processes the LLM's text into Harbor's `planner.CallTool` shape and feeds the repair loop. Co-locating with the loop also matches master-plan glossary lines 927 + 930 ("`ActionParser` (`internal/runtime/planner/parser/`) | 44 (Schema repair pipeline) + 45 (Reference ReAct planner)" — Phase 44 owns the parser, Phase 45 wires it into the ReAct planner). The "runtime/planner/parser" path in brief 07 §8 is pre-RFC nomenclature; the RFC's settled home is `internal/planner/...`. This is consistent with D-047's "the planner package owns its own vocabulary" precedent — recorded as D-050 below.
- **brief 02 §6 sketches `Finish{Reason: NoPath, Followup: true}` — the `Followup` field is NOT on the current `Finish` struct.** Followed at the value level, departed at the field level: the loop sets `Finish.Metadata["followup"] = true` rather than adding a new field to the public `Finish` shape. **Why:** Phase 42 froze the `Finish` shape (Reason, Payload, Metadata); adding a `Followup bool` field would (a) require touching every Phase 45 / 48 / 49 concrete and the conformance pack, (b) re-litigate D-047 (the planner-package shape settled at Phase 42), (c) Metadata is already the documented surface for terminal-decision annotations. Recorded as D-050.

## Goals

- Ship `internal/planner/repair/` housing the `ActionParser` (LLM-text → `[]planner.CallTool`) and the `RepairLoop` (per-step repair driver).
- Ship the ladder in order: salvage → schema repair → graceful failure → multi-action salvage. Each step has a targeted unit test.
- Ship the three configuration knobs: `arg_fill_enabled` (bool; opt-in), `repair_attempts` (int; default 3), `max_consecutive_arg_failures` (int; default 2). Configurable per concrete (Phase 45 / 48 will pass their own values).
- Ship `planner.repair_exhausted` as a planner-side event type with a typed `SafePayload` carrying the identity quadruple, attempt count, and a truncated chain of validator failures. Register in `internal/planner/events.go`.
- Ship the integration test: a stub LLM that returns malformed JSON the first N attempts and valid JSON on the (N+1)th; assert the loop salvages correctly. Plus a negative test: malformed every time → loop returns `Finish{Reason: NoPath, Followup: true}` and `planner.repair_exhausted` fires.
- Ship the D-025 concurrent-reuse test: N=128 concurrent runs through a shared `RepairLoop` instance under `-race`. The compiled artifact is the loop config + parser + the (immutable) LLM-client reference; per-run state lives in `ctx` / `RunContext`.
- Coverage on `internal/planner/repair`: ≥ 85%.

## Non-goals

- No wiring into a concrete planner — Phase 45 (ReAct) consumes `RepairLoop.Run(ctx, rc, llm, prompt)` from its planner step. Phase 44 ships the loop as a reusable utility; the ReAct integration lives in Phase 45's PR.
- No prompt-template authoring beyond the focused-corrective-feedback shape — Phase 45 owns the system-prompt + tool-injection templates.
- No multi-LLM-hop salvage. Multi-action salvage in Phase 44 means "when the LLM emitted >1 well-formed CallTool in ONE response, queue them up"; it does NOT add a second LLM hop.
- No regex finish-extraction "last-ditch" fallback (brief 07 §3 step 6). Harbor prefers explicit `Finish{NoPath}` over guessing the user's answer from malformed JSON — recorded as a deliberate divergence from the predecessor's `react_step.py:412-432` pattern.
- No Phase 36 retry-feedback replacement. The loop calls a supplied `llm.LLMClient` (which already includes Phase 36's retry wrapper); repair is OUTSIDE the LLM-call boundary (it consumes the response). The two-parallel-implementations rule (§13) bans building a second retry layer.
- No conformance-pack scenarios for repair — Phase 49 fills the conformance scenarios; Phase 44 ships only the unit + integration + D-025 tests for the repair package itself.

## Acceptance criteria

- [ ] `internal/planner/repair/` package exists with the `ActionParser` and `RepairLoop` types.
- [ ] `ActionParser.Parse(text string) ([]planner.CallTool, error)` extracts one OR many `CallTool`s from raw LLM text. Tolerant: handles fenced JSON (` ```json ... ``` `), prose-wrapped JSON, multiple JSON objects in one response, and bare JSON arrays of CallTools. Returns `ErrNoActionsFound` when nothing parseable is present.
- [ ] `RepairLoop` config: `Config{ArgFillEnabled bool, RepairAttempts int, MaxConsecutiveArgFailures int}`. Defaults: `ArgFillEnabled=true`, `RepairAttempts=3`, `MaxConsecutiveArgFailures=2`. Negative / zero values fall back to defaults (defensive — Phase 45 / 48 configs are expected to set them explicitly).
- [ ] `RepairLoop.New(cfg Config) *RepairLoop` ships a reusable artifact. Construction is the only mutable step; per-call state lives on the stack / in ctx.
- [ ] `RepairLoop.Run(ctx, rc planner.RunContext, llm llm.LLMClient, req llm.CompleteRequest) (planner.Decision, error)` is the entry point. Identity is mandatory: missing identity in ctx returns `llm.ErrIdentityMissing` (the LLM-client edge enforces this; repair surfaces the same sentinel verbatim).
- [ ] **Step 1 (Salvage).** When the LLM's response parses cleanly into `[]CallTool` AND each CallTool's args validates against the tool's `Validate`, return `CallTool{...}` (single) or `CallParallel{Branches: [...]}` (many — see Step 4). Targeted unit test.
- [ ] **Step 2 (Schema repair).** When a parsed `CallTool` has args that fail the tool's `Validate`, build a corrective sub-prompt (`appendCorrectiveTurn`) naming the failing field + validator error, and re-call the LLM. Bounded by `RepairAttempts`. Targeted unit test.
- [ ] **Step 3 (Graceful failure).** After `MaxConsecutiveArgFailures` consecutive arg-validation failures (NOT counting non-arg errors), return `Finish{Reason: planner.FinishNoPath, Metadata: {"followup": true, "repair_chain": "..."}}` — never an error. Targeted unit test + `planner.repair_exhausted` event assertion.
- [ ] **Step 4 (Multi-action salvage).** When the parser returns >1 `CallTool` AND every one validates, emit `CallParallel{Branches: [...], Join: &JoinSpec{Kind: JoinAll}}`. Targeted unit test.
- [ ] **Event registration.** `planner.repair_exhausted` registered in `internal/planner/events.go`. Typed `RepairExhaustedPayload` (SafePayload) carries `Identity`, `Attempts`, `ConsecutiveArgFailures`, `Reasons []string` (each truncated), `OccurredAt`.
- [ ] **Fail-loudly emit (§13).** The loop emits `planner.repair_exhausted` on every graceful-failure path BEFORE returning `Finish{NoPath}`. No silent `Finish{}` returns.
- [ ] **D-025 concurrent-reuse test** (`internal/planner/repair/d025_test.go`). N=128 concurrent `RepairLoop.Run` calls against ONE shared loop instance. Each goroutine carries a unique identity; the validator forces a repair on roughly half. Asserts: no races, no identity bleed (each call's RunID round-trips out), no cancellation cross-talk (cancel one ctx → siblings still complete), no goroutine leak (baseline `runtime.NumGoroutine` restored).
- [ ] **Integration test** (`internal/planner/repair/integration_test.go`). A stub `llm.LLMClient` returns malformed JSON on attempts 1..N then valid JSON on attempt N+1; assert the loop returns the expected `CallTool` decision. Companion negative case: malformed every time → returns `Finish{NoPath}` + `planner.repair_exhausted` event observed on a real `events.EventBus`.
- [ ] **Identity propagation.** The integration test asserts every `Complete` call carries the run's identity quadruple in ctx (via `identity.WithRun`); the `planner.repair_exhausted` event payload's `Identity` field matches.
- [ ] `scripts/smoke/phase-44.sh` exists, is executable, runs `go test -race ./internal/planner/repair/...`, asserts the three config-knob names appear in the package source, and asserts the `planner.repair_exhausted` event type registered. No protocol surface yet (skip-shaped on that axis).
- [ ] `docs/decisions.md` D-050 records: (a) ladder ordering rationale (salvage → repair → graceful → multi-action), (b) why graceful failure is `Finish{NoPath}` not error, (c) `Followup` carried via `Metadata["followup"]` not a new field on `Finish`, (d) parser+loop both live under `internal/planner/repair/` not `internal/runtime/planner/parser/`.
- [ ] `docs/glossary.md` gains entries for: `ActionParser`, `RepairLoop`, `arg_fill_enabled`, `repair_attempts`, `max_consecutive_arg_failures`, `planner.repair_exhausted`, `FinishNoPath` (re-tightened).
- [ ] `docs/plans/README.md` Phase 44 row flips to `Shipped`.
- [ ] `README.md` Status table updated (if a Phase 44 row exists).
- [ ] Coverage on `internal/planner/repair`: ≥ 85%.

## Files added or changed

- `internal/planner/repair/repair.go` (new) — `Config`, `RepairLoop`, `New`, `Run`.
- `internal/planner/repair/parser.go` (new) — `ActionParser`, `Parse`, error sentinels.
- `internal/planner/repair/feedback.go` (new) — corrective-sub-prompt builder shared with Phase 36 retry's style (separate file; no import cycle).
- `internal/planner/repair/repair_test.go` (new) — unit per step (salvage, schema repair, graceful failure, multi-action).
- `internal/planner/repair/parser_test.go` (new) — parser unit tests (fenced JSON, prose-wrap, multi-object, bare array).
- `internal/planner/repair/integration_test.go` (new) — stub LLM + real event bus; positive (salvages on retry) + negative (graceful failure path with event assertion).
- `internal/planner/repair/d025_test.go` (new) — N=128 concurrent-reuse stress.
- `internal/planner/events.go` (modified) — register `planner.repair_exhausted` + ship `RepairExhaustedPayload`.
- `internal/planner/events_test.go` (new or modified) — assert the new type is registered.
- `internal/planner/errors.go` (modified) — `ErrRepairExhausted` sentinel for callers that want to inspect; the loop itself returns `Finish{NoPath}` (no error), but the parser surfaces `ErrNoActionsFound`.
- `scripts/smoke/phase-44.sh` (new) — assertions per "Smoke script additions" below.
- `docs/plans/phase-44-schema-repair.md` (this file).
- `docs/plans/README.md` (modified) — Phase 44 row → `Shipped`.
- `docs/decisions.md` (modified) — D-050 entry.
- `docs/glossary.md` (modified) — new vocabulary entries.
- `README.md` (modified — Status table reference).

## Public API surface

```go
package repair

import (
    "context"

    "github.com/hurtener/Harbor/internal/llm"
    "github.com/hurtener/Harbor/internal/planner"
)

// Config carries the three knobs the master plan / RFC §6.2 spec'd.
type Config struct {
    ArgFillEnabled            bool // opt-in; when false the loop returns parser+validate failures verbatim
    RepairAttempts            int  // default 3; total LLM re-asks before graceful-failure consideration
    MaxConsecutiveArgFailures int  // default 2; consecutive arg-validation failures that trip graceful-failure
}

// RepairLoop drives the salvage → repair → graceful failure →
// multi-action salvage ladder. The loop is a reusable artifact
// (D-025): one instance is safe to share across N concurrent runs.
// Per-run state lives in ctx + RunContext, never on the receiver.
type RepairLoop struct {
    cfg    Config
    parser *ActionParser
}

func New(cfg Config) *RepairLoop

// Run executes one planner step: parse → validate → repair → emit.
// On exhaustion returns Finish{Reason: NoPath, Metadata: {followup: true, ...}}.
func (l *RepairLoop) Run(
    ctx context.Context,
    rc planner.RunContext,
    client llm.LLMClient,
    req llm.CompleteRequest,
) (planner.Decision, error)

// ActionParser extracts one or more planner.CallTool actions from
// raw LLM text. Tolerant: fenced JSON, prose-wrap, multi-object scan.
type ActionParser struct{}

func NewParser() *ActionParser

// Parse returns the actions found in text. Order preserved (the
// LLM's order). Returns ErrNoActionsFound when nothing parseable.
func (p *ActionParser) Parse(text string) ([]planner.CallTool, error)

// Sentinels.
var (
    ErrNoActionsFound       = errors.New("repair: parser found no actions in LLM response")
    ErrArgsValidationFailed = errors.New("repair: tool args failed schema validation")
)
```

## Test plan

- **Unit:**
  - `parser_test.go` — happy path (single `CallTool` JSON), fenced JSON (` ```json ... ``` `), prose-wrapped JSON, multi-object scan (two `CallTool`s in one response), bare JSON array, malformed (no closing brace) → `ErrNoActionsFound`, empty string → `ErrNoActionsFound`. Property: parse(serialize(action)) round-trips byte-stable for canonical shapes.
  - `repair_test.go` — Step 1 salvage path; Step 2 schema-repair path with synthetic catalog whose `Validate` rejects until the corrective turn lands; Step 3 graceful-failure with `MaxConsecutiveArgFailures=2`, assert `Finish{NoPath, Metadata["followup"]=true}` and `planner.repair_exhausted` event; Step 4 multi-action salvage emits `CallParallel`. Plus defaults test (zero Config → DefaultConfig); plus ArgFillEnabled=false short-circuits the schema-repair path.
- **Integration:** `integration_test.go` wires real `events.EventBus` (inmem driver) + a stub `llm.LLMClient` driven by a per-test response table. Positive test: 2 malformed responses then 1 valid → loop returns `CallTool`. Negative test: 4 malformed responses → loop returns `Finish{NoPath}` + observes `planner.repair_exhausted` on the bus with the correct identity. Identity propagation pinned (each `Complete` call's ctx carries the run's quadruple).
- **Conformance:** N/A — Phase 49 fills the conformance pack's repair scenarios; this PR only ships the repair package's unit + integration + D-025 tests.
- **Concurrency / leak:** `d025_test.go` — N=128 concurrent `RepairLoop.Run` calls against ONE shared loop instance. Each goroutine carries a unique identity (tenant-N). The validator forces a single retry on i%2==0 goroutines. Asserts: no races (`-race`), no identity bleed (each call's RunID round-trips out via the response observation; per-call assertion), no cancellation cross-talk (a pre-cancelled ctx on i%3==0 returns ctx.Err()), no goroutine leak (baseline `runtime.NumGoroutine` restored within 500ms of WaitGroup join).

## Smoke script additions

`scripts/smoke/phase-44.sh`:

- Run `go test -race -count=1 -timeout 180s ./internal/planner/repair/...` → OK on pass / FAIL otherwise.
- Static guard: grep for the three config-knob names (`ArgFillEnabled`, `RepairAttempts`, `MaxConsecutiveArgFailures`) in `internal/planner/repair/repair.go` → FAIL if any is missing.
- Event-registry assertion: grep for `EventTypeRepairExhausted` (or `planner.repair_exhausted` literal) in `internal/planner/events.go` → FAIL on miss.
- Static guard: no `internal/runtime/...` import in `internal/planner/repair/` Go files (Phase 42 import-graph contract).
- Skip the HTTP / Protocol surface stub (Phase 60+) — repair has no protocol surface yet.

## Coverage target

- `internal/planner/repair`: 85%.

## Dependencies

- 42 (planner interface + Decision sum + RunContext — the loop returns `planner.Decision`).
- 32 (LLM client core — the loop calls `llm.LLMClient.Complete`).

## Risks / open questions

- **Parser tolerance vs. false positives.** A maximally-tolerant parser can mis-extract reasoning-channel JSON examples as actions. Mitigation: prefer the multi-object scanner (brief 07 §10 sharp edge) as the primary extractor; fall back to first-`{`/last-`}` only when scan finds zero objects. Test covers fenced ` ```python ` blocks adjacent to ` ```json ` action blocks.
- **Repair-attempt diversity.** Brief 07 §10: "if the model's response is *consistently* malformed, `_repair_attempts` (default 3) of identical-shape feedback may never converge." Mitigation: `MaxConsecutiveArgFailures` is a *separate* counter from `RepairAttempts` and is the load-bearing storm guard. Identical-shape failures still terminate via the consecutive-failure path.
- **`Followup` field on `Finish` vs `Metadata["followup"]`.** Carrying `Followup` via Metadata avoids touching Phase 42's frozen `Finish` shape. The conformance pack (Phase 49) reads `Metadata["followup"]` to detect the followup signal; documented in glossary.
- **Multi-action salvage ordering.** When the parser returns N actions, the `CallParallel` emit assumes the actions can run independently. Phase 47 (parallel execution) enforces the atomic-setup-validation contract; if the planner needs sequential salvage instead, the concrete (Phase 45 ReAct) opts out by setting `ArgFillEnabled=false` and salvages manually. Phase 44 ships parallel-salvage as the default.

## Glossary additions

- `ActionParser` — planner-side utility that extracts one OR many `planner.CallTool` actions from raw LLM text. Tolerant: handles fenced JSON, prose-wrap, multi-object scan, bare arrays. Lives in `internal/planner/repair/parser.go`. Phase 44 ships; Phase 45 (ReAct) consumes.
- `RepairLoop` — driver for the salvage → schema repair → graceful failure → multi-action salvage ladder. One method: `Run(ctx, rc, client, req) (Decision, error)`. Reusable artifact (D-025). Phase 44 ships.
- `arg_fill_enabled` — Phase 44 `RepairLoop` knob. When true (default), schema-validation failures on `CallTool.Args` trigger a focused corrective sub-prompt and re-ask; when false, the loop surfaces the parser's first valid action verbatim and lets the dispatcher reject it. Per-concrete knob.
- `repair_attempts` — Phase 44 `RepairLoop` knob. Total LLM re-asks before graceful-failure consideration. Default 3.
- `max_consecutive_arg_failures` — Phase 44 `RepairLoop` storm guard. Independent counter from `repair_attempts`; trips graceful failure when N arg-validation failures land in a row (even within `repair_attempts` budget). Default 2.
- `planner.repair_exhausted` — Phase 44 event. Emitted on the graceful-failure path (after `max_consecutive_arg_failures` consecutive failures OR `repair_attempts` exceeded). Typed `RepairExhaustedPayload` carries identity + attempt count + truncated reasons chain. The fail-loudly surface that makes graceful failure NOT silent.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. The `RepairLoop` IS a reusable artifact; `d025_test.go` ships the N=128 stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. Phase 44 consumes `internal/llm` (Phase 32) and `internal/planner` (Phase 42); `integration_test.go` wires real `events.EventBus` (inmem) + a stub `LLMClient` (the stub is allowed at the LLM boundary because Phase 33 is the only place that wires the bifrost driver; the stub here is a controlled test fixture, not a mock at the subsystem boundary).
- [ ] If new vocabulary: glossary updated — YES
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-050
