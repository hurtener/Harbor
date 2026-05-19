# Phase 73c — Console Sessions page (Protocol + UI bundled)

## Summary

Phase 73c ships the Console **Sessions** page — the past-and-active durable
record of every Harbor execution the operator has access to — as a
single bundled phase that lands (a) the NEW `sessions.list` Protocol
method with cursor pagination + the full filter set (status / agent /
user / tenant / window / has-intervention / has-failed-task /
cost-above), (b) `sessions.inspect` refinements pulled from Phase 73's
parent scope into the page-binding §12 reconciliation (right-rail
"Session Summary" projection), (c) the SvelteKit `/console/sessions`
list + detail route, (d) the matching Playwright spec, and (e) the
identity-scoped audit events on cross-tenant queries. Per Wave 13 §13
primitive-with-consumer compliance, the page IS the first consumer of
`sessions.list`; no Protocol primitive lands without a same-phase
end-to-end exercise.

## RFC anchor

- RFC §5.2
- RFC §6.9
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Sessions view": "Filters: status, agent, user, tenant,
  started-in-window, has-pending-intervention, has-failed-task,
  cost-above-threshold. Per-row: session ID, agent, user, status,
  started, duration, tasks, total-cost, total-tokens. Bulk actions:
  cancel selected, pause selected (gated on scope). Pagination / virtual
  scroll for large operators." Phase 73c implements the filter set
  verbatim on the wire (`sessions.list` payload) and the per-row shape
  verbatim in the table column projection. Bulk actions iterate the
  already-shipped per-row `cancel` / `pause` methods (D-072) — no new
  bulk Protocol method, just a Console-local toolbar.
- brief 11 §CC-2 (Identity-aware UI): "Tenant-scoped users see only
  their tenant's data. Admin-scoped users see fleet across tenants (with
  an explicit elevated view indicator). Per-feature gates (...) require
  admin scope." Phase 73c gates the tenant facet on `auth.ScopeAdmin`
  (per D-079) — the facet renders only when the JWT carries the claim,
  AND the runtime's `sessions.list` enforces it server-side
  (CLAUDE.md §6 — UI gates are convenience, server gates are security).
- brief 11 §CC-4 (Global search): "Recommendation: runtime-side for
  sessions/tasks (cardinality is high); Console-side for slow-moving
  catalog data (tools / agents / flows / MCP servers)." Phase 73c
  consumes the runtime-side `search.sessions` method that lands in
  Phase 72c — the Sessions search box wires straight to it, no
  Console-local index for sessions.
- brief 11 §"Per-task detail pane": the bottom-dock detail tab strip
  (Trajectory | Events | Cost History | Control History |
  Interventions) reuses the per-task detail pane shape from Live Runtime
  rather than inventing a new layout. The trajectory tab consumes
  `state.list_trajectories` + `state.load_planner_checkpoint` (shipped
  by Phase 73 parent) and the events tab consumes the SSE stream
  filtered to the session.
- brief 12 §"The two-surface model": the Sessions page is one of the 14
  full-Console pages served exclusively by `harbor console` (D-091); it
  is NOT mounted by `harbor dev`. The chat module (D-091's encapsulated
  library) is NOT a Sessions-page dependency — Sessions has no chat
  surface, the per-row "Continue this session in Live Runtime" action
  just navigates to the Live Runtime route.

## Findings I'm departing from (if any)

None. Every brief recommendation that touches sessions either lands
verbatim in this phase or is deferred per the binding carve-outs noted
in §11 below (D-064 Convert-to-Evaluation; D-065 no session priority;
brief 11 §"Open architectural questions" #10 session-share token; brief
11 §PG-5 drift-mode fork; cross-runtime session aggregator from D-091).

## Goals

