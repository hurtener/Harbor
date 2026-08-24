# Phase 254 — Context-bound external execution grants and durable attempt receipts (HA-70)

## Summary

This phase adds a generic, opt-in external execution edge to Harbor's existing
one-method `LLMClient`: a signed grant authorizes one verified runtime identity,
logical run, provider route, immutable credential asset generation, reasoning
ceiling, output ceiling, and bounded compute lease. The concrete first consumer
is the Bifrost account and its retry/downgrade/failover path, so every provider
attempt is checked and metered without exposing provider secrets to callers.

The same phase adds a StateStore-backed, content-free usage-receipt outbox with
conditional enqueue/ACK, response-loss idempotency, bounded retry/backoff, and
circuit breaking. Disabled mode preserves existing local-key behavior, optional
mode supports mixed fleets, and required mode fails closed when grant or receipt
authority is unavailable.

## RFC anchor

- RFC §6.5
- RFC §6.11
- RFC §6.15

## Briefs informing this phase

- brief 03
- brief 08

## Brief findings incorporated

- brief 03 §5: “LLM provider quirks are real and need a single correction
  layer.” The grant wrapper is one layer in the existing correction/retry/
  downgrade chain and does not create a provider-specific LLM interface.
- brief 03 §2: tool/provider surfaces must preserve the full
  `(tenant,user,session)` isolation triple. Grant verification and credential
  resolution require the verified identity and logical run before the driver.
- brief 08 “How `bifrost` maps onto Harbor's `LLMClient`”: the Bifrost `Account`
  seam is the concrete provider-key resolution boundary, so verified opaque
  handles are resolved there rather than in the coordinator-facing request.
- brief 08 “Cancellation caveat”: cancellation is observable at the Harbor
  edge even when a provider stream drains buffered chunks; the receipt records
  a canceled/error outcome without placing streamed content in the ledger.

## Findings I'm departing from (if any)

None.

## Goals

- Define a canonical signed, content-free `ExternalGrant` with version, key id,
  audience, expiry, policy generation, runtime/identity/run binding, provider
  connection and immutable connection generation, provider model/route, opaque
  credential handle, immutable asset generation,
  reasoning/output ceilings, and bounded lease.
- Verify grants against request-edge verified identity and organization context
  before Bifrost, including exact model, reasoning, output, runtime, route, and
  lease checks; reject stale, revoked, rotated, malformed, or cross-scope use.
- Resolve credentials only through the verified grant context and immutable
  asset generation; strict grant mode must not require a boot secret or fall
  back to the mutable local key path for a granted call.
- Place verification inside retry, structured-output downgrade, and
  Harbor-orchestrated failover so each provider attempt has independent
  attempt coordinates and a content-free usage receipt.
- Persist receipts through the existing StateStore drivers with durable
  idempotency, response-loss-safe replay, ACK, bounded backoff, cancellation,
  and a circuit breaker.
- Preserve local governance as an emergency ceiling and preserve exact legacy
  behavior when external grants are disabled.

## Non-goals

- A Harbor-owned global quota, provider catalog, billing system, or coordinator
  implementation.
- A new Protocol version or provider-specific `LLMClient` method.
- Persisting prompts, responses, tool arguments, reasoning traces, or provider
  credentials in grants, receipts, StateStore payloads, logs, or errors.
- Replacing the existing local `LiveKey` rotation path for legacy calls.
- Claiming exact invoice truth from provider-reported or estimated usage.
- A release/tag, hosted deployment, or downstream fleet mutation.

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
golden cleanup is included in this follow-up; no downstream runtime, fleet, or
database deployment/acceptance is claimed.

## Acceptance criteria

- [ ] A signed grant verifies version, key id, audience, expiry, runtime,
  organization, verified identity/run, provider connection and immutable
  connection generation, provider model/route, policy
  generation, reasoning/output ceilings, and bounded lease before Bifrost.
- [ ] Two organizations sharing one runtime resolve distinct opaque bindings
  concurrently with no credential, identity, generation, or receipt bleed.
- [ ] Same-generation binding replacement is refused; rotation and revocation
  fence every prior grant and credential resolution without exposing secrets.
- [ ] Required mode rejects missing grants, missing verifier/resolver/receipt
  sink, invalid signatures, wrong model/reasoning/output, expired grants,
  stale generations, insufficient leases, and missing verified context before
  the provider is called.
- [ ] Optional mode accepts valid grants and preserves legacy calls without a
  grant; disabled mode preserves the pre-phase local-key behavior.
- [ ] Retry, structured-output downgrade, and failover provider attempts each
  re-verify the grant and receive distinct deterministic attempt coordinates.
- [ ] Receipts carry only content-free identity/route/generation/usage/outcome
  facts plus a canonical body hash and idempotency key; no prompt, response,
  tool argument, reasoning trace, or credential appears in serialized output.
- [ ] The StateStore outbox conditionally enqueues and ACKs receipts, accepts
  duplicate enqueue/replay safely, persists bounded retry state, and opens a
  circuit breaker without an unbounded replay loop.
