# Phase 73i — Console Flows page (Protocol + UI bundled)

## Summary

Ships the Flows page Protocol surface and UI as a single Stage 2.1 phase. Protocol additions: `flows.list` (with aggregate metrics), `flows.describe` (engine-graph payload), `flows.runs.list`, `flows.runs.describe`, `flows.run`, `flows.metrics`. UI: catalog table + Flow Metrics card + read-only engine graph canvas + Budget meter + Run history table + selected-run summary panel + per-page Playwright spec. **Authoring is OUT of V1** per D-063 (page is view-only with `Run flow` as the only mutating action).

## RFC anchor

- RFC §6.1 (Core runtime — engine)
- RFC §6.2 (Planner interface)
- RFC §7 (Console layer)
- RFC §12 (post-V1 future work — flows authoring)

## Briefs informing this phase

- brief 11 (Console feature surface — "Flows view")
- brief 12 (deployment + two-surface model)

## Brief findings incorporated

- brief 11 §"Flows view": "operators inspect registered flows, understand their topology, and run them — but they don't author them in the Console at V1". This phase honors that exactly: 6 read methods + 1 run method, NO author methods.
- brief 11 §LR shared component family: the engine graph canvas uses the SAME renderer family as the Live Runtime topology view (Phase 73b). Sharing this renderer reduces V1 code mass; it's added to `web/console/src/lib/components/graph/` so both pages consume it.
- brief 12 §"two-surface model": all heavy content (run outputs ≥ heavy-content threshold per D-026) routes through `artifacts.get`. NO inline bytes in `flows.runs.describe` payloads.

## Findings I'm departing from (if any)

None.

## Goals

- Ship 6 NEW Protocol methods on the runtime side, each scope-checked + identity-mandatory + redacted on emit.
- Ship the Flows page UI matching `docs/rfc/assets/console-flows-page.png` — view-only per D-063.
- The engine graph canvas renderer (`web/console/src/lib/components/graph/EngineGraphCanvas.svelte`) is shared with Phase 73b Live Runtime — establish the contract here.
- Per-page Playwright spec covering: catalog table, Run flow button (with scope-claim degradation test), Compare versions (Console-local diff), Run history → run summary panel.

## Non-goals

