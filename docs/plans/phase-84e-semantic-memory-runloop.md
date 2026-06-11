# Phase 84e — Semantic memory consumption in the run loop

## Summary

Phase 84d (D-191) shipped semantic memory retrieval as a store/SDK surface:
`MemoryStore.SearchTurns` ranks identity-scoped embedded turns by cosine
similarity. The gap: **nothing in the run loop calls it** — the agent itself
never semantically recalls earlier conversation turns; the planner prompt's
memory injection (the Phase 83d path) carries the rolling-summary patch only.
Phase 84e closes that: when the opt-in semantic memory mode is on
(`memory.retrieval: semantic`), the run loop searches the session's embedded
turns with the current query (the store embeds the query internally — the run
loop never touches the `Embedder`) and injects the top-k recalled turns into
the planner prompt's **`<read_only_external_memory>`** tier — the
`planner.MemoryBlocks.External` slot that has been nil on every production
path since 83d, whose documented purpose (brief 13 §2.3) is exactly "read-only
external memory **retrieved before this run**." Recall COMPOSES with
`rolling_summary` (the Conversation tier is untouched), and with the mode off
the prompt is byte-for-byte unchanged — zero behavioral change, zero embedder
traffic. The fetch+recall logic lands in ONE shared home
(`internal/runtime/runctx`), consumed by both the production run-loop driver
and the devstack mirror — no third hand-copied fetch block (the D-094 mirror
tax 110b started collapsing). (D-211 reserved.)

## RFC anchor

- RFC §6.2 — Planner interface / RunContext: the runtime populates
  `MemoryBlocks`; the planner renders what it is handed (the 83d injection
  contract). 84e is runtime-side population only — the planner gains no new
  surface.
- RFC §6.5 — the `Embedder` seam (D-189/D-191 settled text): embedding traffic
  is identity-attributed and opt-in; the run loop reaches it only through the
  memory store's semantic mode.
- RFC §6.6 — Memory subsystem: "Semantic retrieval is an opt-in mode, not a
  strategy (D-191)" — composition, never replacement. 84e is the first
  run-loop consumer of `SearchTurns`.

## Briefs informing this phase

- brief 04 — memory-and-skills (the retrieval surface + session-scoping
  contract the recall path consumes)
- brief 13 — react-prompt-engineering (the memory-injection surface: tier
  semantics, UNTRUSTED framing, injection ordering / KV-cache stability)

## Brief findings incorporated

- **brief 13 §2.3 — distinct tag names per memory tier.** The reference design
  reserves `<read_only_external_memory>` for "retrieved/long-term" memory —
  "read-only external memory retrieved before this run" — distinct from
  `<read_only_conversation_memory>` (the session window). Recalled turns are
  retrieved-before-this-run memory by definition; they land in the External
  tier, which 83d shipped (wrapper, UNTRUSTED rules block, fail-loud
  serialization) and nothing populates in production. 84e needs **zero planner
  changes** as a result.
- **brief 13 §2.3 — UNTRUSTED framing.** Recalled turns are stored
  conversational content — the exact prompt-injection vector the
  `<read_only_*_memory>` anti-injection preamble exists for. By riding the
  existing wrapper, recall inherits the framing for free; no new framing is
  invented.
- **brief 13 §5 (via the D-146 injection order) — prefix stability.** The
  injection order most-stable → least-stable preserves KV-cache windows. The
  recalled block is keyed to the run's query, which is fixed for the run's
  lifetime — so the External tier is stable across all of a run's steps, and
  the Conversation tier's bytes are untouched.
- **brief 04 §4.2 — identity is mandatory; memory is session-scoped.**
  Recall reuses the same session quadruple (RunID zeroed) the 83f memory fetch
  established; `SearchTurns` is identity-scoped at the store and the run loop
  never widens it.
