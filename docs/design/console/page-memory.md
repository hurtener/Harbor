# Console page — Memory

**Slug:** `memory` &middot; **Sidebar cluster:** Resources &middot; **Route:** `/console/memory`
**Mockup:** `docs/rfc/assets/console-memory-page.png` (canonical, 2026-05-18)

## 1. Purpose

Memory is the per-identity inspector for the runtime's memory subsystem. Memory in Harbor is session-scoped by default (CLAUDE.md §6 rule 4) — cross-session promotion (user-level, tenant-level) requires an explicit declared policy with audit. The page answers: "what's in this session's working memory right now?", "why did the memory strategy select these items for the planner's last step?", "did a cross-session promotion happen, and was it audited?", "is the memory driver healthy?". The page is an inspector — read-mostly with admin-only mutating actions (add / edit / evict) for debugging and recovery.

## 2. Where it sits in the IA

Memory sits second under the **Resources** cluster (Resources → Flows, Memory, MCP Connections, Artifacts). The operator reaches it from the sidebar, from an Agent detail's Memory-tab "Inspect memory" link, from a Session detail's "Memory" sub-panel link (post-V1), or from the global search palette. Breadcrumb: `<runtime> / Memory` (default identity-scope filter view).

## 3. Functionality matrix

- **Identity-scoped browser — filter by tenant / user / session / agent; the visible scope respects the JWT (only sees what the JWT scope allows).** `[wave-13-extends]` Requires `memory.list` Protocol method (NEW) returning per-item snapshot for the requested identity scope. The runtime backs this on the `MemoryStore` interface (Phases 23–25 shipped). The Protocol projection is NEW — currently no Console-facing memory method.
- **Memory items list — per-item: content (post-redaction), TTL, created, last-accessed, scope (session / user / tenant), driver (`inmem` / `sqlite` / `postgres`).** `[wave-13-extends]` `memory.list` payload.
- **Memory strategy debugger — render how a strategy (Phase 24) selected items for a given session for a given step (the selection trace, the rejected items, the why).** `[wave-13-extends]` Requires `memory.strategy_trace` Protocol method (NEW) returning the strategy's input / output / discard reasons for a given run-step. Brief 11 §"Memory view" calls this out as a feature; it's a complex projection.
- **Filter bar — identity (tenant / user / session), scope (session / user / tenant), driver, has-TTL-expiring, content-search.** `[wave-13-extends]` `memory.list` query payload.
- **Memory health indicator.** `[shipped]` Subscribe to `memory.health_changed` (`EventTypeMemoryHealthChanged`) + `memory.identity_rejected` (`EventTypeMemoryIdentityRejected`) + `memory.recovery_dropped` (`EventTypeMemoryRecoveryDropped`).
- **Identity-rejection log — recent `memory.identity_rejected` events (the runtime fails-loud on missing identity per D-033; this surface makes those rejections visible).** `[shipped]` Subscribe to `memory.identity_rejected`.
- **Recovery-dropped indicator (Phase 25 / D-035).** `[shipped]` Subscribe to `memory.recovery_dropped`.
- **Manual add — admin-only; add a memory item under a specific identity scope.** `[wave-13-extends]` `memory.put` Protocol method (NEW). Admin scope required; fully audited; identity is mandatory.
- **Manual edit — admin-only; replace an existing item.** `[wave-13-extends]` `memory.put` (idempotent on identity + key) Protocol method.
- **Manual evict — admin-only; remove a memory item.** `[wave-13-extends]` `memory.delete` Protocol method (NEW).
- **Cross-session promotion viewer — list of explicit declared promotions (session → user / user → tenant), each with the audit trail.** `[wave-13-extends]` `memory.promotions` Protocol method (NEW). The page surfaces the declared promotion policies; memory promotion itself is a runtime concern.
- **Driver-comparison rollup — when a tenant uses different drivers per scope (in-mem for session, postgres for tenant-level), surface the active per-scope driver.** `[wave-13-extends]` Derived from `memory.list` (NEW) payload's `driver` field.
- **Export memory snapshot (JSONL).** `[shipped]` Client-side aggregation.
- **No Priority field rendered.** `[deferred]` D-065 invariant preserved.
- **Saved filter chips.** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page):
  - Row 1 — identity-scope filter (tenant / user / session) + scope-radio (session / user / tenant) + content-search box.
  - Row 2 — saved-filter chips + driver-comparison rollup chip.
  - Row 3 — three-tab strip: Items | Strategy trace | Promotions.
  - Row 4 — selected tab content (virtualised items list, strategy trace tree, promotions log).
