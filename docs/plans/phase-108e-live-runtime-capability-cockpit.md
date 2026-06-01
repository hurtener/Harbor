# Phase 108e — Live Runtime → single-runtime capability-adaptive cockpit (reframe)

> Reframe of the Live Runtime page (108d shipped the topology-first build). The
> page stops being a topology canvas with trim and becomes the **single-runtime
> operations cockpit** — the drill-down from Overview (fleet) into ONE selected
> runtime's live operational state. Its composition is **driven by the
> runtime's advertised capabilities** (`runtime.info.capabilities`): the spine
> is what's true for any runtime shape; topology and future shapes are
> capability-gated panels, not the spine. This supersedes the topology-first
> composition (D-126) via a new decision **D-177**, and removes the
> Playground-overlapping free-floating steering composer (D-062). Operator
> decision (2026-06-01): **adopt the capability-adaptive cockpit in full.**

## Summary

108d brought the Live Runtime page to its mock with a topology graph as the
spine. In practice that inverts the runtime population: a planner/RunLoop
runtime (the common V1 shape — the dev posture, most scaffolded agents) has **no
engine graph**, so the page's hero is the honest D-164 "topology not available"
banner and the rest reads as a weaker Playground. This phase reframes the page
so the runtime's **advertised capability set composes it**: always-present
operational panels (posture · activity · interventions · live events · sessions ·
cost/governance · health) form the spine, and capability-gated panels (topology,
and future multi-agent / workflow / distributed views) light up only when the
selected runtime advertises them — **no page rebuild per new runtime shape**.
The conversational steering surface (free-floating composer) is removed; steering
a specific run is a drill into a session → Playground (D-062, "chat is one panel").

## RFC anchor

- RFC §7 (Console — the observability/control-plane product)
- RFC §7.1 (runtime-lens principle — every panel sources a Protocol surface)
- RFC §6.3 (steering + pause/resume — the intervention verbs)
- RFC §6.13 (event bus — the live event stream)

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §"Live Runtime view" (LR-1..LR-6):** Live Runtime is the operator's
  present-tense workbench for one running system — observe + steer what is
  happening *now*. This phase keeps that intent but stops conflating "the live
  run's topology" (one capability) with "the page" (the runtime cockpit).
- **brief 11 §PG-1..§PG-7 (chat/playground is ONE panel, not the page):** the
  conversational/test surface belongs to the Playground; on Live Runtime it is at
  most a drill-target, never the page's spine. This phase removes the
  free-floating Start/Redirect/Inject composer that duplicated Playground.
- **brief 11 §CC-1 (multi-runtime / runtime lens):** the operator works one
  runtime at a time; the cockpit is explicitly single-runtime with a switcher,
  the drill-down from the Overview fleet view.
- **brief 11 §CC-2 (identity-aware UI):** control-plane verbs render only with the
  elevated scope claim (D-066); the cockpit's "Needs attention" panel gates
  Approve/Reject/Resume on the claim and never fakes a local-only effect.

## Findings I'm departing from (if any)

- **D-126 / `page-live-runtime.md` §4 "the topology graph at default":** the
  topology-first composition is superseded. Topology becomes ONE
  capability-gated panel; the spine is capability-agnostic operational panels.
  Justification: the topology surface is absent on the dominant V1 runtime shape
  (planner/RunLoop → `unknown_method`, D-164), so a topology-first page is empty
  for most runtimes. Filed as **D-177** (supersedes the composition half of
  D-126; D-126's data-source map + "no Console shadow store" stays intact).
- **`page-live-runtime.md` §5 "Start composer" as a page element:** the
  free-floating conversational composer is removed from the cockpit (it
  duplicated Playground, D-062). Run-level steering is reached by drilling into a
  session. Justification: the operator judged it "crosses lines with Playground,
  which is miles better." Recorded under D-177.

## Goals

- Reframe Live Runtime as the **single-runtime operations cockpit** — the
  Overview→runtime drill-down, one runtime selected at a time (switcher).
- Make page composition a pure function of `runtime.info.capabilities`: a
  declarative **capability → panel registry**, so a new runtime shape adds a
  panel entry, never a page rewrite.
- Be **full and meaningful on a planner/RunLoop runtime** (no dead topology
  void): posture, activity, interventions, live events, sessions, and
  (capability-gated) cost/governance + health fill the viewport.
- **Remove the Playground overlap:** no free-floating steering composer; "open /
  steer a run" routes to Playground/Sessions.
- Hit the mock-grade spatial bar this time: **viewport-locked, no full-page
  scroll, components on a shared baseline grid, deliberate negative space** — and
  validate it in the EMPTY/info state (what dev runtimes render), not only
  populated (the 108d miss — see `feedback-console-page-polish-layout-fidelity`).
- Preserve every honest state + zero fabrication (capability-probe, not invent).

## Non-goals

- **No standing-up of a flow/engine runtime** to demo topology — the topology
  panel's rendering stays structurally verified (injected sample projection +
  the 108d adapter tests); live it shows the honest empty/gated state on
  planner runtimes.
