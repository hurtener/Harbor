# Console page — Flows

**Slug:** `flows` &middot; **Sidebar cluster:** Resources &middot; **Route:** `/console/flows`
**Mockup:** `docs/rfc/assets/console-flows-page.png` (canonical, 2026-05-18)

## 1. Purpose

Flows is the read-only viewer for the runtime's engine graphs — the DAG structures graph-family planners (Graph / Workflow / Deterministic) run on (D-063). A "Flow" in the Console is exactly an `internal/runtime/engine/` node graph filtered to agents whose planner is graph-shaped; the page is the lens over it. The operator opens it to: inspect a flow's graph structure visually, see the flow's run history (every time it was executed, with timing + outcome), kick off a one-shot invocation of the flow via the standard `start` Protocol method, and drill into a specific historical run's per-node trace. V1 is intentionally scoped to read / run / inspect-history (D-063); authoring / versioning / import-export is post-V1 and may need a real subsystem behind it.

## 2. Where it sits in the IA

Flows is the first entry under the **Resources** cluster (Resources → Flows, Memory, MCP Connections, Artifacts). The operator reaches it from the sidebar, from an Agent detail's "Planner" tab (when planner = Graph/Workflow/Deterministic, the configured flow is linked), from a Live Runtime topology graph node that's part of a flow ("View as flow"), or from the global search palette. Breadcrumb: `<runtime> / Flows` (list) and `<runtime> / Flows / <flow-name>` (detail).

## 3. Functionality matrix

- **Flow catalog list — registered flows in the runtime, with name, planner family, node count, last-run timestamp, runs in window, error rate.** `[wave-13-extends]` Requires `flows.list` Protocol method (NEW) returning a projection of the runtime's engine-graph descriptors filtered to graph-family planners. Today the closest shipped surface is the Flow-as-Tool registration (Phase 26a, D-023) — a flow can register as a `Tool`; but the Console-facing list view is not yet a Protocol method.
- **Per-row metadata — name, planner family (Graph / Workflow / Deterministic), node count, edge count, last-run, runs-in-window, p50/p95 latency.** `[wave-13-extends]` `flows.list` payload.
- **Free-text search.** `[shipped]` Console-side index per Brief 11 §CC-4 (flows are slow-moving catalog data).
- **Per-flow detail — DAG visualisation (nodes + edges).** `[wave-13-extends]` `flows.get` Protocol method (NEW) returning the engine-graph structure (nodes / edges / per-node descriptor + per-node policy from `NodePolicy`).
- **Per-flow source-of-truth tab — the underlying Go code reference (e.g. `internal/runtime/flow/X.go`) or YAML descriptor (D-023: Go-coded V1; declarative YAML in V1.1).** `[wave-13-extends]` `flows.get` extended fields; source is a string reference, never executable code in the Console.
- **Per-flow run history — chronological list of every invocation of the flow with status, started, duration, identity, error class.** `[wave-13-extends]` Filtered `tasks.list` (NEW) by flow name OR direct projection via `flows.history` Protocol method (NEW); Wave 13 to decide.
- **Per-run detail (drill from history) — per-node execution trace, status, latency, input/output (post-redaction).** `[wave-13-extends]` `state.history` Protocol method (Phase 73 acceptance) keyed to the run id.
- **"Run this flow" action — kick off a one-shot invocation with hand-crafted args.** `[shipped]` Invoke `start` Protocol method (`types.StartRequest`) targeting the flow (the flow's `agent_id` is the target; the flow is the planner's program); identity is mandatory; emits canonical events. Brief 11 §"Flows view": "Run this flow" → playground-like invocation.
- **Per-flow metrics rollup — total runs, success rate, p50/p95 latency, total cost in window.** `[shipped]` Client-side aggregation over `task.completed` / `task.failed` / `llm.cost.recorded` events filtered to the flow.
- **Per-flow `flow.budget_exceeded` indicator (Phase 26a per-flow budget).** `[shipped]` Subscribe to `flow.budget_exceeded` events (`flow.BudgetExceededPayload`).
- **Flow authoring / editor.** `[deferred]` D-063 — V1 is read / run / inspect-history only. Authoring / versioning / import-export is post-V1 (and may need a real subsystem).
- **Flow versioning.** `[deferred]` D-063 — post-V1.
- **Flow import / export.** `[deferred]` D-063 — post-V1.
- **Diff between two flow versions.** `[deferred]` D-063 — post-V1.
- **"Convert this flow to an evaluation suite."** `[deferred]` D-064 — Evaluations is post-V1.
- **Saved filter chips.** `[shipped]` Console-local per D-061.
- **No Priority field rendered.** `[deferred]` D-065 invariant preserved.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box.
  - Row 2 — flows table (virtualised).
