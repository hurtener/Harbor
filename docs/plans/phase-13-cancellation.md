# Phase 13 — Cancellation + per-run fetch dispatcher

## Summary

Implement Phase 10's stubbed `Engine.Cancel(runID)` and `Engine.FetchByRun(runID)`. `Cancel` is idempotent: it sets a per-run cancellation flag, drops the run's queued envelopes from every channel, cancels its in-flight node invocations, drains its egress subqueue, AND releases its Phase 12 capacity waiter. `FetchByRun(runID)` reads from the dispatcher's per-run subqueue (always-on dispatcher from Phase 10) and never returns frames from a different run. Together these close the runtime-kernel chain's per-run-isolation contract: two concurrent runs cancel and fetch independently.

## RFC anchor

- RFC §6.1
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §4 — cancellation propagation.** "`Cancel(runID)` does four things: sets a per-run `Event`/atomic flag, drops the run's already-enqueued envelopes from every channel, cancels active invocation goroutines, and drains the per-run egress queue. Subflow runs mirror parent cancellation via a watcher goroutine. Harbor port: `Cancel` returns `bool` indicating whether the run was active; idempotent." Phase 13 ships exactly these four behaviors plus the Phase 12 capacity-waiter release.
- **brief 01 §3 — `Cancel` returns `bool`.** "Idempotent per-run cancellation that propagates through queues and active invocations." Returns `(true, nil)` if the run was active and got cancelled; `(false, nil)` if no run with that ID exists or it had already finished. Errors (`ctx.Err()`, etc.) propagate normally.
- **brief 01 §4 — trace-scoped fetch dispatcher.** "A separate goroutine reads from the egress queue and demultiplexes results into per-run subqueues so that callers can `FetchByRun(runID)` without ordering surprises. This is non-trivial — it is what makes the API 'one engine, many concurrent runs, each addressable'." Phase 10's dispatcher is the always-on demux; Phase 13 routes `FetchByRun(runID)` to the right per-run subqueue.
- **brief 01 §5 — `emit_nowait(trace_id=...)` is silently unsupported.** Harbor's API is type-shaped so this can't be expressed at compile time. Phase 13 doesn't expose `EmitNoWait(runID=...)` — only `Emit` accepts `WithRunID`, never `EmitNoWait`. Documented in Phase 10's API surface.
- **brief 01 §5 — per-run roundtrip locks.** "Per-run roundtrip locks serialize concurrent emit/fetch calls sharing the same run id. The semantics are subtle and the failure mode (lock not released on error path) is mitigated with `suppress(RuntimeError)`. Harbor: design `Emit/Fetch` so concurrent same-run roundtrips either work natively or are forbidden by the API; no half-measure." Phase 13 makes concurrent `FetchByRun(runID)` *forbidden* — only one fetcher per run at a time. The dispatcher's per-run subqueue has a single consumer; concurrent `FetchByRun` returns `ErrConcurrentFetchByRun`.
- **brief 01 §6 — concurrency tests required.** "N concurrent runs, each addressed by `RunID`, no cross-run frames in `FetchByRun`. `Cancel(runID)` drops queued envelopes for that run only; other runs continue. Subflow cancellation mirrors when the parent is cancelled mid-subflow." Phase 13 ships all three (subflow cancellation lands in Phase 14 since `Subflow` isn't built yet; Phase 13's plan acknowledges this dependency).
- **brief 01 §6 — fuzz / property.** "`Cancel` is idempotent regardless of when it's called relative to the run lifecycle." Phase 13 ships `TestCancel_Idempotent_Property` exercising Cancel-before-Emit, Cancel-during-flight, Cancel-after-completion, double-Cancel.
- **D-025 — concurrent reuse.** Cancellation flags + per-run subqueues are stored on the `*engine` in mutex-guarded maps. The N≥100 reuse test from Phase 10 is extended in Phase 13 to include cancellation under load; asserts no race / no goroutine leak / cancelled runs don't drop completed runs' work.

## Findings I'm departing from (if any)

- **None.** Phase 13 follows brief 01 §4-5 verbatim. The "concurrent FetchByRun is forbidden" choice is brief 01 §5's preferred resolution ("no half-measure"); no D-NNN required.

## Goals

