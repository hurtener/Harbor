# Phase 255 — provider-neutral descriptors and runtime-origin model discovery (HA-71)

## Summary

Deliver the first Harbor-owned technical contract for provider setup facts,
runtime-origin validation, and normalized model discovery. The contract is
provider-neutral and deliberately stops at facts the Bifrost integration can
prove: credential modes and required technical fields, custom-endpoint
support, bounded validation/discovery capability, model limits/modalities/tool
and reasoning signals, and pricing provenance.

The phase adds two consumers. `harbor llm providers` lists technical
descriptors locally and, for explicit probes, uses the configured account
without booting Runtime/EventBus; it reports `runtime_origin=false`. The
booted runtime exposes the same bounded validation/discovery through the
existing protected `llm.posture` request envelope for admin-tier callers,
using the runtime's shared credential holder (including broker-pulled
credentials) and reporting `runtime_origin=true`. No new Protocol method or
version is needed. A custom endpoint is queried through Bifrost when it
exposes model discovery; its configured model list remains an explicit manual
fallback when discovery is unavailable or empty. No provider response body,
endpoint credential, or presentation metadata crosses the Harbor contract.

## RFC anchor

- RFC §3 (architecture overview and seam boundaries)
- RFC §6.5 (LLM client and provider integration)
- RFC §6.15 (governance/provider boundary)
- RFC §8 (CLI)

## Briefs informing this phase

- brief 03 — Tools, Integrations, and LLM Client
- brief 08 — LLM client validation (Bifrost)
- brief 06 — Events, observability, and devx (operator evidence)

## Brief findings incorporated

- Provider technical capability and consumer presentation metadata are
  different ownerships. Harbor exposes IDs, field kinds, support states, and
  bounded operation facts; a downstream control plane supplies labels, logos,
  and help copy.
- Bifrost's model response contains useful normalized fields but also an
  opaque `ProviderExtra` and key-level error messages. The adapter copies only
  the bounded neutral subset and reduces key failures to a count.
- Missing model limits/modalities/reasoning signals are `unknown`, not
  `unsupported`; absent pricing is `unpriced`; configured custom models are
  `manual`; incomplete pages/key results are `partial`; cached stale results
  have an explicit `stale` outcome. The adapter never guesses a provider fact
  from a model name.

## Findings I'm departing from (if any)

None. The phase keeps provider presentation metadata outside Harbor, uses the
existing Bifrost account/list-model seam rather than adding a parallel
provider SDK, and extends the existing `llm.posture` envelope rather than
adding a new Protocol method.

## Goals

- Define an immutable, concurrently reusable descriptor/catalog contract in
  `internal/llm/provider` with stable support states and redacted outcomes.
- Describe every Bifrost standard provider's credential-mode posture without
  leaking keys or provider-specific presentation metadata. Keyless-capable
  providers advertise `none` in addition to `api_key`.
- Describe declared OpenAI-compatible providers with explicit secret/base-URL
  fields, supported custom-endpoint capability, runtime validation, and
  runtime discovery with an explicit manual model-catalog fallback.
- Reuse Bifrost's real `Account` and `ListModelsRequest` path for validation and
  discovery. Bound page size/pages, honor cancellation, reject malformed or
  duplicate model rows, and map provider failures to stable safe outcomes.
- Normalize only provider facts that are present: context/input/output limits,
  modalities, tool support, canonical reasoning-effort signals, deprecation,
  and pricing provenance.
- Ship `harbor llm providers` as a thin operator consumer with stable JSON and
  human output. Static listing and explicit CLI probes never boot a runtime
  and report `runtime_origin=false`.
- Project the same descriptor/validation/discovery contract through the
  existing protected `llm.posture` envelope, with admin-tier authorization,
  shared runtime credential state, and `runtime_origin=true` only from a
  booted runtime.
- Cover supported, unsupported, manual, partial, stale, unpriced, malformed,
  custom-endpoint, cancellation, secret-redaction, and concurrent reuse
  behavior.

## Non-goals

- Provider logos, display names, help copy, consumer aliases, policy
  profiles, quotas, allowance enforcement, or billing truth.
- A new Protocol method or wire-type version. The runtime-origin projection
  uses the already-shipped protected `llm.posture` surface; the offline CLI
  remains explicitly non-runtime-origin.
- Direct HTTP/provider SDK calls outside Bifrost, exposing raw provider errors,
  returning provider response bodies, returning API keys or environment
  variable names, or claiming invoice pricing.
