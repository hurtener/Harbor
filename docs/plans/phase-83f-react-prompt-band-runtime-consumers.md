# Phase 83f — react-prompt-band-runtime-consumers

## Summary

Phase 83f closes the Wave-15 dev-binary consumer gap (issue #208 — surfaced
by the §17.5 Wave 15 checkpoint audit). The 83-band shipped four operator-
facing per-run primitives — `RunContext.RepairCounters`, `RunContext.PlanningHints`,
`RunContext.MemoryBlocks`, `RunContext.SkillsContext` — and the 83e reasoning-
trace capture path, but `cmd/harbor/cmd_dev_runloop.go::runOne` only populates
`Quadruple`. Operators running `harbor dev` therefore benefit from 83a's
twelve-section prompt and 83b's enriched `<available_tools>` (both wired
through the planner factory) but never see memory injection, skills injection,
repair guidance, or planning hints. 83f wires the dev run loop to populate
all four primitives from the real Memory + Skills + Task stores, threads a
fresh `RepairCounters` per run, and verifies the 83e reasoning trace round-
trips dev → planner → trajectory in a real run loop.

## RFC anchor

- RFC §6.2 — Planner subsystem (`PlannerConfig.PlanningHints`, repair
  guidance, the per-run mutable state contract).
- RFC §6.5 — LLM subsystem (reasoning-trace capture on
  `llm.CompleteResponse.Reasoning`).
- RFC §6.6 — Memory subsystem (`MemoryStore.GetLLMContext` is the
  identity-scoped fetch the runtime calls per run).
- RFC §6.7 — Skills subsystem (`SkillStore.Search` is the per-run retrieval).

## Briefs informing this phase

- brief 13
- brief 04
- brief 02

## Brief findings incorporated

- brief 13 §2.3: memory is injected with UNTRUSTED framing — the runtime
  fetches identity-scoped blobs and hands them to the planner; the planner
  renders. 83f closes the half the dev binary never wired (fetch).
- brief 13 §2.4: skill bodies are pre-retrieved by the runtime and reach
  the planner via `RunContext.SkillsContext`, capped to keep prompt cost
  bounded. 83f introduces the cap as an operator-controlled config knob
  (`planner.skills_context_max`, default 5).
- brief 13 §2.2: repair counters scope to per-run state. 83f's run loop
  allocates one `*RepairCounters` per run and threads the same pointer
  through every step's `RunContext` (the D-145 contract on the producer
  side; the consumer side already lands in 83c).
- brief 02 §6: the runtime is the per-run state owner; the planner is the
  reader. 83f honours that split — every fetch happens at the run-loop
  boundary, never inside the planner.

## Findings I'm departing from (if any)

None. 83f is a pure consumer phase against already-shipped primitives — no
new design surface, no brief findings to depart from.

## Goals

- The dev binary's `perTaskRunLoopDriver` populates `RunContext.MemoryBlocks`,
  `RunContext.SkillsContext`, `RunContext.RepairCounters`, and
  `RunContext.PlanningHints` for every run.
- The user-facing `task.Query` is set as the run's `Query` + `Goal` on the
  `RunContext` (it is the natural starting goal).
- The 83e reasoning capture path round-trips end-to-end in the dev binary:
  a planner step whose LLM returned `Reasoning` produces a `TrajectoryStep`
  whose `ReasoningTrace` field carries it (and `harbor inspect-runs` shows
  it when `--with-reasoning` is set).
- Operators get a real working agent on `harbor dev` — with memory, skills,
  repair guidance, planning hints — without any extra wiring.

## Non-goals

- No new planner-side rendering — 83a/b/c/d/e own the rendering contract.
- No long-term external-memory tier — `MemoryBlocks.External` stays nil in
  V1.1; the Memory subsystem's V1 tier ships only the conversation
  recent-turns window via `GetLLMContext` (deferred to a future memory phase).
- No PlanningHints UI surface (CLI flag etc.) beyond the config key —
  richer per-tenant policy lands later.