- Implement `Cancel(runID)`: idempotent, four-step (flag → drop queued → cancel in-flight → drain egress + release capacity waiter), returns `bool` for "was the run active?"
- Implement `FetchByRun(runID)`: reads from the dispatcher's per-run subqueue; concurrent fetchers per run rejected with `ErrConcurrentFetchByRun`.
- Pin per-run isolation under cancellation: cancelling run A leaves run B's frames + workers + egress untouched. The integration test asserts this with two concurrent streaming runs.
- Hook into Phase 12's capacity waiter: `Cancel` releases waiters on the cancelled run so the producing goroutine returns `ErrRunCancelled` immediately.
- Maintain D-025: cancellation maps live behind a mutex; concurrent `Cancel` and `FetchByRun` calls are race-free.

## Non-goals

- No `Subflow` cancellation mirroring — Phase 14 ships `Subflow` and adds the watcher goroutine that propagates parent `Cancel` to child engines.
- No deadline-driven cancellation — Phase 11's `DeadlineExceeded` is a separate path; if a run's `DeadlineAt` expires, the worker emits `RunError(DeadlineExceeded)` but does NOT call `Cancel` (the run is already terminal). Documented.
- No multi-cancel-batched API (`CancelMany([]runID)`) — operators call `Cancel` per run.
- No cancel-with-reason — V1 cancellation is reasonless; the bus event (Phase 13 emits `runtime.run_cancelled`) carries the run's identity but no operator-facing reason. Steering's `CANCEL` event (Phase 52+) will provide reasons.
- No serialization of cancelled-run state. The runtime forgets a cancelled run's identity once the dispatcher drains; downstream consumers (Phase 50 pause/resume) handle persistence.

## Acceptance criteria

- [ ] `internal/runtime/engine/cancel.go` defines `(e *engine) Cancel(ctx context.Context, runID string) (bool, error)`. Replaces Phase 10's stub returning `ErrNotImplemented`.
- [ ] `Cancel` returns `(true, nil)` if the run was active (had pending envelopes OR an in-flight worker invocation OR a non-empty egress subqueue); `(false, nil)` otherwise. `ctx.Err()` returns immediately if ctx is already cancelled.
- [ ] **Four-step cancellation:**
  - (1) Set `cancellation[runID] = true` under the engine's cancellation mutex.
  - (2) Walk every channel; drop envelopes whose `RunID` matches. Track count for the test.
  - (3) Cancel in-flight worker invocations: each worker checks the per-run cancel flag between iterations AND between retry attempts (Phase 11 hooks in here); if true, returns `ErrRunCancelled` and the shell builds `RunError(RunCancelled)`.
  - (4) Release Phase 12's capacity waiter for that run AND drain the dispatcher's per-run subqueue.