- Treating a manual model list as discovered, treating missing metadata as
  unsupported, or silently substituting guessed capabilities.
- Fleet configuration, downstream databases, deployment, release tagging, or
  local preflight.

## Release-candidate evidence (2026-08-23)

Implementation PR #738 was merged at
`d9bf28fe703e10eb9f995657f4ac52949aa57e04` (tree
`72f8093049a3f7bc952d8d3e0decdd8d02ea7744`). Hosted candidate run
`32670321270` completed successfully on PR head
`1da7845326088e451bcf19970136a62b8e274e5a` (the same tree), including the full
preflight. Post-merge main run `32673186738` also completed successfully on
merged commit `d9bf28fe703e10eb9f995657f4ac52949aa57e04`, including full
preflight.
The annotated `v1.30.0` tag object is
`53c388028f1150c9afb6263332583d319c3ba544` and peels to
`466b307c563f8193950ac5abef36677e48b1bae8`. Release workflow `32683661507`
completed successfully and published 13 assets with verified aggregate
`checksums.txt`, six sidecar checksums, and six GitHub attestations. Public
module provenance records `Sum=h1:9MfAk67WbACqvXnwSMMv0WYonE+S0fV5Y7wcuhwNo8o=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=466b307c563f8193950ac5abef36677e48b1bae8`, and
`Origin.Ref=refs/tags/v1.30.0`. The native darwin/arm64 artifact reports
Harbor v1.30.0, Protocol 0.1.0, build
`466b307c563f8193950ac5abef36677e48b1bae8`. The post-tag scaffold pin and
golden cleanup is included in this follow-up. Operator-approved provider
acceptance and downstream runtime/database acceptance remain pending.

## Contract shape

`ProviderDescriptor` carries an opaque provider ID/kind, credential modes,
secret/url/text field kinds, custom-endpoint state, and bounded
runtime-origin validation/discovery capability. `Catalog.Validate` performs a
single bounded probe. `Catalog.Discover` pages at most 20 times with a maximum
page size of 1,000, normalizes rows, and refuses duplicate or invalid limits.

`Outcome` is content-free and uses a fixed message vocabulary. Provider
adapters map status/type to stable codes such as
`provider_credential_rejected`, `provider_endpoint_unavailable`,
`model_discovery_unsupported`, `model_discovery_partial`,
`provider_reply_malformed`, and `provider_timeout`. No adapter error string is
serialized.

`ModelCapabilities` records each fact with a state. The `reasoning_effort`
parameter maps to Harbor's canonical `off|low|medium|high` set; otherwise
reasoning remains `unknown`. Pricing is `provider_reported` or `unpriced`,
without exporting a provider rate table in this prerequisite.

## Acceptance criteria

- [x] Static `harbor llm providers --json` lists technical descriptors without
      secrets, endpoint values, or response bodies.
- [x] Standard providers distinguish API-key and keyless-capable modes;
      native custom-endpoint handling remains conservative.
- [x] Declared custom providers expose supported custom endpoint and runtime
      discovery facts, falling back to a manual catalog without exposing their
      configured URL, env-var name, or model list as discovered provider data.
- [x] Runtime-origin validation/discovery reuses the Bifrost account setup,
      is bounded/cancellable, closes cleanly, and the protected Protocol path
      uses the booted runtime; the CLI path never boots Runtime/EventBus and
      reports `runtime_origin=false`.
- [x] The existing protected `llm.posture` envelope exposes `validate` and
      `discover` only to admin-tier callers and projects sanitized descriptor,
      capability, outcome, and model facts.
- [x] Normalized model limits, modalities, tools, canonical reasoning,
      deprecation, and pricing provenance preserve unknown/unpriced facts.
- [x] Unsupported, unavailable, credential-rejected, partial, stale, empty,
      malformed, duplicate, and cancellation outcomes are explicit and safe.
- [x] Generic catalog and Bifrost adapter tests pass under `-race`; the
      concurrent-reuse test exercises 100 discovery calls.
- [x] CLI help/golden and static smoke coverage identify the shipped consumer.
- [x] Post-merge main CI run `32673186738` is green on merged commit
      `d9bf28fe703e10eb9f995657f4ac52949aa57e04`, including full preflight.
- [ ] Operator-approved provider acceptance and downstream runtime/database
      acceptance remain separate release follow-up gates; the immutable
      v1.30.0 release and post-tag scaffold cleanup are recorded above.

## Files added or changed

