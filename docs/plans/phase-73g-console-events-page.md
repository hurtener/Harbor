# Phase 73g — Console Events page (UI consumer of Phase 72a `events.aggregate` + saved-filter `events.subscribe` chips)

## Summary

Phase 73g ships the Console Events page — the full-screen, query-driven
investigative surface over the runtime's event bus. The page is a pure UI
consumer of Phase 72a's `events.subscribe` filter extensions and
`events.aggregate` time-bucket method; **this phase ships NO new
Protocol method.** Truncated-payload `Open artifact` links route through
the already-shipped `artifacts.get` so the D-026 heavy-content rule is
enforced at the page edge (never inline bytes). The acceptance is
`web/console/src/routes/console/events/+page.svelte` plus the per-page
Playwright spec covering every binding refinement in
`docs/design/console/page-events.md` §12.

## RFC anchor

- RFC §5.2
- RFC §6.13
- RFC §7

## Briefs informing this phase

- brief 11
- brief 06
- brief 12

## Brief findings incorporated

- **brief 11 §"Events view (OVERVIEW)".** "The event stream from the
  bottom dock but as a full-screen, query-driven view: Time-range
  picker, type filter, identity filter, free-text search. Save / share
  filtered views. Export to JSONL / CSV (per-row rendered,
  post-redaction). Aggregate counts visualization (rate-over-time)."
  The page-events.md §12 mockup refinements pin exactly that shape:
  faceted filter chips, saved-view chips (Console-local), client-side
  NDJSON / CSV export, and a stacked-area sparkline. Phase 73g
  implements the page surface to that spec.
- **brief 11 §CC-4 "Global search (⌘K)" — events as high-cardinality.**
  Brief 11 recommends a runtime-side `search.events` Protocol method
  because events are high-cardinality. Phase 73g's free-text
  "Search events…" input is a **client-side substring match** over
  event names + the loaded page's payload-JSON strings (the same
  Console-local mode page-events.md §12 specifies). A runtime-side
  `search.events` is Phase 72c's surface (Wave 13 Stage 1); the Events
  page is wired to call into `search.events` once Phase 72c ships, but
  the Stage-2.2 cut for this phase does not block on it. The fallback
  is explicit and surfaced: when `search.events` is not advertised in
  the `VersionHandshake` (D-077), the Search box renders a tooltip
  "Searching the loaded page only — runtime-side search not yet
  available on this Runtime." This is the §13 "fail loudly" surface,
  read at the UI layer.
- **brief 06 §"Replay semantics".** A subscriber's cursor is "the last
  Sequence I have." Pause-stream toggle is a **Console-local view
  toggle**: while paused, the underlying SSE cursor keeps advancing
  per D-029 (`Replay` returns `[]Event`, never gaps); on resume, the
  table flushes accumulated events in cursor order without dropping
  the cursor position. This is observably DIFFERENT from the runtime
  `pause` Protocol method (which is task-scoped) — the Pause-stream
  toggle is purely a render gate; no Protocol call fires.
- **brief 06 §"Persistence" + D-074 (durable event log).** A
  cursor-too-old window is recoverable when the `durable` driver is
  active; the page surfaces "Older window — using durable backfill"
  vs. "Older window unavailable — narrow time range" exactly as
  page-events.md §7 prescribes. The page does NOT decide between the
  two; it renders whichever `CodeReplayUnavailable` reason the
  Protocol surface reports.
- **brief 12 §"The two-surface model".** The Console is shipped via
  `harbor console` (D-091), never embedded in `harbor dev`. The
  Events page's `+page.svelte` is part of the SvelteKit static build
  that `harbor console` serves; it is reachable from any Runtime the
  operator attaches to. No assumption that the Runtime and Console
  share a process.

## Findings I'm departing from (if any)

None. Phase 73g implements page-events.md §12 verbatim under the
"UI composes Phase 72a surface" cut the wave-13 decomposition §5 row
73g settled. The decomposition's note "NO new Protocol method (composes
Phase 72a's surface)" is binding; no surface from this phase escapes the
Console-local / Phase-72a / Phase-73 (artifacts.get) boundary.

## Goals

- A single SvelteKit route under `web/console/src/routes/console/events/`
  rendering the four-row main canvas (faceted filter chips +
  saved-view chip row + event-rate sparkline + virtualised table) and
  the right-rail Event Details card per page-events.md §4 + §12.
- The page subscribes to `events.subscribe` (Phase 72a filter shape)
  through the typed Protocol client at
  `web/console/src/lib/protocol.ts` — no hand-rolled `fetch` in
  `.svelte` files (CLAUDE.md §4.5 + §13).
- The page consumes `events.aggregate` (Phase 72a, time-bucketed counts)
  to drive the stacked-area sparkline; bucket granularity is selected
  from the active window (5 min / 1 h / 24 h / 7 d) per page-events.md
  §12; hover-highlight + click-to-pin filter chip interactions both
  fire only Console-local state changes.
- **Saved-view chips, Pause-stream toggle, Export ▾, and pagination
  size are Console-local** per D-061 — they persist in the Console DB
  (Phase 72h surface; the Events page reads/writes the
  `saved_filters` table only). No Protocol method mutates Console-side
  state; the Events page is observation-only at V1 (D-066).
- **Truncated-payload `Open artifact` link routes through the shipped
  `artifacts.get`** (Phase 73's surface, already shipped); the page
  NEVER inlines heavy payload bytes, closing the D-026 leak shape at
  the Console edge. A payload that exceeds the heavy-content threshold
  renders a `Truncated` badge plus an `Open artifact` link whose
  `href` is the `ArtifactRef` resolved via the typed client.
- **Cross-tenant `Tenant ▾` facet is gated on `auth.ScopeAdmin` /
  `auth.ScopeConsoleFleet`** (D-079) — the facet's dropdown lists only
  tenants the operator's scope authorizes; toggling cross-tenant
  fan-in on `events.subscribe` is the runtime's path, and the page
  surfaces the emitted `audit.admin_scope_used` event in the table
  itself (a visible, retroactively-detectable footprint).
- **Pause-stream toggle is a Console-local view toggle** — distinct
  primitive from the runtime `pause` Protocol method (which is
  task-scoped). The toggle name in code is `streamPaused` (Svelte 5
  rune-state); the runtime `pause` method has no call site in this
  page. The footer chip flips to `Events Stream: PAUSED` (amber)
  while paused; the SSE cursor is preserved.
- The page ships its per-page Playwright spec
  `web/console/tests/events-page.spec.ts` covering: faceted filter
  chips narrow rows; saved-view chips apply; pause-stream toggle
  freezes the table; Export ▾ produces NDJSON; Open-artifact link
  resolves heavy payloads via `artifacts.get`.
- A Go-side integration test
  `test/integration/events_page_test.go` wires the real
  `events.EventBus` + Phase 72a's `events.subscribe` + `events.aggregate`
  - the shipped `artifacts.get` + the Phase 60 transport edge, and
  asserts the sparkline aggregation is correct over deliberate event
  emission (count and per-type bucket totals match what was
  published). Identity propagation is the failure mode; the test
  runs under `-race`.

## Non-goals

- **A new Protocol method.** Every Protocol surface this page consumes
  is shipped (`artifacts.get`, Phase 73) or shipped by Phase 72a
  (`events.subscribe` filter extensions, `events.aggregate`). The
  primitive-with-consumer rule (CLAUDE.md §13) is satisfied
  trivially: 73g IS the consumer Phase 72a's primitives wait for.
- **A runtime-side `search.events`** — that is Phase 72c's surface
  (Wave 13 Stage 1). The Events page's Search box is Console-local
  substring match until 72c lands; when it does, the page upgrades
  the Search box to call `search.events` without a re-render of the
  surrounding chrome.
- **Cross-runtime aggregator** (D-091 — post-V1). The page is bound to
  the single Runtime the Console is currently attached to.
- **Anomaly detection / alert rules** (page-events.md §10 — post-V1).
- **OTel deep-link from the trace cell** (page-events.md §10 — D-073
  ships the traceparent carrier; the link is rendered as
  disabled-with-tooltip in V1).
- **URL-encoded shareable saved-views** — page-events.md §10 lists
  this as Console-only UX polish; landing it is acceptable inside
  73g as long as the encoding/decoding stays in the page module and
  no Protocol method mutates Console state. **Default for this
  phase: defer URL encoding to a Wave 13 follow-up — the §17.7
  cadence prefers narrow, reviewable scope per phase.**
- **Console DB schema definition.** The `saved_filters` table is
  Phase 72h's surface; 73g consumes it.
- **The `harbor console` subcommand.** That subcommand bundles into
  Phase 73m (Settings) per wave-13-decomposition §9 question 8.

## Acceptance criteria

- [ ] `web/console/src/routes/console/events/+page.svelte` renders the
      four-row main canvas: faceted filter chips (Event type / Tenant
      / User / Session / Run / Window / More filters), saved-view chip
      row, event-rate sparkline (stacked area), virtualised event
      table — matching page-events.md §4 + §12 column order.
- [ ] The right-rail Event Details card (sticky, full-height when a
      row is selected) renders Source / Identity / Payload (json) /
      Quick Actions in mockup order, with copyable
      `tenant_id` / `user_id` / `session_id` / `run_id` / `task_id`.
- [ ] The subscription opens via the typed Protocol client
      (`web/console/src/lib/protocol.ts`) — `events.subscribe` with
      the filter shape from Phase 72a; on Last-Event-ID reconnect the
      SSE handler resumes from the last received `Sequence` per
      D-029 + Phase 60 §3.
- [ ] The sparkline data source is `events.aggregate` (Phase 72a);
      bucket granularity matches the active window (5 min: 30s; 1 h:
      1 min; 24 h: 5 min; 7 d: 1 h) per page-events.md §12. Hover
      highlights the corresponding row; click pins that event-type
      facet chip. No Protocol mutation fires on either interaction.
- [ ] Saved-view chips are Console-local — the chip row reads/writes
      `saved_filters` only (Phase 72h's Console DB table); selecting a
      chip rewrites the filter chips above and re-opens the
      subscription. No Protocol method is called.
- [ ] Pause-stream toggle is a render-only gate. While paused, the SSE
      cursor keeps advancing; on resume, accumulated events flush in
      cursor order with no gap (D-029); the table reorders deterministically.
      The footer chip reflects `Events Stream: ON | PAUSED` (amber when paused).
- [ ] Export ▾ produces NDJSON (default) or CSV from the currently
      filtered page — client-side aggregation; no Protocol call. The
      `Bottom dock` strip shows export progress and clears on
      completion.
- [ ] Truncated-payload rows render a `Truncated` badge and an `Open
      artifact` link; clicking it resolves `artifacts.get` (already
      shipped) and opens the artifact viewer. **Heavy payload bytes
      NEVER appear inline in any Svelte component** (D-026; CLAUDE.md
      §13 raw-heavy-content rule, read at the Console edge).
- [ ] The `Tenant ▾` facet's dropdown lists only tenants the operator
      scope authorizes; toggling cross-tenant fan-in on
      `events.subscribe` (Phase 72a's filter shape) emits
      `audit.admin_scope_used` on subscribe (Phase 05's already-shipped
      auditing path); the resulting event surfaces in the page's own
      table — observable, retroactively detectable (D-079).
- [ ] All four `CodeReplayUnavailable` reasons are rendered with the
      banners page-events.md §7 prescribes: empty timeline, bus
      dropped, cursor too old (with `durable` backfill fork),
      identity required, auth rejected, scope mismatch, invalid request.
- [ ] No raw color / spacing / type-scale literals in any `.svelte`
      file the page introduces (CLAUDE.md §4.5 §3 — tokens-only,
      stylelint-enforced).
- [ ] No hand-rolled `fetch` calls in any `.svelte` file the page
      introduces; every Protocol round-trip goes through the typed
      client (CLAUDE.md §4.5 §5 + §13).
- [ ] No Svelte-4 reactivity syntax (`$:`, top-level `let` as reactive
      state, `export let` props, `$store` script auto-subscription)
      in any new file; `svelte-check --fail-on-warnings` is clean
      (CLAUDE.md §4.5 §1 + §6, D-092).
- [ ] The page composes Skeleton primitives (`@skeletonlabs/skeleton`)
      for table, chips, dropdown, dialog, copy-to-clipboard, tooltip;
      no hand-rolled component primitives Skeleton already provides
      (CLAUDE.md §4.5 §4).
- [ ] `web/console/tests/events-page.spec.ts` ships and is wired into
      the Phase 75 Playwright harness; the spec asserts: (a) faceted
      filter chips narrow rows; (b) saved-view chips apply; (c)
      Pause-stream toggle freezes the table; (d) Export ▾ produces
      NDJSON; (e) Open-artifact link resolves heavy payloads via
      `artifacts.get`.
- [ ] `test/integration/events_page_test.go` ships: real `events.EventBus`
      (in-mem) + Phase 72a `events.subscribe` + Phase 72a
      `events.aggregate` + shipped `artifacts.get`, asserts sparkline
      correctness over deliberate event emission (count and per-type
      bucket totals match published), identity propagation through
      every layer, ≥1 failure mode (truncated-payload artifact-fetch
      identity-rejected branch), runs under `-race`.
- [ ] `scripts/smoke/phase-73g.sh` exists, is executable, classified
      `# PREFLIGHT_REQUIRES: live-server`, and probes upstream
      `events.subscribe` / `events.aggregate` (SKIP via `protocol_call`
      until 72a's surface ships) + the `/console/events` route's
      static asset 200 (SKIP until `harbor console` lands per Phase
      73m).
- [ ] Master plan `docs/plans/README.md` flips Phase 73g's status row
      from `Pending` to `Shipped` in the same PR; root `README.md`
      Status table updated.

## Files added or changed

- `docs/plans/phase-73g-console-events-page.md` (this file).
- `scripts/smoke/phase-73g.sh` (new — page-route + upstream-method probes).
- `web/console/src/routes/console/events/+page.svelte` (new — page surface).
- `web/console/src/routes/console/events/+page.ts` (new — `load`
  function; identity / scope read from the Protocol client's session
  context; no SSR, per CLAUDE.md §4.5 §7).
- `web/console/src/lib/events/` (new module dir):
  - `subscription.ts` — typed wrapper over
    `protocol.events.subscribe`; surfaces a Svelte 5 `$state`-backed
    rolling page of `Event` plus a `cursor: events.Cursor` rune.
  - `aggregate.ts` — typed wrapper over `protocol.events.aggregate`;
    surfaces a Svelte 5 `$derived`-backed time-bucketed counts shape
    keyed by `event_type`.
  - `filters.ts` — pure filter-builder + URL-encode/decode helpers
    (deferring URL share per Non-goals); `compileFilter` returns the
    `events.Filter` payload Phase 72a accepts.
  - `saved-views.ts` — Console-local saved-filter read/write against
    the Phase 72h `saved_filters` Console DB table via the typed
    client's Console-local endpoint surface.
  - `sparkline.ts` — pure bucket-aggregation helpers (rebucketing
    Phase 72a output when the window switches without a re-fetch).
  - `export.ts` — pure NDJSON + CSV serialisers over the loaded page.
- `web/console/src/lib/events/components/` (Skeleton-composed primitives):
  - `FilterBar.svelte` — faceted filter chips.
  - `SavedViewChips.svelte` — chip row (Console-local).
  - `EventRateSparkline.svelte` — stacked-area chart over
    `aggregate.ts` output.
  - `EventTable.svelte` — virtualised table with the columns
    page-events.md §12 prescribes.
  - `EventDetailRail.svelte` — right-rail Event Details card.
  - `PauseStreamToggle.svelte` — Console-local toggle; flips
    footer chip; no Protocol call.
  - `ExportMenu.svelte` — Export ▾ dropdown over `export.ts`.
  - `TruncatedPayloadLink.svelte` — renders the `Truncated` badge +
    `Open artifact` link; resolves via the typed client's
    `artifacts.get`.
- `web/console/tests/events-page.spec.ts` (new — per-page Playwright spec).
- `test/integration/events_page_test.go` (new — Go-side integration
  test over real drivers).
- `scripts/smoke/phase-73g.sh` (new).
- `docs/glossary.md` (append: see "Glossary additions" below).
- `docs/plans/README.md` (flip 73g row to Shipped).
- `README.md` (flip 73g row to Shipped in the Status table).

No new top-level directory; `web/console/` is the canonical Console
home introduced by Phase 72 (the Console-wave parent) per CLAUDE.md
§4.5. The page is one route under that tree; no §3 layout change.

## Public API surface

**No Go-side public API surface in this phase.** Phase 73g is UI
composition; the Go side surfaces only the integration test against
existing Phase 72a methods.

**Console-side typed surfaces** (additive, lib-local — not Protocol
wire types; nothing in `internal/protocol/types/`):

- `EventsSubscription` (in `web/console/src/lib/events/subscription.ts`):

  ```ts
  export class EventsSubscription {
    readonly events: Event[];          // $state-backed
    readonly cursor: events.Cursor;    // $state-backed
    readonly streamPaused: boolean;    // Console-local view toggle
    open(filter: events.Filter): void; // calls protocol.events.subscribe
    close(): void;
    pause(): void;                     // Console-local; NOT protocol.pause
    resume(): void;                    // flushes buffered events
  }
  ```

- `EventsAggregator` (in `web/console/src/lib/events/aggregate.ts`):

  ```ts
  export class EventsAggregator {
    readonly buckets: AggregateBucket[]; // $derived from filter + window
    setWindow(w: TimeWindow): void;
    setFilter(f: events.Filter): void;
  }
  ```

These are Console-local lib types; they live entirely under
`web/console/src/lib/events/` and are not part of the Protocol wire
surface (CLAUDE.md §8 + D-093).

## Test plan

- **Unit (web/console):**
  - `web/console/src/lib/events/filters.test.ts` — `compileFilter`
    round-trip, identity-quadruple propagation, "More filters"
    facets (transport, tool id, planner id) map correctly to the
    Phase 72a filter shape.
  - `web/console/src/lib/events/sparkline.test.ts` — rebucketing
    correctness when the window changes (5 min ↔ 1 h ↔ 24 h ↔ 7 d);
    stacked totals = bucket sum.
  - `web/console/src/lib/events/export.test.ts` — NDJSON / CSV
    serialisation of a known event log; no heavy payloads are
    inlined (truncated rows render the `ArtifactRef`, never bytes).
  - `web/console/src/lib/events/saved-views.test.ts` — saved-view
    CRUD against a mocked Console DB; no Protocol method is invoked
    (asserted by spying on the typed client).
- **Integration (test/integration/events_page_test.go):**
  - Real drivers everywhere on the seam (CLAUDE.md §17.3 rule 1):
    `events.drivers.inmem` (the V1 bus), Phase 72a's
    `events.subscribe` + `events.aggregate` handler implementations,
    Phase 73's `artifacts.get` handler, the Phase 60 transport edge
    mux (`transports.NewMux` with `WithoutValidator()` for the test
    posture — production posture is Phase 61's `WithValidator`).
  - Publishes a deliberate sequence: 100 `tool.invoked` + 30
    `tool.failed` + 20 `planner.decision` + 1 truncated-payload
    `tool.completed` whose payload references an `ArtifactRef`;
    asserts:
    - `events.aggregate` returns per-type bucket totals matching the
      published count (the sparkline correctness gate).
    - `events.subscribe` filter narrowing by `event_type=tool.failed`
      returns exactly 30 rows.
    - The truncated-payload event's `ArtifactRef` resolves via
      `artifacts.get` and the resolved bytes match what was stored
      (the heavy-content artifact round-trip).
  - Identity propagation: every event carries the publishing
    identity quadruple; cross-tenant fan-in is rejected unless
    `auth.ScopeAdmin` is held; this is the cross-tenant isolation
    gate (CLAUDE.md §6 rules 5 + 9).
  - Failure mode: `artifacts.get` called WITHOUT the matching
    identity returns `CodeIdentityRequired`; the page-side
    `TruncatedPayloadLink` renders the recovery banner. (The
    integration test exercises the server-side rejection; the
    Playwright spec exercises the client-side banner render.)
  - Runs under `-race`; the bus subscription is exercised with N≥16
    concurrent subscribers (one per identity quadruple) — no
    cross-talk, no goroutine leak after teardown.
- **Conformance:** N/A — no new Protocol method; nothing extends a
  shipped conformance pack. (Phase 62 Protocol conformance already
  guards `events.subscribe` / `events.aggregate` once Phase 72a lands
  them.)
- **Concurrency / leak:** the integration test's N≥16 concurrent
  subscriber stress + `runtime.NumGoroutine` baseline-restored
  assertion after teardown is the cross-subsystem leak gate
  (CLAUDE.md §17.3 + §17.5 "concurrency stress run"). No Go-side
  reusable artifact is built in this phase, so a per-package
  D-025 concurrent-reuse test is N/A — the integration stress is the
  gate for the cross-package wiring 73g exercises.
- **Playwright (per-page):** `web/console/tests/events-page.spec.ts`
  covers:
  - Faceted filter chips narrow rows (event-type ▾, Tenant ▾, Run
    ▾) — drives the typed client, asserts the table row count
    matches the filter.
  - Saved-view chips apply: clicking a chip rewrites the filter
    chips above; the Console DB write fires; no Protocol method
    fires for the save (spy assertion).
  - Pause-stream toggle freezes the table: rendered row count stays
    constant while the runtime publishes; resume flushes the
    accumulated rows in `Sequence` order.
  - Export ▾ produces NDJSON: clicks Export → NDJSON, asserts the
    download contains the currently-filtered events, asserts no
    heavy payload bytes inlined (truncated rows carry the
    `ArtifactRef` only).
  - Open-artifact link resolves heavy payloads: clicks the link,
    asserts an `artifacts.get` call fires, asserts the artifact
    viewer opens with the resolved content.

## Smoke script additions

`scripts/smoke/phase-73g.sh` (`# PREFLIGHT_REQUIRES: live-server`) ships with:

- `protocol_call 'events.subscribe' '{"identity":{...},"types":["tool.failed"]}'`
  — SKIPs until Phase 72a's `events.subscribe` filter extensions ship;
  flips to OK once the surface lands (per the 404/405/501 → SKIP
  convention, CLAUDE.md §4.1).
- `protocol_call 'events.aggregate' '{"identity":{...},"window":"1h","bucket":"1m"}'`
  — SKIPs until Phase 72a's `events.aggregate` ships; flips to OK
  once the surface lands.
- `assert_status 200 "$(api_url /console/events)" "console events page route serves 200"`
  — SKIPs (`404`) until `harbor console` is wired (Phase 73m). The
  build-time check `assert_file web/console/src/routes/console/events/+page.svelte`
  is run too so a missing page surfaces even before `harbor console`
  lands.
- `assert_file web/console/tests/events-page.spec.ts "per-page Playwright spec exists"`
  — fails if the per-page spec is missing.
- `assert_grep_absent '#[0-9a-fA-F]\{3,8\}' web/console/src/routes/console/events/+page.svelte "no raw hex colour literals in events page"`
  — defence-in-depth raw-literal guard alongside stylelint.

## Coverage target

- `web/console/src/lib/events/`: 80% (the four lib modules under
  unit-test coverage). The page `.svelte` files are exercised by the
  Playwright spec, not by Go-side coverage tooling.
- `test/integration/events_page_test.go`: N/A line-coverage target;
  the integration test is a wave-boundary regression gate (CLAUDE.md
  §17.2 — "test/integration tests don't bloat per-package coverage
  reports"). The acceptance is functional, not coverage-pinned.

## Dependencies

- 72a (the `events.subscribe` filter extensions + `events.aggregate`
  time-bucket method).
- 73 (the existing state-inspection wave — `artifacts.get` is already
  shipped on this Runtime).
- 75 (Playwright harness baseline — the per-page spec hooks into the
  Stage-1 harness `npm run test:e2e` entry).

The Phase 72c `search.events` Protocol method is a SOFT dependency:
when it ships the Search box upgrades to a Protocol call; until then
the Search box is Console-local substring match over the loaded page.

## Risks / open questions

- **What happens when the operator narrows the `events.aggregate`
  window mid-subscription?** A: the page re-fetches `aggregate` with
  the new window; the table-level filter is independent (table
  subscribes via `events.subscribe`; sparkline subscribes via
  `events.aggregate`). The two cursors are deliberately independent
  per page-events.md §4 — the sparkline is a derived rate view, the
  table is the event stream.
- **What does Pause-stream do for the sparkline?** A: nothing —
  Pause-stream freezes the TABLE's rendering only. The sparkline
  continues to refresh from `events.aggregate` because hiding the
  rate-over-time view while events are flowing is a worse UX than
  freezing only the table. Documented in `EventRateSparkline.svelte`'s
  godoc and asserted in the Playwright spec.
- **Cross-tenant facet for non-admin operators.** A: the `Tenant ▾`
  dropdown lists only tenants the operator's scope authorizes (the
  Console reads its own scope from the verified JWT via the typed
  client). For a non-admin operator the dropdown shows ONLY their own
  tenant, disabled. This is a render-side enforcement; the
  authoritative gate is Phase 61's `auth.HasScope(ScopeAdmin)` at the
  transport edge (D-079) — the page render is defence-in-depth.
- **`bus.dropped` indicator strip behavior under heavy event rate.**
  A: page-events.md §3 ships this as a `[shipped]` surface; the page
  subscribes to `bus.dropped` events specifically and renders the
  banner on first drop in the active window. Already covered by
  Phase 05's bus surface; 73g just renders it.
- **The OTel `Open trace` Quick Action remains disabled-with-tooltip
  in V1** per page-events.md §10 — D-073's traceparent carrier ships,
  but the OTel viewer link is post-V1.

## Glossary additions

- **Event-rate sparkline** — the per-event-type stacked-area chart at
  the top of the Console Events page main canvas. Data source is
  `events.aggregate` (Phase 72a) bucketed by the active window
  (5 min: 30 s; 1 h: 1 min; 24 h: 5 min; 7 d: 1 h). Hover highlights
  the corresponding event row in the table below; click pins that
  event-type filter chip. Pure read; no Protocol mutation. RFC §6.13,
  RFC §7, page-events.md §12.
- **Pause-stream toggle** — the Console-local view toggle on the
  Console Events page that freezes the table's rendering without
  closing the underlying SSE subscription. **Distinct primitive from
  the runtime `pause` Protocol method** (which is task-scoped — RFC
  §5.2). While the toggle is on, the underlying `events.Cursor`
  continues to advance per D-029; resuming flushes accumulated events
  in cursor order without dropping the cursor position. The footer
  chip flips to `Events Stream: PAUSED` (amber). The toggle does NOT
  fire any Protocol method. RFC §7, D-029, D-061.
- **Saved-view chip (events)** — a Console-local chip on the Console
  Events page that captures a named filter combination (event types
  - identity facets + window). Persisted in the Phase 72h Console DB
  `saved_filters` table; never round-trips through the runtime
  (D-061). Distinct from a Protocol `events.Filter` shape — the chip
  is the operator's named pointer at a particular `events.Filter`
  payload. RFC §7, D-061.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes (the integration test asserts cross-tenant fan-in is
      rejected without `auth.ScopeAdmin` — covers the seam this phase
      exercises).
- [ ] **Concurrent-reuse test:** N/A — Phase 73g builds no Go-side
      reusable artifact (no engine / tool / planner / driver /
      redactor / client / catalog). The Console-local TypeScript
      classes (`EventsSubscription`, `EventsAggregator`) are
      per-page-instance, not "compiled artifacts" in the D-025 sense.
- [ ] **Integration test:** the integration test
      `test/integration/events_page_test.go` ships in the same PR,
      wires real drivers, asserts identity propagation, covers ≥1
      failure mode (truncated-payload `artifacts.get` identity-rejected
      branch), runs under `-race`. The Playwright per-page spec
      `web/console/tests/events-page.spec.ts` covers the UI
      interactions enumerated in Acceptance criteria.
- [ ] If new vocabulary: glossary updated (three terms added — see
      "Glossary additions").
- [ ] If a brief finding was departed from: justified above +
      `docs/decisions.md` entry filed — N/A; "None" recorded.
