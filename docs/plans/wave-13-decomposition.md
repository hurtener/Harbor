# Wave 13 decomposition — Console subsystem (Protocol + UI)

**Status:** Draft for operator review (2026-05-18). **Author:** coordinator. **Branch:** `docs/wave-13-decomposition`.

This doc proposes the Wave 13 phase decomposition for the Harbor Console subsystem. It re-decomposes the existing `Pending` Console phases (72 / 73 / 74 / 75) into letter-suffixed sub-phases that align with:

- The 14-page IA (Overview, Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background Jobs, Flows, Memory, MCP Connections, Artifacts, Settings, Playground).
- The 13 canonical mockups landed in PR #138 (`docs/rfc/assets/console-<slug>-page.png`) plus the pre-existing `console-agents-page.png`.
- Each spec's `§12. Mockup-aligned refinements (2026-05-18)` — the binding source for `[wave-13-extends]` Protocol additions.
- The §13 **primitive-with-consumer rule**: every Protocol primitive ships with at least one same-wave consumer.
- The §17.7 wave-delivery cadence: scope → stage → parallel dispatch → drain → wave-end E2E → checkpoint audit.

**This doc is NOT a phase plan.** Phase plan files come in the next PR after operator signoff on the staging here.

---

## 1. Existing master-plan anchors (preserved)

The Console-wave row in `docs/plans/README.md` lists four `Pending` phases. Their goals are preserved verbatim where unchanged and amended where Wave 13 expands scope:

| # | Existing role | Wave 13 disposition |
|---|---|---|
| 72 | Console subscription protocol surface (`events.subscribe` scope) | **Re-affirmed as parent** + extended via 72a-72h sub-phases. |
| 73 | Console state inspection surface (`sessions.inspect`, `tasks.get`, `state.history`, `state.list_trajectories`, `state.load_planner_checkpoint`, `artifacts.list/get/get_ref/delete`) | **Re-affirmed as parent** + extended via 73a-73n sub-phases (one per page). |
| 74 | Console topology projection events (`topology.snapshot`) | **Unchanged** — already correctly scoped for Wave 13. |
| 75 | Console e2e Playwright (CI gate) | **Split into 75 (baseline harness, Stage 1) + 75a (wave-end suite, Stage 3).** |

---

## 2. Total surface to land

From PR #138's per-spec tag totals:

- **293 `[wave-13-extends]` markers** across the 14 page specs (combined §3 and §12 tables).
- **Deduplicated, this collapses to:**
  - ~14 NEW Protocol method clusters (one per page-shaped subsystem) covering `agents.*`, `tasks.*`, `sessions.*`, `tools.*`, `flows.*`, `mcp.*`, `artifacts.*`, `memory.*`, `state.*`, `runtime.*`, `metrics.*`, `governance.*`, `llm.*`, `search.*`.
  - 1 NEW Protocol method cluster for pause-list snapshot (`pause.list`).
  - 1 NEW event topic (`notification.*`).
  - 1 NEW wire-type extension (`IdentityScope.actor` / `requester` / `impersonating`).
  - 14 Console page UI implementations + the chat module (shared by Playground).
  - Console-local Console DB schema (saved views, saved filters, profile, runtime registry, auth profiles, PAT store, notifications routing, keybindings) — D-061 carve-out.

---

## 3. Letter-suffix scheme (binding)

Phase numbers use the precedent from 36a/36b/53a/64a: a letter suffix means "sub-phase under the named parent that is small, parallel-able, and shares the parent's RFC anchor." The Wave 13 letter-suffix scheme:

- **72a-72h** = foundation Protocol primitives (subscription extensions, scope claims, posture surfaces, search, pause-list, notification topic, Console DB shape). Each ships with at least one Stage-2 page-phase as consumer in the same wave (§13 primitive-with-consumer compliance).
- **73a-73n** = per-page Protocol additions + page UI bundled into one phase per page. The §13 rule is satisfied trivially: the page IS the consumer of the primitive that lands in the same phase.
- **75a** = wave-end Playwright suite (consumes every page).

The 36-letter Phase 75 ceiling is comfortable; no renumbering of downstream phases (76-82) is required.

---

## 4. Stage 1 — Foundation Protocol primitives + Console DB shape

