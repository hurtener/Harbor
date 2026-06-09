# Phase 110b — RunContext population + event-closure promotion (`internal/runtime/runctx`)

## Summary

Five RunContext-population helpers — the code that turns subsystem state into what the
planner actually sees — live as unexported `package main` functions, duplicated
cmd↔devstack (the D-094 mirror tax): `projectMemoryBlocks`
(`cmd/harbor/cmd_dev_runloop.go:996-1015`), `projectSkillsContext` (`:1022-1041`),
`extractSkillKeywords` + its stopword set (`:1051-1120` — the D-156 FTS5/BM25 query
shaping, which the audit found mirrored in a THIRD copy), `extractAssistantAnswer`
(`:1130-1145`), and the `resolveInputArtifacts` policy (`:856-911` — D-166). Only
`BuildArtifactManifest` was ever promoted (`internal/planner/artifact_manifest.go`).
Phase 110b promotes the five siblings into a new direction-safe package
`internal/runtime/runctx`, promotes the two per-run event-emission closures
(`cmd_dev_runloop.go:601-660`) into constructors on their owning packages
(`events.IdentityStampingEmitter`, `llm.NewChunkPublisher` — the closure whose
identity-envelope trap once produced 280+ bus-rejected chunks per task), and converts
cmd + devstack to thin callers. Devstack additionally gains the parity it is missing
today: `Emit`/`OnChunk` wired in its RunSpec, and `MarkComplete` carrying the
110a-exported `planner.AnswerEnvelope` instead of an empty `TaskResult{}`
(`devstack.go:1639`). Part of the Wave B re-homing program (D-193); this phase's
decision is **D-195 (reserved; logged when the phase ships)**.

## RFC anchor

- RFC §6.2 — Planner interface, Trajectory, RunContext (the population surface being
  promoted: MemoryBlocks, SkillsContext, InputArtifacts, Emit, OnChunk).
- RFC §6.5 — LLM client layer (the `llm.completion.chunk` streaming event the chunk
  publisher emits; the multimodal input views `resolveInputArtifacts` builds).
- RFC §6.13 — typed event bus (identity-mandatory envelope validation — the rule the
  promoted emitter constructors encode so the 280-rejected-chunks trap cannot recur).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (RunContext is the planner's ONLY window; what
  populates it is runtime contract, not CLI plumbing).
- brief 04 — memory + skills (the llm_context shaping + FTS5 retrieval the helpers
  implement).
