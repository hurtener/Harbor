# Phase 33a — Custom OpenAI-compatible providers + per-provider timeouts

## Summary

Extend Phase 33's bifrost driver so operators can configure any OpenAI-compatible LLM endpoint (NVIDIA NIM, vLLM, ollama, lm-studio, third-party gateways) via `harbor.yaml` without per-provider Go code. Adds first-class per-provider timeout/retry/backoff/concurrency knobs that high-latency providers (NIM in particular) require. The mechanism plugs into bifrost's `schemas.CustomProviderConfig` surface — Harbor exposes the operator-tunable subset; bifrost handles the wire-level routing.

## RFC anchor

- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 08

## Brief findings incorporated

- **brief 08 §"Architecture":** "bifrost ships a queue + pool plus per-provider drivers" — Phase 33a leans on the queue/pool unchanged; only the `Account` configuration surface widens to expose custom providers.
- **brief 08 §"Cancellation caveat":** Phase 33 already abandons the chunk reader on `ctx.Done()`. Phase 33a inherits the pattern; the per-provider timeout knob complements (not replaces) ctx-driven cancellation.
- **brief 03 §4 ("NIM rejects mid-thread system"):** Phase 34 ships the `OrderingSystemFirstStrict` quirk normalizer keyed on the `nim/*` model prefix. Phase 33a documents that operators configuring a custom NIM provider get the quirk applied via `ModelProfile.Corrections.MessageOrdering` (explicit), not via prefix (since OpenAI-compatible model names are typically unprefixed).
- **brief 03 §"Provider catalog":** Listed providers (OpenAI / Anthropic / Cohere / Mistral / NIM / etc.) anchored the native list; Phase 33a's custom-provider mechanism is the seam for the long-tail providers brief 03 acknowledges but doesn't enumerate.

## Findings I'm departing from (if any)

None — Phase 33a is additive. The directive's "GetConfiguredProviders returns native + custom mixed" framing was tightened to "GetConfiguredProviders returns the single configured provider (which can be either native or custom)" so D-040's "single-provider per Harbor instance" still holds. The `CustomProviders` list registers candidate names; the operator's `llm.provider` field selects the one that actually runs. Multi-provider routing is a future extension that bifrost already supports — Phase 33a opens the seam without committing to multi-routing semantics in this PR.

## Goals

- Operators wire NIM (or any OpenAI-compatible endpoint) without touching Go code.
- High-latency providers get per-provider `Timeout` / `MaxRetries` / `RetryBackoffInitial` / `RetryBackoffMax` config that survives `make preflight` (no hardcoded driver-side defaults).
- Phase 33's native-provider path (OpenAI, OpenRouter, Anthropic, etc.) is unchanged at runtime.
- The corrections layer (Phase 34) continues to fire on operator-configured `ModelProfile` entries when the operator declares them.
- The seam is extensible: future Phase 33b/33c can widen `BaseProviderType` to non-OpenAI shapes; Phase 33a only supports OpenAI-compatible.

## Non-goals

- Multi-provider routing within a single Harbor instance (D-040 single-primary still holds; operator picks ONE provider via `llm.provider`).
- Non-OpenAI base types (Anthropic-compat, Cohere-compat, etc.) — Phase 33a only accepts `base_provider_type: openai` (default).
- Live NIM calls — gated; the wave-end E2E exercises live NIM behind `HARBOR_LIVE_LLM=1`.
- Hot-reload of provider config — restart-required per AGENTS.md §10.
- Adding new corrections quirks — Phase 34's 5 quirks cover Phase 33a's needs.

## Acceptance criteria

