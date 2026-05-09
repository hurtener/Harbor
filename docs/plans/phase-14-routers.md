# Phase 14 — Routers + concurrency utils + subflows

## Summary

Land the runtime's routing + concurrency surface — `PredicateRouter`, `UnionRouter`, `RoutePolicy`, `MapConcurrent`, `JoinK` — plus the `Subflow(factory, parent, opts...)` primitive. A subflow is a freshly-built child engine that runs to completion for one parent envelope, mirrors parent cancellation, and returns the first egress payload. Phase 14 closes the runtime kernel chain (Phases 09-14) by giving the planner the full RFC §6.1 surface to express fan-out / join-K / conditional routing / nested flows.

## RFC anchor

- RFC §6.1
- RFC §6.2
- RFC §3.2

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §2 — routers.** "`Routers: PredicateRouter, UnionRouter, RoutePolicy`." Phase 14 ships these three. `PredicateRouter`: routes based on a per-envelope predicate (`func(env) bool` per branch). `UnionRouter`: routes based on a payload type-tag (`Tag(payload) string`). `RoutePolicy`: explicit-target override that bypasses both routers (the planner-driven path).
- **brief 01 §2 — concurrency utilities.** "`MapConcurrent`, `JoinK`." `MapConcurrent` runs a node-fn over N envelopes in parallel with a max-concurrency bound; preserves order on output. `JoinK` waits for K out of N branches to complete and emits the joined result.
- **brief 01 §2 / §4 — Subflow.** "`Subflow(factory, parent, opts...)` runs a child engine with the parent's `RunID`, mirrors parent cancellation, returns the first egress payload." Phase 14 ships this with the watcher-goroutine pattern from brief 01 §4 ("Cancellation propagation. Subflow runs mirror parent cancellation via a watcher goroutine").
- **brief 01 §3 — Subflow factory shape.** "A subflow is a freshly-built engine that runs to completion for one parent envelope, then `Stop`s. Cancellation is mirrored from the parent." Phase 14's `factory func() (Engine, error)` returns a new engine per call; the caller never reuses subflow engines (cheap construction is the contract). Engines themselves are immutable post-`New` per D-025; what's "fresh" is the per-invocation lifecycle.
- **brief 01 §5 — sharp edge: subflow registries are recreated each call.** The predecessor recreates the validator adapters on every invocation. Harbor caches validator function pointers at the parent engine's registration time (Phase 11 already takes `ValidateFunc` as part of `NodePolicy`); the subflow's `factory` closure captures these pointers cheaply. Documented; no new pattern needed.
- **brief 01 §3 — `NodeContext.CallSubflow` surface.** "`CallSubflow` mirrors parent cancellation into the child, returns the first egress payload." Phase 14's `(nctx *NodeContext) CallSubflow(ctx, factory) (Envelope, error)` is the API.
- **brief 01 §6 — integration tests.** "`MapConcurrent` honors max-concurrency bound. Predicate router selects targets; explicit policy overrides predicate. Subflow: parent emits, subflow processes, parent receives." Phase 14 ships all three named tests.
- **D-025 — concurrent reuse.** Routers + concurrency utils are stateless (reusable across goroutines by construction). `Subflow` constructs a fresh engine per call; the parent engine's `*engine` is reusable. The N≥100 reuse test extends to cover routers + a subflow path; asserts no race / no leak.

## Findings I'm departing from (if any)

- **None.** Phase 14 follows brief 01 verbatim. The biggest design choice (subflow factory returns a fresh engine per call rather than a shared one) is brief 01's recommendation; documented.

## Goals

- Ship the three routers (`PredicateRouter`, `UnionRouter`, `RoutePolicy`) so planners can express conditional routing without poking engine internals.
- Ship `MapConcurrent` + `JoinK` as the two canonical concurrency primitives. Both honor per-run capacity backpressure (Phase 12) and per-run cancellation (Phase 13).
- Ship `Subflow(factory, parent, opts...)` with the watcher-goroutine pattern: parent `Cancel(runID)` propagates to the child engine within bounded time.
- Pin the subflow cancellation contract: cancelling the parent run cancels every in-flight subflow. The integration test exercises this with a 2-level subflow chain.
- Maintain D-025: routers + concurrency utils are stateless; subflows construct fresh engines per call.

## Non-goals

