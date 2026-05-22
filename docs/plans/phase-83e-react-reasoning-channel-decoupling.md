# Phase 83e — react-reasoning-channel-decoupling

## Summary

Decouple the ReAct planner's **decision contract** (the JSON the model emits) from its **reasoning capture** (provider-side thinking content). Three coordinated changes:

1. **Narrow the action schema** — drop `Reasoning` from `Decision_CallTool` (and any other Decision sum variant carrying it). The model emits `{tool, args}` only; Phase 44 schema repair strips incoming `reasoning` fields and logs a soft telemetry event.
2. **Surface captured reasoning on `CompleteResponse`** — extend `internal/llm.CompleteResponse` with `Reasoning string`. Update the bifrost driver to read `BifrostChatResponse.Choices[0].Message.ReasoningDetails` and concatenate the text-typed entries. This closes (a) the unary-path gap (today `OnReasoning` is streaming-only) AND (b) the Gemini-direct black hole pinned in brief 13 §2.6 (today bifrost populates `reasoning_details[]` on the message but Harbor drops it). The bifrost streaming path continues to forward per-delta reasoning to `OnReasoning` for live UX, while the final response's `Reasoning` is the authoritative captured trace.
3. **Add the per-agent replay knob** — `PlannerConfig.ReasoningReplay` enum, defaulting to `never` for ALL models. When set to `text`, the trajectory renderer prepends each prior step's captured reasoning trace as a text block before the prior `{tool, args}` action JSON. No `provider_native` mode in V1 (Bifrost docs don't address thinking-block round-trips).

## RFC anchor

- RFC §6.2 (Planner)
- RFC §6.5 (LLM client)

## Briefs informing this phase

- brief 13
- brief 03
- brief 08

## Brief findings incorporated

- brief 13 §2.6 (revision 2026-05-19, empirical Bifrost probe): *"`OnReasoning` works as documented for thinking-class providers routed via OpenRouter; the native Gemini path is a black hole; `CompleteResponse` has no `Reasoning` field."* This phase closes all three observations.
- brief 13 §2.6 (Bifrost docs verification): *"Bifrost normalizes all provider-specific reasoning formats to a consistent OpenAI-compatible structure using `reasoning` in requests and `reasoning_details` in responses."* This phase reads `reasoning_details[]` from the response message — bifrost's documented canonical surface — rather than relying on the per-delta `delta.Reasoning` field alone (which is `nil` for Gemini-direct).
- brief 13 §2.6 (Bifrost cliffs): *"Anthropic requires `reasoning.max_tokens >= 1024`. Requests with lower values will fail with an error."* This phase **does NOT silently clamp** — operators see the failure surfaced per AGENTS.md §5 fail-loudly. The `internal/llm/drivers/bifrost/translate.go` translator adds documentation noting the floor; the failing request returns a typed `ErrReasoningBudgetTooLow` with the provider's minimum named.
- brief 13 §2.6 (replay policy): the predecessor never replays; Harbor's stance is **never-replay default for ALL models**, per-agent operator opt-in. This phase ships only two replay modes: `never` (default) and `text`. `provider_native` waits for upstream Bifrost guidance on signed-thinking-block round-trips.

## Findings I'm departing from (if any)

- **Schema-narrowing the Decision sum is a binary departure from Phase 45.** Phase 45 (D-051) ships `Decision_CallTool{Tool, Args, Reasoning}` as the V1 action shape. This phase removes the `Reasoning` field. The departure is recorded as **D-147** in `docs/decisions.md` with rationale: (a) the prompt-side CRITICAL clamp removes the model's expectation (Phase 83a); (b) the empirical reasoning capture happens via the provider channel (Phase 83e §2 of this plan); (c) replaying is now a per-agent knob (Phase 83e §3) so the "we need reasoning visible in the trajectory" use case is preserved by configuration rather than by schema.
- **No `provider_native` replay mode in V1.** Brief 13 §2.6 noted three possible modes (`never`, `text`, `provider_native`). This phase ships only the first two. The `provider_native` mode would pass Anthropic's `signature`-bearing thinking blocks through bifrost as API constructs across turns; Bifrost docs do not address that round-trip, and Harbor cannot guarantee correctness today. Recorded as **D-148** (replay knob shape: enum with two values, not three; revisit when Bifrost grows the round-trip surface).

