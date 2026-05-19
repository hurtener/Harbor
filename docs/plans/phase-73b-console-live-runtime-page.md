# Phase 73b — Console Live Runtime page (Protocol + UI bundled)

## Summary

Phase 73b ships the **Live Runtime** Console page — the operator's
present-tense workbench for initiating, observing, and steering a live
execution — bundled as a single phase with the two remaining
`[wave-13-extends]` Protocol additions the page consumes: `tasks.list`
gains a status-counter strip projection (`pending/running/completed/paused/failed`
aggregates for the header chips) and `events.subscribe` gains a
trace-tab filter shape (correlate events to a topology node by run id
for the bottom-dock Trace tab). The page composes those additions with
shipped surfaces (Phase 54 task-control methods, Phase 60 wire
transport, Phase 61 auth, the canonical event taxonomy) plus the
already-decided Wave 13 primitives it inherits as deps: `events.subscribe`
filter shape (72a), `topology.snapshot` projection events (74), the
state-inspection cluster (73 — `sessions.inspect` / `tasks.get` /
`state.history` / `artifacts.list`), and the shared engine-graph canvas
(`web/console/src/lib/components/graph/` from 73i). It also lands
`web/console/tests/live-runtime-page.spec.ts` and a Go-side integration
test (`test/integration/live_runtime_page_test.go`) that drives the
real Protocol surfaces end-to-end.

## RFC anchor

- RFC §5.2
- RFC §6.3
- RFC §6.13
- RFC §7
- RFC §7.1

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Live Runtime view" / §LR-1: the topology graph is the
  centerpiece of the page; nodes carry name + type tag + status pill +
  latency + edge; click selects into the bottom-dock per-task pane.
  Phase 73b implements exactly that surface against the Phase 74
  `topology.snapshot` projection — no separate node store, no parallel
  layout state.
- brief 11 §LR-2 (Topology / Timeline / Metrics / Health tab strip):
  Topology + Timeline are sibling projections of the same
  `topology.snapshot` data; Phase 73b ships the Topology tab plus
  Timeline-tab swimlane affordances. Metrics + Health tabs are gated
  behind `metrics.snapshot` / `runtime.health` Wave 13 primitives that
  ship as separate phases (72f / 72g) — when those phases land, the
  tabs activate; until then they render an empty-state with a pointer
  to the responsible phase (per the 404/405/501 → SKIP analogue).
- brief 11 §LR-5 (Event Stream bottom dock): the page subscribes to
  `events.EventBus` via Phase 60 SSE filtered to the session's
  `(tenant, user, session)`; renders `task.*`, `tool.*`, `planner.*`,
  `pause.*`, `control.*`, `tool.auth_required` per the shipped event
  taxonomy.
- brief 11 §LR-6 (per-task detail pane Details / Input / Output /
  Logs): consumes `tasks.get` (Phase 73) for Details / Input / Output
  and `state.history` (Phase 73) for Logs; the bottom-dock Trace tab is
  the new mockup affordance and consumes `state.history` extended with
  span correlation per the §12 reconciliation.
- brief 11 §PG-7 (trace toggle): the trace overlay correlates events to
  topology nodes by run id; Phase 73b wires that as a local UI toggle
  using the `events.subscribe` trace-tab filter shape this phase lands
  (filter `events_subscribe.run` is already the shipped run-scoped
  carrier from Phase 60's `X-Harbor-Run` header per the D-082
  post-PR amendment).
- brief 12 §"The shared chat / playground library": Phase 73b is **NOT** a consumer of the canonical chat module. The chat module's V1 first consumer is 73n Playground; the second consumer is the post-V1 packed dev UI in `harbor dev` per CLAUDE.md §4.5 #11 (the "encapsulate first, extract on second consumer" rule). A second in-V1 consumer would trigger extraction to `web/shared/chat/` in the same wave, which is out of scope. Live Runtime's Start / Redirect / Inject-context / User-message composer at the bottom-right dock is rendered with **non-chat Skeleton primitives** (`Stack`, `Card`, `Textarea`, `Button`) calling the shipped Phase 54 control verbs through the typed Protocol client directly — no chat module dependency.
- brief 12 §"Why `harbor console`, not `harbor dev`, serves the
  Console": the page lives in the Console SvelteKit SPA and is served
  by `harbor console`; `harbor dev` stays headless. Phase 73b ships
  routes under `web/console/src/routes/console/live-runtime/`; no
  embedding into `harbor dev`.

