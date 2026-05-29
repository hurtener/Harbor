# Phase 107 — Streaming completion pipeline (bifrost → events bus → Playground)

## Summary

Phase 106 plumbed the *final* assistant answer to the Playground via `tasks.get` → `result_inline`, but every operator turn arrives as a single wall of text — no progressive rendering. The bifrost driver already has a working streaming path (`internal/llm/drivers/bifrost/bifrost.go::streamComplete`, with `OnContent` and `OnReasoning` callbacks) that the runtime never exercises: the RepairLoop calls `client.Complete(...)` without setting `req.Stream = true`, and no event type carries per-token deltas to subscribers. This phase wires the existing bifrost stream through to a new `llm.completion.chunk` event, threads a per-run `OnChunk` callback through the planner contract, and gives the Playground an SSE subscriber that appends chunk text into the in-flight agent bubble.

Phase 106 explicitly deferred this work ("Per-token streaming via `llm.completion.chunk` events is NOT in scope — see Non-goals" / "wiring streaming through would touch the planner contract + add new event types, which is a Phase 107 concern"). This phase is the cash-out.

## RFC anchor

- RFC §6.5 — LLM client streaming contract (Stream flag + chunk semantics).
- RFC §7 — Console as Protocol client (the Playground consumes Protocol-emitted events).
- RFC §1 — first-five-minutes adoption guarantee (perceived latency dominates first impressions; a 12-second wall of text reads as "broken" even when the run is fine).

## Briefs informing this phase

- brief 08 — bifrost driver coverage (the streaming path on the driver side).
- brief 13 — operator UX (token-by-token visibility is the canonical Playground expectation, set by every other LLM chat surface).
- brief 06 — events / observability / devx (event-bus shape + per-task event emission).

## Brief findings incorporated

- **brief 08 §"Streaming".** "The bifrost SDK's `ChatCompletionStreamRequest` returns a `chan *BifrostStreamChunk` whose elements carry `Content` deltas and a terminal `Done` marker; Harbor's driver wraps this as `OnContent(delta, done)` and `OnReasoning(delta, done)` callbacks on `CompleteRequest`. The wrapper is in place; the runtime never invokes it." Phase 107 invokes it.
- **brief 13 §"Operator UX".** "A Playground that shows no output for >2 seconds reads as broken to most operators, regardless of what the runtime is actually doing." A 5-15 second wait for a wall of text is the most common adoption-blocker after first-attach. Token streaming closes this.
- **brief 06 §"Event-bus shape".** "Every per-task observation MUST be emitted under the canonical `(tenant, user, session, task)` identity tuple so identity-filtered subscribers see only their own events. Cross-task subscribers carry the elevated `console:fleet` scope." The new `llm.completion.chunk` event follows this convention verbatim.

## Findings I'm departing from (if any)

None.

## Goals

