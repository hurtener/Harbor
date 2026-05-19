# Phase 73j — Console Memory page (Protocol + UI bundled)

## Summary

Bundles the Memory page Protocol surface and UI into a single phase per the Wave 13 staging (`docs/plans/wave-13-decomposition.md` §5). Protocol additions are three read-only methods: `memory.list`, `memory.get`, and `memory.health` — together they let an operator inspect the runtime's `MemoryStore` per-identity, drill into a single memory record's content + metadata, and surface aggregate health counters (records / expiring / identity-rejected / recovery-dropped). The UI is the catalog table + selected-item right rail + Memory health / Recent identity rejections / Recovery dropouts status cards, reconciled against `docs/rfc/assets/console-memory-page.png` per the page spec's §12, plus per-page Playwright spec `web/console/tests/memory-page.spec.ts`. **V1 is view-only** — the mockup's bulk-action toolbar (`Delete selected`, `Refresh TTL`, `Pin`) renders disabled-with-tooltip; the runtime-side mutation surface (`memory.put`, `memory.delete`, `memory.promotions`, `memory.strategy_trace`) is deferred to Phase 73 / post-V1 per the page-memory.md §10 carve-out. Stage 2.1 phase (parallel with 73f / 73i / 73k / 73l).

## RFC anchor

- RFC §5.2 (Protocol surface — state snapshots row)
- RFC §6.6 (Memory subsystem)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Memory view", §CC-4 search posture, §"the two-surface model")
- brief 04 (Memory + Skills — strategy taxonomy, identity-mandatory fail-closed gate, recovery loop)
- brief 12 (deployment + two-surface model — Protocol-client posture)

## Brief findings incorporated

- brief 11 §"Memory view": "Inspect memory state per identity / session / agent ... Filter by identity (the visible scope respects the JWT — only sees what the JWT scope allows) ... Memory items list ... Manual operations: add memory, edit, evict (admin-only)." This phase ships only the read surface (list / get / health); the manual-mutation surface is the page-memory.md §10 carve-out, surfaced as disabled-with-tooltip in the UI.
- brief 11 §CC-4: "events are high-cardinality runtime-side — the runtime owns the index and exposes a search method, not Console-side substring matching." `memory.list` follows the same posture — content-search and identity facets are filter parameters on the request, not Console-side scans over an exported snapshot.
- brief 04 §4.2: "if the identity triple is incomplete, the operation behaves as if memory is disabled and emits an audit event, never returns data scoped to a default." This phase MUST honour that gate at every Protocol method — missing identity component → `ErrIdentityRequired` (D-001) and a `memory.identity_rejected` audit emit (D-033 shape, already shipped on the `MemoryStore` driver layer). The Console surfaces those rejections through the Recent identity rejections card; **the UI never offers a "view rejected memory anyway" affordance** (§13 forbidden practice).
- brief 04 §6 "Fail-closed: `MemoryStore` operation with a missing `SessionID` returns no data and emits an audit event." The `memory.list` / `memory.get` / `memory.health` Protocol handlers compose on top of the shipped `MemoryStore.Snapshot` / `Health` surfaces; identity propagation is mandatory at the Protocol edge (no opt-out knob).
- brief 12 §"the two-surface model": the wire shapes for `memory.list` / `memory.get` / `memory.health` MUST live in `internal/protocol/types/` so third-party Console implementations consume the same shape (D-002 single-source rule).

## Findings I'm departing from (if any)

- **Event-name renaming in page-memory.md §12.** The page spec's §12 mockup-aligned refinements table names the recovery-dropped event `memory.overflow_drop_oldest` (and the Wave 13 decomposition doc §5 echoes it). The actually-shipped runtime constant is `EventTypeMemoryRecoveryDropped` with wire string `memory.recovery_dropped` (per D-035 + `internal/memory/events.go:33`). This phase **uses the shipped wire string `memory.recovery_dropped`** (consistent with the rest of the page-memory.md spec §3 + §7 + §11 and the runtime). Renaming the shipped event would be a D-035 RFC re-litigation and is explicitly NOT in scope here; flagged as a §12 mockup-refinement drift to be cleaned up in a follow-up `docs(design)` PR. Recorded under "findings I'm departing from" because the §12 §13 / D-035-closure footer reads literally as a brief-shaped finding.