- Authoring flows in the Console (Add node / Delete edge / Save graph / New flow). D-063 — post-V1.
- YAML descriptor editing. D-023's V1 path is Go-coded; YAML is V1.1.
- `flows.set_budget` (per-flow Budget edit). Post-V1 per page-flows.md §10.
- Evaluation hooks ("Convert to evaluation" / "Run benchmark"). D-064 — post-V1.
- Cross-runtime aggregator. D-091 — post-V1.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares 6 new method names: `flows.list`, `flows.describe`, `flows.runs.list`, `flows.runs.describe`, `flows.run`, `flows.metrics`.
- [ ] `internal/protocol/types/flows.go` defines the Protocol projections: `Flow` (catalog row with aggregates), `FlowFilter`, `FlowDescription` (engine-graph nodes + edges + per-node descriptor + `NodePolicy` + per-flow `Budget` per D-023), `FlowRun` (run history row), `FlowRunDescription` (per-node timeline + output preview), `FlowRunRequest` (input form), `FlowMetrics` (sparkline aggregates).
- [ ] All 6 methods enforce identity-mandatory (`tenant_id` / `user_id` / `session_id` required); cross-tenant filter requires admin scope per D-079.
- [ ] `flows.run` requires the `flows.run` scope claim per D-066; degrades to 403 without the claim.
- [ ] `flows.describe` returns the engine graph (nodes + edges + per-node descriptors + per-node `NodePolicy`); the source-of-truth reference (Go path or YAML descriptor per D-023) is a STRING, never executable code.
- [ ] `flows.runs.describe` returns heavy outputs via `ArtifactRef` per D-026 — NEVER inlines bytes.
- [ ] The engine graph canvas at `web/console/src/lib/components/graph/EngineGraphCanvas.svelte` is shared with 73b Live Runtime — establish the typed `GraphInput` interface here.
- [ ] The page renders mockup-aligned: catalog table + Flow Metrics card + detail header with `Run this flow ▶` + engine graph canvas + Budget meter + Run history table + selected-run summary panel + footer.
- [ ] No author affordances render (`Add node`, `Delete edge`, `Save graph`, `New flow` are ABSENT, not disabled-with-tooltip). Per D-063 + §13 ("no two parallel implementations" — there's no V1 authoring path to half-render).
- [ ] All data flows go through the typed Protocol client (`web/console/src/lib/protocol.ts`, D-093). NO hand-rolled `fetch` calls.
- [ ] Design tokens only — no raw color / spacing / type-scale literals (§13).
- [ ] `svelte-check --fail-on-warnings` passes (D-092).
- [ ] Per-page Playwright spec at `web/console/tests/flows-page.spec.ts` covers: catalog renders rows; selected-flow detail header; engine graph canvas renders nodes + edges; `Run flow` invokes `flows.run` when claim present, disabled-with-tooltip when missing; Run history click → summary panel; heavy output → `Open artifact` link.
- [ ] `scripts/smoke/phase-73i.sh` asserts all 6 Protocol methods round-trip.
- [ ] **Concurrent-reuse test:** N≥100 concurrent `flows.list` / `flows.describe` calls against a shared runtime under `-race` (D-025).
- [ ] **Integration test:** `test/integration/flows_page_test.go` — real engine + Protocol transport + identity scope; cross-tenant rejection without admin; `flows.run` rejection without claim; under `-race`.

## Files added or changed

```text
internal/protocol/methods/methods.go                   # +6 flows.* methods
internal/protocol/types/flows.go                       # +Flow, FlowFilter, FlowDescription, FlowRun, FlowRunDescription, FlowRunRequest, FlowMetrics
internal/protocol/transports/stream/flows_handler.go
internal/protocol/transports/stream/flows_handler_test.go
internal/runtime/flow/protocol/list.go                 # flows.list (aggregates over engine registry)
internal/runtime/flow/protocol/describe.go             # flows.describe (graph payload)
internal/runtime/flow/protocol/runs_list.go            # flows.runs.list
internal/runtime/flow/protocol/runs_describe.go        # flows.runs.describe (heavy outputs → ArtifactRef)
internal/runtime/flow/protocol/run.go                  # flows.run (scope-claim gated)
internal/runtime/flow/protocol/metrics.go              # flows.metrics (consumes flow.* events from 72a aggregate)
internal/runtime/flow/protocol/*_test.go
internal/runtime/flow/protocol/concurrent_reuse_test.go
test/integration/flows_page_test.go
web/console/src/lib/components/graph/EngineGraphCanvas.svelte    # SHARED — also consumed by 73b Live Runtime
web/console/src/lib/components/graph/GraphNode.svelte
web/console/src/lib/components/graph/GraphEdge.svelte
web/console/src/lib/components/graph/types.ts                    # GraphInput typed interface
web/console/src/routes/flows/+page.svelte
web/console/src/routes/flows/[flow_id]/+page.svelte
web/console/src/lib/components/flows/CatalogTable.svelte
web/console/src/lib/components/flows/FlowMetricsCard.svelte
web/console/src/lib/components/flows/DetailHeader.svelte
web/console/src/lib/components/flows/BudgetMeter.svelte
web/console/src/lib/components/flows/RunHistoryTable.svelte
web/console/src/lib/components/flows/RunSummaryPanel.svelte
web/console/src/lib/components/flows/RunFlowModal.svelte
web/console/src/lib/components/flows/CompareVersions.svelte     # Console-local diff (D-061)
web/console/tests/flows-page.spec.ts
web/console/src/lib/protocol.ts                                 # REGENERATED via make protocol-ts-gen
scripts/smoke/phase-73i.sh
docs/glossary.md                                                # +6 flows.* method entries, +Flow/FlowDescription/FlowRun/FlowMetrics types
```

## Public API surface

```go
// internal/protocol/types/flows.go
type Flow struct {
    ID            string
    Name          string
    Owner         string
    Version       string
    Runs24h       int64
    P50Latency    time.Duration
    P95Latency    time.Duration
    SuccessRate   float64
    LastRun       time.Time
    Budget        FlowBudget // per D-023 — token cap + cost cap + request cap
}

type FlowDescription struct {
    Flow    Flow
    Nodes   []FlowNode
    Edges   []FlowEdge
    Source  string  // Go path or YAML reference (NEVER executable code)
}

type FlowNode struct {
    ID         string
    Type       string  // "subflow" | "tool" | "pause" | "artifact_emitter"
    Descriptor string  // schema reference
    NodePolicy *NodePolicy  // retry, timeout, etc.
}

type FlowRunRequest struct {
    FlowID string
    Inputs map[string]any
}

type FlowRunDescription struct {
    Run        FlowRun
    NodeStates []NodeRunState  // per-node timeline (succeeded / failed / retried)
    OutputRef  *ArtifactRef    // heavy outputs go here (D-026); never inline bytes
}
```

## Test plan

- **Unit:**
  - Each protocol handler `_test.go` — identity-rejection, scope-claim gating, projection shape.
  - `metrics.go` — bucket arithmetic over deliberate `flow.*` event emission.
- **Integration:**
  - `test/integration/flows_page_test.go` — real engine + Protocol transport + identity propagation; tests: catalog list cross-tenant rejection; `flows.run` rejection without claim; heavy output round-trips via `ArtifactRef`.
- **Conformance:**
  - All 6 methods run against the Protocol conformance suite (Phase 62).
- **Concurrency / leak:**
  - `concurrent_reuse_test.go` — N=100 concurrent reads against a shared engine registry under `-race`.
- **UI (Playwright):**
  - `flows-page.spec.ts` — catalog renders; engine graph canvas renders nodes + edges; `Run flow` button visible-and-enabled with claim, disabled-with-tooltip without; Run history click → summary panel with `Open artifact` link for heavy outputs.

## Smoke script additions

`scripts/smoke/phase-73i.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call` each of the 6 methods with valid fixture payloads.
- `flows.run` without `flows.run` scope claim → expect 403.
- Cross-tenant `flows.list` without admin → expect 403.
- Page route /console/flows — SKIPped until 73m's `harbor console` lands.

## Coverage target

- `internal/runtime/flow/protocol`: 85%.
- `internal/protocol/transports/stream`: 80%.
- `web/console/src/routes/flows/`: 70%.
- `web/console/src/lib/components/graph/`: 75% (the shared canvas component).

## Dependencies

**Same-wave (Wave 13):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72a (events filter — `flows.metrics` consumes `events.aggregate`)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 26a (Flow-as-Tool registration + per-flow Budget — `Shipped`; D-023)
- Phase 73 (state inspection — `Pending`; `state.history` supplies per-run trace)
- Phase 54 (Protocol task control surface — `Shipped`; supplies `start` for invoking flow runs)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)