- [ ] `LLMConfig.CustomProviders []LLMCustomProviderConfig` + `LLMConfig.NetworkDefaults LLMNetworkDefaults` land in `internal/config/config.go`.
- [ ] `internal/config/validate.go` rejects: missing custom-provider `Name`, missing `BaseURL`, missing `APIKeyEnvVar`, empty `Models`, unknown `BaseProviderType` (only `""` / `"openai"` accepted at Phase 33a), `llm.provider` referencing neither a native bifrost provider nor a declared custom-provider name, two custom providers with the same `Name`, network defaults with negative durations.
- [ ] When `llm.provider` names a custom provider, `llm.api_key` / `llm.base_url` / `llm.timeout` are NOT required at the legacy layer — the custom-provider entry's `api_key_env_var` / `base_url` / `timeout` fill in.
- [ ] When `llm.provider` names a native bifrost provider, the existing Phase 33 validation still applies.
- [ ] `internal/llm.ConfigSnapshot` gains `CustomProviders []CustomProviderSpec` + `NetworkDefaults NetworkDefaults`; the config-loader translation populates them.
- [ ] `internal/llm/drivers/bifrost.Account` resolves the primary provider's config from either the native path (legacy fields) or the custom-provider entry, falling back to `NetworkDefaults` for fields the per-provider config leaves zero.
- [ ] For a custom provider, `GetConfigForProvider` returns a `*schemas.ProviderConfig` whose `CustomProviderConfig.BaseProviderType = schemas.OpenAI` and `Network.BaseURL` / `Network.DefaultRequestTimeoutInSeconds` / etc. reflect the operator config.
- [ ] Missing `APIKeyEnvVar` value at construction time → `ErrMissingAPIKey` (existing sentinel) with the env var named in the error.
- [ ] **Concurrent-reuse test (D-025)** N≥100 concurrent `Complete` calls against ONE shared driver with one native + one custom provider configured under `-race`. No data races, no goroutine leaks, no context bleed.
- [ ] **Integration test** spins up an `httptest.Server` that mimics an OpenAI-compatible `/v1/chat/completions` endpoint and exercises: happy path, 5xx retry, timeout.
- [ ] No tool-call API references introduced — Phase 33's static guard still passes.
- [ ] `examples/harbor.yaml` documents a commented NIM stanza pointing at `https://integrate.api.nvidia.com/v1` with a 180-second timeout default and a comment explaining the latency rationale.
- [ ] `docs/decisions.md` ships a D-NNN entry settling the base-provider-type, per-provider-vs-default fallthrough order, and `APIKeyEnvVar` resolution form.
- [ ] `docs/glossary.md` adds `CustomProvider`, `BaseProviderType`, `ProviderEndpoint`, `NetworkDefaults`.
- [ ] `make preflight` green. `scripts/smoke/phase-33.sh` extended to assert custom-provider registration is resolvable via `internal/llm.Open(...)`.
- [ ] Coverage on `internal/llm/drivers/bifrost`: ≥ 80% (unchanged target from Phase 33).
- [ ] Phase 34's `corrections/profiles.go` `nim/*` lookup verified — no change required, but operators using custom NIM should declare a `ModelProfiles[<unprefixed-model>].Corrections` entry; the yaml example demonstrates this.

## Files added or changed