## Goals

- Ship a complete, mockup-aligned Memory page (`/console/memory`) as a Protocol client per D-091 (served by `harbor console`, NEVER `harbor dev`).
- Land three Protocol methods: `memory.list`, `memory.get`, `memory.health`. All read-only; no write methods land in this phase.
- Introduce two new scope claims gated through the Phase 61 `auth.HasScope` primitive:
  - `memory.read` — required for `memory.list` / `memory.get` / `memory.health` within the caller's own identity quadruple.
  - `memory.crosstenant` — required to widen the identity-scope filter beyond the caller's own tenant (per page-memory.md §9 + D-079).
- Render the page anatomy from page-memory.md §4 + §12: sub-header strip, strategy / overlay chip row, main memory table, right rail status cards (Memory health / Recent identity rejections / Recovery dropouts / Selected item detail), bulk-action toolbar (V1 = disabled-with-tooltip).
- Per-page Playwright spec at `web/console/tests/memory-page.spec.ts` covers list rendering, facet toggle, selected-item drill-down, identity-rejection surfacing, and heavy-value `artifacts.get` deep-link.
- Heavy memory values (≥ heavy-content threshold per D-026) route through the already-shipped `artifacts.get` method via `ArtifactStub` references on `memory.get`'s response — **NEVER inline bytes** (§13 forbidden practice + D-026 closure).
- The 14-page IA's Memory page works end-to-end under `harbor console`: an operator with `memory.read` lists items, opens one, sees its post-redaction value (or follows the `artifacts.get` link for heavy values), checks recent identity-rejection events on the right rail, and the bulk-action toolbar is visibly disabled with the "Memory mutation surface deferred — Phase 73" tooltip.

## Non-goals