- **Main canvas** (per-page, detail mode):
  - Row 1 — flow detail header (name + planner family badge + node count + run-history button + "Run this flow" button).
  - Row 2 — DAG visualisation (full-bleed; pan/zoom).
  - Row 3 — tab strip: Source-of-truth | Run history | Metrics.
  - Row 4 — selected tab content.
- **Right rail** (per-page, detail): metrics rollup card + per-node selection card (when a node is selected, render its descriptor + policy).
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Flows table | `flows.list` (NEW) | Click row → detail | `[wave-13-extends]` |
| Filter bar / search | local UI state + Console-side index | Apply / Submit | `[shipped]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Flow detail header | `flows.get` (NEW) | "Run this flow" → `start` Protocol method; copy name | `[wave-13-extends]` |
| DAG visualisation | `flows.get` (NEW) returning nodes + edges | Click node → right-rail descriptor; pan/zoom (local UI state) | `[wave-13-extends]` |
| Source-of-truth tab | `flows.get` returns source reference (Go path or YAML string) | Copy (local) | `[wave-13-extends]` |
| Run history tab | `flows.history` (NEW) OR filtered `tasks.list` | Click row → per-run detail (`state.history`) | `[wave-13-extends]` |
| Metrics rollup (right rail) | event aggregation client-side | none | `[shipped]` |
| Per-node selection card (right rail) | `flows.get` per-node fields (descriptor + `NodePolicy`) | Click descriptor → Tools page (when node = tool call) | `[wave-13-extends]` |
| "Run this flow" composer | `start` Protocol method | Submit `types.StartRequest` | `[shipped]` |
| `flow.budget_exceeded` indicator | `flow.budget_exceeded` events | Click → expand to show budget breach | `[shipped]` |

## 6. Controls + actions

- **Toolbar:** filter bar + saved-filter chips + search box.
- **Row-action (list):** click → detail; copy name.
- **Canvas-action (DAG):** pan / zoom (mouse + Cmd-scroll); click node → right-rail descriptor.
- **Header-action (detail):** "Run this flow" composer; copy name.
- **Run-history row-action:** click → per-run detail.
- **Keyboard shortcuts:** `g f` Flows; `j` / `k` next / previous; `Enter` open detail; `Esc` back; `+` / `-` zoom DAG canvas.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty catalog | No flows registered | Empty-state: "No flows registered — flows are defined in agents whose planner is Graph/Workflow/Deterministic" + docs link | Visit docs |
| Filtered empty | Filters yield zero | "No flows match these filters" + Clear | Clear |
| Initial loading | `flows.list` in flight | Skeleton rows | Auto |
| Protocol error — `CodeNotFound` on detail | Flow name unknown | "Flow not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on "Run this flow" | Operator submitted without scope | Inline error on the composer | Request elevated scope |
| Protocol error — `CodePayloadInvalid` on "Run this flow" | Args failed validation | Inline error | Adjust |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |

## 8. Multi-tenant / multi-runtime nuances

The flows catalog is per-runtime; the runtime switcher swaps the catalog. Flow definitions are tenant-agnostic (they're descriptors registered at agent-definition time), but invocation history is tenant-scoped — a non-admin operator sees only their own tenant's runs; `admin` fans the history out across tenants (with `audit.admin_scope_used` emitted on the server). Multi-runtime mode renders one runtime's flows at a time in V1; cross-runtime aggregator is post-V1 per D-091.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / inspect flows; see invocation history scoped to one's tenant; "Run this flow" within one's identity.
- `admin` — fan-in invocation history across tenants.
- `console:fleet` — post-V1 cross-runtime aggregator.
- Control-plane verbs are minimal on this page: "Run this flow" is a `start` — same scope as starting any task. No dedicated approve / reject / pause buttons (those live on the per-run detail in Live Runtime / Tasks).

## 10. Out of V1 (deferred)

- **Flow authoring / editor.** D-063 — V1 is read / run / inspect-history only.
- **Flow versioning + diff.** D-063 — post-V1.
- **Flow import / export.** D-063 — post-V1.
- **"Convert this flow to an evaluation suite."** D-064 — Evaluations is post-V1.
- **Declarative YAML flow descriptor format.** D-023 — V1.1 ("Go-coded `flow.Definition` ships V1; declarative recipe (YAML) format ships V1.1").
- **Cross-runtime flows aggregator.** D-091 — post-V1.
- **Priority field on flow cards or run-history rows.** D-065 invariant preserved.

## 11. References

- Brief 11 §"Flows view".
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §6.1 (Core runtime — engine), §6.2 (Planner interface), §7 (Console), §12 (post-V1 future work — flows authoring).
- Decisions: D-023 (Flow-as-Tool: Go-coded V1; YAML V1.1), D-061 (Console DB local-only), D-063 (Flows page = view over engine graphs; authoring post-V1), D-064 (Evaluations post-V1), D-065 (no session priority — invariant), D-066 (control claim).
- Phase plan: phase 26a (Flow-as-Tool registration + per-flow Budget — `Shipped`), phase 73 (state inspection — `Pending`), phase 100 (Recipe loader — post-V1).
- Glossary terms used: `Flows (Console page)`, `Console`, `Runtime lens`, `Scope claim`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-flows-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Flows catalog table (top of page).** Columns in mockup order: **Flow name** / **Owner** / **Version** / **Runs (24h)** / **p50 / p95 latency** / **Success rate** / **Last run** (relative timestamp) / **Per-flow Budget** (token cap + cost cap per D-023's per-flow `Budget`) / row-action menu. Right-side action: `Run flow` button per row (opens the inline runner — gated by `flows.run` scope claim per D-066). Top-right also surfaces a `Refresh`, `Run history`, and `Compare versions` action set.
- **Flow Metrics card (top-right beside catalog table).** Aggregate sparklines for the selected flow: runs-per-hour, p95 latency over time, success-rate over time, per-flow budget consumption vs. cap. Read-only; data sourced from `flow.*` events aggregated client-side over the active window.
- **Selected flow detail header (mid-page, full width).** Header row: flow name + version chip + `Run this flow ▶` button + `Save snapshot` (Console-local) + `Compare versions` (Console-local diff of two `flow.describe` snapshots). Status pill: `Healthy` / `Degraded` / `Errored` driven by the success-rate sparkline.
- **Engine graph canvas (mid-page, primary surface for selected flow).** Read-only DAG render of the flow's engine graph: nodes (subflows / tools / pause points / artifact emitters) connected by edges. Layout uses the same renderer family as the Live Runtime topology (Brief 11 §LR shared component family). Node interactions: click for inline detail popover (input/output schemas, tool transport when applicable, ToolPolicy when applicable), double-click for `/console/tools/<id>` (when node is a tool). **No edit affordances** — Flows is view-only at V1 per D-063 (`Add node`, `Delete edge`, `Save graph` controls are absent, not just disabled).
- **Budget meter (right rail beside graph).** Per-flow `Budget` (D-023): token cap progress bar, cost cap progress bar, request cap counter; refreshes each run. Edit is post-V1 per §10.
- **Run history table (bottom-left, full width below graph).** Per-run rows: **Run ID** (short hash + copyable) / **Started** / **Duration** / **Status** chip / **Trigger** (user / planner / system) / **Cost** / row-action menu. Click a row to surface that run's trace in the bottom-right panel; deep-link button navigates to `/console/sessions/<id>?run=<run_id>` for full-fidelity session view.
- **Selected run summary panel (bottom-right beside run history).** When a run row is selected: run-id header + status pill + per-node status timeline (which nodes succeeded, failed, retried) + final output preview (heavy content via `artifacts.get` per D-026) + `Open session` deep-link.
- **Footer.** `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Flows catalog table (sortable + per-flow row metrics) | `flows.list` response with aggregate fields | Sort / click row | `[wave-13-extends]` (`flows.list` with aggregated metrics) |
| `Run flow` button per row + detail-header `Run this flow ▶` | Selected flow id + input form | Open input modal → invoke `flows.run` | `[wave-13-extends]` (`flows.run` Protocol method) |
| `Save snapshot` (Console-local pin of a flow version) | `flows.describe` snapshot | Pin into saved-views list | `[Console-local]` (D-061) |
| `Compare versions` (Console-local diff of two `flows.describe` snapshots) | Two `flows.describe` snapshots | Open diff viewer | `[Console-local]` (D-061; pure client-side diff) |
| Flow Metrics card (sparklines for runs-per-hour, p95 latency, success-rate, budget consumption) | Aggregated from `flow.*` event stream | None (read-only) | `[wave-13-extends]` (`flows.metrics` aggregate method TBD; events themselves on the bus) |
| Engine graph canvas (read-only DAG render) | `flows.describe` graph payload | Click node for popover; double-click tool node → tools page | `[wave-13-extends]` (`flows.describe` Protocol method) |
| Budget meter (right rail; per-flow `Budget` from D-023) | `flows.describe` Budget fields | None at V1 (edit deferred per §10) | `[wave-13-extends]` (`flows.describe` returns Budget; edit is `flows.set_budget` — post-V1) |
| Run history table (per-run rows with status / cost / trigger) | `flows.runs.list?flow_id=…` | Click row → load summary panel | `[wave-13-extends]` (`flows.runs.list` Protocol method) |
| Selected run summary panel (per-node timeline + output preview) | `flows.runs.describe?run_id=…` | `Open session` deep-link | `[wave-13-extends]` (`flows.runs.describe` Protocol method) |
| Heavy run-output preview (via `artifacts.get`) | Output that exceeds heavy-content threshold (D-026) | Open artifact viewer | `[wave-13-extends]` (`artifacts.get` — already shipped surface) |