- brief 06 — events, observability, devx (one bus, loud publish failures — the emitter
  closures' rules).

## Brief findings incorporated

- **brief 02 §3 "Planner → runtime (via `RunContext`)".** "The planner never imports
  runtime internals. It calls only [Catalog / Memory / Skills / Artifacts / Emit]."
  Everything the planner can see arrives through RunContext — so the projection code
  that POPULATES RunContext is the runtime's half of the planner contract. Today that
  half is unexported in `package main`; a headless consumer building a RunSpec must
  reverse-engineer it. `internal/runtime/runctx` is the promotion that makes the
  contract real for every caller.
- **brief 04 §5 "`llm_context` vs `tool_context` separation is the load-bearing
  decision".** Identifiers live LLM-invisible; conversation memory is the LLM-visible
  projection. `ProjectMemoryBlocks` is exactly that projection boundary — promoting it
  keeps the shape in ONE place instead of three drifting copies.
- **brief 04 §5 "FTS5 is conditionally available" (+ D-156).** The keyword shaping that
  makes the SQLite FTS5/BM25 ranker perform (`extractSkillKeywords`) is retrieval-
  quality-critical and currently triplicated; the audit's verifier found the third
  copy. One exported function, one stopword set, one test suite.
- **brief 06 §5 "Bus publishing failures must be surfaced, not logged silently" + the
  one-bus lesson.** The `Emit`/`OnChunk` closures encode two hard-won rules: identity
  MUST land on the `events.Event` envelope (not just the payload — the in-code comment
  records 280+ rejected chunks per task when this was wrong), and publish failures Warn
  loudly. Promoting them to constructors makes the rules un-forgettable at every call
  site instead of re-discoverable per assembly.

## Findings I'm departing from (if any)

None.

## Goals

- **`internal/runtime/runctx` package** (direction-safe: `runtime/*` may import
  `planner` / `memory` / `skills` / `artifacts`; `planner` gains NO new imports — in
  particular no `memory` import; verified by the package's import list in review):
  - `runctx.ProjectMemoryBlocks(patch memory.LLMContextPatch) *planner.MemoryBlocks`
  - `runctx.ProjectSkillsContext(ranked []skills.RankedSkill) []any`
  - `runctx.ExtractSkillKeywords(query string) string` (+ the unexported stopword set
    and `maxSkillKeywords` cap moving with it; godoc keeps the D-156 pipeline contract
    incl. the all-stopwords→caller-falls-back-to-raw-query rule)
  - `runctx.ExtractAssistantAnswer(fin planner.Finish) string`
  - `runctx.ResolveInputArtifacts(ctx context.Context, store artifacts.ArtifactStore, q identity.Quadruple, ids []string, logger *slog.Logger) []planner.InputArtifactView`
    — the D-166 policy (identity-scoped `GetRef`, image-MIME byte inlining, ref-only
    fallback with loud Warn) as a function over its dependencies rather than a driver
    method. The 84b disposition-policy plan already names this call site as its thin
    caller; promotion here is what 84b's "thin call from cmd AND devstack" lands on.
- **Event-closure constructors on the owning packages** (~20-line pure constructors,
  per the audit's P4):
  - `events.IdentityStampingEmitter(bus EventBus, q identity.Quadruple, logger *slog.Logger) func(Event)`
    — stamps the run's quadruple on any event missing identity, publishes under the
    caller-supplied bus context semantics, Warns loudly on publish failure. Return type
    matches `planner.RunContext.Emit` (`func(events.Event)`) without `events` importing
    `planner`.
  - `llm.NewChunkPublisher(bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) func(delta string, done bool, kind string)`
    — builds the `llm.CompletionChunkPayload` + publishes `EventTypeCompletionChunk`
    with identity on the **envelope** (the trap, encoded). Lives in `internal/llm` next
    to the payload type. Because `planner` imports `llm` (so `llm` cannot name
    `planner.ChunkKind`), the kind parameter is `string`; the run loop adapts with a
    one-line wrapper (`func(d string, done bool, k planner.ChunkKind) { pub(d, done, string(k)) }`).
- **cmd + devstack become thin callers** (§13 consumer, same phase): the five helpers +
  two closures are deleted from `cmd_dev_runloop.go`; devstack's duplicate copies
  (`devStackExtractAssistantAnswer`, its memory/skills projections, the third
  `extractSkillKeywords` copy) are deleted.
- **Devstack parity closure** (§17.6 fix-both-sides — the mirror gains what it was
  missing, not just what it duplicated):
  - its RunSpec wires `Emit` (via `events.IdentityStampingEmitter`) and `OnChunk` (via
    `llm.NewChunkPublisher`) — today it wires neither, so planner telemetry and
    streaming are silently dead on the official test surface;
  - its `MarkComplete` carries the marshalled `planner.AnswerEnvelope` (110a) instead
    of `tasks.TaskResult{}` (`devstack.go:1639`) — closing the audit's "empty result"
    drift.

## Non-goals

- **No run-loop-driver promotion.** The `perTaskRunLoopDriver` subscribe/spawn/drain
  shape stays in cmd (its fan-out home is Phase 110d's assembly story; the steering
  RunLoop itself shipped long ago).
- **No retrieval-behaviour change.** Keyword shaping, stopwords, caps, memory/skills
  projection shapes, and input-artifact policy are promoted verbatim — golden tests pin
  parity. (84b's disposition hook lands on the promoted function later, not here.)
- No new event types, no chunk-payload shape change, no Protocol surface change.
- No summariser/compression work (that is the audit's Wave C, out of scope).

## Acceptance criteria

- [ ] `internal/runtime/runctx` exports the five helpers with behaviour-parity golden
      tests (memory projection shape, skills projection shape, keyword pipeline
      including stopword/dedupe/cap rules, assistant-answer extraction fallbacks,
      input-artifact resolution incl. image-bytes inlining + missing-artifact +
      store-error Warn paths).
- [ ] `internal/planner` gains no new imports (asserted in review; the package import
      list stays `memory`-free) — the direction rule the plan exists to respect.
- [ ] `events.IdentityStampingEmitter` exported; unit tests cover identity stamping
      (empty→stamped, pre-set→preserved), publish-failure Warn, and that the returned
      closure satisfies `planner.RunContext.Emit` (compile-checked from a package that
      may import both).
- [ ] `llm.NewChunkPublisher` exported; unit test asserts identity lands on the
      **Event envelope** (a bus double that rejects identity-less events — the
      280-rejected-chunks regression gate) and the payload carries task/run IDs.
- [ ] **§13 consumer in the same phase:** `cmd_dev_runloop.go` deletes its local copies
      and calls the promoted surface; devstack deletes ALL duplicate copies (including
      the third `extractSkillKeywords`) and calls the same surface — grep-asserted in
      the smoke.
- [ ] **Devstack parity:** devstack's RunSpec wires `Emit` + `OnChunk`; a devstack-run
      task produces `planner.decision` events and `llm.completion.chunk` events on its
      bus (integration-asserted); `MarkComplete` carries the answer envelope and a
      devstack `tasks.get`-shaped read sees a non-empty `result` (the audit's
      empty-`TaskResult{}` drift is closed).
- [ ] All prior phase smokes + integration tests pass against the converted binary.

## Files added or changed

- `internal/runtime/runctx/runctx.go` (+ `runctx_test.go`) — the five helpers + package
  doc (new package under the §3 `internal/runtime/` ellipsis).
- `internal/events/emitter.go` (+ `_test.go`) — `IdentityStampingEmitter`.
- `internal/llm/chunk_publisher.go` (+ `_test.go`) — `NewChunkPublisher`.
- `cmd/harbor/cmd_dev_runloop.go` — local helpers + closures deleted; thin calls.
- `harbortest/devstack/devstack.go` — duplicates deleted; Emit/OnChunk wired;
  `MarkComplete` envelope parity.
- `test/integration/` — devstack-parity integration test (events + chunks + envelope on
  the devstack path).
- `scripts/smoke/phase-110b.sh` — assertions below.
- `docs/decisions.md` — D-195 (authored at ship time).

## Public API surface

- `runctx.ProjectMemoryBlocks(memory.LLMContextPatch) *planner.MemoryBlocks`
- `runctx.ProjectSkillsContext([]skills.RankedSkill) []any`
- `runctx.ExtractSkillKeywords(string) string`
- `runctx.ExtractAssistantAnswer(planner.Finish) string`
- `runctx.ResolveInputArtifacts(ctx, artifacts.ArtifactStore, identity.Quadruple, []string, *slog.Logger) []planner.InputArtifactView`
- `events.IdentityStampingEmitter(EventBus, identity.Quadruple, *slog.Logger) func(Event)`
- `llm.NewChunkPublisher(events.EventBus, identity.Quadruple, string, *slog.Logger) func(delta string, done bool, kind string)`

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers (cmd, harbortest,
> examples); external-team embedding needs a future facade/export RFC (the audit's
> Wave D), out of scope for this band.

### SDK-consumer reachability

A headless consumer that builds its own `RunContext` today gets a planner with empty
memory blocks, no skills context, raw-sentence FTS5 queries, no input-artifact views,
and — if it wires the bus naively — an event stream that silently rejects every chunk
for missing envelope identity. Each of those is solved code, locked in `package main`.
After 110b the consumer composes the same five projections + two emitter constructors
production uses; the planner/RunContext seam moves from "partial" toward "yes" on the
audit's reachability scorecard, and the devstack kit stops validating weaker semantics
than production ships.

## Test plan

- **Unit:** golden parity tests for all five helpers (table-driven; the keyword pipeline
  covers stopword drop, 1-char drop, dedupe-preserving-order, the 10-term cap, and the
  all-stopwords-empty result); emitter identity-stamping + publish-failure Warn; chunk
  publisher envelope-identity regression gate.
- **Integration:** devstack-parity test — assemble a devstack, run one task end-to-end
  with real `inmem` drivers, assert (a) `planner.decision` + `llm.completion.chunk`
  events arrive on the bus under the run's quadruple, (b) the completed task's result
  parses as `planner.AnswerEnvelope` with a non-empty answer, (c) one failure mode: a
  closed bus mid-run produces loud Warns, not silent drops. Identity propagation
  asserted across the seam; `-race`.
- **Conformance:** N/A — pure projection helpers + closures, no driver registry.
- **Concurrency / leak:** the emitter + chunk publisher are per-run closures (D-025
  pattern); a stress test runs N≥100 concurrent runs sharing one bus, asserting no
  cross-run identity bleed in delivered events and goroutine baseline restored. The
  five projections are pure functions — covered by `-race` on the parallel tests; no
  compiled artifact is built (concurrent-reuse checklist item N/A with that reason).

## Smoke script additions

`scripts/smoke/phase-110b.sh` (static-only): assert `internal/runtime/runctx/runctx.go`
exists; grep-assert `cmd_dev_runloop.go` no longer defines `projectMemoryBlocks` /
`projectSkillsContext` / `extractSkillKeywords` / `extractAssistantAnswer` and devstack
no longer defines its duplicates; grep-assert devstack's RunSpec wires `OnChunk`; run
`go test ./internal/runtime/runctx/ ./internal/events/ ./internal/llm/ -run 'Runctx|IdentityStamping|ChunkPublisher' -race -count=1`.
Skeleton ships with this plan (standard skip until the phase implements).

## Coverage target

- `internal/runtime/runctx`: 90% (pure projections).
- `internal/events` / `internal/llm`: the new files ≥ 90%; packages stay ≥ existing
  targets.

## Dependencies

- **110a** (the exported `planner.AnswerEnvelope` devstack's `MarkComplete` parity
  consumes; Stage-1 merge precedes this phase).
- 83f / 83i (D-149 / D-152 — the RunContext population being promoted), 83m item 4
  (D-156 keyword shaping), F11 / D-166 (input-artifact resolution), 107 (chunk
  pipeline), 106 (answer envelope semantics).

## Risks / open questions

- **Merge coordination (staging).** 110b runs in **Stage 2, in parallel with 110d**,
  after Stage 1 (110a ∥ 110c) merges. Both Stage-2 phases touch `cmd_dev_runloop.go` /
  `cmd_dev.go` and `devstack.go`; 110d's assembly promotion consumes this phase's
  constructors at its call sites — the coordinator sequences the two PRs' merges and
  resolves mechanical overlaps.
- **84b coordination.** The pending 84b plan names `resolveInputArtifacts` as its thin
  call site for disposition resolution. Whichever lands second rebases: if 110b lands
  first (expected), 84b's hook goes into `runctx.ResolveInputArtifacts`'s caller path
  instead of a cmd-local function — a strictly simpler landing.
- **Chunk-kind type at the llm boundary.** The `string`-kinded publisher parameter is
  forced by import direction (`planner`→`llm`); the one-line caller adapter is the
  cost. If a future phase moves `ChunkKind` below `llm`, the signature can tighten —
  not this phase.
- **Behaviour-parity risk.** Retrieval quality (keyword shaping) and prompt shape
  (memory/skills projections) are operator-visible; the golden tests are the guard.

## Glossary additions

- N/A — no new vocabulary; existing terms (RunContext, answer envelope, FTS5 keyword
  shaping per D-156) re-home without renaming.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the
      emitter/publisher stress test asserts no cross-run identity bleed.
- [ ] Concurrent-reuse test: N/A as a new compiled artifact (the promoted surface is
      pure functions + per-run closures); the N≥100 shared-bus stress test under
      `-race` covers the closure pattern per D-025.
- [ ] **Integration test (§17):** devstack-parity test with real drivers, identity
      propagation, ≥1 failure mode, under `-race`.
- [ ] If new vocabulary: N/A.
- [ ] If a brief finding was departed from: N/A — none departed.
