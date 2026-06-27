# Phase 132-stream — `WithStream` sink on `RunOnce`

## Summary

Adds a `WithStream(func(StreamEvent)) RunOption` to the production
one-call runner `Stack.RunOnce`: one streaming sink on the SAME blocking
method. `RunOnce` still blocks and returns the terminal
`planner.AnswerEnvelope`, but the sink receives token deltas,
planner-step boundaries, and tool dispatches as they occur. The sink is
wired to the two callbacks the run loop already fires **synchronously on
the run goroutine** — `planner.RunContext.OnChunk` (token + step) and
`steering.RunSpec.OnToolDispatched` (tool dispatch) — so every event is
delivered before the envelope returns, deterministically. No separate
streaming package, no sync/async method split.

## RFC anchor

- RFC §3.6 (the public SDK facade — `sdk/assemble` aliases re-export
  `WithStream` + `StreamEvent`)
- RFC §6.2 (Planner interface + `RunContext` population — the `OnChunk`
  streaming callback the sink taps)
- RFC §6.4 (the Runtime owns the run loop / tool dispatch — the
  `OnToolDispatched` hook the sink taps)

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- brief 01 §3 ("A streaming primitive for incremental outputs that share
  a parent run identity, with sequence ordering and a terminal `Done`
  frame"): the embed streaming sink preserves that contract — token
  deltas in arrival order, a `done=true` terminal mapped to a
  `StreamStep` boundary, all scoped to the run's identity quadruple.
- brief 01 §"Backpressure inside streaming" (per-run capacity waiter so
  "parallel runs can deadlock each other through shared bounded
  queues"): the embed sink adds NO new queue — it rides the existing
  synchronous `OnChunk`/`OnToolDispatched` seam, so a slow sink applies
  natural backpressure on its own run goroutine only, never across runs.
- brief 01 §"A non-blocking emit that silently rejects a run id is a
  trap" / "Bus publishing failures must be surfaced, not logged": the
  sink is additive to (never a replacement for) any bus-backed publisher
  `NewRunContext` already wired, and it introduces no silent-drop path —
  a nil sink is an explicit no-op, not a swallowed error.

## Findings I'm departing from (if any)

None.

## Goals

- A single streaming sink on the existing blocking `Stack.RunOnce` —
  `WithStream(func(StreamEvent)) RunOption` — that emits token / step /
  tool-dispatched events as the run progresses.
- A minimal public `StreamEvent` type (token / tool-dispatched / step
  kinds) re-exported through `sdk/assemble`.
- Deterministic ordering: every `StreamEvent` reaches the sink before
  `RunOnce` returns the envelope (synchronous-seam delivery).
- No cross-run chunk bleed: each run's sink receives only its own run's
  chunks.

## Non-goals

- A separate `RunOnceStream` / `RunOnceAsync` method (a parallel
  implementation of the same feature — §13 forbids it; the sink rides
  the same blocking call).
- A new `internal/runtime/streaming` package on the `RunOnce` path (the
  seam already exists; inventing one is forbidden by the phase brief).
- Streaming raw tool arguments or results (§7 — the streaming surface
  carries progress signals, never unredacted payloads).
- Changing `RunOnce`'s signature (the functional-option `RunOption`
  absorbs the new knob).

## Acceptance criteria

- [ ] `assemble.WithStream(func(StreamEvent)) RunOption` exists and adds
      a `stream` field to `runOnceConfig` without touching `RunOnce`'s
      signature.
- [ ] Public `StreamEvent` type with three sealed kinds:
      `StreamToken`, `StreamToolDispatched`, `StreamStep`.
- [ ] The sink is wired to `planner.RunContext.OnChunk` (token + step)
      and `steering.RunSpec.OnToolDispatched` (tool dispatch), both
      additive to any existing publisher.
- [ ] `sdk/assemble` re-exports `WithStream`, `StreamEvent`,
      `StreamEventKind`, and the three kind constants.
- [ ] §13 primitive-with-consumer: an end-to-end test asserts ordered
      token + step chunks arrive BEFORE the final envelope.
- [ ] The N≥100 concurrent-reuse `-race` test is extended to assert no
      cross-run chunk bleed (each run's sink sees only its own chunks).
- [ ] `WithStream(nil)` is an explicit no-op (no panic; run still
      finishes).

## Files added or changed

- `internal/runtime/assemble/stream.go` (added — `StreamEvent`,
  `StreamEventKind`, the kind constants, `WithStream`, the
  `streamChunkEvent` mapping helper).
- `internal/runtime/assemble/runonce.go` (changed — `stream` field on
  `runOnceConfig`; the `OnChunk` + `OnToolDispatched` sink wiring;
  godoc note).
- `internal/runtime/assemble/runonce_stream_test.go` (added — the
  ordered-chunks-before-envelope e2e test, nil-sink no-op, kind
  mapping).
- `internal/runtime/assemble/runonce_test.go` (changed — concurrent
  test extended with per-run sinks + no-cross-run-bleed assertion).
- `sdk/assemble/assemble.go` (changed — facade re-exports).
- `scripts/smoke/phase-112b.sh` (changed — streaming gate leg).
- `scripts/smoke/phase-132-stream.sh` (added — thin delegating script
  pointing at the 112b coverage).
- `docs/recipes/embed-harbor-headless.md` (changed — streaming
  shorthand in step 4a).
- `docs/decisions.md` (added — D-266).
- `docs/glossary.md` (added — `StreamEvent` / `WithStream`).
- `docs/plans/README.md` (changed — 132-stream index row + detail
  block).

## Public API surface

```go
// internal/runtime/assemble (and re-exported via sdk/assemble)
type StreamEventKind string
const (
    StreamToken          StreamEventKind = "token"
    StreamToolDispatched StreamEventKind = "tool_dispatched"
    StreamStep           StreamEventKind = "step"
)
type StreamEvent struct {
    Kind      StreamEventKind
    Text      string // token delta (StreamToken only)
    Reasoning bool   // reasoning-channel token/step
}
func WithStream(sink func(StreamEvent)) RunOption
```

## Test plan

- **Unit:** `TestRunOnce_WithStream_ChunksArriveBeforeEnvelope` (e2e
  ordered chunks before the envelope, deterministic via the synchronous
  seam); `TestRunOnce_WithStream_NilSink_IsNoOp`;
  `TestRunOnce_StreamEventKinds_Mapping` (the three kinds are distinct —
  pins the tool-dispatched kind, which the mock LLM cannot drive e2e).
- **Integration:** the e2e test wires the real assembled stack (real
  mock LLM driver streaming through `react` → `OnChunk` → sink), under
  the mandatory identity triple; the cancelled-run band in the
  concurrent test is the ≥1 failure mode.
- **Conformance:** N/A — no new driver interface.
- **Concurrency / leak:** `TestRunOnce_ConcurrentReuse_NoBleedNoLeak`
  extended — N=120 concurrent runs, each with its OWN sink, assert each
  sink received only its own run's chunks (no cross-run bleed) plus the
  pre-existing no-context-bleed / no-cross-cancel / restored-goroutine-
  baseline guarantees, all under `-race`.

## Smoke script additions

`scripts/smoke/phase-112b.sh` gains a streaming gate leg: grep-asserts
`TestRunOnce_WithStream_ChunksArriveBeforeEnvelope` exists and the
concurrent test carries the cross-run-chunk-bleed assertion, and the
existing leg-6 `go test -race -run "TestRunOnce|TestNewRunContext"`
already executes both. `scripts/smoke/phase-132-stream.sh` is a thin
delegating script (mirroring `phase-132.sh`) that skips and points at the
112b coverage.

## Coverage target

- `internal/runtime/assemble`: 80% (no regression from the 132 target).

## Dependencies

- 132 (`Stack.RunOnce` + `runctx.NewRunContext` + `RunOption` /
  `runOnceConfig`).

## Risks / open questions

- A sink that blocks stalls its own run goroutine (documented on
  `WithStream`); this is intentional natural backpressure, scoped to the
  one run, never cross-run. No RFC §11 Q-N applies.

## Glossary additions

- `StreamEvent`, `WithStream` — added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes** — the N≥100 `-race` test is extended with the no-cross-run-chunk-bleed assertion.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — the e2e streaming test wires the real assembled stack end-to-end under `-race`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure)