## Goals

- The model emits `{tool, args}` only. The runtime tolerates (strip-and-warn) any `reasoning` / `thought` / V2-rich-output fields older trained models may emit.
- Reasoning is captured from Bifrost's normalised `ReasoningDetails` on the response message — both for streaming AND unary calls. `CompleteResponse.Reasoning` is the canonical carrier.
- The trajectory step (`internal/planner/trajectory`) carries the captured reasoning trace as a separate field; it is **never** part of the `Action` field.
- Operators configure replay per agent via `PlannerConfig.ReasoningReplay` enum (`never` (default) / `text`). Per-run override via `RunContext.ReasoningReplay *ReplayMode` for tenant-specific policy.
- `harbor inspect-runs` surfaces the captured `reasoning_trace` for each step (Console-visible change documented as part of this phase).
- Gemini-direct provider now reports reasoning to Harbor (because we read `ReasoningDetails` from the message, where bifrost's gemini provider populates it). A conformance test validates this against a recorded fixture.

## Non-goals

- `provider_native` replay mode (deferred — D-148).
- Auto-clamping Anthropic reasoning budgets to the 1024-token floor. We fail loudly with `ErrReasoningBudgetTooLow` instead and document the constraint in `examples/harbor.yaml`.
- Surfacing reasoning to the Console **stream**. The existing `OnReasoning` streaming callback continues to fire as today (for providers that emit per-delta reasoning); we add a `<thinking>` SSE channel on the Protocol surface in a separate phase.
- Per-provider reasoning effort floors for non-Anthropic providers. Effort enums map through bifrost as-is; bifrost handles model-specific constraints internally.

## Acceptance criteria

- [ ] `internal/llm.CompleteResponse` gains `Reasoning string` field. Empty when the provider did not surface reasoning.
- [ ] `internal/llm/drivers/bifrost/translate.go` (or a new sibling `reasoning.go`) ships a `reasoningFromMessage(*bfschemas.ChatMessage) string` helper that walks `msg.ReasoningDetails`, concatenates entries whose `Type` is `BifrostReasoningDetailsTypeText` or `BifrostReasoningDetailsTypeReasoningText` (whichever upstream uses for plain text), and returns the joined string. Encrypted/signature-bearing entries are skipped (no use in V1 — `provider_native` is out of scope).
- [ ] Unary path: `internal/llm/drivers/bifrost/bifrost.go::Complete` reads `bfResp.Choices[0].Message.ReasoningDetails` after a successful `ChatCompletionRequest` and stamps it on `CompleteResponse.Reasoning`.
- [ ] Streaming path: the final stream chunk's message — when it carries `ReasoningDetails` — is preferred over the accumulated `reasoningB.String()`. (When the final chunk lacks the details array but `reasoningB` has content, fall back to the accumulated builder for compatibility with providers whose stream emits deltas but no final message-level normalisation.)
- [ ] `internal/planner/ifaces.Decision_CallTool.Reasoning` field is **removed**. Phase 44's schema-repair pipeline tolerates incoming `reasoning` JSON fields by silently dropping them and emitting `planner.action_extra_field_dropped` event with `{field: "reasoning"}` for telemetry. (The CRITICAL clamp in Phase 83a's prompt makes this drift uncommon, but the runtime fails open — strip, never error — for backward compatibility.)
- [ ] `internal/planner/trajectory.TrajectoryStep` gains `ReasoningTrace string` (set by the planner step loop from `CompleteResponse.Reasoning`).
- [ ] `internal/config.PlannerConfig` gains `ReasoningReplay string` (validated as enum: `""` defaults to `"never"`, valid values `"never"` and `"text"`; any other value fails config validation loudly).
- [ ] `RunContext.ReasoningReplay *ReasoningReplayMode` is a per-run override (nil → use `PlannerConfig.ReasoningReplay`; non-nil → use this).
- [ ] When replay mode is `text`: trajectory renderer for the next turn prepends each prior step's `ReasoningTrace` (if non-empty) as a text block ABOVE the prior `{tool, args}` JSON in the assistant turn. Empty traces produce no prepended block.
- [ ] When replay mode is `never`: trajectory renderer emits prior `{tool, args}` JSON only — no reasoning, regardless of whether `ReasoningTrace` is populated.
- [ ] `harbor inspect-runs <run-id> --json` includes `steps[].reasoning_trace` on each step. The CLI's tabular output adds a `reasoning_chars` column (count, not full text) so the table stays scannable.
- [ ] Bifrost driver translator validates Anthropic's `max_tokens >= 1024` reasoning budget at translation time. Lower budgets return `ErrReasoningBudgetTooLow` BEFORE the request hits the API. The error names the provider, the requested budget, and the floor.
- [ ] Concurrent-reuse: 100+ concurrent runs against a shared `ReActPlanner` with disjoint `RunContext.ReasoningReplay` modes pass under `-race`. Counter-leak / state-bleed assertions identical to Phase 83c's d025 suite.
- [ ] Per-provider conformance fixture: `internal/llm/drivers/bifrost/testdata/reasoning_fixtures/` carries one recorded golden-response per probed provider (openrouter-claude, openrouter-deepseek-r1, openrouter-o4-mini, openrouter-gemini-flash, gemini-direct-gemini-flash). The test loads each fixture, invokes the driver's unary path against a stub bifrost client returning the fixture, and asserts the populated `Reasoning` matches an expected value derived from `ReasoningDetails`. **No live API calls in CI**; the live probe (brief 13 §2.6) is the source of the fixtures.

