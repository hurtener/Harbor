# Phase 35 — Structured output strategies + downgrade chain

## Summary

Add Harbor's `OutputMode` enum (`Native` / `Tools` / `Prompted`) and the
per-provider downgrade chain `json_schema → json_object → text` that fires on
`invalid_json_schema`-class errors. Phase 32's `llm.mode_downgraded` event seam
gets its first publisher. The downgrade wrapper composes OUTSIDE Phase 34's
corrections so each downgraded request sees the corrections layer with the new
`ResponseFormat` shape (D-043 settles the order).

## RFC anchor

- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- brief 03 §6 ("Structured output strategies"): `OutputMode = Native | Tools | Prompted` is the per-provider knob; the downgrade chain is the safety net when the model misbehaves. Phase 35 ships exactly this.
- brief 07 (the elegance principle): `OutputMode.Tools` is a *Harbor-side prompting strategy*, NOT a passthrough to provider-native tool-calling APIs. The runtime owns tool dispatch (RFC §6.4); the LLM is asked to emit a JSON object shaped like a tool call as plain output. The Phase 32/33 static guard against `Tools` / `ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec` symbols extends to this package.
- brief 08 §3 ("validation matrix"): per-provider downgrades observed empirically — some OpenRouter routes reject `json_schema`, fall back to `json_object`; some prompts that fail in `json_object` succeed when rendered as text with schema instructions. The chain `json_schema → json_object → text` is the operationally-validated order.

## Findings I'm departing from (if any)

None.

## Goals