- Ship the `sessions.list` Protocol method end-to-end: a typed wire
  request (`SessionsListRequest`) carrying the full filter set + cursor
  pagination, a typed response (`SessionsListResponse` with rows + next
  cursor + total-est), identity-scope enforced at the edge per D-079, an
  audit emit on every cross-tenant query (`audit.admin_scope_used`), and
  a registered method handler under the existing `ControlSurface`
  dispatch.
- Pull the page-binding refinements to `sessions.inspect` (mockup §12
  right-rail "Session Summary" projection: id / status / started /
  duration / events / tasks / agent / user / tenant / cost / last
  activity) into the response shape WITHOUT bumping `ProtocolVersion`
  (the wire surface is additive — new optional fields on the existing
  type per CLAUDE.md §8).
- Ship the SvelteKit `/console/sessions` route with two view modes (list
  - detail), the full §4 page anatomy from `page-sessions.md` (top
  sub-header strip; main table; right rail Session Summary + Recent
  Interventions + Recent Artifacts; bottom dock tab strip).
- Ship `web/console/tests/sessions-page.spec.ts` Playwright spec
  covering: catalog renders with all mockup columns; faceted filter
  narrows results; sub-header chips work; bulk-action toolbar appears on
  row select; right-rail Session Summary populates; bottom-dock tabs
  cycle.
- Ship a D-025 concurrent-reuse test for the `sessions.list` handler
  (N≥100 concurrent calls against one shared `SessionRegistry` under
  `-race`).
- Ship `test/integration/sessions_page_test.go` end-to-end with the
  real `SessionRegistry` + the Phase 60 wire transport + identity
  propagation + cross-tenant rejection without `admin`.

## Non-goals

- A `sessions.cancel` convenience Protocol method — out of scope.
  Cancelling an active session iterates `cancel` over each live task
  (the shipped D-072 method).
- "Convert to Evaluation" row action — D-064 explicitly defers
  Evaluations to post-V1. The row item is rendered DISABLED with a
  tooltip explaining the deferral; the underlying Protocol method does
  not land in V1.
- "Share read-only link" / session-share token — brief 11 §"Open
  architectural questions" #10. Post-V1.
- Priority column / Priority filter — D-065 dropped session-level
  priority. The mockup confirms zero priority surface.
- Cross-runtime session aggregator — D-091 puts the fleet aggregator
  post-V1; in V1 the runtime-switcher (D-091 §sidebar) re-handshakes
  against the new runtime and refreshes the list.
- Drift mode (fork past message, replay) — brief 11 §PG-5 defers
  post-V1.
- A new `bulk.cancel` / `bulk.pause` Protocol method — bulk wrapping is
  local UI iterating the per-row `cancel` / `pause` methods (D-072).
  Server takes one request per row.

## Acceptance criteria

- [ ] `internal/protocol/methods.MethodSessionsList` is registered
      ("sessions.list") and `internal/protocol/methods.MethodSessionsInspect`
      remains the existing entry; the single-source checker
      (`internal/protocol/singlesource`) shows both in
      `CanonicalWireTypes`.
- [ ] `internal/protocol/types.SessionsListRequest` carries:
      `Identity` (`IdentityScope` — D-079); `Filter` shape with
      `Statuses []SessionStatus` (Running / Paused / Completed /
      Failed); `AgentIDs []string`; `UserIDs []string`;
      `TenantIDs []string` (admin-only); `StartedWindow Window`
      (`From`, `To` `*time.Time`); `HasIntervention *bool`;
      `HasFailedTask *bool`; `CostAboveCents *int64`; free-text `Query`
      (forwarded to `search.sessions` per brief 11 §CC-4 when set);
      `Sort` ('started_desc' default / 'started_asc' /
      'last_activity_desc' / 'cost_desc'); pagination `Cursor string` +
      `Limit int` (default 50, max 200).
- [ ] `internal/protocol/types.SessionsListResponse` carries `Rows
      []SessionRow`, `NextCursor string`, `Truncated bool` (NEVER a
      silent total — D-026 fail-loudly: the server emits `Truncated:
      true` when the result hits `Limit`+1 candidates rather than
      silently dropping).
