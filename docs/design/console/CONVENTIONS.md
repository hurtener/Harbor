# Harbor Console — binding design & engineering conventions

> This document is **binding** for every Console page phase (human or AI). It is
> the authority a page phase plan cites in its mandatory "Console consistency"
> section. A page PR that diverges from a convention below is rejected on sight.
> If a convention here conflicts with the RFC, the RFC wins; if it conflicts with
> a page phase plan, this doc wins (it is a cross-cutting convention, like
> `docs/plans/README.md`). See `docs/decisions.md` D-121.

## Why this document exists

The first five Console pages (Tools, Memory, MCP Connections, Artifacts, Flows)
each shipped in isolation. A foundation audit found deep, page-by-page drift:

- **Three conflicting route conventions** — `/tools` and `/memory` and `/flows`
  at the top level, `/console/artifacts` under a `console/` segment,
  `mcp-connections` under a `(console)` group — plus cross-page links that
  pointed at non-existent paths (`/console/tools`, `/tools/[id]`,
  `/sessions/[id]`, `/artifacts/[id]`).
- **No app shell** — no shared sidebar, breadcrumb, identity indicator, or
  footer; each page was its own island.
- **Five incompatible async-state contracts** — every page hand-rolled its own
  loading / empty / error handling, and several conflated "no Runtime attached"
  with "request failed."
- **~13 duplicated UI concepts** — sub-header strips, catalog tables, bulk-action
  toolbars, detail rails, status pills, all forked per page.
- **Five hand-authored Protocol clients** — `ToolsClient`, `MemoryClient`,
  `FlowsClient`, the `mcpApi` object, `HTTPProtocolClient` — each with its own
  error class, its own identity-passing convention, its own `fetch` choke point.
- **Fragmented design tokens** — four border tokens, three rail-width tokens,
  raw hex literals, per-phase comment blocks stamped into `tokens.css`.

This PR lays the shared foundation. A later wave refactors each page's internals
onto it. The nine conventions below are what every future page is built against.

## 1. Routing

All Console pages are **top-level URL segments** hosted under ONE SvelteKit route
group, `web/console/src/routes/(console)/`, whose sole purpose is to attach the
shared app-shell layout. The conventions:

- **No `/console/` URL prefix and no group name in the URL.** The `(console)`
  group is a layout-grouping device only — SvelteKit route groups in parentheses
  do not appear in the URL. A page lives at `(console)/tools/+page.svelte` and is
  served at `/tools`.
- **Detail views are uniform:** `(console)/<page>/[id]/+page.svelte`, served at
  `/<page>/<id>`. The dynamic segment is named `[id]` unless a page has a
  domain-meaningful identifier name already in use (the existing
  `mcp-connections/[server]` and `flows/[flow_id]` predate this doc and are
  grandfathered; new detail routes use `[id]`).
- **Root `/` redirects to `/overview`.** `routes/+page.svelte` issues the
  redirect; `(console)/overview/+page.svelte` is the redirect target.
- **All inter-page links use the unprefixed form** — `/tools`, `/memory`,
  `/flows`, `/artifacts`, `/mcp-connections`, `/sessions/<id>`, etc. A link to
  `/console/<anything>` is a bug. `goto()` calls and `<a href>` both follow this.

## 2. App shell

`(console)/+layout.svelte` is the single app shell every page renders inside. It
provides:

- **A persistent sidebar** listing the full 14-page information architecture in
  four clusters:
  - **Runtime** — Overview, Live Runtime.
  - **Execution** — Sessions, Tasks, Agents, Tools, Events, Background Jobs.
  - **Resources** — Flows, Memory, MCP Connections, Artifacts.
  - **Settings** — Settings.
  - Playground is a **session-level surface**, reached from within a session —
    it is NOT a sidebar entry.
- **A top bar** carrying a breadcrumb (derived from the active route) and an
  identity / connection indicator: the resolved `(tenant, user, session)` triple,
  the Runtime base URL, and a connected / reconnecting / disconnected status dot.