- Ship `OutputMode` enum at `llm` package level (typed; promotes the placeholder string `JSONSchemaMode` to a real enum).
- Ship per-`OutputMode` request shaping: `Native` (pass `FormatJSONSchema` through), `Tools` (wrap schema into a JSON envelope describing a synthetic "respond" tool call as prompted output), `Prompted` (use `FormatJSONObject` with the schema inlined in the system prompt).
- Ship the downgrade chain `Native → Prompted → Text` (max 3 steps including the initial attempt). Each downgrade emits `llm.mode_downgraded` with identity quadruple + From/To/Reason.
- Provide a hook `RegisterDowngradeWrapper` (mirrors Phase 34's pattern) so `internal/llm/output` self-registers via `init()` and `cmd/harbor/main.go` blank-imports it.
- Per-known-provider defaults: OpenAI / Anthropic → `Native`; NIM / custom-OpenAI-compatible → `Prompted`; deepseek-reasoner → `Prompted`. Operator override always wins.
- Settle the compose order — `downgrade(corrections(safetyClient(driver)))`. New D-043 entry records the reasoning.

## Non-goals

- Provider-native tool-calling APIs. Phase 35's `OutputMode.Tools` is a prompted-output strategy.
- Detection of provider-specific schema-error wire shapes (the wrapper classifies via `IsInvalidJSONSchemaError(err)` which inspects error wrapping + a small allowlist of common substrings; provider-specific deeper detection lives in the driver layer as a §17.6 follow-up).
- Auto-cascade recovery beyond the documented three-step chain (post-V1 per RFC §6.5 / D-026).
- Phase 36's retry-with-feedback (different primitive — Phase 36 ships separately).

## Acceptance criteria

- [ ] `OutputMode` enum defined (`OutputModeNative`, `OutputModeTools`, `OutputModePrompted`) at `internal/llm`.
- [ ] `ModelProfile.OutputMode` field defined; `JSONSchemaMode` (string placeholder) remains for backward compat with the loader, normalised to `OutputMode` at snapshot time.
- [ ] Downgrade chain fires on `invalid_json_schema`-class errors (max 3 steps; final text mode surfaces error chain).
- [ ] `llm.mode_downgraded` event emits at every downgrade with identity quadruple + From/To/Reason fields populated.
- [ ] Per-known-provider default `OutputMode` (OpenAI / Anthropic → Native; NIM / openai-compat → Prompted; deepseek-reasoner → Prompted); operator override works.
- [ ] **D-025 concurrent-reuse** test: N≥100 concurrent `Complete` calls against ONE shared downgrade wrapper.
- [ ] **No tool-call APIs** leak into `internal/llm/output/...` — static guard in `scripts/smoke/phase-35.sh` extends to the new package.
- [ ] Coverage on `internal/llm/output`: ≥ 85%.
- [ ] `scripts/smoke/phase-35.sh` green; `make preflight` green; existing Phase 32–34 + 33a tests still pass.

## Files added or changed

- `internal/llm/output/output.go` — NEW: `OutputMode` enum + wrapper interface + compose hook.
- `internal/llm/output/downgrade.go` — NEW: `downgradeClient` wrapper + chain logic + classifier.
- `internal/llm/output/output_test.go`, `downgrade_test.go`, `d025_test.go` — NEW.
- `internal/llm/llm.go` — MODIFIED: add `OutputMode` enum + `ModelProfile.OutputMode` field.
- `internal/llm/errors.go` — MODIFIED: add `ErrInvalidJSONSchema` + `ErrDowngradeExhausted`.
- `internal/llm/events.go` — MODIFIED: `ModeDowngradedPayload` already declared by Phase 32; no shape change required.
- `internal/llm/registry.go` — MODIFIED: add `RegisterDowngradeWrapper` + compose in `Open`.
- `internal/llm/corrections/profiles.go` — MODIFIED: add default `OutputMode` per model-name prefix.
- `internal/config/config.go` — MODIFIED: extend `LLMModelProfileConfig.JSONSchemaMode` documentation (no schema break).
- `internal/config/validate.go` — MODIFIED: validate new enum values.
- `cmd/harbor/main.go` — MODIFIED: blank-import `internal/llm/output`.
- `examples/harbor.yaml` — MODIFIED: document `json_schema_mode` accepted values; commented per-provider defaults.
- `docs/plans/phase-35-structured-output.md` — NEW (this file).
- `docs/plans/README.md` — MODIFIED: flip Phase 35 row to `Shipped`.
- `README.md` — MODIFIED: status table.
- `docs/decisions.md` — NEW D-043 entry: downgrade-outside-corrections compose order + `OutputMode.Tools` semantics.
- `docs/glossary.md` — NEW entries: `OutputMode`, `DowngradeChain`.
- `scripts/smoke/phase-35.sh` — NEW.

## Public API surface

- `llm.OutputMode` enum + constants.
- `llm.ModelProfile.OutputMode` field.
- `llm.ErrInvalidJSONSchema`, `llm.ErrDowngradeExhausted` sentinels.
- `llm.IsInvalidJSONSchemaError(err) bool` classifier.
- `llm.RegisterDowngradeWrapper(fn func(LLMClient, ConfigSnapshot, Deps) LLMClient)` hook.
- `output.Wrap(inner, cfg, deps) LLMClient`.

## Test plan

- **Unit:** per-mode happy path; downgrade Native → Prompted → Text with classifier triggering; final-step error contains chain history; classifier matches expected substrings + wrapped sentinels.
- **Integration:** in-package adapter test that wires a recording driver as inner and asserts downgraded request shape matches the new `ResponseFormat`.
- **Conformance:** N/A — single wrapper, not a driver triad.
- **Concurrency / leak:** D-025 stress N≥128 against one shared wrapper; identity propagation through chain.

## Smoke script additions

- Build clean.
- Run `internal/llm/output/...` tests under `-race`.
- Static guard: no provider-native tool-calling symbols (`ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec`) appear in `internal/llm/output/`.

## Coverage target

- `internal/llm/output`: 85%.

## Dependencies

- 33 (bifrost integration — supplies the cost/usage payloads our downgrade wrapper observes).
- 34 (provider corrections — the downgrade composes OUTSIDE corrections; D-043 settles).

## Risks / open questions

- Schema-error detection is currently substring-based plus sentinel-wrap. Provider-specific wire shapes (e.g. structured 422 responses with codes) could improve precision; deferred to the wave-end E2E audit.
- The `OutputMode.Tools` shape encodes the schema into a synthetic envelope name `respond_with` — the prompt template is held constant in Phase 35; tuning is post-V1.

## Glossary additions

- `OutputMode` — `Native` / `Tools` / `Prompted` request-shaping strategy.
- `DowngradeChain` — the `Native → Prompted → Text` retry-on-schema-error sequence.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no multi-isolation surface changes).
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes (N≥100, single shared instance, `-race`).** YES — the downgrade wrapper is a compiled artifact; `d025_test.go` covers it.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** YES — adapter test against the corrections wrapper composed below.
- [ ] If new vocabulary: glossary updated — YES.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A.