- No re-implementation of Phase 44's repair pipeline — 83c already updates
  the counters; 83f only allocates and threads them.

## Acceptance criteria

- [ ] `cmd/harbor/cmd_dev_runloop.go::runOne` populates `RunContext.MemoryBlocks`
      (from `MemoryStore.GetLLMContext`), `RunContext.SkillsContext` (from
      `SkillStore.Search` keyed by `task.Query`, capped at the configured
      maximum), `RunContext.RepairCounters` (a fresh pointer per run), and
      `RunContext.PlanningHints` (from the operator-supplied config) before
      calling `runLoop.Run`.
- [ ] `RunContext.Query` and `RunContext.Goal` are set from `task.Query`.
- [ ] Two new `harbor.yaml` config keys: `planner.skills_context_max` (int,
      default 5; validator rejects negatives) and `planner.planning_hints`
      (struct with `constraints` and `preferred_tools` for V1.1; validator
      rejects unknown fields).
- [ ] The `perTaskRunLoopDriverOpts` struct gains `memory memory.MemoryStore`
      and `skills skills.SkillStore` dependencies — both mandatory when the
      respective subsystems are configured; `bootDevStack` wires them.
- [ ] Memory + skills fetch failures are loud — the run fails with the
      wrapped error (logged at Warn, `MarkFailed` with code
      `runtime_fetch_error`). No silent degradation to nil blocks.
- [ ] A dev-binary integration test (`test/integration/phase83f_runloop_consumers_test.go`)
      boots the dev stack, spawns a task with a known `Query`, runs one
      planner step against a capturing LLM, and asserts the captured
      request carries: the four UNTRUSTED memory/skills wrappers (proving
      fetch + population), the `<planning_constraints>` block (proving
      PlanningHints threaded), and the reasoning trace landing on
      `TrajectoryStep.ReasoningTrace` (proving the 83e round-trip).
- [ ] `scripts/smoke/phase-83f.sh` asserts the new config keys are
      documented in `examples/harbor.yaml` and that the driver's signature
      carries the Memory + Skills deps.
- [ ] `harbor inspect-runs --with-reasoning` shows captured reasoning for
      a run that produced one (manually verified by the operator post-merge).

## Files added or changed

- `cmd/harbor/cmd_dev_runloop.go` — populate the four primitives + Query/Goal
  in `runOne`; extend `perTaskRunLoopDriverOpts`; fail-loud on fetch errors.
- `cmd/harbor/cmd_dev.go` — `bootDevStack` threads MemoryStore + SkillStore
  plus the new config knobs into `perTaskRunLoopDriverOpts`.
- `internal/config/config.go` — add `PlannerConfig.SkillsContextMax`
  (`skills_context_max`) + `PlannerConfig.PlanningHints` (`planning_hints`
  with `constraints` + `preferred_tools`).
- `internal/config/validate.go` — validate the two new keys.
- `examples/harbor.yaml` — document the new keys as commented examples.
- `docs/plans/README.md` — add the Phase 83f row; flip Status to Shipped;
  resolve the W3/W4 footnote.
- `docs/plans/phase-83f-react-prompt-band-runtime-consumers.md` — this plan.
- `docs/decisions.md` — D-149 (the fetch-and-populate shape: where, when,
  fail-loud-or-soft).
- `test/integration/phase83f_runloop_consumers_test.go` — the binding
  consumer-side end-to-end test.
- `scripts/smoke/phase-83f.sh` — static-only smoke for the doc + config
  surface.

## Public API surface

None new at the public Go-package boundary — 83f populates existing
`planner.RunContext` fields and adds two new config keys (which are
operator-facing YAML, not Go API). The four primitives the wiring populates
(`RunContext.MemoryBlocks`, `SkillsContext`, `RepairCounters`,
`PlanningHints`) are already on the public surface from 83c/83d.

## Test plan

- **Unit:** Config validation for `planner.skills_context_max` (≥0, default 5)
  and `planner.planning_hints` (allowed fields only).
