# Phase 108h — Console Events page (carded retheme + viewport-lock + verify)

> Page-polish phase against `docs/design/console/CONVENTIONS.md` +
> `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The shipped Events page
> (Phase 73g / D-125) is already wired to the shipped Protocol surface
> (`events.subscribe` SSE table feed + `events.aggregate` sparkline + the
> Console-local saved-views / export / pause), but it predates the 108b
> app-shell chrome and the 108c carded retheme, and it is NOT viewport-locked
> (the page full-page-scrolls; the table grows unbounded). This phase rethemes
> it to the carded `.panel.card` vocabulary the five done pages set (Overview,
> Live Runtime, Settings, Playground, Sessions), viewport-locks it, live-wire
> verifies every datum/action, and fills the targeted gaps the audit found.

## Summary

Bring the Events page to verbatim parity with its mock at the look-and-feel +
viewport-discipline level, keeping its rich query composition (faceted filter
strip + per-type rate sparkline + virtualised events table + Event Details
right rail). The page is NOT over-engineered — Events is the power-user
event-bus investigative surface and legitimately needs each region — so this is
a retheme + viewport-lock + verify pass, not a simplify-or-rewire. Drop the
per-page `PageHeader` (the breadcrumb is app-shell chrome), adopt the carded
panels, and viewport-lock the page (the filter strip + sparkline are
fixed-height; the events table scrolls internally behind a sticky header; the
right rail scrolls internally) — the Playground / Sessions pattern. Every
Runtime read stays on the unified `HarborClient` via the `EventsPageState`
controller (no hand-rolled `fetch`). The phase ships NO new Protocol method.

Targeted gap-fills (PAGE-POLISH §1 — no fabrication, surface findings):

- The table feed is the LIVE `events.subscribe` SSE (not a persistent read);
  the prior empty-state copy claimed the list reads a persistent buffer that
  only the `durable` driver populates, which mis-describes the live stream.
  Reframe the empty copy to the honest live-stream reality.
- `events.aggregate` defaults to the caller's own session when the filter
  elides it, so the rate sparkline renders empty on the default view. Scope the
  sparkline aggregate to the active facet set (or the subscription's observed
  events) so it reflects what the table shows.
- The right rail is empty when no row is selected; per page-events.md §4 it
  should show the live subscription status (cursor sequence, dropped count,
  stream state). Fill that idle state.

## RFC anchor

- RFC §7
- RFC §7.1

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Events view" / §LR-5 (Event Stream shared component): Events is the
  full firehose with a powerful query / filter / save-view shape — a list +
  per-event detail + export, distinct from the session-scoped Live Runtime and
  Sessions slices. This phase keeps that composition and makes it calm +
  viewport-locked.
- brief 11 §CC-2 (identity-aware UI): the admin cross-tenant fan-in toggle is
  gated on the `admin` scope (D-079) and emits `audit.admin_scope_used`;
  preserved. The Tenant facet only lists authorized tenants.
- brief 12 §"two-surface model": Events is read-only observation — no
  control-plane verbs on this page (those live on Live Runtime / Tasks).
  Preserved.

## Findings I'm departing from (if any)

None. This phase RE-aligns the page to its own mock (the carded, viewport-locked
composition) and to page-events.md §4 (the idle right-rail subscription status),
without changing the data layer.

## Goals

- Retheme `/events` to the carded `.panel.card` + `.panel-title` vocabulary
  (Overview 108c), tokens only, Svelte 5 runes (D-092), HarborClient only.
- Viewport-lock (PAGE-POLISH §6): no full-page scroll; the events table + the
  right rail scroll internally; the filter strip + sparkline stay fixed.
- Live-verify every wire (table SSE, sparkline aggregate, detail-rail payload +
  quick actions, pause, export, saved views, admin fan-in, bus.dropped strip).
- Fill the three audited gaps (empty-copy reframe, sparkline scope, idle
  right-rail subscription status).
- Delete dead/misleading code; keep the events lib + components otherwise
  unchanged in behaviour.

## Non-goals

- No new Protocol method (the page stays a pure consumer of the shipped
  `events.subscribe`, `events.aggregate`, `artifacts.get_ref`, and Console DB).
- No runtime-side `search.events` (Phase 72c / `[wave-13-extends]`); the search
  box stays Console-local substring match over the loaded page (honest copy).
- No trace deep-link (`Open trace`) — D-073 traceparent surfacing is post-V1;
  stays disabled-with-tooltip.
- No durable-driver dependency added — the page works against the live stream;
  durable backfill remains an operator config choice.

## Acceptance criteria

- [ ] `/events` renders inside the app shell with NO per-page PageHeader title
  bar; a carded filter strip (faceted chips + saved views + search + pause +
  export), a carded rate sparkline, and a carded events table.
- [ ] The page is viewport-locked: the document never full-page-scrolls; the
  events table scrolls internally behind a sticky header; the right rail scrolls
  internally; the filter strip + sparkline are fixed.
- [ ] The events table fills from the live `events.subscribe` SSE (verified
  live — the three latent reactivity/wiring bugs that left it empty in
  production are fixed); the rate sparkline tracks the live stream; the
  Session facet re-scopes the table; the empty-state copy describes the
  live-stream reality honestly.
- [ ] Clicking a row opens the Event Details right rail (Source / Identity /
  Payload JSON / Quick Actions); with no row selected the rail shows the live
  subscription status (cursor sequence, dropped count, stream state).
- [ ] The Event Details is ONE packed card that scrolls internally (never a
  page-level scroll); the Event Rate card is a per-category multi-line chart +
  a Type / Rate / Total legend; the page packs into one viewport (tightened
  padding, no dead top space) with no full-page scroll even with the detail open.
- [ ] Quick Actions pin facets (type / session / tenant / run); Pause toggles
  the view gate (cursor preserved); Export downloads NDJSON / CSV of the loaded
  page; Save view persists Console-local; admin fan-in is scope-gated and shows
  the cross-tenant audit notice.
- [ ] All four `PageState` branches render (loading / loaded / empty / error)
  plus the disconnected branch.
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` green.
- [ ] `scripts/smoke/phase-108h.sh` 0 FAIL; phase-73g (and the other Console
  smokes) stay green.

## Files added or changed

- `web/console/src/routes/(console)/events/+page.svelte` — rethemed + viewport-
  locked; idle right-rail subscription status; honest empty copy.
- `web/console/src/lib/components/events/EventDetailRail.svelte` — reworked to a
  single packed `.panel.card` (was a stack of `RailCard`s) with a severity
  header + close ✕ + Copy JSON, that fills the right column and scrolls
  INTERNALLY — fixes the multi-card page-level scroll the operator flagged.
- `web/console/src/lib/components/events/EventRateSparkline.svelte` — reworked
  from the stacked-bar to the mock's per-category multi-line chart + a Type /
  Rate / Total legend on the right of the same card (page-events.md §12).
- `web/console/src/lib/events/state.svelte.ts` — §17.6 latent-bug fixes the
  live verification surfaced (the page was effectively non-functional in
  production — the table never showed live events): (1) `subscription` /
  `aggregator` made `$state` so the async `load()` assignment triggers the
  reactive re-read (a plain field left the table stuck on the initial `null`);
  (2) default the subscription `eventTypes` to the full taxonomy when no type
  facet is set — the SSE needs a NAMED listener per type, so an empty list
  received nothing; (3) a derived `displayStatus` that flips empty↔ready on the
  live event count (the plain `status` was set once at load and hid the table
  behind the empty-state while events streamed); (4) the Session facet now
  re-scopes the live table feed (previously only the aggregate). The sparkline
  is re-fetched (throttled) as the cursor advances so it tracks the stream.
- `web/console/src/lib/protocol/client.ts` — `subscribeURL` accepts an optional
  `session` override (the Session-facet table re-scope). Backward-compatible.
- `web/console/src/lib/events/subscription.svelte.ts` — `OpenOptions.session`
  override threaded to `subscribeURL`.
- `web/console/tests/events-page.spec.ts` — Playwright e2e, updated for the
  carded / viewport-locked structure.
- `scripts/smoke/phase-108h.sh` — new static-only guard.
- `docs/plans/phase-108h-console-events-page.md` — this plan.
- `docs/decisions.md` — D-180.
- `docs/design/console/page-events.md` — §13 reframe note.

## Public API surface

N/A — Console-only page-polish phase. No Go public API, no new Protocol method.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

- **Route group + shell.** Stays under `(console)/events/+page.svelte`, served
  at `/events`, inside the one app shell. The breadcrumb, ⌘K search and footer
  are chrome (108b) — never rebuilt per page.
- **`<PageState>` four-state contract.** The page routes its async state through
  `<PageState>` (loading / ready / empty / error) plus the disconnected branch.
- **Shared `ui/` inventory + tokens.** Carded `.panel.card` + `.panel-title`
  vocabulary copied from Overview (108c); design tokens only (stylelint-enforced).
  The rail uses `--size-rail`.
- **HarborClient + connection.ts.** All async state flows through the
  `EventsPageState` controller, which uses `HarborClient` + the typed events
  namespace — no hand-rolled `fetch`.
- **No fabrication (PAGE-POLISH §1).** Every datum is traced to `events.subscribe`
  / `events.aggregate`; the V1 gaps (runtime-side search, trace deep-link) stay
  honest (Console-local search copy; disabled-with-tooltip trace link).
- **Viewport discipline (PAGE-POLISH §6).** The table + right rail scroll
  internally; the chrome never full-page-scrolls; no white bleed.

## Test plan

- **Unit (Vitest):** the existing events lib suites (subscription, aggregate,
  filters, taxonomy, export, sparkline) stay green; add coverage for the
  sparkline-facet-scoping change.
- **Integration (Playwright e2e):** `web/console/tests/events-page.spec.ts` —
  hydration, the carded regions render, the four PageState branches, row-select
  → detail rail, idle rail subscription status, pause toggle, and the
  disconnected shell.
- **Conformance / concurrency:** N/A — no driver / reusable Go artifact.

## Smoke script additions

- `scripts/smoke/phase-108h.sh` (static-only): asserts the page drops PageHeader,
  adopts `panel card`, keeps the `events-page` / event-row / detail-rail testids,
  and renders the idle subscription-status surface. Anchors on imports / testids /
  exported symbols (never bare comment strings).

## Coverage target

`web/console` (front-end): the updated Playwright spec + the unchanged/extended
vitest suites must pass; no Go coverage delta (no Go code touched).

## Dependencies

- Phase 73g / D-125 (the Events page this phase rethemes)
- Phase 60 / 72 (`events.subscribe` SSE) + Phase 72a (`events.aggregate`)
- Phase 108b (app-shell chrome) + Phase 108c (the carded vocabulary copied)
- Phase 108g (the DataTable column-alignment + viewport-lock pattern reused)

## Risks / open questions

- The live table + sparkline are only as populated as the event source. On an
  in-memory event driver a quiet window shows an honest empty table (the SSE
  streams live events going forward; no historical backfill). Surfaced in the
  empty copy, not hidden; durable backfill is an operator config choice.
- Scoping the sparkline aggregate to the active facets must not regress the
  admin cross-tenant fan-in case (the aggregate honors the same scope gate).

## Glossary additions

None — reuses existing vocabulary (`Runtime lens`, `Scope claim`, `Protocol`,
`Fleet observation`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; the subscription carries the triple via the unchanged transport.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no reusable Go artifact built (Console-only page-polish).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — covered by the updated Playwright e2e; no Go seam touched.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-180 records the reframe.
