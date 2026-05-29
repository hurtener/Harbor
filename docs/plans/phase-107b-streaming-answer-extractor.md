# Phase 107b — Streaming answer extractor (React planner)

## Summary

Phase 107 wired bifrost streaming through to the Console, but the React planner emits its decisions as prompt-engineered `{tool, args}` JSON inside the LLM's `Content` field (Phase 83a–e narrowed shape). Every `OnContent` delta therefore carries the FULL JSON wrapper — the chat bubble streams `{"tool":"_finish","args":{"answer":"` then the actual answer then `"}}` as raw bytes, then `task.completed` overwrites with the parsed answer. Operators briefly see garbled JSON streaming, then a flash to the clean answer — strictly worse than the V1.2 wall-of-text problem streaming was meant to fix.

Phase 107b adds a per-step JSON-string-buffer state machine inside the React planner package that gates which bytes reach `rc.OnChunk`. The filter detects the `_finish` discriminator + the opening of the `args.answer` string value, then forwards only the decoded answer bytes (with JSON escape handling). Non-finish actions (tool calls, multi-action salvage) emit zero chunks — the Console renders tool-call cards from the trajectory at `task.completed` as today. RepairLoop, the LLM client, and the run-loop driver are unchanged; the fix is wholly inside `internal/planner/react/`.

## RFC anchor

- RFC §6.2 — Planner interface + reasoning policy (the React planner owns its action schema; the filter is schema-aware code).
- RFC §6.5 — LLM client streaming contract (the filter sits ABOVE the LLM client; the client continues to forward raw deltas).
- RFC §7 — Console as Protocol client (the chat bubble subscribes to `llm.completion.chunk` and renders deltas; the filter ensures those deltas are only user-facing prose).
- RFC §1 — first-five-minutes adoption guarantee (perceived progressive streaming is the legibility signal; raw-JSON streaming reads as "broken" even when the run is fine).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the React planner's decision-emission contract).
- brief 13 — react planner prompt engineering (Phase 83a–e narrowed the action shape to `{tool, args}` — the filter consumes that exact shape).
- brief 03 — tools + integrations + LLM client (the bifrost streaming surface the filter sits above).

## Brief findings incorporated

- **brief 13 §"Narrow action schema".** "The planner emits one action per turn shaped `{tool: string, args: object}`. The reserved tool name `_finish` discriminates finish actions; `args.answer` is the user-facing payload." The filter's state machine encodes this shape verbatim — `"tool"\s*:\s*"_finish"` is the discriminator, `args.answer`'s string value is the emission window.
- **brief 02 §"Decision sum is the contract"**. "The planner→runtime contract is the sealed Decision sum; everything else is internal to the planner concrete." The filter is wholly internal to the React planner; the Decision sum is unchanged.
- **brief 03 §"Streaming is per-token from the driver"**. "The bifrost driver fires `OnContent(delta, done)` callbacks during `Complete` when `req.Stream = true`; the runtime is responsible for routing these deltas." The filter is a routing decision — it intercepts deltas at the planner step before they reach the bus emit closure.

## Findings I'm departing from (if any)

None.

## Goals

- A per-step `streamAnswerFilter` in `internal/planner/react/` that takes raw `OnContent` deltas and emits only the decoded bytes of `args.answer` for `_finish` actions. Tool-call actions and salvage-multi-action emissions yield zero chunks.
- The React planner's `Next()` constructs one filter per step on the stack, wraps `rc.OnChunk` into a filtered closure, hands the wrapped `RunContext` down to `repair.RepairLoop.Run(...)`. RepairLoop is unmodified; it continues to forward `req.OnContent → rc.OnChunk` as today.
- `kind: ChunkReasoning` deltas pass through unfiltered — Phase 107a renders reasoning post-completion from the trajectory; live reasoning streaming is deferred to a later phase (107c, V1.4).
- A `parallel` salvage emission whose first action is `_finish` streams that one answer; subsequent actions are ignored by the filter (their semantics are owned by `parser` + `task.completed` reconciliation, not the stream).
- Console UX: the chat bubble streams clean prose only — no JSON wrapper, no tool args, no thought text. When `task.completed` fires, Phase 106's `tasks.get` fetch overwrites the bubble with the canonical envelope `.answer` for byte-exact reconciliation.