- No `RoutePolicy` persistence — V1 RoutePolicies are functions on the Engine struct, not stored. Operators who want long-lived routing policies use config-loaded predicate factories.
- No router-level retries — retries live in `NodePolicy` (Phase 11). The router emits to the selected target; if the target fails, the reliability shell handles it.
- No infinite-recursion guard for subflows. An operator who writes a subflow that calls itself forever will exhaust goroutines; the per-run capacity waiter (Phase 12) backpressures eventually but doesn't kill the recursion. Documented as a known sharp edge; future work could add a max-subflow-depth knob.
- No multi-result subflow — `Subflow` returns the first egress payload only. Multi-result subflows compose via `MapConcurrent` over a list of factories.
- No `Subflow` over a shared sub-engine. Each `CallSubflow` constructs a fresh engine via `factory()`; sharing engines across subflow invocations is the predecessor's anti-pattern (cancel mirroring breaks).

## Acceptance criteria

- [ ] `internal/runtime/routers/predicate.go`: `PredicateRouter` accepts an ordered list of `(predicate func(messages.Envelope) bool, target NodeRef)` pairs plus a default target. The router-as-Node fits into Phase 10's adjacency model: it has one input, multiple outputs, and emits to the first matching predicate's target. Test `TestPredicateRouter_FirstMatch`, `TestPredicateRouter_NoMatch_RoutesToDefault`, `TestPredicateRouter_NoMatch_NoDefault_ReturnsRunErrorRouteNotFound`.
- [ ] `internal/runtime/routers/union.go`: `UnionRouter` accepts a `Tag(payload any) string` function and a `map[string]NodeRef`. Routes envelopes by their payload's tag. Tests assert tag-based dispatch + tag-not-found path.
- [ ] `internal/runtime/routers/policy.go`: `RoutePolicy{ExplicitTarget *NodeRef}` overrides predicate / union routing when set on an envelope's `Meta["route_policy"]`. Test `TestRoutePolicy_Overrides_Predicate`.
- [ ] `internal/runtime/concurrency/map.go`: `MapConcurrent(ctx, in []messages.Envelope, fn func(ctx, env) (messages.Envelope, error), maxConcurrency int) ([]messages.Envelope, error)` runs `fn` over `in` in parallel; honors `maxConcurrency` bound; preserves output order. Test `TestMapConcurrent_HonorsBound` (assert max in-flight goroutines equals bound), `TestMapConcurrent_PreservesOrder`, `TestMapConcurrent_PartialFailure_ReturnsErr`.
- [ ] `internal/runtime/concurrency/join.go`: `JoinK(ctx, in <-chan messages.Envelope, k int) ([]messages.Envelope, error)` reads K envelopes from `in` and returns them; cancels remaining when K reached. Test `TestJoinK_ReturnsKEnvelopes`, `TestJoinK_CancelsRemainingAfterK`, `TestJoinK_CtxCancelled_ReturnsCtxErr`.
- [ ] `internal/runtime/engine/subflow.go`: `(nctx *NodeContext) CallSubflow(ctx, factory func() (Engine, error)) (messages.Envelope, error)`. Constructs the child engine, copies the parent's `RunID` onto the child engine's runtime context, starts a watcher goroutine that propagates parent `Cancel` to child, runs the child, returns the first egress payload, then `Stop`s the child.
- [ ] **Subflow cancellation mirroring.** Test `TestCallSubflow_ParentCancel_PropagatesToChild` constructs a 2-level subflow (parent → subflow A → subflow B); cancels the parent's run; asserts both A and B cancel within 100ms; both child engines' `Stop`s complete.
- [ ] **Subflow returns first egress.** Test `TestCallSubflow_ReturnsFirstEgress` constructs a child engine that emits two egress envelopes; `CallSubflow` returns only the first, then stops the child. Subsequent emissions on the child are dropped (the engine is stopped).
- [ ] **Subflow propagates identity.** Test `TestCallSubflow_PropagatesQuadruple` asserts the child engine sees the parent's full identity quadruple in every envelope.
- [ ] **Predicate router + concurrency integration.** Test `TestE2E_Phase14_PredicateRouter_FanOut` constructs a 5-node graph (input → predicate router → 3 branches → JoinK → output) and asserts routing + concurrent execution + join.
- [ ] **D-025 reuse.** `TestEngine_ConcurrentReuse_WithRouters_AndSubflow` runs N=100 concurrent runs through a graph that includes a `PredicateRouter` + `MapConcurrent` + `Subflow`. Under `-race`, asserts no race / no leak / no cross-run state.
- [ ] **Integration test:** `test/integration/runtime_routers_test.go` per AGENTS.md §17 — wires real audit + events + state + sessions + engine; runs a 2-level subflow under streaming load; cancels the parent; asserts all subflows cancel; bus emits the right `runtime.run_cancelled` events. Identity propagation through subflow boundary; failure mode (subflow construction error) under `-race`.
- [ ] No package-level mutable state in routers / concurrency utils (stateless functions). Subflow's per-call state lives on the `NodeContext` invocation stack.
- [ ] Coverage on `internal/runtime/routers` ≥ 85%, `internal/runtime/concurrency` ≥ 85%, `internal/runtime/engine` (subflow path) ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-14.sh` smoke runs Phase 13's smoke + Phase 14-specific tests + integration test.

## Files added or changed

```text
internal/runtime/routers/predicate.go               # PredicateRouter
internal/runtime/routers/union.go                   # UnionRouter
internal/runtime/routers/policy.go                  # RoutePolicy
internal/runtime/routers/predicate_test.go
internal/runtime/routers/union_test.go
internal/runtime/concurrency/map.go                 # MapConcurrent
internal/runtime/concurrency/join.go                # JoinK
internal/runtime/concurrency/map_test.go
internal/runtime/concurrency/join_test.go
internal/runtime/engine/subflow.go                  # CallSubflow + watcher goroutine
internal/runtime/engine/subflow_test.go             # mirroring + first-egress + identity
test/integration/runtime_routers_test.go            # 2-level subflow + cancel + bus
scripts/smoke/phase-14.sh                           # Go-package + integration test invocation
docs/plans/README.md                                # Status: 14 → Shipped (in implementation PR)
docs/glossary.md                                    # adds PredicateRouter, UnionRouter, RoutePolicy, MapConcurrent, JoinK, Subflow
```

## Public API surface

```go
package routers