## Findings I'm departing from (if any)

None.

## Goals

- Land the two remaining `[wave-13-extends]` Protocol additions the
  Live Runtime page consumes: a `tasks.list` status-counter strip
  projection (`pending/running/completed/paused/failed` counts scoped
  to a session id) and an `events.subscribe` filter shape extension
  that carries the run id as a first-class filter (for the bottom-dock
  Trace tab's per-topology-node correlation). Both additions are
  declared in `internal/protocol/singlesource.CanonicalWireTypes` so
  the generated TypeScript client picks them up; both go through the
  Phase 58 single-source checker.
- Implement the Live Runtime page as a SvelteKit route at
  `web/console/src/routes/console/live-runtime/+page.svelte` plus the
  per-component subtree under
  `web/console/src/lib/components/live-runtime/`. The page composes
  the shared engine-graph canvas (`web/console/src/lib/components/graph/`
  from 73i), Skeleton primitives (`Stack`, `Card`, `Textarea`,
  `Button`), and the typed Protocol client
  (`web/console/src/lib/protocol.ts` — generated per D-093). The page
  does NOT import the canonical chat module from `web/console/src/lib/chat/`
  per D-091 + CLAUDE.md §4.5 #11 (chat module's V1 first consumer is
  73n Playground; a second in-V1 consumer is forbidden).
- Wire every panel of the page to its Protocol surface per the
  `docs/design/console/page-live-runtime.md` §3 + §12 component
  matrix — topology canvas, status legend, tab strip, Event Stream
  dock, per-task detail pane (Details / Input / Output / Logs / Trace),
  session detail header card, Current Step sub-panel, Recent Artifacts
  sub-panel, Interventions sub-panel, composer (Start / Redirect /
  Inject / User Message / Cancel / Pause / Resume), header status
  counter strip, footer (Protocol version / connection state /
  Console version).
- Land `web/console/tests/live-runtime-page.spec.ts` (Playwright)
  covering topology canvas rendering nodes + edges from
  `topology.snapshot`; `events.subscribe` trace-tab filter narrowing
  the event stream by run id; the header status counter strip
  updating on `task.*` events; and the multi-isolation negative case
  (a request without the session's identity triple is rejected at the
  edge per Phase 61 with `auth_rejected` / `identity_required`).
- Land `test/integration/live_runtime_page_test.go` — real runtime
  (in-mem drivers) wired through Phase 60 SSE + REST, real
  `tasks.TaskRegistry`, real `topology.snapshot` projection (74),
  real `events.EventBus` subscription, end-to-end behind
  `httptest.Server`; asserts identity propagation across every surface
  the page touches; covers ≥1 failure mode (missing identity →
  `identity_required`); runs under `-race` with N≥10 concurrent SSE
  subscribers.
- Honour every §13 binding rule: no Console DB shadow of runtime
  entities (D-061); the page IS the consumer of the primitives in
  this phase (§13 primitive-with-consumer); raw color / spacing
  literals rejected by Stylelint; design tokens only (§13); typed
  Protocol client only — no hand-rolled `fetch` (D-093); chat-module
  encapsulation rules (D-091); no Svelte 4 reactivity (D-092); the
  forbidden-name scan (drift-audit) stays clean.

## Non-goals

- The **Metrics tab** and **Health tab** content. Both consume
  `metrics.snapshot` / `runtime.health` primitives that land in Wave
  13's 72f phase. Phase 73b ships the tab strip with the two tabs
  rendering an empty state pointing at the 72f surface; when 72f
  lands, the tabs activate without reshaping the page.
- A session-level `priority` field on the right-rail session detail
  card. D-065 dropped session-level priority from V1. Task-level
  priority via the `prioritize` Protocol method is shipped and IS
  exposed on the per-task detail pane's action menu — that's the
  V1 surface for priority on this page.
- Persistence of operator-laid topology overrides (operator drags
  nodes around). Auto-layout via the shared engine-graph canvas (73i)
  is the V1 surface; per-session layout override persistence is
  post-V1 per the §3 `[deferred]` row.