- [ ] `SessionRow` projects: `SessionID`, `Status`, `AgentID`,
      `AgentName`, `UserID`, `TenantID`, `StartedAt`, `LastActivityAt`,
      `Duration time.Duration`, `TasksCount int`, `EventsCount int`,
      `TotalCostCents int64`, `TotalTokens int64`,
      `HasPendingIntervention bool`, `HasFailedTask bool`. No
      `Priority` field (D-065).
- [ ] `sessions.inspect` response gains optional fields needed by the
      right-rail Session Summary projection (additive — no version bump
      per CLAUDE.md §8): `RecentInterventions []InterventionSummary`
      (capped at 5; each has type / reason / outcome / age),
      `RecentArtifacts []ArtifactRefSummary` (capped at 5; mime /
      filename / size / age).
- [ ] Identity enforcement at the edge: every `sessions.list` call
      without all three of `(tenant, user, session-or-cross-session
      claim)` is rejected `CodeIdentityRequired` (HTTP 401); every
      `sessions.list` with `TenantIDs` outside the operator's own
      tenant requires `auth.ScopeAdmin` (D-079); rejection is
      `CodeScopeMismatch` (HTTP 403); the audit
      emits `audit.admin_scope_used` on every successful admin-scope
      query.
- [ ] `web/console/src/routes/sessions/+page.svelte` renders the list
      view per mockup §4 page anatomy: top sub-header strip with Saved
      filters dropdown + Status chips + Identity picker + Tenants facet
      (admin-only) + Date range picker + More filters + free-text search
      + Refresh + Sort By + bulk-action toolbar (appears on row check).
- [ ] `web/console/src/routes/sessions/[id]/+page.svelte` renders the
      detail view per mockup §4 page anatomy: session detail header card
      + tab strip (Trajectory | Events | Cost History | Control History
      | Interventions) + right rail (Session Summary card + Recent
      Interventions card + Recent Artifacts card) + bottom dock placeholder
      for trajectory replay player.
- [ ] All `.svelte` files reference design tokens — no raw color /
      spacing / type-scale literals; `npm run lint` clean (stylelint
      mechanical gate per CLAUDE.md §4.5).
- [ ] All wire calls in the page go through the generated `protocol.ts`
      typed client (D-093); zero hand-rolled `fetch` calls in `.svelte`
      files.
- [ ] Saved-filter chips persist to the Console DB (`saved_filters`
      table from Phase 72h) — Console-local state ONLY, never a
      sessions-data shadow (D-061).
- [ ] Convert-to-Evaluation row action renders DISABLED with tooltip:
      "Evaluations is a post-V1 subsystem (D-064)." The action is NOT
      wired to any Protocol method.
- [ ] `web/console/tests/sessions-page.spec.ts` Playwright spec passes
      against `harbor console` boot + a seeded `SessionRegistry`,
      covering: (a) catalog renders rows with all mockup columns
      visible; (b) faceted filter (status=Failed) narrows results; (c)
      sub-header chips (status / saved-filter / sort) work end-to-end;
      (d) bulk-action toolbar appears when one or more row checkboxes
      are checked; (e) right-rail Session Summary populates after row
      click; (f) bottom-dock tabs cycle (Trajectory → Events → Cost
      History → Control History → Interventions).
- [ ] D-025 concurrent-reuse test: N≥100 concurrent `sessions.list`
      handlers against one shared `SessionRegistry` under `-race`;
      asserts no data races, no context bleed (per-call identity
      assertions), no cross-cancellation, no goroutine leak.