- **Right rail** (per-page): Memory health card (driver state + recent identity-rejection / recovery-dropped counts) + selected-item detail (when an item is selected, render its full post-redaction content + metadata).
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Identity-scope filter | local UI state → `memory.list` query (identity-bound) | Apply / Clear | `[wave-13-extends]` |
| Content-search box | `memory.list` query payload (content substring) | Submit | `[wave-13-extends]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Items tab | `memory.list` (NEW) | Click row → right-rail detail; Edit / Evict (admin only) | `[wave-13-extends]` |
| Strategy trace tab | `memory.strategy_trace` (NEW) | Click step → expand trace tree | `[wave-13-extends]` |
| Promotions tab | `memory.promotions` (NEW) | Click promotion → audit detail | `[wave-13-extends]` |
| Memory health card (right rail) | `memory.health_changed` / `memory.identity_rejected` / `memory.recovery_dropped` events | Click → Events page filtered to type | `[shipped]` |
| Selected-item detail (right rail) | `memory.list` selected row | Copy content (local); Edit (admin → `memory.put`); Evict (admin → `memory.delete`) | `[wave-13-extends]` |
| Add memory composer | `memory.put` (NEW) (admin) | Submit | `[wave-13-extends]` |
| Driver-comparison rollup chip | derived from `memory.list` (NEW) payload | Click → show per-scope driver detail (local UI state) | `[wave-13-extends]` |
| Export snapshot | `memory.list` (NEW) aggregated client-side | Submit → file download | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar:** identity-scope filter; scope-radio (session / user / tenant); content-search; saved-filter chips; "Add memory" button (admin-only).
- **Row-action (Items list):** click → right-rail detail; right-click → Edit / Evict (admin); Copy content.
- **Tab-action (Strategy trace):** expand / collapse trace nodes.
- **Tab-action (Promotions):** click → audit trail detail.
- **Keyboard shortcuts:** `g m` Memory; `j` / `k` next / previous item; `Enter` open right-rail detail; `Esc` clear selection; `/` focus content-search.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty memory | No items for the scope | Empty-state: "No memory items in this scope" + "Open Live Runtime" CTA (memory builds up during runs) | Visit Live Runtime |
| Filtered empty | Filters / search yield zero | "No items match" + Clear | Clear |
| Initial loading | `memory.list` in flight | Skeleton rows | Auto |
| Identity-rejection log spike | `memory.identity_rejected` rate spiked | Banner: "<N> identity-rejected events in last <window> — check driver health" | Investigate via right-rail health card |
| Recovery-dropped alert | `memory.recovery_dropped` event observed | Banner: "Memory recovery dropped items — see right rail" | Investigate |
| Protocol error — `CodeNotFound` on item | Item evicted concurrently | Inline error; refresh list | Refresh |
| Protocol error — `CodeScopeMismatch` on Edit / Evict / Add | Operator submitted without admin scope | Hide controls; on submission, inline error | Request admin scope |
| Protocol error — `CodeIdentityRequired` on `memory.put` | Identity tuple incomplete in composer | Inline error: "Identity is mandatory — choose tenant / user / session" | Provide identity |
| Protocol error — `CodeAuthRejected` | JWT expired | Banner + re-auth | Re-enter passphrase |

## 8. Multi-tenant / multi-runtime nuances

Memory is the page where multi-isolation discipline is most visible. Default scope renders memory items only for the operator's own `(tenant, user, session)` per CLAUDE.md §6 rule 4. With `admin`, the identity-scope filter widens to other identities in the same tenant; `console:fleet` widens further to cross-tenant memory (with `audit.admin_scope_used` server-side emit). The page MUST NOT allow a non-admin operator to inspect another user's memory — even through a manually-typed identity filter; the runtime enforces this server-side and the Console UI gates the filter widgets. In multi-runtime mode, the runtime switcher swaps the entire memory view — memory is per-runtime; cross-runtime aggregator is post-V1 per D-091.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — inspect / list own session's memory only.
- `admin` (`auth.ScopeAdmin`) — widen the identity-scope filter to other users / sessions in the same tenant; required for Edit / Evict / Add operations.
- `console:fleet` (`auth.ScopeConsoleFleet`) — cross-tenant memory inspection (rare; used for cross-tenant audit).
- **Control / mutating verbs (Add / Edit / Evict)** require admin scope — these are NOT control-plane verbs in the D-066 sense (they're not fleet control), but they mutate runtime state and require strictly more than the read scope.

## 10. Out of V1 (deferred)

- **Cross-runtime memory aggregator.** D-091 — post-V1.
- **Memory-strategy editor in the Console.** Out of V1 — strategy authoring is a runtime configuration concern, not a Console action. Brief 11 §"Memory view" notes the debugger view; not the editor.
- **TTL-based bulk eviction UI.** Out of V1 — TTLs evict automatically per Phase 23/24 contracts; manual bulk eviction is post-V1.
- **Memory-export-to-evaluations.** D-064 — Evaluations is post-V1.
- **Priority field rendered anywhere.** D-065 invariant preserved.

## 11. References

- Brief 11 §"Memory view".
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §6.6 (Memory subsystem), §7 (Console).
- Decisions: D-033 (memory identity-rejection emits `memory.identity_rejected` with `<missing>` substitution), D-034 (persistent memory drivers own their `memory_state` tables), D-035 (`OverflowDropOldest` only; recovery loop bounded), D-061 (Console DB local-only), D-065 (no session priority — invariant), D-066 (control claim).
- Phase plan: phase 23 (MemoryStore iface + InMem + conformance — `Shipped`), phase 24 (Memory strategies — `Shipped`), phase 25 (SQLite + Postgres memory drivers — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `Console`, `Runtime lens`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-memory-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Sub-header strip.** Saved-filter chips on the left (`Saved filters`, `By agent`, `Expiring soon`, `Identity-rejected`, `Last hour`) + faceted filter chips (`Agent` ▾, `Scope` ▾ — `session` / `user` / `tenant` — `Session` ▾, `Tenant` ▾, `Driver` ▾ — `inmem` / `sqlite` / `postgres` — `More filters` ▾). Right side: `Refresh`, `Export ▾` (NDJSON / CSV — Console-local snapshot of current filtered page).
- **Strategy / overlay chip row (immediately below sub-header).** Color-coded chips for memory strategies (`Episodic`, `Recent`, `Pinned`, `Persistent`, `Working set`); selecting one applies an overlay filter. These render the V1 strategy taxonomy from Phase 24; chips for unshipped strategies are absent (no placeholder UI).
- **Main memory table (primary surface).** Columns in mockup order: checkbox / **Name / Memory key** (truncated key + copy-on-hover) / **Strategy** chip / **Scope** chip / **Owner** (identity triple summary — agent + session + user) / **Created** (relative timestamp) / **Last updated** (relative timestamp) / **TTL / Expires** (relative; `—` when none) / **Size** (bytes; heavy-content threshold per D-026 flagged via icon) / **Driver** chip / row-action menu. Virtualised; pagination footer.
- **Right rail — Stacked status cards.**
  - **Memory health** — counters: total records, expiring in 1 h, identity-rejected count (last 24 h per D-033), `OverflowDropOldest` events (last 24 h per D-035). Read-only.
  - **Recent identity rejections** — list of `memory.identity_rejected` events with the `<missing>` substitution per D-033; deep-links into the Events page with the filter pre-applied.
  - **Recovery dropouts** — `OverflowDropOldest` event timeline per D-035, scoped to the active tenant filter. Read-only.
  - **Selected item detail** — when a row is checked: full key, full identity quadruple, complete metadata (strategy, scope, TTL, size, driver), pretty-printed JSON value viewer with collapsible nodes and a `Truncated` badge + `Open artifact` link when size ≥ heavy-content threshold (D-026). Row actions: `Copy key`, `Copy value`, `Inspect related events` (deep-links to Events page).
- **Bulk-action toolbar.** Activates when ≥1 row is checked. **V1 is read-only**: bulk actions present in the mockup (`Delete selected`, `Refresh TTL`, `Pin`) render as disabled-with-tooltip ("Memory mutation surface deferred — Phase 73") consistent with §3 functionality matrix item "Direct mutation (delete a record, edit a value, force TTL)" marked deferred.
- **Footer.** `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Saved-filter chips (`By agent`, `Expiring soon`, `Identity-rejected`, `Last hour`) | Console-local saved views (D-061) | Apply / pin / unpin | `[Console-local]` (D-061) |
| Faceted filter chips (Agent / Scope / Session / Tenant / Driver / More filters) | `memory.list` filter params | Toggle facet | `[wave-13-extends]` (`memory.list` filter shape) |
| Strategy / overlay chip row | `memory.list?strategy=…` | Pin strategy filter | `[wave-13-extends]` (`memory.list` strategy filter) |
| Export ▾ (NDJSON / CSV of filtered page) | Already-loaded page | Client-side export | `[Console-local]` (D-061; no Protocol mutation) |
| Memory health card | `memory.health` aggregates | None (read-only) | `[wave-13-extends]` (`memory.health` aggregate method TBD) |
| Recent identity rejections card | `memory.identity_rejected` events (D-033) | Deep-link to Events page | `[wave-13-extends]` (event-stream filter; event itself shipped per D-033) |
| Recovery dropouts card | `memory.overflow_drop_oldest` events (D-035) | None | `[wave-13-extends]` (event-stream filter; event itself shipped per D-035) |
| Selected item detail (JSON viewer + truncation handling) | `memory.get` response | Copy / Inspect related events | `[wave-13-extends]` (`memory.get` Protocol method TBD) |
| Bulk-action toolbar (Delete / Refresh TTL / Pin — disabled-with-tooltip in V1) | Selected row IDs | None at V1 (deferred per §10) | `[deferred-post-V1]` (memory mutation surface — Phase 73 carve-out) |
| Open-artifact link on heavy-content values | `artifacts.get` for value blob | Open artifact viewer | `[wave-13-extends]` (`artifacts.get` — already shipped surface; memory subsystem must produce artifact stubs for heavy values per D-026) |