- **Integration:** `test/integration/phase83f_runloop_consumers_test.go` —
  boots `harbortest/devstack.Assemble`, spawns a task, drives one planner
  step against a capturing LLM client, asserts the captured request shape
  (memory wrappers, skills wrapper, planning constraints, reasoning trace
  round-trip). Identity propagation asserted at every fetch boundary.
- **Failure-mode:** Force `MemoryStore.GetLLMContext` to return an error
  (e.g. via a wrapping store that fails on demand); assert the run fails
  loud with `MarkFailed(code=runtime_fetch_error)` and the LLM is never
  called. Same shape for `SkillStore.Search`.
- **Concurrency / leak:** Spawn N≥10 tasks in parallel through the driver;
  assert each run's captured prompt carries its own task's marker (the
  Memory + Skills fetches must be identity-scoped, never cross-tenant).
  Goroutine baseline check after teardown.
- **Operator validation (post-merge, manual):** scaffold an agent via
  `harbor scaffold`, run `harbor dev`, send a sequence of messages,
  validate via the Console that runs show captured reasoning, memory
  injection appears in trajectory replays, repair guidance fires when the
  LLM returns malformed output. Pair against the external reference agent
  for parity.

## Smoke script additions

`scripts/smoke/phase-83f.sh` (static-only) asserts:

- `examples/harbor.yaml` carries the `planner.skills_context_max` comment block.
- `examples/harbor.yaml` carries the `planner.planning_hints` comment block.
- `cmd/harbor/cmd_dev_runloop.go` references `MemoryStore` and `SkillStore`
  (the dependency surface lands).
- `cmd/harbor/cmd_dev_runloop.go` references `RepairCounters{` (the per-run
  allocation lands).

## Coverage target

- `cmd/harbor`: 80% (the run-loop driver gets meaningful integration coverage).
- `internal/config`: 90% (already high; the new keys are simple validators).

## Dependencies

- Phase 83c (`*RepairCounters` and `*PlanningHints` on `RunContext`).
- Phase 83d (`*MemoryBlocks` and `SkillsContext` on `RunContext`).
- Phase 83e (`llm.CompleteResponse.Reasoning` capture + `TrajectoryStep.ReasoningTrace`).
- Phase 23 (`memory.MemoryStore` + `GetLLMContext`).
- Phase 37 (`skills.SkillStore` + `Search`).
- Phase 20 (`tasks.TaskRegistry.Get` + the `task.Query` field).

## Risks / open questions

- **External memory tier deferred.** V1.1 ships only `Conversation` from
  `GetLLMContext`; `External` stays nil. A future memory phase introduces
  long-term external memory; until then the `<read_only_external_memory>`
  wrapper is omitted entirely (the planner's render-on-nil contract).
- **PlanningHints surface is intentionally small.** V1.1 ships only
  `constraints` (free-form text) and `preferred_tools`. The richer
  `PlanningHints` shape (`ParallelGroups`, `DisallowTools`, `Budget`) stays
  on the Go struct (already shipped by 83c) but the YAML surface for those
  fields lands in a follow-up — operators who need them today set them via
  a custom planner Option, not via `harbor.yaml`.
- **Fetch-error policy is fail-loud.** A memory or skills driver outage
  fails the run rather than silently degrading. This may surprise operators
  used to systems where "missing memory" is non-fatal. Documented in the
  Wave 15 announcement.

## Glossary additions

None — the four primitives are already in the glossary from 83c/83d.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test in the integration suite asserts no
      cross-tenant memory or skills bleed at the fetch boundary
- [ ] N/A — 83f does not build a new reusable artifact (the existing
      `perTaskRunLoopDriver` gains new dependencies but no new
      compiled-artifact-with-mutable-state surface)
- [ ] Integration test exists per CLAUDE.md §17 —
      `test/integration/phase83f_runloop_consumers_test.go`
- [ ] No new vocabulary
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed
