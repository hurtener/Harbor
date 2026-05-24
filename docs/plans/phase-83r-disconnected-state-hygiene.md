# Phase 83r — disconnected-state-hygiene

## Summary

Closes Walkthrough Bugs **W1**, **W2**, **W3** + Nits **N4**, **N5**,
**N8**, **N9**, **N10** from the post-83k walkthrough. Standardizes
the disconnected state across every Console page so:

- Action buttons + filter controls visually + functionally disable
  when no Runtime is attached (W2, W3).
- No card renders synthetic "$0.00 / 0 / —" data when disconnected;
  every card shows the same disconnected placeholder (W1).
- Each page surfaces ONE disconnected message (not two stacked ones —
  N5).
- Empty-state metric chips use one format (W1-adjacent / N4).
- Status chips drop their state colors when disconnected (N8).
- Page subtitles update to "no Runtime attached" instead of "— 0
  artifacts" / "0 — 0 of 0" when disconnected (N9).
- The empty-state placeholder centers vertically in the viewport
  rather than hugging the top (N10).

## RFC anchor

- RFC §5 — Console as Protocol client (consistent disconnected
  state).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §3 — Disconnected is a first-class state, not a
  degradation. Treat it deliberately on every page.
- brief 12 §2 — The shared `<PageState>` boundary is the single
  contract every page routes through; per-page filter / action rows
  must agree with the boundary's disconnected verdict.

## Findings I'm departing from (if any)

None.

## Goals

- Every page with action buttons (Refresh / Apply / Export / Pause
  stream / Save view / etc.) renders those buttons in a `disabled`
  visual state when `<PageState>` is in the `disconnected` branch.
  Add `title="Attach a Runtime to enable"` (or similar) on each
  disabled button.
- Every page with filter controls (chips, dropdowns, search inputs)
  routes through the same `disconnected` predicate. Filter controls
  may stay visible but become non-interactive when disconnected
  (greyed-out + non-clickable).
- The Overview page's "Cost Rollup" card stops rendering synthetic
  `$0.00 · No cost recorded` data when disconnected — it follows the
  same "Not connected" placeholder as the other three cards on the
  page (W1).
- The Live Runtime page's Compose panel (textarea + Start / User
  message / Redirect / Inject context / Pause / Resume / Cancel)
  routes through the disconnected predicate — disabled buttons +
  disabled textarea + a banner / tooltip explaining why (W2).
- The Tools page stops stacking two empty messages ("Not connected"
  AND "Select a tool from the catalog") in the disconnected state —
  only the first renders (N5).
- The Agents page's KPI cards (Active agents / Running tasks / Total
  cost / Total tokens) use `—` (single em-dash) consistently — match
  the Tools page's pattern (N4).
- The MCP Connections page's status chips desaturate (or hide) when
  the Console is disconnected — the colors are meaningless without
  a backing Runtime (N8).
- The Artifacts page's subtitle "— 0 artifacts" reads "— no Runtime
  attached" in the disconnected state (N9).
- The empty-state placeholder on each page centers vertically in
  the main column rather than hugging the top (N10).

## Non-goals

- Saved-views label drift + duplicate footer dedup — those are 83s.
- Sidebar nav fixes (Playground) — those are 83q.
- Per-page mock visual upgrades (different empty-state copy per
  page) — out of scope; this phase is a CONSISTENCY pass, not a
  design pass.
- Detail pages (`/sessions/[id]`, `/agents/[id]`, etc.) — focus on
  the 14 top-level list pages.

## Acceptance criteria

- [ ] Each page's action buttons + filter controls reach a single
      predicate (likely `connectionStore.status === 'disconnected'`
      or equivalent) that drives a `disabled` attribute + ARIA
      handling.
- [ ] The Overview Cost Rollup card no longer renders synthetic data
      when disconnected.
- [ ] The Live Runtime Compose panel routes through the disconnected
      predicate (textarea disabled, buttons disabled, tooltip on
      hover).