- [ ] Canceled, failed, retry, fallback, response-loss, and crash/replay paths
  remain idempotent and content-free; delivery receives duplicate-safe keys.
- [ ] New reusable artifacts pass N≥100 concurrent reuse under `-race` with no
  cross-context cancellation or identity bleed.
- [ ] Focused package tests, `go vet`, phase smoke, and the repository's hosted
  real-PostgreSQL StateStore acceptance seam pass; no local preflight claim is
  required by this phase.

## Files added or changed

```text
internal/llm/external_grant.go
internal/llm/llm.go
internal/llm/registry.go
internal/llm/grant/grant.go
internal/llm/grant/grant_test.go
internal/llm/grant/wrapper.go
internal/llm/grant/wrapper_test.go
internal/llm/receipts/outbox.go
internal/llm/receipts/outbox_test.go
internal/llm/drivers/bifrost/account.go
internal/llm/drivers/bifrost/account_test.go
internal/llm/retry/retry.go
internal/llm/output/downgrade.go
internal/governance/failover.go
internal/drivers/prod/prod.go
docs/notes/downstream-asks.md
docs/decisions.md
docs/glossary.md
docs/plans/README.md
scripts/smoke/phase-254.sh
```

## Public API surface

The phase keeps the LLM client one-method and adds only additive internal
request/dependency seams:

```go
type ExternalGrant struct { /* signed context-bound claims */ }
type ExternalGrantMode string // disabled | optional | required
type ExternalGrantVerifier interface {
    Verify(context.Context, ExternalGrant, CompleteRequest) error
}
type CredentialResolver interface {
    Resolve(context.Context, ExternalGrant) (ResolvedCredential, error)
}
type LeaseTopUpper interface {
    TopUp(context.Context, ExternalGrant, int64) (ExternalGrant, error)
}
type UsageReceiptSink interface {
    Enqueue(context.Context, AttemptUsageReceipt) error
}
```

`CompleteRequest.ExternalGrant` is optional and is interpreted only by the
registered wrapper. `ExternalGrantConfig` is passed through `llm.Deps`.
`receipts.Delivery` is the outbox's coordinator-facing adapter and receives
only `AttemptUsageReceipt` values.

## Test plan

- **Unit:** Ed25519 canonical signing/verification; claim and lease bounds;
  identity/audience/model/route checks; binding generation fencing; receipt
  canonical hash and redaction; strict/optional/disabled modes.
- **Integration:** Bifrost `Account.GetKeysForProvider` resolves the verified
  binding only after the grant wrapper; missing local secret is allowed only in
  required grant mode; retry/downgrade/failover all traverse the wrapper.
- **Conformance:** StateStore-backed outbox enqueue, duplicate identity,
  conditional ACK, replay, changed-body refusal, and in-memory/SQLite driver
  parity; retain the existing PostgreSQL acceptance selector and PASS guard.
- **Concurrency / leak:** N=100+ two-organization binding resolution under
  `-race`; concurrent grant wrapper calls; outbox replay/enqueue races;
  cancellation joins the bounded replay loop; response-loss delivery is
  safely replayed by receipt id/body hash.

## Smoke script additions

- Assert the phase plan, D-434, HA-70 register entry, and glossary terms.
- Assert the grant verifier, Bifrost verified-binding path, receipt outbox,
  retry/downgrade/failover coordinate hooks, and focused tests exist.
- Assert the smoke remains static-only and does not contact a provider,
  PostgreSQL, or a downstream deployment.

## Coverage target

- `internal/llm`: existing package floor plus all new grant/receipt branches
  covered by focused tests; no new branch may be untested.
- `internal/llm/grant`: ≥85% statement coverage.
- `internal/llm/receipts`: ≥85% statement coverage.
- `internal/llm/drivers/bifrost`: retain existing package floor and cover the
  strict grant account seam.

## Dependencies

- 33 (LLM client and Bifrost driver)
- 36a (retry/governance reliability seams)
- 57 (StateStore durable sequence and driver contract)
- 91 (LiveKey rotation compatibility boundary)
- 93 (Harbor-orchestrated failover seam)
- D-333 / D-334 (inference-plane broker-pull direction)

## Risks / open questions

- A provider may report incomplete token/cost data or finish a stream after
  cancellation; the receipt records availability and outcome rather than
  claiming invoice truth or exact mid-stream cutoff.
- Cross-runtime global reservations and credential custody remain coordinator
  responsibilities. Harbor's bounded lease prevents an unbounded local
  attempt but is not a replacement for coordinator settlement.
- The durable outbox retains ACK records in the existing StateStore namespace;
  retention/compaction is a later operational phase, not an unbounded replay
  loop in this phase.
- A hosted real-provider acceptance requires non-secret test credentials and
  remains environment-gated; no local provider call is implied by this plan.

## Glossary additions

- External execution grant
- External execution usage receipt

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Reusable artifact:** concurrent-reuse test N≥100 passes under `-race`.
- [ ] **Cross-subsystem seam:** an integration test exists and runs under
  `-race` with real StateStore/Bifrost seams where available.
- [ ] New vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
  entry filed