## Non-goals

- **Live reasoning streaming.** Phase 107c (V1.4) ships a sibling `streamThoughtFilter` (or extends this one) for the `reasoning_replay` channel. Phase 107b emits only `ChunkContent`.
- **Native tool-calling migration.** Research brief 15 covers Path B — eliminating the JSON-extractor entirely by switching the React planner to the provider's structured tool-call API. Phase 107b ships the V1.3 fix that lives inside the prompt-engineered shape; the V1.4 migration is a separate work stream.
- **A planner-agnostic streaming filter primitive.** The filter is React-specific — it knows the `{tool, args, _finish, answer}` vocabulary. A future Workflow / Plan-Execute planner that wants streaming brings its own filter. The shared seam (`RunContext.OnChunk`) is what stays generic.
- **Streaming for non-React planners.** The Deterministic planner (Phase 48) produces its answer as plain output; it can wire `rc.OnChunk` directly to the bus without a filter. No change here.
- **Schema discovery via the bus.** The filter is silent; it does not emit a `planner.stream_phase` event when transitioning state. The chunk stream's `kind: ChunkContent` already conveys "this is answer content."
- **Buffer-size limits.** The filter buffers raw bytes until the discriminator is detected (typically ~30 tokens in for the smallest `_finish` shape). A pathological model that never emits the discriminator buffers the entire response without emitting; that's bounded by the LLM's token budget and is not a memory concern. No explicit cap.

## Acceptance criteria

The bullets below are binding.

- [ ] **AC-1** New type `streamAnswerFilter` in `internal/planner/react/stream_filter.go`. Per-instance state (rolling raw buffer + decoded-output accumulator + state-machine stage enum). Methods: `(*streamAnswerFilter) Feed(delta string) string` returns the decoded bytes (zero or more) that should reach `rc.OnChunk` for THIS delta; `(*streamAnswerFilter) Done() bool` reports whether the closing `"` of `args.answer` has been observed. Construction: `newStreamAnswerFilter() *streamAnswerFilter` — zero-config.
- [ ] **AC-2** State machine (load-bearing — pin each stage in tests):
  1. **Stage A — discriminator.** Look for `"tool"\s*:\s*"_finish"` AS A REGEX MATCH against the buffer. If not found, swallow the delta (return "").
  2. **Stage B — args key.** After Stage A matches, look for `"args"\s*:`. Order-insensitive: `"args"` can appear before `"tool"` in the JSON.
  3. **Stage C — opening brace.** Skip whitespace + look for `{` of args object.
  4. **Stage D — answer key.** Look for `"answer"\s*:` inside the args object.
  5. **Stage E — opening quote.** Skip whitespace + look for the opening `"` of the answer string value.
  6. **Stage F — emit.** Forward subsequent bytes (decoded per JSON escape rules — `\n` / `\t` / `\r` / `\"` / `\\` / `\uXXXX`) until the closing unescaped `"`.
  7. **Stage G — done.** On the closing `"`, mark `Done() = true`. Subsequent Feed calls return "".
