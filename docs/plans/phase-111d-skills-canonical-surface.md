# Phase 111d — Skills canonical surface + ingestion

## Summary

The skills subsystem shipped deep and then production routed around it. Three
intertwined gaps (SDK friction audit §3):

- **(a) Two parallel implementations — the §13 smell, live.** The rich
  Phase-38 planner tools (`internal/skills/tools/tools.go:203` `Register` —
  capability default-deny filtering, tool-name redaction, the token budgeter)
  and the Phase-41 generator (`internal/skills/generator/generator.go:171`
  `Register`) are registered NOWHERE. Production registers a **thinner
  parallel implementation** instead: `internal/tools/builtin/skill_search.go`
  (+ `skill_get`), wired at `cmd/harbor/cmd_dev.go:655-661` via
  `builtin.RegisterWith`. The thin path has no capability filter, no
  redaction, no budgeter. Meanwhile `cmd/harbor/main.go:76-90` still promises
  "The Phase 60+ bootstrap will call `skills/tools.Register`" — a promise no
  phase kept.
- **(b) Skills.md ingestion has no shipped path.** The Phase-40 importer
  (`internal/skills/importer/importer.go:189` `New`) is exported but
  test-only — its only non-test consumers are devdraft's path-safety reuse.
  The operator SKILL.md documented fictional `harbor skill import` /
  `harbor skill rm` verbs
  (`docs/skills/configure-memory-and-skills/SKILL.md:94,114,138`) — live §18
  drift the in-flight Wave A chore (PR #278) excises.
  This phase ships the REAL verbs and restores the docs.
- **(c) The Phase-39 Directory** (`internal/skills/directory.go:172`
  `NewDirectory`) has only test consumers; the production run loop bypasses
  it with raw `store.Search` (`cmd/harbor/cmd_dev_runloop.go:545`). "A
  headless consumer cannot tell which retrieval surface Harbor stands
  behind" (audit §3).

Phase 111d's position — stated as the plan's recommendation, alternative
flagged: **unify on the Phase-38 handlers** (builtin delegates; duplicate
bodies deleted; capability filtering / redaction / budgeting re-homed onto
the production path), **ship `harbor skill import` + `harbor skill rm`** over
an exported `importer.ImportAndStore`, and **wire the Directory as the
`<skills_context>` provider** — the Directory question was an explicit owner
decision and is **RESOLVED: wire it** (owner, 2026-06-09; see Risks for the
recorded rationale; D-201 logs it in full at ship).

## RFC anchor

- RFC §6.7 — Skills subsystem (the settled planner-facing tools
  `skill_search` / `skill_get` / `skill_list` / `skill_propose(persist=true)`;
  the Skills.md importer pipeline; the virtual-directory pattern; the
  capability filter + redaction at injection time).
- RFC §8 — CLI layer (the new `harbor skill` verb family).

## Briefs informing this phase

- brief 04 — memory + skills (the rich tool surface, the importer pipeline,
  the virtual directory, capability filtering + redaction — every finding
  this phase re-homes onto production)

## Brief findings incorporated

- **brief 04 §4.5 (planner tools).** `skill_get` does "full content fetch
  with tiered downsizing … until the budget fits"; the applicability gate +
  disallowed-tool-name scrubbing run at injection. These behaviours EXIST in
  `skills/tools` and are absent from the thin builtin path production
  actually serves — the unification re-homes them where the LLM actually
  calls.
- **brief 04 §4.7 (Skills.md import).** "Per-skill manual adaptation is the
  real gap … closing this is a Harbor-defining feature." The importer closed
  it in code and then shipped no operator path to invoke it; a
  Harbor-defining feature that is unreachable is not shipped.
- **brief 04 §4.6 (virtual directory).** "The virtual-directory pattern is
  the right user-visible namespace abstraction even when the backing storage
  swaps." The pattern's designed consumer is exactly the cheap browse
  surface injected per-turn — which production currently fakes with raw
  ranked Search.
- **brief 04 §5 (draft-only generator is a half-feature).** Harbor inverted
  the draft-only generator by shipping persistence — and then never
  registered `skill_propose` anywhere. Half-shipped is the new draft-only.

## Findings I'm departing from (if any)

None — this phase is the act of STOPPING a silent departure: production
drifted off brief 04's designed surface (rich tools, importer, directory)
onto thinner parallels; 111d converges them back.

## Goals

- **(a) One canonical planner-tool surface.** `internal/tools/builtin`'s
  `skill_search` / `skill_get` become thin delegations to the Phase-38
  handlers (`skills/tools`'s search/get handler funcs, exported or wrapped
  for the purpose); the duplicate query/projection bodies in
  `internal/tools/builtin/skill_search.go` (+ sibling) are DELETED. The
  builtin registration seam (`builtin.RegisterWith`, the 107c shape — yaml
  enablement, `LoadingMode`, tags) is KEPT as the registration carrier; the
  Phase-38 package becomes the single implementation home. Net effect:
  capability default-deny filtering, tool-name redaction, and the token
  budgeter run on the production path for the first time.
  - `skill_list` (Phase 38's third tool) registers through the same carrier,
    default-enabled consistent with `skill_search`/`skill_get`.
  - `skill_propose` (Phase 41 generator) registers through the same carrier,
    **default-disabled** (`tools.builtin.skill_propose.enabled: true` opts
    in) — persistence-capable skill authoring is an operator decision, and
    D-054's audit-mandatory + conflict-policy machinery fires as shipped.
  - `cmd/harbor/main.go:76-90`'s stale "Phase 60+ bootstrap will call
    Register" comments are replaced with the truth.
- **(b) Skills.md ingestion ships.**
  - Exported `importer.ImportAndStore(ctx, id identity.Identity, store skills.SkillStore, deps Deps, path string, opts...) (ImportReport, error)`
    — composes the existing `Import` pipeline (frontmatter scan, validation,
    path-safe attachment resolution) with the store upsert + the settled
    conflict policy (refuse to overwrite `PackImport`-vs-`Generated` per RFC
    §6.7; duplicate-name rejection loud).
  - `harbor skill import <path>` + `harbor skill rm <name>` subcommands
    (new `cmd/harbor/cmd_skill.go`, bound in `root.go`): operate on the
    configured store from `harbor.yaml` (same resolution `harbor dev` uses);
    honest output (imported / skipped / rejected + reasons); exit non-zero
    on rejection; `--json` honours the global flag.
  - §18 same-PR rule: `docs/skills/configure-memory-and-skills/SKILL.md` is
    updated in the SAME implementation PR — the verbs the Wave A chore
    excised return as documentation of a real surface.
- **(c) Directory disposition — RESOLVED: wire it (owner decision,
  2026-06-09).** Make `Directory.View` the producer of
  the run loop's `<skills_context>` prompt block: pinned-then-recent,
  identity-scoped, capability-filtered, redacted — replacing the raw
  `store.Search` + `extractSkillKeywords` call at
  `cmd_dev_runloop.go:545`. Rationale for the recommendation, adopted by the owner with the
  D-176 manifest-pattern + KV-cache framing (a stable pinned-then-recent
  browse window mirrors the session-artifact manifest; a stable prompt
  prefix beats a per-turn query-churned block):
  - The Directory is purpose-built for exactly this consumer ("cheap
    browsing … the right user-visible namespace abstraction", brief 04
    §4.6); per-query RELEVANCE retrieval is now the LLM's job via the
    `skill_search` meta-tool (107c) — the prompt block's job is a bounded,
    stable namespace, which is what pinned-then-recent gives.
  - The raw-Search path bypasses the capability filter + redaction the
    Directory shares with Phase 38 via `capfilter` (D-052) — wiring the
    Directory closes a real injection-hygiene gap, not just an aesthetic
    one.
  - Operator-pinned skills (`DirectoryConfig.Pinned`) become functional for
    the first time.
  - The alternative — formally supersede the Directory (decisions entry +
    delete) on the grounds that 107c's meta-tools made browse-by-LLM cheap —
    is coherent but discards the pinning + injection-hygiene value and
    deletes shipped, conformant code to keep a keyword-extraction heuristic
    the audit separately flagged as triple-duplicated. The plan recommended
    AGAINST it; the owner concurred (resolved — see Risks).

## Non-goals

- No SkillStore schema or driver changes (Phase 37's LocalDB + FTS5 ladder
  consumed as shipped).
- No semantic skill retrieval — that is 84d's `Embedder` consumer; this
  phase converges the lexical surface 84d will later extend.
- No skill pack distribution / registry integration (post-V1 ecosystem
  work).
- No new generator capabilities — `skill_propose` registers as shipped
  (D-054 semantics untouched); `Promote` stays a Go-level API, not a
  planner tool.
- No prompt-shape redesign beyond swapping the `<skills_context>` producer
  (if the Directory decision lands "wire it"); section name + injection
  budget conventions are unchanged.

## SDK-consumer reachability

After 111d a headless consumer gets ONE answer to "how do I do skills in
Go": `importer.ImportAndStore` to ingest (same call the CLI verb makes —
the verb is a thin caller, never a second implementation),
`skills/tools.Register`-backed handlers for planner-facing retrieval (the
same handlers the binary registers — no production/SDK behaviour fork), and
`skills.NewDirectory(...).View` for the injection snapshot. The audit's "a
headless consumer cannot tell which retrieval surface Harbor stands behind"
finding is closed by there being exactly one surface. The recipe
`docs/recipes/use-memory-and-skills-from-go.md` (audit §7's named family)
ships in this phase with the ingest + retrieve + inject walkthrough.

## Acceptance criteria

- [x] **§13 two-implementations smell closed:** `tools/builtin`'s
      `skill_search`/`skill_get` delegate to the Phase-38 handlers; the
      duplicate implementation bodies are deleted; a test proves the
      production-registered tool applies capability filtering + redaction +
      the `skill_get` budgeter (the behaviours only the rich path has).
- [x] **§13 primitive-with-consumer:** `skills/tools.Register` (or its
      exported handler seam) + `generator.Register` gain their first
      production registration through the builtin carrier in this phase;
      `main.go`'s stale Phase-60+ promise comments are replaced.
- [x] `skill_list` registered (default-enabled); `skill_propose` registered
      default-DISABLED with yaml opt-in; D-054 conflict policy + audit emit
      asserted through the production registration path.
- [x] `importer.ImportAndStore` exported; conflict policy + duplicate-name
      rejection loud; path-safety preserved (`path_safety.go` — §7 rule 5).
- [x] `harbor skill import <path>` imports a spec-compliant Skills.md into
      the configured store; `harbor skill rm <name>` removes by name;
      non-zero exit + honest stderr on rejection; degradation path: older
      builds without the subcommand keep smoke green (§4.2 rule 8).
- [x] §18: `docs/skills/configure-memory-and-skills/SKILL.md` updated in the
      same PR — real verbs, real flags, real output shapes.
- [x] Directory disposition: **owner decision recorded BEFORE implementation
      starts** — RESOLVED "wire it" (owner, 2026-06-09; recorded in this
      plan + D-201 at ship). The runloop (+ devstack
      mirror) consumes `Directory.View`; pinned skills render; capability
      filter + redaction asserted on the injected block; the
      `extractSkillKeywords` + raw-Search path is deleted from the
      `<skills_context>` producer. If "supersede": the Directory is removed
      with a decisions entry and the glossary/master-plan rows annotated —
      either way, NO third state (the status quo is the one outcome this
      phase forbids).
- [x] E2E: `harbor skill import` a fixture Skills.md → boot → the LLM
      discovers it via `skill_search` (production registration) →
      `skill_get` returns budgeted, redacted content → (if wired) the
      Directory block lists it after use. Identity asserted throughout.
- [x] `scripts/smoke/phase-111d.sh` exercises the verb + the registration
      (see Smoke script additions).
- [x] D-201 (reserved; logged when the phase ships) records: the
      unification, the verb surface, and the Directory decision + who made
      it.

## Files added or changed

- `internal/tools/builtin/skill_search.go` + `skill_get.go` — bodies become
  delegations; duplicates deleted.
- `internal/tools/builtin/skill_list.go` + `skill_propose.go` — **NEW**
  registrations through the carrier (delegating to `skills/tools` +
  `skills/generator`).
- `internal/tools/builtin/builtin.go` — `RegistryContext` gains what the
  rich handlers need (bus for `Deps.Bus`; already has SkillStore).
- `internal/skills/tools/tools.go` — export the handler seam the builtin
  delegations call (smallest exported surface that avoids a second
  registration path; implementor-shaped).
- `internal/skills/importer/importandstore.go` — **NEW**
  `ImportAndStore` + `ImportReport`.
- `cmd/harbor/cmd_skill.go` — **NEW** `harbor skill import` / `rm`;
  `cmd/harbor/root.go` — bind.
- `cmd/harbor/main.go` — stale Phase-60+ comments replaced.
- `cmd/harbor/cmd_dev_runloop.go` (+ `harbortest/devstack/devstack.go`
  D-094 mirror) — Directory wiring (owner decision resolved: wire it).
- `internal/config/config.go` + `validate.go` — `skills.directory` block
  surfacing `DirectoryConfig` (pinned / max_entries / selection) if wired;
  `tools.builtin.skill_propose.enabled` + `skill_list` enablement.
- `examples/harbor.yaml` — new fields documented.
- `docs/skills/configure-memory-and-skills/SKILL.md` — §18 same-PR update.
- `docs/recipes/use-memory-and-skills-from-go.md` — **NEW** headless recipe.
- `docs/recipes/README.md` — index entry.
- `test/integration/phase111d_skills_surface_test.go` — the E2E.
- `scripts/smoke/phase-111d.sh` — real assertions.
- `docs/decisions.md` — D-201 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `importer.ImportAndStore(ctx, id, store, deps, path, opts...) (ImportReport, error)`
  and the `importer.ImportReport` type.
- The exported `skills/tools` handler seam the builtin carrier delegates to
  (one registration path, two carriers collapse to one implementation).
- CLI verbs: `harbor skill import <path>`, `harbor skill rm <name>`
  (operator-visible vocabulary; §18-documented).
- Config: `tools.builtin.skill_list` / `skill_propose` enablement;
  `skills.directory.{pinned,max_entries,selection}` (if the Directory
  decision lands "wire it").

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future
> facade/export RFC (audit §5 / Wave D), out of scope for this phase. The
> CLI verbs, by contrast, are externally reachable today — they are the
> operator-facing half of this phase.

## Test plan

- **Unit:** delegation parity (builtin-registered `skill_search` returns the
  rich handler's filtered/redacted shape — golden against a fixture store
  with a disallowed tool name in a skill body); `skill_get` budget tiering
  through the production path; `ImportAndStore` conflict matrix
  (PackImport-vs-Generated × duplicate names); CLI verb arg/exit-code
  behaviour; `skill_propose` default-disabled.
- **Integration:** `test/integration/phase111d_skills_surface_test.go` —
  real drivers (localdb SkillStore with FTS5, inmem bus, real catalog +
  builtin registration), the import→discover→get→inject E2E from the
  acceptance criteria; identity propagation (skill scoped to one tenant
  never surfaces in another's `skill_search` — §6 rule 10); ≥1 failure
  mode (import of a frontmatter-invalid file fails loud with the validator's
  message; rm of a missing name errors).
- **Conformance:** existing SkillStore conformance suite unchanged — runs
  green post-unification (no store-surface change).
- **Concurrency / leak:** N≥100 concurrent `skill_search`/`skill_get`
  invocations against one registered catalog under `-race` (the registered
  descriptors are compiled artifacts, D-025 — the rich handlers' existing
  guarantee re-proven through the new registration path); concurrent
  import-vs-search no-race.

## Smoke script additions

`scripts/smoke/phase-111d.sh`:

- Static: `internal/tools/builtin/skill_search.go` contains no duplicate
  query body (grep for the delegation call); `cmd/harbor/main.go` no longer
  contains "Phase 60+ bootstrap will call".
- `bin/harbor skill import scripts/smoke/fixtures/phase-111d.skill.md`
  against a temp data dir → exit 0 + "imported" in output; `bin/harbor
  skill rm <name>` → exit 0. Degradation: subcommand absent (pre-phase
  build) → SKIP (§4.2 rule 8).
- `go test ./internal/skills/... ./internal/tools/builtin/... -run
  'Skill|Import'` green.

## Coverage target

- `internal/skills/importer`: 90% (package's existing bar).
- `internal/skills/tools`: 85%.
- `internal/tools/builtin`: 85%.
- `cmd/harbor` verb paths: covered via smoke + integration (cmd is not
  unit-coverage-gated).

## Dependencies

- 37 (SkillStore + localdb), 38 (planner tools), 39 (Directory), 40
  (importer), 41 (generator), 107c (the builtin carrier + meta-tool
  enablement + `LoadingMode` shape this phase registers through).

## Risks / open questions

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111d has no 110-band dependency; all six
  111-band phases are mutually independent.
- **RESOLVED (owner, 2026-06-09) — Directory disposition: wire it** as the
  `<skills_context>` producer (rationale in Goals (c) — injection hygiene +
  pinning + the designed consumer + the D-176 manifest-pattern/KV-cache
  framing). The supersede and hygiene-only alternatives were presented and
  declined. The (c) work-stream is cleared to start; D-201 records the
  decision in full when the phase ships. Coordination consequence: Phase
  110b no longer promotes `extractSkillKeywords` as a permanent surface —
  see the 110b plan's adjusted scope (the helper is deleted by this phase's
  Directory wiring).
- **Prompt-behaviour delta if wired.** Swapping ranked-by-query Search for
  pinned-then-recent browse changes what the first prompt shows. Mitigation:
  the LLM retains full relevance retrieval via `skill_search` (107c), and
  the E2E pins the new block's shape; the implementor should eyeball one
  live run (the validation agent) before merging.
- **Capability-filter inputs on the production path.** The rich handlers'
  default-deny filter needs the allowed-tools/namespaces context; the
  builtin carrier must thread what the run actually grants
  (`tools.granted_scopes`, the 83m item-6 surface). If the granted-set
  plumbing is thinner than the filter expects, the implementor surfaces it
  (pause-and-ask) rather than passing allow-all — default-deny is the
  point.
- **CLI store resolution.** `harbor skill import` must open the SAME store
  the dev binary serves (driver + path from `harbor.yaml`); a mismatch
  silently imports into nowhere. The verb prints the resolved driver+DSN
  (redacted) so the operator can see where the skill landed.

## Deviations recorded at ship (§4.3)

Implementor deviations, all scope-preserving; each is also recorded in
D-201:

1. **`skill_propose` opt-in rides the existing `tools.built_in` names
   list — the sketched `tools.builtin.skill_propose.enabled` key was
   dropped.** A per-name `enabled` map would have been a SECOND
   enablement mechanism next to the 107c names-list carrier (§13
   two-parallel-implementations, applied to config shapes).
   "Default-disabled with yaml opt-in" holds: the tool registers only
   when explicitly listed, and it appears in no recommended set in
   `examples/` or the `harbor init` template.
2. **The stale registration promises no longer lived at
   `cmd/harbor/main.go:76-90`.** Phase 110c's aggregator rewrite had
   already replaced that block; the surviving stale text was the
   `internal/skills/tools` package doc ("Phase 60+ wires it") and the
   `internal/drivers/prod` HONESTY NOTEs — replaced there with the
   post-111d truth.
3. **The capability envelope's namespace/tag axes are empty
   (default-deny).** The Risks section's granted-set concern resolved
   on the deny side: Harbor has no runtime source of namespace/tag
   grants, so `AllowedNamespaces` / `AllowedTags` stay empty and
   skills requiring them are filtered. `AllowedTools` derives from
   `tools.VisibleNames` (the run's identity + `tools.granted_scopes`,
   both loading modes) — the one shared producer across the builtin
   delegations and the run-loop Directory call.
4. **Wiring sites adjusted for the 110d assembly reality.** The
   builtin registration deps (Bus / Redactor / GrantedScopes) thread
   through `internal/runtime/assemble` (the registration moved there
   in 110d); the Directory is constructed at the two run-loop driver
   sites (cmd + devstack) per D-197 call 4 (the driver shell is
   per-caller), via the shared `skills.DirectoryFromConfig`
   projection with the resolved `planner.skills_context_max` as the
   `max_entries` fallback.
5. **§17.6 fix bundled:** the rich `skill_get` returned
   `"skills": null` for an all-missing/all-filtered request and
   failed inproc output-schema validation when invoked through a
   catalog — a latent Phase-38 bug surfaced by the new delegation
   tests; fixed in `GetHandler` in the same PR.

## Glossary additions

- **Canonical skills surface** — post-111d there is one skills
  implementation path: Phase-38 handlers (filter/redact/budget) behind the
  107c builtin carrier, `importer.ImportAndStore` behind `harbor skill
  import`, and (decision-gated) `Directory.View` behind `<skills_context>`.
  Add to `docs/glossary.md`.
- **`harbor skill import` / `harbor skill rm`** — the CLI ingestion verbs
  over `importer.ImportAndStore` / `SkillStore.Delete`. Add to
  `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session/cross-tenant isolation test passes (skill visibility)
- [x] **Primitive + consumer in the same wave (§13):** the Phase-38/41
      Register surfaces gain their first production registration; the
      two-implementations smell is closed by deletion, not by a toggle —
      checked.
- [x] Concurrent-reuse test passes (registered tools, N≥100, `-race`)
- [x] Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`
- [x] §18: SKILL.md updated in the same PR
- [x] New CLI verbs have a smoke degradation path (§4.2 rule 8)
- [x] Directory owner decision recorded before (c) implementation (resolved
      in this plan 2026-06-09; logged in full in D-201 at ship)
- [x] Glossary updated
- [x] D-201 filed when the phase ships