- [ ] The Tools page renders ONE empty-state message in the
      disconnected state (not two).
- [ ] The Agents KPI cards use `—` (matching Tools).
- [ ] The MCP Connections status chips render desaturated (or
      hidden) in the disconnected state.
- [ ] The Artifacts subtitle reads "— no Runtime attached" when
      disconnected.
- [ ] Every page's empty-state placeholder centers vertically in
      the main column (uses `align-items: center` on the wrapping
      flex/grid OR an equivalent CSS pattern).
- [ ] `scripts/smoke/phase-83r.sh` asserts the surface-level changes.

## Files added or changed

The agent will touch one or more of the following per-page Svelte
files (the exact list depends on per-page structure):

- `web/console/src/routes/(console)/overview/+page.svelte`
- `web/console/src/routes/(console)/live-runtime/+page.svelte` (or
  its compose-panel component)
- `web/console/src/routes/(console)/sessions/+page.svelte`
- `web/console/src/routes/(console)/tasks/+page.svelte`
- `web/console/src/routes/(console)/agents/+page.svelte`
- `web/console/src/routes/(console)/tools/+page.svelte`
- `web/console/src/routes/(console)/events/+page.svelte`
- `web/console/src/routes/(console)/background-jobs/+page.svelte`
- `web/console/src/routes/(console)/flows/+page.svelte`
- `web/console/src/routes/(console)/memory/+page.svelte`
- `web/console/src/routes/(console)/mcp-connections/+page.svelte`
- `web/console/src/routes/(console)/artifacts/+page.svelte`
- Shared `<PageState>` or a new helper for the disabled-predicate +
  vertical-centering CSS.
- `docs/plans/README.md` — Phase 83r row.
- `docs/decisions.md` — D-160.
- `docs/glossary.md` — `Disconnected predicate` entry.
- `docs/plans/phase-83r-disconnected-state-hygiene.md` — this plan.
- `scripts/smoke/phase-83r.sh` — static-surface assertions.

## Public API surface

If a new shared helper lands (e.g. `useDisconnectedPredicate()`),
it's a Console-internal helper, not a Protocol surface.

## Test plan

- **Unit:** N/A.
- **Integration:** Playwright tests assert action buttons are
  disabled when no Runtime is seeded; the Overview Cost Rollup
  card no longer shows `$0.00` in disconnected state; the Tools
  page renders one empty message not two.
- **Manual:** boot `harbor console`, walk every page, verify each
  acceptance criterion holds.

## Smoke script additions

`scripts/smoke/phase-83r.sh` asserts:

- The shared helper (or its inline equivalent) exists.
- Each per-page file references the disabled predicate.
- The Cost Rollup card no longer hardcodes `$0.00`.
- The Tools page has one (not two) empty-state messages.

## Coverage target

- Existing Console coverage gates apply.

## Dependencies

- Phase 73m, 73p (Console foundation).
- Phase 83p (Settings two-group layout — the model for how to handle
  "this page works disconnected, that one doesn't").

## Risks / open questions

- **Scope creep into design changes.** The agent must NOT redesign
  empty states — only standardize the existing disconnected pattern.
  If the agent surfaces a new design issue, file it as a follow-up.
- **Test fixture work.** Some Playwright tests seed a connection
  before navigating; the new disconnected-state tests must seed
  auth but NOT the connection triple (the pattern in
  `settings-page.spec.ts::(f)` test).

## Glossary additions

- **Disconnected predicate** — Phase 83r. The shared Console-side
  test (typically a derived value off `connectionStore`) that every
  page's action buttons + filter controls + synthetic-data cards
  route through. When true, controls render disabled + cards stop
  showing synthetic values + the page emits one consolidated
  disconnected placeholder. Closes the W1/W2/W3 cluster from the
  post-83k visual walkthrough.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — Playwright extensions
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