- [ ] **AC-3** Before Stage A matches, `Feed` returns "" for every delta — silent suppression. A response that NEVER matches Stage A (a tool-call action, a malformed response, a salvage multi-action whose first block isn't `_finish`) emits zero chunks for the entire stream. This is intentional — the chat bubble stays empty until `task.completed` populates it via Phase 106's fetch.
- [ ] **AC-4** Boundary-spanning chunks are handled correctly. A delta split that lands inside the discriminator pattern (`"_fin|ish"`), inside the `args` key, inside the opening `"`, inside an escape sequence (`\` + `n`), or inside the closing `"` MUST produce the same total emission as the same payload fed in one Feed call. Tested explicitly by feeding the canonical fixture byte-by-byte.
- [ ] **AC-5** React planner's per-step LLM-call path (in `internal/planner/react/react.go` or its existing `Next()` site) constructs `filter := newStreamAnswerFilter()` on the stack per call. Wraps `rc.OnChunk` into a filtered closure: when invoked with `kind == ChunkContent`, it calls `filter.Feed(delta)` and forwards ONLY non-empty results to the original `rc.OnChunk` with `kind: ChunkContent`. The wrapped closure also forwards `done=true` from upstream when `filter.Done()` becomes true OR when the upstream `done` argument is true (whichever fires first).
- [ ] **AC-6** `kind == ChunkReasoning` deltas pass through the wrapped closure UNFILTERED — the filter applies only to content. Reasoning has no JSON wrapper (the provider surfaces it as a separate field); forwarding raw is correct.
- [ ] **AC-7** The wrapped RunContext is what the React planner passes to `repair.RepairLoop.Run(ctx, rcWrapped, ...)`. RepairLoop is unchanged. The repair loop continues to wire `req.OnContent → rcWrapped.OnChunk`; the schema-aware filtering is invisible to it.
- [ ] **AC-8** D-025 compliance: the filter is stack-local per `Next()` call. NEVER a field on the `ReactPlanner` struct. The planner artifact stays immutable; N concurrent runs each instantiate an independent filter (verified by AC-9 stress test).
- [ ] **AC-9** Unit (Go) — `internal/planner/react/stream_filter_test.go`:
  - `TestFilter_NonFinishAction_EmitsNothing` — `{"tool":"search","args":{"q":"x"}}` fed in one chunk → cumulative emission is "".
  - `TestFilter_FinishAction_EmitsAnswerBytes` — `{"tool":"_finish","args":{"answer":"hello world"}}` → "hello world".
  - `TestFilter_AnswerWithEscapes` — answer `"line1\nline2\ttab\"quote\\back"` → decoded equivalent (`"line1\nline2\ttab\"quote\\back"` as Go string literal: newline + tab + quote + backslash).
  - `TestFilter_AnswerWithUnicodeEscape` — `A` decodes to `A`; `é` decodes to `é`.
  - `TestFilter_SplitAcrossBoundaries` — feed the canonical fixture one byte at a time; assert cumulative emission == single-feed result.
  - `TestFilter_KeyOrderInsensitive` — `{"args":{"answer":"x"},"tool":"_finish"}` — args appears before tool; filter detects via rolling buffer.
  - `TestFilter_NestedAnswerObject` — `{"tool":"_finish","args":{"answer":{"k":"v"}}}` — answer is an object, not a string; filter emits nothing (Stage E never sees the opening `"`).
  - `TestFilter_MalformedJSON_GracefulDegradation` — `{"tool":"_finish","args":{"answer":"hel` then EOF → cumulative emission == "hel", `Done() == false`. The bubble's Phase 106 fetch reconciles post-hoc.
  - `TestFilter_AlternativeAnswerKey` — accept `"answer"` only; an `args.text` or `args.response` does NOT emit (the schema discriminator is `args.answer` by Phase 83 contract).
- [ ] **AC-10** Concurrent-reuse (Go) — `internal/planner/react/stream_filter_concurrent_test.go::TestReactPlanner_StreamFilter_NoCrossTalk`. N=128 concurrent `Next()` calls against a single shared `*ReactPlanner` instance. Each call uses a recording `rc.OnChunk` that captures emissions under the caller's tenant id. Assert: each recorder receives the correct answer bytes for its scripted finish action, no cross-tenant contamination, no data race under `-race`, no goroutine leak (`runtime.NumGoroutine` baseline restored after all calls return).
- [ ] **AC-11** Wire change isolation: `git diff --stat` after Phase 107b shows changes ONLY in `internal/planner/react/`. No diff in `internal/planner/repair/`, `internal/llm/`, `cmd/harbor/cmd_dev_runloop.go`, `web/console/`, or any Protocol type.
- [ ] **AC-12** Smoke — `scripts/smoke/phase-107b.sh` PREFLIGHT_REQUIRES: live-server. Static asserts: `stream_filter.go` exists; the file contains the four discriminator regex literals (`"tool"`, `"_finish"`, `"args"`, `"answer"`). Live assert (gated on `OPENROUTER_API_KEY` or equivalent): start a task with a finish-shaped prompt ("Reply with the single word: ok"), subscribe to `llm.completion.chunk`, concatenate the deltas, and assert the concatenation equals the eventual `tasks.get` `result_inline.answer` parse (no leading `{` JSON bytes). SKIP gracefully when no provider key.
- [ ] **AC-13** Console regression: existing Playwright `reasoning-accordion.spec.ts` continues to pass — the filter change is invisible from the page's point of view (the page subscribes to the same `llm.completion.chunk` event; only the byte content changes). No new Playwright spec needed (the smoke + Go tests carry the coverage).
- [ ] **AC-14** `docs/decisions.md` entry `D-NNN` (pre-assigned at dispatch time) — classifies the filter's coupling to the Phase 83 action schema as load-bearing: a future schema reshape MUST update the filter's regex patterns in the same PR. The decision body lists the four discriminator strings the filter depends on.
- [ ] **AC-15** Glossary entry: `streamAnswerFilter` — per-step React planner JSON-string-buffer extractor that gates which bytes reach `rc.OnChunk`. Filter activates on `"_finish"` discriminator + emits decoded `args.answer` content with JSON escape handling.

## Files added or changed

### Runtime (Go)

- `internal/planner/react/stream_filter.go` — **NEW**. The state-machine extractor + Feed/Done methods. ~150 LOC.
- `internal/planner/react/stream_filter_test.go` — **NEW**. AC-9 unit tests. ~200 LOC.
- `internal/planner/react/stream_filter_concurrent_test.go` — **NEW**. AC-10 concurrent-reuse stress. ~80 LOC.
- `internal/planner/react/react.go` (or the file owning React's `Next()` / per-step LLM-call site) — wrap `rc.OnChunk` with the filter before delegating to RepairLoop. ~15 LOC change at the existing call site.

### Smoke + drift

- `scripts/smoke/phase-107b.sh` — **NEW** per AC-12. ~70 LOC.

### Docs

- `docs/glossary.md` — `streamAnswerFilter` entry (AC-15).
- `docs/decisions.md` — D-NNN entry per AC-14.
- `docs/skills/drive-the-playground/SKILL.md` — short paragraph noting "the chat bubble now streams ONLY the model's user-facing prose; raw action JSON never reaches the panel". (CLAUDE.md §18 same-PR skill update — the playground surface visibly improved.)

## Public API surface

- **No new exported package types.** `streamAnswerFilter` is unexported (internal to `internal/planner/react`).
- **No change to `planner.RunContext`** — the wrap happens INSIDE the React planner's step, invisible to the runtime contract.
- **No change to `llm.CompleteRequest`** or `llm.CompletionChunkPayload` — the wire event carries the same shape; only the byte content differs.

## Test plan

### Unit (Go)

- Every AC-9 case in `stream_filter_test.go`.

### Integration (Go)

- `internal/planner/react/integration_test.go::TestReactPlanner_StreamFilter_EndToEnd_FinishAction` — drive the React planner's `Next()` against a streaming-stub LLM client that fires `OnContent` calls with a chunked `_finish` payload. Assert `rc.OnChunk` receives the decoded answer bytes only.
- `internal/planner/react/integration_test.go::TestReactPlanner_StreamFilter_EndToEnd_ToolCallAction` — same harness, but the scripted response is a tool-call action (`{"tool":"search","args":{"q":"x"}}`). Assert `rc.OnChunk` receives zero deltas.

### Conformance

- N/A — the filter is React-specific; the planner conformance pack (Phase 49) doesn't extend.

### Concurrency / leak

- `TestReactPlanner_StreamFilter_NoCrossTalk` (Go) — AC-10.

## Smoke script additions

`scripts/smoke/phase-107b.sh` — see AC-12. Assertions:

1. `internal/planner/react/stream_filter.go` exists.
2. The file references the four discriminator literals (`"tool"`, `"_finish"`, `"args"`, `"answer"`).
3. SKIP when no LLM provider key.
4. Bootstrap a dev token; subscribe to `/v1/events/subscribe` filtered for `llm.completion.chunk`.
5. POST `/v1/control/start` with `query: "Reply with the single word: ok"`.
6. Concatenate the `delta` fields from received chunk events.
7. Wait for `task.completed`; fetch `/v1/tasks/get`; parse `result_inline.answer`.
8. Assert: concatenated chunks (Stage F output) == parsed `result_inline.answer` modulo trailing whitespace. The concatenation MUST NOT contain a leading `{` (i.e., no JSON wrapper bytes leaked).
9. Assert: NO chunk event was emitted before the first chunk whose decoded content is non-empty answer text (i.e., no JSON-wrapper bytes streamed early).

## Coverage target

- `internal/planner/react/`: 85% (existing target). The new `stream_filter.go` lifts the package's coverage; the React planner's `Next()` change is small.

## Dependencies

- 107 — chunk-event pipeline (the filter sits between the React planner and `rc.OnChunk`; the runloop's emit closure is unchanged).
- 83a–e — narrowed action schema. The filter consumes `{tool, args, _finish, answer}` literally.

## Risks / open questions

- **Schema coupling.** The filter encodes the Phase 83 action shape in regex literals. A future schema change (e.g., renaming `_finish` to `_final` or `args.answer` to `args.response`) must update the filter in lockstep. D-NNN per AC-14 makes this coupling explicit; the `_finish` literal becomes a load-bearing string the action-schema test suite asserts on.
- **Native tool-calling migration.** Research brief 15 evaluates eliminating the filter entirely by switching React to the provider's structured tool-call API. Path B would delete `stream_filter.go` in one PR — Phase 107b doesn't lock in tech debt because the extractor is wholly inside the React package.
- **Multi-action salvage with `_finish` first.** A `parallel` plan whose first action is `_finish` and subsequent actions exist is ill-formed (finish is terminal). The filter handles the first `_finish` correctly; the salvage path's post-stream validation will reject any trailing actions. Documented in AC-2 + the filter's godoc.
- **Pathological "never matches" buffer growth.** A model that never emits `"_finish"` accumulates the full response in the rolling buffer. Bounded by the LLM's token budget (per RFC §6.5 context-window safety net) — no explicit cap needed.
- **Live reasoning streaming (Phase 107c).** When reasoning streaming lands, the wrap point is the same (React's per-step `Next()`); a `streamThoughtFilter` runs in parallel and forwards `kind: ChunkReasoning` deltas (extracted from `args.thought` if the prompt requires it, or directly from `OnReasoning` callbacks for native-reasoning providers). The shared seam stays clean.

## Glossary additions

- **`streamAnswerFilter`** — per-step React planner JSON-string-buffer extractor that gates which bytes reach `rc.OnChunk` (Phase 107b). Activates on the `"_finish"` discriminator + the `args.answer` string-value opening, emits decoded answer bytes (with JSON escape handling) up to the closing `"`, then marks Done. Non-finish actions yield zero emissions. (Alphabetised under S.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: N/A — the filter doesn't widen identity scope (chunk emit still routes through the existing identity-aware `rc.OnChunk` closure)
- [ ] Concurrent-reuse — AC-10 passes
- [ ] Integration test — `TestReactPlanner_StreamFilter_EndToEnd_*` (in-package) per AC + the live-server smoke
- [ ] Glossary updated for `streamAnswerFilter`
- [ ] `docs/skills/drive-the-playground/SKILL.md` updated per CLAUDE.md §18
- [ ] `docs/decisions.md` D-NNN entry filed per AC-14

## Implementation order (suggested)

1. **Filter state machine** (`stream_filter.go` + unit tests, AC-9). Verify byte-boundary correctness FIRST — every other AC depends on it.
2. **Concurrent-reuse test** (AC-10) — pin the no-mutable-state property before wiring.
3. **React planner wrap site** — locate the per-step LLM-call site in `react.go` (or wherever Phase 83's prompt + repair invocation lives), wrap `rc.OnChunk`, hand the wrapped `RunContext` to `RepairLoop.Run(...)`.
4. **In-package integration tests** (AC-9 end-to-end + AC-10 concurrent reuse). Both `go test -race` green.
5. **Live smoke** against `harbor dev` + YouTube agent or any provider in CI's `.env` — confirm the chat bubble streams clean prose only.
6. **Skill drift** (`drive-the-playground/SKILL.md`).
7. **D-NNN decisions entry** (the schema-coupling classification).
8. **Glossary entry** (one line).
9. `make drift-audit && make preflight` — both green.
10. Open PR.
