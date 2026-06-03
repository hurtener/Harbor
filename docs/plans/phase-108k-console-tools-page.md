# Phase 108k — Console Tools page (carded, viewport-locked retheme + deepened rail + king-file refactor)

## Summary

Phase 108k brings the Console **Tools** page to its mock with the carded,
viewport-locked, fully-real-wired quality the prior page-polish waves set
(Overview 108c … Background Jobs 108j). The page was already wired in Phase 73f
(the seven `tools.*` methods, the Console-DB saved filters, and the admin verbs)
but pre-chrome: a per-page header, an un-carded vertical stack, and an ~847-line
king file. This phase rethemes it to the Events-108h / Background-Jobs-108j
composition (TABLE-primary on the left and a right-rail detail on the right; no
mode-switch), deepens the rail to full mock fidelity, refactors the king file
into a `ToolsPageState` controller, pure `derive.ts` projections, and focused
components, and deletes the orphaned pre-chrome surface. No new Protocol method —
a pure consumer of the shipped surface.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Tools view": the Tools page is the registered-tool-catalog browser —
  a filterable catalog list with a per-tool detail surfacing schema, examples,
  transport, policy, and source provenance, plus recent invocations and per-tool
  statistics. The 108k rail composes exactly this descriptor set.
- brief 11 §CC-4: tools are slow-moving catalog data, so free-text search is a
  Console-side concern (here a `tools.list` `search` facet), not a runtime-side
  search method — the page never pretends a `search.tools` surface exists.
- brief 11 §PG-3: MCP-sourced tool output flows only through the canonical
  renderer registry — the Content-size card surfaces the negotiated `DisplayMode`
  (RFC §6.5 / D-026) and never a bespoke renderer.
- brief 12 §"The two-surface model": the Console is a Protocol client; every datum
  here is a projection of a canonical `tools.*` method, never a Console-owned
  shadow of the catalog (D-061).

## Findings I'm departing from (if any)

None.

## Goals

- Retheme Tools to the carded `.panel.card`, viewport-locked composition: a
  filter card and a layout of TABLE-primary (left, internal scroll, sticky header)
  and a right-rail detail (right, one packed internally-scrolling card). The
  document never full-page-scrolls.
- Deepen the right rail to full mock fidelity for the selected tool: descriptor
  header (name, transport/scope, side-effect / OAuth / approval badges), a tab
  strip (Manifest | Inputs | Outputs | Recent invocations | Approval), a
  Statistics card, a Content-size & display-mode card, a Source-provenance card,
  and a Run-history strip. With nothing selected the rail shows the catalog
  overview (the view-level aggregate counters) and an idle hint.
- Refactor the ~847-line king file into a `ToolsPageState` controller
  (`lib/tools/state.svelte.ts`), pure `derive.ts` projections, and the focused
  `ToolCatalogTable` / `ToolDetailRail` components, reusing the shared `ui/`
  inventory and the existing body-only stat cards.
- Keep every datum and action real-wired and live-verified (PAGE-POLISH §3/§4);
  keep the four `PageState` branches; render honest states for the genuine V1 gaps.
- Drop the per-page header (breadcrumb / ⌘K / footer are app-shell chrome, 108b);
  delete the orphaned `ToolDetailTabs` surface.

## Non-goals

- No new Protocol method. `tools.invoke` is NOT shipped at V1 (D-132); the rebuilt
  page surfaces no "try this tool" form (operator decision, recorded in D-183).
- No editing / registering / unregistering tools from the Console (§10 carve-out).
- No cross-runtime aggregator (D-091 — post-V1).
- No durable per-tool recent-invocations read-back (the Events surface owns the
  `tool.*` stream); the rail renders an honest pointer instead.

## Acceptance criteria

- [ ] The page is carded (`.panel.card`) and viewport-locked: `height:100%`,
      `overflow:hidden`, the table region `flex:1; min-height:0; overflow:auto`
      with a sticky `<thead>`, the right rail one internally-scrolling card. No
      full-page scroll (`scrollHeight == innerHeight`); no horizontal table overflow.
- [ ] The per-page header is gone; the page root keeps the `tools-page` testid.
- [ ] Catalog rows and the four aggregate counters render real `tools.list` data;
      a facet toggle re-issues `tools.list` and re-renders.
- [ ] Selecting a row opens the rail with real `tools.describe` / `tools.metrics`
      / `tools.content_stats` data; the metrics-window toggle re-fetches metrics.
- [ ] Approve / Reject invoke the real `tools.set_approval_policy` admin method
      (or render disabled-with-tooltip without the `admin` claim); bulk Revoke
      OAuth invokes the real `tools.revoke_oauth`. No fabricated success (§13).
- [ ] All four `PageState` branches render (loading / loaded / empty / error) plus
      the disconnected idle overview with `—` placeholders (not fabricated zeros).