- **No new Protocol methods invented here.** Where a panel needs a surface that
  isn't shipped (runtime-scoped `sessions.list`, `metrics.snapshot`,
  governance/llm posture), the panel capability-probes and renders an honest
  "not advertised" state. Landing those surfaces is their owning phases' work
  (72f/72g/73-cluster), consumed here as they arrive.
- **No conversational/chat module on this page** (D-062 / D-091 — chat's V1
  consumer is Playground).
- **No Console shadow store** for runtime entities (D-061 stays).
- Not a multi-runtime aggregate view — that is Overview (the cockpit is one
  runtime).

## Acceptance criteria

- [ ] Page composition is driven by a declarative capability→panel registry; the
      set of rendered panels is a pure function of the advertised capability set
      (unit-tested), with an always-present spine + capability-gated panels.
- [ ] On a planner/RunLoop runtime (no `topology_snapshot`): the cockpit renders
      a full viewport of meaningful panels (posture · activity · interventions ·
      live events · sessions · cost/health where advertised) — **no topology
      hero void**; the topology panel is absent or collapsed, not an empty banner
      occupying the hero.
- [ ] On a runtime advertising `topology_snapshot`: the topology panel renders
      (verified structurally via an injected sample projection — 108d adapter
      tests carry forward).
- [ ] The free-floating Start/Redirect/Inject/User-message composer is removed;
      run-level steering is a drill into a session (link to Playground/Sessions).
      No `$lib/chat/` import (proven by smoke).
- [ ] "Needs attention" (pauses / approvals / auth-required across the runtime)
      is a first-class panel with Approve/Reject/Resume, D-066 scope-gated
      (hidden without the claim; server re-checks).
- [ ] Four-state `PageState` holds at the PAGE level (Loading/Loaded/Error/
      Disconnected) AND per panel (nested), each forced + screenshotted; the
      empty/info state is validated SPATIALLY.
- [ ] Layout: viewport-locked (no full-page scroll; only inner regions scroll),
      shared baseline grid, full-bleed; matches the spatial discipline the 108d
      build missed. Screenshotted in ≥3 states.
- [ ] svelte-check `--fail-on-warnings` 0/0 · eslint clean · stylelint tokens-only
      · full Playwright e2e green (incl. `/live-runtime` hydrates w/o console
      errors) · `phase-108e.sh` OK ≥ assertions / FAIL 0 · prior smokes green.
- [ ] §8 per-panel ledger (panel · capability · source · verified-real? ·
      state-branches? · PASS/finding) + §9 read-only checkpoint audit FAIL-free.
- [ ] D-177 filed in `docs/decisions.md`; `page-live-runtime.md` updated to the
      cockpit framing; `docs/plans/README.md` row added.

## Files added or changed

