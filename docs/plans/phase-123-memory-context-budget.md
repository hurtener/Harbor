# Phase 123 — memory context budget enforcement + D-026 conversation-scope refinement

## Summary

`rolling_summary` silently ignores its configured `budget_tokens`, so a long-lived
session's assembled context (recent turns + rolling summary) grows without bound until
the D-026 LLM-edge byte check trips and **every subsequent run fails at planner step 0**
("session poisoning", found by live profiling 2026-06-23). This phase makes
`rolling_summary` honor its documented budget contract (keep a dev-configurable recent-turn
set verbatim; recursively/chunk-summarize older content — never discarding prior summaries —
until the assembled context fits the budget), makes the recent-turn set operator-configurable,
and refines the D-026 heavy-content byte check so it governs **offloadable content**
(tool/MCP results + binary DataURL parts) rather than legitimate conversation text.

## RFC anchor

- RFC §6.5
- RFC §6.6
- RFC §6.10

## Briefs informing this phase

- brief 04
- brief 13

## Brief findings incorporated

- brief 04 §"rolling-summary": the strategy's model is `append turn → evict older into pending → background-summarize`, keeping the last `FullZoneTurns` verbatim and folding the rest into a running summary. This phase keeps that model and adds the **missing** budget-enforcement step the brief's truncation design already mandates ("keep last `FullZoneTurns` verbatim, then enforce the token budget per `OverflowPolicy`") — `rolling_summary` must enforce the same token cap truncation does.
- brief 04 §"OverflowDropOldest": truncation drops oldest turns until the token estimate fits (the shipped `OverflowDropOldest` policy, D-035). `rolling_summary`'s equivalent is "summarize oldest-first until it fits" — compaction is the rolling-summary analogue of drop-oldest, and it must run until the budget is satisfied, not on a fixed turn count.
- brief 13 §"UNTRUSTED memory frame": conversation memory is injected as `<read_only_conversation_memory>` context — it is *legitimate conversation context*, semantically distinct from a heavy tool observation. This grounds the D-026 refinement: the heavy-output byte threshold is an anti-bloat / offload rule for tool & MCP **results**, not a cap on conversation text.

## Findings I'm departing from (if any)

Two deliberate departures from settled decisions, both filed as new `docs/decisions.md` entries:

- **D-241 (new) — narrows D-026.** D-026 / RFC §6.5 state the LLM-edge safety net asserts *"no message reaching the LLM carries raw heavy content"* uniformly. This phase narrows the **byte** check to offloadable content (`RoleTool` text + binary `DataURL` parts of any role); conversation text (`RoleSystem`/`RoleUser`/`RoleAssistant`, including the injected rolling summary) is governed by the existing token-window guard (`ErrContextWindowExceeded`) instead. Rationale: D-026's byte threshold exists to force tool/artifact **offload to `ArtifactStub`**; conversation text is not offloadable that way and a growing rolling summary crossing 32 KiB is a false positive, not a leak. The fail-loud `ErrContextLeak` invariant is unchanged for the content classes it was designed for.
- **D-242 (new) — recent-turn set becomes operator-configurable.** `strategy.FullZoneTurns` is currently a const with the comment "an operator who needs to tune it files an RFC PR rather than fighting yaml". This phase adds `memory.recent_turns` to `MemoryConfig`; the const becomes the default when unset. Consistent with D-035 (budget knobs *are* operator-tunable; only the retry/backoff/cadence constants are not).

Not departing from D-026's "**V1 does not auto-truncate at the safety net; the planner is responsible for recovery; auto-cascade is post-V1**": this phase does NOT add safety-net auto-recovery. It fixes the **memory strategy** to self-bound to its *own* configured budget (its documented contract) so that, with `budget_tokens ≤ provider window`, memory never emits context that overflows the window in the first place. The safety net's behavior is unchanged.

## Goals

