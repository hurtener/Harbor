# Phase 73h — Console Background Jobs page

## Summary

Wave 13 Stage 2.3 sub-phase. Ships the **Background Jobs** Console page — the
queue view for planner-spawned background tasks. The page is a focused
projection of the `tasks.list` Protocol method (introduced in Phase 73d) with a
`kinds=["background"]` filter and the queue-shaped affordances the design spec
(`docs/design/console/page-background-jobs.md`) §12 binds: faceted filter chips,
saved-filter chips, a per-job right-rail detail panel, bulk Cancel / Pause /
Resume / Prioritize toolbar, an `AwaitTask` orphan detector, and a per-job
Artifacts / Pending Approvals / Related Sessions / Logs sub-panel set. **This
phase introduces no new top-level Protocol methods**; it extends the
`tasks.list` filter shape Phase 73d ships, and consumes the shipped Phase 54
control verbs (`cancel` / `pause` / `resume` / `prioritize` / `approve` /
`reject`) for the bulk-action toolbar — no parallel control-plane (§13).

## RFC anchor

- RFC §5.2
- RFC §6.8
- RFC §7

## Briefs informing this phase

- brief 11

## Brief findings incorporated

- brief 11 §"Background Jobs view": "Long-running tasks that don't belong to a foreground session… Filters: status, type, identity, age. Per-row: job ID, type, status, started, ETA, # related sessions. Click → progress detail (artifacts produced so far, sub-task progress). Bulk actions: cancel, retry, requeue. Backing: Phase 20–21 + Phase 72. Phase 20's `TaskRegistry` already unifies foreground/background." The plan adopts this verbatim: the page is a `tasks.list` with `Kinds: ["background"]` projection, NOT a parallel `background_jobs.list` surface (§13 two-parallel-implementations rule).
- brief 11 §"Background Jobs" + §CC-4: faceted search over background jobs uses the Console-side palette dispatcher pattern from Phase 72c (Stage 1) — `tasks.list` carries the structured filters, free-text "search this queue" hits `search.tasks` (introduced in Phase 73d) on the same row shape.
- brief 11 §510 dependency table: `Background Jobs view → Task list filtered to background → 20, 21, 72`. The dependency on 72 is the subscription/scope foundation (Phase 72 + 72a); the dependency on 20/21 is shipped (TaskRegistry per-task + group surface). This phase adds 73d as the immediate Protocol-shape upstream and 75 as the Playwright harness consumer.

## Findings I'm departing from (if any)

None. The page spec §12 already pre-discharges every binding question:

- D-006 (background-task persistence in-process at V1) is explicitly surfaced in §8 of the spec as an Empty-state hint after restart — this plan ships exactly that copy and does not propose a durable backend.
- D-065 (no session-level priority) and D-072 (task-level priority surfaced via the shipped `prioritize` verb) are obeyed: the bulk-action toolbar's "Prioritize" targets `task_id` only; rows render a task-level priority badge, no session-level priority badges.
- D-061 (Console DB local-only) is obeyed: saved-filter chips, the `Stuck > 1h` derived chip, column visibility, and bulk-select state live in the Console DB; every row round-trips through `tasks.list`.
- The §13 "primitive-with-consumer" rule is satisfied trivially — the page IS the consumer of the `tasks.list` filter-shape extensions this phase ships. The Wave 13 decomposition (Stage 2.3) is the larger frame.

## Goals

