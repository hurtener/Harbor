# Phase 110c — Config-projection exporters (`FromConfig` on the owning packages)

## Summary

Five subsystems (llm, memory, skills, planner, governance) deliberately decouple from
`internal/config` via snapshot/config types — but every config→snapshot **projection**
is an unexported `package main` helper, duplicated in devstack. That mechanism already
shipped one silent-field-drop bug (D-155: `cmd/harbor/cmd_dev.go` dropped
`CustomProviders`/`NetworkDefaults`/`Corrections` from the LLM snapshot) and carries a
second live one today (audit B3: devstack's planner projection at
`harbortest/devstack/devstack.go:1259-1264` maps only `Driver`/`MaxSteps`/`Extra`,
silently dropping `ExtraGuidance`/`ReasoningReplay`/`MaxToolExamplesPerTool`/
`ParallelToolCalls` — despite its own "MUST track production field-for-field" comment).
Phase 110c exports ONE projection per owning package (`llm.SnapshotFromConfig`,
`memory.SnapshotFromConfig`, `skills.SnapshotFromConfig`, `planner.ConfigFromOperator`,
`governance.ConfigFromOperator`), converts cmd + devstack to callers and deletes every
duplicate, exports `config.Defaults()`, re-homes the planner-adjacent knob projections
(`skills_context_max` default, `planner.HintsFromConfig`, the deduped spawn-depth
default), adds a headless validation profile (`ValidateCore`) so config-without-binary
stops demanding JWT identity fields, and ships the single production blank-import
aggregator (`internal/drivers/prod`) that cmd and devstack both import. Part of the
Wave B re-homing program (D-193); this phase's decision is **D-196 (logged —
shipped)**.

## RFC anchor

- RFC §6.5 — LLM client layer (the `ConfigSnapshot` whose projection D-155 broke once
  and this phase exports).
- RFC §6.6 — memory subsystem (the `memory.ConfigSnapshot` projection).
- RFC §6.7 — skills subsystem (the `skills.ConfigSnapshot` projection).
- RFC §9 — persistence triad (the driver registries the aggregator package seats; the
  §4.4 blank-import pattern being centralised).
- RFC §10 — stack decisions / configuration (the `internal/config` schema as the
  operator-facing leaf the subsystems now project FROM, additively).

## Briefs informing this phase

- brief 06 — events, observability, devx (no-toggle / one-shape lessons; the devx cost
  of hand-mirrored wiring).
- brief 03 — tools + integrations + LLM client (the provider-config surface whose
  projection drift D-155/B3 instantiate).

## Brief findings incorporated

- **brief 03 §5 "Two parallel LLM modes (the toggle smell)".** "Two modes shipping in
  parallel because one path didn't cover provider quirks — pick one architecture and
  bake the correction in." Two hand-maintained copies of the same projection are the
  config-layer version of the smell, and they have ALREADY diverged twice (D-155
  shipped, B3 live). One exported `FromConfig` per package is the bake-it-in fix.
- **brief 06 §5 "Logging in two formats with a flag is the wrong shape".** The brief's
  general lesson — don't ship two ways to express one configuration intent — applies to
  defaults too: today `defaults()` is loader-private (`internal/config/loader.go:165`),
  so a YAML-loaded config and a hand-built config get DIFFERENT baselines, and
  factories compensate inconsistently (events fails loud on zero values; sessions
  self-defaults). Exporting `config.Defaults()` gives both paths one baseline.
- **brief 06 §5 "Tightly coupled Playground" (the re-implementation symptom).** A dev
  surface that re-implements runtime concepts instead of consuming them is the symptom
  of missing seams. Devstack's five projection duplicates are exactly that symptom; the
  fix is seams (exported projections), not better mirroring discipline — D-094's
  "track field-for-field" comment was discipline, and B3 proves discipline loses.

## Findings I'm departing from (if any)