- [ ] The king file is refactored: `ToolsPageState` controller, `derive.ts`
      (with `derive.test.ts`), `ToolCatalogTable`, and `ToolDetailRail`; no monolith.
- [ ] The Save-view contract is preserved (`tools-save-filter` → "Save view";
      `placeholder="Save current as…"`; `DISCONNECTED_TOOLTIP`).
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` (incl. `derive.test.ts`)
      green; the full static-smoke sweep and drift-audit are FAIL-free; the e2e specs
      (tools-page, disconnected-state, wave13, shell-no-regression) pass.

## Files added or changed

```text
web/console/src/routes/(console)/tools/+page.svelte          # rethemed, controller-driven
web/console/src/lib/tools/state.svelte.ts                    # NEW — ToolsPageState controller
web/console/src/lib/tools/derive.ts                          # NEW — pure projections
web/console/src/lib/tools/derive.test.ts                     # NEW — unit tests
web/console/src/lib/components/tools/ToolCatalogTable.svelte  # NEW — DataTable wrapper
web/console/src/lib/components/tools/ToolDetailRail.svelte    # NEW — one packed rail
web/console/src/lib/components/tools/ToolDetailTabs.svelte    # DELETED — superseded by the rail
web/console/tests/tools-page.spec.ts                         # updated for the rebuilt structure
scripts/smoke/phase-108k.sh                                  # NEW — static-only assertions
scripts/smoke/phase-83x.sh                                   # N13 grep retargeted (§17.6)
docs/design/console/page-tools.md                            # §13 reframe note
docs/decisions.md                                            # D-183
```

## Public API surface

N/A — Console-only. No Go Protocol surface added or changed; the page consumes the
shipped seven `tools.*` methods (Phase 73f / D-116) only.

## Test plan

- **Unit:** `web/console/src/lib/tools/derive.test.ts` — the pure projections
  (`lastUsed` zero-time → "never"; `oauthKind` / `approvalKind` / `statusKind`
  exhaustive over the wire enums; `toPageError` keeps the Protocol code;
  `displayStatus` derives ready/empty live from the row count).
- **Integration:** `web/console/tests/tools-page.spec.ts` (Playwright) — route
  serves and hydrates; the catalog table renders the mockup columns; a facet toggle
  re-renders; a row drill-down opens the rail tabs; Approve is wired to the real
  admin method or disabled-with-tooltip; the disconnected redirect. Cross-ref specs
  (disconnected-state N7, wave13, shell-no-regression) re-run unchanged.
- **Conformance:** N/A — no driver.
- **Concurrency / leak:** N/A — the controller owns no long-lived subscription
  (the catalog is polled on demand; `close()` is a no-op).

## Smoke script additions

`scripts/smoke/phase-108k.sh` (static-only) asserts: the route exists; the per-page
header is gone and `ToolDetailTabs.svelte` is deleted; the page keeps the
`tools-page` root, the carded `.panel.card` vocabulary, and composes `ToolsPageState`;
the controller exists and wires the shipped admin writes and exports `ToolsPageState`;
`derive.ts` exports the pure projections; `ToolCatalogTable` renders the catalog rows,
the Reliability column width token, and the scoped DataTable override; `ToolDetailRail`
renders the rail, the five tab keys, and the real admin Approve/Reject; the Save-view
contract (`tools-save-filter` / "Save view" / "Save current as…" / `DISCONNECTED_TOOLTIP`)
is preserved.

## Coverage target

`web/console/src/lib/tools/`: the pure `derive.ts` is unit-covered by `derive.test.ts`
(the controller and `.svelte` views are exercised by the Playwright specs — the
Console has no Go-style line-coverage gate; the bar is the §0 acceptance, the e2e,
and the static smoke).

## Dependencies

- Phase 73f (the seven `tools.*` methods and the page's first wiring) — Shipped.
- Phase 108b (the app-shell chrome that owns the breadcrumb / ⌘K / footer) — Shipped.
- Phase 108h / 108j (the carded table + right-rail-detail pattern this mirrors) — Shipped.

## Risks / open questions

- The mock's "Try this tool" developer form depends on a `tools.invoke` method that
  is NOT shipped at V1 (D-132). The live probe confirmed only the seven canonical
  `tools.*` methods exist. Per operator sign-off the rebuilt page omits the
  affordance (D-183) rather than carrying a disabled stub; the `tools.invoke`
  deferral itself is unchanged.
- `tools.metrics` / `tools.content_stats` are all-zero / empty for never-invoked
  tools (the live probe confirmed this for every catalog tool in the validation
  agent). The stat cards render honest "no invocations yet" copy, never a
  fabricated rate/latency.

## Glossary additions

None — no new vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no identity-scoped storage path changed (the page is a read lens; `tools.list` is identity-scoped server-side via the X-Harbor-* headers, unchanged).
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, Console UI; no Go reusable artifact.
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — the Playwright `tools-page.spec.ts` exercises the page end-to-end against the live Runtime surface.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure).
