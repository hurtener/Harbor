# Phase 73e — Console Agents page (Protocol + UI bundled)

## Summary

Bundles the heaviest single Wave 13 phase: 8 NEW Protocol methods exposing the Agent Registry (`agents.list`, `agents.get`, `agents.tools`, `agents.memory`, `agents.governance`, `agents.skills`, `agents.permissions`, `agents.metrics`), plus the page UI (cards grid + detail view with 6 tabs + topology mini-graph + recent activity + control buttons) + per-page Playwright spec. The operator's binding allocation per the decomposition doc §12 lock-in #3: 2x agent time + mandatory coordinator-verify pass (no trust on agent completion signal).

## RFC anchor

- RFC §6.4 (tool catalog and transports)
- RFC §6.16 (Agent Registry)
- RFC §7 (Console layer)
- RFC §7.2 (Agents are NOT chatbots)
- RFC §7.4 (CLI scaffolding owns agent authoring)

## Briefs informing this phase

- brief 11 (Console feature surface — "Agents view", "Agent Detail view" sub-sections, "Open architectural questions" #1 agent-as-Protocol-principal)
- brief 12 (deployment + two-surface model — "Open architectural questions Brief 11 raised, resolved here", agent management surface required)
- brief 09 (agent-as-actor / agent-bound OAuth — agent_id keys agent-bound tokens)

## Brief findings incorporated

- brief 11 §"Agents view": agents carry rich operational metadata (planner / model / tools / memory / cost / OAuth / activity) — the page surfaces all of it via specialised Protocol methods rather than overloading `agents.get`. This phase's split into 8 methods follows that recommendation directly.
- brief 11 §"Open architectural questions" #1: the agent is a Protocol-addressable principal with a registration identity (`agent_id`), but **`agent_id` is NOT an isolation principal** (D-059) — the isolation tuple is `(tenant, user, session)`. Every method here filters by the tuple, never by `agent_id` as a `WHERE` clause for security.
- brief 12 §"Open architectural questions Brief 11 raised, resolved here": agent management is a binding V1 surface for the Console; Brief 11's open question is closed in favor of full agent inspector + fleet-control affordances.
- brief 09 §"agent-bound OAuth": the Tools tab renders per-binding OAuth scope (`auth.BindingScope` per D-083) with Connect / Reconnect / Revoke; these actions invoke the SHIPPED `tool.auth_required` flow — no parallel implementation.

## Findings I'm departing from (if any)

None.

## Goals

- Ship 8 NEW Protocol methods on the runtime side, each scope-checked, redaction-aware, and identity-mandatory.
- Ship the Agents list mode + detail mode page UI matching `docs/rfc/assets/console-agents-page.png`.
- Ship the per-page Playwright spec covering: list-mode card render, filter, search, detail navigation, every detail tab, every control button (Pause / Drain / Restart / Force-Stop / Deregister), OAuth Connect / Reconnect / Revoke per binding.
- Control-plane verbs (Pause / Drain / Restart / Force-Stop / Deregister) MUST invoke the EXISTING shipped registry control methods (Phase 53a) — no parallel implementation, no new wire types.

## Non-goals

- Authoring agents in the Console (create / edit). RFC §7.4 — scaffolding is in `harbor dev` + CLI; Console is inspector only (page-agents.md §10).
- Versioning & rollback dashboards (success-rate-over-`version_hash`, baseline promotion). D-064 — post-V1.
- Permissions ACL editor when permissions are implicit (V1 default). Surface lands when `agents.permissions` matures.
- Cross-runtime agent aggregator. D-091 — post-V1.
- Per-agent theming / branding. Post-V1.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The Agents page composes the shared inventory like every other catalog page; its registration-detail rail is a `DetailRail` of `RailCard`s, never a bespoke renderer.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/agents/` and is served at `/agents` with
  **no `/console/` URL prefix** (the `(console)` route group is a
  layout-grouping device and does not appear in the URL). Detail views live at
  `(console)/agents/[id]/` and are served at `/agents/<id>`. All inter-page
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
  page-specific components go in `components/agents/`.
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

- [ ] `internal/protocol/methods/methods.go` declares 8 new method names: `agents.list`, `agents.get`, `agents.tools`, `agents.memory`, `agents.governance`, `agents.skills`, `agents.permissions`, `agents.metrics`.
- [ ] `internal/protocol/types/agents.go` defines the Protocol projections: `Agent`, `AgentFilter`, `AgentConfig` (planner config + model + max-steps), `AgentToolBinding`, `AgentMemoryBinding`, `AgentGovernance`, `AgentSkillBinding`, `AgentPermissions`, `AgentMetrics` — single source of truth (D-002).
- [ ] All 8 methods enforce **identity is mandatory** — missing `tenant_id` / `user_id` / `session_id` → `ErrIdentityRequired` (NEVER silently downgrade).
- [ ] Methods filter by `(tenant, user, session)` per CLAUDE.md §6 — NEVER by `agent_id` as an isolation key.
- [ ] `agents.list` returns paginated agents the operator's identity-tuple can use; admin scope returns every agent in the runtime regardless of tenant.
- [ ] `agents.get` returns the full Protocol projection — registration identity (`agent_id` / `incarnation` / `version_hash` per D-068) + hosting (locally-hosted vs remote per D-060) + status + health + `AgentConfig`.
- [ ] `agents.tools` joins the agent's tool-binding records to `tools.Tool`-shaped projections; per-binding OAuth status (`Bound` / `Required` / `Expired` per D-083) + token freshness.
- [ ] `agents.memory` returns configured memory strategy (Phase 24 strategy id), TTLs, scope (session / user / tenant).
- [ ] `agents.governance` returns per-identity-tier ceilings (Phase 36a) + current spend + rate-limit posture (Phase 36b).
- [ ] `agents.skills` returns agent-attached skills (Phase 38 + Phase 41 generated skills).
- [ ] `agents.permissions` — V1 default returns implicit policy ("every authenticated user in the tenant can invoke this agent"); admin operator may see a future ACL.
- [ ] `agents.metrics` returns the registry-wide rollup (Active Agents / Running Tasks / Total Cost / Total Tokens) for the operator's scope.
- [ ] Control-plane verbs (`Pause` / `Drain` / `Restart` / `Force-Stop` / `Deregister`) invoke the EXISTING shipped registry control methods (`registry.Pause`, `registry.Drain`, `registry.Restart`, `registry.ForceStop`, `registry.Deregister`) gated by D-066's control-scope claim. NO new control method.
- [ ] OAuth Connect / Reconnect / Revoke invoke the SHIPPED `tool.auth_required` event flow (D-083). NO new auth method.
- [ ] `web/console/src/routes/agents/+page.svelte` (list mode) renders the agent cards grid + filter bar + top metrics rollup per the mockup.
- [ ] `web/console/src/routes/agents/[agent_id]/+page.svelte` (detail mode) renders the header (status badge + version_hash + control buttons), 6-tab strip (Identity / Autonomy / Tools / Memory / Cost / Skills), topology mini-graph, recent activity feed, connected tools panel, memory strategy summary.
- [ ] All data flows go through the typed Protocol client (`web/console/src/lib/protocol.ts`, D-093). NO hand-rolled `fetch` in `.svelte` files.
- [ ] Design tokens only — no raw color/spacing/type-scale literals (§13).
- [ ] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092).
- [ ] Per-page Playwright spec at `web/console/tests/agents-page.spec.ts` covers: list-mode render, filter+search, card → detail navigation, every detail tab, every control button (Pause / Drain / Restart / Force-Stop / Deregister) — each as a scope-claim degradation test (disabled when missing, succeeds when present), OAuth Connect / Reconnect / Revoke.
- [ ] `scripts/smoke/phase-73e.sh` asserts every new method and the page route.
- [ ] **Concurrent-reuse test:** N≥100 concurrent `agents.list` calls against a single shared registry projection under `-race` (D-025).
- [ ] **Integration test:** `test/integration/agents_page_test.go` — real Agent Registry + Protocol transport + identity scope; cross-tenant rejection without admin claim; control verb degradation without control claim.

## Files added or changed

```text
internal/protocol/methods/methods.go                          # +8 agents.* methods
internal/protocol/types/agents.go                             # +Agent, AgentFilter, AgentConfig, AgentToolBinding, AgentMemoryBinding, AgentGovernance, AgentSkillBinding, AgentPermissions, AgentMetrics
internal/protocol/transports/stream/agents_handler.go         # method dispatch + scope-claim checks
internal/protocol/transports/stream/agents_handler_test.go
internal/runtime/registry/protocol/list.go                    # agents.list (filter + pagination + identity scope)
internal/runtime/registry/protocol/get.go                     # agents.get (full projection)
internal/runtime/registry/protocol/tools.go                   # agents.tools (joins registry + tools/auth)
internal/runtime/registry/protocol/memory.go                  # agents.memory (joins registry + memory configs)
internal/runtime/registry/protocol/governance.go              # agents.governance (joins registry + Phase 36 governance)
internal/runtime/registry/protocol/skills.go                  # agents.skills (joins registry + skills catalog)
internal/runtime/registry/protocol/permissions.go             # agents.permissions (V1 default implicit)
internal/runtime/registry/protocol/metrics.go                 # agents.metrics (rollup aggregate)
internal/runtime/registry/protocol/*_test.go                  # one _test.go per file
internal/runtime/registry/protocol/concurrent_reuse_test.go   # D-025 — N≥100 concurrent reads against shared registry
test/integration/agents_page_test.go
web/console/src/routes/agents/+page.svelte
web/console/src/routes/agents/[agent_id]/+page.svelte
web/console/src/lib/components/agents/CardsGrid.svelte
web/console/src/lib/components/agents/AgentCard.svelte
web/console/src/lib/components/agents/TopMetricsRollup.svelte
web/console/src/lib/components/agents/FilterBar.svelte
web/console/src/lib/components/agents/DetailHeader.svelte
web/console/src/lib/components/agents/ControlButtons.svelte
web/console/src/lib/components/agents/IdentityTab.svelte
web/console/src/lib/components/agents/AutonomyTab.svelte
web/console/src/lib/components/agents/ToolsTab.svelte
web/console/src/lib/components/agents/MemoryTab.svelte
web/console/src/lib/components/agents/CostTab.svelte
web/console/src/lib/components/agents/SkillsTab.svelte
web/console/src/lib/components/agents/TopologyMiniGraph.svelte
web/console/src/lib/components/agents/RecentActivityFeed.svelte
web/console/src/lib/components/agents/OAuthBindingRow.svelte
web/console/tests/agents-page.spec.ts
cmd/harbor-gen-protocol-ts/                                   # this phase regenerates protocol.ts
web/console/src/lib/protocol.ts                               # REGENERATED ONLY by make protocol-ts-gen
scripts/smoke/phase-73e.sh
docs/glossary.md                                              # +8 agents.* method entries, +AgentConfig (Protocol projection)
```

## Public API surface

```go
// internal/protocol/types/agents.go
type Agent struct {
    ID            string  // agent_id (registration identity per D-059) — NOT an isolation principal
    Name          string
    Description   string
    Incarnation   int64
    VersionHash   string  // SHA-256 over canonical JSON of AgentConfig per D-068
    Owner         string  // admin who registered
    Status        string  // "active" | "paused" | "drained" | "force_stopped" | "deregistered"
    Health        string  // "Healthy" | "Degraded" | "Paused" | "Drained" | "Force-Stopped"
    PlannerType   string  // "react" | "deterministic" | future
    Model         string
    ToolsCount    int
    MCPCount      int
    SessionsToday int64
    UsersAuthorized int64
}

type AgentFilter struct {
    Status      []string
    PlannerType []string
    Search      string  // free-text over Name + Description
    Page        int
    PageSize    int
}

type AgentConfig struct {
    Planner   PlannerConfig // MaxSteps, repair policy, etc.
    Model     string
    MaxTokens int
    Cost      CostConfig    // per-identity-tier ceilings (Phase 36a)
}

type AgentToolBinding struct {
    ToolID       string
    Transport    string
    AuthStatus   string  // "no_auth" | "headers" | "oauth_user_bound" | "oauth_agent_bound" | "oauth_expired"
    BindingScope string  // auth.BindingScope: "user" | "agent" (D-083)
    LastUsed     time.Time
}

type AgentMemoryBinding struct {
    StrategyID string  // Phase 24 strategy id
    TTL        time.Duration
    Scope      string  // "session" | "user" | "tenant"
}

type AgentGovernance struct {
    Ceilings    map[string]CostCeiling  // per identity tier (Phase 36a)
    CurrentSpend map[string]float64
    RateLimits   map[string]RateLimit    // per identity tier (Phase 36b)
}

type AgentMetrics struct {
    ActiveAgents int64
    RunningTasks int64
    TotalCost    float64
    TotalTokens  int64
}
```

## Test plan

- **Unit:**
  - Each of `list.go` / `get.go` / `tools.go` / `memory.go` / `governance.go` / `skills.go` / `permissions.go` / `metrics.go` carries its own `_test.go` with table-driven coverage: identity-rejection, scope-claim gating, projection shape.
- **Integration:**
  - `test/integration/agents_page_test.go` — real Agent Registry from Phase 53a + real Protocol transport. Tests: (a) `agents.list` returns operator's accessible agents; (b) cross-tenant `agents.list` without admin claim → 403; (c) control verb without control-scope claim → 403 + no registry mutation; (d) OAuth Connect flow round-trips through shipped `tool.auth_required`.
- **Conformance:**
  - All 8 methods run against the Protocol conformance suite (Phase 62 — Shipped) — every transport emits identical wire shapes.
- **Concurrency / leak:**
  - `concurrent_reuse_test.go` — N=100 concurrent calls each to `agents.list` / `agents.get` / `agents.tools` against a single shared registry projection under `-race`. Asserts no data races, no context bleed, baseline goroutine count restored (D-025).
- **UI (Playwright):**
  - `agents-page.spec.ts` — list mode renders cards; filter narrows; click card → detail; tab strip cycles through Identity / Autonomy / Tools / Memory / Cost / Skills; control buttons (Pause / Drain / Restart / Force-Stop / Deregister) each test for: visible-and-enabled when claim present, disabled-with-tooltip when claim missing; OAuth Connect opens authorize URL in popup; Reconnect refreshes token; Revoke clears binding.

## Smoke script additions

`scripts/smoke/phase-73e.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'agents/list' '{}'` → returns paginated agents.
- `protocol_call 'agents/get' '{"id": "agent-smoke-fixture"}'` → returns projection.
- `protocol_call` for each of the remaining 6 methods.
- Cross-tenant `agents.list` without admin → expect 403.
- Control verb (`registry.Pause` etc.) without control claim → expect 403.
- Page route probe (SKIPped until 73m's `harbor console` lands).

## Coverage target

- `internal/runtime/registry/protocol`: 85%.
- `internal/protocol/transports/stream`: 80%.
- `web/console/src/routes/agents/`: 70% (via `svelte-check` + Playwright).

## Dependencies

**Same-wave (Wave 13, Stage 1):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72a (events filter shape — `agents.metrics` consumes `events.aggregate`)
- Phase 72c (search.agents — Console-side per Brief 11 §CC-4)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 30 (tool-side OAuth — `Shipped`; supplies `tool.auth_required` event flow)
- Phase 36a (Cost accumulator + per-identity ceilings — `Shipped`)
- Phase 36b (Per-identity rate limits + MaxTokens — `Shipped`)
- Phase 38 (Skills subsystem — `Shipped`)
- Phase 41 (Generated skills — `Shipped`)
- Phase 53a (Agent Registry + `agent.*` event taxonomy + `registry.*` control verbs — `Shipped`)
- Phase 54 (Protocol task control surface — `Shipped`)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)
- Phase 64a (tool catalog OAuth + approval wiring — `Shipped`)

## Risks / open questions

- **Cost of `agents.metrics` aggregate.** Computing Active Agents / Running Tasks / Total Cost / Total Tokens may require scanning tasks + governance accumulators per request. V1 accepts the cost; post-V1 may cache.
- **`agents.permissions` V1 default is implicit.** The Protocol method returns "every authenticated user in the tenant" until an explicit ACL surface lands. Per spec page §10 — explicit ACL editor is post-V1.
- **OAuth Reconnect / Revoke flow.** Console initiates via `tool.auth_required`; the runtime owns token storage (`tools/auth`, D-083). Console must never receive the raw token in any payload — verify the Protocol projection strips it before delivery.
- **Topology mini-graph aggregation.** Aggregated over recent runs via a `topology.snapshot`-derived projection. Phase 74's topology.snapshot ships the primitive; this phase's `agents.tools` must NOT re-implement topology — it consumes the aggregate.
- **`registry.Deregister` is irreversible.** UI MUST confirm twice + audit event. The shipped `registry.Deregister` already audit-emits; UI confirmation modal is added here.

## Glossary additions

- **`agents.list`** — Protocol method returning paginated agents accessible to the caller's identity scope.
- **`agents.get`** — Protocol method returning the full projection of one agent (registration identity, hosting, status, health, AgentConfig).
- **`agents.tools`** — Protocol method returning the agent's tool bindings + per-binding OAuth status.
- **`agents.memory`** — Protocol method returning the agent's memory strategy + TTL + scope.
- **`agents.governance`** — Protocol method returning the agent's per-identity-tier ceilings + spend + rate-limit posture.
- **`agents.skills`** — Protocol method returning the agent's attached skills.
- **`agents.permissions`** — Protocol method returning the agent's permission model (V1 default: implicit).
- **`agents.metrics`** — Protocol method returning the registry-wide rollup for the operator's scope.
- **`AgentConfig`** — Protocol projection of an agent's planner + model + cost configuration (NOT the runtime-side `registry.AgentSnapshot`; the wire shape is flat per RFC §5.1).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes (8 new Protocol type clusters regenerate `protocol.ts`)
- [ ] `svelte-check --fail-on-warnings` passes
- [ ] `npm run lint` passes in `web/console/` (no raw color / spacing literals)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **Multi-isolation paths changed** — cross-session/cross-tenant tests pass (the 8 new methods all touch identity; integration test asserts cross-tenant rejection)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent reads against shared registry projection under `-race` (D-025)
- [ ] **Integration test passes** — `test/integration/agents_page_test.go` with real Registry + Protocol transport (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/agents-page.spec.ts`
- [ ] Glossary updated with 8 new method names + AgentConfig
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete + 2x agent time allocated** per decomposition doc §12 lock-in #3 — the coordinator MUST NOT trust the agent's completion signal; the coordinator reads the produced files, greps for shipped-vs-`[wave-13-extends]` mistakes (the 8 methods here are all `[wave-13-extends]`; only `registry.Pause/Drain/Restart/ForceStop/Deregister` and `tool.auth_required` are `[shipped]`), audits §13 compliance, and only THEN advances.