- Drift mode (fork-a-run, edit a past message, re-play) and
  side-by-side comparison. Both are post-V1 per the §10 `[deferred]`
  list; the page surface for them stays absent.
- A new `tasks.trace` Protocol method. The §12 reconciliation
  notes that the Trace tab can be served either by a NEW `tasks.trace`
  method OR by extending `state.history` with span correlation. Phase
  73b takes the second option (extend `state.history`'s row shape
  with an optional `span_id` + `parent_span_id` per record); no new
  method name lands in `internal/protocol/methods/methods.go`. This
  keeps the canonical method set at the Phase 54 ten + the
  `[wave-13-extends]` cluster decided at the wave-decomposition stage.
- The agent picker on the Start composer ("Multi-agent / agent picker").
  That requires `agents.list` (NEW), which lands in 73e. Phase 73b's
  composer renders a single-agent default; when 73e lands, the picker
  activates additively.
- Cross-tenant Event Stream fan-in. That requires the `admin` /
  `console:fleet` scope claim (Phase 61) plus the Brief 11 §"elevate
  to fleet view" gesture; Phase 73b ships the page in single-tenant
  mode and rejects the elevation control client-side when the JWT
  lacks the claim (Protocol re-checks server-side and returns
  `scope_mismatch`).
- Embedding the page into `harbor dev`. The Console is served by
  `harbor console` per D-091; `harbor dev` stays headless.

## Acceptance criteria

- [ ] `internal/protocol/singlesource.CanonicalWireTypes` extends the
      `tasks.list` query/response shape with a `status_counter_strip`
      aggregate projection — five `Count` fields keyed by the canonical
      task statuses (`pending`, `running`, `completed`, `paused`,
      `failed`). The aggregate is computed server-side per request,
      scoped to the call's `(tenant, user, session)` triple; the
      counter never crosses the isolation boundary (no global counter).
- [ ] `internal/protocol/singlesource.CanonicalWireTypes` extends
      `events.subscribe` with a `RunID` filter field. The filter shape
      is server-built from the verified identity + request payload
      and enforced server-side per the shipped Phase 60 `events.Filter`
      composition (the existing `X-Harbor-Run` carrier from D-082 is
      preserved; this is the structured-payload counterpart).
- [ ] `internal/protocol/methods/methods.go` is **unchanged** — Phase
      73b ships no new method name (the canonical task-control set is
      closed; the `tasks.list` method itself lands in 73d per the
      wave-13 decomposition, and Phase 73b extends only its payload
      projection, which is a single-source canonical-wire-type
      extension).
- [ ] The Protocol TypeScript client (`web/console/src/lib/protocol.ts`)
      regenerates cleanly via `make protocol-ts-gen-check`; `git diff
      --exit-code` is clean on the committed file. The TS surface
      exposes the new `status_counter_strip` aggregate and the
      `events.subscribe` `run_id` filter.
- [ ] `web/console/src/routes/console/live-runtime/+page.svelte`
      mounts the Live Runtime page at the
      `/console/live-runtime/[session_id]` route. The page renders:
      (1) header status counter strip, (2) breadcrumb (`<runtime> /
      Live Runtime / <session-id>`), (3) tab strip (Topology /
      Timeline / Metrics / Health — Metrics + Health render the
      empty-state pointer until 72f lands), (4) main canvas
      consuming the shared engine-graph component for Topology and a
      swimlane variant for Timeline, (5) right rail (session detail
      header / Current Step / Recent Artifacts / Interventions /
      Cost / Last error / Tenant), (6) bottom dock (left: Event
      Stream / right: per-task detail pane with Details / Input /
      Output / Logs / Trace tabs OR a Skeleton-primitive composer
      when no node is selected — NO chat-module dependency per
      D-091's encapsulate-first rule), (7) footer (Protocol version +
      Events Stream connection state + Console version).
- [ ] All data sourcing flows through the typed Protocol client; no
      hand-rolled `fetch` in any `.svelte` file under
      `web/console/src/lib/components/live-runtime/` or in the
      `+page.svelte`. The Stylelint config (introduced by the
      first Console phase that creates `web/console/`) reports zero
      raw color / spacing / type-scale literals on these files.
