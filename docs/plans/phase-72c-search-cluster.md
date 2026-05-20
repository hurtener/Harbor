# Phase 72c — `search.*` cluster (5 methods, one phase)

**Status:** Shipped (D-108).

## Summary

Lands the Wave 13 cross-cutting global-search primitive as five Protocol methods sharing a single conformance surface: a Console-side palette dispatcher (`search.query`) and four runtime-side per-subsystem indexes (`search.sessions`, `search.tasks`, `search.events`, `search.artifacts`). Brief 11 §CC-4 pins the split — sessions / tasks / events / artifacts are high-cardinality runtime-side and own server-enforced indexes; tools / agents / flows / MCP connections stay Console-side because their catalogs are slow-moving (those Console-side adapters land inside each Stage-2 page phase, NOT here). All five methods share the same shape (identity-aware filter + pagination + free-text query + `ArtifactRef` heavy-payload bypass) per the §12 lock-in #4 operator decision to keep the cluster as one phase. The §13 primitive-with-consumer obligation is discharged in-phase by a query-shape conformance test per index; the Stage-2 Sessions / Tasks / Events / Artifacts pages are the page-level consumers that ride on top.

## RFC anchor

- RFC §5.2 (state snapshots row — search is the runtime-lens projection over sessions/tasks/events/artifacts)
- RFC §6.13 (typed event bus — `search.events` is a server-enforced filter over the same identity-mandatory subscription shape)
- RFC §7 (Console layer — `search.*` is a "cross-cutting Console need" per §7.3 that lands as named acceptance criteria of its consumer phases)

## Briefs informing this phase

- brief 11 (Console feature surface — §CC-4 Global search; per-page search boxes in Sessions / Tasks / Events / Artifacts views; the runtime-side vs Console-side split is the central design call)
- brief 12 (Console deployment + shared UI library — every Protocol method must serve the third-party Console implementation, not just the bundled SvelteKit one)
- brief 05 (state, tasks, artifacts, sessions — supplies the shape of the underlying entities `search.sessions` / `search.tasks` / `search.artifacts` query against)
- brief 06 (events, observability, devx — supplies the identity-mandatory subscription shape `search.events` extends with a search predicate)

## Brief findings incorporated

- brief 11 §CC-4: "runtime-side for sessions/tasks (cardinality is high); Console-side for slow-moving catalog data (tools / agents / flows / MCP servers)." This phase ships ONLY the four runtime-side indexes (sessions, tasks, events, artifacts); the Console-side Tools / Agents / Flows / MCP search adapters are per-page concerns inside their Stage-2 phases (73c / 73d / 73e / 73f / 73g / 73i / 73k) — not free-floating runtime primitives.
- brief 11 §CC-4: "Indexes: session IDs (and metadata), task IDs, agent names, tool names, flow names, MCP server names, artifact filenames, event types, user/tenant identifiers." Each runtime-side index this phase ships exposes free-text query over the subset of those entities it owns: `search.sessions` over session IDs + agent + status + age; `search.tasks` over task IDs + status + agent + tool; `search.events` over event type + source + identity + payload-header free text; `search.artifacts` over artifact ID + filename + mime + source.
- brief 12 §"the two-surface model": "every operation flows through canonical Protocol methods." The four runtime-side methods + the palette dispatcher MUST be Protocol-level — a third-party Console implementation can call them with the same wire shape. No Console-private search hooks.
- brief 05 §"Session-lifetime invariants": session identity triple is immutable for lifetime; `search.sessions` MUST enforce that the requesting subscriber's identity scope filters the result set (a session in tenant T1 NEVER surfaces in a T2 caller's query unless the caller holds the cross-tenant `auth.ScopeAdmin` admin scope claim).
- brief 06 §"Isolation-triple filtering by default": Subscribe ignores any filter that elides the triple unless the caller has admin scope; `search.events` reuses the same shape — empty triple non-admin search is rejected loudly with `ErrIdentityScopeRequired` (NEVER silently downgraded to "empty result set").