## Files added or changed

- `internal/llm/llm.go` — add `Reasoning string` to `CompleteResponse`; godoc explains it's populated from the provider's normalised reasoning channel when available, empty otherwise.
- `internal/llm/drivers/bifrost/bifrost.go` — read `ReasoningDetails` from response message; integrate into the unary + streaming response paths.
- `internal/llm/drivers/bifrost/reasoning.go` (new) — `reasoningFromMessage` helper + `BifrostReasoningDetailsType*` constant references.
- `internal/llm/drivers/bifrost/translate.go` — Anthropic `max_tokens >= 1024` validation; typed `ErrReasoningBudgetTooLow`.
- `internal/planner/ifaces/decision.go` (or wherever `Decision_CallTool` lives) — drop `Reasoning` field. Update godoc.
- `internal/planner/repair/repair.go` (Phase 44 territory) — strip `reasoning` / `thought` keys from incoming JSON before Decision marshaling; emit `planner.action_extra_field_dropped` event.
- `internal/planner/trajectory/step.go` — `ReasoningTrace string` field on `TrajectoryStep`.
- `internal/planner/react/react.go` — wire `CompleteResponse.Reasoning` to `TrajectoryStep.ReasoningTrace`; consult `ReasoningReplay` when rendering prior turns.
- `internal/planner/react/prompt.go` — trajectory rendering branches on `effectiveReplayMode(rc)`; the new `replayPriorStep` helper handles the `text` mode prepend.
- `internal/planner/planner.go` — `ReasoningReplayMode` type + `RunContext.ReasoningReplay *ReasoningReplayMode`.
- `internal/config/config.go` — `PlannerConfig.ReasoningReplay string` field + enum validation in `Validate`.
- `examples/harbor.yaml` — comment the new key; document the Anthropic `max_tokens >= 1024` floor with a worked example.
- `internal/events/taxonomy.go` — register `EventTypePlannerActionExtraFieldDropped`.
- `cmd/harbor/cmd_inspect.go` (or wherever inspect-runs lives) — expose `reasoning_trace` in `--json` output + add a `reasoning_chars` column to the tabular view.
- `internal/llm/drivers/bifrost/testdata/reasoning_fixtures/*.json` — recorded fixtures, one per probed provider.
- `internal/llm/drivers/bifrost/reasoning_test.go` (new) — fixture-driven tests.
- `internal/llm/drivers/bifrost/conformance_test.go` — extend with a `ReasoningCapture` conformance pass.
- `docs/decisions.md` — **D-147** (schema narrowing — drop `Reasoning` from `Decision_CallTool`) + **D-148** (replay knob shape — two enum values, defer `provider_native`).
- `scripts/smoke/phase-83e.sh` — static-only assertions on the testdata fixtures + the new enum validation.
- `docs/glossary.md` — fill in `Reasoning channel` + `Reasoning replay knob` (placeholders added with this brief 13 revision).
- `docs/plans/README.md` — Status column flip on merge.

