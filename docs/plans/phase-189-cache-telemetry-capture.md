# Phase 189 — Cache-token capture: stop dropping provider cache accounting

## Summary

Bifrost (`github.com/maximhq/bifrost/core@v1.5.21`) already returns provider
cache-token accounting on every chat-completion response
(`BifrostLLMUsage.PromptTokensDetails.CachedReadTokens` /
`CachedWriteTokens`); Harbor's translator never reads it, so the data is
computed by the provider, returned by bifrost, and silently discarded before
it reaches `llm.Usage`. This phase closes that gap end to end: the two
aggregate cache-token counts land on `llm.Usage`, mirror onto
`llm.CostRecordedPayload`, and become visible everywhere Usage already is —
the TUI's turn status and context readout, the Console's per-task cost
breakdown, and (as a documented, deliberate non-extraction) the sessions
enricher. Telemetry-only: no request-side field, no governance ceiling
change, no new Protocol method.

## RFC anchor

- RFC §6.5
- RFC §5.2

## Briefs informing this phase

- brief 17
- brief 08
- brief 03

## Brief findings incorporated

- brief 17 §1: "Harbor's translator `extractUsageAndCost`
  (`internal/llm/drivers/bifrost/translate.go:639-665`) never reads
  `PromptTokensDetails` — only `PromptTokens` / `CompletionTokens` /
  `TotalTokens` / `CompletionTokensDetails.ReasoningTokens` / `Cost`. Cache
  read/write token counts are computed by the provider, returned by bifrost,
  and silently discarded." — this phase's entire scope is closing exactly
  this gap, nothing more.
- brief 17 §2: "`ProviderExtras` as a cache surface — Wrong —
  `Usage.ProviderExtras` exists (`llm.go:494-499`) but has ZERO writers; it
  is an unused placeholder, not a signal." — the new fields are typed,
  first-class `llm.Usage` fields (`CacheReadTokens`, `CacheWriteTokens`), not
  a write into the unused `ProviderExtras` bag.
- brief 17 §4: "`CacheUsage` must land on `CompleteResponse.Usage`
  (`llm.go:489-499`) FIRST for the synchronous governance path; the
  `llm.cost.recorded` mirror (`CostRecordedPayload`, `events.go:150-167`,
  stamped by `emitCostRecorded`, `safety.go:151-163`) is secondary... Three
  consumers hand-decode it independently with no compile check... A new
  field missed in one silently reads zero — the phase plan must enumerate
  all three." — Acceptance criteria below enumerate all three verified
  decode sites by name and file:line, plus the one Console display component
  that renders the fields this phase adds.
- brief 17 §5: "Cache-token capture first (the v1.16 phase): translator
  reads + `Usage` fields + `CostRecordedPayload` mirror + the three
  hand-decoded consumers + Console cost components + docs regen. Additive,
  zero design forks... Governance ceiling math intentionally untouched
  (cache tokens informational first)." — this is the phase's exact scope
  statement; Goals/Non-goals below mirror it directly.
- brief 03 §6: "Unit tests: ... cost calculation across providers ...
  provider-quirk normalizers (one test per documented quirk)." — grounds
  the Test plan's requirement for a fixture per provider cache-reporting
  shape (OpenAI's collapsed `cached_tokens` compatibility field vs. the
  explicit `cached_read_tokens`/`cached_write_tokens` split), not just one
  synthetic struct literal.
