# Phase 12 — Streaming + per-run capacity backpressure

## Summary

Land the streaming primitive — `StreamFrame`, `EmitChunk`, per-stream `Seq` ordering, terminal `Done` frame — and the **per-run capacity backpressure** that prevents the predecessor's deadlock-under-streaming sharp edge. Capacity waiters are keyed by `RunID`; a single run that emits hundreds of stream frames cannot fill an outgoing queue and block another run sharing the engine's bounded channels. **Backpressure is baked in, not bolted on.** This phase is the second-highest-risk phase on the V1 critical path (per master plan README); a "we'll add it later" PR is rejected on sight.

## RFC anchor

- RFC §6.1
- RFC §3.5

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §1 — streaming is one of the runtime's three core responsibilities.** "A streaming primitive for incremental outputs that share a parent run identity, with sequence ordering and per-stream backpressure." Phase 12 ships exactly this.
- **brief 01 §2 — `StreamFrame` shape.** `StreamID string` (defaults to `RunID`; can be sub-stream within a run), `Seq int` (monotonic per StreamID), `Text string` (model-emitted text or adapter-converted), `Done bool` (terminal frame), `Meta map[string]any` (tokens, finish reason, citations). Phase 12 ships this verbatim.
- **brief 01 §4 — backpressure inside streaming.** "A run that emits hundreds of stream frames could fill its outgoing queue and block the producing goroutine. The source addresses this with `_await_trace_capacity`: per-run pending counters with capacity waiters, gating chunk emissions when a single run's pending count exceeds the queue maxsize. Harbor must port this — it is *not* a nice-to-have. Without it, parallel runs can deadlock each other through shared bounded queues." Phase 12 implements this as a hard requirement. The capacity waiter is keyed by `RunID`; a per-run pending counter is incremented on every `EmitChunk` and decremented when the dispatcher drains the frame.
- **brief 01 §5 — `Stop` releases capacity waiters cleanly.** "The waiter's resumption path needs an explicit 'engine stopped' sentinel, not 'you happen to observe trace_count=0'." Phase 12 ships an explicit `errEngineStoppedDuringWait` sentinel that the shell maps to `ErrEngineStopped` from Phase 10.
- **brief 01 §6 — streaming tests required.** "Per-stream `Seq` ordering, terminal `Done` frame, downstream backpressure under a slow consumer." Phase 12 implements all three, plus the deadlock-prevention test that's the entire reason this phase exists.
- **RFC §6.1 — per-run capacity backpressure is a Runtime primitive.** "Without it, parallel runs can deadlock through shared bounded channels under streaming load." The RFC settles backpressure as foundational, not optional.
- **D-025 — concurrent reuse.** Capacity waiters are stored on the `*engine` in a `map[runID]*runCapacity` guarded by a `sync.Mutex`. The N≥100 reuse test from Phase 10 is extended in Phase 12 to exercise streaming under concurrent runs; asserts no race / no deadlock / no goroutine leak after Stop.
- **AGENTS.md §17.3 — concurrency stress run.** Phase 12's integration test exercises N parallel runs × K frames each; asserts ordering preserved per `StreamID`, no cross-run deadlock, goroutine baseline restored after Stop. This is the wave-end-style test for streaming hygiene.

## Findings I'm departing from (if any)

- **None.** Phase 12 follows brief 01 verbatim. The biggest design decision (backpressure baked in, not optional) is RFC-settled and master-plan-flagged as the highest-risk phase in the runtime chain. No D-NNN required.

## Goals