- **brief 04 §retrieval (as adopted by D-191).** Semantic retrieval composes
  with the strategy; an embed failure fails the search loudly — the store
  never silently degrades. 84e extends that posture to the run loop: a recall
  failure fails the RUN loudly (see "Degradation posture" under Goals).

## Findings I'm departing from (if any)

- **`ProjectMemoryBlocks`' godoc sketch reserved the External tier for "a
  long-term memory phase."** The Phase 110b helper's comment says "the
  External tier remains nil pending a long-term memory phase" — implying a
  user/tenant-tier producer (the Phase 88 episodic tier). 84e populates it
  earlier, with **session-scoped** recall, because brief 13 §2.3 defines the
  tier by *provenance* ("retrieved before this run"), not by scope. The tier
  renders a map, so the future episodic/long-term producer composes additional
  keys alongside `recalled_turns` rather than competing for the slot. Recorded
  here (and to be logged in D-211 at ship); not a brief departure — it is a
  correction of a code-comment sketch toward the brief's actual tier semantics
  (comments are the least-authoritative source, CLAUDE.md §2).

## Goals

- **One injection call site — promoted, not duplicated.** A new helper in
  `internal/runtime/runctx` (the Phase 110b home for RunContext-population
  code) owns the whole memory step:

  ```go
  // FetchMemoryBlocks fetches the session's memory patch, optionally
  // recalls semantically similar past turns for `query`, and projects
  // both into the planner's MemoryBlocks. The ONE production home for
  // the memory step of run-loop RunContext population.
  func FetchMemoryBlocks(ctx context.Context, store memory.MemoryStore,
      id identity.Quadruple, query string,
      recall memory.RecallSettings) (*planner.MemoryBlocks, error)
  ```

  Internally: `GetLLMContext` → `ProjectMemoryBlocks` (unchanged, still
  exported); when `recall.Enabled`, additionally `SearchTurns(ctx, id, query,
  recall.TopK)` → similarity-floor filter → dedup against the recent-turn
  window → per-turn text cap → populate `MemoryBlocks.External` as
  `{"recalled_turns": [...]}` (each entry: user / assistant / score /
  timestamp). Both production call sites —
  `cmd/harbor/cmd_dev_runloop.go::runOne` and the devstack mirror in
  `harbortest/devstack/devstack.go` — replace their inline `GetLLMContext` +
  `ProjectMemoryBlocks` blocks with one call. The D-094 mirror for this step
  collapses to a one-liner on both sides.
- **Opt-in, with `memory.retrieval: semantic` as the ONLY switch.** The mode
  that enables `SearchTurns` on the store is the mode that enables recall in
  the run loop — no second enablement knob (a "with-recall / without-recall"
  toggle next to the mode would be the §13 two-parallel-mechanisms smell, the
  same call 111d made when it dropped the sketched `skill_propose.enabled`
  key). Not configured → `recall.Enabled` false → `SearchTurns` is never
  called → zero embedder traffic. Developers are never forced to configure an
  embedding model.
- **Golden default parity (the 84b posture).** With semantic mode off, the
  `MemoryBlocks` value — and therefore the rendered prompt — is byte-for-byte
  what pre-84e code produced. A golden test pins it.
- **Config knobs in the existing memory block, with documented defaults.**
  Top-k reuses the existing `memory.retrieval_top_k` (84d; default
  `memory.DefaultSemanticTopK` = 5 — one cap for `SearchTurns` callers, not a
  second run-loop-only cap). One NEW field: `memory.retrieval_min_score`
  (float, the cosine similarity floor; valid range [-1, 1]; **default 0.0** —
  turns with negative similarity, i.e. anti-correlated with the query, are
  never worth prompt tokens; 0.0 keeps the knob meaningful without guessing a
  model-dependent "good" threshold). Projected via a 110c-shaped exporter —
  `memory.RecallSettings{Enabled, TopK, MinScore}` +
  `memory.RecallFromConfig(cfg config.MemoryConfig) RecallSettings` next to
  `SnapshotFromConfig`, with the same field-parity test pattern (the B3
  silent-field-drop drift class stays closed). `docs/CONFIG.md` gains the new
  field's heading (the doc-drift gate enforces it); the 83n `harbor init`
  template's commented memory block and `examples/harbor.yaml` are updated in
  the same PR.