- `rolling_summary` enforces `budget_tokens`: the assembled `GetLLMContext` patch (summary + recent turns) never exceeds the configured token budget when one is set.
- Compaction is **size-driven, not count-driven**: older turns are summarized oldest-first, and — when the recent set itself plus the summary still exceeds budget — the recent turns are summarized in chunks too, **folding into (never discarding) the prior summary**, until the estimate fits.
- The recent-turn set is operator-configurable via `memory.recent_turns`.
- The D-026 byte heavy-content check applies only to offloadable content (`RoleTool` text + binary `DataURL` parts); conversation text is exempt and governed by the token-window guard.
- A long conversation that previously poisoned its session now compacts and stays runnable (live-Console verified on a fresh session).

## Non-goals

- **Artifact-spill of oversized single conversation messages** (carry a too-large user/assistant message in turn 1 as text, auto-offload to an `ArtifactRef`, embed the ref id in the summary for later partial/full re-read). Genuinely new memory↔artifact integration; deferred to a follow-up phase. Documented here so it is not lost.
- Safety-net **auto-recovery / auto-cascade** when the token-window guard fires (remains post-V1 per D-026; the planner still owns that recovery path).
- Changing `truncation` or `none` strategies (truncation already enforces the budget correctly — it is the reference implementation this phase mirrors into `rolling_summary`).
- Semantic-retrieval (`retrieval: semantic`) behavior.

## Acceptance criteria

- [ ] AC-1: With `budget_tokens > 0`, `rolling_summary.GetLLMContext` returns a patch whose `Tokens` (summary + recent turns) is `≤ budget_tokens` for any sequence of `AddTurn` calls, including turns whose individual size approaches the budget.
- [ ] AC-2: When the recent-turn set (`recent_turns` verbatim) alone exceeds the budget, the executor summarizes the recent turns in oldest-first chunks, folding each chunk into the existing summary (prior summary content is preserved, not replaced wholesale), until `Tokens ≤ budget_tokens`.
- [ ] AC-3: `budget_tokens == 0` preserves today's "no budget / unbounded" behavior (back-compat).
- [ ] AC-4: `memory.recent_turns` config is honored; unset → `strategy.FullZoneTurns` default (4). Validated in `loader.go::Validate` (non-negative).
- [ ] AC-5: The D-026 byte check (`findContextLeak`) fails with `ErrContextLeak` for a `RoleTool` message text ≥ threshold and for a binary `DataURL` part ≥ threshold (any role), and does NOT fire for a `RoleSystem`/`RoleUser`/`RoleAssistant` text ≥ threshold.
- [ ] AC-6: The token-window guard (`ErrContextWindowExceeded`) still fires for an oversized assembled conversation (it remains the governor for conversation size).
- [ ] AC-7: Regression — replay of the live-found poisoning sequence (≥50 turns into one key with a 32 KiB `budget_tokens` and a 32 KiB heavy threshold) yields a runnable context (no `ErrContextLeak`) on turn 51.
- [ ] AC-8: Concurrent-reuse — N≥100 concurrent `AddTurn`/`GetLLMContext` across distinct keys on one shared executor under `-race`: no races, no cross-key bleed, budget honored per key.
- [ ] AC-9: `docs/decisions.md` gains D-241 + D-242; RFC §6.5 updated to state the byte check's offloadable-content scope; glossary updated.

## Files added or changed

- `internal/memory/strategy/rolling_summary.go` — budget enforcement + recursive/chunked compaction in `AddTurn`/`GetLLMContext`; consume a `RecentTurns` dep.
- `internal/memory/strategy/strategy.go` — `Deps.RecentTurns` (default `FullZoneTurns`); doc.
- `internal/memory/registry.go`, `internal/memory/from_config.go`, `internal/memory/drivers/{inmem,sqlite,postgres}/*.go` — thread `RecentTurns` through `Open` → driver → executor `Deps`.
- `internal/config/config.go` — `MemoryConfig.RecentTurns int` (`yaml:"recent_turns,omitempty"`) + doc.
- `internal/config/loader.go` — validate `recent_turns ≥ 0`.
- `internal/llm/safety.go` — `findContextLeak` role-scoped: byte check on `RoleTool` text + binary parts; exempt conversation text.
- `internal/memory/strategy/rolling_summary_test.go`, `internal/llm/safety_test.go` — new tests (AC-1..AC-8).
- `internal/memory/conformancetest/conformancetest.go` — budget-enforcement assertion (all rolling_summary drivers).
- `RFC-001-Harbor.md` §6.5 — byte-check scope clarification.
- `docs/decisions.md` — D-241, D-242.
- `docs/glossary.md` — new terms.
- `examples/harbor.yaml` — document `recent_turns`.
- `scripts/smoke/phase-123.sh`.