- brief 08: bifrost's usage/cost pass-through was empirically validated
  (23/24 gating items across six providers, including "token usage and cost
  reporting") before adoption — this phase extends the already-validated
  translator boundary rather than opening a new integration risk surface.

## Findings I'm departing from (if any)

None. One scope call made *beyond* what any brief specifies (not a
departure from a stated finding): bifrost v1.5.21 also carries a nested
Anthropic/Bedrock-specific TTL breakdown,
`ChatPromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens5m` /
`CachedWriteTokens1h` (`schemas/chatcompletions.go:1637-1639`, populated by
the `anthropic`/`bedrock` provider drivers from Anthropic's
`cache_creation.ephemeral_5m_input_tokens` /
`ephemeral_1h_input_tokens`). Brief 17 documents only the two top-level
aggregate counts (`CachedReadTokens` / `CachedWriteTokens`); it does not
mention the TTL sub-struct, so scoping it out is not a departure from a
brief finding — it is this plan's own scope decision, recorded here per
the "verify and say so" instruction. See Non-goals.

## Goals

- Stop dropping bifrost's cache-token accounting at the translator boundary:
  `extractUsageAndCost` reads `PromptTokensDetails.CachedReadTokens` /
  `CachedWriteTokens` when present.
- Make the two counts observable everywhere `llm.Usage` already is:
  `CompleteResponse.Usage` (the synchronous governance-visible path),
  `llm.cost.recorded` (the observability mirror), and every verified
  hand-decoder of that event.
- Ship a real UI consumer in the same PR — the Console's per-task cost
  breakdown surfaces the new counts, not just the wire.
- Zero behavior change: governance cost/budget accounting, retry/downgrade
  routing, and prompt construction are byte-for-byte unaffected.

## Non-goals

- No `CachePolicy` / `CacheHint` field on `CompleteRequest` and no
  request-side cache-control wiring into bifrost's `ChatParameters.CacheControl`
  / `PromptCacheKey` / per-content-block `CachePoint`. This is the wave's
  deferred "Phase B" (brief 17 §5's "cache intent → bifrost lowering") — an
  operator decision point mid-wave, not scoped here, and (per brief 17 §5)
  needs its own `D-NNN` the way every prior `CompleteRequest` field addition
  has (D-021/D-022/D-026/D-189/D-272/D-273/D-276).
- No canonicalization, `StabilityClass`, or prefix-fingerprinting of prompt
  sections (brief 17 §3's prefix-invalidation hazards are read and cited for
  context; none are touched — this phase is response-side only).
- No cache-aware trajectory-compaction gating. Blocked on plumbing that does
  not exist: `Usage` is never threaded back into `planner.RunContext` /
  `Budget` today (brief 17 §5 — no reference in `compression.go` or the
  `MaybeCompress` call site, `runloop.go:769-772`), and must not be designed
  against real cache-hit data this phase doesn't yet produce a history of.
- No Anthropic/Bedrock 5m/1h cache-write TTL breakdown
  (`ChatCachedWriteTokenDetails`). It exists in bifrost v1.5.21 today but has
  no consumer need yet (no governance/UI feature asks for TTL-level
  granularity) and would roughly double this phase's decode/mirror/render
  surface for a single-provider-family diagnostic. Deferred to a future
  phase if a real consumer need appears; the field is already sitting in the
  dependency, so that phase is additive, not a bifrost upgrade.
- No governance ceiling/budget math change. `governance.CostAccumulator.PostCall`
  (`internal/governance/cost.go:180-235`) folds `resp.Cost.TotalCost` plus the
  attempt-cost tap only; cache tokens carry no dollar figure of their own
  (they are informational token counts, not a cost line) and PostCall is not
  touched — the coverage-target package list below omits `internal/governance`
  because no source line in it changes.
- No new Protocol method, REST endpoint, or wire-visible `internal/protocol/types`
  change. `llm.CostRecordedPayload` crosses the wire as opaque `Payload any`
  inside `StateEvent` (verified: no `CostRecordedPayload` reference anywhere
  under `internal/protocol/` or `web/console/src/lib/protocol/`) — D-223's
  TS lockstep gate has no manifest entry for it and stays untouched by this
  phase; see the acceptance criterion below that makes this explicit instead
  of silently skipping the gate.
- The Console overview cost-rollup (`web/console/src/lib/overview/cost.ts`,
  feeding `CostRollupCard.svelte` and `cost-governance-panel.svelte`) is
  untouched. Verified: it decodes only `Model` and `Cost.TotalCost` from
  `llm.cost.recorded` — it never reads `Usage` at all — so it has no
  cache-token gap to close and is not a fourth hand-decoder in brief 17 §4's
  sense.

## Acceptance criteria

- [ ] `extractUsageAndCost` (`internal/llm/drivers/bifrost/translate.go:644-665`)
      reads `resp.Usage.PromptTokensDetails.CachedReadTokens` /
      `CachedWriteTokens` into the new `llm.Usage` fields when
      `PromptTokensDetails != nil`; a nil `PromptTokensDetails` (or nil
      `resp.Usage`) yields `CacheReadTokens == 0 && CacheWriteTokens == 0` —
      the existing "nil-usage yields zero values" contract extends, it does
      not change shape.
- [ ] `llm.Usage` (`internal/llm/llm.go:489-499`) gains
      `CacheReadTokens int` and `CacheWriteTokens int`, both documented as
      "a subset of `PromptTokens`, not additional tokens" (mirroring bifrost's
      own semantics: cache reads are prompt tokens served from cache, not
      extra tokens). `ProviderExtras` is untouched — no writer added to it.
- [ ] `llm.CostRecordedPayload` (`internal/llm/events.go:150-167`) carries the
      new fields for free through its embedded `Usage` field; `emitCostRecorded`
      (`internal/llm/safety.go:403-421`) requires no code change (it already
      forwards `resp.Usage` verbatim) — a test asserts the two new fields
      survive the `Publish` round trip unchanged.
- [ ] `governance.CostAccumulator.PostCall` (`internal/governance/cost.go:180-235`)
      is byte-for-byte unmodified; a test feeds a response with non-zero
      `CacheReadTokens`/`CacheWriteTokens` and asserts `folded` / the
      accumulator's per-key total is identical to the same response with
      those fields zeroed — cache tokens are provably inert to budget math.
- [ ] TUI reducer: the `costPayload` struct and the `"llm.cost.recorded"`
      case (`internal/tui/projection/projection.go:337-366`, struct at
      `:886-895`) decode `Usage.CacheReadTokens` / `CacheWriteTokens` and
      accumulate them into `projection.Usage` and each `RunUsage[event.Run]`
      entry (the `Usage` struct at `:110-117` gains the two matching
      `int64` fields with `json:"cache_read_tokens,omitempty"` /
      `"cache_write_tokens,omitempty"`).
- [ ] TUI rendering: the per-turn status line
      (`internal/tui/app/live.go:1033-1036`, which already renders
      `usage.TotalTokens` + `usage.USD`) and the composer context readout
      (`internal/tui/app/transcript_render.go:376-383`, which already
      renders `Context {prompt}/{window} ({pct}%)`) surface a cache
      indicator (e.g. a `(Nk cached)` qualifier) when `CacheReadTokens > 0`,
      using the existing `compactTokens`/`formatTokens` helpers — no new
      formatting primitive.
- [ ] Console decoder: `projectRunCost` (`web/console/src/lib/tasks/run-events.ts:133-161`)
      reads `Usage.CacheReadTokens` / `CacheWriteTokens` (PascalCase wire,
      the same defensive `readNumber(usage, [...])` pattern already used for
      the other four `Usage` fields) into two new `RunCost` fields
      (`cacheReadTokens`, `cacheWriteTokens`, interface at `:87-108`,
      `EMPTY_RUN_COST` at `:110-121`), documented as a subset of
      `promptTokens`, not additive to `totalTokens`.
- [ ] Console rendering: `RightRailCostBreakdown.svelte` (`web/console/src/lib/components/tasks/RightRailCostBreakdown.svelte`)
      surfaces the cache counts as a non-summed annotation — e.g. a
      qualifier on the existing "Input" row ("12,000 (8,000 cached)") or a
      clearly-labeled non-total informational line — never as a fifth peer
      row silently added into the visual "Total" the way `Input`/`Output`/`Reasoning`
      currently are (those rows are already documented as "not the sum of
      the three above" via `totalUSD`/`totalTokens`, but a naive additive
      "Cache" row would double-count against `promptTokens` since cache
      reads are a subset of prompt tokens, not a fifth category). Hidden
      (no cache row/qualifier rendered) when both counts are zero for every
      folded event — never a rendered `0 cached`.
- [ ] Sessions enricher (`internal/sessions/protocol/enricher.go`): verified
      that neither `costFromEvent` (`:338-347`) nor `costFromMap`
      (`:349-365`) reads `PromptTokens`/`CompletionTokens` — both extract
      only `Cost.TotalCost` and `Usage.TotalTokens`, and `TotalTokens` is
      unaffected by the cache split (cache reads are already included inside
      `PromptTokens`, which already rolls into `TotalTokens`). No `SessionRow`
      wire field exists to carry a cache-token count today (verified against
      `internal/protocol/types/sessions.go`), so mechanically extracting
      cache tokens here would be dead code with nowhere to land — a real
      session-level cache facet is Protocol-wire-shaped work (new `SessionRow`
      field, D-223/D-209 regen, `projectioncheck` registration per D-313) out
      of scope for a telemetry-only phase. Both functions' doc comments are
      updated to name `CacheReadTokens`/`CacheWriteTokens` explicitly and
      state why they are intentionally not extracted this phase — closing
      brief 17 §4's "enumerate all three, a missed one silently reads zero"
      concern with a documented decision instead of silence, without
      inventing an unconsumed wire field.
- [ ] `make protocol-docs-gen` regenerates `docs/site/protocol/events.md` /
      `types.md` to reflect the two new `Usage` fields (the reflection index
      entry at `cmd/harbor-gen-protocol-docs/events.go:115`,
      `llm.EventTypeCostRecorded: {Payloads: []reflect.Type{reflect.TypeOf(llm.CostRecordedPayload{})}}`,
      requires no code change — regeneration alone picks up the new struct
      fields via reflection); `make protocol-docs-gen-check` passes.
- [ ] `make protocol-ts-gen-check` passes with no manifest diff. Verified:
      `llm.CostRecordedPayload` has zero references under `internal/protocol/`
      or `web/console/src/lib/protocol/` — it is not part of
      `singlesource.CanonicalWireTypes`, so D-223's lockstep gate has nothing
      to regenerate for this phase. This criterion exists to make that
      explicit rather than have the plan silently omit the gate.
- [ ] `npm run check && npm run lint && npm run build` pass in `web/console/`.
- [ ] Coverage targets met (below); `go test -race ./internal/llm/... ./internal/tui/... ./internal/sessions/...` passes.

## Files added or changed

- `internal/llm/drivers/bifrost/translate.go` — `extractUsageAndCost` reads
  `PromptTokensDetails`.
- `internal/llm/drivers/bifrost/translate_test.go` — new cache-token fixture
  tests (real bifrost schema types / real JSON shapes, §17.8).
- `internal/llm/llm.go` — `Usage.CacheReadTokens` / `CacheWriteTokens`.
- `internal/llm/events.go` — `CostRecordedPayload` doc-comment refresh
  naming the new fields (no struct change — they ride the embedded `Usage`).
- `internal/llm/events_test.go` / `internal/llm/safety_test.go` — round-trip
  test that `emitCostRecorded` forwards the new fields unchanged.
- `internal/governance/cost_test.go` — cache-tokens-are-inert-to-ceiling-math
  test.
- `internal/tui/projection/projection.go` — `costPayload`, `Usage`, the
  `"llm.cost.recorded"` case.
- `internal/tui/projection/projection_test.go` — cache-token accumulation +
  absent-field zero-value tests.
- `internal/tui/app/live.go`, `internal/tui/app/transcript_render.go` — cache
  qualifier in the turn-status and context readouts.
- `internal/tui/app/*_test.go` (existing cost/context render tests extended).
- `internal/sessions/protocol/enricher.go` — doc-comment-only clarification
  on both `costFromEvent`/`costFromMap` branches.
- `web/console/src/lib/tasks/run-events.ts` — `RunCost` fields +
  `projectRunCost` decode.
- `web/console/src/lib/tasks/run-events.test.ts` — decode + absent-field
  tests.
- `web/console/src/lib/components/tasks/RightRailCostBreakdown.svelte` —
  non-summed cache annotation.
- `docs/site/protocol/events.md`, `docs/site/protocol/types.md` —
  regenerated (`make protocol-docs-gen`), not hand-edited.
- `scripts/smoke/phase-189.sh` — new.
- `docs/glossary.md` — new terms (see Glossary additions).
- `docs/plans/README.md` — Phase 189 index row + detail block, `Status: Shipped`
  flip when merged.

## Public API surface

```go
// internal/llm/llm.go — additive fields on the existing exported type.
type Usage struct {
    PromptTokens     int
    CompletionTokens int
    ReasoningTokens  int
    TotalTokens      int
    LatencyMS        int64
    // CacheReadTokens is the count of PromptTokens served from the
    // provider's prompt cache (a subset of PromptTokens, not additional
    // tokens). Zero when the provider/response reports no cache data.
    CacheReadTokens int
    // CacheWriteTokens is the count of PromptTokens newly written to the
    // provider's prompt cache on this call (a subset of PromptTokens).
    // Zero when the provider/response reports no cache data.
    CacheWriteTokens int
    ProviderExtras   map[string]string
}
```

No interface signatures change — `LLMClient.Complete`, `Driver.Complete`, and
`CostRecordedPayload`'s field set (only its embedded `Usage`'s shape) are
unaffected. Downstream phases (a future cache-intent phase) consume the two
new `Usage` fields as their observability baseline.

## Test plan

- **Unit:**
  - `internal/llm/drivers/bifrost/translate_test.go`: `extractUsageAndCost`
    against (a) a `bfschemas.ChatPromptTokensDetails{CachedReadTokens: N,
    CachedWriteTokens: M}` literal (explicit-split shape, e.g. Anthropic/OpenRouter),
    (b) a JSON payload unmarshaled through `bfschemas.BifrostChatResponse`
    carrying only `"cached_tokens"` (OpenAI's collapsed compatibility field,
    exercising `ChatPromptTokensDetails.UnmarshalJSON`'s
    `raw.CachedTokens != nil && raw.CachedReadTokens == 0 && raw.CachedWriteTokens == 0`
    fallback at `schemas/chatcompletions.go:1663-1665`), and (c) a nil
    `PromptTokensDetails` (zero-value honesty — matches the existing
    `TestTranslateResponse_NoUsage` pattern at `translate_test.go:623-631`).
    Per §17.8, (a)/(c) use bifrost's own vendored struct types directly
    (already this package's convention) and (b) is a captured/representative
    raw-JSON fixture run through bifrost's real `UnmarshalJSON`, not a
    hand-authored belief about OpenAI's wire shape.
  - `internal/llm/events_test.go`: `CostRecordedPayload` carries the new
    fields through `emitCostRecorded` → `Publish` unchanged.
  - `internal/governance/cost_test.go`: `PostCall` with cache tokens present
    vs. zeroed yields an identical accumulator total.
  - `internal/tui/projection/projection_test.go`: `costPayload` decode
    accumulates cache tokens into `Usage`/`RunUsage`; a payload with no
    cache fields (older-shaped fixture) decodes to zero, not a decode
    failure.
  - `web/console/src/lib/tasks/run-events.test.ts`: `projectRunCost` decodes
    both PascalCase field names and the absent-field zero case, mirroring
    the existing per-field test shape in that file.
- **Integration:** N/A — no cross-subsystem seam opens this phase; the
  translator→`Usage`→event→consumer path is a single already-shipped seam
  gaining two fields, exercised end to end by the unit tests above plus the
  live-gated smoke below. `Dependencies` names only an already-shipped phase
  (184, transitively — no new wiring).
- **Conformance:** N/A — no new driver, no multi-driver interface.
- **Concurrency / leak:** N/A — no new reusable artifact; `Usage` remains a
  plain value type copied by the existing D-025-compliant `safetyClient` /
  `CostAccumulator` paths (already N=128 / N-concurrent gated by their
  existing `concurrent_test.go` suites, unmodified by this phase).
- **Live (env-gated):** an `HARBOR_LIVE_LLM`-gated test issues two
  consecutive completions sharing a long static system-prompt prefix against
  a cache-capable configured provider and asserts the second call's
  `Usage.CacheReadTokens > 0`. Skipped by default; CI does not set
  `HARBOR_LIVE_LLM`.

## Smoke script additions

`scripts/smoke/phase-189.sh` (class: mixed `unit-tests` + `static-only`,
per-section `PREFLIGHT_REQUIRES` headers):

- Run `go test -race ./internal/llm/... ./internal/tui/... ./internal/sessions/...`
  and assert exit 0 (`unit-tests`).
- A static grep asserting `extractUsageAndCost` in
  `internal/llm/drivers/bifrost/translate.go` references
  `PromptTokensDetails` (guards against the fix regressing silently) —
  `skip` (not `fail`) until the phase ships, `ok` once it does, per the
  404/405/501-style SKIP convention adapted for a static check.
- A static grep asserting `llm.go` declares `CacheReadTokens` and
  `CacheWriteTokens` on `Usage`.
- A static grep asserting `RightRailCostBreakdown.svelte` references
  `cacheReadTokens` (guards the Console consumer landed, not just the wire).

## Coverage target

- `internal/llm`: 85%
- `internal/llm/drivers/bifrost`: 85%

## Dependencies

- 184 (independent track within the wave; consumes the already-shipped
  `llm`/`tui`/`sessions`/Console surfaces those phases and their
  predecessors left in place — no new cross-phase wiring opens here).

## Risks / open questions

- **Console row semantics.** `RightRailCostBreakdown.svelte`'s existing
  Input/Output/Reasoning/Total table already documents `totalUSD`/`totalTokens`
  as "not the sum of the three above" (independent wire figures shown
  side by side). Cache tokens are different in kind — they are a *named
  subset* of `promptTokens`, not an independent figure — so the
  implementor must render them as a subset annotation, not a fifth summed
  row, or the visual table will appear to double-count. Flagged explicitly
  in Acceptance criteria to avoid a plausible-looking but wrong
  implementation shipping green.
- **Sessions enricher's "documented non-extraction."** This phase
  deliberately does not add a session-level cache-token wire field. If a
  later phase wants "cache tokens saved this session" as an operator-facing
  rollup, that is new `SessionRow` wire surface (D-223/D-209 regen +
  `projectioncheck` registration per D-313), not a trivial follow-on to this
  phase — named here so it isn't mistaken for scope this phase already
  covers.
- **TTL-level cache-write diagnostics** (`ChatCachedWriteTokenDetails`,
  Anthropic/Bedrock-only) sit unused in the same bifrost struct this phase
  reads adjacent fields from. A future phase extending into them is
  additive (no bifrost version bump needed) but should re-justify the need
  before adding a second dimension to every consumer this phase touches.
- **Governance follow-on.** Brief 17 §5 explicitly defers "cache-aware
  compaction" — this phase's `Usage.CacheReadTokens` is the first real data
  point such a future phase would need, but the plumbing gap it's blocked on
  (`Usage` never reaching `planner.RunContext`/`Budget`) is unchanged by
  this phase and remains a named prerequisite, not a silent implication that
  this phase unblocks it.

## Glossary additions

- cache read tokens
- cache write tokens

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — no identity-scoped code path
      changes; `Usage` is a per-call value carried on `ctx`-scoped requests
      already, unmodified in shape by this phase's additions beyond two
      int fields.
- [ ] **If this phase builds a reusable artifact...**: N/A — no new
      reusable artifact; `Usage` is a plain value type, not a compiled
      artifact under D-025.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam...**: N/A — no new seam opens; this phase adds
      fields to an already-shipped, already-tested value type and updates
      its existing (already cross-subsystem) consumers in place. The
      existing per-consumer unit tests (TUI projection, Console
      `run-events.test.ts`, translator tests) are the binding gate, not a
      new integration test — no new wiring is introduced for one to prove.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed — N/A, no brief finding departed from (see above); the
      pre-assigned **D-326** is filed for the record of this phase's own
      TTL-diagnostics scope decision.