- `internal/config/config.go` — `LLMCustomProviderConfig`, `LLMNetworkDefaults` types; `LLMConfig` gains `CustomProviders`, `NetworkDefaults` fields.
- `internal/config/validate.go` — `validateLLM` widened to validate custom-provider list + cross-check `llm.provider` against (native ∪ custom).
- `internal/config/testdata/harbor.yaml` — fixture grows a `custom_providers` block (kept commented so the legacy fixture still loads).
- `internal/llm/registry.go` — `ConfigSnapshot` gains `CustomProviders []CustomProviderSpec` + `NetworkDefaults NetworkDefaults` fields; `CustomProviderSpec` + `NetworkDefaults` types defined in `internal/llm/llm.go` or `registry.go`.
- `internal/llm/drivers/bifrost/account.go` — `newAccount` extended to read the custom-provider entry when `cfg.Provider` matches a custom-declared name; `GetConfigForProvider` returns `CustomProviderConfig` when applicable; `NetworkDefaults` fallback wired for every field; `isKnownProvider` lookup updated.
- `internal/llm/drivers/bifrost/account_test.go` — new tests covering custom-provider construction + validation paths.
- `internal/llm/drivers/bifrost/custom_provider_test.go` — NEW. Integration test via `httptest.Server` for happy / 5xx / timeout paths.
- `internal/llm/drivers/bifrost/concurrent_test.go` — extended (or new test file) for the D-025 concurrent-reuse stress against a mixed config.
- `docs/plans/phase-33a-custom-providers.md` — THIS FILE.
- `docs/decisions.md` — new D-NNN entry.
- `docs/glossary.md` — `CustomProvider`, `BaseProviderType`, `ProviderEndpoint`, `NetworkDefaults`.
- `docs/plans/README.md` — Phase 33 row notes mention the Phase 33a extension; new Phase 33a row added to the index for visibility.
- `README.md` — Status table gains a Phase 33a row pointing to the new plan + section text mentions custom-provider support.
- `examples/harbor.yaml` — commented NIM stanza + network-defaults documentation.
- `scripts/smoke/phase-33.sh` — extended assertion (or `scripts/smoke/phase-33a.sh` — see Smoke section below for the choice).

## Public API surface

```go
package config

type LLMCustomProviderConfig struct {
    Name                 string            `yaml:"name"`
    BaseURL              string            `yaml:"base_url"`
    APIKeyEnvVar         string            `yaml:"api_key_env_var"`
    Models               []string          `yaml:"models"`
    BaseProviderType     string            `yaml:"base_provider_type,omitempty"` // default "openai"
    Timeout              time.Duration     `yaml:"timeout,omitempty"`
    MaxRetries           int               `yaml:"max_retries,omitempty"`
    RetryBackoffInitial  time.Duration     `yaml:"retry_backoff_initial,omitempty"`
    RetryBackoffMax      time.Duration     `yaml:"retry_backoff_max,omitempty"`
    Concurrency          int               `yaml:"concurrency,omitempty"`
    BufferSize           int               `yaml:"buffer_size,omitempty"`
    RequestPathOverrides map[string]string `yaml:"request_path_overrides,omitempty"`
}

type LLMNetworkDefaults struct {
    Timeout             time.Duration `yaml:"timeout,omitempty"`
    MaxRetries          int           `yaml:"max_retries,omitempty"`
    RetryBackoffInitial time.Duration `yaml:"retry_backoff_initial,omitempty"`
    RetryBackoffMax     time.Duration `yaml:"retry_backoff_max,omitempty"`
    Concurrency         int           `yaml:"concurrency,omitempty"`
    BufferSize          int           `yaml:"buffer_size,omitempty"`
}

package llm

type CustomProviderSpec struct {
    Name                 string
    BaseURL              string
    APIKeyEnvVar         string
    Models               []string
    BaseProviderType     string
    Timeout              time.Duration
    MaxRetries           int
    RetryBackoffInitial  time.Duration
    RetryBackoffMax      time.Duration
    Concurrency          int
    BufferSize           int
    RequestPathOverrides map[string]string
}

type NetworkDefaults struct {
    Timeout             time.Duration
    MaxRetries          int
    RetryBackoffInitial time.Duration
    RetryBackoffMax     time.Duration
    Concurrency         int
    BufferSize          int
}
```

## Test plan

- **Unit:**
  - `internal/config/validate_test.go`: custom-provider validation (missing fields, duplicate names, unknown base type, network-defaults negative durations, `llm.provider` cross-check against native ∪ custom).
  - `internal/llm/drivers/bifrost/account_test.go`: `newAccount` resolves a custom-provider primary; `GetConfigForProvider` returns `CustomProviderConfig.BaseProviderType = schemas.OpenAI`; per-provider knobs override `NetworkDefaults`; defaults flow through to provider config when zero.
- **Integration:**
  - `internal/llm/drivers/bifrost/custom_provider_test.go`: `httptest.Server` mimicking `/v1/chat/completions` — happy path, 5xx retry, timeout (server sleeps longer than per-provider timeout → driver gives up cleanly).