- [ ] `Cancel` is **idempotent**. Test `TestCancel_Idempotent_DoubleCancel` calls `Cancel` twice; second call returns `(false, nil)`.
- [ ] `Cancel` is safe to call **before** the run starts. Test `TestCancel_BeforeEmit_ReturnsFalse` calls `Cancel(runID)` first then `Emit(env{RunID: runID})`; the Emit returns `ErrRunCancelled` (the engine remembers the cancellation for a bounded TTL — default 60s — to handle the legitimate "operator cancels just before the run lands" case).
- [ ] **Cancelled run's events emit on the bus.** `runtime.run_cancelled` is registered as a `SafePayload`-marked event type carrying `{run_id, cancelled_at, dropped_envelope_count}`. Test `TestCancel_EmitsBusEvent` asserts an admin subscriber sees the event.
- [ ] `internal/runtime/engine/fetch_by_run.go` defines `(e *engine) FetchByRun(ctx context.Context, runID string) (messages.Envelope, error)`. Replaces Phase 10's stub returning `ErrNotImplemented`.
- [ ] **`FetchByRun` reads from the dispatcher's per-run subqueue.** The dispatcher's demux map (Phase 10) keyed by `RunID` is the source. If no subqueue exists for `runID`, the call blocks waiting for the dispatcher to create one (a fresh `Emit` for that run will). When `ctx` cancels, returns `ctx.Err()`.
- [ ] **Concurrent `FetchByRun` per run is forbidden.** Test `TestFetchByRun_ConcurrentSameRun_ReturnsErrConcurrentFetchByRun` calls `FetchByRun(runID)` from two goroutines; one returns the envelope, the other returns `ErrConcurrentFetchByRun`. The dispatcher's per-run subqueue tracks the active fetcher via a per-run `atomic.Bool`.
- [ ] **`FetchByRun` honors cancellation.** When `Cancel(runID)` is called concurrently with a `FetchByRun(runID)`, the fetch returns `ErrRunCancelled` (mapped from the closed subqueue channel). Test `TestFetchByRun_AfterCancel_ReturnsErrRunCancelled`.
- [ ] **Two-runs isolation.** Test `TestCancel_OneRun_LeavesOtherCompletes` runs two concurrent runs (different RunIDs), cancels run A mid-flight, asserts: (a) run A's worker invocation receives `ErrRunCancelled`, (b) run B's worker completes normally, (c) `FetchByRun("B")` returns run B's egress envelope, (d) the bus emits exactly one `runtime.run_cancelled` event (for A).
- [ ] **Cross-tenant isolation under cancellation.** Test `TestCancel_CrossTenant_NoBleed` cancels tenant A's run; tenant B's runs are untouched.
- [ ] **No deadlock under streaming + cancellation.** Test `TestCancel_DuringStreaming_NoDeadlock` starts a streaming run that is `EmitChunk`-blocked at capacity; `Cancel` is called from another goroutine; asserts: (a) `EmitChunk` returns `ErrRunCancelled` immediately, (b) the worker exits, (c) goroutine baseline restored after `Stop`.
- [ ] **Property test:** `TestCancel_Idempotent_Property` exercises Cancel-before-Emit, Cancel-during-Fetch, Cancel-after-completion, double-Cancel — asserts the boolean return matches the actual run state at each call.
- [ ] **D-025 reuse extended.** `TestEngine_ConcurrentReuse_WithCancel` runs N=100 emitters where 25% randomly call `Cancel(runID)` mid-flight; under `-race`, asserts no race / no leak / no cross-run state corruption.
- [ ] No package-level mutable state. Cancellation map is private + mutex-guarded. The cancelled-runs TTL is implemented via a periodic sweeper in the engine's `Run` loop (default 60s TTL, swept every 10s).
- [ ] Coverage on `internal/runtime/engine` ≥ 85% (cancellation + FetchByRun paths included).
- [ ] **Integration test:** `test/integration/runtime_cancel_test.go` per AGENTS.md §17 — wires real audit + events + state + sessions + engine; runs two streaming runs concurrently; cancels one; asserts the bus emits `runtime.run_cancelled`, run B completes, no goroutine leak.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-13.sh` smoke runs Phase 12's smoke + Phase 13-specific tests + integration test.

## Files added or changed

```text
internal/runtime/engine/cancel.go             # Cancel implementation, cancellation map, TTL sweeper
internal/runtime/engine/fetch_by_run.go       # FetchByRun + per-run fetcher exclusivity
internal/runtime/engine/dispatcher.go         # extended: closes subqueue on Cancel
internal/runtime/engine/streaming.go          # extended: capacity waiter releases on Cancel
internal/runtime/engine/cancel_test.go        # idempotency + bool return + property
internal/runtime/engine/fetch_by_run_test.go  # demux + concurrent rejection
internal/runtime/engine/payloads.go           # extended: RunCancelledPayload (SafePayload)
internal/events/events.go                     # registers runtime.run_cancelled EventType
test/integration/runtime_cancel_test.go       # cross-subsystem integration test
scripts/smoke/phase-13.sh                     # Go-package + integration test invocation
docs/plans/README.md                          # Status: 13 → Shipped (in implementation PR)
docs/glossary.md                              # adds Cancel, FetchByRun, cancellation TTL, run_cancelled
```

## Public API surface

```go
package engine

// Cancel is idempotent per-run cancellation. Returns (true, nil) if
// the run was active (had pending envelopes, in-flight workers, or a
// non-empty egress subqueue); (false, nil) otherwise. Cancellation is
// remembered for a bounded TTL (default 60s) so an Emit landing just
// after Cancel is rejected with ErrRunCancelled.
//
// Cancel propagates through:
//   1. Per-run cancellation flag.
//   2. Drops queued envelopes for the run from every channel.
//   3. Cancels in-flight worker invocations (workers observe the flag
//      between iterations; the reliability shell observes between
//      retries).
//   4. Releases capacity waiters AND drains the egress subqueue.
//
// Subflow cancellation mirroring lands in Phase 14 (parent Cancel
// propagates to subflow engines via a watcher goroutine).
func (e *engine) Cancel(ctx context.Context, runID string) (bool, error)