## Findings I'm departing from (if any)

None. The split (high-cardinality runtime-side; slow-moving Console-side) is taken verbatim from brief 11 §CC-4 and re-confirmed by the decomposition doc §12 lock-in #4. The palette dispatcher's pure-aggregation shape (it composes the four runtime-side calls + Console-side catalog filters; it carries no index of its own) follows brief 11's recommendation literally.

## Goals

- Ship FIVE Protocol methods atop the shipped Phase 60 transport: `search.query` (palette dispatcher), `search.sessions`, `search.tasks`, `search.events`, `search.artifacts`.
- All five methods share one wire shape: `SearchRequest` (free-text query + identity-aware filter + pagination + facet/index selector) and `SearchResponse` (paginated results scoped by the caller's `(tenant, user, session)` triple + heavy payloads ship as `ArtifactRef`).
- All five methods enforce identity rejection (D-033 shape): missing `tenant_id` / `user_id` / `session_id` → fail loudly with `ErrIdentityRequired` (NEVER silently degrade to "empty result set").
- Cross-tenant queries gate on a `auth.ScopeAdmin` admin scope claim (D-079 shape extended to the search surface); a missing claim → 403 with `CodeIdentityRequired` / `CodeAuthRejected` (NEVER returns "empty" silently).
- The §13 primitive-with-consumer obligation is discharged in-phase by a query-shape conformance test per index (one test per runtime-side method, plus a fifth aggregating-shape test for `search.query`); the Stage-2 page consumers (Sessions / Tasks / Events / Artifacts) ride on top of the same surface.
- Heavy-payload bypass: result rows that exceed the heavy-content threshold (D-026) MUST ship `ArtifactRef` rather than inline bytes. Verified by an integration test that deliberately materialises a payload above threshold and asserts the response carries a ref.
- Concurrent-reuse contract (D-025): N≥100 concurrent search calls against shared indexes under `-race`; assert no data races, no context bleed, no cross-cancellation, baseline goroutine count restored.

## Non-goals

- **Console-side adapters for Tools / Agents / Flows / MCP Connections** — those are per-page concerns landing in 73f (Tools), 73e (Agents), 73i (Flows), 73k (MCP Connections). The runtime owes them NO `search.*` method; their catalog data is Console-side per brief 11 §CC-4.
- **Substring search over event payload contents.** Brief 11 §CC-4 explicitly defers this (would force materialisation of heavy payloads through the LLM-edge safety net contract; D-026). The filter operates on event header fields + ArtifactStub summaries; deep-payload search is post-V1.
- **Saved searches** — Console-local per D-061; lives in the `saved_filters` Console DB schema landing in Phase 72h, NOT in the runtime.
- **Cross-runtime search aggregation** — D-091 post-V1.
- **Faceted aggregations** (count buckets per facet within a search result) — `events.aggregate` (Phase 72a) covers the events-only flavor; cross-index aggregate facets are post-V1.
- **Ranking / relevance scoring beyond lexicographic / time-order.** V1 ships substring / prefix match + ordered by recency. Relevance scoring is a post-V1 concern.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares five new method constants: `MethodSearchQuery` (`search.query`), `MethodSearchSessions` (`search.sessions`), `MethodSearchTasks` (`search.tasks`), `MethodSearchEvents` (`search.events`), `MethodSearchArtifacts` (`search.artifacts`). All five register in `canonicalMethods`.
- [ ] `internal/protocol/types/search.go` defines `SearchRequest`, `SearchResponse`, `SearchResultRow`, `SearchIndex` (enum: `sessions` / `tasks` / `events` / `artifacts`), `SearchFacet` — single source of truth (D-002 wire-type rule).
- [ ] Each of the four runtime-side methods (`search.sessions`, `search.tasks`, `search.events`, `search.artifacts`) is implemented as a server-enforced index that filters by the caller's `(tenant, user, session)` triple BEFORE materialising any payload bytes. Identity-rejection is enforced loudly with `ErrIdentityRequired` when ANY component of the triple is missing; never silently downgraded.
- [ ] `search.query` is a pure aggregator: given a `SearchRequest` whose `Indexes` field selects ≥1 of `{sessions, tasks, events, artifacts}`, it concurrently fans out to each selected runtime-side method, merges the result rows, paginates the union, and returns. It carries NO index of its own and emits NO new events. Console-side catalog indexes (tools / agents / flows / mcp) are NOT invoked by `search.query` (per the Console-side / runtime-side split).
- [ ] Cross-tenant query gating: a `SearchRequest` whose `Filter.TenantIDs` lists multiple tenants (or a tenant other than the caller's authenticated tenant) requires the `auth.ScopeAdmin` scope claim (D-079 closed-scope-set shape). Without the claim: 403 with `CodeAuthRejected` (NEVER silently downgraded to empty result).
- [ ] Pagination is identical across all five methods: `Page` + `PageSize` request fields; `Page`, `PageCount`, `TotalCount`, `HasMore` response fields. Default `PageSize=20`, max `PageSize=200` (request a larger size → 400 with `CodeInvalidRequest`).
- [ ] Heavy-payload bypass (D-026): a `SearchResultRow` whose underlying entity carries a payload ≥ heavy-content threshold ships an `ArtifactRef` in the row's preview field, NEVER inline bytes. The result-shape test asserts this for every index.
- [ ] Audit redaction (RFC §6.13 audit-before-emit boundary): every search result row goes through `audit.Redactor` before emission (`SearchResponse` is NOT a `SafePayload`; default-redactor path applies). On redaction failure: emit `audit.redaction_failed`, fail the request loudly with the wrapped error; NEVER ship un-redacted bytes.
- [ ] Per-method query-shape conformance test (the §13 primitive-with-consumer in-phase consumer): one test per runtime-side method exercises the query shape end-to-end with a real index — emits known entities, runs the search, asserts only in-scope rows return + identity propagation + pagination math + heavy-payload bypass. The fifth test exercises `search.query` aggregating across all four indexes.
- [ ] Concurrent-reuse test (D-025): N≥100 concurrent search calls against a single shared index per subsystem under `-race`, each call with a distinct per-goroutine identity quadruple — asserts no data races, no context bleed (rows from goroutine A never surface in goroutine B's response), no cross-cancellation (cancelling A's ctx never affects B), baseline goroutine count restored after all calls return.
- [ ] Identity-isolation cross-call test: N≥10 concurrent searches across two tenants under `-race`, asserts tenant T1's rows NEVER surface in tenant T2's response and vice versa.
- [ ] Integration test (`test/integration/search_cluster_test.go`) at the cross-subsystem seam: real sessions registry + real tasks registry + real events bus + real artifacts store + real Protocol transport — runs one search per index, asserts identity propagation across the full Runtime↔Protocol seam, covers the cross-tenant 403 failure mode AND the missing-identity 401 failure mode AND the heavy-payload `ArtifactRef` bypass failure mode (≥1 explicit failure mode per §17.3).
- [ ] `scripts/smoke/phase-72c.sh` invokes all five methods, asserts each round-trips (or SKIPs while surface is not yet implemented per 404→SKIP rule); asserts cross-tenant call without admin claim → 403 per method; asserts missing identity → 401 per method.

## Files added or changed

```text
internal/protocol/methods/methods.go                # +5 search.* method constants + canonicalMethods entries
internal/protocol/methods/methods_test.go           # +exhaustiveness test for the 5 new methods
internal/protocol/types/search.go                   # +SearchRequest, +SearchResponse, +SearchResultRow, +SearchIndex enum, +SearchFacet
internal/protocol/types/search_test.go              # +wire-type round-trip
internal/protocol/errors/errors.go                  # confirm ErrSearchScopeRequired (or reuse ErrIdentityScopeRequired) is registered; +CodeSearchInvalidIndex if not subsumed by CodeInvalidRequest
internal/protocol/transports/control/search_handler.go     # +dispatch routes for the 5 methods
internal/protocol/transports/control/search_handler_test.go
internal/search/search.go                           # +Searcher interface; +SearcherRegistry (one per index) — the §4.4 seam pattern
internal/search/aggregate.go                        # +Query(ctx, req) — the search.query palette dispatcher (concurrent fanout across selected indexes; merge + paginate)
internal/search/sessions/index.go                   # +SessionsSearcher (queries sessions.SessionRegistry)
internal/search/sessions/index_test.go              # query-shape conformance + concurrent-reuse + identity-rejection
internal/search/tasks/index.go                      # +TasksSearcher (queries tasks.TaskRegistry)
internal/search/tasks/index_test.go
internal/search/events/index.go                     # +EventsSearcher (queries events.Replayer + filter)
internal/search/events/index_test.go
internal/search/artifacts/index.go                  # +ArtifactsSearcher (queries artifacts.ArtifactStore catalog)
internal/search/artifacts/index_test.go
internal/search/concurrent_reuse_test.go            # D-025 — N≥100 concurrent calls per index, plus cross-tenant isolation stress
test/integration/search_cluster_test.go             # cross-package: real sessions + tasks + events + artifacts + Protocol transport
scripts/smoke/phase-72c.sh                          # PREFLIGHT_REQUIRES: live-server — 5 happy-path + 5 cross-tenant + 5 missing-identity
docs/glossary.md                                    # +search.query, +search.sessions, +search.tasks, +search.events, +search.artifacts, +SearchRequest, +SearchResponse
```

## Public API surface

```go
// internal/protocol/methods/methods.go (added constants)
const (
    MethodSearchQuery     Method = "search.query"
    MethodSearchSessions  Method = "search.sessions"
    MethodSearchTasks     Method = "search.tasks"
    MethodSearchEvents    Method = "search.events"
    MethodSearchArtifacts Method = "search.artifacts"
)

// internal/protocol/types/search.go
type SearchIndex string

const (
    SearchIndexSessions  SearchIndex = "sessions"
    SearchIndexTasks     SearchIndex = "tasks"
    SearchIndexEvents    SearchIndex = "events"
    SearchIndexArtifacts SearchIndex = "artifacts"
)

// SearchRequest is the shared wire shape for all five search.* methods.
// search.query honours Indexes (the palette dispatcher selects 1..4 of
// the runtime-side indexes); the four per-index methods ignore Indexes
// and operate on their own index.
type SearchRequest struct {
    Query    string        // free-text — substring / prefix match
    Indexes  []SearchIndex // search.query only; empty = all four
    Filter   SearchFilter  // identity scope + optional time-window
    Facets   []SearchFacet // optional per-index facet selectors (e.g. tasks.status, events.type)
    Page     int
    PageSize int           // default 20, max 200
}

// SearchFilter narrows results. The TenantIDs / UserIDs / SessionIDs
// fields default to the caller's authenticated triple; supplying values
// OTHER THAN the caller's own requires the auth.ScopeAdmin scope
// claim (D-079 closed-scope-set shape).
type SearchFilter struct {
    TenantIDs  []string
    UserIDs    []string
    SessionIDs []string
    Since      time.Time
    Until      time.Time
}

// SearchFacet is a per-index dimension selector — e.g. {"tasks.status",
// "running"} or {"events.type", "tool.failed"}. Unknown facets are
// silently ignored (post-V1 may tighten to error).
type SearchFacet struct {
    Key   string
    Value string
}

// SearchResultRow is the uniform result row shape across all five
// methods. Preview is REDACTED via audit.Redactor before emission.
// Heavy payloads (≥ D-026 threshold) ship as ArtifactRef, NEVER inline.
type SearchResultRow struct {
    Index       SearchIndex
    ID          string           // session ID / task ID / event ID / artifact ID
    Identity    identity.Quadruple
    OccurredAt  time.Time
    Preview     string           // short summary (≤ threshold)
    Ref         *ArtifactRef     // populated when preview would exceed threshold
    Facets      map[string]string
}

// SearchResponse is the uniform response shape.
type SearchResponse struct {
    Rows       []SearchResultRow
    Page       int
    PageCount  int
    TotalCount int64
    HasMore    bool
}

// internal/search/search.go
// Searcher is the §4.4 seam interface every runtime-side index implements.
// One Searcher instance per index, registered into SearcherRegistry at
// boot. Searcher is a compiled artifact under the D-025 concurrent-reuse
// contract — immutable after construction, safe for N concurrent calls.
type Searcher interface {
    Index() SearchIndex
    Search(ctx context.Context, req SearchRequest) (SearchResponse, error)
}

// Query is the search.query palette dispatcher: concurrent fan-out
// across the request's selected indexes, merge result rows, paginate
// the union, return. Carries NO index of its own.
func Query(ctx context.Context, reg *SearcherRegistry, req SearchRequest) (SearchResponse, error)
```

## Test plan

- **Unit:**
  - `internal/search/sessions/index_test.go` — query-shape conformance: seed N sessions across two tenants, run searches with varied queries / facets / pagination, assert (a) only caller-scoped rows returned, (b) pagination math correct, (c) ordering deterministic.
  - `internal/search/tasks/index_test.go` — same shape as sessions, exercising the tasks-specific facets (status, type, agent, tool).
  - `internal/search/events/index_test.go` — same shape, exercising event-type filter + time-window; asserts ArtifactRef-based payload bypass for events whose payload exceeds the heavy-content threshold (D-026).
  - `internal/search/artifacts/index_test.go` — same shape, exercising mime / source / size facets; asserts every result row carries an `ArtifactRef` (artifacts are by-reference by construction).
  - `internal/search/aggregate_test.go` — `Query` palette dispatcher: a request with `Indexes=[sessions,tasks]` concurrently calls both indexes, merges rows, paginates the union; tested under deliberate per-index latency to assert correct ordering after merge.
  - `internal/protocol/types/search_test.go` — wire-type JSON round-trip (request + response + result row + facet); enum-value validation; pagination-default validation.
- **Integration:**
  - `test/integration/search_cluster_test.go` — wires real `sessions/drivers/inmem`, `tasks/drivers/inmem`, `events/drivers/inmem`, `artifacts/drivers/inmem` and the real Protocol transport. Tests: (a) each of the 5 methods round-trips end-to-end; (b) cross-tenant query without `auth.ScopeAdmin` claim → 403 (the explicit failure mode per §17.3); (c) missing identity component → 401 (second explicit failure mode); (d) heavy payload ≥ D-026 threshold ships `ArtifactRef` (third explicit failure mode — silent bytes-inlining is forbidden); (e) cross-session isolation under N≥10 concurrent searchers across two tenants.
- **Conformance:**
  - The five new methods register in the Phase 80 Protocol conformance suite (`internal/protocol/conformance.RunSuite`). All five round-trip identically over both the in-process `ControlSurface.Dispatch` AND the over-the-wire Phase 60 mux under `httptest.Server` (D-080 two-transport matrix). When Phase 80's suite reaches search-cluster scope, the auto-enumerate gate asserts a scenario exists for each of the five methods.
- **Concurrency / leak:**
  - `internal/search/concurrent_reuse_test.go::TestSearchers_ConcurrentReuse` — N=100 concurrent `Search` calls against EACH of the four shared `Searcher` instances under `-race`. Each call has a distinct per-goroutine identity quadruple. Asserts: no data races (race-detector pass), no context bleed (response rows always carry the requesting goroutine's identity), no cross-cancellation (cancelling goroutine A's ctx never affects B's response), baseline `runtime.NumGoroutine()` restored after all calls return. The `Searcher` instances are constructed ONCE for the whole test (the D-025 immutable-artifact contract).

## Smoke script additions

`scripts/smoke/phase-72c.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- 5 happy-path round-trips, one per method: `protocol_call 'search/query' '{"query":"hello"}'`, `protocol_call 'search/sessions' '{"query":"agent-a"}'`, `protocol_call 'search/tasks' '{"query":"in-progress"}'`, `protocol_call 'search/events' '{"query":"tool.failed"}'`, `protocol_call 'search/artifacts' '{"query":"report.pdf"}'`. While the surface is not yet implemented, each call SKIPs per the protocol_call stub; once the phase lands, replace with `assert_status 200` + `assert_json_path '.rows | type' 'array'`.
- 5 cross-tenant failure assertions, one per method: same call with `"filter":{"tenant_ids":["t1","t2"]}` BUT without the `auth.ScopeAdmin` scope claim → `assert_status 403`.
- 5 missing-identity failure assertions, one per method: same call with an empty body in a context that strips the identity triple → `assert_status 401`.
- Surface-existence probes (`skip_if_404`) for each of the 5 routes, so until the Protocol layer ships these methods the smoke remains green via SKIP.

## Coverage target

- `internal/search`: 85% (aggregate dispatcher + per-index packages).
- `internal/search/sessions`: 85%.
- `internal/search/tasks`: 85%.
- `internal/search/events`: 85%.
- `internal/search/artifacts`: 85%.
- `internal/protocol/types/search.go`: 90% (wire-type serialisation).
- `internal/protocol/transports/control` (search_handler.go scope): 85%.

## Dependencies

**Same-wave (Wave 13, Stage 1 — Batch A per §12 lock-in #2):**

- Phase 72 (events.subscribe scope foundation — supplies the identity-scope claim primitive `auth.ScopeAdmin` reuses)
- Phase 72a (events filter — `search.events` reuses the EventFilter predicate over event header fields)

**Already shipped (pre-Wave 13):**

- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`; supplies the closed-scope-set primitive `auth.ScopeAdmin` extends per D-079)
- Phase 06 (events bus + replayer — `Shipped`; `search.events` consumes the Replayer capability)
- Phase 08 (sessions registry — `Shipped`)
- Phase 20 (tasks registry — `Shipped`)
- Phase 17/18/19 (artifacts store — `Shipped`)

**Pending dependency — Phase 73 (state inspection):** the decomposition doc §4 row 72c lists `Deps: 60, 73 (state inspection)`. Phase 73's status is `Pending` at the time of this plan. `search.sessions` and `search.tasks` query against the same shipped `sessions.SessionRegistry` / `tasks.TaskRegistry` interfaces Phase 73 will extend; this phase consumes the ALREADY-SHIPPED interfaces and does NOT block on 73's UI-feeding methods. If Phase 73 reshapes the underlying registry interfaces (unlikely — see Risks), 72c slips. See "Risks / open questions" for the contingency.

## Risks / open questions

- **Phase 73 (state inspection) is `Pending` at plan-authoring time.** The decomposition doc §4 lists 73 as a dependency of 72c, but the dependency is on the `sessions.SessionRegistry` / `tasks.TaskRegistry` interfaces — both ALREADY SHIPPED in Phases 08 and 20. Phase 73 will EXTEND those interfaces with UI-feeding methods (per the master plan); `search.*` consumes only the listing methods Phases 08 and 20 already provide. **Contingency:** if Phase 73 reshapes the underlying registry interfaces in a backward-incompatible way during Wave 13, the search-cluster phase slips by the same delta and the per-index tests are re-pinned against the new shape. The integration test will surface this immediately under `-race`.
- **Index storage strategy.** V1 ships in-memory iterate-and-filter per query (matches Brief 11 §CC-4's runtime-side high-cardinality posture and the §9 V1 persistence triad's in-memory floor). For deployments with millions of sessions / tasks / events / artifacts the linear scan is expensive — post-V1 may add a search-index sidecar (SQLite FTS5 / Postgres pg_trgm). The wire shape (`SearchRequest` / `SearchResponse`) is index-strategy-agnostic, so the upgrade is additive.
- **Cross-tenant scope claim name** (resolved per audit R7 / N1): `auth.ScopeAdmin` from the D-079 closed two-scope set (`ScopeAdmin` + `ScopeConsoleFleet`). No new `search.crosstenant` or `events.crosstenant` scope is minted — Phase 72's Non-goals explicitly forbid a third scope. Cross-tenant search reuses the existing closed set, matching 72 / 72a / 72e / 73c / 73e patterns.
- **`search.query` palette dispatcher latency.** A naive concurrent fan-out across four indexes returns when the slowest completes. V1 accepts this; a per-index timeout (default 2s) caps tail latency. The integration test exercises a deliberate per-index latency to assert correct ordering after merge.
- **Console-side adapter ownership.** Per brief 11 §CC-4 the Tools / Agents / Flows / MCP catalogs are searched Console-side; those adapters MUST live inside their per-page phases (73c / 73d / 73e / 73f / 73g / 73i / 73k), NOT in this phase. A reviewer who sees a Tools-search method added to `internal/protocol/methods/methods.go` in this phase's PR should reject on sight — that's a §13 "page primitive without its consumer" smell read backward.
- **Pagination default size.** V1 ships `PageSize=20` default and `PageSize=200` max. Operator may revise at phase-review time; the integration test exercises both boundaries.

## Glossary additions

- **`search.query`** — Protocol method serving the Console-side palette dispatcher (Wave 13 Phase 72c). Pure aggregator: concurrent fan-out across the selected runtime-side indexes, merges + paginates the union. Carries no index of its own; emits no events. Heavy-payload bypass via `ArtifactRef` per D-026.
- **`search.sessions`** — Runtime-side Protocol method (Wave 13 Phase 72c) returning paginated session matches scoped to the caller's `(tenant, user, session)` triple. Cross-tenant query requires the `auth.ScopeAdmin` admin scope claim (D-079 closed-scope-set shape).
- **`search.tasks`** — Runtime-side Protocol method (Wave 13 Phase 72c) returning paginated task matches; same identity-scope contract as `search.sessions`.
- **`search.events`** — Runtime-side Protocol method (Wave 13 Phase 72c) returning paginated event matches filtered by event type + identity scope + time-window. Reuses the `EventFilter` predicate landed in Phase 72a. Substring search over event payload contents is post-V1 (brief 11 §CC-4).
- **`search.artifacts`** — Runtime-side Protocol method (Wave 13 Phase 72c) returning paginated artifact matches; rows always carry an `ArtifactRef` (artifacts are by-reference by construction per D-026).
- **`SearchRequest`** — Wire-shape struct shared by all five `search.*` methods. Carries free-text query + identity-aware filter + facets + pagination + (for `search.query` only) the selected index set.
- **`SearchResponse`** — Wire-shape struct shared by all five `search.*` methods. Carries paginated result rows + pagination cursors. Result rows ship heavy payloads as `ArtifactRef`, NEVER inline (D-026).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (binding for this phase — every search method filters by the isolation triple; the integration test asserts cross-tenant rejection without `auth.ScopeAdmin` claim AND cross-session isolation under concurrent searchers)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `Search` calls against EACH shared `Searcher` instance under `-race`, asserting no data races, no context bleed, no cross-cancellation, baseline goroutine count restored (D-025)
- [ ] **Integration test exists** — `test/integration/search_cluster_test.go` wires real sessions + tasks + events + artifacts + Protocol transport under `-race`; covers cross-tenant 403, missing-identity 401, and heavy-payload `ArtifactRef` bypass failure modes (§17.3 ≥1 failure mode rule — this phase ships 3)
- [ ] Glossary updated with the 5 new method names + `SearchRequest` + `SearchResponse`
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (decomposition doc §12 lock-in item 3 + the binding coordinator-verify protocol — five Protocol methods is a high-audit-cost surface; coordinator MUST grep each method's smoke assertion, identity-rejection test, concurrent-reuse test, and `[wave-13-extends]` citation back to its declaration)
