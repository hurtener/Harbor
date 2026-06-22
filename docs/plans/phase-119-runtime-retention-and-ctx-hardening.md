# Phase 119 — runtime-retention-and-ctx-hardening

## Summary

Closes the three confirmed unbounded-growth / shutdown-hazard findings surfaced by the 2026-06 runtime audit: the engine streaming-capacity maps that are never reaped (one entry per run, retained for the engine's lifetime), the governance cost/rate-limit caches that grow one entry per identity-quadruple forever, and the rolling-summary recovery loop whose `context.Background()` summariser call can wedge `Close()` indefinitely. Bundles two low-severity control-flow cleanups in the same wave. No new operator surface — pure runtime correctness + the missing leak test that should have guarded the recovery loop.

## RFC anchor

- RFC §6.1
- RFC §6.6
- RFC §6.15

## Briefs informing this phase

- brief 01
- brief 04

## Brief findings incorporated

- brief 01 §"Concurrent reuse contract": compiled artifacts (the `Engine`) hold no per-run state that crosses run boundaries; any map keyed by `RunID` that the engine retains for its own lifetime is a latent retention leak and must be reaped on a run-end signal under the same lock that guards it. This phase makes the `capacities` / `runCapacityOverrides` maps obey that contract.
- brief 01 §"Goroutine lifecycle": every goroutine a long-lived component starts must be cancellable via a `ctx` it owns and joined before the owner's shutdown returns; a background loop that blocks on a non-cancellable downstream call violates the join guarantee. The rolling-summary `recoveryLoop` is brought into compliance.
- brief 04 §"Memory strategies — summarisation": the summariser is an injected, possibly-remote dependency; a strategy that calls it must thread a cancellable `ctx` so a hung summariser cannot pin the strategy's shutdown. The recovery path currently passes `context.Background()`, carrying nothing to honour.
- brief 01 §"Fail loudly / honest docs": godoc that claims a bound it does not deliver is drift; the `Close()` doc is corrected to match the (now actually cancellable) behaviour.

## Findings I'm departing from (if any)

None.

## Goals

- The engine's `capacities` and `runCapacityOverrides` maps (and the per-run `sync.Cond`) are released when a run **actually ends** — keyed off a true run-terminal signal (the streaming-completion path) or a `completedAt`-stamped TTL sweep mirroring the existing cancellation-map sweeper, **not** off `markRunDone` (which is a per-invocation refcount that cycles 1→0→1→0 as an envelope hops nodes — see Risks). A long-lived engine serving N runs returns toward its baseline map footprint rather than growing without bound.
- The governance cost-aggregation and rate-limit caches stop growing one entry per `(tenant, user, session, run)` forever. RFC §6.15 scopes both ceilings to **identity** (`(tenant, user, session)`, and per-model for rate) — `RunID` is not part of identity, so per-run keying is a latent correctness bug, not just a leak. The fix drops `RunID` from the cache key **and from the matching persisted StateStore key** so the in-memory aggregate and the durable record agree (and survive restart).
- The rolling-summary recovery loop runs the summariser under a `ctx` that `Close()` cancels, so shutdown is bounded by the summariser honouring cancellation rather than blocking forever; the `Close()` godoc is corrected to describe the real bound.
- A goroutine-leak test covers the recovery loop (the §11-mandated `NumGoroutine` baseline that was missing), including a hung-summariser variant.
- The two low-severity cleanups land: the no-op `break`-inside-`select` early-exit in `MapConcurrent` is removed, and the engine multi-parent fan-in read path stops busy-polling with a fresh per-iteration timer.

## Non-goals

- Exposing any of these metrics over the Protocol or the Console — that is Phase 120 (gauges) and Phase 121 (surfacing them in the existing health panel). This phase only makes the maps *bounded*; observing them is downstream.
- Adding `goleak` as a dependency — Phase 120. This phase's recovery-loop leak test uses the existing `runtime.NumGoroutine` baseline pattern.
- Re-architecting the governance aggregation model or the streaming backpressure design. Dropping `RunID` from the governance cache key is **not** a re-architecture — it is the RFC §6.15-mandated identity scope (per-run keying was the bug); the aggregation model itself is unchanged. The engine reap mirrors the existing cancellation-map TTL sweeper rather than inventing new machinery.

## Acceptance criteria

- [ ] After a run truly ends, `engine.capacities` and `engine.runCapacityOverrides` no longer contain that run's entry; verified by (a) a steady-state test that runs N sequential runs against one engine and asserts both maps return to baseline, AND (b) an interleaved test that drives run A's chunks while run B reaches terminal state and asserts **no mid-run reap** of A's in-flight tracker (the sequential test alone would pass even under a premature/churning reap — this guards against keying off the `markRunDone` per-invocation refcount).
- [ ] Reaping takes `capMu` and is safe under the `-race` detector with concurrent runs in flight (a reap of run A must not disturb run B's tracker).
- [ ] The governance cost cache and rate-limit cache are keyed by identity (`(tenant, user, session)`, per-model for rate) with `RunID` removed, in both the in-memory `keys` map and the persisted StateStore key: a test issuing N runs for one session asserts the in-memory `keys` length is bounded by distinct identity-scopes (not N), AND a restart-reload test asserts the aggregate reloaded from the StateStore matches the pre-restart total (no per-run fragmentation, no divergence).
- [ ] `rollingSummaryExec.Close()` returns within a bounded time when the summariser is hung, because the recovery `ctx` is cancelled **and the summariser honours cancellation**; the leak-test fake summariser must `select` on `ctx.Done()`, and the test asserts `Close()` completes and the goroutine count returns to baseline.
- [ ] The `Close()` godoc no longer claims an unconditional bound that `context.Background()` cannot deliver; it describes the cancellable recovery `ctx`.
- [ ] `MapConcurrent` no longer contains the dead `break`-inside-`select`; cancellation behaviour is unchanged (covered by the existing cancellation test) and the second `select` remains the real cancel check.
- [ ] The engine `readAny` poll loop (`engine.go`, which serves every node's worker — not only multi-parent fan-in) no longer allocates a fresh `time.After` per iteration; the replacement reuses one timer with correct `Reset`/`Stop`/drain handling; cancellation is still honoured (a parallel-cancel test passes).
- [ ] No regression: all prior engine / governance / memory-strategy tests pass under `-race`.

## Files added or changed

- `internal/runtime/engine/engine.go` — struct comment correction; the `readAny` single-timer fix; reap hook wiring on the run-terminal signal.
- `internal/runtime/engine/streaming.go` — capacity-tracker reap keyed off true run completion (or a `completedAt` TTL sweep mirroring the cancellation-map sweeper), under `capMu`.
- `internal/runtime/engine/cancel.go` — reap on cancel path (already takes `capMu`); note this is distinct from `markRunDone`'s per-invocation refcount.
- `internal/runtime/engine/streaming_reap_test.go` (new) — steady-state + interleaved no-mid-run-reap tests.
- `internal/governance/cost.go` — drop `RunID` from the `quadKey` used for both the in-memory `keys` map and the persisted StateStore key (identity scope per RFC §6.15).
- `internal/governance/ratelimit.go` — same identity-scoping for the rate-limit `keys` map + persisted key.
- `internal/governance/governance.go` — the shared `quadKey` derivation (if RunID is stripped there).
- `internal/governance/*_test.go` — bounded-cache test + restart-reload aggregate-correctness test.
- `internal/memory/strategy/rolling_summary.go` — cancellable recovery `ctx`; godoc correction.
- `internal/memory/strategy/rolling_summary_test.go` (new leak test) — `NumGoroutine` baseline + hung-summariser variant.
- `internal/runtime/concurrency/map.go` — remove dead `select`.

## Public API surface

- No new exported types or signatures. The reap is internal; the recovery-ctx change is internal to the strategy. `Close()` semantics are unchanged from the caller's perspective except that they are now actually bounded.

## Test plan

- **Unit:** `MapConcurrent` cancellation unchanged after the dead-`select` removal; governance cache-key scoping computes the intended scope.
- **Integration:** steady-state run loop (N≥50 sequential runs on one shared engine) asserting `capacities`/`runCapacityOverrides` length returns to baseline + an interleaved no-mid-run-reap case; governance cache bounded across N runs for one session AND a restart-reload aggregate-correctness check (persist N runs, reload, assert the identity-scoped total matches) — real in-mem `StateStore` (and a persisted driver for the restart case) on the seam, identity propagated, run under `-race`.
- **Conformance:** N/A — no driver-interface surface changes.
- **Concurrency / leak:** rolling-summary `Close()`-with-hung-summariser leak test (`NumGoroutine` baseline-restored); engine reap under concurrent in-flight runs with `-race` (a reap of one run must not race another run's tracker).

## Smoke script additions

- `scripts/smoke/phase-119.sh` (static-only): assert the corrected `Close()` godoc no longer contains the overstated-bound phrasing; assert a reap call site referencing `capMu` exists in `streaming.go`/`cancel.go`; assert `MapConcurrent` no longer contains the `break` inside the first `select`. These are file/text greps — the runtime behaviour is gated by the `go test` acceptance criteria, not the live server.

## Coverage target

- `internal/runtime/engine`: 85% (maintain).
- `internal/governance`: 90% (maintain).
- `internal/memory/strategy`: 85% (raise from current — the new leak test closes the §11 gap).

## Dependencies

- 12 (streaming + per-run capacity backpressure — the maps being reaped)
- 13 (cancellation + per-run fetch dispatcher — the cancel path the reap hooks into)
- 24 (memory strategies — the rolling-summary strategy)
- 36a / 36b (governance cost ceilings + rate limits — the caches being bounded)

## Risks / open questions

- **Engine reap signal (DO NOT use `markRunDone`):** `markRunDone` (`cancel.go:337`) is a *per-invocation* refcount that brackets a worker's presence on a run; `activeRuns[runID]` cycles 1→0→1→0 as an envelope hops nodes, so reaping when it hits zero would delete a still-running run's capacity tracker mid-flight. The struct comment (`engine.go:104-106`) already says capacities await "a future run-end signal," and the cancellation maps are "bounded by the cancellation TTL sweeper" (`:112-114`) — that sweeper is the idiom to mirror. Open question: hook the reap on the genuine streaming-completion signal, or stamp `completedAt` and TTL-sweep like cancellations? Resolve by reading the streaming-completion path before implementation; **proposed D-248** records the reap-on-true-run-end policy.
- **Governance cache key — settled by RFC, not open:** RFC §6.15 (lines ~1192-1193) scopes cost ceilings and rate limits to **identity** ("per-identity cost ceilings"; "token bucket per (identity, model)"), and `RunID` is not part of `identity`. So per-run keying is a pre-existing correctness bug (it fragments the aggregate) and dropping `RunID` is the RFC-valid fix — there is no per-run alternative to weigh. The only real care is consistency: strip `RunID` from the in-memory key **and** the persisted StateStore key together so the aggregate survives restart. **Proposed D-249** records the identity-scoping fix (cite §6.15 + master-plan rows 36a/36b).
- **Rolling-summary recovery ctx:** **proposed D-250** records the cancellable-recovery-ctx lifecycle (derived via `context.WithCancel`, cancel stored, called in `Close()` before `loopWG.Wait()`).
- RFC §11 open questions: none directly; this is corrective work within shipped subsystems.

## Glossary additions

- None — "reap", "capacity tracker", "recovery loop" are already in use in the touched packages.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (governance cache scoping is identity-keyed — assert no cross-quadruple bleed)
- [ ] **If this phase builds a reusable artifact:** N/A for new artifacts — this phase modifies existing reusable artifacts (`Engine`, the rolling-summary strategy); the existing D-025 concurrent-reuse tests must still pass and the engine reap is additionally exercised under concurrent in-flight runs with `-race`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** integration test exists (steady-state engine loop + governance bounded-cache), real drivers on the seam, identity propagation, ≥1 failure mode (hung summariser), under `-race`.
- [ ] If new vocabulary: glossary updated — N/A
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departures); proposed D-248/D-249/D-250 filed at implementation