## Public API surface

```go
// internal/memory/strategy
type Deps struct {
    // ... existing ...
    RecentTurns int // recent-window size; 0 → FullZoneTurns default
}

// internal/config
type MemoryConfig struct {
    // ... existing ...
    RecentTurns int `yaml:"recent_turns,omitempty"` // 0 → strategy default (FullZoneTurns)
}
```

No change to the `MemoryStore` / `StrategyExecutor` interfaces or `LLMContextPatch` shape.

## Test plan

- **Unit:** rolling_summary budget enforcement (AC-1..AC-4, AC-7); chunked recent-set compaction preserving prior summary (AC-2); `findContextLeak` role-scoping (AC-5, AC-6); config validation (AC-4).
- **Integration:** `test/integration/` — memory(rolling_summary, real sqlite StateStore) + llm safety edge end-to-end: a long conversation assembles a within-budget request that passes the LLM-edge safety pass. Real drivers on the seam, identity propagation, ≥1 failure mode (budget=0 unbounded path still rejected by token-window guard), under `-race`.
- **Conformance:** budget-enforcement assertion added to the memory conformance suite (inmem + sqlite + postgres rolling_summary all honor `budget_tokens`).
- **Concurrency / leak:** AC-8 — N≥100 concurrent invocations on one shared executor under `-race`; goroutine baseline restored after `Close`.

## Smoke script additions

`scripts/smoke/phase-123.sh` (PREFLIGHT_REQUIRES: unit-tests):

- `go test ./internal/memory/strategy/... -run RollingSummary_Budget -race`
- `go test ./internal/llm/... -run FindContextLeak_RoleScope -race`
- assert `recent_turns` documented in `examples/harbor.yaml`.

## Coverage target

- `internal/memory/strategy`: ≥ 85% (existing target held)
- `internal/llm` (safety.go touched): ≥ 80%

## Dependencies

- 25a (durable memory strategies / shared executor — the package this phase edits)

## Risks / open questions

- **Summarizer cost under aggressive compaction.** Chunked recent-set summarization can issue multiple summarizer (LLM) calls in one `AddTurn` when a session is far over budget. Mitigation: only compact when over budget; chunk oldest-first; cap chunks per call and let the next turn continue (bounded work per turn, like the recovery loop). Open: exact per-turn chunk cap — pick a constant, document it.
- **Token estimate vs byte threshold mismatch (the root-cause class).** Budget is tokens; heavy threshold is bytes. The D-026 refinement (D-241) decouples them for conversation, but operators can still set a `budget_tokens` whose byte footprint exceeds a (now tool-only) heavy threshold — harmless after D-241 since conversation text is exempt. Documented.
- **Degraded mode.** When the summarizer is degraded (health FSM), compaction can't run; `GetLLMContext` already falls back to recent-window-only. Ensure the budget path degrades to drop-oldest on the recent window rather than emitting over-budget context. Covered by AC + degraded-path test.

## Glossary additions

- **Recent-turn set** — the most-recent N conversation turns kept verbatim in the LLM context (operator-configurable via `memory.recent_turns`; default `FullZoneTurns`).
- **Recursive compaction** — summarizing oldest-first (including chunks of the recent set when needed), folding into — never discarding — the prior rolling summary, until the assembled context fits the token budget.
- **Conversation context (vs offloadable content)** — System/User/Assistant message text, governed by the token-window budget; distinct from offloadable content (tool/MCP results, binary parts) governed by the heavy-output byte threshold.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (per-key budget, AC-8)
- [ ] Concurrent-reuse test passes (AC-8) — the executor is a reusable artifact.
- [ ] Integration test exists (memory↔llm-safety seam) under `-race`.
- [ ] Glossary updated (3 terms above)
- [ ] Brief/decision departures justified above + D-241 + D-242 filed
