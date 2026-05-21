# Phase 73f — Console Tools page (Protocol + UI bundled)

## Summary

Bundles the Tools page Protocol surface and UI into a single phase per the Wave 13 staging (`docs/plans/wave-13-decomposition.md` §5). Protocol additions: `tools.list`, `tools.get`, `tools.describe`, `tools.metrics`, `tools.content_stats`, `tools.set_approval_policy` (admin), `tools.revoke_oauth` (admin). UI: the catalog table + selected-tool detail panel + right-rail aggregate cards + per-page Playwright spec, all reconciled against `docs/rfc/assets/console-tools-page.png` per the spec's §12. First Stage-2 page after the Stage 2.1 dependency-set is satisfied.

## RFC anchor

- RFC §6.4 (Tool catalog and transports)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Tools view", §PG-3 MCP-Apps compliance)
- brief 12 (deployment + two-surface model — shared chat / playground library)

## Brief findings incorporated

- brief 11 §"Tools view": the page is "a runtime lens over the tool catalog with per-tool drill-down, approval and OAuth status visibility, and aggregate health". The Protocol surface this phase ships covers exactly that — catalog list + per-tool describe + metrics + content stats.
- brief 11 §PG-3: MCP-Apps content flows through the canonical renderer registry (`web/console/src/lib/chat/renderers/`); the Tools page MUST surface negotiated `DisplayMode` (D-062) but never invokes a bespoke renderer.
- brief 12 §"the shared chat / playground library": even though Tools is not a chat page, its Recent-invocations tab links into the session's bottom dock — that link MUST route through the shared chat module's session deep-link helper, not a Tools-private nav.

## Findings I'm departing from (if any)

None.

## Goals

