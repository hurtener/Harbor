# Phase 108m — Console MCP Connections page

## Summary

Phase 108m brings the Console **MCP Connections** page to the Phase 108
page-polish acceptance bar (`docs/design/console/PAGE-POLISH-PROCEDURE.md`):
verbatim-vs-mock parity, every datum wired to real Protocol data and verified
against a live Runtime, every interactive element performing its real action,
all four `PageState` branches, a viewport-locked shell, and a Playwright
browser-truth pass. It rethemes the page to the carded, viewport-locked
master-detail composition (the servers **table** on the left + a right-rail
server detail on the right — the Tools-108k pattern), deepens the rail to full
mock fidelity (header + five tabs + live Recent-events card + binding-scope
summary), and refactors the king file into `McpListState` / `McpDetailState`
controllers + a pure, unit-tested `derive.ts`. It is a **pure Protocol
consumer** — the `mcp.servers.*` surface shipped in Phase 73k / D-119; this
phase builds NO new Protocol method.

## RFC anchor

- RFC §6.4 (Tool catalog and transports — the MCP southbound surface)
- RFC §7 (Console layer — the runtime-lens Protocol-client principle)

## Briefs informing this phase

- brief 11 (Console feature surface — §"MCP Connections view", §PG-3, §"Open architectural questions" #8)
- brief 12 (Console deployment and shared UI — §"The two-surface model")

## Brief findings incorporated

- brief 11 §"MCP Connections view": MCP servers are slow-moving catalog data,
  so free-text search is a Console-side index over the loaded page (no extra
  Protocol round-trip) — `McpListState.visibleServers` narrows Console-side.
- brief 11 §"Open architectural questions" #8: raw HTML / SVG fragments are
  **default-deny**; the per-server trust toggle is admin-gated and audited.
  The rail's `raw-html-toggle` is disabled-with-tooltip without the `admin`
  claim (D-079) and calls the real `mcp.servers.set_raw_html_trust` — never a
  fabricated success.
- brief 11 §PG-3 (MCP-Apps renderer registry): no bespoke per-server renderer;
  the Resources tab surfaces advertised resources read-only and defers
  rendering to the canonical registry — this page only inventories them.
- brief 12 §"The two-surface model": the Console is a Protocol client — every
  datum round-trips through `mcp.servers.*` / `tools.list` / `events.subscribe`;
  the Console DB holds only the Console-local saved-view chips (D-061).

## Findings I'm departing from (if any)

None.

## Goals

- Retheme the page to the carded (`.panel.card`), viewport-locked master-detail
  composition: a filter card (saved-view chips + state facets + search) + a
  servers table that scrolls internally behind a sticky `<thead>` + a right-rail
  server detail (or the catalog-overview idle state when nothing is selected).
- Deepen the detail rail to full mock fidelity: header (name + state badge +
  transport + endpoint + last-discovery + tool/resource/prompt/OAuth counts +
  Refresh discovery / Test connection / raw-HTML toggle), a five-tab strip
  (Tools | Resources | Prompts | OAuth bindings | Policy), a LIVE Recent-events
  card, and a binding-scope summary.
- Refactor the king file into `McpListState` + `McpDetailState` controllers + a
  pure `derive.ts` (folding in the former `status.ts`) + focused
  `ServersTable` / `McpDetailRail` / `McpOverviewCard` / `McpRecentEvents`
  components. Replace the separate tabbed-detail route with the rail (§13 — no
  two parallel detail surfaces).
- Wire every action real + honest: Refresh discovery re-reads the runtime;
  Test connection shows the actual probe outcome; the raw-HTML toggle + OAuth
  Connect/Revoke are admin-gated and audited; the Recent-events card is a
  LIVE-only `events.subscribe` projection that is honestly empty until events
  stream in.

## Non-goals

- No new Protocol method — the `mcp.servers.*` surface shipped in Phase 73k.
- No add/remove of MCP servers from the Console (config + restart only — §10).
- No MCP-Apps content rendering (the 109a–c wave owns the sandboxed host).
- No Health tab — the five-tab strip is Tools | Resources | Prompts | OAuth
  bindings | Policy (the `mcp.servers.health` method stays available but is not
  consumed by this page).

## Acceptance criteria

- [x] The page is carded + viewport-locked: `scrollHeight == innerHeight`, no
      horizontal overflow at 1512×945 (verified live).
- [x] The per-page `PageHeader` is gone (breadcrumb / ⌘K / footer are 108b chrome).
- [x] The servers table renders the real catalog (`mcp.servers.list`); clicking
      a row populates the right-rail detail.
- [x] The rail header + counts + policy + tabs are real-wired (`mcp.servers.get`
      / `resources` / `prompts` / `bindings.list` / `policy` + `tools.list`).
- [x] Refresh discovery re-reads the runtime (header "last connect" updates);
      Test connection shows the real probe outcome (`Reachable — round-trip N ms`).
- [x] The raw-HTML toggle calls the real admin method; disabled-with-tooltip
      without the `admin` claim (D-079).
- [x] The Recent-events card is a LIVE `events.subscribe` projection
      (`mcp.resource_updated` + `tool.auth_required` + `tool.failed`),
      honest-empty when quiet, attributed to the server (no fabrication, §13).
- [x] All four `PageState` branches render (loading / loaded / empty / error)
      + the disconnected → `/settings` redirect (Phase 105).
- [x] Zero browser console errors on a clean load; `npm run check` 0/0,
      `npm run lint` clean, the unit + e2e suites green.

## Files added or changed

- `web/console/src/routes/(console)/mcp-connections/+page.svelte` — rebuilt (carded master-detail).
- `web/console/src/routes/(console)/mcp-connections/[server]/+page.svelte` — REMOVED (rail is the single detail surface).
- `web/console/src/lib/mcp-connections/state.svelte.ts` — extended controllers (`McpListState` + deepened `McpDetailState`).
- `web/console/src/lib/mcp-connections/derive.ts` — NEW pure projections (folds in the former `status.ts`).
- `web/console/src/lib/mcp-connections/status.ts` — REMOVED (folded into `derive.ts`).
- `web/console/src/lib/components/mcp-connections/ServersTable.svelte` — NEW.
- `web/console/src/lib/components/mcp-connections/McpDetailRail.svelte` — NEW.
- `web/console/src/lib/components/mcp-connections/McpOverviewCard.svelte` — NEW.
- `web/console/src/lib/components/mcp-connections/McpRecentEvents.svelte` — NEW.
- `web/console/src/lib/components/mcp-connections/StateFacetChips.svelte` — import re-pointed to `derive.ts`.
- `web/console/src/lib/mcp-connections/tests/derive.test.ts` — NEW unit suite (real-payload-pinned).
- `web/console/tests/mcp-connections-page.spec.ts` — updated for the rebuilt structure.
- `scripts/smoke/phase-108m.sh` — NEW static Console-side guard.
- `docs/plans/phase-108m-console-mcp-connections-page.md` — this plan.
- `docs/decisions.md` — D-185.
- `docs/design/console/page-mcp-connections.md` — §13 reframe note.

## Public API surface

N/A — Console-only. No Go / Protocol surface changes (a pure consumer of the
shipped `mcp.servers.*` + `tools.list` + `events.subscribe` surfaces).

## Test plan

- **Unit:** `web/console/src/lib/mcp-connections/tests/derive.test.ts` (Vitest) —
  the `mcpStatusKind` / `mcpStateLabel` mappings exhaustive over the wire enum;
  `relativeTime` renders the Go zero time as `never`; `serverStateCounts` rolls
  a page up per-state; `displayStatus` derives ready/empty live from the row
  count (D-180); `extractEventSource` / `extractEventToolName` read the REAL
  PascalCase SSE payload fields (§3 casing gotcha); `summarizeMcpEvent` +
  `projectServerEvents` attribute resource/auth events by `Source` and
  `tool.failed` by owned tool name, dropping (never mislabelling) unattributable
  events. The existing `tests/state.svelte.spec.ts` (four-state contract +
  control-surface routing) stays green against the extended controllers.
- **Integration:** N/A — Console-only; no Go cross-subsystem seam. The
  end-to-end seam is covered by the Playwright `mcp-connections-page.spec.ts`
  against the live Runtime + `harbor console`.
- **Conformance:** N/A — no driver / Protocol shape added.
- **Concurrency / leak:** N/A — no reusable Go artifact; the live subscription
  is closed on page unmount (`McpDetailState.close()`).

## Smoke script additions

`scripts/smoke/phase-108m.sh` (static-only): the `[server]` route is removed +
the `PageHeader` is gone + the carded vocabulary + the page-root / search /
empty / save-view testids + the `McpListState` / `McpDetailState` controllers +
`derive.ts` (with `status.ts` folded in) + the four focused components + the
five tabs (MCP_DETAIL_TABS) + the real `refresh-discovery` / `test-connection` /
`raw-html-toggle` wiring + the `isAdmin` admin gate + the live `EventsSubscription`
and the Save-view N7 contract + the no-hand-rolled-fetch rule.

## Coverage target

- `web/console/src/lib/mcp-connections/`: the pure `derive.ts` projections are
  unit-covered (15 cases); the controllers' four-state + routing paths are
  covered by the existing 8-case `state.svelte.spec.ts`.

## Dependencies

- Phase 73k / D-119 (the `mcp.servers.*` Protocol surface this page consumes).
- Phase 73f / D-116 (`tools.list` — the per-server Tools tab + event attribution).
- Phase 73g / D-125 (`events.subscribe` SSE + `EventsSubscription`).
- Phase 108b (the app-shell chrome) + Phase 108k / D-183 (the carded
  viewport-locked master-detail pattern + the controller refactor it mirrors).
- Phase 105 (the disconnected → `/settings` redirect).

## Risks / open questions

- `tool.failed` carries no server field, so the Recent-events card attributes it
  by membership in the server's owned tool-name set (`tools.list` `owner ===
  name`). An event that cannot be attributed is dropped, never mislabelled. The
  validation runtime's youtube tools never failed during verification, so the
  card was honestly empty — the attribution logic is pinned by the unit suite
  against the real PascalCase payload shape.
- The detail rail takes the `McpDetailState` controller instance as a prop
  (the page owns one instance) rather than fully prop-drilling its ~20 fields —
  page-specific coupling sanctioned by the controller godoc ("components read it
  and call its actions").

## Glossary additions

None — the MCP vocabulary (`mcp.servers.list`, the raw-HTML trust toggle,
`mcp.raw_html_trust_toggled`, `auth.BindingScope`, `tool.auth_required`) already
lives in `docs/glossary.md` from Phase 73k / Phase 30.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A (Console-only; identity flows through the shared `HarborClient`).
- [x] **If this phase builds a reusable artifact:** N/A — no Go reusable artifact; the Console controllers are per-page instances, the live subscription is closed on unmount.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** the end-to-end seam (Console ↔ `mcp.servers.*` / `events.subscribe`) is covered by `mcp-connections-page.spec.ts` against the live Runtime; the controller four-state + routing is unit-covered. No Go integration test (Console-only).
- [x] If new vocabulary: glossary updated — N/A (no new terms).
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure).