- Ship the Background Jobs Console page route at `/console/background-jobs` exactly per `docs/design/console/page-background-jobs.md` §12.
- Extend the `tasks.list` Protocol filter shape (introduced in Phase 73d) with the four facets the page binds: `kinds=["background"]` filter, `status[]` multi-select, `has_pending_approval` boolean, `group_id=…` sibling query. The wire-type extension lives in `internal/protocol/types/tasks.go` (the file Phase 73d creates).
- Ship the `tasks.list` row-shape **enrichments** the page binds: a `progress` numeric hint (drawn from planner-emitted progress events — no new event topic), a `tags` slice (parent task type + planner-emitted labels), `parent_task_id` pointer (already in `Task` per RFC §6.8 — surfaced on the row), `last_activity_at` timestamp.
- Ship the **`AwaitTask` orphan detector** as a Console-side cross-check: a background job whose `parent_task_id` is no longer in the active-task set the same `tasks.list` snapshot returned is flagged with an orphan badge. The detector surfaces — at the UI — the §13 binding that `SpawnTask` + `AwaitTask` MUST emit in the same phase (Phase 47 / D-056 closed this for ReAct); the page is the observability surface for the property, not a re-implementation.
- Ship the per-job right-rail tabs: **Details**, **Progress**, **Logs**, **Pending approvals**, **Artifacts for this Job**, **Related Sessions** — each a projection of an already-shipped or 73d-extended Protocol surface (`tasks.get`, `events.subscribe`, `artifacts.list?task_id=…`, `tasks.list?group_id=…`).
- Ship the bulk-action toolbar that invokes the shipped Phase 54 control verbs (`cancel`, `pause`, `resume`, `prioritize`, `approve`, `reject`) one-per-selected-row. NO new control method, NO bulk endpoint. The toolbar is disabled-with-tooltip when the operator's identity scope lacks the `tasks.control` claim (D-066).
- Ship `web/console/tests/background-jobs-page.spec.ts` — the Playwright per-page spec that lands alongside this phase per the §17.7 wave-end suite obligation.
- Ship the `test/integration/background_jobs_page_test.go` integration test asserting the new `tasks.list` filter shape, real-TaskRegistry round-trip with both foreground + background tasks, identity propagation across tenants, the orphan-detector accuracy claim, and the scope-claim failure mode on a bulk control invocation.
- Append the new vocabulary (`Background Jobs page`, `AwaitTask orphan detector`, `tasks.list kinds=["background"] filter`) to `docs/glossary.md`.

## Non-goals

- **No new Protocol methods.** `tasks.list` is wave-13-extended in Phase 73d; this phase only extends its filter-shape + row-shape. `search.tasks` is wave-13-extended in Phase 72c; this phase consumes it for the page's free-text search box but does not define it.
- **No bulk control verb.** Phase 54's per-task `cancel` / `pause` / `resume` / `prioritize` are invoked once per selected row; a single-call bulk endpoint would be a parallel implementation (§13).
- **No durable background-job persistence.** D-006 dropped that to post-V1; the Empty-state copy after a runtime restart surfaces the limitation per the spec §8.
- **No session-level priority.** D-065 dropped it; row priority badges + the Prioritize toolbar action are task-level only (D-072).
- **No cross-runtime fan-in.** Multi-runtime aggregation is a `console:fleet` post-V1 scope (D-066); this page renders the connected runtime's queue only.
- **No new `notification.*` topic emissions.** Phase 72d ships the topic; this page consumes it (for the queue's "Recently failed" saved-filter chip's evidence ribbon) but does not emit it.
- **No new event topics.** Planner-emitted progress events are already on the bus (per spec §12); this page surfaces them via the `tasks.list` row-shape enrichment.
- **No Skeleton-component reinvention.** Filter chips, the bulk-action toolbar, the right-rail tabs, and the virtualised queue table use `@skeletonlabs/skeleton` primitives (CLAUDE.md §4.5 rule 4 + §13).

## Acceptance criteria