- **A shared footer**, the `ConnectionFooter` component.
- **A content region** that renders the active page (`{@render children?.()}`).

The shell stamps the `console-hydrated` test marker the Playwright harness waits
on (no fixed-timeout synchronisation — CLAUDE.md §17.4).

## 3. Shared component inventory

Reusable UI primitives live in `web/console/src/lib/components/ui/`. They are
built on Skeleton (`@skeletonlabs/skeleton`, the chosen component library —
CLAUDE.md §4.5 rule 4) and reference design tokens only. The inventory:

| Component          | Responsibility                                                                 |
|--------------------|---------------------------------------------------------------------------------|
| `PageHeader`       | Page title + subtitle + an `actions` slot for page-level buttons.               |
| `FilterBar`        | A horizontal bar with slots for saved-view chips, facet chips, search, export.   |
| `SavedViewChips`   | Console-DB-backed saved-filter chips (saved views are Console-local — D-061).    |
| `DataTable`        | A columns-config-driven table: row slot, selection model, built-in empty slot.   |
| `BulkActionBar`    | The action bar shown when ≥1 `DataTable` row is selected.                        |
| `DetailRail`       | The right-hand detail rail container; composes `RailCard`s.                      |
| `RailCard`         | One titled card inside a `DetailRail`.                                           |
| `StatusChip`       | A status pill; a `kind` prop maps over the status token scale.                   |
| `Pagination`       | Page / page-size / total display with prev / next controls.                      |
| `ConnectionFooter` | The shared footer: Runtime URL + connection status.                              |
| `PageState`        | The async-state boundary — owns the four mutually-exclusive states (see §4).     |

Rules:

- **Page-specific components stay in `components/<page>/`** (e.g.
  `components/tools/`, `components/flows/`). The `ui/` directory is for the
  cross-page inventory only.
- **No two components share a name** anywhere in the Console — not across `ui/`
  and `<page>/`, not across two `<page>/` directories. The audit found two
  `CatalogTable.svelte` and two `SubHeaderStrip.svelte`; the refactor wave
  collapses those onto `DataTable` / `PageHeader`.

## 4. The async-state contract

`<PageState>` owns FOUR mutually-exclusive states, rendered as an
`if / else-if / else-if / else` chain. A page never hand-rolls loading / empty /
error markup — it feeds `PageState` and slots its primary view.

1. **Disconnected** — the Console is not attached to a Runtime: `connection.ts`
   returns `null`. `PageState` renders a centered call-to-action: "Not connected
   to a Harbor Runtime — attach one in Settings". This state is **never conflated
   with Error** — an unattached Console is not a failure.
2. **Loading** — a request is in flight. `PageState` renders a **skeleton that
   matches the shape of the primary view** (a table skeleton for a table page, a
   canvas skeleton for a graph page) — never a bare "Loading…" string.
3. **Error** — a `ProtocolError` was thrown. `PageState` renders
   `code: message` PLUS a mandatory **Retry** button that re-invokes the page
   loader. The Error state **suppresses any stale primary view** — a page in
   Error never shows last-good data underneath the error.
4. **Empty** — the request succeeded and returned zero rows. `PageState` renders
   a page-specific message and the page's primary affordance (e.g. an upload
   button on Artifacts).

Detail rails get their own **nested `<PageState>`** — a rail can be Loading
while the primary table is Loaded, and a rail-load failure surfaces in the rail,
not the whole page.

## 5. Depth bar — every Console page MUST clear it

A page is not "done" until it has all of:

- a `PageHeader`;
- a `FilterBar` (even if it carries only a search input);
- a primary `DataTable` or canvas;
- a `DetailRail` or a tabbed detail route;
- Console-DB-backed `SavedViewChips`;
- real `Pagination` (page / size / total, prev / next — not a fake "load more");
- a `ConnectionFooter`;
- the full four-state `PageState`.