- [ ] Integration test (`test/integration/sessions_page_test.go`):
      real `SessionRegistry` (Phase 08 driver) + real Phase 60 wire
      transport (`httptest.Server` mounting `transports.NewMux`) + real
      Phase 61 auth middleware; covers (a) happy-path tenant-scoped
      `sessions.list`; (b) cross-tenant query without `admin` rejected
      `CodeScopeMismatch` + audit emit; (c) malformed cursor rejected
      `CodeInvalidRequest`; (d) N≥10 concurrent SSE subscribers
      consuming the same session's events while `sessions.list` is hit
      in parallel — no cross-talk, no goroutine leak.
- [ ] `scripts/smoke/phase-73c.sh` (PREFLIGHT_REQUIRES: live-server)
      exercises `sessions.list`, `sessions.inspect`, cross-tenant
      rejection without admin, identity-mandatory rejection, and the
      Playwright spec via `npm run test:e2e -- sessions-page.spec.ts`
      when the Console build is present.
- [ ] `docs/glossary.md` gains the new vocabulary listed in §"Glossary
      additions" below.
- [ ] `docs/plans/README.md` flips the Phase 73c row to Shipped on the
      implementing PR.
- [ ] `README.md` Status table reflects Phase 73c shipped on the
      implementing PR (Sessions page reader-facing surface pointer).

## Files added or changed

```text
internal/protocol/methods/methods.go                              # add MethodSessionsList constant
internal/protocol/methods/methods_test.go                         # extend the canonical method set
internal/protocol/types/sessions.go                               # SessionsListRequest / Response / SessionRow / SessionStatus enum / Window / InterventionSummary / ArtifactRefSummary (additive on existing sessions.inspect types)
internal/protocol/types/sessions_test.go                          # marshal round-trip + identity-mandatory zero-value rejection
internal/protocol/singlesource/canonical_test.go                  # extend CanonicalWireTypes set; regen TS via cmd/harbor-gen-protocol-ts
internal/sessions/list.go                                         # SessionRegistry.List(ctx, ListQuery) (rows, next-cursor, error) — extends interface
internal/sessions/list_test.go                                    # per-filter conformance + cursor pagination + admin-scope query path
internal/sessions/concurrent_test.go                              # D-025 N>=100 concurrent List against one shared *Registry
internal/server/sessions_list.go                                  # Protocol method handler binding to SessionRegistry.List
internal/server/sessions_list_test.go                             # handler unit tests (decode / encode / error mapping)
web/console/src/lib/protocol.ts                                   # REGENERATED via cmd/harbor-gen-protocol-ts (per D-093)
web/console/src/routes/sessions/+page.svelte                      # list view
web/console/src/routes/sessions/+page.ts                          # SSR-disabled loader (D-091 #7: client-side; no SSR)
web/console/src/routes/sessions/[id]/+page.svelte                 # detail view
web/console/src/routes/sessions/[id]/+page.ts                     # detail loader
web/console/src/lib/sessions/filter-bar.svelte                    # top sub-header strip component (Saved filters + Status + Identity + Tenants + Date range + More + Search + Refresh + Sort)
web/console/src/lib/sessions/sessions-table.svelte                # virtualised table (Skeleton DataTable wrapped)
web/console/src/lib/sessions/bulk-action-toolbar.svelte           # row-checkbox-gated bulk Cancel / Clone / Export
web/console/src/lib/sessions/session-summary-card.svelte          # right-rail card driven by sessions.inspect
web/console/src/lib/sessions/recent-interventions-card.svelte     # right-rail card driven by pause.* + tool.approval_* events
web/console/src/lib/sessions/recent-artifacts-card.svelte         # right-rail card driven by artifacts.list (shipped Phase 73 parent)
web/console/src/lib/sessions/bottom-dock-tabs.svelte              # Trajectory | Events | Cost History | Control History | Interventions
web/console/tests/sessions-page.spec.ts                           # Playwright spec
test/integration/sessions_page_test.go                            # real-driver end-to-end
scripts/smoke/phase-73c.sh                                        # new smoke (PREFLIGHT_REQUIRES: live-server)
docs/plans/phase-73c-console-sessions-page.md                     # this file
docs/decisions.md                                                 # append D-NNN (coordinator-assigned at dispatch — sessions.list shape + Truncated semantics + saved-filters carve-out)
docs/glossary.md                                                  # SessionsList method, Saved filter (Console-local), Session Summary card, Faceted filter (sessions)
docs/plans/README.md                                              # flip Phase 73c row Pending -> Shipped
README.md                                                         # Status table Phase 73c -> Shipped
```