// PredicateRouter selects the first branch whose predicate returns
// true. Branch order matters; the default target catches "no match"
// (or returns a RunError when nil).
type PredicateRouter struct {
    Branches []PredicateBranch
    Default  *engine.NodeRef
}

type PredicateBranch struct {
    Predicate func(messages.Envelope) bool
    Target    engine.NodeRef
}

// AsNode wraps the router as an engine.Node so it slots into Phase 10's
// adjacency model.
func (r *PredicateRouter) AsNode(name string) engine.Node

// UnionRouter dispatches based on payload tag. The Tag function reads
// a string from the payload (e.g. struct discriminator); the result
// keys into Branches.
type UnionRouter struct {
    Tag      func(any) string
    Branches map[string]engine.NodeRef
    Default  *engine.NodeRef
}

func (r *UnionRouter) AsNode(name string) engine.Node

// RoutePolicy overrides predicate / union routing when set on an
// Envelope's Meta["route_policy"]. Useful for planner-driven branch
// selection where the planner already knows the target.
type RoutePolicy struct {
    ExplicitTarget *engine.NodeRef
}

package concurrency

// MapConcurrent runs fn over each envelope in `in` with at most
// maxConcurrency goroutines in flight. Output preserves input order.
// Returns the first error encountered; remaining work is cancelled.
func MapConcurrent(
    ctx context.Context,
    in []messages.Envelope,
    fn func(context.Context, messages.Envelope) (messages.Envelope, error),
    maxConcurrency int,
) ([]messages.Envelope, error)

// JoinK reads exactly K envelopes from `in`. After K, cancels the
// upstream producer (via ctx) and returns the K envelopes. If `in`
// closes before K, returns ErrJoinKShortRead with however many
// arrived.
func JoinK(ctx context.Context, in <-chan messages.Envelope, k int) ([]messages.Envelope, error)

var ErrJoinKShortRead = errors.New("concurrency: JoinK source closed before K envelopes arrived")

package engine

