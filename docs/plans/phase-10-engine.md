# Phase 10 — Engine + workers + cycle detection

## Summary

Land `internal/runtime/engine`: the typed, async, queue-backed graph executor that powers every later runtime phase. Ships the `Engine` interface, the worker-loop (one goroutine per node), bounded per-adjacency channels (default 64), the always-on egress fetch dispatcher, cycle detection at construction (`AllowCycle` opt-in), and `Run / Stop / Emit / Fetch`. Phase 10 is the foundation; Phases 11 (reliability), 12 (streaming), 13 (cancel), and 14 (routers) layer on top without changing this surface.

## RFC anchor

- RFC §6.1
- RFC §3.5

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §4 — worker loop.** "One goroutine per `Node`. Each iteration: `Fetch` from incoming channels → check deadline → check cancel → invoke under reliability shell → emit to outgoing channels (or to the Outlet) → finalize bookkeeping. Cancellation propagation uses a sentinel error (`RunCancelled`) that unwinds the loop without killing the worker." Phase 10 ships the worker loop without the reliability shell (Phase 11) and without cancellation (Phase 13) — both are slot-filled later via composition.
- **brief 01 §4 — channel semantics.** Each adjacency `(A, B)` gets a bounded queue of size `QueueMaxSize` (default 64 in the predecessor). Backpressure is implicit: a slow consumer pauses upstream `Emit`. Two synthetic endpoints — `Inlet` (ingress) and `Outlet` (egress) — let external code talk to the engine via the same channel mechanic. Phase 10 ships exactly this.
- **brief 01 §5 — sharp edges to design out.** The predecessor's "two egress modes (pre-dispatcher direct fetch vs post-dispatcher per-run demux)" is bolted-on. Harbor ships **one mode**: the dispatcher is on by default, always. Phase 13's `FetchByRun` slots into the same dispatcher; `Fetch` (any-run) and `FetchByRun(runID)` are both served from the dispatcher's per-run subqueues.
- **brief 01 §5 — type-mismatch is a hard error.** The predecessor's `warnings.warn` for type mismatch is rejected. Phase 10 enforces "node returns wrong shape ⇒ `RunError(NodeException)`" (the actual `RunError` shape lands in Phase 11; Phase 10 returns a typed error placeholder that Phase 11 promotes).
- **brief 01 §5 — `Stop` releases capacity waiters cleanly.** The predecessor's "set the waiter, hope they observe trace_count=0" pattern is rejected; Phase 10 uses an explicit "engine stopped" sentinel error path. Phase 12's capacity waiters honor this contract.
- **brief 01 §5 — bus publishing failures surface to the Protocol; never silently swallowed.** Phase 10's worker loop captures `ErrChannelClosed` / `ErrEngineStopped` / `ErrCycleDetected` / `ErrNodeNotFound` and returns them rather than catching-and-logging. Phase 11 routes them to the Protocol via `RunError`.
- **brief 01 §3 — the public API.** `engine.New(adjacencies..., opts...)`, `engine.Run(registry)`, `engine.Stop()`, `engine.Emit(env, opts...)`, `engine.Fetch(opts...)`. Phase 10 ships exactly these (without `Cancel` and without `FetchByRun`'s explicit demux — those are Phase 13).
- **RFC §6.1 — cycle detector at construction.** `AllowCycle` is opt-in per-node. Phase 10's `New` calls a topological-sort-style detector before returning the engine; an unintended cycle fails loudly with `ErrCycleDetected` listing the cycle's node names.
- **RFC §6.1 — default queue maxsize 64.** Settled (resolves brief 01 Q-4). Phase 10 ships `WithQueueSize(int)` and `WithChannelOverride(NodeRef, NodeRef, int)` for engine- and per-channel-level overrides.
- **RFC §6.1 — error routing.** Errors go to the Protocol unconditionally; egress emission (`emit_errors_to_rookery`-equivalent) is the optional path. Phase 10 ships the engine option `WithErrorEmissionToEgress(bool)` (default false: errors go to Phase 04's logger + Phase 05's bus, never to egress unless opted in). The Protocol-routing path lands in Phase 04+05 wiring already shipped; Phase 10 calls `Logger.Error` + bus emit on RunError.
- **D-025 — concurrent reuse contract.** A compiled `Engine` is reusable across goroutines. `Emit` and `Fetch` are concurrent-safe; per-run state lives in the dispatcher's subqueues + the worker loops, never on the `Engine` struct itself. Phase 10 ships the N≥100 reuse test — N goroutines `Emit`ing distinct envelopes, M goroutines `Fetch`ing under `-race`, asserting no race / no leak / no cross-run bleed.

## Findings I'm departing from (if any)

- **None.** Phase 10 follows brief 01 §4-5 verbatim plus the RFC §6.1 settled decisions. The biggest deliberate divergence from the predecessor (always-on dispatcher) is itself an RFC-settled departure (§6.1 "Settled decisions" — "the egress fetch dispatcher is always-on. The dual-mode the predecessor ships exists for backward compatibility Harbor doesn't owe to anyone."). No new D-NNN required.

## Goals

- Ship the `Engine` interface and the in-memory implementation that every later runtime phase will lean on. The interface is `Emit / EmitTo / Fetch / FetchByRun / Cancel / Stop`; Phase 10 implements `Emit / EmitTo / Fetch / Stop` and stubs `Cancel` + `FetchByRun` (Phase 13 fills them).
- Pin the worker-loop shape: one goroutine per `Node`, bounded per-adjacency channels, the always-on dispatcher demuxing the egress queue into per-run subqueues.
- Cycle detection at `New` time. `AllowCycle` is per-node; an unintended cycle fails loudly listing the cycle path.
- Identity-mandatory at the `Emit` boundary: `Envelope.Identity()` must satisfy `Validate` (non-empty triple); empty rejected with `ErrIdentityRequired`.
- Goroutine-leak-free: `Stop(ctx)` cancels all worker goroutines AND the dispatcher AND the capacity waiters (Phase 12 hooks in here) AND any per-run egress drainers (Phase 13). Phase 10 wires the join points.
- D-025 reuse test: shared `*Engine` across N goroutines, no race / no leak / no cross-run bleed.

## Non-goals

- No reliability shell — `NodePolicy.TimeoutMS` / retries / validation land in Phase 11. Phase 10's worker invokes the node directly; failures surface as raw errors.
- No streaming — `StreamFrame` and `EmitChunk` land in Phase 12. Phase 10's `NodeContext.Emit` returns one envelope per node invocation; multi-emission is via `NodeContext.EmitNoWait` (which returns `ErrChannelFull` rather than blocking).
- No cancellation — `Cancel(runID)` is stubbed to return `(false, nil)` ("nothing to cancel; not yet implemented"). Phase 13 implements it.
- No per-run dispatcher demux — Phase 10's dispatcher demuxes by `RunID` into a `map[string]chan Envelope` but `FetchByRun` is stubbed to return `ErrNotImplemented`. Phase 13 wires it.
- No routers, no `MapConcurrent`, no `JoinK`, no `Subflow` — Phase 14.
- No bus-emit hooks beyond the audit/log path already shipped. Phase 10 calls `Logger.Error` + the eventbus adapter (already wired in Wave 2) on internal errors; richer event taxonomy (`runtime.node_started`, `runtime.node_completed`) is a separate phase if it's ever needed.
- No `Protocol`-side wire transport. The engine is in-process; Phase 60 exposes it.

## Acceptance criteria

- [ ] `internal/runtime/engine/engine.go` defines the `Engine` interface and the in-memory `*engine` implementation. Public functions: `New(adjacencies []Adjacency, opts ...Option) (Engine, error)`, `(e *engine).Run(ctx context.Context) error`, `(e *engine).Stop(ctx context.Context) error`, `(e *engine).Emit(ctx, env, opts...) error`, `(e *engine).EmitTo(ctx, env, target NodeRef) error`, `(e *engine).Fetch(ctx, opts...) (Envelope, error)`. `Cancel` + `FetchByRun` are stubs returning `ErrNotImplemented`.
- [ ] `Adjacency` type: `type Adjacency struct { From Node; To []Node }`. `New` validates: every `To` exists; no duplicate `Node.Name`; no cycle without `AllowCycle`; at least one node has no parent (Inlet) and at least one has no child (Outlet).
- [ ] **Cycle detection at construction.** `New` runs a topological-sort-style detector before allocating channels. An unintended cycle returns `ErrCycleDetected` wrapping the cycle path: `fmt.Errorf("%w: A → B → C → A", ErrCycleDetected)`. Per-node `AllowCycle: true` opts that node out of the detector. Test: `TestEngine_New_RejectsCycle_WithoutAllowCycle`, `TestEngine_New_AcceptsCycle_WithAllowCycle`.
- [ ] **Worker loop.** `Run(ctx)` starts one goroutine per node. Each iteration: read incoming `Envelope` → check `DeadlineAt` (returns `ErrDeadlineExceeded` if expired; Phase 11 promotes to `RunError`) → invoke `Node.Func` → write outgoing `Envelope` (or drop if `nil`). On node return, the worker loops back to read the next incoming envelope.
- [ ] **Bounded per-adjacency channels.** Default queue size 64 (`DefaultQueueSize` exported constant). `WithQueueSize(n int) Option` overrides engine-wide. `WithChannelOverride(from, to NodeRef, n int) Option` overrides per-channel. Out-of-bounds (n ≤ 0) returns `ErrInvalidQueueSize` from `New`.
- [ ] **Always-on egress dispatcher.** A single dispatcher goroutine reads from the engine's egress queue (the synthetic Outlet channel) and demuxes per `Envelope.RunID` into `map[string]chan Envelope`. `Fetch(ctx, opts...)` reads from any subqueue (any-run); Phase 13 will route `FetchByRun(runID)` to a specific subqueue.
- [ ] **Identity-mandatory at Emit.** `Emit(ctx, env)` rejects with `ErrIdentityRequired` (wrapping `identity.ErrIdentityIncomplete`) when `env.Identity()` fails `Validate` for the triple — empty `RunID` is acceptable in Phase 10's surface (Phase 13 will tighten this when it adds `FetchByRun`).
- [ ] **Stop semantics.** `Stop(ctx)` cancels the engine's internal context, joins every worker goroutine + the dispatcher + any pending capacity waiters (Phase 12 wires here) within `ctx`'s deadline. Returns `ctx.Err()` if the deadline is hit before the joins complete (operator can force-kill at that point).
- [ ] **`NodeContext` surface.** `nctx.Emit(ctx, env)` blocks if the outgoing channel is full (backpressure; Phase 12 wires capacity waiters here). `nctx.EmitNoWait(env) error` returns `ErrChannelFull` if the channel is saturated. `nctx.Fetch(ctx) Envelope` reads from any incoming channel via select. `nctx.FetchAny / FetchNoWait` mirror the predecessor's surface.
- [ ] **No package-level mutable state on `*engine`.** Per-run state lives in the dispatcher's subqueues; per-invocation state on the worker stack. Compile-time assertion: `var _ Engine = (*engine)(nil)`.
- [ ] **Coverage** on `internal/runtime/engine` ≥ 85%. New unit tests cover happy paths + every error sentinel.
- [ ] **Concurrent-reuse test (D-025):** `TestEngine_ConcurrentReuse_ReuseContract` runs N=100 goroutines emitting envelopes (each with a distinct `RunID`) AND M=10 goroutines `Fetch`ing under `-race`, on a single shared `*engine` running a 3-node graph. Asserts: no race, no goroutine leak after `Stop`, no cross-run bleed (each `Fetch` returns one of the emitted envelopes; the union of fetched envelopes equals the union of emitted envelopes).
- [ ] **Cross-tenant isolation test:** `TestEngine_CrossTenant_NoBleed` runs 4 tenants × 16 envelopes; assert no `Fetch` returns an envelope from a different tenant than the one inferred from the worker's input. (Trivially holds because the engine is identity-blind beyond Emit-side validation; the test pins it so a future regression is caught.)
- [ ] **Goroutine leak test:** `TestEngine_NoGoroutineLeak_AfterStop` for: idle engine, engine mid-run, engine with full queues at Stop.
- [ ] **Cycle detection tests:** `TestEngine_New_RejectsCycle_WithoutAllowCycle` (linear A→B→A fails), `TestEngine_New_AcceptsCycle_WithAllowCycle` (same graph with `AllowCycle: true` on B succeeds), `TestEngine_New_AcceptsLinear` (no cycle, no AllowCycle), `TestEngine_New_RejectsCycle_ListsCyclePath` (error message includes the cycle's nodes).
- [ ] **Integration test:** `test/integration/runtime_engine_test.go` per AGENTS.md §17 — wires real audit + events + state + sessions + engine drivers; runs a 3-node engine that processes envelopes carrying the full identity quadruple; asserts the bus sees lifecycle events and the engine doesn't leak goroutines after `Stop`. Identity propagation through the engine + ≥1 failure mode (cycle detection rejected at construction) under `-race`.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-10.sh` smoke script runs `go test -race ./internal/runtime/engine/...` AND the integration test (`go test -race -run TestE2E_Phase10 ./test/integration/...`).

## Files added or changed

```text
internal/runtime/engine/engine.go              # Engine iface + *engine, New, Run, Stop, Emit, Fetch
internal/runtime/engine/node.go                # Node, NodeFunc, NodeContext, NodeRef
internal/runtime/engine/adjacency.go           # Adjacency, cycle detector
internal/runtime/engine/dispatcher.go          # always-on egress dispatcher (per-run demux)
internal/runtime/engine/options.go             # WithQueueSize, WithChannelOverride, WithErrorEmissionToEgress
internal/runtime/engine/errors.go              # ErrCycleDetected, ErrIdentityRequired, ErrChannelFull, ErrEngineStopped, ErrInvalidQueueSize, ErrNotImplemented, ErrNodeNotFound
internal/runtime/engine/engine_test.go         # unit + cycle + leak tests
internal/runtime/engine/concurrent_test.go     # D-025 concurrent reuse
test/integration/runtime_engine_test.go        # cross-subsystem integration test
scripts/smoke/phase-10.sh                      # Go-package + integration test invocation
docs/plans/README.md                           # Status: 10 → Shipped (in implementation PR)
docs/glossary.md                               # adds Engine, Node, Adjacency, Cycle detector, Dispatcher (engine), QueueMaxSize
```

## Public API surface

```go
package engine

// Engine is the runtime container — the typed, async, queue-backed
// graph executor. One concrete implementation in V1 (the in-memory
// engine); future remote-engine drivers (post-V1) plug behind the
// same interface via the §4.4 seam pattern.
type Engine interface {
    Emit(ctx context.Context, env messages.Envelope, opts ...EmitOption) error
    EmitTo(ctx context.Context, env messages.Envelope, target NodeRef) error
    Fetch(ctx context.Context, opts ...FetchOption) (messages.Envelope, error)
    FetchByRun(ctx context.Context, runID string) (messages.Envelope, error) // Phase 13
    Cancel(ctx context.Context, runID string) (bool, error)                  // Phase 13
    Run(ctx context.Context) error
    Stop(ctx context.Context) error
}

// New constructs an Engine from a list of adjacencies + options.
// Cycle detection runs at construction; per-node AllowCycle opts out.
func New(adjacencies []Adjacency, opts ...Option) (Engine, error)

// Node wraps a typed async function with policy + cycle opt-in.
type Node struct {
    Name       string
    Func       NodeFunc
    Policy     NodePolicy   // Phase 11 fills this
    AllowCycle bool
}

type NodeFunc func(ctx context.Context, in messages.Envelope, nctx *NodeContext) (messages.Envelope, error)

// NodeContext is the per-invocation handle the worker passes to the
// NodeFunc. Carries the engine reference (for Emit / EmitNoWait /
// Fetch / FetchAny / EmitChunk in Phase 12 / CallSubflow in Phase 14).
type NodeContext struct {
    // unexported; constructed by the worker loop
}

func (nctx *NodeContext) Emit(ctx context.Context, env messages.Envelope) error
func (nctx *NodeContext) EmitNoWait(env messages.Envelope) error
func (nctx *NodeContext) Fetch(ctx context.Context) (messages.Envelope, error)

type Adjacency struct {
    From Node
    To   []Node
}

type Option func(*config)
type EmitOption func(*emitOptions)
type FetchOption func(*fetchOptions)

func WithQueueSize(n int) Option
func WithChannelOverride(from, to NodeRef, n int) Option
func WithErrorEmissionToEgress(enabled bool) Option

const DefaultQueueSize = 64

var (
    ErrCycleDetected      = errors.New("engine: cycle detected without AllowCycle")
    ErrIdentityRequired   = errors.New("engine: Emit requires non-empty identity triple")
    ErrChannelFull        = errors.New("engine: channel full (use Emit for blocking semantics)")
    ErrEngineStopped      = errors.New("engine: stopped")
    ErrInvalidQueueSize   = errors.New("engine: queue size must be > 0")
    ErrNodeNotFound       = errors.New("engine: node not found")
    ErrNotImplemented     = errors.New("engine: not implemented in this phase")
)
```

## Test plan

- **Unit:** `TestEngine_New_HappyPath_LinearGraph`, `TestEngine_New_RejectsCycle_WithoutAllowCycle`, `TestEngine_New_AcceptsCycle_WithAllowCycle`, `TestEngine_New_RejectsCycle_ListsCyclePath`, `TestEngine_New_RejectsDuplicateNodeName`, `TestEngine_New_RejectsInvalidQueueSize`, `TestEngine_Emit_RejectsEmptyIdentity`, `TestEngine_Emit_BlocksOnFullChannel`, `TestEngine_EmitNoWait_ReturnsErrChannelFull`, `TestEngine_Fetch_ReturnsAnyRun`, `TestEngine_FetchByRun_ReturnsErrNotImplemented`, `TestEngine_Cancel_ReturnsErrNotImplemented`, `TestEngine_Stop_JoinsWorkers`, `TestEngine_Stop_RespectsCtxDeadline`, `TestEngine_DeadlineExceeded_ReturnsTypedError`.
- **Integration:** `test/integration/runtime_engine_test.go` per AGENTS.md §17 — real audit + events + state + sessions + engine; runs a 3-node engine processing envelopes; asserts bus sees lifecycle events; identity propagation; cycle detection failure mode; under `-race`.
- **Conformance:** N/A — single concrete impl. Future remote-engine drivers will share a conformance suite at the time they land.
- **Concurrency / leak:** `TestEngine_ConcurrentReuse_ReuseContract` (N=100 emitters + 10 fetchers on shared `*engine` under `-race`, D-025), `TestEngine_NoGoroutineLeak_AfterStop` (idle, mid-run, full-queues-at-Stop), `TestEngine_CrossTenant_NoBleed`.

## Smoke script additions

- `phase-10.sh`: runs `go test -race ./internal/runtime/engine/...` AND `go test -race -run TestE2E_Phase10 ./test/integration/...`. Phase 10 has no HTTP/Protocol surface yet (Phase 60).

## Coverage target

- `internal/runtime/engine`: 85%

## Dependencies

- 09 (envelopes — the engine's wire shape)

## Risks / open questions

- **Phase 10 stubs `Cancel` + `FetchByRun`.** Phase 13 fills them. The risk: a downstream phase (e.g. Phase 11's reliability shell) might want to call `Cancel` from within a node. Mitigation: Phase 11 doesn't need cancellation (timeout / retry are within-invocation); Phase 13 lands before Phase 12 in the dependency graph reads from `Cancel`. Documented.
- **`Stop` semantics under load.** With many in-flight invocations, `Stop` might exceed its deadline. Phase 10 returns `ctx.Err()` in that case; the operator can force-kill the process. Documented as expected behavior.
- **Cycle detector cost.** O(V+E) topological sort runs once at `New`. For graphs with thousands of nodes the cost is bounded; for production V1 graphs (low double digits typical) it's negligible. Documented.
- **Bus-emit on internal errors.** Phase 10 calls `Logger.Error` (Phase 04) + the eventbus adapter (Phase 04→05 wiring) when an internal error escapes the worker loop. The wiring is already shipped in Wave 2; this phase just consumes it. The integration test asserts the bus sees the error event.

## Glossary additions

- **Engine.** Harbor's runtime container — the typed, async, queue-backed graph executor. One in-memory implementation in V1 (`internal/runtime/engine`). Owns the worker loop, channel semantics, cycle detection, and the always-on egress dispatcher. Distinct from `events.EventBus` (the cross-subsystem event bus); the engine is the runtime kernel.
- **Node.** A typed async function inside the engine. Wraps a `NodeFunc` plus `NodePolicy` (Phase 11) and a per-node `AllowCycle` opt-in. One worker goroutine per node.
- **Adjacency.** A `(From Node, To []Node)` pair the engine's `New` consumes to allocate channels. The full set of adjacencies forms the DAG (with cycle opt-in per-node).
- **Cycle detector (engine).** A topological-sort-style check the engine runs at `New` time. Rejects unintended cycles with `ErrCycleDetected`; per-node `AllowCycle: true` opts out.
- **Dispatcher (engine).** The single always-on goroutine the engine runs to demux egress envelopes by `RunID`. Phase 10 ships the dispatcher; Phase 13's `FetchByRun(runID)` reads from a per-run subqueue managed by the dispatcher.
- **`DefaultQueueSize`.** `64`. The default bounded per-adjacency channel capacity. Settled per RFC §6.1 (resolves brief 01 Q-4). Engine-wide override via `WithQueueSize(n)`; per-channel via `WithChannelOverride(from, to, n)`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestEngine_CrossTenant_NoBleed`)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — `TestEngine_ConcurrentReuse_ReuseContract`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** See AGENTS.md §17. — `test/integration/runtime_engine_test.go`.
- [ ] If new vocabulary: glossary updated (Engine, Node, Adjacency, Cycle detector, Dispatcher, DefaultQueueSize)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 10; section says "None")
