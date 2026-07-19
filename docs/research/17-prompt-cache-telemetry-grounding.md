# Research Brief 17 — Prompt-cache telemetry and intent, grounded in Harbor's LLM edge

Status: research / pre-RFC. Grounds an externally-drafted "Prompt Cache Intelligence" proposal against Harbor's actual source (four read-only grounding passes over `internal/llm`, `internal/planner/react`, the usage/cost/telemetry plumbing, and the config/capability surface). Informs the v1.16 cache-telemetry phase and the deferred cache-intent/stability phases. Every claim below carries a file:line from the grounding passes; the proposal's ungrounded claims are corrected, not repeated.

## 1. The finding that reorders everything: Harbor already drops cache data

The bifrost dependency (v1.5.21) **already returns cache accounting on every response**: `BifrostLLMUsage.PromptTokensDetails.CachedReadTokens` / `CachedWriteTokens` (bifrost `schemas/chatcompletions.go:1618-1634`). Harbor's translator `extractUsageAndCost` (`internal/llm/drivers/bifrost/translate.go:639-665`) never reads `PromptTokensDetails` — only `PromptTokens` / `CompletionTokens` / `TotalTokens` / `CompletionTokensDetails.ReasoningTokens` / `Cost`. Cache read/write token counts are computed by the provider, returned by bifrost, and silently discarded.

The same is true on the request side: bifrost already ships the full cache wire vocabulary — request-level `ChatParameters.CacheControl` / `PromptCacheKey` / `PromptCacheRetention` (`schemas/chatcompletions.go:203-232`), per-content-block `ChatContentBlock.CacheControl` / `CachePoint` (`:1118-1128`), per-tool `ChatTool.CacheControl` (`:381`). None of Harbor's `translateRequest` / `translateMessages` / `translateParts` / `translateToolDeclaration` sets any of them.

**Consequence:** the cache capability is not "invent a provider-neutral mechanism" — bifrost owns the wire mechanism. Harbor's work is (a) stop dropping the response counters, (b) wire a neutral hint through to fields that already exist. Both are narrower and lower-risk than the external proposal framed.

## 2. Verified vs corrected claims about Harbor's insertion points

| Claimed insertion point | Verdict |
|---|---|
| "One shared CompleteRequest" | Real as a TYPE (`internal/llm/llm.go:119-160`); fiction as a construction path — five independent sites build it (react prompt builder, summarizer ×2, run-naming, governance conformance). The only universal seam is the `llm.Open()` wrapper chain. |
| "One provider boundary" | Real. `LLMClient`/`Driver` (`llm.go:63-88`), single production driver (bifrost), §4.4 registry (`registry.go:396-409`). |
| "Centralized prompt construction" | Overstated. `PromptBuilder` (`react.go:189-196`) is react-planner-local and operator-overridable. |
| "Mandatory LLM safety wrapper" | Real and non-bypassable: `safetyClient` innermost band, `registry.go:463-552` `Open()` composes governance(retry(downgrade(corrections(safety(driver))))). |
| "Normalized usage and cost" | Real but **bifurcated**: governance consumes `CompleteResponse.Cost/Usage` synchronously in-band (`internal/governance/cost.go:180-235` `PostCall`, deliberately NOT event-subscribed to avoid a bus race — `safety.go:389-395`); `llm.cost.recorded` (`events.go:150-167`) is a best-effort observability mirror. |
| "Immutable shared clients with per-call state" | Real, documented (`llm.go:40-44`), pinned at N=128 under `-race`. |
| `ProviderExtras` as a cache surface | Wrong — `Usage.ProviderExtras` exists (`llm.go:494-499`) but has ZERO writers; it is an unused placeholder, not a signal. |
| A driver `CacheCapabilities` query | Nothing to extend, and `Supports*` capability protocols are a §13-forbidden practice when all drivers implement everything (one production driver exists). Cache-support variance is per-**provider**, not per-driver — resolve as operator-declared per-model config (`config.LLMModelProfileConfig`, `config.go:320-337`, mirrored via `from_config.go:780-819` like `ReasoningEffort` / `JSONSchemaMode`), never a capability handshake. |

## 3. Prefix-invalidation hazards in the react prompt builder, ranked