- **Memory mutation surface** (`memory.put`, `memory.delete`, manual add / edit / evict UI). Deferred to Phase 73 / post-V1 per page-memory.md §10. The bulk-action toolbar renders disabled-with-tooltip.
- **`memory.strategy_trace`** — strategy debugger that returns the selection trace + rejected items + discard reasons for a planner step. Deferred (page-memory.md §10 — strategy editor is post-V1; the trace projection is heavy and not on V1's floor).
- **`memory.promotions`** — list of declared cross-session promotion policies. Deferred (page-memory.md §10 — cross-runtime memory aggregator is post-V1; the promotions surface lands when the promotion policy machinery itself ships, post-V1).
- **TTL-based bulk eviction UI.** Out of V1 per page-memory.md §10 — TTLs evict automatically per Phase 23/24 contracts.
- **Cross-runtime memory aggregator.** D-091 — post-V1.
- **Priority field rendered anywhere.** D-065 invariant preserved; the `Pinned` strategy chip is a Phase 24 STRATEGY, not a priority. **Renamed mockup-refinement event `memory.overflow_drop_oldest`** — see "Findings I'm departing from".

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `memory.list`, `memory.get`, `memory.health` constants. The wire strings are lowercase snake_case (`memory/list`, `memory/get`, `memory/health` per the stream transport's routing convention).
- [ ] `internal/protocol/types/memory.go` defines `MemoryItem`, `MemoryFilter`, `MemoryListResponse`, `MemoryItemDetail`, `MemoryHealthAggregate`, `MemoryHealthResponse` — single source of truth (D-002). The wire types are generated into `web/console/src/lib/protocol.ts` by `cmd/harbor-gen-protocol-ts` (D-093); the regeneration is part of this phase's PR (`make protocol-ts-gen-check` clean).
- [ ] `memory.list` accepts `MemoryFilter` (`TenantIDs`, `UserIDs`, `SessionIDs`, `AgentIDs`, `Scopes` — `session | user | tenant` — `Drivers` — `inmem | sqlite | postgres` — `Strategies` — `none | truncation | rolling_summary`, future `Pinned` / `Episodic` / `Recent` / `Persistent` overlay chips reserved — `HasTTLExpiring bool`, `ContentSearch string`, `Page int`, `PageSize int`); returns paginated `MemoryItem` rows + aggregate counters (Total / Expiring1h / IdentityRejected24h / RecoveryDropped24h). Identity scope is mandatory; the filter widens beyond the caller's tenant ONLY when `memory.crosstenant` is granted.
- [ ] `memory.get` accepts `(MemoryKey, identity.Quadruple)` and returns `MemoryItemDetail` carrying the full key + identity quadruple + metadata (strategy, scope, TTL, size, driver) + post-redaction `Value` (when size < heavy-content threshold) OR `ValueArtifact ArtifactStub` (when size ≥ heavy-content threshold per D-026). **NEVER both populated; NEVER inline bytes above threshold.**
- [ ] `memory.health` returns `MemoryHealthAggregate` with counters: total records, expiring in 1 h, identity-rejected count (last 24 h), recovery-dropped count (last 24 h, per D-035), plus the driver-comparison per-scope `driver_by_scope map[string]string` (e.g. `{"session": "inmem", "tenant": "postgres"}`). Counters derive from `events.aggregate` (Phase 72a) over `memory.identity_rejected` / `memory.recovery_dropped` / `memory.health_changed` event types — runtime-side aggregation per brief 11 §CC-4.
- [ ] All three methods enforce identity-mandatory: missing `tenant_id` / `user_id` / `session_id` → fail loudly with `ErrIdentityRequired` (D-001 + §13 forbidden-practice "silent degradation"). The runtime emits `memory.identity_rejected` (D-033 shape — already shipped on the driver layer); the Protocol handler propagates the failure as Protocol error `CodeIdentityRequired` so the Console surfaces it on the Recent identity rejections card.
- [ ] Two new scope claims registered: `memory.read` (required for all three methods) + `memory.crosstenant` (required to widen `TenantIDs` beyond the caller's own tenant). Violations rejected with `ErrControlScopeRequired` (or `ErrIdentityScopeRequired` — match the shipped sentinel from Phase 61 / 72a). Every rejection audited (D-079 pattern).
- [ ] Heavy-value bypass: `memory.get` MUST NOT return raw bytes ≥ heavy-content threshold (D-026). Implementation produces an `ArtifactStub` via the shipped `artifacts.put` path on the runtime-side (when the materialised value crosses the threshold) and returns the stub on `memory.get.ValueArtifact`. **An LLM-edge enforcement pass already exists in `internal/llm/safety.go`; this phase mirrors the policy at the Protocol edge for memory inspection** — same shape, same `ErrContextLeak` posture if a driver ever returns raw heavy bytes via `memory.get`.
- [ ] **D-033 invariant — identity-rejection events surface, never masked.** The page's Recent identity rejections card consumes `memory.identity_rejected` from the Phase 72a-extended subscription with `<missing>` substitution preserved. **The Console renders the rejection event verbatim — NO "view rejected memory anyway" affordance, NO partial-identity substitution Console-side** (§13 forbidden-practice + binding-carve-outs item D-033 in page-memory.md §12).
- [ ] **D-065 invariant — no session-level priority.** No `Priority` field on `MemoryItem`, no priority column in the page table, no priority chip in the right rail. The `Pinned` strategy chip is a Phase 24 strategy filter (chip in the strategy / overlay chip row), NOT a priority dimension.
- [ ] The Memory page SvelteKit route (`web/console/src/routes/memory/+page.svelte`) renders against `docs/rfc/assets/console-memory-page.png` with: sub-header strip + strategy / overlay chip row + main memory table + bulk-action toolbar (disabled-with-tooltip) + right-rail Memory-health / Recent-identity-rejections / Recovery-dropouts / Selected-item-detail cards + footer (`Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`).
- [ ] The page goes through the **typed Protocol client** at `web/console/src/lib/protocol.ts` (D-093 generated from `CanonicalWireTypes`); NO hand-rolled `fetch` calls in `.svelte` files (§13 forbidden-practice).
- [ ] Bulk-action toolbar (`Delete selected`, `Refresh TTL`, `Pin`) renders disabled with the tooltip "Memory mutation surface deferred — Phase 73". The disabled state is a CSS-disabled `<button>` (not a hidden element) so screen-reader users hear the carve-out. **NO Console-private mutation path** — the buttons MUST NOT wire to any Protocol call (§13 forbidden-practice "two parallel implementations").
- [ ] Selected-item detail (right rail) renders a pretty-printed JSON value viewer with collapsible nodes; when `ValueArtifact` is populated, the viewer renders a `Truncated` badge + an `Open artifact` link that invokes the already-shipped `artifacts.get` Protocol method.
- [ ] Saved-filter chips, sort preferences, column visibility, and Export ▾ (NDJSON / CSV — Console-local snapshot of current filtered page) persist in Console DB (D-061 — Console-local; NEVER mutate runtime entities).
- [ ] Design tokens only — no raw color/spacing/type-scale literals in `.svelte` files (§13 + Stylelint enforcement).
- [ ] Per-page Playwright spec `web/console/tests/memory-page.spec.ts` covers: (a) catalog table renders rows with all mockup columns, (b) facet chip toggle updates row count, (c) selected-item drill-down opens detail panel with metadata + value viewer, (d) Recent identity rejections card surfaces a `memory.identity_rejected` event with `<missing>` substitution visible, (e) heavy-value row shows `Open artifact` deep-link, (f) bulk-action toolbar buttons are disabled and reveal the tooltip on hover.
- [ ] `scripts/smoke/phase-73j.sh` asserts all three Protocol methods round-trip + cross-tenant rejection without admin + identity-required rejection.

## Files added or changed

```text
internal/protocol/methods/methods.go                  # +memory.list, +memory.get, +memory.health
internal/protocol/types/memory.go                     # +MemoryItem, MemoryFilter, MemoryListResponse, MemoryItemDetail, MemoryHealthAggregate, MemoryHealthResponse
internal/protocol/errors/errors.go                    # confirm ErrIdentityRequired + ErrControlScopeRequired (or add ErrMemoryCrossTenantRequired if Phase 61 sentinel not reusable)
internal/protocol/transports/stream/memory_handler.go   # method dispatch + scope-claim checks
internal/protocol/transports/stream/memory_handler_test.go
internal/memory/protocol/list.go                      # memory.list implementation (filter + pagination + aggregate counters from events.aggregate)
internal/memory/protocol/get.go                       # memory.get implementation (heavy-value bypass via ArtifactStub per D-026)
internal/memory/protocol/health.go                    # memory.health implementation (counters derived from events.aggregate + Snapshot driver-by-scope)
internal/memory/protocol/list_test.go
internal/memory/protocol/get_test.go
internal/memory/protocol/health_test.go
internal/memory/protocol/concurrent_reuse_test.go     # D-025 — N≥100 concurrent memory.list calls against shared MemoryStore
test/integration/memory_page_test.go                  # cross-package: MemoryStore + Protocol transport + identity scope; surfaces memory.identity_rejected
internal/auth/scopes.go                                # +ScopeMemoryRead, +ScopeMemoryCrossTenant (alongside the shipped admin / console:fleet scopes)
web/console/src/routes/memory/+page.svelte
web/console/src/lib/components/memory/MemoryTable.svelte
web/console/src/lib/components/memory/SelectedItemDetail.svelte
web/console/src/lib/components/memory/MemoryHealthCard.svelte
web/console/src/lib/components/memory/RecentIdentityRejectionsCard.svelte
web/console/src/lib/components/memory/RecoveryDropoutsCard.svelte
web/console/src/lib/components/memory/SubHeaderStrip.svelte
web/console/src/lib/components/memory/StrategyOverlayChipRow.svelte
web/console/src/lib/components/memory/BulkActionToolbar.svelte  # V1: disabled-with-tooltip
web/console/src/lib/console-db/saved_filters_memory.ts  # Console DB schema for memory-page saved filters (depends on Phase 72h schema)
web/console/tests/memory-page.spec.ts
web/console/src/lib/protocol.ts                          # REGENERATED ONLY by `make protocol-ts-gen` — never hand-edited
scripts/smoke/phase-73j.sh
docs/glossary.md                                         # +memory.list, +memory.get, +memory.health, +ScopeMemoryRead, +ScopeMemoryCrossTenant
```

## Public API surface

```go
// internal/protocol/types/memory.go

// MemoryItem is one row in the memory page's main table.
// Heavy values are NEVER inlined on this row shape — call memory.get for
// the per-item detail (which produces an ArtifactStub above the heavy
// threshold per D-026).
type MemoryItem struct {
    Key           string             // memory key (per-strategy semantics)
    Strategy      string             // "none" | "truncation" | "rolling_summary" (extensible to "pinned" / "episodic" / "recent" / "persistent" overlay chips post-V1)
    Scope         string             // "session" | "user" | "tenant"
    Identity      identity.Quadruple // tenant + user + session + agent_id
    CreatedAt     time.Time
    LastUpdatedAt time.Time
    ExpiresAt     time.Time          // zero value → no TTL
    SizeBytes     int64              // count only — bytes never inlined on this row
    HeavyContent  bool               // true if SizeBytes ≥ D-026 heavy-content threshold
    Driver        string             // "inmem" | "sqlite" | "postgres"
}

// MemoryFilter is the query payload for memory.list.
type MemoryFilter struct {
    TenantIDs        []string // empty = caller's own tenant; >1 or != caller requires memory.crosstenant claim
    UserIDs          []string
    SessionIDs       []string
    AgentIDs         []string
    Scopes           []string // subset of ["session", "user", "tenant"]
    Drivers          []string // subset of ["inmem", "sqlite", "postgres"]
    Strategies       []string // subset of MemoryStore.Strategy + future overlay chips
    HasTTLExpiring   bool     // when true, only items with ExpiresAt within (now, now+1h]
    ContentSearch    string   // substring against post-redaction value text; runtime-side per brief 11 §CC-4
    Page             int
    PageSize         int      // bounded server-side
}

// MemoryListResponse is the memory.list result. Aggregates derive from
// events.aggregate (Phase 72a) over the memory.* event types — runtime
// side computation per brief 11 §CC-4.
type MemoryListResponse struct {
    Items      []MemoryItem
    Page       int
    PageCount  int
    Aggregates MemoryAggregates
}

type MemoryAggregates struct {
    Total                  int64
    ExpiringIn1h           int64
    IdentityRejected24h    int64 // count from memory.identity_rejected events (D-033)
    RecoveryDropped24h     int64 // count from memory.recovery_dropped events (D-035 — wire string per existing runtime constant)
}

// MemoryItemDetail is the memory.get result. Exactly one of Value /
// ValueArtifact is populated; never both. Above the heavy-content
// threshold (D-026), Value is empty and ValueArtifact is populated.
type MemoryItemDetail struct {
    Item          MemoryItem
    Value         []byte            // post-redaction; populated only when SizeBytes < heavy threshold
    ValueArtifact artifacts.Stub    // populated when SizeBytes ≥ heavy threshold; resolves via artifacts.get
    Metadata      MemoryMetadata
}

type MemoryMetadata struct {
    TTL              time.Duration
    StrategyConfig   map[string]any // bounded, strategy-named knobs surfaced for the right-rail detail
    RelatedEventIDs  []string       // recent event IDs touching this key — for "Inspect related events" deep-link
}

// MemoryHealthAggregate is the memory.health response.
type MemoryHealthAggregate struct {
    Total                int64
    ExpiringIn1h         int64
    IdentityRejected24h  int64
    RecoveryDropped24h   int64
    DriverByScope        map[string]string // e.g. {"session":"inmem", "user":"sqlite", "tenant":"postgres"}
}

type MemoryHealthResponse struct {
    Aggregate MemoryHealthAggregate
}
```

```go
// internal/memory/protocol/list.go
func List(ctx context.Context, store memory.MemoryStore, ev events.Store, filter types.MemoryFilter) (types.MemoryListResponse, error)

// internal/memory/protocol/get.go
func Get(ctx context.Context, store memory.MemoryStore, art artifacts.Store, key string, id identity.Quadruple) (types.MemoryItemDetail, error)

// internal/memory/protocol/health.go
func Health(ctx context.Context, store memory.MemoryStore, ev events.Store, id identity.Quadruple) (types.MemoryHealthResponse, error)
```

## Test plan

- **Unit:**
  - `list_test.go` — filter combinations: every facet axis tested in isolation + an "all facets" combination; aggregates math; pagination boundaries.
  - `get_test.go` — heavy-content threshold cutoff: assert `Value` populated below threshold and zero above; assert `ValueArtifact` populated above threshold and `artifacts.Stub.Ref` resolves; **negative test: a constructed driver that returns raw bytes above threshold causes `Get` to fail loudly with `ErrContextLeak`** (D-026 closure mirrored at the memory-inspector edge).
  - `health_test.go` — counter arithmetic: deliberately emit N `memory.identity_rejected` + M `memory.recovery_dropped` events; query `memory.health`; assert the counters match. Driver-by-scope is derived from the live `MemoryStore.Snapshot(...)` per-scope; assert the mapping reflects the configured drivers.
  - `concurrent_reuse_test.go` — N=100 concurrent `memory.list` calls with overlapping filters against a single shared `MemoryStore` under `-race` (D-025). Assert no data races, no context bleed (each goroutine's filter + identity quadruple preserved), no cross-cancellation, baseline `runtime.NumGoroutine` restored after all calls return.

- **Integration:**
  - `test/integration/memory_page_test.go` — real `MemoryStore` (inmem driver first; conformance against sqlite + postgres in the conformance bucket), real Protocol transport, real `events/drivers/inmem` event store, real `artifacts/drivers/inmem` artifact store. Wires the full stack: client subscribes to `memory.identity_rejected` per Phase 72a-extended `events.subscribe`; calls `memory.list` for the caller's own quadruple → 200 OK; calls `memory.list` with a foreign tenant filter WITHOUT `memory.crosstenant` claim → `ErrControlScopeRequired`; calls `memory.list` with a deliberately empty `session_id` in the request identity → `ErrIdentityRequired` AND the integration test asserts a `memory.identity_rejected` event reaches the operator's subscriber stream. Heavy-value round-trip: a deliberately oversized value materialised into the store → `memory.get` returns `ValueArtifact` only → the test calls `artifacts.get` against the returned stub → bytes round-trip. Concurrency stress: N=10 concurrent producers + N=10 concurrent `memory.list` subscribers across two tenants under `-race`; assert no cross-tenant leakage. (See AGENTS.md §17 — real drivers everywhere on the seam, ≥1 failure mode, identity propagation, under `-race`.)

- **Conformance:**
  - The three Protocol methods run against the Phase 62 Protocol conformance suite — every transport (HTTP+SSE / WebSocket / stdio) emits identical wire shapes for each method.
  - `memory.list` / `memory.get` / `memory.health` run against the existing memory-driver conformance suite (`internal/memory/conformancetest`) in the in-mem / SQLite / Postgres drivers — all three return identical row counts + identical `MemoryItem` shapes for the same fixture state.

- **Concurrency / leak:**
  - `concurrent_reuse_test.go` (above) — D-025 baseline.
  - Integration concurrency stress (above) — wave-level cross-package.

- **UI (Playwright):**
  - `memory-page.spec.ts` — see acceptance criteria item.

## Smoke script additions

`scripts/smoke/phase-73j.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'memory/list' '{}'` → assert 200 once the surface lands; SKIP under 404/405/501 until then. Assert `assert_json_path '.items | type' 'array'`.
- `protocol_call 'memory/list' '{"filter": {"scopes": ["session"]}}'` → assert scope facet honoured.
- `protocol_call 'memory/list' '{"filter": {"strategies": ["truncation"]}}'` → assert strategy facet honoured.
- `protocol_call 'memory/get' '{"key": "<fixture-key>"}'` → assert 200; assert exactly one of `.value` / `.value_artifact` populated; assert `assert_json_path '.item.key' '<fixture-key>'`.
- `protocol_call 'memory/health' '{}'` → assert 200; assert `assert_json_path '.aggregate.total | type' 'number'`.
- `protocol_call 'memory/list' '{"filter": {"tenant_ids": ["t-other"]}}'` (without `memory.crosstenant` claim) → `assert_status 403`.
- `protocol_call 'memory/list' '{}'` with a deliberately empty session in the identity scope → `assert_status 401` or `403` (the runtime fails closed per D-033; smoke accepts either canonical identity-required code).
- `assert_status 200 /console/memory` → SvelteKit page route — SKIP until 73m's `harbor console` subcommand ships (the static asset is served by `harbor console`, not `harbor dev`).

## Coverage target

- `internal/memory/protocol`: 85%.
- `internal/protocol/transports/stream` (memory handler): 80%.
- `internal/protocol/types/memory.go`: 90% (struct serialization paths).
- `internal/auth/scopes.go` (new scope constants): N/A (pure constants — exercised by handler tests).
- `web/console/src/routes/memory/`: 70% (Svelte component coverage via `svelte-check` + Playwright).

## Dependencies

**Same-wave (Wave 13, Stage 1):**

- Phase 72 (`events.subscribe` scope foundation — required for the right-rail Recent identity rejections card subscription).
- Phase 72a (`events.subscribe` filter extensions + `events.aggregate` — `memory.health` counters derive from `events.aggregate` over `memory.*` event types).
- Phase 72h (Console DB schema — saved-filter chips, sort preferences, column visibility, Export ▾).
- Phase 75 (Playwright harness baseline — per-page spec `memory-page.spec.ts` lands here).

**Already shipped (pre-Wave 13):**

- Phase 23 (MemoryStore iface + InMem driver — `Shipped`; provides `MemoryStore.Snapshot` + `Health`).
- Phase 24 (Memory strategies — `Shipped`; provides `truncation` + `rolling_summary` strategy executors + `memory.health_changed` + `memory.recovery_dropped` events).
- Phase 25 (SQLite + Postgres memory drivers — `Shipped`; provides the persistent drivers; D-035 recovery-dropped event under the same wire string).
- Phase 60 (Protocol wire transport — `Shipped`).
- Phase 61 (Protocol auth — `Shipped`; supplies the scope-claim primitive + JWT validator).
- Phase 73 parent (state inspection — `Shipped`; provides `artifacts.get` consumed by the Selected-item-detail heavy-value `Open artifact` link).

## Risks / open questions

- **Aggregate counter cost.** `memory.health` derives counters from `events.aggregate` over a 24-h window. For high-cardinality tenants this is an O(N) scan per call. V1 accepts the cost (brief 11 §CC-4 high-cardinality posture); post-V1 may add a continuous-aggregate materialized view.
- **Heavy-value materialisation cost.** A `memory.get` against a heavy-value key materialises through `artifacts.put` to produce the stub. If the value is already in the store as bytes, the materialisation is a copy; the cost is bounded by the per-call heavy-threshold ceiling. Acceptable for V1.
- **`memory.overflow_drop_oldest` event-name drift (page-memory.md §12).** The §12 mockup refinements name the event `memory.overflow_drop_oldest`; the shipped runtime constant is `memory.recovery_dropped` per D-035. This phase uses the shipped name; a follow-up `docs(design)` PR should reconcile the §12 wording.
- **Console DB schema for saved filters lives in Phase 72h.** This phase's `saved_filters_memory.ts` adds a memory-specific table on top of the 72h base schema; if 72h ships a different shape, this table definition needs to match.
- **Per-page Playwright spec coverage.** The wave-end 75a aggregator suite enumerates every page spec and asserts a matching `*.spec.ts` exists. The 73j spec MUST be merged before 75a's enumeration runs — i.e. before the final Stage-2.3 PR.
- **`memory.crosstenant` scope claim** is a new scope, not yet declared in `internal/auth/scopes.go`. Adding it is a Phase 61 extension at the auth-layer constants level; no new validator code; the claim flows through `auth.HasScope` like the shipped `admin` / `console:fleet` scopes.
- **§13 forbidden-practice mirror.** This phase introduces a NEW Protocol surface (3 methods) — the §13 primitive-with-consumer rule is satisfied trivially by the page UI in the same phase. No extension to §13's two consequence-clauses (`SpawnTask`/`AwaitTask` twinning, pause-primitive producer) is needed here.

## Glossary additions

- **`memory.list`** — Protocol method returning paginated memory items for the caller's identity scope, with optional facet filters (scope / driver / strategy / TTL-expiring / content-search) + aggregate counters (Total / Expiring1h / IdentityRejected24h / RecoveryDropped24h). Added in Phase 73j. Read-only at V1.
- **`memory.get`** — Protocol method returning the full detail of a single memory item: metadata + post-redaction value (when below the heavy-content threshold per D-026) OR `ValueArtifact ArtifactStub` (above threshold). NEVER inlines bytes ≥ heavy threshold. Added in Phase 73j.
- **`memory.health`** — Protocol method returning aggregate memory health counters (Total / Expiring1h / IdentityRejected24h / RecoveryDropped24h) + per-scope driver mapping. Counters derive from `events.aggregate` over `memory.*` event types. Added in Phase 73j.
- **`ScopeMemoryRead`** — Phase 61 scope claim required to call `memory.list` / `memory.get` / `memory.health` within the caller's own identity quadruple. Added in Phase 73j.
- **`ScopeMemoryCrossTenant`** — Phase 61 scope claim required to widen `MemoryFilter.TenantIDs` beyond the caller's own tenant. D-079 pattern. Added in Phase 73j.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes (`web/console/src/lib/protocol.ts` regenerated from `CanonicalWireTypes` per D-093)
- [ ] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092)
- [ ] `npm run lint` passes in `web/console/` (no raw color / spacing literals per §13)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the three new methods all touch identity; the integration test asserts cross-tenant rejection without `memory.crosstenant` + identity-required failure-loud on missing session_id)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `memory.list` calls against a single shared `MemoryStore` under `-race` (D-025)
- [ ] **Integration test exists** — `test/integration/memory_page_test.go` wires real `MemoryStore` + real Protocol transport + real events store + identity propagation under `-race`; asserts `memory.identity_rejected` surfaces in the operator's subscriber stream (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/memory-page.spec.ts` exists and passes (binding for every 73x phase per the decomposition doc §12 lock-in)
- [ ] Glossary updated with the 5 new entries (`memory.list`, `memory.get`, `memory.health`, `ScopeMemoryRead`, `ScopeMemoryCrossTenant`)
- [ ] **D-033 invariant honoured** — `memory.identity_rejected` events surface in the Recent identity rejections card verbatim, `<missing>` substitution preserved, NO "view rejected memory anyway" affordance (§13 forbidden-practice)
- [ ] **D-026 invariant honoured** — heavy memory values route through `artifacts.get` via `ValueArtifact`; no inline bytes above threshold; constructed-driver negative test asserts `ErrContextLeak` on raw-bytes shape
- [ ] **D-065 invariant honoured** — no session-level priority field anywhere; `Pinned` strategy chip is a Phase 24 strategy, not a priority
- [ ] **D-066 invariant honoured** — bulk-action mutation toolbar disabled-with-tooltip at V1; the buttons MUST NOT wire to any Protocol mutation call (mutation surface deferred to Phase 73)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (one departure recorded — event-name drift in page-memory.md §12; tracked as a `docs(design)` follow-up, NOT an RFC-level departure)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (decomposition doc §12 lock-in item 3 + the binding coordinator-verify protocol)