**Goal**: land the cross-cutting primitives every Console page depends on. Each primitive ships with a first consumer in the same phase (test or partial page surface).

| Phase | Surface | First consumer | Deps |
|---|---|---|---|
| **72** (existing Pending) | `events.subscribe` scope + cross-tenant claim (D-079) | Integration test: cross-tenant call rejected unless scoped admin. Scope-degradation regression suite. | 60, 05, 06 |
| **72a** | `events.subscribe` filter extensions (event-type / tenant / session / run / time-window) + `events.aggregate` (time-bucketed counts for sparklines) | Test consumer: filter-shape conformance. Page partial: Events page event-rate sparkline (extracted into 73g). | 72 |
| **72b** | `IdentityScope` extension for admin impersonation (`actor` / `requester` / `impersonating` on `types.StartRequest` and `types.IdentityScope`) per Brief 11 §PG-5 | Test consumer: impersonation round-trip with audit event emission. Page partial: Sessions page identity column (extracted into 73c). | 60, 61 (Protocol auth) |
| **72c** | `search.query` (Console-side palette dispatcher) + `search.sessions` + `search.tasks` + `search.events` + `search.artifacts` (runtime-side, high-cardinality per Brief 11 §CC-4) | Test consumer: query-shape conformance per index. First page consumer: Sessions/Tasks/Events/Artifacts search boxes (extracted into respective Stage-2 phases). | 60, 73 (state inspection) |
| **72d** | `notification.*` event topic per Brief 11 §CC-3 (NEW event family; rules-engine-lite mapper from event taxonomy to notification class) | First page consumer: Overview page alert ribbon (Stage 2: 73a). Settings notifications-routing matrix (Stage 2: 73m). | 05, 06 |
| **72e** | `pause.list` snapshot (paused tasks/sessions filtered by identity scope; consumes the shipped pause-coordinator state) | First page consumer: Overview page intervention queue (Stage 2: 73a). | 50 (pause primitive), 73 |
| **72f** | Runtime posture surface: `runtime.info` / `runtime.health` / `runtime.counters` / `runtime.drivers` / `metrics.snapshot` | First page consumer: Overview counter cards + Settings Runtime Info card (Stages 2: 73a, 73m). | 60 |
| **72g** | `governance.posture` (read-only `IdentityTiers` view per D-081) + `llm.posture` (provider/model/region/mock-flag per D-089) | First page consumer: Settings Governance Posture + LLM-Provider Posture cards (Stage 2: 73m). | 36a, 36b, 89 |
| **72h** | Console DB local schema (per D-061): `saved_filters`, `saved_views`, `profiles`, `runtime_registry`, `auth_profiles`, `pat_store`, `notifications_routing`, `keybindings` | First consumer: every Stage-2 page that has a saved-views chip or facet (smoke-tested with the Settings page first). | 60 (Protocol auth for PAT) |
| **74** (existing Pending) | `topology.snapshot` Protocol method + events on engine construction / edge change (static graph + live queue depth) | First page consumer: Live Runtime page topology canvas (Stage 2: 73b) + Playground trace toggle (Stage 2: 73n). | 05, 09 |
| **75** (existing Pending, narrowed) | Playwright harness baseline — `web/console/tests/*.spec.ts` skeleton + CI hook; per-page specs land alongside their Stage-2 phase | First consumer: 73a's Overview Playwright spec lands in 73a (not deferred). | 64, 72, 73 |

**Stage 1 size: 10 phases.** All parallel-able (no inter-deps among 72a–72h; 74 and 75 are independent). Suggested dispatch: 2 worktree agents per phase = 10 agents in one batch, or split into two batches of 5 if 10 parallel feels too wide.

---

## 5. Stage 2 — Per-page Protocol + UI bundled (one phase per page)

**Goal**: ship each Console page as a single phase that bundles (a) any remaining per-page `[wave-13-extends]` Protocol additions, (b) the page UI, (c) its Playwright spec, (d) any per-page audit events. This shape satisfies §13 trivially: the page IS the consumer of the primitive that lands in the same phase.

Letter-suffix maps page slug → 73-letter:

| Phase | Page | Net-new Protocol additions in this phase | Stage-1 deps |
|---|---|---|---|
| **73a** | Overview | Aggregates only (no new methods — composes `runtime.counters` + `pause.list` + `notification.*` + `tasks.list` + `events.subscribe`) | 72d, 72e, 72f, 73 (tasks.list), 75 |
| **73b** | Live Runtime | `tasks.list` status-counter strip extensions; `events.subscribe` trace-tab filter | 72a, 74, 75 |
| **73c** | Sessions | `sessions.list` + `sessions.list` filter extensions (saved filters, identity, tenant, status, time-range); `sessions.inspect` refinements | 72a (events filter), 72b (identity), 72c (search.sessions), 73 (sessions.inspect), 75 |
| **73d** | Tasks (kanban + bulk control) | `tasks.list` row-shape extension (priority, status, type filters), `tasks.get` enrichments; bulk-action toolbar consumes shipped `cancel/pause/resume/prioritize/approve/reject` | 72a, 72c (search.tasks), 73 (tasks.get), 75 |
| **73e** | Agents | `agents.list` + `agents.get` + `agents.tools` + `agents.memory` + `agents.governance` + `agents.skills` + `agents.permissions` + `agents.metrics` (8 methods — heaviest single page) | 72a, 72c (search.agents — Console-side), 75 |
| **73f** | Tools | `tools.list` + `tools.get` + `tools.describe` + `tools.metrics` + `tools.content_stats`; admin methods `tools.set_approval_policy`, `tools.revoke_oauth` (gated by `tools.admin` scope claim) | 72a, 75 |
| **73g** | Events page extensions | `events.aggregate` consumer (sparkline); `events.subscribe` saved-filter chips; truncated-payload `artifacts.get` link | 72a (events.aggregate), 73 (artifacts.get), 75 |
| **73h** | Background Jobs | `tasks.list` filter `type=background`; `tasks.list?group_id=…` sibling query; orphan-detector cross-check; `tasks.list` planner-progress enrichment | 73d (tasks.list shape), 75 |
| **73i** | Flows | `flows.list` (with aggregate metrics) + `flows.describe` + `flows.runs.list` + `flows.runs.describe` + `flows.run` + `flows.metrics`; per-flow `Budget` read (D-023) | 73 (state.history for per-run detail), 75 |
| **73j** | Memory | `memory.list` + `memory.get` + `memory.health`; consumers for `memory.identity_rejected` (D-033) + `memory.overflow_drop_oldest` (D-035) events | 72a, 75 |
| **73k** | MCP Connections | `mcp.servers.list` + `get` + `resources` + `prompts` + `refresh_discovery` + `probe` + `health` + `bindings.list` + `policy`; admin methods `refresh_binding`, `revoke_binding`; `mcp.raw_html_trust_toggled` audit event | 72a, 75 |
| **73l** | Artifacts | `artifacts.list` filter extensions (mime, source, size, task_id); `artifacts.put` (Brief 11 §PG-2 upload pipeline); `PresignGet` resolver; canonical renderer registry first consumer | 73 (artifacts.list / get / get_ref / delete shipped via existing Phase 73), 75 |
| **73m** | Settings | Composes `runtime.info` + `governance.posture` + `llm.posture` + Console DB local profile / runtime registry / auth profiles / PAT store / notifications routing / keybindings. Includes `auth.rotate_token` admin method. | 72f, 72g, 72h, 75 |
| **73n** | Playground | `runs.set_overrides` (Brief 11 §PG-5); chat module first consumer (`web/console/src/lib/chat/` per D-091); trace toggle via topology.snapshot | 74, 73 (artifacts), 75 |

**Stage 2 size: 14 phases.** Not all are independent — see §6 dependency graph below.

---

## 6. Dependency-aware staging within Stage 2

The 14 Stage-2 phases break into three sub-stages by Protocol-surface dependency:

### Stage 2.1 — Self-contained pages (parallel, 5 agents)

These pages depend only on Stage 1 primitives:

- **73f Tools** — needs 72a only.
- **73i Flows** — needs existing Phase 73 (state.history shipped). No same-wave dependency.
- **73j Memory** — needs 72a (events filter) only.
- **73k MCP Connections** — needs 72a only.
- **73l Artifacts** — needs existing Phase 73 (artifacts.list shipped) only.