- **Degradation posture: fail loud — a recall error fails the run.** When
  recall is enabled and `SearchTurns` errors (embed failure, store failure,
  dimension drift), `FetchMemoryBlocks` returns the wrapped error and the
  run-loop drivers take the EXISTING memory-failure path:
  `MarkFailed(code=runtime_fetch_error)`, the LLM is never called (83f's
  posture, verbatim). Degrading to rolling-summary-only with a logged notice
  was considered and rejected: it is precisely the §13 silent-degradation
  shape — the operator enabled semantic recall, the agent would silently
  answer without it, and the silent-context-loss bug class CLAUDE.md §5 names
  would be back. It also matches the band: D-191 call 4 ("an embed failure
  fails the search loudly; the store never silently degrades"), and the
  owner's lean toward fail-loud recovery semantics. Recovery is the
  operator's move (fix the embeddings config / provider), not the runtime's
  guess. `ErrSemanticDisabled` surfacing here would mean the settings
  projection and the store mode disagree — a wiring bug; failing loud is
  correct, and the exporter's test pins that they cannot disagree under
  `RecallFromConfig`.
- **D-026 heavy-content guard.** Recalled turns enter the prompt as text only
  (`UserMessage` / `AssistantResponse` — never `ArtifactsShown` blobs or
  hidden refs), each side truncated to a documented per-turn cap
  (`recalledTurnTextCap`, 2 KiB, with an explicit `…[truncated]` marker) so
  the injected block stays far below the heavy-output threshold by
  construction. The LLM-edge `ErrContextLeak` pass remains the backstop, not
  the mechanism.
- **Token-budget interaction (111e) addressed, not hand-waved.** Recalled
  turns ride injection MESSAGES, not the trajectory — `MaybeCompress`'s
  estimator (D-202) measures the serialized trajectory and will not see them.
  The control is therefore the bounded shape: ≤ top-k turns × ≤ 2 × 2 KiB
  text, a hard ceiling independent of `planner.token_budget`. The helper's
  godoc documents this; folding injection size into the compression estimator
  is explicitly out of scope (it would belong to a budget-estimator phase,
  and today's recent-turns + summary injection has the same property).
- **Observability without new wire surface.** A `slog.Debug` line (count +
  top score; never turn content — CLAUDE.md §7) when recall fires. No new
  canonical event type and no Protocol method (keeps this phase clear of the
  113a generated-docs lockstep); the Protocol sibling is recorded under
  Risks.
- **Godoc hygiene.** All new exported identifiers (`FetchMemoryBlocks`,
  `RecallSettings`, `RecallFromConfig`, the config field) carry jargon-free
  godoc — written for a reader who has never seen a Harbor phase number
  (Phase 102's drift-audit guard will be active; do not lean on "Phase 84e
  (D-211)" as the explanation, cite behavior).

## Non-goals

- **No `memory.search` Protocol method and no Console memory-search page.**
  The Protocol read over `SearchTurns` is the deferred sibling: per D-062 a
  Console page cannot ship without its feeding Protocol surface, so the
  method must precede (or accompany) any such page — parked for post-109
  planning, recorded in Risks below and in the master-plan detail block.
- **No planner changes.** The `<read_only_external_memory>` wrapper, the
  UNTRUSTED rules block, the D-146 ordering, and `MemoryBlocks` are 83d
  surface, reused as-is.
- **No memory-interface changes.** `SearchTurns` is consumed, not modified;
  no similarity-floor parameter is added to the store (the floor is a
  prompt-injection policy, so it lives with the projection).
- **No second enablement knob, no per-run recall opt-out.** One switch:
  `memory.retrieval: semantic`.
- **No re-fetch on REDIRECT.** Memory (including recall) is fetched once
  before the run starts, exactly like the existing 83f fetch; a goal-mutating
  REDIRECT does not re-run recall at V1.1.x.
- **No compression-estimator integration** (see the token-budget Goal).
- **No backfill embedding.** Turns stored before semantic mode was enabled
  have no vectors and are not recalled (84d's contract); 84e does not add a
  re-embedding pass.

## Acceptance criteria

- [ ] `runctx.FetchMemoryBlocks` is the ONE home for the run-loop memory
      step: both `cmd/harbor/cmd_dev_runloop.go::runOne` and
      `harbortest/devstack/devstack.go`'s run loop call it, and their inline
      `GetLLMContext` + `ProjectMemoryBlocks` blocks are deleted (no third
      copy of the fetch logic exists; `grep -rn "GetLLMContext"` outside
      `internal/memory`, `internal/runtime/runctx`, the Protocol memory
      surface, and tests returns nothing).
- [ ] **Opt-in:** with `memory.retrieval` unset, `SearchTurns` is never
      called and the embedder receives zero traffic — asserted with a
      call-counting `embeddingstest` embedder.
- [ ] **Default parity:** with semantic mode off, the produced
      `*planner.MemoryBlocks` (and the rendered injection messages) are
      byte-for-byte identical to the pre-84e output — golden test.
- [ ] **Owner's acceptance shape (E2E):** a session whose rolling-summary
      recent window has scrolled past an early turn; semantic mode on; a new
      task whose query is semantically related to that early turn. The
      recalled turn appears in the prompt's `<read_only_external_memory>`
      block AND changes the answer — the scripted LLM answers correctly only
      when the recalled fact is present in its request. Real drivers on the
      seam (memory + state + tasks + steering RunLoop; the deterministic
      `embeddingstest` embedder, which is test-scoped by D-191 design).
- [ ] Recall **composes** with `rolling_summary`: the Conversation tier
      (summary + recent turns) is unchanged when recall is on; recalled turns
      render only in the External tier.
- [ ] `memory.retrieval_min_score` config field: validated to [-1, 1]
      (`config.semantic` category on violation), default 0.0; documented in
      `docs/CONFIG.md` (doc-drift gate passes), the 83n init template's
      commented memory block, and `examples/harbor.yaml`. Turns scoring below
      the floor are not injected — covered by a unit test.
- [ ] `memory.RecallFromConfig` exporter + field-parity test (the
      `SnapshotFromConfig` pattern): a new recall-relevant field on
      `config.MemoryConfig` without a corresponding `RecallSettings`
      projection fails the test.
- [ ] **Fail loud:** with recall enabled, a `SearchTurns` error fails the run
      (`runtime_fetch_error`), the LLM is never called, and there is no
      silent fallback to rolling-summary-only — covered by a test with a
      failing embedder.
- [ ] **Identity scoping:** recalled turns never cross `(tenant, user,
      session)` — integration test with two concurrent sessions storing
      distinguishable turns asserts no cross-talk at the run-loop edge.
- [ ] **D-026:** each recalled turn's text is capped (per-side
      `recalledTurnTextCap` with a truncation marker); a test storing an
      oversized turn asserts the injected block stays sub-threshold and the
      LLM edge raises no `ErrContextLeak`.
- [ ] **Dedup:** a recalled turn already present in the patch's recent-turn
      window is dropped from the External tier — covered by a unit test.
- [ ] **Concurrent reuse (§11 / D-025):** N≥100 concurrent
      `FetchMemoryBlocks` invocations against one shared semantic store under
      `-race` — no data races, no cross-identity bleed, no goroutine leaks.
- [ ] `scripts/smoke/phase-84e.sh` carries real assertions (see Smoke script
      additions) with FAIL = 0 and OK > 0.
- [ ] New exported identifiers carry jargon-free godoc (no bare phase-number
      explanations).

## Files added or changed

- `internal/runtime/runctx/memory_fetch.go` (+ `memory_fetch_test.go`) —
  `FetchMemoryBlocks`, the recall filter/dedup/cap pipeline, the
  `recalledTurnTextCap` constant. `ProjectMemoryBlocks` keeps its signature;
  its godoc's "External tier remains nil" sentence is updated.
- `internal/memory/from_config.go` (+ test) — `RecallSettings` +
  `RecallFromConfig` + the field-parity test.
- `internal/config/config.go` + `loader.go` — `memory.retrieval_min_score`
  field + range validation.
- `cmd/harbor/cmd_dev_runloop.go` — driver opts gain
  `memoryRecall memory.RecallSettings`; the inline memory block becomes one
  `runctx.FetchMemoryBlocks` call; `cmd_dev.go::bootDevStack` projects the
  settings via `memory.RecallFromConfig`.
- `harbortest/devstack/devstack.go` — the D-094 mirror: same swap, same
  projection.
- `docs/CONFIG.md` — the new field's heading; `examples/harbor.yaml` + the
  83n init template's commented memory block — the new knob.
- `test/integration/phase84e_semantic_recall_test.go` — the owner's
  acceptance-shape E2E + isolation + fail-loud cases.
- `scripts/smoke/phase-84e.sh` — real assertions at ship.
- `docs/decisions.md` — D-211 (logged at ship; number reserved now).
- `docs/glossary.md` — the semantic-recall term.
- `docs/plans/README.md` — row + detail block (this PR); status flip at ship.

## Public API surface

- `runctx.FetchMemoryBlocks(ctx context.Context, store memory.MemoryStore,
  id identity.Quadruple, query string, recall memory.RecallSettings)
  (*planner.MemoryBlocks, error)` — the run-loop memory step; what
  `cmd/harbor`, devstack, and a headless SDK consumer building a RunSpec all
  call.
- `memory.RecallSettings{Enabled bool; TopK int; MinScore float64}` +
  `memory.RecallFromConfig(cfg config.MemoryConfig) RecallSettings`.
- Config: `memory.retrieval_min_score` (new), riding the existing
  `memory.retrieval` / `memory.retrieval_top_k` pair.
- No planner surface change; no memory-interface change; no Protocol change.

> Scope note (as 84d): "public" is module-internal; the external facade is
> the `sdk/` program's concern. `runctx` is already part of the promoted
> 110b surface in-module consumers compose.

## Test plan

- **Unit:** the recall filter pipeline (min-score floor, dedup against the
  recent window, per-turn cap + truncation marker, top-k bound); the
  External-tier map shape; the default-parity golden; `RecallFromConfig`
  projection + field-parity; config validation range; the disabled-mode
  short-circuit (zero `SearchTurns` / embedder calls).
- **Integration:** `test/integration/phase84e_semantic_recall_test.go` — the
  owner's acceptance shape end-to-end on real drivers (memory + state +
  tasks + steering RunLoop + react planner + scripted LLM + `embeddingstest`
  embedder): recalled-turn-changes-the-answer; two-session isolation;
  fail-loud on embedder failure (`runtime_fetch_error`, no LLM call).
  Devstack consumes the same helper, so the kit exercises the production
  code path (§17.6 fix-both-sides by construction).
- **Conformance:** none new — `SearchTurns` conformance shipped with 84d;
  84e consumes it.
- **Concurrency / leak:** N≥100 concurrent `FetchMemoryBlocks` against one
  shared store under `-race`; goroutine baseline restored.

## Smoke script additions

`scripts/smoke/phase-84e.sh` (unit-tests class — the preflight dev boot
deliberately carries no embedding provider, the same honesty call 84d's smoke
recorded; the live-provider leg stays with the `HARBOR_LIVE_LLM` probes):

- Seam assertion: `runctx.FetchMemoryBlocks` exists and both run-loop call
  sites consume it (no surviving inline `GetLLMContext` fetch block in
  `cmd/harbor` / devstack).
- Config assertion: built-binary `harbor validate` rejects an out-of-range
  `memory.retrieval_min_score` (exit 1, names the key) and accepts the
  documented default.
- The recall round-trip: the Phase 84e integration tests under `-race`
  (acceptance shape + parity + isolation).
- Doc assertion: `docs/CONFIG.md` carries the `memory.retrieval_min_score`
  heading.

Until the phase ships, the skeleton is a single honest
`skip "phase 84e: not yet implemented"`.

## Coverage target

- `internal/runtime/runctx`: ≥ 90% (the 110b package target holds).
- `internal/memory` (the exporter addition) + `internal/config` (the field):
  ≥ the touched packages' existing targets.

## Dependencies

- 84d (D-191) — `SearchTurns`, the semantic mode, `embeddingstest`.
- 83d (D-146) — the `<read_only_external_memory>` wrapper + UNTRUSTED
  framing + injection order.
- 83f (D-149) — the run-loop memory fetch + `runtime_fetch_error` posture
  this phase promotes and extends.
- 110b (D-195) — the `internal/runtime/runctx` home + `ProjectMemoryBlocks`.
- 110c (D-196) — the `FromConfig` exporter pattern `RecallFromConfig`
  follows.
- 107c — the run-loop prompt surface (native tool-calling era) the injected
  block composes into.
- 111e (D-202, soft) — the `planner.token_budget` surface the token-budget
  Goal positions against.

## Risks / open questions

- **Deferred sibling (owner-recorded): a `memory.search` Protocol method must
  precede any Console memory-search page (D-062 ordering).** Out of 84e's
  scope; parked for post-109 planning. D-191 already noted "a Protocol read
  over `SearchTurns` is future work that rides the existing memory-protocol
  pattern when a Console page demands it" — 84e re-records it so the Console
  wave cannot forget the ordering.
- **Recall quality is embedding-model-dependent.** The 0.0 default floor
  filters only anti-correlated turns; a weak embedding model can still
  surface marginal recalls. The knob exists (`retrieval_min_score`) and the
  UNTRUSTED framing bounds the blast radius ("if it conflicts with the
  current query, ignore it"); tuning guidance belongs to the operator docs,
  not to a runtime heuristic.
- **Pre-enablement turns are invisible to recall** (no vectors — the 84d
  contract; no backfill at V1.1.x). An operator flipping the mode mid-session
  sees recall over post-flip turns only; the CONFIG.md entry says so.
- **Single fetch per run.** A REDIRECT that mutates the goal does not re-run
  recall; acceptable at V1.1.x (the whole memory fetch has had this property
  since 83f) and recorded so a steering phase can revisit deliberately.
- **External-tier cohabitation.** The future episodic/long-term tier (Phase
  88) will share `MemoryBlocks.External`; the map-with-keys shape
  (`recalled_turns` now, sibling keys later) is the agreed composition —
  drift risk if 88's planning forgets; this plan + the helper godoc name it.

## Glossary additions

- **Semantic recall** — the run-loop consumption of semantic memory retrieval
  (Phase 84e, D-211 reserved): when `memory.retrieval: semantic` is on, the
  run loop searches the session's embedded turns with the task query
  (`MemoryStore.SearchTurns`) and injects the top-k scored, floored, deduped,
  text-capped turns into the planner prompt's `<read_only_external_memory>`
  tier — composing with `rolling_summary`, never replacing it; off by default
  with byte-for-byte prompt parity. Add to `docs/glossary.md` in the
  implementation PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (`FetchMemoryBlocks` over a shared store is the artifact here.)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (`test/integration/phase84e_semantic_recall_test.go`.)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