- Ship `StreamFrame` and `NodeContext.EmitChunk` so nodes that produce incremental output (LLM stream adapters, multi-step tool outputs) can emit frames without filling their outgoing channel.
- Implement the per-run capacity waiter: a single `RunID` cannot have more than `Policy.RunCapacity` (default = engine's `DefaultQueueSize` = 64) frames pending across the egress dispatcher's subqueues. When the cap is hit, `EmitChunk` blocks until the dispatcher drains a frame from that run's subqueue.
- Pin `Seq` ordering per `StreamID`: frames emitted in order are delivered in order. The dispatcher's per-run subqueue is a FIFO; cross-run interleaving is fine but within-stream order is invariant.
- Terminal `Done: true` is honored: the dispatcher releases the run's capacity counter when the `Done` frame drains, so a finished stream never leaves stale capacity bookkeeping.
- Stop semantics: `Stop(ctx)` closes all capacity waiters with `ErrEngineStopped`; an in-flight `EmitChunk` returns immediately; the worker loop never deadlocks on shutdown.

## Non-goals

- No cancellation of streaming runs — Phase 13's `Cancel(runID)` will close the run's subqueue + release its capacity waiter; Phase 12 ships only the no-cancel happy path.
- No JSON-Schema validation of stream frames. The frames are semi-structured (`Text + Meta`); operators who need schema validation use Phase 11's `NodePolicy.ValidateFunc` on the producing node's emit.
- No per-frame cost accounting. Phase 36a (governance cost accumulator) reads from the LLM client, not from individual stream frames.
- No SSE/WebSocket protocol projection — Phase 60 ships the wire transport.
- No flow-level capacity (`flow.Budget`). Phase 26a wires that.
- No back-buffering of historical frames. The runtime delivers each frame once; the events bus's Replayer (Phase 06) preserves bus-level events but not raw stream frames.

## Acceptance criteria

- [ ] `internal/runtime/engine/streaming.go` defines `StreamFrame{StreamID, Seq, Text, Done, Meta}` and `(nctx *NodeContext) EmitChunk(ctx context.Context, frame StreamFrame) error`.
- [ ] `EmitChunk` blocks when the run's pending-frame counter has reached `Policy.RunCapacity`. The block is on a `sync.Cond` keyed by `RunID`; the dispatcher's drain path signals the cond when it drains a frame from the run's subqueue. Test `TestEmitChunk_BlocksAtCapacity_ReleasedOnDrain` constructs a slow consumer + fast producer and asserts the producer blocks (not deadlocks) until the consumer drains.
- [ ] `Seq` is monotonic per `StreamID`. The engine maintains a per-`StreamID` `atomic.Int64` and rejects any frame whose `Seq` is non-zero on input (the engine assigns `Seq`; callers don't pre-fill). Test `TestEmitChunk_RejectsCallerProvidedSeq` and `TestEmitChunk_SeqMonotonicPerStream`.
- [ ] **`Done: true` is terminal.** When the dispatcher drains a `Done` frame, it: (a) decrements the per-run pending counter, (b) deletes the per-`StreamID` `Seq` counter, (c) signals the capacity cond. Subsequent `EmitChunk` for that `StreamID` returns `ErrStreamClosed`. Test `TestEmitChunk_DoneFrame_TerminatesStream`, `TestEmitChunk_AfterDone_ReturnsErrStreamClosed`.
- [ ] **No cross-run deadlock.** Test `TestEmitChunk_CrossRun_NoDeadlock` runs 8 producer goroutines (one per `RunID`) emitting 100 frames each into a 3-node engine with a slow Outlet consumer; one consumer goroutine drains the Outlet at 50ms intervals. Asserts: (a) all 800 frames delivered, (b) per-stream order preserved, (c) goroutine baseline restored after `Stop`. Pins the deadlock-under-streaming gap brief 01 §4 calls out.
- [ ] **N parallel runs × K frames.** The classic streaming integration test from brief 01 §6: `TestE2E_Phase12_ParallelRuns_StreamFrames` does 4 runs × 50 frames; asserts ordering preserved per StreamID, no goroutine leak, no race. Under `-race`.
- [ ] **`Stop` releases capacity waiters.** Test `TestEmitChunk_Stop_ReleasesWaiters` starts a producer goroutine on a saturated run, then calls `Stop(ctx)`. Asserts: (a) the producer's `EmitChunk` returns `ErrEngineStopped` (not deadlock), (b) goroutine baseline restored.
- [ ] **`Policy.RunCapacity` configurable.** Default = engine's `DefaultQueueSize` (64). Per-run override via `WithRunCapacity(n int) EmitOption` on the originating `Emit`; passed through the worker loop into the run's capacity counter.
- [ ] **Identity propagation in stream frames.** The engine wraps each `StreamFrame` in an `Envelope` (per the existing channel mechanic) carrying the originating run's quadruple; admin subscribers can scope by tenant/user/session/run. Test `TestStreamFrame_CarriesIdentity` asserts the wrapping envelope's identity matches the originating run.
- [ ] **D-025 reuse test extended.** `TestEngine_ConcurrentReuse_Streaming` runs ≥100 goroutines `EmitChunk`-ing on a single shared `*engine` (one stream per goroutine, all distinct `RunID`s); under `-race`, asserts no race, no goroutine leak after Stop, no cross-stream interleave.
- [ ] No package-level mutable state on `*engine`. The capacity tracker is a private `sync.Map[runID]*runCapacity` accessed via mutex-guarded helpers. Documented as "internally synchronized" per AGENTS.md §5.
- [ ] Coverage on `internal/runtime/engine` ≥ 85% (streaming code paths included).
- [ ] **Integration test:** `test/integration/runtime_streaming_test.go` per AGENTS.md §17 — wires real audit + events + state + sessions + engine; runs the parallel-runs streaming scenario; covers ≥1 failure mode (`Stop` mid-stream returns `ErrEngineStopped` to in-flight `EmitChunk`); under `-race`.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-12.sh` smoke runs Phase 11's smoke + Phase 12-specific tests + integration test.

## Files added or changed

```text
internal/runtime/engine/streaming.go            # StreamFrame, EmitChunk, run-capacity
internal/runtime/engine/dispatcher.go           # extended: drains stream frames + signals capacity
internal/runtime/engine/streaming_test.go       # ordering, Done, cross-run no-deadlock
internal/runtime/engine/streaming_concurrent_test.go  # D-025 streaming
test/integration/runtime_streaming_test.go      # parallel-runs streaming integration
scripts/smoke/phase-12.sh                       # Go-package + integration test invocation
docs/plans/README.md                            # Status: 12 → Shipped (in implementation PR)
docs/glossary.md                                # adds StreamFrame, EmitChunk, capacity waiter, Stop sentinel
```

## Public API surface

```go
package engine

// StreamFrame is a chunked payload tied to a parent run. StreamID
// defaults to RunID; sub-streams within a run use a custom StreamID.
// Seq is monotonic per StreamID and is engine-assigned (callers must
// NOT pre-fill — the engine rejects with ErrSeqProvided).
type StreamFrame struct {
    StreamID string
    Seq      int
    Text     string
    Done     bool
    Meta     map[string]any
}

// EmitChunk emits a stream frame. Blocks when the originating run's
// pending-frame count has reached Policy.RunCapacity (default = the
// engine's DefaultQueueSize, 64). The block is per-run, never per-
// engine — a single run's saturation does not pause other runs (this
// is the deadlock-prevention guarantee from brief 01 §4).
//
// Done: true marks the terminal frame for the StreamID. After a Done
// frame drains, subsequent EmitChunk for that StreamID returns
// ErrStreamClosed.
func (nctx *NodeContext) EmitChunk(ctx context.Context, frame StreamFrame) error

// WithRunCapacity overrides the default per-run capacity for the run
// initiated by this Emit. Pass to Engine.Emit at run start. Default is
// the engine's DefaultQueueSize (64).
func WithRunCapacity(n int) EmitOption

var (
    ErrSeqProvided  = errors.New("engine: caller pre-filled StreamFrame.Seq; engine owns sequencing")
    ErrStreamClosed = errors.New("engine: stream closed (Done frame already drained)")
)
```

## Test plan

- **Unit:** `TestStreamFrame_RejectsCallerProvidedSeq`, `TestEmitChunk_SeqMonotonicPerStream`, `TestEmitChunk_DoneFrame_TerminatesStream`, `TestEmitChunk_AfterDone_ReturnsErrStreamClosed`, `TestEmitChunk_BlocksAtCapacity_ReleasedOnDrain`, `TestEmitChunk_Stop_ReleasesWaiters`, `TestStreamFrame_CarriesIdentity`, `TestWithRunCapacity_OverridesDefault`.
- **Integration:** `test/integration/runtime_streaming_test.go` per AGENTS.md §17 — N parallel runs × K frames; bus subscriber asserts each run's frames appear in order; failure mode `Stop` mid-stream covered; under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** `TestEmitChunk_CrossRun_NoDeadlock` (the deadlock-prevention test that justifies this phase), `TestEngine_ConcurrentReuse_Streaming` (D-025), `TestEngine_NoGoroutineLeak_AfterStop_WithStreaming`.

## Smoke script additions

- `phase-12.sh`: runs Phase 11's smoke + Phase 12-specific tests + integration. The streaming smoke is the heaviest concurrency exercise so far in Wave 4; flake-resistant assertions (channels, controllable clock for the slow consumer) per AGENTS.md §17.4.

## Coverage target

- `internal/runtime/engine`: 85%

## Dependencies

- 10 (engine — extends the dispatcher)
- 11 (reliability — extends `NodePolicy` with `RunCapacity`)

## Risks / open questions

- **The deadlock-prevention test is the gate.** If `TestEmitChunk_CrossRun_NoDeadlock` ever times out under -race in CI, the phase has shipped wrong and must be rolled back. The master plan explicitly flags this: "This is Brief 01's 'must bake in.' Don't accept a 'we'll add it later' PR."
- **Capacity waiter under high contention.** Many concurrent producers on the same run with a saturated subqueue all wait on the same `sync.Cond`; signal vs broadcast affects fairness. Phase 12 uses `cond.Signal()` + a per-run mutex (one waiter wakes per drain), matching the predecessor's behavior. If a future workload exposes starvation, we revisit; documented.
- **`RunCapacity` default = `DefaultQueueSize`.** Means a single run can saturate the dispatcher's subqueue before triggering backpressure. This is intentional: backpressure is the *fallback*, not the primary throttle. Operators who want tighter streaming budgets pass `WithRunCapacity(smaller)`. Documented.
- **Backpressure visibility.** When `EmitChunk` blocks, no event is currently emitted. Phase 56 (metrics) will surface "blocked-on-capacity" as a runtime metric. Out of scope here; documented.

## Glossary additions

- **`StreamFrame`.** Chunked payload tied to a parent run. `StreamID` (defaults to `RunID`), `Seq` (engine-assigned, monotonic per StreamID), `Text`, `Done`, `Meta`. Distinct from `events.Event` (which carries lifecycle markers); StreamFrames carry incremental output.
- **`EmitChunk`.** `NodeContext` method that emits a `StreamFrame`. Blocks when the run's pending-frame count has reached `Policy.RunCapacity`. Backpressure is per-run; one run's saturation never pauses another.
- **Capacity waiter (engine).** Per-run `sync.Cond` the engine uses to gate `EmitChunk` when a run's pending-frame count has reached its `RunCapacity`. Released when the dispatcher drains a frame from the run's subqueue, or when `Stop` closes the engine.
- **`RunCapacity`.** Per-run cap on pending stream frames. Default = `DefaultQueueSize` (64). Overridable per-run via `WithRunCapacity(n)` on `Emit`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestStreamFrame_CarriesIdentity` + extended `TestEngine_CrossTenant_NoBleed`)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** See AGENTS.md §5 + §11 + D-025. — `TestEngine_ConcurrentReuse_Streaming`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** See AGENTS.md §17. — `test/integration/runtime_streaming_test.go`.
- [ ] If new vocabulary: glossary updated (StreamFrame, EmitChunk, Capacity waiter, RunCapacity)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 12)