## Public API surface

```go
// internal/llm/llm.go (delta)

type CompleteResponse struct {
    Content   string
    Reasoning string // NEW — populated when provider exposes reasoning_details; empty otherwise.
    Cost      Cost
    Usage     Usage
}

// internal/planner/planner.go (delta)

type ReasoningReplayMode string

const (
    ReasoningReplayNever ReasoningReplayMode = "never" // default — no replay regardless of model.
    ReasoningReplayText  ReasoningReplayMode = "text"  // prepend captured reasoning as text in prior assistant turns.
)

type RunContext struct {
    // ...existing fields...
    ReasoningReplay *ReasoningReplayMode // NEW — nil falls back to PlannerConfig.ReasoningReplay.
}

// internal/config/config.go (delta)

type PlannerConfig struct {
    Driver           string
    MaxSteps         int
    ExtraGuidance    string              // Phase 83a
    ReasoningReplay  string              // NEW — enum, "" defaults to "never". Valid: "never", "text".
    Extra            map[string]string
}

// internal/planner/ifaces/decision.go (delta — REMOVAL)

type Decision_CallTool struct {
    Tool string
    Args map[string]any
    // Reasoning string // REMOVED — captured on TrajectoryStep.ReasoningTrace instead.
}

// internal/planner/trajectory/step.go (delta)

type TrajectoryStep struct {
    // ...existing fields...
    ReasoningTrace string // NEW — captured from CompleteResponse.Reasoning; never re-injected into prompts unless ReasoningReplay == "text".
}

// internal/llm/drivers/bifrost (new typed error)

var ErrReasoningBudgetTooLow = errors.New("bifrost: provider-specific reasoning budget below floor")
```

## Test plan

- **Unit:**
  - `reasoningFromMessage` tests over a table of fixture `ChatReasoningDetails` slices (mixed text + encrypted entries; empty slice; nil slice).
  - `effectiveReplayMode` tests: nil run-context override falls back to config; non-nil overrides; invalid config value rejected by `Validate`.
  - Trajectory render tests: replay-`never` produces `{tool, args}` only; replay-`text` with non-empty trace produces a text block ABOVE the JSON; replay-`text` with empty trace produces no block.
  - Phase 44 repair: incoming JSON with `"reasoning": "..."` is stripped silently; one event emitted.
  - Anthropic budget validator: `effort=low` mapping to <1024 tokens returns `ErrReasoningBudgetTooLow` BEFORE bifrost is called.
- **Integration:**
  - Stubbed bifrost client returns a `BifrostChatResponse` with `ReasoningDetails`; assert `CompleteResponse.Reasoning` is populated and `TrajectoryStep.ReasoningTrace` is set on the resulting step.
  - End-to-end planner run with `ReasoningReplay=text` over two turns: turn 2's prompt contains the turn-1 reasoning trace as a text block in the assistant turn. With `ReasoningReplay=never` the same scenario produces no reasoning in turn 2's prompt.
  - Conformance pass against the five recorded fixtures (one per provider probed in brief 13 §2.6): each fixture's `ReasoningDetails` produces a non-empty `Reasoning` string after driver translation. Crucially, the `gemini-direct-gemini-flash` fixture passes — proving the Gemini-direct black hole is closed by reading the message-level details.
- **Conformance:** existing planner conformance pack extended with a `ReasoningCapture` test all `Planner` impls must pass (asserts a successful step populates `ReasoningTrace` when the driver returns reasoning).
- **Concurrency / leak:** mandatory — N≥100 concurrent runs against a shared `ReActPlanner` with disjoint per-run replay modes; assert no replay-mode bleed, no trace bleed, no goroutine leak.