- The runtime invokes bifrost's streaming completion path on every Playground turn (and on every other run-loop-driven LLM call, since the seam is shared).
- A new typed event `llm.completion.chunk` carries `{TaskID, RunID, Delta string, Done bool}` payloads, emitted on the canonical event bus under the originating identity triple.
- The Playground subscribes to the chunk event and progressively appends `Delta` into the agent bubble's text; the existing `ChatMessage.streaming?: boolean` flag (already declared on `web/console/src/lib/chat/types.ts`) toggles `true` for the in-flight bubble and `false` on terminal `task.completed`.
- The single-shot answer path (Phase 106's `result_inline` envelope) still works on `task.completed` — the chunk stream populates the bubble incrementally, the final fetch reconciles it. A consumer that misses every chunk still sees the full answer at terminal time (graceful degradation without loss).
- The `ChatPanel` / chat-bubble component renders a streaming-style indicator (existing CSS / icon) while `streaming === true`; flips to the post-stream rendering on completion.
- An end-to-end Playwright test asserts that against a real LLM-backed `harbor dev`, at least one `llm.completion.chunk` event arrives at the browser **before** the `task.completed` event (the load-bearing latency assertion).

## Non-goals

- **Reasoning streaming.** Phase 107 streams `OnContent` only. `OnReasoning` deltas are deferred to Phase 107a — that phase ships the reasoning surface (`TaskDetail.Trajectory` + the Playground accordion); a follow-up enhancement layers reasoning streaming onto the same chunk-event channel with a `kind: "reasoning"` tag once the rendering surface exists.
- **Streaming through the steering surface.** A `user_message` injection mid-run does not stream a new bubble; the operator's injection remains the existing fire-and-forget verb. The runtime's next planner turn will stream its response normally.
- **Cross-tenant chunk aggregation.** The Fleet view (`console:fleet` scope) sees only chunk *counts* per session, never chunk text. Identity-mandatory filtering applies as elsewhere; admin-scope is not a chunk-content escape hatch (CLAUDE.md §6 + §7 audit posture stays settled).
- **Provider-native streaming-multimodal.** Phase 84b owns the multimodal streaming surface (audio chunks, image-token interleaving). Phase 107 is text-only; the bifrost driver already supports text streaming for every V1 provider.
- **Backpressure shaping.** The event bus already has a drop-oldest policy with first-drop emission (CLAUDE.md §5 concurrency rules). Phase 107 inherits it; chunk subscribers under heavy load may see deltas dropped, but the terminal fetch reconciles. No new backpressure knob.

## Acceptance criteria

The bullets below are binding:

- [ ] **AC-1** `internal/planner/RunContext` gains a new optional `OnChunk func(delta string, done bool, kind ChunkKind)` field. `ChunkKind` is a typed enum (sealed: `ChunkContent`, `ChunkReasoning`). Existing planners that don't set it stay backward-compatible (nil callback = no streaming).
- [ ] **AC-2** `internal/planner/repair/repair.go::Run` wires the callback: when `rc.OnChunk != nil`, it sets `req.Stream = true` and supplies `req.OnContent = func(d, done bool) { rc.OnChunk(d, done, ChunkContent) }`. `req.OnReasoning` is wired the same way with `ChunkReasoning` (the *emit* side; Phase 107 still does not render reasoning in the Playground — that's 107a — but the runtime emit is forward-compatible).
- [ ] **AC-3** New event type `llm.completion.chunk` registered in `internal/llm/events.go`. Payload struct `ChunkPayload{ TaskID string; RunID string; Delta string; Done bool; Kind string }` (SafeSealed per D-020 redaction posture; content is operator-visible per-session, never cross-tenant).
- [ ] **AC-4** The runtime emits one chunk event per `OnChunk` callback firing under the originating run's identity quadruple. The `Done=true` chunk fires exactly once per stream (terminator marker); subscribers can use it to detach without waiting on `task.completed`.
- [ ] **AC-5** `cmd/harbor/cmd_dev_runloop.go` constructs each `RunContext` with `OnChunk: emitChunk(bus, identity, taskID, runID)` where `emitChunk` is a closure that publishes the canonical event. Concurrent runs each carry their own closure (D-025 — per-run state on RunContext, not on the engine).
- [ ] **AC-6** No change to `TaskResult.Value` shape (Phase 106's envelope). The single-shot final fetch path stays as-is. The chunk stream is *additive* — a consumer that ignores it sees the existing terminal result.
- [ ] **AC-7** Console — `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` subscribes to `llm.completion.chunk` events on the same EventSource the page already opens for `task.*`. On each event whose `task_id === activeTaskID`, append `delta` to the agent bubble's text. Set `streaming: true` on first chunk; set `streaming: false` and `pending: false` on `Done=true` OR on `task.completed`, whichever fires first.
- [ ] **AC-8** Console — the chat bubble renderer (`web/console/src/lib/chat/`) consumes `streaming: true` and renders an existing streaming indicator (cursor / pulse animation). On `streaming: false` the indicator hides. No new CSS variant required — the existing `streaming?: boolean` flag (declared at `web/console/src/lib/chat/types.ts:84`) is the seam.
- [ ] **AC-9** Console — the chunk parsing logic is extracted to `web/console/src/routes/(console)/playground/[session_id]/chunk-stream.ts` (mirroring the Phase 106 `answer-envelope.ts` pattern) so it's unit-testable without rendering Svelte.
- [ ] **AC-10** Unit (Go) — `internal/planner/repair/repair_test.go` extends with a streaming round-trip test: a mock `LLMClient` whose `Complete` invokes `req.OnContent("Hello, ", false)` then `req.OnContent("world.", true)` should yield three callback fires (two content deltas + one done marker passing through `rc.OnChunk`). Run under `-race`.
- [ ] **AC-11** Unit (Go) — `internal/llm/events_test.go` (or per-package equivalent) pins the `llm.completion.chunk` event-type registration + the payload schema (the SafePayload-of-Kind discipline from D-020).
- [ ] **AC-12** Unit (Svelte) — `web/console/src/routes/(console)/playground/[session_id]/chunk-stream.test.ts` covers: (a) chunk arrives first, then `task.completed` → bubble text is incremental concat then frozen; (b) `task.completed` arrives WITHOUT any chunks (mock or non-streaming provider) → existing Phase 106 fetch path still populates; (c) chunks arrive for a different `task_id` → ignored.
- [ ] **AC-13** Integration (Playwright) — `web/console/tests/streaming.spec.ts` against real `harbor dev` (the YouTube agent or any provider in CI's `.env`): send a prompt that elicits ≥3 tokens, assert the first chunk event reaches the browser within ≤2 seconds of the `start` response, AND at least one chunk event arrives BEFORE the `task.completed` event. SKIP cleanly when no LLM provider key is set (no silent pass — the skip message names the missing env var).
- [ ] **AC-14** Smoke — `scripts/smoke/phase-107.sh` exercises the wire surface (a real `harbor dev` boot + a streaming `start` + an EventSource client reading chunk events). SKIP gracefully when no provider key.
- [ ] **AC-15** Concurrent-reuse — `internal/planner/repair/repair_test.go::TestStreamingConcurrentReuse` runs N=100 concurrent streaming runs against a single shared RepairLoop instance; asserts no chunk-event cross-talk (no chunk emitted under run A's identity reaches run B's subscriber). Per CLAUDE.md §5 + D-025.
- [ ] **AC-16** No regression to non-streaming planners. The Phase 48 deterministic planner (and any future non-React planner) leaves `OnChunk` nil; its runs complete as today with no chunk events emitted.

## Files added or changed

### Runtime (Go)

- `internal/planner/planner.go` (or wherever `RunContext` is declared) — add the `OnChunk` field + the `ChunkKind` enum. ~15 LOC.
- `internal/llm/events.go` — register `llm.completion.chunk` event type + `ChunkPayload` struct. ~25 LOC.
- `internal/llm/events_test.go` — pin the registration + payload shape. ~30 LOC.
- `internal/planner/repair/repair.go` — wire the callback through to `req.Stream / req.OnContent / req.OnReasoning`. ~20 LOC change at the existing `client.Complete(...)` call site.
- `internal/planner/repair/repair_test.go` — streaming round-trip + concurrent-reuse test. ~120 LOC.
- `cmd/harbor/cmd_dev_runloop.go` — construct `RunContext.OnChunk` as `emitChunk(bus, identity, taskID, runID)`. ~30 LOC (closure + emit wiring).
- `cmd/harbor/cmd_dev_runloop_test.go` — assert one `llm.completion.chunk` event lands on the bus per streaming turn under a fake streaming client. ~80 LOC.

### Console (Svelte)

- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — extend the existing `subscribeTaskLifecycle` to also listen for `llm.completion.chunk`; append `delta` to the in-flight bubble. ~30 LOC.
- `web/console/src/routes/(console)/playground/[session_id]/chunk-stream.ts` — **NEW**. Pure functions: `applyChunk(messages, taskID, delta, kind)` + `finalizeStream(messages, taskID)`. ~50 LOC.
- `web/console/src/routes/(console)/playground/[session_id]/chunk-stream.test.ts` — **NEW**. ~80 LOC unit tests per AC-12.

### E2E

- `web/console/tests/streaming.spec.ts` — **NEW**. Real-LLM Playwright spec per AC-13. ~120 LOC.

### Smoke + drift

- `scripts/smoke/phase-107.sh` — **NEW**. PREFLIGHT_REQUIRES: live-server. Curls the `start` + an SSE reader; SKIP when no provider key. ~70 LOC.

### Docs

- `docs/glossary.md` — new entries: `llm.completion.chunk` event; `OnChunk` callback; `ChunkKind`. Alphabetised under L / O / C.
- `docs/skills/drive-the-playground/SKILL.md` — short paragraph noting "responses stream token-by-token; the cursor indicator marks an in-flight stream". (CLAUDE.md §18 — surface change requires same-PR skill update.)

## Public API surface

- `planner.RunContext.OnChunk func(string, bool, ChunkKind)` — new optional field. Nil-callback safe.
- `planner.ChunkKind` enum — sealed two-value (`ChunkContent`, `ChunkReasoning`).
- `llm.ChunkPayload{TaskID, RunID, Delta, Done, Kind string}` — event payload struct.
- `llm.EventTypeCompletionChunk` constant — `"llm.completion.chunk"`.
- Console `ChatMessage.streaming` (already declared) — no new TS surface.

## Test plan

### Unit (Go)

- `internal/planner/repair/repair_test.go::TestStreamingCallbacks_ForwardToRunContext`
- `internal/planner/repair/repair_test.go::TestStreamingConcurrentReuse` (AC-15)
- `internal/llm/events_test.go::TestChunkEventTypeRegistered`
- `internal/llm/events_test.go::TestChunkPayloadSafeSealed`
- `cmd/harbor/cmd_dev_runloop_test.go::TestRunLoop_EmitsChunkEventsPerStream`

### Unit (Svelte)

- `chunk-stream.test.ts::applyChunk_appendsToMatchingBubble`
- `chunk-stream.test.ts::applyChunk_ignoresMismatchedTaskID`
- `chunk-stream.test.ts::finalizeStream_clearsStreamingFlag`
- `chunk-stream.test.ts::applyChunk_handlesReasoningKind_gracefulNoOpForPhase107` (Phase 107a is the renderer; 107 emits but doesn't render reasoning)

### Integration (Playwright)

- `streaming.spec.ts::TestE2E_Playground_StreamsTokens` (AC-13)
- `streaming.spec.ts::TestE2E_Playground_DegradesWhenProviderNonStreaming` — when a mock-LLM driver doesn't stream, the bubble still populates from `task.completed`.

### Concurrency / leak

- `TestStreamingConcurrentReuse` (Go) — listed above.
- `cmd/harbor/cmd_dev_runloop_test.go::TestRunLoop_ChunkEmit_NoGoroutineLeak` — baseline goroutine count restored after N=20 concurrent streaming runs.

### Conformance

- N/A — the chunk event is additive on the existing event bus; no driver-conformance suite extension.

## Smoke script additions

`scripts/smoke/phase-107.sh` — PREFLIGHT_REQUIRES: live-server. Assertions:

1. SKIP when `OPENROUTER_API_KEY` (or whichever provider key the dev config uses) is unset.
2. Bootstrap a dev token via `/v1/dev/bootstrap.json`.
3. Open `/v1/events/subscribe` SSE; filter to the `(dev, dev, dev)` identity scope.
4. POST `/v1/control/start` with a prompt that elicits ≥3 tokens.
5. Assert at least one `llm.completion.chunk` event arrives within 10s.
6. Assert at least one chunk's `Delta` is non-empty.
7. Assert the `task.completed` event arrives AFTER ≥1 chunk event (load-bearing latency assertion).
8. Static: `grep -q 'llm.completion.chunk' internal/llm/events.go` — event type registered.
9. Static: `grep -q 'chunk-stream' web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — page imports the chunk handler.

## Coverage target

- `internal/planner/repair/`: 85% (existing). The repair-loop edit is small; the new tests cover both the streaming and non-streaming branches.
- `internal/llm/`: 80% (existing). The event-type registration is trivially covered.
- `cmd/harbor/`: 80% (existing). The emit closure is covered by the new runloop test.
- `web/console/src/routes/(console)/playground/`: 80% (existing CONVENTIONS.md §10 target).

## Dependencies

- 106 — Playground answer plumbing (the chunk stream layers onto the same bubble Phase 106 created).
- 105 — operator must be able to attach in <5 minutes before streaming visibility matters.
- 84b (V1.3) — bifrost extended multimodal + streaming on the driver side. Phase 107 does NOT block on 84b — it only needs the existing text-streaming path that's already shipped on bifrost. Multimodal-streaming is 84b's job; once both land, the Playground gets streaming text AND streaming image-token interleave for free.
- 83e — react reasoning-channel decoupling. Phase 107 emits `OnReasoning` chunks on the wire but does NOT render them; Phase 107a is the renderer.

## Risks / open questions

- **Stream interrupt semantics.** A `task.cancel` (Phase 54 control verb) mid-stream should cause bifrost to close its chunk channel cleanly. Test pin needed: cancel-during-stream produces no goroutine leak + emits no chunks after the cancel returns. (Listed under Concurrency / leak above.)
- **`task.completed` arriving BEFORE the last chunk.** Possible if the runtime acks completion before the final chunk reaches the bus. Mitigation: the chunk handler is idempotent — it appends to a bubble identified by `taskID`; a late chunk after `task.completed` is just an extra append (the bubble's `streaming: false` was already set; reapply is a no-op). Document the invariant in `chunk-stream.ts` godoc.
- **Event-bus drop policy.** Under heavy load the bus drops oldest chunks (CLAUDE.md §5). The Playground's terminal `task.completed` → `tasks.get` fetch reconciles by overwriting the bubble text with the full envelope `.answer`. This is the graceful-degradation seam — losing a few chunks under load doesn't lose the final answer.
- **SafePayload audit posture.** `ChunkPayload.Delta` is per-session operator-visible content. It MUST NOT be redacted by the default audit redactor (which targets credentials / PII patterns) — these are the LLM's own outputs the operator just typed a prompt for. Confirm `ChunkPayload` is classified `SafePayload` per D-020, not `SafeSealed`.
- **Provider non-streaming fallback.** A future LLM driver that doesn't expose a streaming method should still satisfy the contract by emitting one `Done=true` chunk with the full content. Mock LLM driver test covers this fallback.

## Glossary additions

- **`llm.completion.chunk`** — Phase 107 event type carrying per-token deltas from the LLM provider to subscribers, scoped to the originating run's identity quadruple. Payload: `{TaskID, RunID, Delta string, Done bool, Kind string}`. (Alphabetised under L.)
- **`OnChunk` callback** — `planner.RunContext` field (Phase 107) the engine constructs per-run to translate streaming-LLM callbacks into bus events. (Alphabetised under O.)
- **`ChunkKind`** — sealed enum on `planner` package (Phase 107). Values: `ChunkContent` (the model's natural-language output), `ChunkReasoning` (the model's thinking-trace stream; Phase 107a renders). (Alphabetised under C.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse test — AC-15
- [ ] Integration test — `streaming.spec.ts` against a real LLM-backed `harbor dev`
- [ ] Glossary updated for `llm.completion.chunk`, `OnChunk`, `ChunkKind`
- [ ] `docs/skills/drive-the-playground/SKILL.md` updated per CLAUDE.md §18

## Implementation order (suggested)

1. **Define the event type + payload** (`internal/llm/events.go` + test). Verify with `go test -race ./internal/llm/...`.
2. **Add `RunContext.OnChunk` + `ChunkKind` enum**. Smallest-touch — no consumer yet.
3. **Wire the RepairLoop** to set `req.Stream / req.OnContent / req.OnReasoning` when `rc.OnChunk` is non-nil. Verify with the new `repair_test.go` round-trip test.
4. **Construct the emit closure in `bootDevStack`** + thread through to `RunContext`. Verify with `cmd_dev_runloop_test.go::TestRunLoop_EmitsChunkEventsPerStream`.
5. **Add the Playground chunk-stream module** (`chunk-stream.ts`) + unit tests.
6. **Wire the SSE listener** in `+page.svelte`.
7. **Write the Playwright e2e** + the smoke script.
8. **Run `make drift-audit && make preflight`** — both green.
9. **Open PR.**