### No mockup violations of binding carve-outs

- **D-023 (Flow-as-Tool: Go-coded V1; YAML V1.1; per-flow `Budget`).** The Budget meter reflects the V1 Go-coded Budget surface; no YAML authoring UI in V1 (deferred per §10). Per-flow Budget displays only — edit is `flows.set_budget` which lands post-V1.
- **D-061 (Console DB local-only).** Save snapshot, Compare versions, sort preferences, saved-view chips, and the run-summary annotations are Console-local. The mockup never persists a Protocol-mutating shadow of flow definitions or run history.
- **D-063 (Flows page = view over engine graphs; authoring post-V1).** The engine graph canvas is read-only — `Add node`, `Delete edge`, `Save graph`, `New flow` actions are absent (not disabled-with-tooltip; the affordances themselves don't render at V1). `Run flow` is allowed (D-063 explicitly permits running existing flows). Compare versions is a diff of read-only snapshots, not an edit affordance.
- **D-064 (Evaluations post-V1).** No "Convert to evaluation" / "Run benchmark" actions appear on the run history rows.
- **D-065 (no session-level priority).** No priority field on flows, runs, or budgets.
- **D-066 (control-scope claims).** `Run flow` requires the `flows.run` scope claim and degrades to disabled-with-tooltip; observation (catalog, graph, run history, metrics) requires only the read scope.
- **D-091 (`harbor console` deployment).** Footer carries Protocol + Console versions and the connected-runtime label.
- **§13 forbidden practices.** No authoring UI dressed up as "view-only edit" — the graph canvas is pure render. Heavy run outputs route through `artifacts.get` (closes D-026). No parallel implementation of run execution — `flows.run` is the only path.