- [ ] The page consumes the shared engine-graph canvas from 73i at
      `web/console/src/lib/components/graph/`. The canvas's
      `<EngineGraphCanvas>` component receives a typed `topology.snapshot`
      payload and emits a typed `node-click` event the page binds to
      the bottom-dock per-task detail pane.
- [ ] **The page does NOT consume the canonical chat module.** Per
      D-091 + CLAUDE.md §4.5 #11's "encapsulate first, extract on
      second consumer" rule, the chat module's V1 first consumer is
      73n Playground; a second in-V1 consumer would force extraction
      to `web/shared/chat/`, which is out of V1 scope. Live Runtime's
      composer surface (Start / Redirect / Inject context / User
      Message / Cancel / Pause / Resume) is built with Skeleton
      primitives (`Stack`, `Card`, `Textarea`, `Button`) under
      `web/console/src/lib/components/live-runtime/composer/` and
      calls the shipped Phase 54 control verbs through the typed
      Protocol client directly. The composer module's grep proves no
      imports from `$lib/chat/`.
- [ ] The Header status counter strip wires to the new
      `tasks.list` status-counter-strip aggregate; the chips update
      live on every `task.*` event the SSE subscription delivers.
- [ ] The right-rail Cost / Last error / Tenant fields wire to the
      shipped surfaces (`llm.cost.recorded` aggregation on the client
      via the `internal/llm/events.go::EventTypeCostRecorded` event;
      the most recent `task.failed` / `tool.failed` / `planner.error`
      payload for Last error; the identity-tuple tenant from
      `events.Event.Identity.TenantID` for Tenant).
- [ ] **No session-level `priority` field anywhere on the page.** This
      includes the right rail, the composer, and the per-task detail.
      Task-level priority via the shipped `prioritize` method is
      exposed only on the per-task detail pane's action menu.
- [ ] `web/console/tests/live-runtime-page.spec.ts` (Playwright)
      covers: (a) the topology canvas renders nodes + edges fed from
      a `topology.snapshot` fixture, (b) the `events.subscribe`
      trace-tab filter narrows the event stream to one run id, (c)
      the header status counter strip increments on a `task.started`
      event and decrements on a `task.completed` event, (d) a request
      without the session's identity triple is rejected
      `identity_required` at the edge.