```text
docs/plans/phase-108e-live-runtime-capability-cockpit.md   # this plan
docs/decisions.md                                          # + D-177 (supersedes D-126 composition)
docs/design/console/page-live-runtime.md                   # reframe: cockpit, capability→panel map
docs/plans/README.md                                       # + phase-108e row
scripts/smoke/phase-108e.sh                                # new smoke (static-only)
web/console/src/lib/live-runtime/
  panels.ts                                                # NEW — capability→panel registry + pure resolver
  tests/panels.test.ts                                     # NEW — resolver unit tests (planner vs engine vs future)
web/console/src/lib/components/live-runtime/
  runtime-posture-header.svelte                            # NEW — name/version/protocol/capability chips + switcher
  needs-attention-panel.svelte                             # NEW — runtime-wide pauses/approvals/auth (promote interventions)
  active-sessions-panel.svelte                             # NEW — live sessions on this runtime → drill rows
  cost-governance-panel.svelte                             # NEW — cost rate + ceilings + rate-limit posture (gated)
  health-panel.svelte                                      # health (rename/evolve health-tab-empty → real+honest)
  topology-panel.svelte                                    # topology-canvas wrapped as a capability-gated panel
  (reuse) event-stream-dock.svelte, status-counter-strip.svelte,
          interventions-panel.svelte, session-detail-card.svelte,
          recent-artifacts-panel.svelte, topology-canvas.svelte, detail-rail.svelte
  (remove) bottom-dock.svelte's composer arm / run-composer.svelte from THIS page
web/console/src/routes/(console)/live-runtime/+page.svelte # recomposed against panels.ts; spine + gated grid
web/console/tests/live-runtime-page.spec.ts                # rebuilt: cockpit states + capability matrix
```

## Capability → panel map (the spine of the reframe)

| Panel | Capability gate | Protocol source | Shape |
|---|---|---|---|
| Runtime posture header | always | `runtime.info` (84a) | spine |
| Activity counters | always | `tasks.list` strip / event-fold | spine |
| Needs attention (pauses/approvals/auth) | always | `pause.list` + `pause.requested`/`tool.approval_requested`/`tool.auth_required` events; resolve via approve/reject/resume (D-072, D-066) | spine |
| Live event stream | always | `GET /v1/events` SSE (RFC §6.13) | spine |
| Active sessions | `sessions_list` (else event-fold of `session.opened`) | `sessions.list`/`sessions.inspect` (73-cluster) | spine (degrades to event-derived) |
| Cost & governance posture | `governance_posture` / `llm.cost.recorded` | 72g posture + `llm.cost.recorded` aggregate | gated → honest "not advertised" |
| Health | `runtime_health` | `runtime.health` (72f) | gated → honest |
| Topology | `topology_snapshot` | `topology.snapshot` (74) | gated → absent on planner |
| (future) multi-agent / workflow / distributed | new capability key | future surfaces | gated, additive |

Reuse vs new: **reuse** the event dock, counter strip, interventions list,
session-detail card, recent-artifacts, topology-canvas + adapter, detail-rail,
and the capability-probe (`client.capabilities()`) the page already calls.
**New** is the small declarative `panels.ts` registry + the spine panels that
were previously rail cards / absent. **Removed** from this page: the run
composer arm.

## Public API surface

Console-only (no Go surface). The cross-cutting contract other Console work
depends on is the panel registry:

```ts
// web/console/src/lib/live-runtime/panels.ts
export interface CockpitPanel {
  id: string;                 // stable key (also a data-testid anchor)
  title: string;
  /** null = always present (spine); else the runtime.info capability that gates it. */
  capability: string | null;
  /** spine panels render even while a gated surface is absent. */
  spine: boolean;
}
/** Pure: the ordered panels to render for an advertised capability set. */
export function resolvePanels(capabilities: ReadonlySet<string>): CockpitPanel[];
```

## Test plan

- **Unit:** `panels.test.ts` — `resolvePanels` returns the correct ordered panel
  set for (a) planner/RunLoop caps (no topology/health/metrics), (b) an
  engine+posture runtime (topology+health+cost present), (c) a future/unknown
  capability (spine intact, unknown gated panel ignored — no crash). Carry
  forward the 108d `topology-adapter.test.ts` (15 cases) for the topology panel.
- **Integration:** rebuilt `live-runtime-page.spec.ts` Playwright — boots
  `harbor console`, seeds a connection, asserts the cockpit hydrates w/o console
  errors and the spine panels render on the (planner) fixture runtime; a
  `page.route()`-mocked capability set that advertises `topology_snapshot`
  asserts the topology panel appears (structural). Identity propagation +
  disconnected→/settings + scope-gated intervention verbs covered.
- **Conformance:** N/A — no new driver/interface.
- **Concurrency / leak:** N/A — Console page; no reusable Go artifact.