- `internal/llm/provider/catalog.go` and `catalog_test.go`
- `internal/llm/drivers/bifrost/provider_catalog.go` and focused tests
- `cmd/harbor/cmd_llm_provider.go`, CLI tests, root help, and help golden
- `internal/protocol/posture.go`, `internal/protocol/types/llm.go`, and
  runtime/serve wiring for the protected runtime-origin projection
- `docs/decisions.md` (D-435)
- `docs/notes/downstream-asks.md` (HA-71)
- `docs/glossary.md`
- `docs/plans/README.md`
- `scripts/smoke/phase-255.sh`
- `README.md`, `CHANGELOG.md`

## Public API surface

The first consumer is the operator command:

```text
harbor llm providers [--provider <id>] [--validate|--discover]
```

The reusable Go contract also feeds the existing protected `llm.posture`
request envelope. A request supplies `provider_operation=validate|discover`
and is admitted only for admin-tier scopes; no new method or Protocol version
is introduced. The offline CLI path remains explicitly non-runtime-origin.

## Test plan

- **Unit:** descriptor validation, stable outcomes, bounded page shape,
  normalized capabilities, manual fallback, malformed/duplicate rows, and
  provider-error redaction.
- **Integration:** Bifrost adapter mapping, CLI descriptor/probe wiring, and
  the protected `llm.posture` projection; the CLI uses the same account
  construction without booting Runtime/EventBus, while the Protocol path uses
  the booted runtime's shared credential holder.
- **Conformance:** supported, unsupported, custom-endpoint, manual,
  partial/stale, unpriced, empty, and unavailable provider states.
- **Concurrency / leak:** 100 concurrent calls against one immutable Catalog,
  cancellation before source invocation, and safe Bifrost client close.
- **Hosted:** release CI and an operator-approved runtime-origin probe remain
  open evidence; no credentialed provider probe is claimed locally.

## Smoke script additions

`scripts/smoke/phase-255.sh` checks the phase plan, D-435/HA-71 registration,
the provider/catalog and Bifrost adapter files, explicit support states,
redaction tests, the CLI consumer, and unreleased documentation. It performs
no provider call or database mutation.

## Coverage target

- `internal/llm/provider`: all catalog branches covered by focused race tests;
  concurrent reuse is exercised with 100 calls.
- `internal/llm/drivers/bifrost`: descriptor, mapping, and stable-error
  adapter paths plus concurrent catalog reuse (100 calls) covered by focused
  race tests.
- `cmd/harbor`: CLI help, static JSON, invalid invocation, and config failure
  paths covered by the package suite.

## Dependencies

- Existing LLM/Bifrost integration from Phases 03, 08, 25, 33, and 33a.
- Existing CLI/config seams from Phase 08 and the current root command.
- D-018/D-025/D-333/D-334 and the provider boundary in RFC §6.5/§6.15.

## Risks / open questions

- Provider model-list APIs differ in pagination and metadata quality. The
  adapter therefore bounds pages, rejects malformed shape, and preserves
  unknown/unpriced/manual facts instead of guessing.
- A custom endpoint may not expose `/models`; a configured model list is
  returned only as an explicit manual fallback and never as discovered data.
- The runtime projection uses the existing admin-tier posture authority and
  bounded request envelope; future caching/freshness remains a consumer
  concern and must not turn an offline CLI probe into runtime-origin evidence.

## Glossary additions

- **Provider descriptor:** provider-neutral technical setup and operation facts.
- **Runtime-origin provider validation:** a bounded Bifrost probe issued from
  the runtime's configured provider account.
- **Normalized model capability:** an explicitly supported, unknown, manual,
  or unpriced provider model fact.

## Pre-merge checklist

- [x] Focused provider/Bifrost race tests pass.
- [x] CLI package tests, vet, diff check, and phase smoke pass.
- [x] `AGENTS.md` and `CLAUDE.md` remain identical.
- [x] Cross-references and glossary entries are present.
- [x] Static CLI consumer check passes without credentials.
- [ ] Independent adversarial review and hosted CI remain release gates.
- [ ] Local `make preflight` is intentionally not run for this candidate.

## Verification

Focused local gates are:

```text
go test -race ./internal/llm/provider
go test -race ./internal/llm/drivers/bifrost
go test ./cmd/harbor
bash scripts/smoke/phase-255.sh
go vet ./internal/llm/provider ./internal/llm/drivers/bifrost ./cmd/harbor
```

The static CLI query is the non-network live consumer check. A configured
runtime-origin probe must be run only with operator-approved credentials and
is not part of this repository candidate's evidence.