- **Conformance:**
  - `internal/llm/drivers/bifrost/conformance_test.go` (existing): unchanged path — live tests still gated behind `HARBOR_LIVE_LLM=1`. Live NIM smoke runs in the wave-end E2E.
- **Concurrency / leak:**
  - `internal/llm/drivers/bifrost/concurrent_test.go` (extended): N≥100 concurrent `Complete` calls against ONE shared driver configured with a custom provider; assert no races / no leaks / no context bleed. Goroutine baseline restored after teardown.

## Smoke script additions

`scripts/smoke/phase-33.sh` extended (preferred — the smoke is tied to the bifrost driver phase, and 33a is a feature extension, not a separate-named phase in the master plan):

- Build / vet / race-test the bifrost driver package (existing).
- Add an assertion that a `ConfigSnapshot` with a custom-provider entry resolves via `internal/llm.Open(...)` to a non-nil client (uses the existing stub-client test harness so the smoke stays mock-only — no live calls).

The `phase-33-extension` block in the smoke is documented with a one-line comment so a future auditor sees the addition without hunting.

## Coverage target

- `internal/llm/drivers/bifrost`: ≥ 80% (Phase 33's target; Phase 33a maintains it across the larger surface).
- `internal/config`: existing target (Phase 02) preserved.

## Dependencies

- 33 (bifrost driver — extended here, not replaced)

## Risks / open questions

- **Bifrost API surface drift.** The `CustomProviderConfig` types in v1.5.8 match brief 08's report; verified directly against `~/go/pkg/mod/github.com/maximhq/bifrost/core@v1.5.8/schemas/provider.go`. No drift.
- **OpenAI-compatible endpoints with non-standard paths.** Some providers (older NIM versions, third-party gateways) host `/chat/completions` at the root (no `/v1/` prefix). `LLMCustomProviderConfig.RequestPathOverrides` exposes bifrost's `RequestPathOverrides` map; operators set it explicitly when needed.
- **NIM latency.** A 180-second default timeout is suggested in the yaml example with a comment. The cost of being too generous is a slower fail; the cost of being too tight is false failures on cold-start NIM models. The 180s number is conservative but operator-tunable.
- **Corrections / model-prefix matching.** Phase 34's `defaultProfileFor` matches model name prefixes (e.g., `nim/*`). Custom-provider model names are typically UNPREFIXED (`google/gemma-4-31b-it`). Operators using custom providers SHOULD declare `ModelProfiles[<model>].Corrections` explicitly to get the quirk. The yaml example demonstrates the pattern. A future §17.6 hardening could make corrections look up by `cfg.Provider` when no model-prefix match — out of scope for Phase 33a.
- **Single-primary vs multi-routing.** D-040 settled single-primary for Phase 33. Phase 33a preserves the contract by registering only the configured primary in `GetConfiguredProviders` even though `CustomProviders` may declare multiple candidate entries. A future phase can widen the surface; the seam is ready.

## Glossary additions

- `CustomProvider` — an operator-declared LLM provider whose endpoint is OpenAI-compatible but is not on bifrost's native provider list. Configured under `llm.custom_providers` in `harbor.yaml`.
- `BaseProviderType` — the wire-protocol family a custom provider emulates. Phase 33a supports only `openai`.
- `ProviderEndpoint` — the URL bifrost POSTs requests to for a given provider. For custom providers, the operator supplies it via `BaseURL`.
- `NetworkDefaults` — the operator-tunable default timeout / retry / backoff / concurrency / buffer-size values bifrost applies when a provider doesn't override them.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (no new identity paths; existing tests cover)
- [x] **Concurrent-reuse test passes** — N≥100 concurrent invocations against a single shared instance under `-race`. See AGENTS.md §5 + §11 + D-025.
- [x] **Integration test exists** — `httptest.Server` happy / 5xx / timeout paths. AGENTS.md §17.
- [x] Glossary updated
- [x] Brief findings departure (none) — explicit in "Findings I'm departing from"