## Public API surface

```go
// internal/protocol/methods
const MethodSessionsList Method = "sessions.list"
// MethodSessionsInspect already exists from Phase 73 parent.

// internal/protocol/types
type SessionsListRequest struct {
    Identity IdentityScope    // identity triple + optional admin claim (D-079)
    Filter   SessionFilter    // see below
    Sort     SessionSort      // started_desc default
    Cursor   string           // opaque pagination cursor
    Limit    int              // default 50, max 200
}

type SessionFilter struct {
    Statuses         []SessionStatus // Running / Paused / Completed / Failed
    AgentIDs         []string
    UserIDs          []string
    TenantIDs        []string        // requires ScopeAdmin
    StartedWindow    Window
    HasIntervention  *bool
    HasFailedTask    *bool
    CostAboveCents   *int64
    Query            string          // free-text; forwarded to search.sessions when non-empty
}

type SessionStatus string
const (
    SessionStatusRunning   SessionStatus = "running"
    SessionStatusPaused    SessionStatus = "paused"
    SessionStatusCompleted SessionStatus = "completed"
    SessionStatusFailed    SessionStatus = "failed"
)

type SessionSort string
const (
    SessionSortStartedDesc      SessionSort = "started_desc" // default
    SessionSortStartedAsc       SessionSort = "started_asc"
    SessionSortLastActivityDesc SessionSort = "last_activity_desc"
    SessionSortCostDesc         SessionSort = "cost_desc"
)

type SessionsListResponse struct {
    Rows       []SessionRow
    NextCursor string
    Truncated  bool // D-026: fail-loudly when result hit Limit+1 candidates
}

type SessionRow struct {
    SessionID              string
    Status                 SessionStatus
    AgentID                string
    AgentName              string
    UserID                 string
    TenantID               string
    StartedAt              time.Time
    LastActivityAt         time.Time
    Duration               time.Duration
    TasksCount             int
    EventsCount            int
    TotalCostCents         int64
    TotalTokens            int64
    HasPendingIntervention bool
    HasFailedTask          bool
    // NO Priority — D-065 dropped session-level priority from V1.
}

// SessionInspectResponse (existing from Phase 73 parent) gains additive
// optional fields used by the right-rail Session Summary projection.
// No version bump per CLAUDE.md §8 — additive.
type SessionInspectResponse struct {
    // ... existing fields from Phase 73 ...
    RecentInterventions []InterventionSummary // capped at 5
    RecentArtifacts     []ArtifactRefSummary  // capped at 5
}

// internal/sessions — interface extension
type SessionRegistry interface {
    // ... existing Open / Get / Touch / Close / Inspect / GC / CloseRegistry ...
    List(ctx context.Context, q ListQuery) (rows []SessionRow, next string, truncated bool, err error)
}

type ListQuery struct {
    Filter SessionFilter
    Sort   SessionSort
    Cursor string
    Limit  int
}
```

## Test plan

- **Unit:** `internal/protocol/types/sessions_test.go` round-trips
  `SessionsListRequest` / `Response` / `SessionRow` / additive
  `SessionInspectResponse` fields; zero-value rejection asserts
  identity-mandatory at decode time. `internal/server/sessions_list_test.go`
  pins the handler's decode / encode / error mapping (every
  `CodeInvalidRequest` / `CodeIdentityRequired` / `CodeScopeMismatch`
  branch + cursor decode failure → `CodeInvalidRequest`).
  `internal/sessions/list_test.go` pins per-filter conformance against
  the in-mem registry: each filter axis independently narrows; cursor
  round-trip is stable; the Truncated boundary fires at exactly
  `Limit`+1 rows (D-026); admin-scope path emits
  `audit.admin_scope_used`.
