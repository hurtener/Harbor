# Phase 74 — Console topology projection events

## Summary

Ship the canonical `topology.snapshot` Protocol method and the paired `topology.changed` event so any Protocol client (the Live Runtime topology canvas in Phase 73b first, the Playground trace toggle in Phase 73n second) can render an engine's node graph + per-edge queue depth from the Protocol alone — never touching internal Runtime types. The snapshot is on-demand (request → reply); the event fires on engine construction and on every edge-set change, with the same `TopologyProjection` shape on both wire surfaces. Identity is mandatory; cross-tenant snapshots require admin scope per D-079.

## RFC anchor

- RFC §5.2
- RFC §6.1
- RFC §6.13
- RFC §7.1

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- **brief 11 §LR-1 (Topology graph)**: "A directed graph of the run's nodes ... each node carries a name, a type tag, a status pill, a latency, an edge to downstream nodes, a selection state." The `TopologyProjection` shape lands name + type tag (`NodeKind`) + queue depth on edges; status / latency / selection are per-run overlays the consumer composes from the existing event taxonomy. **brief 11 §LR-1 (Open questions)**: "Per-run vs per-session — Recommendation: per-run primary." Phase 74's snapshot is engine-scoped (the static graph + live channel depth); per-run trajectory data stays on the event bus (Phase 05, Phase 06) and the consumer overlays it. Splitting the projection this way keeps the snapshot byte-stable across runs of the same engine.
- **brief 11 §LR-1 (Protocol surface required)**: "Topology projection events (Phase 74). Cursor-resumable subscription so reconnect catches up cleanly (Phase 6)." Phase 74 ships the event-side as `topology.changed` over the Phase 05 EventBus (so the Phase 06 replay capability subsumes it for free) AND the request-side as a `topology.snapshot` method (so a fresh consumer doesn't have to wait for the next edge change to draw the canvas).
- **brief 06 §"Visualization protocol surface"**: "Topology snapshots published over the bus as `topology.snapshot` events; Console consumes; CLI `harbor inspect-topology --run` renders ASCII." Phase 70 (`harbor inspect-topology`) already ships the CLI consumer per D-102, currently driven by trajectory synthesis from the existing event taxonomy; Phase 74 lands the canonical source. The Phase 70 renderer gains a preferred-source branch in a follow-up phase (out of Wave 13 scope) — Phase 74 is the producer, not the renderer retrofit.
- **brief 06 §"Visualization couples to private state"**: "Harbor's visualization derives from the canonical event/topology surface published over the protocol — no private fields." The `TopologyProjection` is a Phase 74-owned wire type in `internal/protocol/types/`; it is NOT a re-export of `engine.Adjacency` or `engine.Node` or the engine's private `channels` map. The runtime constructs a projection from those internals at request/emit time; the consumer never sees them.
- **brief 06 §6 (events as canonical state)**: "logs and OTel derive from the same events rather than being parallel paths." The `topology.changed` event rides the same `events.EventBus` every other canonical event uses — no separate topology channel. The wave-13-decomposition row's "or confirm it's an event-only surface" question resolves in favor of **both surfaces** because the snapshot covers fresh-consumer cold-start while the event covers in-flight updates; an event-only surface would force every consumer to wait for an edge change to see the engine.

## Findings I'm departing from (if any)

None. Brief 11 §LR-1's "automatic layout" recommendation is a consumer concern (the Live Runtime canvas in Phase 73b will pick dagre or ELK); the wire shape is layout-agnostic. The §9.4 "topology projection granularity" open question is resolved as **engine-scoped projection + per-run overlay from existing events** — see "Brief findings incorporated" above.

## Goals

- Land `TopologyProjection` as a canonical Protocol wire type in `internal/protocol/types/` — nodes, edges, kind tags, per-edge queue depth, engine identity (no run-scoped fields).
- Land `MethodTopologySnapshot Method = "topology.snapshot"` in `internal/protocol/methods/methods.go` AND extend `internal/protocol/singlesource.CanonicalMethods` in lockstep (D-077 single-source posture).
- Land `EventTypeTopologyChanged EventType = "topology.changed"` in `internal/events/events.go` via the existing `RegisterEventType` path; payload is a `SafePayload` (no secret-shaped fields) so the audit redactor bypasses it and the consumer keeps typed access (D-028).
- Land an `internal/runtime/engine.Topology()` accessor that builds a `TopologyProjection` from the engine's private adjacency list + per-edge channel depth — pure read, no mutation, race-detector clean.
- Wire `engine.New` to publish a `topology.changed` event on construction (paired with the EventBus passed via a new `WithEventBus(b)` option — nil-bus = no emit, preserving every Phase 02 engine test verbatim).
- Wire the Protocol surface: extend `ControlSurface.Dispatch` (or the Phase 73 sibling state-inspection surface — see "Files added or changed") so a `topology.snapshot` request returns a `TopologyProjection` for the engine the caller is scoped to.
- Identity-mandatory: a `topology.snapshot` call without `(tenant, user, session)` fails loud with `CodeIdentityRequired`; a cross-tenant call (caller's identity ≠ target engine's tenant) requires `auth.ScopeAdmin` per D-079 and emits `audit.admin_scope_used`.
- Concurrent-reuse contract per D-025: the engine's `Topology()` accessor is safe for N≥100 concurrent calls against a shared engine instance under `-race`.
- First consumer in same wave per §13: the Stage 2.2 Live Runtime page (Phase 73b) renders the topology canvas from `topology.snapshot` + `topology.changed` alone; the Phase 73n Playground trace toggle is the second consumer. Phase 74's own acceptance test stands in for both in this PR — a real engine + real EventBus + real Protocol transport asserting the event arrives at a subscriber and the snapshot round-trips.

## Non-goals

- A per-run topology overlay (node status, latency, selection state). The consumer composes those from existing event types (`tool.invoked`, `tool.completed`, `task.spawned`, `pause.requested`, `planner.finish`) — Phase 70 already proves this synthesis is viable (D-102).
- A WebSocket-multiplexed topology channel. The SSE + control-surface split shipped in Phase 60 carries both surfaces — `topology.changed` rides the existing subscription stream, `topology.snapshot` rides the existing control surface.
- Retrofitting the Phase 70 CLI renderer to consume the canonical event. D-102 documents the synthesis-source-of-truth posture for V1; Phase 74's event becomes a preferred-source branch in a follow-up phase (out of Wave 13 scope).
- Authoring layout (dagre / ELK / hand-laid) — the wire shape is layout-agnostic; canvas-side layout is a Phase 73b concern.
- A `topology.subscribe` method. The existing `events.subscribe` surface (Phase 72) is the subscription primitive; filtering on `Types: []{"topology.changed"}` is how a consumer subscribes.

## Acceptance criteria

- [x] `MethodTopologySnapshot Method = "topology.snapshot"` lands in `internal/protocol/methods/methods.go`; `canonicalMethods` is updated; `methods.IsValidMethod("topology.snapshot")` returns true; `singlesource.CanonicalMethods` is updated in lockstep; the `TestSingleSource_CanonicalMethodsInLockstep` test still passes.
- [x] `TopologyProjection` lands in `internal/protocol/types/topology.go` with fields `EngineID string`, `OccurredAt time.Time`, `Nodes []TopologyNode`, `Edges []TopologyEdge`; `TopologyNode` carries `Name string`, `Kind TopologyNodeKind` (with V1 constants `NodeKindInlet` / `NodeKindNode` / `NodeKindOutlet`); `TopologyEdge` carries `From string`, `To string`, `QueueDepth int`, `QueueCapacity int`. `singlesource.CanonicalWireTypes["TopologyProjection"] = "types"` is added; `TestSingleSource_CanonicalWireTypesInLockstep` still passes.
- [x] `EventTypeTopologyChanged EventType = "topology.changed"` is registered via `init() { events.RegisterEventType(EventTypeTopologyChanged) }`; the payload type `TopologyChangedPayload` implements `SafePayload` (per D-028 — projection has no secret-shaped fields).
- [x] `engine.Topology(ctx context.Context) (types.TopologyProjection, error)` is exposed on the `engine.Engine` interface; identity-mandatory (rejects empty `(tenant, user, session)` with `ErrIdentityRequired`); returns a deterministic projection (`Nodes` + `Edges` sorted lexicographically by `Name` / `(From, To)`).
- [x] `engine.New` honours a new `WithEventBus(b events.EventBus) Option`; when supplied, the constructor publishes one `topology.changed` event on the bus carrying the initial projection. A nil bus (default) preserves the existing Phase 02 engine-test surface verbatim — no new mandatory dependencies on Phase 02 callers.
- [x] A `topology.snapshot` Protocol call against a runtime that hosts a single engine returns the same `TopologyProjection` the engine emitted via `topology.changed` on construction (byte-stable across the two surfaces).
- [x] A `topology.snapshot` call without `(tenant, user, session)` on the request returns `CodeIdentityRequired`; with the triple but targeting an engine whose tenant differs from the caller's, the call returns `CodeAuthRejected` unless `auth.HasScope(ctx, ScopeAdmin)` is true. The admin path additionally publishes `audit.admin_scope_used` per RFC §6.13.
- [x] Integration test (`test/integration/phase74_topology_test.go`) — real `engine.engine` instance + real `events/drivers/inmem` bus + real Protocol transport (httptest.Server wrapping the Phase 60 mux) — asserts: (a) the constructor-time `topology.changed` event arrives on a subscriber within `200ms` of `engine.New`; (b) the snapshot RPC round-trip yields the same projection bytes; (c) cross-tenant call without admin scope rejects with `CodeAuthRejected`; (d) cross-tenant call with admin scope succeeds and emits `audit.admin_scope_used`; (e) a re-construction with one more adjacency emits a second `topology.changed` whose `Edges` differ by exactly one entry.
- [x] Concurrent-reuse test (`engine/topology_concurrent_test.go`): N=128 concurrent calls to `engine.Topology(ctx)` against ONE shared `engine.engine` under `-race`; every call returns the same projection bytes (no race-induced field tearing); goroutine baseline restored after all calls return.
- [x] `scripts/smoke/phase-74.sh` (`PREFLIGHT_REQUIRES: live-server`) runs the touched-package tests under `-race` (a real assertion covering every unit + concurrent-reuse test), then drives `topology.snapshot` against the preflight-booted dev server with a valid dev token and asserts the response carries non-empty `nodes` + `edges` — OR cleanly SKIPs on the engine-less `harbor dev` stack (404 → SKIP), where the surface returns `CodeUnknownMethod`. **Deviation:** the planned SSE-`topology.changed` smoke step is dropped — `harbor dev` hosts no engine-graph so no `topology.changed` event is emitted on the dev server, and no `/v1/dev/engine/rebuild` endpoint exists to synthesise one; the event surface is instead exercised end-to-end by `test/integration/phase74_topology_test.go` (real engine + real bus + real SSE-capable subscriber).
- [x] `docs/glossary.md` gains entries for `topology.snapshot`, `topology.changed`, `TopologyProjection`.
- [x] `docs/decisions.md` gains the pre-assigned `D-114` entry (the plan's original `D-106` collided with a parallel Wave 13 phase; see D-114) pinning: the dual-surface posture (method + event), the engine-scoped (not run-scoped) projection shape, the identity-mandatory + admin-cross-tenant gating, and the relationship to the Phase 70 CLI consumer.
- [x] `docs/plans/README.md` flips the Phase 74 row's `Status` to `Shipped` and updates the row's RFC anchor cell to include `§7.1`.

## Files added or changed

- `internal/protocol/types/topology.go` — new: `TopologyProjection` + `TopologyNode` + `TopologyNodeKind` + `TopologyEdge` + the three `NodeKind*` constants.
- `internal/protocol/types/topology_test.go` — new: marshal round-trip + sort-determinism + zero-value handling.
- `internal/protocol/methods/methods.go` — extend `canonicalMethods` with `MethodTopologySnapshot`.
- `internal/protocol/methods/methods_test.go` — extend the exhaustiveness assertion.
- `internal/protocol/singlesource/singlesource.go` — extend `CanonicalMethods` + `CanonicalWireTypes` in lockstep.
- `internal/protocol/singlesource/singlesource_test.go` — assertion updates.
- `internal/events/events.go` — register `EventTypeTopologyChanged`; declare `TopologyChangedPayload` as `SafePayload`.
- `internal/events/events_test.go` — assertion updates.
- `internal/runtime/engine/engine.go` — extend `Engine` interface with `Topology(ctx)`; extend `engineConfig` with `eventBus events.EventBus`; add `WithEventBus(b)` option; in `New`, after the inlet/outlet allocation, publish the initial `topology.changed` event when a bus is configured.
- `internal/runtime/engine/topology.go` — new: pure-function builder `buildProjection(nodes, adjs, channels) types.TopologyProjection` (deterministic sort; reads channel cap + len under the engine's existing mutex for queue-depth fields).
- `internal/runtime/engine/topology_test.go` — new: unit tests for the builder (cycle-friendly graph, multi-inlet, multi-outlet, queue-depth liveness).
- `internal/runtime/engine/topology_concurrent_test.go` — new: D-025 N≥128 stress.
- `internal/protocol/protocol.go` — extend `ControlSurface` with a `topology` field (a small interface `TopologyAccessor` the Runtime wires up — engine.Engine satisfies it); extend `Dispatch` with the `topology.snapshot` branch; extend `NewControlSurface` to accept the accessor + `auth.HasScope` for the admin gate.
- `internal/protocol/protocol_test.go` — extend `TestDispatch_*` with the topology branch + the identity-mandatory + admin-cross-tenant assertions.
- `internal/protocol/concurrent_test.go` — extend the N≥100 stress to include `topology.snapshot`.
- `internal/protocol/transports/control/control.go` — minor: register the new method in the request-routing table so the SSE-side transport surfaces the method name in error messages (no per-method handler — Dispatch handles it generically).
- `cmd/harbor/cmd_dev.go` (or the existing dev-stack wiring) — pass the configured engine + EventBus into `NewControlSurface` via the new accessor field; nil-safe when the dev stack runs without an engine (e.g. validate-only mode).
- `harbortest/devstack/devstack.go` — extend `Assemble` to wire the engine + bus into `ControlSurface` for the integration test (per D-094 + §17.6 — fix the test-side fixture AND the production call site in the same PR).
- `test/integration/phase74_topology_test.go` — new: the cross-package integration test enumerated under Acceptance.
- `scripts/smoke/phase-74.sh` — new: live-server smoke per the assertions enumerated under Acceptance.
- `docs/glossary.md` — append `topology.snapshot`, `topology.changed`, `TopologyProjection` entries.
- `docs/decisions.md` — append D-114 (the plan's original pre-assignment `D-106` collided; D-114 is the collision-free number).
- `docs/plans/phase-74-console-topology.md` — this file.
- `docs/plans/README.md` — flip Phase 74 row to Shipped.
- `README.md` — flip Phase 74 row in the Status table.

## Public API surface

```go
// internal/protocol/types/topology.go
type TopologyProjection struct {
    EngineID    string          `json:"engine_id"`
    OccurredAt  time.Time       `json:"occurred_at"`
    Nodes       []TopologyNode  `json:"nodes"`
    Edges       []TopologyEdge  `json:"edges"`
}

type TopologyNodeKind string

const (
    NodeKindInlet  TopologyNodeKind = "inlet"
    NodeKindNode   TopologyNodeKind = "node"
    NodeKindOutlet TopologyNodeKind = "outlet"
)

type TopologyNode struct {
    Name string           `json:"name"`
    Kind TopologyNodeKind `json:"kind"`
}

type TopologyEdge struct {
    From          string `json:"from"`
    To            string `json:"to"`
    QueueDepth    int    `json:"queue_depth"`
    QueueCapacity int    `json:"queue_capacity"`
}

// internal/protocol/methods/methods.go (additive)
const MethodTopologySnapshot Method = "topology.snapshot"

// internal/events/events.go (additive)
const EventTypeTopologyChanged EventType = "topology.changed"

// TopologyChangedPayload is a SafePayload (D-028): the projection has
// no secret-shaped fields, so the audit redactor bypasses it and
// subscribers keep typed access.
type TopologyChangedPayload struct {
    SafeSealed
    Projection types.TopologyProjection
}

// internal/runtime/engine/engine.go (additive on the Engine interface)
type Engine interface {
    // ... existing methods ...
    Topology(ctx context.Context) (types.TopologyProjection, error)
}

// internal/runtime/engine/options.go (additive)
func WithEventBus(b events.EventBus) Option

// internal/protocol/protocol.go (additive)
type TopologyAccessor interface {
    Topology(ctx context.Context) (types.TopologyProjection, error)
    TenantID() string  // used by the admin-cross-tenant gate
}

// NewControlSurface signature gains an accessor argument:
func NewControlSurface(
    taskRegistry tasks.TaskRegistry,
    steeringRegistry *steering.Registry,
    topology TopologyAccessor,  // new — may be nil; nil-topology dispatchers return CodeMethodNotSupported
    opts ...Option,
) (*ControlSurface, error)
```

## Test plan

- **Unit:**
  - `internal/protocol/types/topology_test.go` — JSON round-trip; deterministic sort under multiple `Nodes` / `Edges` orderings.
  - `internal/protocol/methods/methods_test.go` — exhaustiveness extended to 11 methods.
  - `internal/protocol/singlesource/singlesource_test.go` — `CanonicalMethods` + `CanonicalWireTypes` lockstep.
  - `internal/events/events_test.go` — registration assertions extended.
  - `internal/runtime/engine/topology_test.go` — every adjacency shape (single-inlet single-outlet, multi-inlet, multi-outlet, cycle-with-AllowCycle, channel-override queue caps); identity rejection paths.
- **Integration:**
  - `test/integration/phase74_topology_test.go` — boots `harbortest/devstack.Assemble`; wires a real `engine.engine` instance with `WithEventBus(busFromDevstack)`; opens an `events.subscribe` stream filtered on `topology.changed` via the Phase 60 SSE transport; asserts the constructor-time event arrives within 200ms (using a channel-bound `eventually` helper, NOT `time.Sleep` per §17.4); drives a `topology.snapshot` RPC against the Phase 60 control transport; asserts the projection bytes match the event payload's; tests cross-tenant rejection without admin scope; tests cross-tenant success with admin scope + audit emission. Per §17.6, if the integration surfaces a wiring gap in `cmd/harbor/cmd_dev.go::bootDevStack` that the `devstack.Assemble` test fixture would otherwise mask, the production wiring is fixed in the SAME PR.
- **Conformance:**
  - `internal/protocol/conformance/conformance.go` — extend the method matrix with `topology.snapshot`; add the four documented failure modes (identity-required / cross-tenant-without-admin / cross-tenant-with-admin / engine-not-configured) to the error-code matrix so the Phase 62 conformance pack covers the new method against both the in-process and over-the-wire transports.
- **Concurrency / leak:**
  - `internal/runtime/engine/topology_concurrent_test.go` — D-025 N≥128: 128 goroutines concurrently call `engine.Topology(ctx)` against ONE shared engine; assert no race detector hits, no cross-call mutation of return values, and `runtime.NumGoroutine()` returns to baseline within 100ms of all goroutines joining (via `eventually` channel + `runtime.Gosched`).
  - The integration test additionally runs N=10 concurrent subscribers + N=10 concurrent snapshot callers against the live runtime per §17.3 (concurrency stress on long-lived wiring).

## Smoke script additions

- `scripts/smoke/phase-74.sh` (new; `PREFLIGHT_REQUIRES: live-server`):
  1. Runs `go test -race -count=1 -timeout 60s ./internal/protocol/types/... ./internal/protocol/methods/... ./internal/events/... ./internal/runtime/engine/...` and asserts pass (covers all unit + concurrent-reuse tests on the touched packages).
  2. Drives `topology.snapshot` against the preflight-booted dev server with `HARBOR_DEV_TOKEN` set, identity headers carrying the dev's `(tenant, user, session)`, and asserts the response is 200 with a JSON body containing non-empty `nodes` + `edges`.
  3. Drives the same call WITHOUT identity headers and asserts the response is the structured `CodeIdentityRequired` shape (401).
  4. Drives the same call WITH identity headers carrying a foreign tenant and asserts the response is the structured `CodeAuthRejected` shape (401) unless the dev token carries `admin` scope (the dev stack's default token does NOT — so the assertion is unconditional).
  5. Opens an SSE subscription to `events.subscribe` filtered on `topology.changed`, force-triggers a synthetic engine reconstruction via a dev-only endpoint (`/v1/dev/engine/rebuild` — gated behind `HARBOR_DEV_ALLOW_MOCK=1` if and only if the dev stack exposes it; SKIP cleanly when the endpoint is 404), and asserts at least one event arrives within a 2-second window.
- The smoke script honours the 404/405/501 → SKIP convention so it stays green on Phase N+1 builds where the new dev endpoint or the topology surface is being reworked.

## Coverage target

- `internal/protocol/types/`: ≥ 95% (the package is small + pure; the topology types raise the line-count denominator marginally).
- `internal/protocol/methods/`: ≥ 95% (additive).
- `internal/events/`: ≥ 90% (existing target; additive).
- `internal/runtime/engine/`: ≥ 80% (the master-plan target; the new `Topology()` builder + the `WithEventBus` option are net-new lines that this PR must cover with the new unit + concurrent tests).
- `internal/protocol/`: ≥ 85% (extends the existing ControlSurface coverage; the new `topology.snapshot` branch + the admin-cross-tenant gate must be exercised under both happy and failure paths).

## Dependencies

- Phase 05 (event taxonomy + InMem EventBus) — Shipped. Provides `events.EventBus`, `RegisterEventType`, `SafePayload` / `Sealed` / `SafeSealed`.
- Phase 09 (Routers) — Shipped. Master-plan dep; not directly consumed by the wire shape but ensures the engine's adjacency model is stable before topology projects it.
- Phase 02 (Runtime engine skeleton) — Shipped. Provides `engine.engine`, `Adjacency`, `Node`, `Option` (the `WithEventBus` option lands as a sibling of the existing options).
- Phase 54 (Protocol task-control surface) — Shipped. Provides `ControlSurface`, `Dispatch`, the `Method` registry; Phase 74 extends them.
- Phase 60 (Protocol wire transport) — Shipped. Provides the SSE-and-control-surface mux; Phase 74's smoke + integration test hit the same transport.
- Phase 61 (Protocol auth) — Shipped. Provides `auth.HasScope`, `ScopeAdmin`, `CodeAuthRejected`; Phase 74's admin-cross-tenant gate consumes them per D-079.
- Phase 62 (Protocol conformance) — Shipped. Phase 74 extends the conformance matrix.
- Phase 72 (`events.subscribe` scope claim) — Stage 1 sibling. Phase 74 emits onto the bus Phase 72 subscribes to; their PRs may land in either order within Stage 1 — the test fixture in this PR uses `harbortest/devstack` which already wires the bus.
- Phase 73 (state inspection surface) — Stage 1 sibling. Phase 74's `topology.snapshot` lands alongside the other read-only state-inspection methods; the `ControlSurface` signature change in this PR is compatible with Phase 73's additions.

## Risks / open questions

- **Cycle between `internal/protocol/protocol` and `internal/runtime/engine`.** The `TopologyAccessor` interface lives in `internal/protocol/protocol` so the Runtime engine doesn't need to import the Protocol package directly — the engine satisfies the interface structurally. The wiring at `cmd/harbor` instantiates the accessor adapter; the engine package stays Protocol-free. The integration test asserts no import cycle is introduced (the standard `go build` check is sufficient).
- **`WithEventBus` as additive option.** Phase 02 callers that do NOT pass `WithEventBus` see ZERO behavioural change. The default-nil-bus posture is the same shape Phase 04's `BusEmitter` ↔ `EventBus` wiring took before Wave 2's audit closed that seam (PR #11); this time the seam closes IN-PHASE: the production call site at `cmd/harbor/cmd_dev.go::bootDevStack` wires the bus AND `harbortest/devstack.Assemble` wires it AND the integration test asserts the bus actually carries a `topology.changed` event from production (not from a test fixture). Per §17.6, fixing the test fixture without fixing production is the bug-shape this rule guards against.
- **Queue-depth liveness vs lock contention.** Reading `len(channel)` is atomic in Go but the builder also reads `cap(channel)` and the channel map itself — the engine's existing `mu` mutex guards the channel-map mutation pattern. The builder acquires a short read lock for the duration of the snapshot construction (≤1ms in practice for any real engine size). The D-025 stress proves this doesn't introduce contention at N=128.
- **Cross-tenant projection leak.** The projection contains node names + edge endpoints which COULD encode tenant-specific information if a deployment uses tenant IDs in node names (a violation of multi-isolation hygiene but theoretically possible). The admin-cross-tenant gate ensures the only way to read another tenant's projection is to have explicitly elevated to `ScopeAdmin`, which emits `audit.admin_scope_used`. The wire shape itself carries no `tenant_id` field — the projection is engine-scoped, not tenant-scoped, but the engine's tenant is the runtime's tenant in V1 (one engine per runtime process).
- **Phase 70 retrofit out of scope.** The Phase 70 CLI's renderer continues to consume the trajectory-synthesised topology per D-102. A follow-up phase (not in Wave 13) will add the `topology.changed`-preferred-source branch. This split is documented in D-114; the master plan stays consistent because D-102's "no retrofit-required risk" clause was forward-looking to exactly this moment.

## Glossary additions

- `topology.snapshot` — Phase 74 Protocol method.
- `topology.changed` — Phase 74 canonical event type.
- `TopologyProjection` — Phase 74 wire type.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — the integration test covers cross-tenant isolation explicitly (identity-mandatory + admin-cross-tenant gate).
- [x] **If this phase builds a reusable artifact:** YES — the engine gains a `Topology(ctx)` accessor and a constructor-time bus publish. The D-025 concurrent-reuse test at `internal/runtime/engine/topology_concurrent_test.go` (N=128 against ONE shared engine) is mandatory.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** YES — Phase 74 wires `internal/runtime/engine` to `internal/events` (the bus emit), to `internal/protocol/protocol` (the method dispatch + admin gate), and to `internal/protocol/auth` (the `HasScope` check). `test/integration/phase74_topology_test.go` exercises all three seams end-to-end with real drivers.
- [x] If new vocabulary: glossary updated — `topology.snapshot` / `topology.changed` / `TopologyProjection`.
- [x] If a brief finding was departed from: N/A.