## Smoke script additions

- `scripts/smoke/phase-83e.sh` (classification: `static-only`):
  - Assert `internal/llm/drivers/bifrost/testdata/reasoning_fixtures/*.json` exists for the five probed providers.
  - Assert each fixture contains a non-empty `reasoning_details` field.
  - Assert `internal/planner/ifaces/decision.go` does NOT contain the substring `Reasoning string` in the `Decision_CallTool` definition (post-narrowing).
  - Assert `examples/harbor.yaml` documents `planner.reasoning_replay` with a comment naming both valid enum values.

## Coverage target

- `internal/llm/drivers/bifrost`: 90% (reasoning capture is a hot path; conformance fixtures carry weight).
- `internal/planner/react`: 85%.
- `internal/planner`: 90%.
- `internal/planner/trajectory`: 90%.

## Dependencies

- 45 (reference ReAct planner — the Decision sum we narrow).
- 32 (LLM client core — the `CompleteResponse` we extend).
- 33 (bifrost driver — where reasoning capture lands).
- 44 (schema repair — where the extra-field drop lives).

## Risks / open questions

- **Bifrost upstream evolution.** Bifrost is at v1.5.10 as of authoring; the `ChatReasoningDetails` shape may evolve (signature semantics, type taxonomy). Harbor pins the version in `go.mod`; bumps require re-running the conformance fixtures.
- **Provider-native pass-through demand.** Operators running Anthropic-thinking-mode workloads may benefit from passing signed thinking blocks across turns to preserve provider-side optimization. Phase 83e leaves this as **D-148**; revisit when (a) Bifrost docs cover the round-trip explicitly OR (b) we see a real workload that measurably benefits.
- **Fixture freshness.** Provider behavior shifts under us (Anthropic adds new thinking-block types, Gemini changes its parts layout). The fixture set wants periodic re-recording. Phase 83e ships a `scripts/probe/record-reasoning-fixtures.sh` helper documented in its README; running it against the live providers refreshes the fixtures. NOT a CI gate (live keys), but operator-runnable.
- **Replay-text token cost on long histories.** Operators flipping to `text` replay on a chatty workload may see prompts balloon. Mitigation: the replay renderer respects the existing trajectory-summary pathway (Phase 46); when a summary exists, replayed reasoning lives in the summary scope, not the per-step scope. A test asserts this composition.
- **Console SSE for live thinking.** Out of scope for this phase; tracked as a follow-up.
- **D-147 / D-148 numbering.** Confirm against the current `docs/decisions.md` head before committing — if parallel work claimed D-147 we bump to the next free pair. (Same rule as Phase 83c's D-105.)

## Glossary additions

- **Reasoning channel** — the provider-side surface (Anthropic extended thinking, OpenAI o-series, DeepSeek native, Gemini `thought:true` parts) Bifrost normalises into `reasoning_details[]` on the response message. Harbor's bifrost driver reads this field after Phase 83e and exposes it via `CompleteResponse.Reasoning`. Distinct from the action JSON: reasoning never appears in the structured output the model emits.
- **Reasoning replay knob** — `PlannerConfig.ReasoningReplay` enum, default `never`. When set to `text`, the trajectory renderer prepends each prior step's captured reasoning trace as a text block before the prior `{tool, args}` action JSON. Per-agent operator opt-in for workloads that benefit from CoT continuity across turns.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — required (replay-mode per-run scope is the headline guarantee). Passes.
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent runs against a shared `ReActPlanner` with disjoint `RunContext.ReasoningReplay` modes; no state bleed.
- [ ] **Integration test passes** — required (Deps lists 32 + 33 + 44). Real bifrost driver path (stubbed at the bifrostClient boundary) + real schema-repair pass + real trajectory renderer.
- [ ] **Fixture conformance pass** — five providers' recorded `ReasoningDetails` produce non-empty `Reasoning` strings after driver translation; Gemini-direct case explicitly passes.
- [ ] Glossary updated (Reasoning channel + Reasoning replay knob).
- [ ] `docs/decisions.md` D-147 + D-148 entries filed.
