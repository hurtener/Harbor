# Phase 34 — Provider correction layer + SchemaSanitizer (one mode, baked in)

## Summary

Lands the per-provider correction layer that sits BETWEEN the runtime and the `LLMClient` driver. Covers five quirks settled by brief 03 §4 + brief 08 §"Phase 34 scope": message reordering (NIM), JSON-Schema sanitization (`additionalProperties` / `strict` mode), reasoning-effort routing for thinking-class models, per-provider `response_format` envelope translation, and usage backfill for proxies that report `0/0`. Single baked-in mode — there is no `use_native` toggle (per master plan and brief 03 §5 "sharp edges from the source"). The layer is opt-out for testing; production callers wire `corrections(safetyClient(driver))` by default.

## RFC anchor

- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- **brief 03 §4 "LLM provider quirks"**: NIM rejects mixed system / developer-then-user message ordering and needs reorder/collapse; OpenAI structured-output mode requires `additionalProperties: false` + `strict: true` while other providers fail when those fields are present; thinking-class models (`o1`, `o3`, `deepseek-reasoner`) interpret `ReasoningEffort` differently; per-provider `response_format` envelope varies (`{"type":"json_object"}` vs `{"type":"json_schema","json_schema":{...}}` vs Anthropic's tool-schema envelope); some streaming proxies report `0/0` tokens so we estimate from byte length when a `CostOverrides` profile table is configured.
- **brief 03 §5 "two parallel LLM modes (the toggle smell)"**: the reference's `use_native_llm=True/False` shipped both LiteLLM and a `NativeLLMAdapter` in parallel — Harbor picks one architecture and bakes the correction in. The corrections layer compiles per-provider drivers into a single `LLMClient`; no toggle ships in V1.
- **brief 07 §"the structured-output / tool-calling boundary"**: structured output is the LLM's "what to say" — Phase 34 owns this layer. Tool calling is the runtime's "what to do" — Phase 34 NEVER references provider-native tool-call APIs. The scope is structured-output and message-shape correctness only.
- **brief 08 §"Phase 34 scope shrinks slightly"**: bifrost already handles provider-specific quirks for the 23 providers it ships. Harbor's `SchemaSanitizer` may only need to cover (a) `response_format` shape adjustments not handled by bifrost, (b) reasoning-effort routing for thinking-class models, (c) quirks specific to providers Harbor cares about beyond bifrost's coverage. We still ship all five master-plan quirks because they're cheap to implement, and they let the operator switch providers without bifrost-version surprises.
- **brief 08 §"ModelProfile.JSONSchemaMode"**: profiles select between native / tools / prompted schema modes per model. Phase 32 stored the field opaquely; Phase 34 begins reading it for the schema-mode dispatch (Phase 35 owns the downgrade chain).

## Findings I'm departing from (if any)

- **brief 03 §7 phase decomposition** sketched a separate L-3 phase ("Provider correction layer — one mode, baked in") and a separate L-4 ("Structured output strategies; ModelProfile; SchemaPlan with downgrade chain; `llm.mode_downgraded` events"). Phase 34 ships ONLY L-3 here — the master plan splits L-4's surface (`OutputMode = Native | Tools | Prompted` + downgrade chain + `llm.mode_downgraded` event emit) into Phase 35. This is not a brief departure so much as a scope partition; recorded for clarity so a reader who reaches for the downgrade chain knows to land Phase 35.

## Goals

- Ship a thin `Corrections` wrapper that takes `(client llm.LLMClient, cfg ConfigSnapshot) llm.LLMClient` and applies the five quirks on a per-call basis, keyed by `req.Model` → `ModelProfile.Corrections`.
- Compose order: `Open()` returns `corrections(safetyClient(driver))` — the corrections layer is the OUTERMOST wrapper so the safety pass sees the post-correction request (the final outgoing payload). Settled as D-041.
- Single baked-in mode. The operator either enables corrections (default) or disables them for tests via `LLMConfig.Corrections.Enabled=false`. No second implementation, no `use_native` toggle.
- Concurrent-reuse safe (D-025). The wrapper, the sanitizer, and the normalizer are stateless across calls.
- No `Tool*` symbol leaks into `internal/llm/corrections/...` (RFC §6.4 / brief 07). The Phase 32 smoke static guard extends to the new package path.

## Non-goals

- Phase 35's downgrade chain (`json_schema → json_object → text` on `invalid_json_schema` errors) and the `llm.mode_downgraded` event emit. Phase 34 leaves `ResponseFormat.Kind` untouched once it has translated the envelope shape; if a provider rejects the schema, Phase 35 handles the recovery.
- Phase 36's retry-with-feedback. Phase 34's corrections are PRE-call rewrites; retry-with-feedback is a POST-call rewrap with a corrective sub-prompt. Distinct seams.
- Phase 36a's cost accumulator subscription. The corrections layer's usage backfill computes an estimate; Phase 36a's accumulator subscribes to `llm.cost.recorded` events emitted by the driver (Phase 33).
- Auto-cascading recovery on context-window exceedance. Phase 32's safety pass still fails loudly with `ErrContextWindowExceeded`; Phase 34 does not (yet) compute auto-truncation. Post-V1.

## Acceptance criteria

- [ ] **One unit test per quirk** (5 tests minimum, each in `*_test.go` under `internal/llm/corrections/`):
  - Message reordering (NIM `SystemFirstStrict` policy).
  - Schema sanitization (`additionalProperties: false` + `strict: true` mode insertion / removal).
  - Reasoning-effort routing (thinking-class `Effort` → empty / `enabled=false` mapping; OpenAI `o1` / `o3` / `deepseek-reasoner` routing).
  - Per-provider `response_format` shape translation (`json_object` / `json_schema` / Anthropic-style envelope).
  - Usage backfill (zero usage in response + `CostOverrides` profile → backfilled estimate).
- [ ] Switching providers does NOT require a configuration toggle — the operator sets `ModelProfiles.<model>.Corrections.<field>` and corrections fire automatically for that model.
- [ ] **NO tool-call API references** in `internal/llm/corrections/...`. The smoke static guard catches the leak; the test asserts the absence.
- [ ] **Concurrent-reuse test (D-025)** N≥100 concurrent `Complete` invocations against ONE shared corrections wrapper under `-race`. No data races; no goroutine leaks (baseline goroutine count restored after teardown); no context bleed (per-call identity sink verifies).
- [ ] Identity-mandatory: missing identity in ctx → still fails closed (the safety pass underneath catches it; the corrections wrapper does not bypass).
- [ ] `Open()` composes `corrections(safetyClient(driver))` when `cfg.LLM.Corrections.Enabled` is true (default).
- [ ] `LLMConfig.Corrections.Enabled=false` opts out — `Open()` returns `safetyClient(driver)` verbatim. Test path uses this to verify the safety pass surface unchanged.
- [ ] Coverage on `internal/llm/corrections`: ≥ 85%.
- [ ] `scripts/smoke/phase-34.sh` green. `make preflight` green.

## Files added or changed

- `internal/llm/corrections/corrections.go` — NEW. The `Corrections` wrapper (`LLMClient` shape).
- `internal/llm/corrections/sanitizer.go` — NEW. `SchemaSanitizer.Sanitize(json.RawMessage, profile) (json.RawMessage, error)`.
- `internal/llm/corrections/normalizer.go` — NEW. `MessageNormalizer.Normalize([]llm.ChatMessage, profile) ([]llm.ChatMessage, error)`.
- `internal/llm/corrections/profiles.go` — NEW. Per-known-provider default profile lookup table + helpers.
- `internal/llm/corrections/corrections_test.go` — NEW. Wrapper-level unit tests + the D-025 concurrent-reuse test.
- `internal/llm/corrections/sanitizer_test.go` — NEW. Schema-mode quirks.
- `internal/llm/corrections/normalizer_test.go` — NEW. Message-reordering quirks.
- `internal/llm/corrections/profiles_test.go` — NEW. Per-provider default-lookup coverage.
- `internal/llm/llm.go` — MODIFIED. Add `CorrectionsProfile` struct + four enum types (`MessageOrderingPolicy`, `SchemaSanitizationMode`, `ReasoningRouting`, `ResponseFormatProfile`) + the `ModelProfile.Corrections` field. The types live in the `llm` package so the corrections sub-package can read them without import cycle; the LOGIC lives in `internal/llm/corrections/`.
- `internal/llm/registry.go` — MODIFIED. Extend `ConfigSnapshot.CorrectionsEnabled bool`. `Open()` composes `corrections.Wrap(safetyClient(...))` when the flag is true (default).
- `internal/config/config.go` — MODIFIED. Extend `LLMConfig.Corrections LLMCorrectionsConfig`. Extend `LLMModelProfileConfig.Corrections *LLMCorrectionsProfileConfig`.
- `internal/config/validate.go` — MODIFIED. `validateLLM` checks the new fields' enum constraints.
- `internal/config/fixtures/`-equivalent (the existing per-test snapshot fixtures, if any) — MODIFIED to pass the new field shape through.
- `examples/harbor.yaml` — MODIFIED. Document the corrections block + per-profile overrides (commented examples).
- `scripts/smoke/phase-34.sh` — NEW.
- `README.md` Status table — Phase 34 row → Shipped.
- `docs/plans/README.md` master plan — Phase 34 row → Shipped.
- `docs/glossary.md` — add `SchemaSanitizer`, `MessageNormalizer`, `CorrectionsProfile`, `MessageOrderingPolicy`, `SchemaSanitizationMode`, `ReasoningRouting`, `ResponseFormatProfile`.
- `docs/decisions.md` — D-041 entry settling the compose order (corrections outside safety) + the "no `use_native` toggle" stance + `CorrectionsProfile` lives on `ModelProfile`.

## Public API surface

```go
// In internal/llm (types only — logic lives in corrections sub-package).

type ModelProfile struct {
    ContextWindowTokens int
    TokenEstimator      string
    JSONSchemaMode      string
    DefaultMaxTokens    *int
    ReasoningEffort     ReasoningEffort
    CostOverrides       *CostTable
    Corrections         CorrectionsProfile // NEW (Phase 34)
}

type CorrectionsProfile struct {
    MessageOrdering        MessageOrderingPolicy
    SchemaMode             SchemaSanitizationMode
    ReasoningEffortRouting ReasoningRouting
    ResponseFormatShape    ResponseFormatProfile
    UsageBackfillEnabled   bool
}

// Enum types (zero-value = "use Harbor default behaviour").
type MessageOrderingPolicy string
const (
    OrderingDefault            MessageOrderingPolicy = ""
    OrderingSystemFirstStrict  MessageOrderingPolicy = "system_first_strict"  // NIM
)

type SchemaSanitizationMode string
const (
    SchemaDefault        SchemaSanitizationMode = ""
    SchemaOpenAIStrict   SchemaSanitizationMode = "openai_strict"   // adds additionalProperties:false, strict:true
    SchemaPermissive     SchemaSanitizationMode = "permissive"      // strips strict-mode-only fields
)

type ReasoningRouting string
const (
    ReasoningRouteDefault   ReasoningRouting = ""
    ReasoningRouteThinking  ReasoningRouting = "thinking_model"  // o1, o3, deepseek-reasoner: Effort routes to provider-specific param
)

type ResponseFormatProfile string
const (
    ResponseFormatOpenAI       ResponseFormatProfile = ""              // default; {"type":"json_object"} / {"type":"json_schema","json_schema":{...}}
    ResponseFormatJSONOnly     ResponseFormatProfile = "json_only"     // provider rejects json_schema → coerce to json_object
    ResponseFormatAnthropic    ResponseFormatProfile = "anthropic"     // tool-schema-style envelope
)
```

```go
// In internal/llm/corrections.

// Wrap composes Corrections on top of an inner LLMClient. The wrapped
// client applies the five quirks (per-profile) before delegating.
// Wrap is the only entry point; the registry consumes it.
func Wrap(inner llm.LLMClient, cfg llm.ConfigSnapshot) llm.LLMClient

// (Internal types/functions: SchemaSanitizer.Sanitize, MessageNormalizer.Normalize, ProfileFor.)
```

## Test plan

- **Unit:**
  - `corrections_test.go::TestCorrections_MessageReordering_NIM` — registers a `SystemFirstStrict` profile, builds a request with `[user, system, assistant, user]`, asserts the inner client receives `[system, user, assistant, user]`.
  - `sanitizer_test.go::TestSanitizer_OpenAIStrict_AddsRequiredFields` — schema without `additionalProperties` or `strict` → sanitizer adds both.
  - `sanitizer_test.go::TestSanitizer_Permissive_StripsStrictFields` — schema with `strict:true` + `additionalProperties:false` → sanitizer strips both.
  - `corrections_test.go::TestCorrections_ReasoningEffort_ThinkingRouting` — `ReasoningHigh` + `ReasoningRouteThinking` profile → request reaches inner with `ReasoningEffort` cleared + `Extra["reasoning_effort"]="high"` set (provider-specific hint).
  - `corrections_test.go::TestCorrections_ResponseFormat_AnthropicEnvelope` — `Kind: FormatJSONSchema` + `ResponseFormatAnthropic` profile → request reaches inner with `Extra["anthropic_tool_schema"]` populated and `ResponseFormat` cleared.
  - `corrections_test.go::TestCorrections_UsageBackfill_ZeroUsage` — inner returns `Usage{}` (all zeros) + profile has `UsageBackfillEnabled: true` + `CostOverrides` → wrapped response surfaces a non-zero `Usage.PromptTokens` / `Usage.CompletionTokens` computed from request/response byte length.
- **Integration:**
  - Existing Phase 32 tests continue to pass with `Open()` now returning `corrections(safetyClient(driver))`. The mock driver receives post-correction requests; tests that previously asserted shape continue to pass because the default profile is a no-op for the mock model.
- **Conformance:** N/A — Corrections is not a multi-driver subsystem.
- **Concurrency / leak:**
  - `corrections_test.go::TestCorrections_ConcurrentReuse_D025` — 128 concurrent goroutines invoking `Complete` against one shared `Wrap()`-built client. Asserts: no `-race` hits; goroutine baseline restored after the WG drains; each goroutine's mock-side `SeenIdentity` channel sees ONLY its own identity quadruple (no context bleed); each call's `Extra` map is independent (sanitizer never mutates a shared map).

## Smoke script additions

- `scripts/smoke/phase-34.sh`:
  - Runs `go test -race -count=1 -timeout 120s ./internal/llm/corrections/...` — the corrections package's full suite under `-race`.
  - Extends the Phase 32 / 33 static no-tool-call-API symbol guard to `internal/llm/corrections/`.
  - Documents the compose-order invariant: `Open()` returns `corrections(safetyClient(driver))` when `Corrections.Enabled=true`; the smoke prints a SKIP for the not-yet-shipped Phase 35 downgrade chain.

## Coverage target

- `internal/llm/corrections`: 85%.

## Dependencies

- 33 (bifrost integration — corrections sit between Harbor and the bifrost driver, and the bifrost driver's `Extra` map is the channel for some provider-specific hints).

## Risks / open questions

- **Bifrost may already correct some quirks.** Brief 08 §"Phase 34 scope shrinks slightly" notes bifrost's per-provider drivers handle some quirks internally. Harbor's corrections layer applies its own pass; if bifrost has already normalized a field, the corrections pass should be a no-op for that case. The `permissive` `SchemaSanitizationMode` is the escape hatch — for a provider where bifrost adds `additionalProperties:false` itself, the operator configures `Permissive` and Harbor's layer strips it before passing to bifrost. Tested via the sanitizer tests.
- **Schema-shape `Extra` channel.** Some quirks (e.g. Anthropic-style envelopes) push the operator-supplied JSON-schema into `Extra` rather than `ResponseFormat`. This is provider-specific glue; if Phase 33's bifrost driver doesn't recognise the `Extra` key, the bifrost driver swallows it silently. To make this observable, the corrections layer emits an `llm.correction.applied` event (debug-level — operator-tunable cost overhead). Recorded in the phase plan for the implementor's reference; no code in this phase emits the event yet (forward-compat seam: register the type, no emit).
- **Profile lookup race.** The `ModelProfile.Corrections` is read on every `Complete` call; the lookup must be O(1) and lock-free. `cfg.ModelProfiles` is a `map[string]ModelProfile` populated at construction and never mutated; safe for concurrent read.
- **Future quirk additions** (e.g. Vertex-AI-specific message-name handling) extend `MessageOrderingPolicy` / `ResponseFormatProfile` with new enum values. Operators set the enum in `harbor.yaml`; the corrections layer dispatches on the enum. No breaking change.

## Glossary additions

- `SchemaSanitizer` — the per-call JSON-Schema transform that applies `additionalProperties` / `strict` toggles per `ModelProfile.Corrections.SchemaMode`.
- `MessageNormalizer` — the per-call chat-message reorderer that enforces `MessageOrderingPolicy` per profile.
- `CorrectionsProfile` — the per-model bundle of provider-quirk flags. Lives on `ModelProfile.Corrections`.
- `MessageOrderingPolicy` — enum: `default` (no reorder), `system_first_strict` (NIM-style: all system messages first, then alternating user/assistant).
- `SchemaSanitizationMode` — enum: `default` (passthrough), `openai_strict` (adds `additionalProperties:false`+`strict:true`), `permissive` (strips both).
- `ReasoningRouting` — enum: `default` (use bifrost's `Reasoning.Effort` field), `thinking_model` (`o1` / `o3` / `deepseek-reasoner`: clear top-level `ReasoningEffort`, surface in `Extra["reasoning_effort"]`).
- `ResponseFormatProfile` — enum: `openai` (default; OpenAI envelope), `json_only` (provider rejects `json_schema`; coerce to `json_object`), `anthropic` (tool-schema-style envelope in `Extra`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the corrections wrapper does not change identity flow; the underlying safety pass continues to enforce — verified by the concurrent-reuse test's identity-bleed assertion)
- [ ] **If this phase builds a reusable artifact** (the corrections wrapper is one): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam**: an integration test exists. The corrections layer consumes the LLM subsystem's `LLMClient` surface (Phase 32) and the bifrost driver (Phase 33) — registry-path tests in `corrections_test.go` cover the wiring with the mock driver; the existing `internal/llm` tests continue to pass against the new `Open()` shape.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
