# Phase 256 — public external-grant SDK and runtime-default route compatibility (D-436)

## Summary

This hotfix closes the first external-consumer composition gap exposed by the
v1.30.0 external execution grant. The generic Harbor contract is public and
nameable from a package outside `internal/`; it does not add a Protocol method,
HTTP product surface, provider catalog, or downstream policy vocabulary.

The public SDK exposes the grant/configuration, route mode, signer/verifier,
receipt delivery, canonical receipt JSON/hash, attempt identity, and
receipt-to-grant validation seams. A second consumer can compose a signed grant
and implement receipt delivery without copying Harbor's internal structs or
re-deriving canonical identity rules.

## RFC anchor

RFC §6.5 (provider-neutral LLM execution), §6.11 (governance), and §6.15
(usage/cost accounting). This hotfix is additive and does not change the
Protocol surface.

## Briefs informing this phase

- Brief 03: provider-neutral LLM execution and configured provider routing.
- Brief 06: content-free telemetry, cost, and bounded observability.
- Brief 08: configuration, SDK composition, and fail-loud validation.

### Route modes

`coordinator_bound` is the v1.30.0 shape: the signed grant carries the provider,
model, route, connection generation, and opaque credential-binding generation;
the runtime resolves the credential only after verification. Blank route mode
continues to mean this legacy shape so existing v1.30.0 signed grants and
receipt hashes remain readable.

`runtime_default` is an explicit alternative for deployments where provider
management is outside the grant coordinator. The signed grant carries the
verified organization/runtime/identity/run, policy ceilings, and bounded lease,
but no provider, model, route, connection, or credential-binding claims. Harbor
uses the boot-configured provider and model, records the actual provider/model
in the content-free receipt, and still enforces the grant, lease, reservation,
receipt, retry, and downgrade boundaries. A mixed grant is rejected before the
provider call. External cross-provider fallback is rejected before any provider
call; same-route retries and structured-output downgrades retain their distinct
server-derived attempt coordinates and receipts.

The route mode is signed and may be restricted by configuration. A top-up must
preserve the original route mode and attempt identity; it cannot turn a
runtime-default grant into a coordinator-bound grant or widen authority locally.

### Receipt identity

The canonical content-free receipt includes explicit route mode, logical call
and attempt nonce, parent logical call/nonce, planner-step coordinate,
retry/downgrade/fallback coordinates, and the actual provider/model. The
canonical attempt id is derived from the grant id, logical call, nonce, and all
attempt coordinates. `ValidateAttemptUsageReceiptAgainstGrant` verifies the
grant identity, route shape, root or planner-child derivation, attempt id, and
route binding without reading prompt, response, tool, reasoning, credential, or
identity payloads beyond the already verified content-free claims.

The public `Delivery` interface remains transport-neutral. HTTP, queues, or
files may use `MarshalCanonicalAttemptUsageReceipt`; Harbor does not prescribe
an HTTP delivery protocol in this hotfix.

## Acceptance criteria

- `internal/llm`, the Bifrost account, the reference signer/verifier, the
  receipt outbox, runtime assembly, configuration, and the public SDK compile
  and pass focused tests.
- A real assembled runtime in `runtime_default` mode completes through Bifrost
  to an OpenAI-compatible test provider without a credential resolver, uses its
  configured custom-primary key and provider/model, and emits a validated
  receipt with actual route facts. Account-level coverage also pins the native
  `LiveKey` path. An unrestricted (empty `route_mode`) mixed-fleet boot
  opportunistically retains a configured runtime key but preserves v1.30.0's
  keyless coordinator-bound boot. Without that key, runtime-default fails at
  call time while coordinator-bound resolver use remains available.
- A package-level external consumer names `sdk/assemble.Options`,
  `sdk/config.LLMExternalGrantConfig`, `sdk/llm.ExternalGrant`,
  `sdk/llm/grant`, and `sdk/llm/receipts.Delivery` without internal imports.
- Coordinator-bound grants remain covered by the existing multi-organization,
  generation-fencing, retry, and receipt tests.
- Mixed route claims, forged root receipts, forged planner children, and
  response-loss/replay attempt identities fail closed without a second provider
  call.
- Focused race tests, vet, phase smoke, docs/mirror, and repository diff checks
  pass. Hosted candidate and post-merge main CI, including full preflight, also
  pass; this plan makes no v1.30.1 tag, release, module-provenance, or downstream
  deployment claim.

## Files added or changed

- `sdk/llm`, `sdk/llm/grant`, `sdk/llm/receipts`, `sdk/config`, and
  `sdk/assemble` expose and compile the public composition surface.
- `internal/llm`, `internal/llm/grant`, and `internal/llm/receipts` own route
  validation, canonical receipt identity, runtime-default execution, and
  durable delivery validation.
- `internal/runtime/assemble` and `internal/config` wire and validate the
  optional runtime route restriction.
- This plan, D-436, HA-70's register extension, configuration/glossary/index
  truth, changelog, and `scripts/smoke/phase-256.sh` carry governance evidence.

## Test plan

- Run focused race suites for the LLM grant/receipt edge, runtime assembly,
  configuration, and public SDK packages.
- Prove external-package compilation, legacy v1.30.0 signature/hash
  compatibility, explicit route-shape refusal, zero-resolver runtime-default
  assembly through Bifrost, native/custom configured-key selection, legacy
  blank-required keyless boot plus coordinator resolver use, actual
  provider/model receipts, deterministic multi-step receipt identity, forged
  root/child refusal, and response-loss replay refusal.
- Run focused `go vet`, the Phase-256 smoke, mirror/diff checks, and the full
  drift audit. Broad preflight remains the hosted release gate.

## Smoke script additions

`scripts/smoke/phase-256.sh` asserts the plan/decision/ask register, public SDK
packages and external consumer, route/receipt validators, assembled
runtime-default consumer, adversarial test names, and unchanged Protocol
boundary.

## Coverage target

No repository-wide numeric threshold changes. Every new route branch and
public composition seam has focused positive and fail-closed coverage; the
existing `go test -race` package gate remains authoritative.

## Compatibility and evidence boundary

The change is additive to the LLM edge and does not change Protocol version
`0.1.0`. Existing v1.30.0 coordinator-bound grants and legacy receipt hashes
remain accepted. `runtime_default` is opt-in and never inferred from omitted
provider fields. No credential, prompt, response, tool payload, reasoning
trace, or provider response body enters the public receipt contract.

Implementation PR #742 was reviewed at exact head
`9af8e6e72dfb8398329554feadda495272e686c1` and squash-merged as
`506d1f8cbab78eb87cbc87050369fff8fe36abb1` (tree
`7a1266b50b0f268e7c0ea19163958b67ce19dd3a`). Hosted candidate run
`32705738802` and post-merge main run `32710662323` completed successfully,
including full preflight. The annotated `v1.30.1` tag object `b540f631` peels
to `fd801b14`; release workflow `32720513063` succeeded with 13 assets,
verified aggregate and sidecar checksums, six attestations, the expected native
binary stamp, and public module provenance. Post-tag scaffold cleanup is
included in this follow-up. Downstream runtime acceptance remains pending.

## Dependencies

- D-434 / Phase 254 / HA-70: original external execution grant and receipt
  contract.
- D-435 / Phase 255 / HA-71: provider-neutral descriptor boundary.
- D-025: immutable-after-construction and concurrent reuse.
- D-333/D-334: credential custody and broker boundaries.