- **Integration:** `test/integration/sessions_page_test.go` boots the
  assembled dev stack (per D-094 — `harbortest/devstack.Assemble`),
  seeds the `SessionRegistry` with two tenants × N sessions, opens an
  authenticated client (Phase 61 JWT), and (a) lists own-tenant happy
  path; (b) lists cross-tenant without `admin` → `CodeScopeMismatch`
  - asserts audit event; (c) lists cross-tenant WITH `admin` → success
  - asserts `audit.admin_scope_used`; (d) malformed cursor →
  `CodeInvalidRequest`; (e) N≥10 concurrent SSE subscribers on the
  same session's events fan in while `sessions.list` is hit in
  parallel — no cross-talk, baseline goroutine restored after teardown.
- **Conformance:** the Phase 62 Protocol conformance suite gains
  scenarios for `MethodSessionsList` (every error code + the
  in-process AND over-the-wire transport per D-080). Single-source
  checker confirms `MethodSessionsList` + the new wire types live only
  in `internal/protocol/methods` + `internal/protocol/types`.
- **Concurrency / leak:** `internal/sessions/concurrent_test.go` runs
  N≥100 concurrent `List` calls against one shared `*Registry` under
  `-race`, asserts no data races, no context bleed (each goroutine's
  identity round-trips through the rows it gets back), no
  cross-cancellation, baseline goroutine count restored after the
  workgroup drains (D-025).

## Smoke script additions

`scripts/smoke/phase-73c.sh` (PREFLIGHT_REQUIRES: live-server):

- Runs `go test -race -count=1 -timeout 180s ./internal/sessions/...
  ./internal/protocol/methods/... ./internal/protocol/types/...
  ./internal/server/...` and asserts pass.
- Runs `go test -race -count=1 -timeout 240s -run TestE2E_Phase73c
  ./test/integration/...` and asserts pass.
- Live-wire asserts against the preflight-booted `harbor dev` (per
  D-094 the dev stack already mounts the Protocol mux):
  - `protocol_call sessions.list` with a complete identity scope →
    HTTP 200; response shape includes `rows`, `next_cursor`,
    `truncated` (skip per 404/405/501 → SKIP convention if the method
    is not yet registered).
  - `protocol_call sessions.list` without `(tenant, user, session)` →
    HTTP 401 / `CodeIdentityRequired`.
  - `protocol_call sessions.list` with `TenantIDs` containing a tenant
    other than the operator's own AND no `admin` scope → HTTP 403 /
    `CodeScopeMismatch`.
  - `protocol_call sessions.inspect` against a seeded session →
    response includes the additive `recent_interventions` +
    `recent_artifacts` fields (post-Phase-73c shape).