**No stubbed action presented as done.** An action either calls the real
Protocol method, or renders **disabled-with-tooltip** explaining why. A button
that pretends to work by emitting a fake feedback string is a §13-class
violation (silent degradation / test stub as production default).

## 6. Typed client layer

One typed client package, `web/console/src/lib/protocol/`:

- **`HarborClient`** — a single class. Method namespaces hang off it:
  `client.tools.list(...)`, `client.memory.health(...)`, `client.flows.run(...)`,
  `client.artifacts.list(...)`, `client.mcp.servers.list(...)`, etc. New page
  surfaces add a namespace, never a new top-level client.
- **`ProtocolClient`** — an injectable interface the `HarborClient` implements,
  so tests (and the Playwright harness) can inject a deterministic in-page
  client.
- **`ProtocolError`** — ONE error class, with uniform `(code, message, status)`.
  `status` is **never dropped** — the audit found one of the five legacy error
  classes silently discarded the HTTP status.
- **One transport choke point** — a single `fetch` call site inside
  `HarborClient`. No `.svelte` component ever calls `fetch` (CLAUDE.md §4.5
  rule 5, §13).
- **`web/console/src/lib/connection.ts`** — one resolver returning
  `{ baseURL, token, identity }` from a single storage convention, or `null`
  when the Console is not attached to a Runtime. Every page resolves its
  connection through this one module; no `.svelte` file reads `localStorage`
  directly.

**Each method targets whatever route the Runtime actually mounts for it.** The
Runtime does not serve a single uniform endpoint shape — `tools.*` mounts at
`POST /v1/tools/{verb}`, `memory.*` at `POST /v1/memory/{verb}`, `flows.*` at
`POST /v1/flows/...`, `artifacts.*` and `mcp.servers.*` at the control surface
`POST /v1/control/{method}`. The namespace methods match the Go side; do not
invent a uniform endpoint the Runtime does not serve.

## 7. Design tokens

`web/console/src/lib/tokens.css` is ONE coherent scale, extended **in place** —
never via per-phase append blocks. Rules:

- **Exactly one hairline-border token** (`--border-hairline`) and **one
  rail-width token** (`--size-rail`). A page references those; it does not
  introduce `--border-thin` / `--border-width-thin` / `--layout-rail-width`
  variants.
- **No raw hex / rgb literals** anywhere except as the base palette definitions —
  semantic and component tokens derive from the base palette via `var(...)`.
- **No phase-stamped comment blocks.** A token is grouped by what it is
  (color / spacing / type / radius / motion / sizing), not by which phase added
  it. `tokens.css` reads as a design system, not a changelog.
- The `.stylelintrc.cjs` rule set mechanically rejects raw color / spacing
  literals in `.svelte` files; `npm run lint` fails CI on a violation.

## 8. Error handling

- Every Protocol call goes through `HarborClient`.
- A non-2xx Runtime response throws a `ProtocolError` carrying
  `(code, message, status)`.
- A page's loader catches the `ProtocolError` and routes it into `PageState`'s
  **Error** state; the **Retry** button re-invokes the loader.
- **Disconnected** — the Console has no Runtime — is detected by `connection.ts`
  returning `null`, and is **distinct from Error**. A page never shows the
  "request failed" Error UI when the real situation is "no Runtime attached."
- No silent degradation: a failed call never resolves to an empty result
  (CLAUDE.md §13).

## 9. Per-phase callout

Every Console page phase plan MUST carry a **"Console consistency"** section that
explicitly cites this document and confirms the page:

- routes under `(console)/` with no `/console/` URL prefix;
- renders inside the app shell;
- uses the `components/ui/` inventory (and does not fork a primitive that already
  exists);
- routes all async state through the four-state `PageState`;
- clears the §5 depth bar;
- talks to the Runtime only through `HarborClient` + `connection.ts`;
- introduces no raw token literals.

A page PR that diverges from these conventions is **rejected on sight**.