// FetchByRun reads from the dispatcher's per-run subqueue. Blocks
// until a frame is available or ctx cancels. Concurrent FetchByRun
// for the same runID returns ErrConcurrentFetchByRun — only one
// fetcher per run at a time (brief 01 §5).
func (e *engine) FetchByRun(ctx context.Context, runID string) (messages.Envelope, error)

var (
    ErrRunCancelled            = errors.New("engine: run cancelled")
    ErrConcurrentFetchByRun    = errors.New("engine: only one FetchByRun in flight per run")
)
```

## Test plan

- **Unit:** `TestCancel_HappyPath_ReturnsTrue`, `TestCancel_Idempotent_DoubleCancel`, `TestCancel_BeforeEmit_RememberedForTTL`, `TestCancel_EmitsBusEvent`, `TestCancel_OneRun_LeavesOtherCompletes`, `TestCancel_CrossTenant_NoBleed`, `TestCancel_DuringStreaming_NoDeadlock`, `TestCancel_Idempotent_Property`, `TestFetchByRun_HappyPath`, `TestFetchByRun_ConcurrentSameRun_ReturnsErrConcurrentFetchByRun`, `TestFetchByRun_AfterCancel_ReturnsErrRunCancelled`, `TestFetchByRun_CtxCancelled_ReturnsCtxErr`.
- **Integration:** `test/integration/runtime_cancel_test.go` per AGENTS.md §17 — two streaming runs concurrent, one cancelled; bus subscriber asserts `runtime.run_cancelled`; run B completes; goroutine baseline restored; identity propagation in the cancelled event; under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** `TestEngine_ConcurrentReuse_WithCancel` (D-025 with random cancellation), `TestEngine_NoGoroutineLeak_AfterStop_WithCancellation`.

## Smoke script additions

- `phase-13.sh`: runs Phase 12's smoke + Phase 13 tests + integration. Includes the property test as a separate `go test -run TestCancel_Idempotent_Property -count=10` invocation to flush flake-prone interleavings.

## Coverage target

- `internal/runtime/engine`: 85%

## Dependencies

- 10 (engine — fills `Cancel` + `FetchByRun` stubs)
- 12 (streaming — `Cancel` releases capacity waiters)
- 11 (reliability — shell observes per-run cancel flag between retries)
- 04 (logger), 05 (events bus — emits `runtime.run_cancelled`)

## Risks / open questions

- **Cancelled-run TTL.** Default 60s. Operators who pre-compute `RunID` and call `Cancel` before `Emit` need the cancellation to "stick" for at least the round-trip. 60s is generous; documented. Configurable via `WithCancelTTL(d time.Duration) Option` if a future workload demands tighter.
- **Periodic sweeper.** A goroutine sweeps cancelled-runs older than `CancelTTL` every 10s. Joined on `Stop`. Documented; goroutine baseline test asserts it joins.
- **`FetchByRun` blocking semantics.** When no subqueue exists for `runID`, the call blocks until one appears (a future `Emit` creates it). If `runID` is never `Emit`-ed, the call blocks until ctx cancels. This matches the predecessor's behavior; documented.
- **Subflow cancellation deferred to Phase 14.** Phase 13's plan flags this; Phase 14's plan must wire the watcher goroutine that propagates parent `Cancel` to child engines.
- **No batched cancel API.** Operators cancel per run; if a future workload needs `CancelMany`, we revisit. Documented.

## Glossary additions

- **`Cancel(runID)`.** Engine method that idempotently cancels a run: sets the cancellation flag, drops queued envelopes, cancels in-flight workers, drains egress, releases capacity waiters. Returns `(bool, error)` indicating whether the run was active.
- **`FetchByRun(runID)`.** Engine method that reads from the dispatcher's per-run subqueue. Concurrent fetchers per run are forbidden (`ErrConcurrentFetchByRun`).
- **Cancellation TTL.** Bounded duration (default 60s) the engine remembers cancellation flags for runs that may not have started yet. Sweeper goroutine runs every 10s; joined on `Stop`.
- **`runtime.run_cancelled`.** SafePayload event type. Emitted by `Cancel` when the run was active. Carries `{run_id, cancelled_at, dropped_envelope_count}`. Filterable by admin subscribers.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestCancel_CrossTenant_NoBleed`)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** See AGENTS.md §5 + §11 + D-025. — `TestEngine_ConcurrentReuse_WithCancel`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** See AGENTS.md §17. — `test/integration/runtime_cancel_test.go`.
- [ ] If new vocabulary: glossary updated (Cancel, FetchByRun, Cancellation TTL, run_cancelled)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 13)