## Smoke script additions

`scripts/smoke/phase-108e.sh` (static-only) asserts:

- the page imports + uses `resolvePanels` from `panels.ts` (composition is
  capability-driven, not a hardcoded panel list);
- the spine panels are present (`needs-attention`, `active-sessions`,
  `runtime-posture-header`, event stream, counters);
- the topology panel is **capability-gated** (rendered behind a
  `topology_snapshot` check), not the page spine;
- the free-floating run composer is **removed** (no `run-composer` import on the
  page; no `$lib/chat/` import) — the Playground-overlap guard;
- `panels.ts` carries the capability keys (`topology_snapshot`, `runtime_health`,
  `governance_posture`) so the capability vocabulary stays guarded;
- honest no-fabrication copy retained for gated-absent panels ("does not
  advertise …").

(Smoke greps anchor on imports/usage/testids/exported symbols, never bare
comment strings — the 108d lesson; if a guarded string moves modules, the smoke
is updated in the same PR, §17.6.)

## Coverage target

Console-only — no Go coverage delta. Gate: svelte-check `--fail-on-warnings`
0/0 + eslint no-unused clean + stylelint tokens-only. New pure logic
(`panels.ts::resolvePanels`) is unit-covered by `panels.test.ts` (planner /
engine / future-capability cases). Behavioural coverage: rebuilt
`live-runtime-page.spec.ts` + the carried-forward `topology-adapter.test.ts`.

## Dependencies

- **108d** (shipped — the components/adapter/capability-probe this reframe
  recomposes).
- **84a** (shipped — `runtime.info` capabilities; the composition input).
- Consumed-as-available (honest-gated if absent): **72f** (runtime.health /
  metrics posture), **72g** (governance/llm posture), **73-cluster**
  (`sessions.list`/`sessions.inspect`, `tasks.get`, `artifacts.list`), **74**
  (`topology.snapshot`). None block this phase — each panel probes its
  capability and degrades honestly.

## Risks / open questions

- **Capability vocabulary stability.** The page keys off advertised capability
  strings (`topology_snapshot`, `runtime_health`, `governance_posture`, …); these
  must be a stable, documented set in `runtime.info`. Risk: drift between the
  Go-advertised keys and the Console's expected keys. Mitigation: the smoke
  guards the keys; a future generated capability enum (D-093-style) would close
  it. Open question: do we want a generated capability constant surface?
- **Runtime-scoped `sessions.list`.** The "Active sessions" spine panel ideally
  needs a runtime-scoped session list; until it ships it degrades to an
  event-fold of `session.opened`/`session.closed` (honest, partial). Flag in the
  ledger.
- **Overlap with Sessions page.** The Active-sessions panel must be a drill
  *index* (→ Sessions/Playground), not a re-implementation of the Sessions page
  (D-062 boundary). Keep it a thin list.
- **Scope creep into Overview.** The cockpit is ONE runtime; resist
  re-aggregating across runtimes (that's Overview). The switcher selects context;
  it does not fan in.

## Glossary additions

- **Runtime cockpit** — the single-runtime operations view; the Overview→runtime
  drill-down. Distinct from Overview (fleet) and Playground (one conversation).
- **Capability-adaptive page** — a Console page whose rendered panels are a pure
  function of the runtime's advertised `runtime.info` capabilities.
- **Capability panel registry** — the declarative `panels.ts` map of
  panel → gating capability the cockpit composes from.

(Added to `docs/glossary.md` in the same PR.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (panels.ts unit-covered)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (Console page; identity flows via the connection, asserted in the e2e)
- [ ] Reusable-artifact concurrent-reuse test — N/A: this phase builds no Go reusable artifact (Console page + pure TS resolver).
- [ ] Integration test exists (rebuilt `live-runtime-page.spec.ts`, real `harbor console`, identity propagation, disconnected + scope-gated failure modes) — see AGENTS.md §17.
- [ ] New vocabulary added to `docs/glossary.md`
- [ ] Brief departure (topology-first) justified above + **D-177** filed in `docs/decisions.md`; `page-live-runtime.md` reframed; skill drift checked (`observe-with-the-console` surface=console — update if the page's operator steps change, §18)