- Static guards (defence-in-depth):
  - No raw color / spacing literals under `web/console/src/routes/
    sessions/` or `web/console/src/lib/sessions/` (stylelint already
    catches; the smoke greps as belt-and-braces).
  - `web/console/src/lib/protocol.ts` carries the generated header
    (`// CODE GENERATED BY cmd/harbor-gen-protocol-ts. DO NOT EDIT.`)
    — D-093.
  - No hand-rolled `fetch(` in `web/console/src/routes/sessions/` or
    `web/console/src/lib/sessions/` — every wire call goes through the
    typed client (CLAUDE.md §4.5 #5 + §13).
  - No reference to `Priority` / `priority_field` / `Convert to
    Evaluation` action handler in the Sessions Svelte source —
    D-064 / D-065 carve-out enforcement.
- Optional Playwright invocation when the Console build + a Playwright
  install are present: `(cd web/console && npm run test:e2e --
  sessions-page.spec.ts)`; SKIP if Playwright not installed (the
  baseline harness from Phase 75 documents the install gate).

## Coverage target

- `internal/protocol/types` (sessions wire types): ≥ 85% (matches
  Phase 60's typed-wire-surface target).
- `internal/sessions` (List extension): ≥ 80% (matches the parent
  package's Phase 08 target; the List path is the new code).
- `internal/server` (sessions_list handler): ≥ 80%.
- `web/console`: covered by Playwright + svelte-check `--fail-on-warnings`;
  no Go-side coverage target.

## Dependencies

- Phase 60 — Protocol wire transport (SSE + REST). The new
  `sessions.list` handler mounts on the existing `transports.NewMux`.
- Phase 61 — Protocol auth (JWT). `auth.ScopeAdmin` is the gate for
  the tenant facet + cross-tenant `sessions.list`.
- Phase 73 (parent) — `sessions.inspect` shipped; Phase 73c
  ADDITIVELY extends the response shape.
- Phase 72a — `events.subscribe` filter extensions consumed by the
  per-session Events tab in the bottom dock + the Recent Interventions
  card.
- Phase 72b — `IdentityScope` extension for admin impersonation
  consumed by the Identity picker in the top sub-header strip.
- Phase 72c — `search.sessions` consumed by the free-text Search box;
  the Sessions page's `Query` field forwards to it.
- Phase 73f — `tools` facet for the cross-drill from Sessions → Tools
  (a session row's tool-column drill-down).
- Phase 75 — Playwright baseline harness; this phase ships
  `sessions-page.spec.ts` against it.
- Phase 72h — Console DB schema (`saved_filters` table) consumed by
  the Saved-filter chips per D-061.
- Phase 08 — `SessionRegistry` (the data source `sessions.list` reads).

## Risks / open questions

- **Cursor opaqueness.** The cursor SHOULD be opaque to clients (a
  base64-encoded `(StartedAt, SessionID)` tuple for `started_desc`,
  rotated when the sort changes). Risk: a future schema change to the
  registry's index could invalidate the cursor; mitigation — the
  cursor carries a 1-byte version prefix; mismatched versions fail
  loudly (`CodeInvalidRequest`) instead of silently degrading.
- **Truncated semantics vs total count.** Brief 11 §"Sessions view"
  asks for "pagination / virtual scroll for large operators" but does
  NOT require a total count. Phase 73c emits `Truncated: bool` (per
  D-026 fail-loudly), not an exact total — exact counts under high
  cardinality are O(N) and the page can lazily fetch more pages until
  `NextCursor == ""`. The mockup's footer micro-counter "X sessions"
  shows `len(rows)` for the current page (with a `(more)` suffix
  when `Truncated`), not a global aggregate.
- **`search.sessions` integration boundary.** When the `Query` field
  is non-empty, the handler MUST forward to `search.sessions` (Phase
  72c) and merge the result-set under the same pagination contract.
  Open question for the implementing agent: forward then filter, or
  filter then forward? Resolution — forward then filter (the search
  index is the cardinality bottleneck; the filter axes are
  post-search refinements). Documented in the D-NNN entry the
  coordinator assigns at dispatch.
- **Saved filters vs Protocol filter shape.** D-061: saved filters are
  Console-local. The phase MUST NOT introduce a `sessions.saved_filter.*`
  Protocol method (forbidden practice — Console DB shadow of runtime
  entities). Saved filters live in the Console DB `saved_filters` table
  from Phase 72h; the wire shape carries only the inflated filter,
  never a saved-filter ID.
- **Bottom-dock Cost History tab data source.** Aggregates
  `llm.cost.recorded` events filtered to the session. The aggregation
  is Console-local (a sparkline / per-step table) — Phase 73c does
  NOT introduce a `cost.aggregate` Protocol method (yagni; the event
  stream already carries the data).
- **Recent Artifacts card cap.** Capped at 5 in `sessions.inspect`'s
  additive response field; the operator clicks through to the full
  Artifacts page (Phase 73l) for the unbounded list.

## Glossary additions

- **SessionsList method** — the NEW `sessions.list` Protocol method
  that returns a paginated, filtered projection of the
  `SessionRegistry` keyed by `(tenant, user)` (or fleet-wide under
  `auth.ScopeAdmin`). Wire shape lives in
  `internal/protocol/types.SessionsListRequest` / `Response`.
  Distinct from `sessions.inspect`, which returns the full snapshot
  for a single session. RFC §5.2, RFC §6.9.
- **Saved filter (Console-local)** — a named, persistent filter
  configuration the operator builds in the top sub-header strip and
  pins as a chip. Persisted ONLY in the Console DB's `saved_filters`
  table (Phase 72h) per D-061 — the wire shape carries only the
  inflated filter, NEVER a saved-filter ID. Distinct from server-side
  views, which Harbor does NOT expose.
- **Session Summary card** — the right-rail card on the Sessions
  detail view that projects the additive
  `SessionInspectResponse` fields (id, status, started, duration,
  events, tasks, agent, user, tenant, cost, last activity). Sourced
  exclusively from `sessions.inspect`; the card NEVER reads runtime
  internals.
- **Faceted filter (sessions)** — the top sub-header strip composed of
  Status / Identity / Tenants / Date range / More-filters chips. Each
  chip compiles to a field on `SessionFilter` on the wire; the chip
  set IS the wire filter shape.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes (the integration test asserts cross-tenant rejection
      without `admin` + the audit emit on the admin-scope query).
- [ ] **If this phase builds a reusable artifact: concurrent-reuse
      test passes — N≥100 concurrent invocations against a single
      shared instance under `-race`.** The `sessions.list` handler +
      the `SessionRegistry.List` extension are reusable artifacts;
      `internal/sessions/concurrent_test.go` covers the registry path,
      and `internal/server/sessions_list_test.go` covers the handler
      path.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
      a cross-subsystem seam: an integration test exists, wires real
      drivers end-to-end, asserts identity propagation, covers ≥1
      failure mode, runs under `-race`.** Phase 73c consumes Phase 08
      (SessionRegistry) + Phase 60 (transport) + Phase 61 (auth) +
      Phase 72a (events filter) + Phase 72b (identity scope) + Phase
      72c (search.sessions). `test/integration/sessions_page_test.go`
      wires all of them with real drivers.
- [ ] If new vocabulary: glossary updated (four new terms above).
- [ ] If a brief finding was departed from: N/A — none.
- [ ] D-064 carve-out enforced: Convert-to-Evaluation row action is
      disabled with tooltip; no Protocol method shipped.
- [ ] D-065 carve-out enforced: no `Priority` column / filter / field
      anywhere in the Sessions page or `SessionRow` wire shape.
- [ ] D-061 carve-out enforced: saved filters persist to Console DB
      only; no `sessions.saved_filter.*` Protocol method.
- [ ] D-079 enforced: cross-tenant `sessions.list` requires
      `auth.ScopeAdmin`; rejection on missing claim is loud
      (`CodeScopeMismatch` + audit emit).
- [ ] D-066 enforced: bulk Cancel / bulk Pause require the
      control-scope claim; the toolbar gates on the claim and the
      runtime enforces on each per-row `cancel` / `pause` call.
- [ ] D-091 enforced: the Sessions page is served by `harbor console`,
      NOT by `harbor dev`. The Playwright spec boots against `harbor
      console`.
- [ ] D-092 enforced: Sessions `.svelte` files use Svelte 5 runes
      mode; `svelte-check --fail-on-warnings` clean.
- [ ] D-093 enforced: every wire call goes through the generated
      `protocol.ts`; `make protocol-ts-gen-check` clean (the
      `sessions.list` types regenerated).
- [ ] No naming of the predecessor project (drift-audit checks).