None. One §17.6 cross-fix landed beyond the plan's enumerated fields: the parity
gate surfaced a THIRD live field-drop in the absorbed `copyModelProfiles` helpers —
both copies silently dropped `LLMModelProfileConfig.CostOverrides` and
`.Corrections` (an operator's per-model `cost_overrides:` / `corrections:` yaml
validated cleanly and then did nothing). `llm.SnapshotFromConfig` maps both,
pinned by the sub-struct parity tests (D-196 call 2).

## Goals

- **One exported projection per owning package** (settled direction: the subsystem
  imports `internal/config` **additively** — config stays a leaf with no subsystem
  imports; the deliberate snapshot decoupling is preserved because `FromConfig` is one
  optional helper on the side, never a required path — `Open(ctx, snapshot, deps)`
  signatures are unchanged and snapshot-first construction remains the headless
  golden path):
  - `llm.SnapshotFromConfig(cfg config.LLMConfig, art config.ArtifactsConfig) ConfigSnapshot`
    — absorbs `cmd_dev.go:493-511` + the four private `copy*` helpers
    (`cmd_dev.go:1933-1982`: `copyCustomProviders`, `copyNetworkDefaults`,
    `disableCorrectionsFromConfig`, `copyModelProfiles`) and devstack's duplicates.
    This closes the **D-155 recurrence class**: the projection lives next to the
    snapshot type it fills, so a new snapshot field and its projection land in the
    same package (and the parity test below fails when they don't).
  - `memory.SnapshotFromConfig(cfg config.MemoryConfig) ConfigSnapshot` (absorbing
    `cmd_dev.go:538-552`'s inline build).
  - `skills.SnapshotFromConfig(cfg config.SkillsConfig) ConfigSnapshot` (absorbing
    `cmd_dev.go:580+`'s inline build).
  - `planner.ConfigFromOperator(cfg config.PlannerConfig) PlannerConfig` (absorbing
    `cmd_dev.go:1729`'s `plannerConfigFromConfig`) — **fixing live bug B3** by
    deleting devstack's four-field copy (`devstack.go:1259-1264`).
  - `governance.ConfigFromOperator(cfg config.GovernanceConfig) Config` (absorbing
    `cmd_dev.go:1895`'s `governanceConfigFromConfig` + `devstack.go:1765`'s
    `governanceConfigForDevstack`).
- **Field-parity gate:** a reflection-based parity test pins that
  `planner.ConfigFromOperator` maps **every** `config.PlannerConfig` field (the B3
  drift class, made mechanical); the same test shape covers the llm snapshot's
  config-sourced fields. A new field without a projection (or an explicit exclusion
  comment naming why) fails the build.
- **`config.Defaults()` exported** — rename the unexported `defaults()`
  (`internal/config/loader.go:165`); `Load` keeps calling it; hand-built configs start
  from the same documented baseline. Security-relevant fields stay intentionally
  absent (the existing fail-loud posture is unchanged).
- **Planner-adjacent knob projections re-homed** (audit config-duality finding #4 —
  `config.go:1082-1145`'s knobs that are "only consumed by the dev binary's per-task
  run loop"):
  - the `skills_context_max` zero→5 default becomes an exported resolver/const on the
    consuming package (`config.PlannerConfig.SkillsContextMaxResolved()` or
    equivalent), not a run-loop literal;
  - `planner.HintsFromConfig(cfg config.PlannerPlanningHintsCfg) PlanningHints` — the
    YAML→`PlanningHints` projection out of the run loop;
  - the spawn-depth default deduped: `config.go`'s constant is exported as
    `config.DefaultSpawnDepthCap` (the ONE source, referenced by `SpawnDepthCap()`);
    `dispatch`'s clamp (110a; formerly `cmd_dev_executor.go::defaultMaxSpawnDepth`)
    references it — wired by the coordinator at the Stage 1 merge, since 110a and
    110c built in parallel worktrees and 110c does not touch the executor region.
- **Headless validation profile:** `config.ValidateCore()` (or a scope parameter on
  `Validate`) runs every section validator EXCEPT the Protocol-server-only identity
  ceremony (`validate.go:33-36,124-133` — JWT algorithms/keys), so a library consumer
  validating a hand-built config is not forced to configure a JWT surface it never
  serves. `Validate()` keeps today's full-binary semantics unchanged; the profile is
  subtractive and documented (which sections it skips and why).
- **One production blank-import aggregator:** new package `internal/drivers/prod`
  whose doc-commented blank imports register everything `cmd/harbor/main.go:27-95`
  registers (artifacts/audit/distributed/events/llm-wrappers/governance/memory/skills/
  state/tasks/telemetry drivers). `main.go` and `harbortest/devstack` both reduce to
  `_ "github.com/hurtener/Harbor/internal/drivers/prod"` — closing the audit's §7
  llm-wrapper trap (devstack composes the LLM client WITHOUT corrections/downgrade/
  retry today because its hand-listed blank imports omit them; invisible under mock,
  real against live providers). AGENTS.md §3 (layout) + §4.4/§13 (the
  driver-blank-import rule) are amended in the same PR to name the aggregator as the
  sanctioned single home (mirror to CLAUDE.md).

## Non-goals

- **No assembly promotion** — the ordered config→stack fan-out is Phase 110d; this
  phase only makes each projection it needs exportable.
- **No schema change** — zero new YAML fields, zero validation-rule changes inside the
  sections `ValidateCore` keeps; example configs unchanged.
- **No governance enforcement wiring** (`SetFactory` consumers are the audit's Wave C);
  this phase only exports the posture projection that already exists.
- No snapshot-type reshaping: every `ConfigSnapshot` / `PlannerConfig` /
  `governance.Config` field set is unchanged.

## Acceptance criteria

- [x] The five `FromConfig`/`ConfigFromOperator` projections are exported on their
      owning packages with unit tests; `internal/config` gains no subsystem imports
      (leaf preserved — asserted in review).
- [x] **§13 consumer in the same phase + duplicates deleted:** `cmd/harbor` AND
      `harbortest/devstack` consume every exported projection; the cmd-local helpers
      (`plannerConfigFromConfig`, `governanceConfigFromConfig`, the four `copy*` LLM
      helpers, the inline memory/skills snapshot builds) and ALL devstack duplicates
      are deleted — grep-asserted in the smoke.
- [x] **B3 fixed and pinned:** the reflection parity test proves
      `planner.ConfigFromOperator` maps every `config.PlannerConfig` field; a
      devstack-assembled stack resolves `ExtraGuidance` / `ReasoningReplay` /
      `MaxToolExamplesPerTool` / `ParallelToolCalls` identically to production
      (integration-asserted).
- [x] `config.Defaults()` exported; `Load` behaviour unchanged (golden: loading the
      examples yields byte-identical configs pre/post); a hand-built
      `*config.Defaults()` passes `ValidateCore` after setting only the fields the
      docs name as required-for-core.
- [x] `config.ValidateCore` (or the scope parameter) exists; full `Validate` semantics
      unchanged (existing validate tests untouched); a JWT-less hand-built config
      passes core validation and still FAILS full validation (both asserted).
- [x] Planner-adjacent knobs: `planner.HintsFromConfig` exported; the
      `skills_context_max` default has one source; the spawn-depth default has one
      source referenced by config + dispatch (grep-asserted: no third literal).
- [x] `internal/drivers/prod` exists; `cmd/harbor/main.go`'s driver blank-import block
      collapses to the aggregator import; devstack's documented "Required blank
      imports" list is replaced by the aggregator — and a devstack-composed LLM client
      now seats corrections/downgrade/retry (integration-asserted via the wrapper
      chain's observable behaviour or hook registry); AGENTS.md/CLAUDE.md §3 + §4.4
      amended + mirrored.
- [x] All prior phase smokes + integration tests pass against the converted binary.

## Files added or changed

- `internal/llm/from_config.go` (+ `_test.go`) — `SnapshotFromConfig` + the absorbed
  copy helpers + parity test.
- `internal/memory/from_config.go` (+ `_test.go`) — `SnapshotFromConfig`.
- `internal/skills/from_config.go` (+ `_test.go`) — `SnapshotFromConfig`.
- `internal/planner/from_config.go` (+ `_test.go`) — `ConfigFromOperator`,
  `HintsFromConfig`, the reflection field-parity test.
- `internal/governance/from_config.go` (+ `_test.go`) — `ConfigFromOperator`.
- `internal/config/loader.go` — `defaults()` → exported `Defaults()`.
- `internal/config/validate.go` (+ tests) — `ValidateCore` profile.
- `internal/config/config.go` — knob-default single-sourcing (doc + resolver
  adjustments only; no schema change).
- `internal/drivers/prod/prod.go` — the aggregator (new `internal/` directory —
  AGENTS.md §3 updated in the same PR).
- `cmd/harbor/main.go` + `cmd/harbor/cmd_dev.go` — thin-caller conversion; local
  helpers deleted.
- `harbortest/devstack/devstack.go` — duplicates deleted; aggregator imported;
  projections consumed.
- `AGENTS.md` + `CLAUDE.md` — §3 layout entry + §4.4/§13 blank-import-home amendment
  (verbatim mirror).
- `scripts/smoke/phase-110c.sh` — assertions below.
- `docs/glossary.md` — "FromConfig projection", "production driver aggregator".
- `docs/decisions.md` — D-196 (authored at ship time).

## Public API surface

- `llm.SnapshotFromConfig(config.LLMConfig, config.ArtifactsConfig) ConfigSnapshot`
- `memory.SnapshotFromConfig(config.MemoryConfig) ConfigSnapshot`
- `skills.SnapshotFromConfig(config.SkillsConfig) ConfigSnapshot`
- `planner.ConfigFromOperator(config.PlannerConfig) PlannerConfig` +
  `planner.HintsFromConfig(config.PlannerPlanningHintsCfg) PlanningHints`
- `governance.ConfigFromOperator(config.GovernanceConfig) Config`
- `config.Defaults() *Config` + `config.ValidateCore() error` (profile)
- `internal/drivers/prod` — import-for-effect aggregator (no identifiers).

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers (cmd, harbortest,
> examples); external-team embedding needs a future facade/export RFC (the audit's
> Wave D), out of scope for this band.

### SDK-consumer reachability

A Go consumer with a `*config.Config` (loaded or hand-built) cannot today turn it into
subsystem snapshots without transcribing five unexported package-main helpers — and the
two existing transcriptions have both shipped silent field-drop bugs (D-155, B3). It
also cannot get loader defaults without the loader, cannot validate without configuring
JWT, and must hand-curate ~30 blank imports (and devstack proves even the in-repo copy
got that wrong — no corrections/retry on its LLM chain). After 110c the consumer writes
`config.Defaults()` → set fields → `ValidateCore()` → `<pkg>.SnapshotFromConfig(...)` →
`Open(...)`, with one aggregator import — the config-duality seam moves from "partial"
to "yes" on the audit's scorecard, and it is the substrate 110d's `Assemble` composes.

## Test plan

- **Unit:** per-projection golden tests (config in → snapshot out, including the D-155
  fields on llm and every B3 field on planner); the reflection field-parity gates;
  `Defaults()` golden vs the pre-rename loader behaviour; `ValidateCore`
  accepts-JWT-less / `Validate` rejects-JWT-less pair; knob-default single-source
  resolvers.
- **Integration:** devstack parity — assemble via devstack consuming the exported
  projections; assert the resolved planner config matches a production-shaped boot
  field-for-field (B3 regression gate); assert the aggregator-imported devstack LLM
  chain seats the production wrapper set; one failure mode: a config with an unknown
  driver name fails loudly through the projections (factory error names registered
  drivers). Identity propagation via the assembled stack's stores; `-race`.
- **Conformance:** N/A — projections are pure functions; driver registries already
  carry their own conformance suites (unchanged).
- **Concurrency / leak:** no compiled artifact is built (pure functions + an
  import-for-effect package) — concurrent-reuse N/A with that reason; projections run
  under `-race` in the parallel unit suite.

## Smoke script additions

`scripts/smoke/phase-110c.sh` (static-only): assert the five `from_config.go` files +
`internal/drivers/prod/prod.go` exist; grep-assert `cmd_dev.go` no longer defines
`plannerConfigFromConfig` / `governanceConfigFromConfig` / `copyCustomProviders` and
devstack no longer defines `governanceConfigForDevstack` or its planner projection;
grep-assert `main.go` imports `internal/drivers/prod`; run
`go test ./internal/config/ ./internal/planner/ ./internal/llm/ ./internal/memory/ ./internal/skills/ ./internal/governance/ -run 'FromConfig|FromOperator|Defaults|ValidateCore' -race -count=1`.
Skeleton ships with this plan (standard skip until the phase implements).

## Coverage target

- The new `from_config.go` files: 95% each (pure projection).
- `internal/config` (Defaults/ValidateCore additions): meets the package's existing
  target.

## Dependencies

- 83l (D-155 — the recurrence class this closes), 83f (D-149 — the planner-adjacent
  knobs), 107d/107e (D-169/D-170 — the planner fields + spawn-depth default in scope),
  36a/36b (the governance config shape), 02 (the config loader).
- **No dependency on 110a/110b/110d** — this phase runs in **Stage 1, in parallel with
  110a**.

## Risks / open questions

- **Merge coordination (staging).** Stage 1 = 110a ∥ 110c (independent; both touch
  `cmd_dev.go` + `devstack.go`, mechanical conflicts drained by the coordinator);
  Stage 2 (110b ∥ 110d) dispatches only after Stage 1 merges — 110d consumes every
  exported projection here.
- **Subsystem→config import direction is a one-way door.** Settled by the audit +
  coordinator: config remains the leaf; `FromConfig` is optional sugar, never a
  required path. The risk of snapshot/coupling erosion is fenced by keeping `Open`
  signatures snapshot-first and asserting config-leafness in review.
- **ValidateCore scope judgement.** Exactly which sections are "core" (state/llm/
  events/sessions/artifacts/tasks…) vs "binary-only" (server identity/JWT) needs one
  careful pass; the rule of thumb is "skips only what a headless embedder cannot
  meaningfully configure" — anything ambiguous stays in core (fail-closed bias).
- **Aggregator vs. §13's driver-import rule.** The aggregator concentrates — not
  widens — the blank-import privilege; the AGENTS.md amendment is part of the
  acceptance so the rule and the code can't drift.

## Glossary additions

- **FromConfig projection** — the exported per-subsystem helper
  (`llm.SnapshotFromConfig`, `planner.ConfigFromOperator`, …) that projects the
  operator's `internal/config` block onto the subsystem's decoupled snapshot/config
  type. One per owning package; cmd, devstack, and headless embedders all call the
  same one (closing the D-155/B3 silent-field-drop class). Add to `docs/glossary.md`.
- **Production driver aggregator** — `internal/drivers/prod`, the import-for-effect
  package whose blank imports register the full production driver + LLM-wrapper set;
  the single sanctioned home of driver blank imports (imported by `cmd/harbor`,
  devstack, and embedders). Add to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes (AGENTS.md/CLAUDE.md amended in this phase — verify)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: N/A — projections carry no identity; the
      integration test still asserts identity flows through the assembled stores.
- [x] Concurrent-reuse test: N/A — no compiled artifact built (pure projections + an
      import-for-effect package); unit suite runs under `-race`.
- [x] **Integration test (§17):** devstack-parity + wrapper-chain test with real
      drivers, ≥1 failure mode, under `-race`.
- [x] Glossary updated (FromConfig projection, production driver aggregator)
- [x] If a brief finding was departed from: N/A — none departed.