### Stage 2.2 — Pages depending on Stage 2.1 catalog methods (parallel, 5 agents)

These pages compose Stage 1 + Stage 2.1 surfaces:

- **73c Sessions** — needs 72a/72b/72c + 73f's tools-filter facet for the Sessions tools-column drill-down.
- **73d Tasks** — needs 72a/72c + 73f (tools-filter facet) for Task → Tool drill-down.
- **73e Agents** — needs 72a/72c + 73f/73i/73j (Agent → Tools / Flows / Memory drill-downs).
- **73g Events** — needs 72a/72c (search.events).
- **73b Live Runtime** — needs 74 + 72a + 73d (tasks.list status-counter strip).

### Stage 2.3 — Pages depending on Stage 2.2 (parallel, 4 agents)

- **73a Overview** — depends on 72d/72e/72f + 73d (tasks.list) + 73b (Live Runtime intent for "Open Live Runtime" deep-links).
- **73h Background Jobs** — depends on 73d (tasks.list shape extensions).
- **73m Settings** — depends on 72f/72g/72h. Independent of other Stage 2 pages.
- **73n Playground** — depends on 74 + chat module first consumer (no prior Stage 2 consumer exists; this IS the chat module's first consumer).

Suggested ordering: 2.1 → 2.2 → 2.3 (sequential between sub-stages; parallel within).

---

## 7. Stage 3 — Wave-end suite + audit

- **75a — Console e2e wave-end Playwright suite.** Full IA navigation across all 14 pages; scope-claim degradation regression; identity isolation cross-page; saved-view persistence; notification routing end-to-end. **Bundles with the final Stage-2 phase's PR per §17.5.** Includes `test/integration/wave13_test.go` (Go-side wire-type round-trip + cross-page identity isolation + N≥10 concurrent SSE subscriber stress).
- **Wave 13 checkpoint audit (§17.5).** Read-only fork audits every Stage-1 + Stage-2 phase. Single `chore(checkpoint): wave-13 audit fixes` PR. **Gates Wave 14 planning.**

---

## 8. Full staging summary (chronological)

| Stage | Phases dispatched | Parallel agents | Gate to next stage |
|---|---|---|---|
| **1** | 72, 72a, 72b, 72c, 72d, 72e, 72f, 72g, 72h, 74, 75 | 11 (or two batches of 5–6) | All Stage-1 PRs merged. |
| **2.1** | 73f, 73i, 73j, 73k, 73l | 5 | All Stage-2.1 PRs merged. |
| **2.2** | 73b, 73c, 73d, 73e, 73g | 5 | All Stage-2.2 PRs merged. |
| **2.3** | 73a, 73h, 73m, 73n | 4 | All Stage-2.3 PRs merged. |
| **3** | 75a (bundles into final 2.3 PR) + checkpoint audit (single PR) | 1 + 1 | Wave 13 closed; Wave 14 unblocked. |

**Total: 30 phases in Wave 13.** Largest wave in the project so far (Wave 11 was ~8 phases). Justified: the Console subsystem is intentionally a full layer.

---

## 9. Risks + open questions for operator review

1. **Stage 1 width (11 parallel agents).** Comfortable, or split into two batches of 5–6? The §17.7 §3 dispatch rule doesn't cap parallelism, but agent-API-overload risk scales with width.
2. **Heaviest single phase: 73e Agents.** 8 Protocol methods + the page UI in one phase. Two options: (a) keep as one phase per the §13 primitive-with-consumer rule, or (b) split into 73e.p (Protocol) + 73e.u (UI) — but that re-introduces the split-primitive shape §13 explicitly forbids. Recommendation: **keep as one heavy phase**, allocate 2x agent time.
3. **`search.*` cluster** (72c). Five methods touching five subsystems. Could split into 72c.1 (palette + search.events) and 72c.2 (search.sessions/tasks/artifacts). Recommendation: **keep as one phase** — the methods share the same conformance surface, and splitting would create two phases with no clear ownership.
4. **`notification.*` event topic** (72d). First consumer in same wave is 73a Overview's alert ribbon. Acceptable, but the §13 rule reads naturally as "primitive's first consumer is a runtime call site that emits it, not a UI page that renders it." Recommendation: **add a Stage-1 test consumer** that fires `notification.task_failed` from a deliberate `tool.failed` and asserts a subscriber receives it via the bus, before the UI ships.
5. **Console DB schema (72h).** Lands as Stage 1 because every Stage-2 page needs at least the saved-views table. First UI consumer is 73f Tools (saved-filter chips). Acceptable.
6. **Playwright harness (75).** Currently `Deps: 64, 72, 73`. Wave 13 narrows it: 75's baseline-only role lands in Stage 1 with deps 60, 72; per-page specs land alongside each 73x. The wave-end aggregator suite is 75a in Stage 3.
7. **Single PR for the decomposition vs many small PRs.** This planning doc lands as one `docs(plans)` PR. Per-phase plan files land as a SECOND `docs(plans)` PR (one commit per phase plan file for reviewability). Implementation PRs follow §17.7 per-stage.
8. **`harbor console` subcommand phase.** D-091 says the Console is served via `harbor console`, not `harbor dev`. Where does the subcommand itself land? Recommendation: **bundle into 73m Settings** (Settings is the first page that needs `harbor console` to be running for the Connected-Runtimes card to mean anything). Alternative: **standalone 72i** as a Stage-1 phase.

---

## 10. Acceptance for this doc

- [ ] Operator reviews §3 letter-suffix scheme; signs off or proposes alternate.
- [ ] Operator reviews §4 Stage 1 phase list; flags any missing primitive.
- [ ] Operator reviews §5/§6 Stage 2 staging; signs off on the 2.1 → 2.2 → 2.3 sub-stage shape.
- [ ] Operator answers §9 open questions 1, 2, 3, 4, 8 (the others are recommendations).
- [ ] Operator confirms the decomposition doc → per-phase-files → implementation cadence (two `docs(plans)` PRs before any implementation dispatch).

Once signed off: this doc gets committed as-is to `docs/plans/wave-13-decomposition.md` and the per-phase plan files are authored in a second `docs(plans)` PR (one commit per phase plan file for reviewability), each following the §16 phase-plan ritual.

---

## 11. References

- `RFC-001-Harbor.md` §5 (Protocol), §6 (Runtime subsystems), §7 (Console).
- `docs/plans/README.md` — existing Console-wave phases 72-75 and the pre-plan note (items 1-8).
- `docs/design/console/page-*.md` — 14 per-page specs with binding §12 mockup-aligned refinements.
- `docs/research/11-console-feature-surface.md` (Brief 11) — verbal decomposition.
- `docs/research/12-console-deployment-and-shared-ui.md` (Brief 12) — deployment + shared chat module.
- `docs/decisions.md` — D-014, D-022, D-026, D-029, D-033, D-035, D-061, D-062, D-063, D-064, D-065, D-066, D-072, D-077, D-079, D-081, D-083, D-089, D-091, D-092, D-093.
- `CLAUDE.md` §4.5 (Console / Protocol-client conventions), §13 (forbidden practices — frontend bullets + primitive-with-consumer + test-stubs-as-defaults), §17.7 (wave-delivery cadence).
- PR #138 (`docs/console-page-specs` merged at `d7ab563`) — the 14 specs + 14 canonical mockups feeding this decomposition.

---

## 12. Locked answers (2026-05-19) — operator signoff

Following operator review of §9 in PR #141 (merged at `4069955`), the eight open questions are resolved as below. **These answers are binding on all per-phase plans.** §9 above is preserved as the audit trail.

1. **Letter-suffix scheme (72a-72h / 73a-73n / 75a).** **Confirmed.**
2. **Stage 1 width (11 parallel agents).** **Split into two batches of 5-6.** Batch A: 72, 72a, 72b, 72c, 72d (5 — foundational primitives). Batch B: 72e, 72f, 72g, 72h, 74, 75 (6 — posture + Console DB + topology + Playwright). Operator merges Batch A's PRs before Batch B's agents dispatch.
3. **73e Agents heaviest single phase.** **Keep as one heavy phase, allocate 2x agent time + mandatory coordinator-verify pass.** The coordinator MUST NOT trust the agent's finishing signal; the coordinator reads the produced files, greps for shipped-vs-`[wave-13-extends]` mistakes, audits the §13 primitive-with-consumer compliance, and only THEN advances. This rule is binding on every Wave 13 dispatch (not just 73e), but 73e carries the highest audit cost given its width.
4. **`search.*` cluster shape (72c — 5 methods).** **Keep as one phase.** The methods share the same conformance surface (identity filtering, redaction, pagination, scope claim).
5. **`notification.*` first consumer (72d).** **Add a Stage-1 test consumer that fires `notification.task_failed` from a deliberate `tool.failed` and asserts a subscriber receives it via the bus — BEFORE the UI ships.** This makes 72d §13-compliant without depending on 73a Overview's alert ribbon to land. The test consumer lives in `internal/runtime/notifications/notifications_test.go` (or equivalent — Phase 72d picks the package shape).
6. **Console DB schema (72h).** **Accepted as Stage 1.** First UI consumer is 73f Tools (saved-filter chips); 72h ships the schema + a migration test + an integration smoke that 73f's saved-filter-chip handler can write/read a row through the Protocol client.
7. **Playwright harness (75).** **Accepted with operator amendment: 75's baseline-only role lands in Stage 1 with deps 60, 72; per-page Playwright specs land alongside each 73x in Stage 2; the wave-end aggregator suite is 75a in Stage 3.** **Binding addition from operator: 75a MUST cover every single generated page (all 14 pages), with full coordinator validation that no page is skipped.** The coordinator verifies this by enumerating page-spec files and asserting a matching `*.spec.ts` exists in `web/console/tests/` for each.
8. **Single PR for the decomposition vs many small PRs.** **Confirmed.** This decomposition doc landed as PR #141. Per-phase plan files land as a SECOND `docs(plans)` PR (this branch — `docs/wave-13-phase-plans`), one commit per phase plan file for reviewability. Implementation PRs follow §17.7 per-stage cadence after that second PR merges.
9. **`harbor console` subcommand phase placement.** **Bundle into 73m Settings** per the recommendation. The Settings page is the first page where the Connected-Runtimes card has visible meaning, and the subcommand is the runtime side of that card. Concrete acceptance criterion on 73m: `harbor console` subcommand exists, serves the SvelteKit build via `embed.FS` per D-091, and is gated behind the same Protocol-auth + identity-scope surface as every other Runtime call.

**Wire-shape decision left to me per the AskUserQuestion exhaustion:** the `notification.*` event family uses **per-class topic naming** (`notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`) — NOT a per-instance `notification.emit` with a payload class field. Rationale: per-class topic composes naturally with the existing event taxonomy (`tool.failed`, `task.completed`, `governance.budget_exceeded` all use per-class topics) and with the `events.subscribe` topic-filter shape. Operator may redirect in Phase 72d's plan review; I'll flag it explicitly in 72d's "Risks / open questions" section.

**Mandatory coordinator-verify protocol (binding on every Wave 13 implementation dispatch):**

For every agent PR before it goes to operator review:

1. **Quantitative-claim verification.** Re-count files, methods, tests, coverage % that the agent's PR description claims. Mismatches are a §13 rejection-on-sight signal.
2. **Citation grep-verification.** Every `[shipped]` Protocol-method citation in the phase plan or PR body greps back to its declaration in `internal/protocol/methods/`, `internal/*/events.go`, or `internal/protocol/types/`. Every `[wave-13-extends]` citation matches a line in the spec's §12.
3. **Code-side §13 read.** Read the actual code for: stub-as-default smell (godoc strings matching "test-grade", "canned responses", "deterministic for tests"), silent degradation (try/catch returning nil), identity-downgrading knobs, primitive without consumer, mutable artifact state.
4. **Source-material cross-check.** Open the page spec's §12 component table; verify each `[wave-13-extends]` row that the agent claims is now `[shipped]` has a matching Protocol-method addition in `internal/protocol/methods/methods.go` AND a matching `assertJsonPath` in `scripts/smoke/phase-NN.sh`.
5. **Only after all four pass: advance.** A FAIL on any of the four means: report the precise finding to the operator and either fix in-PR (coordinator side) or block the merge.