- [ ] `web/console/src/routes/background-jobs/+page.svelte` renders the Background Jobs page at `/console/background-jobs` exactly per `docs/design/console/page-background-jobs.md` §12 (sub-header filter strip + main queue table + bulk-action toolbar + right-rail detail panel + footer).
- [ ] `internal/protocol/types/tasks.go` (extended by Phase 73d) declares `TasksListFilter` carrying: **`Kinds []TaskKind`** (the canonical plural shape from 73d; setting to `["background"]` is the queue-mode binding; empty slice = all kinds), `Status []TaskStatus`, `HasPendingApproval *bool`, `GroupID *TaskGroupID`, `ParentTaskID *TaskID`, `IdentityFilter` (the existing identity-triple shape), `AgeMin` / `AgeMax time.Duration`, `Priority *int`, plus pagination (`Cursor`, `Limit`). The wire-key on the JSON-RPC envelope is `"kinds"` (plural array) — matches 73d's canonical shape.
- [ ] `internal/protocol/types/tasks.go` declares the `TasksListRow` enrichments: `Progress *float64` (nil when no planner-emitted progress), `Tags []string`, `ParentTaskID *TaskID` (mirrors `Task.ParentTaskID`), `LastActivityAt time.Time` (max of `UpdatedAt` and any event on the run's stream), `IsBackground bool` (mirrors `Task.Kind == "background"`).
- [ ] `tasks.list` with `Kinds: ["background"]` returns ONLY background tasks; cross-kind contamination is a test failure.
- [ ] `tasks.list?group_id=<gid>` returns sibling tasks (foreground + background) under the same `TaskGroup`; identity scope is still enforced.
- [ ] The Console-side orphan detector flags rows whose `ParentTaskID` is non-nil and absent from the same `tasks.list` snapshot. The detector is pure (no Protocol call); per-render it's `O(N)` with one hashed lookup per row.
- [ ] The bulk-action toolbar invokes the shipped Phase 54 control verbs per selected row; no new endpoint is added. Disabled-with-tooltip when the identity lacks `tasks.control` claim per D-066.
- [ ] `web/console/tests/background-jobs-page.spec.ts` covers: (a) page loads, queue renders, sub-header filter chips visible; (b) background-kind facet returns only background rows; (c) bulk-select + bulk Cancel invokes the verb per row + the rows transition; (d) right-rail tab navigation; (e) orphan badge renders for a planted orphan row; (f) bulk-toolbar is disabled when the operator's scope lacks `tasks.control`.
- [ ] `test/integration/background_jobs_page_test.go` runs under `-race`: real `TaskRegistry` with foreground + background + grouped + orphaned tasks; real Protocol transport (SSE+REST via `httptest.Server`); identity propagation across two tenants; orphan-detector returns the expected set; ≥1 failure mode (scope-claim mismatch on bulk Cancel returns `CodeScopeMismatch`); N≥10 concurrent `tasks.list` with `Kinds: ["background"]` subscribers — no goroutine leak after teardown.
- [ ] `scripts/smoke/phase-73h.sh` (PREFLIGHT_REQUIRES: live-server) probes (a) `tasks.list` with `Kinds: ["background"]` returns 200 against the live preflight runtime (or SKIPs on 404/405 until Phase 73d lands); (b) `tasks.list?group_id=…` returns 200 (or SKIPs); (c) a bulk control invocation against the live runtime without the `tasks.control` claim returns the shipped Phase 54 `CodeScopeMismatch` error code (or SKIPs on 404).
- [ ] `make drift-audit` passes — all required headings present; the cited RFC `§5.2` / `§6.8` / `§7` resolve; the cited `brief 11` resolves; the `scripts/smoke/phase-73h.sh` companion exists.
- [ ] `make preflight` passes locally (drift-audit + all phase smokes under live server).
- [ ] CLAUDE.md §13 naming rule respected — the drift-audit's forbidden-name scan returns no matches against the diff.
- [ ] `docs/glossary.md` gains entries for `Background Jobs page`, `AwaitTask orphan detector`, and `tasks.list kinds=["background"] filter`.
- [ ] `docs/plans/README.md` flips the row for Phase 73h to `Shipped` on merge.
- [ ] `README.md` Status table reflects the new page being shipped (one-line pointer in the Console section).

## Files added or changed

- `web/console/src/routes/background-jobs/+page.svelte` — page route shell (sub-header strip + main table + right rail + footer).
- `web/console/src/lib/pages/background-jobs/queue-table.svelte` — virtualised queue table component (Skeleton DataTable wrapper).
- `web/console/src/lib/pages/background-jobs/bulk-toolbar.svelte` — bulk-action toolbar invoking shipped Phase 54 verbs per row.
- `web/console/src/lib/pages/background-jobs/orphan-badge.svelte` — orphan-badge renderer + click-to-diagnostic-dialog.
- `web/console/src/lib/pages/background-jobs/right-rail.svelte` — Details / Progress / Logs / Pending approvals / Artifacts for this Job / Related Sessions tab strip.
- `web/console/src/lib/pages/background-jobs/saved-filter-chips.svelte` — Console-DB-backed saved-filter chips (`Active only`, `High-priority`, `Stuck > 1h`, `Recently failed`).
- `web/console/src/lib/pages/background-jobs/orphan-detector.ts` — pure Console-side detector (`detectOrphans(rows: TasksListRow[]): Set<TaskID>`).
- `web/console/src/lib/pages/background-jobs/orphan-detector.test.ts` — unit tests for the detector (planted orphans + happy path + idempotency under sort).
- `web/console/tests/background-jobs-page.spec.ts` — per-page Playwright spec.
- `internal/protocol/types/tasks.go` — **extended** (Phase 73d creates the file; this phase adds the filter / row-shape extensions listed in Acceptance).
- `internal/tasks/list_filter.go` — new — the runtime-side translator from the wire `TasksListFilter` into the `tasks.TaskFilter` the Phase 20 / 21 `TaskRegistry.List` consumes. Identity-scope enforcement at the entry point.
- `internal/tasks/list_filter_test.go` — unit tests + the concurrent-reuse test if this phase introduces a reusable artifact (note: the translator is a pure function; the artifact is `TaskRegistry` which Phase 20 already covered).
- `test/integration/background_jobs_page_test.go` — integration test (see Test plan).
- `scripts/smoke/phase-73h.sh` — new — live-server smoke probing the `tasks.list` filter shape + the bulk control scope-claim degradation.
- `docs/plans/phase-73h-console-background-jobs-page.md` — this file.
- `docs/glossary.md` — new entries (`Background Jobs page`, `AwaitTask orphan detector`, `tasks.list kinds=["background"] filter`).
- `docs/decisions.md` — append `D-114` (assigned in dispatch) documenting the orphan-detector-lives-on-the-Console-side choice + the bulk-toolbar-no-new-endpoint choice.
- `docs/plans/README.md` — flip the row for Phase 73h to `Shipped` on merge.
- `README.md` — Status-table row + one-line Console-section pointer to the new page.

## Public API surface

- `internal/protocol/types.TasksListFilter` — extended shape (Phase 73d created the field; this phase adds new fields). Other phases / Console / third-party clients depend on the JSON wire shape.
- `internal/protocol/types.TasksListRow` — extended shape. Wire-stable JSON contract.
- `internal/tasks.ListFilterFromWire(*types.TasksListFilter) (tasks.TaskFilter, error)` — translator function used by the Protocol handler. Pure; no per-run state.
- No new Console-side public Go API (the Console is TypeScript / SvelteKit; module path lives under `web/console/src/lib/pages/background-jobs/`).
- The Console-side `detectOrphans(rows): Set<TaskID>` is exported from `web/console/src/lib/pages/background-jobs/orphan-detector.ts` and re-exported via the page's barrel.

## Test plan

- **Unit:**
  - `internal/tasks/list_filter_test.go` — translator unit tests covering every field combination + identity-required failure mode + the `Kinds: ["background"]` / `Kinds: ["foreground"]` / empty-`Kinds` round-trip.
  - `web/console/src/lib/pages/background-jobs/orphan-detector.test.ts` — orphan detector unit tests: empty input → empty set; planted orphan with absent parent_task_id → flagged; child whose parent IS in the snapshot → not flagged; sort-invariance (running the detector twice with different row orders returns the same set).
  - `web/console/src/lib/pages/background-jobs/queue-table.test.ts` — Svelte component snapshot tests (renders all spec §12 columns; the Progress mini-bar handles nil; bulk-select state updates the toolbar disabled state).
- **Integration:** `test/integration/background_jobs_page_test.go` boots the assembled dev stack (`harbortest/devstack.Assemble` per D-094): real `TaskRegistry` with mixed foreground + background + grouped + planted-orphan tasks across two tenants; real Protocol SSE+REST transport via `httptest.Server`; one operator scope with `tasks.control`, one without. Asserts:
  - `tasks.list` with `Kinds: ["background"]` returns only background tasks; `tasks.list` with `Kinds: ["foreground"]` returns only foreground; empty `type` returns all kinds.
  - `tasks.list?group_id=<gid>` returns the siblings; cross-tenant `group_id` lookup is rejected with `CodeScopeMismatch`.
  - Identity propagation: a tenant-`B` operator listing tasks sees ZERO tenant-`A` rows (the canonical multi-isolation gate; CLAUDE.md §6 rule 2).
  - Orphan detector: a planted background task whose parent foreground task is `MarkCancelled`'d (orphaning it) is flagged after the next `tasks.list` snapshot; a non-orphan child is not flagged.
  - Bulk control failure mode: an operator without the `tasks.control` claim invoking `cancel` against a row returns `CodeScopeMismatch` (the shipped Phase 54 error code); no row transitions.
  - N=10 concurrent SSE subscribers to `tasks.list` with `Kinds: ["background"]` against a shared transport — no goroutine leak after teardown (baseline `runtime.NumGoroutine` restored within 50ms).
- **Conformance:** N/A — no new driver, no new persistence-shaped subsystem. The `TaskRegistry` conformance suite (Phase 20) already exercises every driver against `List` with arbitrary filters.
- **Concurrency / leak:** the runtime-side translator (`ListFilterFromWire`) is a pure function. The reusable-artifact path stays the `TaskRegistry` which Phase 20 already concurrent-reuse-tested (D-025). The integration test's N=10 concurrent-subscribers run + the `runtime.NumGoroutine` baseline-restoration assertion is the cross-package concurrency-stress proof.

## Smoke script additions

`scripts/smoke/phase-73h.sh` (PREFLIGHT_REQUIRES: live-server):

- Assertion 1: `tasks.list` with `Kinds: ["background"]` returns 200 against the preflight-booted dev server when authenticated with the dev token; SKIPs on 404/405 (Phase 73d not yet landed against this build).
- Assertion 2: `tasks.list?group_id=phase73h-smoke-nonexistent-group` returns 200 with an empty `rows[]` (no group → no siblings); SKIPs on 404/405.
- Assertion 3: A bulk control invocation (`cancel` against a synthetic `task_id`) without the `tasks.control` scope claim returns the shipped Phase 54 `CodeScopeMismatch` error (probes the degradation path the bulk toolbar relies on); SKIPs on 404/405.
- Assertion 4 (static): `web/console/tests/background-jobs-page.spec.ts` exists and matches the regex `test\(.*Background Jobs.*\)` to assert the Playwright spec is present (drift signal: a page phase without its spec).
- Assertion 5 (static): the new Console route file `web/console/src/routes/background-jobs/+page.svelte` exists.
- Assertion 6 (static): `web/console/src/lib/pages/background-jobs/orphan-detector.ts` exists and exports the pure function (regex grep).
- Assertion 7 (cross-phase smoke maintenance, §17.6): if Phase 73d's smoke probes `tasks.list` with `Kinds: ["foreground"]` and asserts the new filter shape, this script SKIPs that probe to avoid duplication; this script's `Kinds: ["background"]` probe is the new surface.

## Coverage target

- `internal/tasks` (the new `list_filter.go`): ≥ 85% (master-plan target for `tasks` package).
- `internal/protocol/types`: no coverage target (data types only).
- `web/console/src/lib/pages/background-jobs`: ≥ 80% (Console code coverage target, Wave 13).

## Dependencies

- Phase 73d (`tasks.list` shape extensions — Wave 13 Stage 2.2) — the upstream that introduces `tasks.list` as a Protocol method.
- Phase 75 (Playwright harness baseline — Wave 13 Stage 1) — the per-page spec consumer.
- Phase 20 (`TaskRegistry` per-task surface — Shipped).
- Phase 21 (`TaskGroup` + retain-turn + patches — Shipped).
- Phase 47 (parallel exec + ReAct emission upgrade — Shipped) — the `SpawnTask` / `AwaitTask` emission paths the orphan detector observes.
- Phase 54 (Protocol task control surface — Shipped) — the bulk-action toolbar's control verbs.
- Phase 60 (Protocol wire transport — SSE + REST — Shipped) — the wire the page rides.
- Phase 61 (Protocol auth — Shipped) — the JWT-claim source for the `tasks.control` scope.

## Risks / open questions

- **Orphan-detector latency.** The detector runs on every `tasks.list` snapshot; a snapshot with thousands of background tasks could surface a perceptible lag in the Console. Mitigation: the detector is `O(N)` with a single hashed set; we benchmark in the Console-side unit test against `N=10_000` rows and assert sub-50ms. If load exceeds that, a runtime-side `parent_alive` boolean on the row is the obvious lift (post-V1, no new Protocol method needed — a row-shape extension).
- **The `tasks.list?group_id=…` shape's behaviour on cross-tenant `group_id` lookup.** The decision: fail closed with `CodeScopeMismatch` (the same posture every multi-isolation gate takes). The integration test pins this.
- **Bulk-toolbar one-call-per-row latency.** Spec §12 explicitly accepts that the bulk toolbar invokes the per-task verb once per selected row; a single bulk endpoint would be a §13 parallel implementation. Operators bulk-cancelling N=100 background jobs see N=100 round-trips; SSE keeps the perceived latency low (each transition arrives as an event). If load profile shows this is unacceptable post-V1, a `tasks.control_batch` Protocol method is a scoped follow-up phase.
- **`tasks.list` with `Kinds: ["foreground"]`.** Phase 73d ships the `Kinds` filter field; this phase's smoke probes only `Kinds: ["background"]` because the page is the focused queue UI for background tasks. The general `Kinds: ["foreground"]` and empty-`Kinds` (no filter, all kinds) probes belong to Phase 73d's smoke script.
- **D-114 scope.** The assigned decision number documents the two binding architectural calls this phase makes: (1) the orphan detector lives Console-side (no new Protocol field), (2) the bulk-action toolbar invokes per-row Phase 54 verbs (no new bulk endpoint). Both are reaffirmations of §13 rather than new design; D-114 captures them so the choices don't get re-litigated at the Wave 13 checkpoint audit.

## Glossary additions

- `Background Jobs page` — Console route at `/console/background-jobs` ; a focused queue projection of `tasks.list` with `Kinds: ["background"]` with queue-shaped affordances.
- `AwaitTask orphan detector` — Console-side `O(N)` cross-check that flags a background task whose `parent_task_id` is absent from the same `tasks.list` snapshot. The detector surfaces, at the UI, the §13 binding that `SpawnTask` + `AwaitTask` MUST emit in the same phase (Phase 47 / D-056 closed this for ReAct).
- `tasks.list kinds=["background"] filter` — `TasksListFilter.Kind = "background"`; the queue-mode filter the Background Jobs page binds.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — `test/integration/background_jobs_page_test.go` asserts cross-tenant isolation on `tasks.list` with `Kinds: ["background"]` and `tasks.list?group_id=…`.
- [ ] **If this phase builds a reusable artifact:** N/A — `ListFilterFromWire` is a pure function (no goroutines, no shared state); the queue-table Svelte component is stateless w.r.t. the row data (per-render); the orphan detector is a pure function. The reusable artifact is `TaskRegistry` itself, which Phase 20 already concurrent-reuse-tested under D-025.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** Yes — Phase 20 / 21 (`TaskRegistry`) + Phase 47 (`SpawnTask` / `AwaitTask` emission) + Phase 54 (control verbs) + Phase 60 (Protocol wire) + Phase 61 (auth scope claims). The integration test wires real drivers across all of these, asserts identity propagation, covers ≥1 failure mode, runs under `-race`.
- [ ] If new vocabulary: glossary updated (three entries — `Background Jobs page`, `AwaitTask orphan detector`, `tasks.list kinds=["background"] filter`).
- [ ] If a brief finding was departed from: N/A — no departures; `D-114` documents the two binding architectural calls (orphan detector Console-side; bulk-toolbar per-row Phase 54 verbs) as reaffirmations of §13.