- Ship a complete, mockup-aligned Tools page (`/console/tools`) as a Protocol client per D-091 (served by `harbor console`, NEVER `harbor dev`).
- Land 7 Protocol methods: 5 read (`tools.list`, `tools.get`, `tools.describe`, `tools.metrics`, `tools.content_stats`) + 2 admin (`tools.set_approval_policy`, `tools.revoke_oauth`).
- The admin methods are gated by the `tools.admin` control-scope claim (D-066); the read methods require only the tools-read scope.
- Approve / Reject buttons in the per-tool Approval tab invoke the existing shipped `approve` / `reject` Protocol methods (Phase 54) — NO new approval method here (forbidden by §13's "no parallel implementations" rule).
- Per-page Playwright spec at `web/console/tests/tools-page.spec.ts` covers the catalog filter, selected-tool drill-down, and Approve / Reject path.

## Non-goals

- Editing tool descriptors (register / unregister tools from the Console). Tool registration is a runtime configuration concern, NOT a Console action; deferred (per page-tools.md §10).
- Cross-runtime tools catalog aggregator. D-091 — post-V1.
- Per-tool cost dashboards. Deeper dashboards deferred to post-V1 cost subsystem expansion.
- Bespoke per-MCP-server tool renderers. Forbidden per Brief 11 §PG-3.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The Tools page is a catalog surface with a
selected-tool detail rail; it mounts inside the shared app shell and clears
the §5 depth bar like every other page.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/tools/` and is served at `/tools` with
  **no `/console/` URL prefix** (the `(console)` route group is a
  layout-grouping device and does not appear in the URL). Detail views live at
  `(console)/tools/[id]/` and are served at `/tools/<id>`. All inter-page
  links use the unprefixed form; a link to `/console/<anything>` is a bug.
- **Render inside the shared app shell.** The page renders as a child of
  `(console)/+layout.svelte` — the single app shell carrying the sidebar,
  breadcrumb, identity/connection indicator, and footer. It never ships a
  standalone layout.
- **Use the shared `components/ui/` inventory.** It composes the cross-page
  primitives in `web/console/src/lib/components/ui/` — `PageHeader`,
  `FilterBar`, `DataTable`, `DetailRail`/`RailCard`, `BulkActionBar`,
  `SavedViewChips`, `Pagination`, `StatusChip`, `ConnectionFooter`,
  `PageState`. It **never forks a primitive that already exists**;
  page-specific components go in `components/tools/`.
- **Route all async state through the four-state `<PageState>`.** Every async
  surface flows through `<PageState>`'s four mutually-exclusive states —
  Disconnected / Loading / Error / Empty. The Error state ships a working
  **Retry** that re-invokes the loader and suppresses any stale primary view;
  **Disconnected** ("no Runtime attached") is detected via `connection.ts`
  returning `null` and is **never conflated with Error**.
- **Clear the §5 depth bar.** The page is not "done" until it has all of:
  a `PageHeader`; a `FilterBar`; a primary `DataTable` or canvas; a
  `DetailRail` or a tabbed detail route; Console-DB-backed `SavedViewChips`;
  real `Pagination` (page / size / total, prev / next — not a fake "load
  more"); a `ConnectionFooter`; and the full four-state `PageState`.
- **Talk to the Runtime only through `HarborClient` + `connection.ts`.** All
  Protocol calls go through the single typed `HarborClient` (adding a
  namespace, never a new top-level client); the connection resolves through
  `web/console/src/lib/connection.ts`. **No `fetch` in `.svelte` files, no
  direct `localStorage` access, no hand-rolled per-page client.**
- **Introduce no raw token literals.** No raw color / spacing / type-scale
  literals in `.svelte` files — design tokens from `tokens.css` only
  (Stylelint enforces this; `npm run lint` fails CI on a violation).
- **Ship no stubbed action presented as done.** Every action either invokes
  the real Protocol method or renders **disabled-with-tooltip** explaining
  why. A button that fakes success with a feedback string is a §13-class
  silent-degradation violation.

See `docs/design/console/CONVENTIONS.md` §9 for the per-phase callout
contract and D-121 for the rationale.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `tools.list`, `tools.get`, `tools.describe`, `tools.metrics`, `tools.content_stats`, `tools.set_approval_policy`, `tools.revoke_oauth`.
- [ ] `internal/protocol/types/tools.go` defines `Tool`, `ToolFilter`, `ToolMetrics`, `ToolContentStats`, `ToolApprovalPolicy`, `ToolOAuthBinding` structs — single source of truth (D-002).
- [ ] `tools.list` accepts `ToolFilter` (Scope, Transport, OAuthStatus, ApprovalPolicy, ReliabilityTier facets) and returns paginated tool rows + aggregate counters (Total / Active / Pending approval / Awaiting OAuth) for the filtered view.
- [ ] `tools.describe` returns the full manifest of the registered descriptor (transport, version, scopes, OAuth binding scope per D-083, approval policy per D-086, reliability shell per D-024).
- [ ] `tools.metrics` returns error-rate gauges over a selectable window (1h / 24h / 7d toggle) plus a status pill (`Healthy` / `Degraded` / `Offline`).
- [ ] `tools.content_stats` returns the per-tool distribution of recent result sizes vs the heavy-content threshold (RFC §6.5 / D-026) and the negotiated `DisplayMode` snapshot (D-062).
- [ ] `tools.set_approval_policy` and `tools.revoke_oauth` BOTH require the `tools.admin` control-scope claim (D-066) and emit audit events through the shipped `audit.Redactor`.
- [ ] All seven methods enforce identity-mandatory: missing `tenant_id` / `user_id` / `session_id` → fail loudly with `ErrIdentityRequired` (NEVER silently downgrade — §13 forbidden-practice rule).
- [ ] The Tools page SvelteKit route (`web/console/src/routes/tools/+page.svelte`) renders against `console-tools-page.png` with: sub-header strip + catalog table + selected-tool detail panel + right-rail Tool-overview / Status / Content-size cards + bottom-right Run history.
- [ ] The page goes through the **typed Protocol client** at `web/console/src/lib/protocol.ts` (D-093 generated from `CanonicalWireTypes`); NO hand-rolled `fetch` calls in `.svelte` files.
- [ ] Approve / Reject buttons in the Approval tab invoke the EXISTING shipped `approve` / `reject` methods (Phase 54) — verified by grep of the page source.
- [ ] Saved-filter chips, Export ▾ (CSV / JSON), pin/sort preferences persist in Console DB (D-061 — Console-local; NEVER mutate runtime entities).
- [ ] Design tokens only — no raw color/spacing/type-scale literals in `.svelte` files (§13 + Stylelint enforcement).
- [ ] Per-page Playwright spec `web/console/tests/tools-page.spec.ts` covers: (a) catalog table renders rows with all mockup columns, (b) facet chip toggle updates rows, (c) selected-tool drill-down opens detail panel, (d) Approve action invokes `approve` Protocol method and updates the row.
- [ ] `scripts/smoke/phase-73f.sh` asserts all 7 Protocol methods round-trip and the page route returns 200.

## Files added or changed

```text
internal/protocol/methods/methods.go                # +7 tools.* methods
internal/protocol/types/tools.go                    # +Tool, ToolFilter, ToolMetrics, ToolContentStats, ToolApprovalPolicy, ToolOAuthBinding
internal/protocol/errors/errors.go                  # confirm ErrToolsAdminRequired (or reuse ErrControlScopeRequired)
internal/protocol/transports/stream/tools_handler.go  # method dispatch + scope-claim checks
internal/protocol/transports/stream/tools_handler_test.go
internal/tools/protocol/list.go                     # tools.list implementation (filter + pagination + aggregates)
internal/tools/protocol/describe.go                 # tools.describe implementation
internal/tools/protocol/metrics.go                  # tools.metrics implementation (consumes tool.* events from Phase 72a aggregate)
internal/tools/protocol/content_stats.go            # tools.content_stats implementation (heavy-content threshold)
internal/tools/protocol/admin.go                    # tools.set_approval_policy + tools.revoke_oauth + audit emission
internal/tools/protocol/list_test.go
internal/tools/protocol/describe_test.go
internal/tools/protocol/metrics_test.go
internal/tools/protocol/content_stats_test.go
internal/tools/protocol/admin_test.go
internal/tools/protocol/concurrent_reuse_test.go    # D-025 — N≥100 concurrent calls against shared catalog
test/integration/tools_page_test.go                 # cross-package: tools/catalog + protocol transport + identity scope
web/console/src/routes/tools/+page.svelte
web/console/src/lib/components/tools/CatalogTable.svelte
web/console/src/lib/components/tools/ToolDetailPanel.svelte
web/console/src/lib/components/tools/ToolOverviewCard.svelte
web/console/src/lib/components/tools/StatusErrorRateCard.svelte
web/console/src/lib/components/tools/ContentSizeCard.svelte
web/console/src/lib/components/tools/SubHeaderStrip.svelte
web/console/src/lib/components/tools/RunHistoryStrip.svelte
web/console/src/lib/db/saved_filters_tools.ts  # TYPED wrapper over 72h's saved_filters table (NOT a new table; uses 72h's `page` discriminator column). Exports typed get/put/list/delete helpers for tools-page saved-filter rows.
web/console/tests/tools-page.spec.ts
cmd/harbor-gen-protocol-ts/                            # if not yet shipped, this phase's Protocol-type additions trigger regeneration
web/console/src/lib/protocol.ts                        # REGENERATED ONLY by `make protocol-ts-gen` — never hand-edited
scripts/smoke/phase-73f.sh
docs/glossary.md                                       # +tools.list, +tools.describe, +tools.metrics, +tools.content_stats, +"approval policy" if not already glossed
```

## Public API surface

```go
// internal/protocol/types/tools.go
type Tool struct {
    ID             string
    Name           string
    Version        string
    Scope          string // "tenant" | "agent" | "session"
    Transport      string // "in-proc" | "HTTP" | "MCP" | "A2A"
    OAuthStatus    string // "Bound" | "Required" | "n/a" | "Expired"
    ApprovalPolicy string // "auto" | "gated" | "denied"
    ReliabilityTier string
    LastUsedAt     time.Time
    Owner          string
}

type ToolFilter struct {
    Scopes          []string
    Transports      []string
    OAuthStatuses   []string
    ApprovalPolicies []string
    ReliabilityTiers []string
    Search          string // free-text over tool name + version
    Page            int
    PageSize        int
}

type ToolListResponse struct {
    Tools      []Tool
    Page       int
    PageCount  int
    Aggregates ToolAggregates
}

type ToolAggregates struct {
    Total            int64
    Active           int64
    PendingApproval  int64
    AwaitingOAuth    int64
}

type ToolMetrics struct {
    ErrorRate1h   float64
    ErrorRate24h  float64
    ErrorRate7d   float64
    Status        string // "Healthy" | "Degraded" | "Offline"
}

type ToolContentStats struct {
    Histogram         []ContentBucket // buckets by size; one row per power-of-2 range
    HeavyThreshold    int64           // bytes (from D-026)
    NegotiatedDisplay map[string]string // mime → DisplayMode (per D-062)
}
```

## Test plan

- **Unit:**
  - `list_test.go` — filter combinations: every facet axis tested in isolation + a "all facets" combination; aggregates math.
  - `describe_test.go` — manifest shape conformance per transport (in-proc / HTTP / MCP / A2A).
  - `metrics_test.go` — window arithmetic: deliberate `tool.invoked` + `tool.failed` emission over a known window; assert error-rate calculation matches.
  - `content_stats_test.go` — heavy-content threshold cutoff; bucket count for known emission pattern.
  - `admin_test.go` — `set_approval_policy` without `tools.admin` claim → 403; with claim → audit event emitted + policy updated.
- **Integration:**
  - `test/integration/tools_page_test.go` — real `tools/catalog`, real `tools/inproc`, real Protocol transport. Subscribes for tool events, lists catalog, drills into one tool, approves a pending approval (round-trip through shipped `approve`); asserts identity propagation across the seam (D-061 + §17).
- **Conformance:**
  - The 7 methods run against the Protocol conformance suite (Phase 62 shipped) — every transport (HTTP+SSE / WebSocket / stdio) emits identical wire shapes.
- **Concurrency / leak:**
  - `concurrent_reuse_test.go` — N=100 concurrent `tools.list` calls with overlapping filters against a single shared catalog under `-race` (D-025).
- **UI (Playwright):**
  - `tools-page.spec.ts` — page load returns 200; catalog table renders mockup columns; toggle a facet chip → row count updates; click a row → detail panel opens; click Approve in Approval tab → row's approval-pending counter decrements + audit event fires.

## Smoke script additions

`scripts/smoke/phase-73f.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'tools/list' '{}'` → assert 200; `assert_json_path '.tools | type' 'array'`.
- `protocol_call 'tools/list' '{"filter": {"transports": ["MCP"]}}'` → assert filter honored.
- `protocol_call 'tools/get' '{"id": "<first-tool-id>"}'` → assert 200.
- `protocol_call 'tools/describe' '{"id": "<first-tool-id>"}'` → assert manifest shape.
- `protocol_call 'tools/metrics' '{"id": "<first-tool-id>", "window": "1h"}'` → assert 200; `assert_json_path '.status' '<oneof Healthy|Degraded|Offline>'`.
- `protocol_call 'tools/content_stats' '{"id": "<first-tool-id>"}'` → assert 200.
- `protocol_call 'tools/set_approval_policy' '{"id": "<first-tool-id>", "policy": "gated"}'` (without `tools.admin` claim) → `assert_status 403`.
- `protocol_call 'tools/revoke_oauth' '{"id": "<first-tool-id>"}'` (without `tools.admin` claim) → `assert_status 403`.
- `assert_status 200 /console/tools` → asserts the SvelteKit page route returns the static asset (lands when `harbor console` ships in 73m; SKIPped here).

## Coverage target

- `internal/tools/protocol`: 85%.
- `internal/protocol/transports/stream`: 80%.
- `web/console/src/routes/tools/`: 70% (Svelte component coverage via `svelte-check` + Playwright).

## Dependencies

**Same-wave (Wave 13, Stage 1):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72a (events filter + aggregate — `tools.metrics` consumes the aggregate)
- Phase 72h (Console DB schema — saved-filter chips)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 26 (Tool catalog core — `Shipped`)
- Phase 27 (HTTP driver — `Shipped`)
- Phase 28 (MCP driver — `Shipped`)
- Phase 29 (A2A driver — `Shipped`)
- Phase 30 (tool-side OAuth — `Shipped`)
- Phase 31 (tool-side approval — `Shipped`)
- Phase 54 (Protocol task control surface — `Shipped`, supplies `approve` / `reject`)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)
- Phase 64a (tool catalog OAuth + approval wiring — `Shipped`)

## Risks / open questions

- **Aggregate counter cost.** `tools.list` returns `ToolAggregates` over the filtered set. Naive impl scans every tool's metrics per request; for catalogs with thousands of tools this is wasteful. V1 accepts O(N) per call (matches Brief 11 §CC-4 high-cardinality posture); post-V1 may add a cached aggregator.
- **`tools.metrics` consumes events from Phase 72a's `events.aggregate`.** A 72a delivery slip would slip 73f. Mitigation: 72a is Stage-1 Batch A; 73f is Stage 2.1 (runs only after Batch A + B both merge).
- **Console DB schema for saved filters lives in Phase 72h.** 73f's `saved_filters_tools.ts` is a TYPED WRAPPER over 72h's existing `saved_filters` table (which has a `page TEXT` discriminator column); this phase adds NO new tables. The wrapper exports typed get/put/list/delete helpers that scope reads/writes to `page = "tools"`. If 72h ships a different column shape, the wrapper's typed signature needs to match.
- **Per-page Playwright spec coverage.** The wave-end 75a aggregator suite enumerates every page-spec and asserts a matching `*.spec.ts` exists. The 73f spec MUST be merged before 75a's enumeration runs — i.e. before the final Stage-2.3 PR.

## Glossary additions

- **`tools.list`** — Protocol method returning the catalog of tools available to the caller's identity scope, with optional facet filters and aggregate counters.
- **`tools.describe`** — Protocol method returning the full manifest of a registered tool descriptor.
- **`tools.metrics`** — Protocol method returning per-tool error-rate gauges + status pill over a selectable window.
- **`tools.content_stats`** — Protocol method returning per-tool result-size distribution + negotiated `DisplayMode` snapshot.
- **`tools.set_approval_policy`** — Admin method updating a tool's approval policy. Requires `tools.admin` claim.
- **`tools.revoke_oauth`** — Admin method revoking all OAuth bindings for a tool. Requires `tools.admin` claim.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes (`web/console/src/lib/protocol.ts` regenerated from `CanonicalWireTypes` per D-093)
- [ ] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092)
- [ ] `npm run lint` passes in `web/console/` (no raw color / spacing literals per §13)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the seven new methods all touch identity; the integration test asserts cross-tenant rejection)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `tools.list` calls against a single shared catalog under `-race` (D-025)
- [ ] **Integration test exists** — `test/integration/tools_page_test.go` wires real `tools/catalog` + real transport + identity propagation under `-race` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/tools-page.spec.ts` exists and passes (binding for every 73x phase per the decomposition doc §12 lock-in)
- [ ] Glossary updated with the 6 new method names
- [ ] If a brief finding was departed from: justified + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (decomposition doc §12 lock-in item 3 + the binding coordinator-verify protocol)