### No mockup violations of binding carve-outs

- **D-033 (identity-rejection emits `memory.identity_rejected`).** The Recent identity rejections card surfaces exactly that event with the `<missing>` substitution; the Console never hides the rejection or substitutes a partial identity itself.
- **D-034 (persistent memory drivers own their `memory_state` tables).** The Driver column and faceted filter expose the driver per row; no Console-side shadow of memory state.
- **D-035 (`OverflowDropOldest`).** The Recovery dropouts card surfaces the event; the Console never inflates the in-memory buffer beyond runtime bounds.
- **D-061 (Console DB local-only).** Saved filters, sort preferences, column visibility, and Export are Console-local. The mockup never persists a Protocol-mutating shadow of memory records.
- **D-065 (no session-level priority).** No priority field renders on rows or in the right rail; the `Pinned` strategy chip is a Phase 24 strategy, not a priority field.
- **D-066 (control-scope claims).** All mutation surfaces are disabled-with-tooltip at V1; observation requires the `memory.read` scope; cross-tenant inspection requires `memory.crosstenant` per D-079 and gates the `Tenant ▾` facet list to scope-authorised tenants only.
- **D-091 (`harbor console` deployment).** Footer carries Protocol + Console versions and the connected-runtime label.
- **§13 forbidden practices.** Heavy memory values route through `artifacts.get` rather than inlining (closes D-026); the Console never bypasses identity rejection (no UI affordance to "view rejected memory anyway"); no parallel implementation of memory mutation (the deferred bulk actions are explicitly disabled until Phase 73 lands the Protocol surface).