// CallSubflow constructs a child engine via factory, runs it under the
// parent's RunID, mirrors parent cancellation, returns the first
// egress envelope, then Stops the child. Multi-result subflows
// compose via MapConcurrent over a list of factories.
func (nctx *NodeContext) CallSubflow(
    ctx context.Context,
    factory func() (Engine, error),
) (messages.Envelope, error)
```

## Test plan

- **Unit:**
  - `TestPredicateRouter_FirstMatch`, `TestPredicateRouter_NoMatch_RoutesToDefault`, `TestPredicateRouter_NoMatch_NoDefault_ReturnsRunError`,
  - `TestUnionRouter_TagDispatch`, `TestUnionRouter_TagNotFound_RoutesToDefault`,
  - `TestRoutePolicy_Overrides_Predicate`, `TestRoutePolicy_Overrides_Union`,
  - `TestMapConcurrent_HonorsBound`, `TestMapConcurrent_PreservesOrder`, `TestMapConcurrent_PartialFailure_ReturnsErr`,
  - `TestJoinK_ReturnsKEnvelopes`, `TestJoinK_CancelsRemainingAfterK`, `TestJoinK_CtxCancelled_ReturnsCtxErr`, `TestJoinK_ShortRead`,
  - `TestCallSubflow_ReturnsFirstEgress`, `TestCallSubflow_ParentCancel_PropagatesToChild`, `TestCallSubflow_PropagatesQuadruple`, `TestCallSubflow_FactoryError_ReturnsWrappedErr`.
- **Integration:** `test/integration/runtime_routers_test.go` per AGENTS.md §17 — 2-level subflow, streaming load, parent Cancel propagates to both children, bus subscriber asserts run_cancelled events, identity propagation, factory-error failure mode, under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** `TestEngine_ConcurrentReuse_WithRouters_AndSubflow` (D-025 with full surface), `TestEngine_NoGoroutineLeak_AfterStop_WithSubflow`.

## Smoke script additions

- `phase-14.sh`: runs Phase 13's smoke + Phase 14 tests + integration. Closes the runtime kernel chain — phase-09 through phase-14 together cover RFC §6.1.

## Coverage target

- `internal/runtime/routers`: 85%
- `internal/runtime/concurrency`: 85%
- `internal/runtime/engine`: 85% (subflow path)

## Dependencies

- 10 (engine — routers + subflow lean on the engine surface)
- 11 (reliability — `RunError(RouteNotFound)` uses Phase 11's RunError)
- 13 (cancellation — subflow watcher mirrors parent `Cancel`)

## Risks / open questions

- **Subflow recursion has no built-in depth limit.** Documented; future work could add `WithMaxSubflowDepth(n int)`. The per-run capacity waiter backpressures eventually, but a misbehaving subflow factory can still exhaust goroutines. Operators who write recursive subflows are responsible for their own termination conditions.
- **`MapConcurrent` order preservation cost.** The implementation uses an output array indexed by input position; goroutines write to their own slot. O(N) memory per call, fine for V1's expected scale.
- **`JoinK` short-read semantics.** When the input channel closes before K envelopes arrive, `JoinK` returns `ErrJoinKShortRead` along with what it got. Callers must check both the error AND the slice length. Documented.
- **Subflow + cancellation watcher join.** The watcher goroutine joins on either parent-cancel or child-stop; under heavy contention the join could leak briefly until `Stop`. Tested via `TestEngine_NoGoroutineLeak_AfterStop_WithSubflow`.
- **`PredicateRouter.Branches` ordering.** Linear search; O(N) per envelope. For graphs with many branches this is fine; if a future workload needs faster routing, we'd revisit (e.g. add an indexed router variant). Documented.

## Glossary additions

- **`PredicateRouter`.** Router that selects the first branch whose predicate matches the input envelope. Default target catches "no match"; nil default returns `RunError(RouteNotFound)`.
- **`UnionRouter`.** Router that dispatches by payload tag (a string discriminator). Used for sum-type-shaped payloads (e.g. planner `Decision` variants in Phase 42).
- **`RoutePolicy`.** Override mechanism that bypasses predicate/union routing when an envelope's `Meta["route_policy"]` carries an explicit target. The planner-driven path.
- **`MapConcurrent`.** Concurrency utility that runs `fn` over a slice of envelopes with a max-in-flight bound. Preserves input order in output.
- **`JoinK`.** Concurrency utility that reads exactly K envelopes from a channel and cancels remaining producers. Short-read returns `ErrJoinKShortRead`.
- **`Subflow`.** Runtime primitive: `(nctx *NodeContext) CallSubflow(ctx, factory) (Envelope, error)`. Runs a child engine for one parent envelope, mirrors parent cancellation, returns the first egress payload, then `Stop`s the child.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestCallSubflow_PropagatesQuadruple`)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** See AGENTS.md §5 + §11 + D-025. — `TestEngine_ConcurrentReuse_WithRouters_AndSubflow`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** See AGENTS.md §17. — `test/integration/runtime_routers_test.go`.
- [ ] If new vocabulary: glossary updated (PredicateRouter, UnionRouter, RoutePolicy, MapConcurrent, JoinK, Subflow)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 14)