- [ ] `test/integration/live_runtime_page_test.go` exists, wires the
      REAL runtime end-to-end (Phase 60 SSE + REST, real
      `tasks.TaskRegistry`, real `topology.snapshot` projection, real
      `events.EventBus`), asserts identity propagation across every
      Protocol surface the page touches, covers the missing-identity
      failure mode, and runs N≥10 concurrent SSE subscribers under
      `-race`. The test asserts the new `tasks.list` status-counter
      aggregate is identity-scoped (a second session never sees the
      first's counters).
- [ ] `scripts/smoke/phase-73b.sh` (executable; `# PREFLIGHT_REQUIRES:
      live-server`) probes `tasks.list` for the status-counter-strip
      projection shape AND probes `topology.snapshot` for a session
      with at least one task; both surfaces use the 404/405/501 →
      SKIP convention so the script coexists with builds that haven't
      yet landed the surface; FAIL = 0.
- [ ] `docs/glossary.md` carries the new vocabulary this phase
      introduces (`Status counter strip`, `Trace tab filter`,
      `Engine-graph canvas` — added in the same PR; checked for
      duplicates first).
- [ ] `docs/plans/README.md` row for Phase 73 is updated to show the
      Live Runtime page sub-row (`73b — Console Live Runtime page`)
      with `Status: Pending → Shipped` when the implementation
      lands; `README.md` Status table flips Phase 73b to Shipped.
- [ ] The forbidden-name scan (drift-audit + the
      `scripts/smoke/phase-73b.sh` defence-in-depth guard) is clean on
      `docs/plans/phase-73b-*.md`, `scripts/smoke/phase-73b.sh`,
      `web/console/src/routes/console/live-runtime/`,
      `web/console/src/lib/components/live-runtime/`, and
      `test/integration/live_runtime_page_test.go`.

## Files added or changed

```text
docs/plans/phase-73b-console-live-runtime-page.md
docs/plans/README.md                                  # 73b row Pending → Shipped on impl
docs/glossary.md                                      # Status counter strip, Trace tab filter, Engine-graph canvas
README.md                                             # Status table 73b → Shipped on impl
scripts/smoke/phase-73b.sh                            # tasks.list status-counter probe + topology.snapshot probe
internal/protocol/singlesource/                       # CanonicalWireTypes extensions for tasks.list aggregate + events.subscribe RunID filter
internal/protocol/singlesource/canonicalwiretypes_test.go
internal/tasks/                                       # tasks.list status-counter-strip aggregate projection (identity-scoped)
internal/tasks/tasks_test.go                          # aggregate unit + concurrent-reuse + cross-session isolation
internal/events/                                      # events.Filter RunID first-class field (the structured counterpart to D-082)
internal/events/events_test.go
web/console/src/lib/protocol.ts                       # regenerated from CanonicalWireTypes (D-093)
web/console/src/routes/console/live-runtime/
  +page.svelte                                        # the page mount
  +page.ts                                            # SvelteKit load function (typed client wiring)
  [session_id]/+page.svelte                           # session-scoped route
web/console/src/lib/components/live-runtime/
  status-counter-strip.svelte
  tab-strip.svelte
  timeline-tab.svelte
  metrics-tab-empty.svelte                            # empty-state pointer to 72f
  health-tab-empty.svelte                             # empty-state pointer to 72f
  event-stream-dock.svelte
  per-task-detail-pane.svelte                         # Details / Input / Output / Logs / Trace tabs
  session-detail-card.svelte
  current-step-panel.svelte
  recent-artifacts-panel.svelte
  interventions-panel.svelte
  trace-toggle.svelte
  footer.svelte
web/console/tests/live-runtime-page.spec.ts           # Playwright spec
test/integration/live_runtime_page_test.go            # Go-side integration test
```

## Public API surface

```go
// internal/protocol/singlesource — CanonicalWireTypes extensions
//
// tasks.list response gains a typed aggregate:
type TasksListStatusCounterStrip struct {
    Pending   int `json:"pending"`
    Running   int `json:"running"`
    Completed int `json:"completed"`
    Paused    int `json:"paused"`
    Failed    int `json:"failed"`
}
// (carried on the existing TasksListResponse shape under a typed
// `StatusCounterStrip *TasksListStatusCounterStrip` field; identity-
// scoped, computed server-side, never global.)

// events.subscribe filter shape gains a RunID first-class field:
type EventsSubscribeFilter struct {
    // ... existing fields (TenantID / UserID / SessionID / Admin / EventTypes ...)
    RunID string `json:"run_id,omitempty"`  // run-scoped filter; structured counterpart to D-082's X-Harbor-Run header
}

// No new Protocol method name — Phase 54's canonical set is unchanged.
```

```ts
// web/console/src/lib/protocol.ts (regenerated; not hand-written)
//
// Exposes the typed `status_counter_strip` aggregate on the tasks.list
// response and the `run_id` filter on events.subscribe.
```

```svelte
<!-- web/console/src/lib/components/graph/EngineGraphCanvas.svelte (consumed; from 73i) -->
<EngineGraph nodes={...} edges={...} on:node-click={...} />
```

## Test plan

- **Unit (Go):**
  - `internal/protocol/singlesource/canonicalwiretypes_test.go` — the
    two extensions round-trip through the Phase 58 single-source
    checker; the TS generator emits both fields.
  - `internal/tasks/` — `tasks.list` status-counter-strip aggregate
    is identity-scoped (cross-session call returns zero counts for a
    foreign session); aggregate computation under N≥100 concurrent
    callers is race-free.
  - `internal/events/` — `events.Filter.RunID` composes correctly
    with the shipped `events.Filter` triple; a subscription with a
    `RunID` filter receives only events whose `RunID` matches; cross-
    run isolation is asserted.
- **Unit (Svelte):**
  - Components under `web/console/src/lib/components/live-runtime/`
    receive typed Protocol payloads as props and render the
    page-spec-mandated affordances; tested via `svelte-check
    --fail-on-warnings` + a thin Vitest harness for prop-shape
    assertions.
- **Integration:**
  - `test/integration/live_runtime_page_test.go` — REAL runtime end-
    to-end behind `httptest.Server`: Phase 60 SSE + REST + real
    `tasks.TaskRegistry` + real `topology.snapshot` projection + real
    `events.EventBus`. Submits `start` over REST, observes the
    `task.spawned` event on the SSE stream, observes the new
    `tasks.list` status-counter-strip aggregate update, opens the
    bottom-dock per-task detail (via `tasks.get`), observes
    `state.history` entries; asserts the identity triple propagates
    through every surface; covers missing-identity failure mode
    (`identity_required`); N≥10 concurrent SSE subscriber stress;
    `-race`.
  - **Playwright** (`web/console/tests/live-runtime-page.spec.ts`):
    drives the rendered page against a mocked `harbor console` build
    with a typed Protocol-client double; covers (a) topology canvas
    nodes + edges from a `topology.snapshot` fixture, (b) trace-tab
    filter narrows the event stream, (c) status counter strip
    updates on `task.*`, (d) missing-identity request rejection at
    the edge.
- **Conformance:**
  - The Phase 58 single-source checker covers the new
    `CanonicalWireTypes` fields (no method strings / no error codes
    introduced under `internal/tasks/` or `internal/events/` outside
    their existing seams).
- **Concurrency / leak:**
  - `internal/tasks/` aggregate computation: N≥100 concurrent
    `tasks.list` callers against one shared `TaskRegistry`, under
    `-race`, asserting no data races + no goroutine leaks.
  - `internal/events/` `RunID` filter: N≥100 concurrent subscribers
    with overlapping + disjoint run filters, under `-race`.
  - `test/integration/live_runtime_page_test.go` includes an N≥10
    concurrent-SSE-subscriber stress arm; goroutine baseline
    restored after teardown.

## Smoke script additions

- `scripts/smoke/phase-73b.sh` (PREFLIGHT_REQUIRES: live-server):
  - Probes `POST /v1/control/tasks.list` (404/405/501 → SKIP)
    asserting the response carries a `status_counter_strip` object
    with the five canonical counters when the request scopes to a
    real session; OK once Phase 73b lands.
  - Probes `POST /v1/control/topology.snapshot` (404/405/501 →
    SKIP) asserting the response carries `nodes` + `edges` arrays
    keyed by run id; OK once Phase 74 + Phase 73b both land.
  - Probes the SSE event stream with the new `run_id` filter and
    asserts the stream only delivers events for the requested run.
  - Asserts a request without the session's identity triple is
    rejected `401 identity_required` per Phase 61.
  - Asserts the Stylelint guard against raw color / spacing
    literals reports zero offenders under
    `web/console/src/routes/console/live-runtime/` and
    `web/console/src/lib/components/live-runtime/` (static guard).
  - Asserts no `.svelte` file under the same paths contains a
    hand-rolled `fetch` call (static guard for D-093).
  - Asserts no occurrence of `priority` as a session-level field on
    any `.svelte` file under
    `web/console/src/lib/components/live-runtime/` (static guard for
    D-065).

## Coverage target

- `internal/protocol/singlesource`: 85% (extension fields covered)
- `internal/tasks` (status-counter-strip aggregate path): 85%
- `internal/events` (RunID filter path): 85%
- `web/console/src/lib/components/live-runtime/` Svelte components:
  Vitest unit-test coverage ≥ 70% (a Console-side soft target; the
  Playwright spec carries the end-to-end gate)

## Dependencies

- 60 (Phase 60 — Protocol wire transport, shipped)
- 61 (Phase 61 — Protocol auth, shipped)
- 72 (Phase 72 — Console subscription protocol surface; precedes
  this wave)
- 72a (Phase 72a — `events.subscribe` filter shape; Stage 1 of Wave
  13)
- 73 (Phase 73 — Console state inspection surface, parent; ships
  `sessions.inspect` / `tasks.get` / `state.history` /
  `artifacts.list`)
- 73i (Phase 73i — Console Flows page; ships the shared engine-graph
  canvas at `web/console/src/lib/components/graph/`)
- 73n is NOT a dep — 73b's composer uses Skeleton primitives, NOT the
  73n chat module (per D-091 + CLAUDE.md §4.5 #11)
- 74 (Phase 74 — Console topology projection events; Stage 1 of
  Wave 13)
- 75 (Phase 75 — Console e2e Playwright harness baseline; Stage 1
  of Wave 13)

## Risks / open questions

- **Page ↔ shared-component layering risk.** The page consumes the
  73i engine-graph canvas. If 73i reshapes its exported props late,
  this page breaks. Mitigated by the integration test wiring the REAL
  73i canvas in (not a mock).
- **Metrics + Health tabs as empty-state.** The tabs land in this
  phase but their content depends on 72f. The risk is the empty-
  state pointer drifts (the linked phase number changes). Mitigated
  by a static smoke assertion that the empty-state copy references
  the canonical 72f phase number; if 72f's number changes (renumber),
  the smoke fails and forces an update.
- **Span correlation on `state.history` extension.** The Trace tab
  consumes a `state.history` row shape extended with `span_id` +
  `parent_span_id`. The risk is that a future `tasks.trace` method
  (the §12 alternative path) lands later and orphans this surface.
  Mitigated by treating the Trace tab as a span-graph consumer
  abstracted by a small typed adapter — when / if `tasks.trace`
  lands, the adapter switches without reshaping the tab. The
  decision to extend `state.history` rather than mint a new method
  is recorded under the per-page §12 reconciliation; if it later
  reverses, that's a new `docs/decisions.md` entry, not silent
  drift.
- **`tasks.list` aggregate cost.** Computing the status-counter
  aggregate per `tasks.list` call adds a per-status count over the
  identity-scoped task set. For a session with many tasks, this is
  O(N) per call. Mitigated by (a) the aggregate being optional —
  callers opt in via a query field — and (b) the live update path
  using the SSE event stream to maintain the strip rather than re-
  polling. The aggregate call is the page's initial-load shape only.
- **D-066 control-claim hiding vs server enforcement.** The
  Approve / Reject / Resume / Pause / Cancel-hard buttons are
  client-side hidden when the JWT lacks the elevated control claim,
  but the server-side check is the gate. Risk: a stale client
  showing the buttons after a scope downgrade. Mitigated by the
  `events.subscribe` stream emitting a `scope.degraded` event when
  the server re-evaluates the JWT's scope set mid-session (Phase 61
  surface); the page reacts by re-hiding the buttons.

## Glossary additions

- **Status counter strip** — the header-level five-chip aggregate on
  the Console Live Runtime page rendering `pending / running /
  completed / paused / failed` counts for the page's session, fed
  by the `tasks.list` status-counter-strip aggregate (identity-
  scoped, computed server-side, updated live via `task.*` SSE
  events). Distinct from the canvas-level status legend, which is a
  per-topology-tab affordance. Phase 73b.
- **Trace tab filter** — the structured `events.subscribe` filter
  field (`RunID`) that narrows the event subscription to one run id.
  Used by the bottom-dock Trace tab to correlate events to a
  topology node by run. Structured counterpart to the
  `X-Harbor-Run` header carrier added under D-082. Phase 73b.
- **Engine-graph canvas** — the shared SvelteKit component at
  `web/console/src/lib/components/graph/` (introduced by Phase 73i;
  consumed by Phase 73b) that renders a runtime topology as a
  directed graph with auto-layout, status pills, edge bundles, and a
  typed `node-click` event. Reusable across the Console pages that
  visualise an engine graph (Live Runtime, Flows). RFC §7.1.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes (the new `tasks.list` aggregate is identity-scoped — the
      cross-session assertion is in
      `test/integration/live_runtime_page_test.go`)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse
      test passes — N≥100 concurrent invocations against a single
      shared instance under `-race`.** The page UI itself is not a
      Go-side reusable artifact, but the new `tasks.list` aggregate
      computation path AND the `events.Filter.RunID` composition path
      are exercised by aggregate-side N≥100 concurrent tests under
      `-race` (in `internal/tasks/` and `internal/events/`).
- [ ] **If this phase consumes a shipped subsystem's surface OR
      closes a cross-subsystem seam: an integration test exists,
      wires real drivers end-to-end, asserts identity propagation,
      covers ≥1 failure mode, runs under `-race`.**
      `test/integration/live_runtime_page_test.go` covers the live
      runtime + topology.snapshot + events.subscribe + tasks.list +
      Phase 60 SSE/REST seam end-to-end.
- [ ] If new vocabulary: glossary updated (Status counter strip,
      Trace tab filter, Engine-graph canvas)
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — N/A, no departures.