## Risks / open questions

- **Engine graph canvas SHARED with 73b Live Runtime.** This phase establishes the `GraphInput` typed interface; 73b consumes it. If 73b's mockup needs a richer node-state model (live queue depth, etc.), the shared interface must extend cleanly. Coordinator confirms during the 73b dispatch.
- **`flows.runs.list` cost.** For a flow with many runs, naive list is O(N). V1 accepts the cost; pagination is the floor protection.
- **Per-run trace (per-node timeline) consumes shipped `state.history`.** If Phase 73's `state.history` shape differs from this plan's assumption, `flows.runs.describe` glue needs to adjust.
- **`Compare versions` is Console-local diff.** The diff renderer runs in the browser over two `flows.describe` snapshots pulled by the same Console-local `Save snapshot` flow. NO Protocol-side comparison method (D-061).
- **D-064 Evaluations are post-V1.** No "Convert to evaluation" affordance renders on run-history rows; verified absent in the Playwright spec.

## Glossary additions

- **`flows.list`** — Protocol method returning paginated registered flows with aggregate metrics.
- **`flows.describe`** — Protocol method returning a flow's full engine-graph description.
- **`flows.runs.list`** — Protocol method returning a flow's run history rows.
- **`flows.runs.describe`** — Protocol method returning per-node timeline + output ArtifactRef for a single run.
- **`flows.run`** — Protocol method invoking a flow run. Requires `flows.run` scope claim.
- **`flows.metrics`** — Protocol method returning sparkline aggregates derived from `flow.*` events.
- **`FlowDescription`** — Protocol projection of an engine-graph flow with nodes + edges + source reference.
- **`FlowRunDescription`** — Protocol projection of a single flow run's per-node timeline + heavy-output ArtifactRef.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes
- [ ] `svelte-check --fail-on-warnings` passes
- [ ] `npm run lint` passes in `web/console/` (no raw color / spacing literals)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-tenant test passes
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent reads against shared engine registry under `-race` (D-025)
- [ ] **Integration test passes** — `test/integration/flows_page_test.go` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/flows-page.spec.ts`
- [ ] **Shared engine graph canvas published** at `web/console/src/lib/components/graph/` — typed `GraphInput` interface stable for 73b
- [ ] Glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review