1. **Trajectory compaction is a one-shot cache-annihilation event.** `MaybeCompress` (`internal/planner/compression.go:194-262`) replaces the entire per-step replay (`prompt.go:298-349`) with a short summary block; the majority of a long run's prompt tokens vanish in one turn. Nothing gates the decision on cache economics — no cache metric exists anywhere today.
2. **Deferred tool discovery appends unsorted, mid-run.** `mergeDiscovered` (`discovered_tools.go:170-201`) appends newly-discovered tools in discovery order to BOTH the `<available_tools>` prompt text (`prompt.go:636-679`) and `req.Tools[]` — invalidating the prompt prefix and the tool-declaration array simultaneously. (Contrast: the static catalog IS sorted, `tools/catalog.go:152-166`.)
3. **Repair guidance mutates the system prompt mid-run.** `<additional_guidance>` recomputes each turn from mutating repair counters (`prompt.go:592-601`, `repair_guidance.go:281-321`); since the whole system prompt is one joined string (`prompt.go:227-231,611`), a repair event on turn N invalidates the prefix from that section onward for the rest of the run.
4. **Heavy-result artifact refs are freshly minted per render** (`prompt.go:434-441`, `1264-1448`) — tool-result messages are never byte-identical across renders. Any byte-stability conformance must normalize refs or it always fails.
5. **The date-only identity section already embeds an ad-hoc stability decision** (`prompt.go:614-621` — the comment explicitly reasons about same-day KV-cache stability, rolling at UTC midnight). Harbor's first StabilityClass call already exists, undocumented.

Two structural corrections for any fingerprint/hint design:

- Sections are `strings.Join`-collapsed into ONE string before `CompleteRequest` exists — section-level hints must be computed inside `buildSystemContent` (`prompt.go:553-612`), react-only; they cannot attach at the `Open()`/driver seam post-hoc.
- The safety pass MUTATES content between build and driver (`safety.go:52` → `enforceContextSafety` → DataURL materialization, `materialize.go:34`) — a fingerprint computed at build time is invalidated; compute post-safety or prove idempotence.

## 4. The observation path, corrected

- `CacheUsage` must land on `CompleteResponse.Usage` (`llm.go:489-499`) FIRST for the synchronous governance path; the `llm.cost.recorded` mirror (`CostRecordedPayload`, `events.go:150-167`, stamped by `emitCostRecorded`, `safety.go:151-163`) is secondary.
- **Lockstep reality:** `CostRecordedPayload` crosses the wire as opaque `Payload any` inside `StateEvent` — the wire manifest lists only the event-type string, not the payload shape. Three consumers hand-decode it independently with no compile check: the TUI reducer (`internal/tui/projection/projection.go:351-360`), the Console run-events reader (`web/console/src/lib/tasks/run-events.ts:126-154`), and the sessions enricher's two branches (`internal/sessions/protocol/enricher.go:338-365`). A new field missed in one silently reads zero — the phase plan must enumerate all three. The ONE generated gate that does fire automatically: the protocol-docs event-payload reflection index (`cmd/harbor-gen-protocol-docs/events.go:115` + its lockstep test).
- Existing fingerprint precedent worth mirroring for later phases: `Trajectory.Serialize()` (`internal/planner/trajectory/serialize.go:10-59`) — byte-stable canonical JSON with a documented round-trip contract and `ErrUnserializable` fail-loud; and `ArtifactStub.MarshalJSON` (`llm.go:542-558`), byte-stable but single-struct-scoped.
- A `llm.prompt.diff` diagnostic is a NEW surface category: nothing today puts prompt content on any bus, and the audit redactor's three canonical rules (`internal/audit/rules.go:78-108`) are structurally blind to section-level diffs — it needs its own §7 threat-model pass before shipping.

## 5. Phase sequencing the grounding supports

- **Cache-token capture first** (the v1.16 phase): translator reads + `Usage` fields + `CostRecordedPayload` mirror + the three hand-decoded consumers + Console cost components + docs regen. Additive, zero design forks, immediately produces the data every later decision needs. Governance ceiling math intentionally untouched (cache tokens informational first).
- **Cache intent → bifrost lowering** (candidate later in the wave, per operator decision): `CachePolicy` field on `CompleteRequest` (needs its own D-number — every `CompleteRequest` field addition historically carries one: D-021/D-022/D-026/D-189/D-272/D-273/D-276), config via `LLMModelProfileConfig`, per-run override riding `planner.LLMOverrides` (`planner.go:391-430`) nil-means-inherit.
- **Compiler wrapper / StabilityClass sections / cache-aware compaction**: deferred. Compaction gating is blocked on plumbing that does not exist (`Usage` is never threaded back into `planner.RunContext`/`Budget` — no reference in `compression.go` or the `MaybeCompress` call site, `runloop.go:769-772`) and must not be designed without real cache-hit data.

## 6. Spawned-child cache posture (cross-reference)

Whether a Batch-spawned child's first prompt is cache-warm relative to its parent (same static sections, same tool catalog) is grounded in brief 16 §6 — the child-inheritance pass. The cache phases do not depend on it; the batch executor phase consumes it.
