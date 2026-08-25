# Phase 262 — stock external-grant credential resolution (HA-73, D-442)

## Summary

This phase makes coordinator-bound external grants usable by ordinary stock
`harbor serve`, not only custom embedding hosts. It publishes one canonical
credential-resolution exchange, wires an optional authenticated coordinator
client and public server injection, and preserves the runtime-default and
disabled paths without coordinator work.

## RFC anchor

- RFC §6.5 — provider credentials remain behind the provider-neutral LLM edge.
- RFC §6.11 — durable grant verification/reservation remains authoritative.
- RFC §6.15 — the exchange remains content-free except for the short-lived
  secret confined to the provider-driver response edge.

## Briefs informing this phase

- brief 03 — unified LLM-client and provider-attempt boundaries.
- brief 08 — public SDK contracts and pure-Go serving compatibility.

## Goals

- Publish a transport-neutral `sdk/llm/credentials` v1 request/response
  contract with strict canonical parsing and explicit byte bounds.
- Derive every resolver request field from the exact signed grant already
  installed by Harbor's successful verifier; accept no caller authority.
- Add optional stock authenticated HTTP resolution and public
  `sdk/server.Options.ExternalGrant` injection.
- Bind responses to the exact provider, opaque handle, connection generation,
  and credential-asset generation before exposing a secret to the driver.
- Bound secret-bearing cache lifetime/cardinality, coalesce only exact full
  binding digests, isolate cancellation, and clear material on close.
- Report coordinator-bound route readiness truthfully at the existing
  `runtime.info` surface.

## Non-goals

- No coordinator policy, provider catalog, billing, UI, or product-specific
  contract.
- No runtime-global provider-key fallback for coordinator-bound grants.
- No idle poll, discovery, refresh loop, StateStore scan, or Protocol change.
- No requirement that runtime-default deployments configure a coordinator.
- No replacement of the existing public host-injected resolver seam.

## Acceptance criteria

- [x] An external package can parse an exact canonical request and produce an
  exact bound response without importing `internal/`.
- [x] Stock `harbor serve` wires the resolver from a safe boot-pinned endpoint,
  while `sdk/server` can inject the same public seam.
- [x] Missing verified context, runtime-default grants, mismatched canonical
  grants, unsafe endpoints, redirects, oversized/noncanonical bodies, and
  mismatched response generations fail before provider key use.
- [x] Concurrent requests for two organizations on one runtime never share
  credential material; same exact requests singleflight safely.
- [x] The cache has a fixed 256-entry / 30-second maximum, is clamped by both
  signed grant and response expiry, generation-fenced, and cleared on close.
- [x] A real stock SDK server reports coordinator-bound ready only when the
  resolver plus existing strict seams are concretely wired.
- [x] Disabled and runtime-default paths perform no new coordinator work.

## Public contract

`sdk/llm/credentials` exports `Request`, `Response`, version and byte-bound
constants, typed invalid-request/response errors, and canonical marshal/parse
helpers. The request has exactly `{version, grant}`. The grant is Harbor's full
canonical signed authority: organization, runtime, effective AgentID,
tenant/user/session, logical run/call, provider connection and generation,
route, opaque handle and asset generation, policy, limits, lease, validity, and
signature. No authority field exists beside it.

The coordinator response includes the provider, handle, connection generation,
asset generation, secret, and expiry. Harbor requires every binding field to
equal the request. Unknown, duplicate, reordered, alternative, trailing, or
over-bound JSON fails strict canonical re-encoding. The public parser validates
shape/binding, not grant signature; the runtime verifier remains the signature
authority before transport.

## Stock transport and cache

`llm.external_grant.coordinator.credential_url` shares the existing named
service-token environment variable and timeout. HTTPS is required except for
loopback HTTP. Userinfo, query, fragment, and redirects are refused. Request and
response bodies are bounded and coordinator response bodies, URLs, tokens,
handles, and secrets are never reflected in errors or logs.

The cache key is the SHA-256 of the exact canonical verified signed grant. It
therefore includes every content-free binding dimension and both credential
generations, rather than a runtime-global or connection-only key. Entries are
limited to 256 and 30 seconds and also expire at the earlier response/grant
deadline. Singleflight uses the same digest. Caller cancellation releases that
caller but does not cancel another exact-binding waiter; transport timeout
still bounds the detached fetch. Closing the client increments an epoch, clears
all entries, and prevents an in-flight response from repopulating the cache.

## Configuration and readiness

The stock endpoint and `sdk/server.Options.ExternalGrant.Credentials` are
mutually exclusive. Required coordinator-bound mode still fails boot without
one of them. Optional mode may boot without it but does not advertise that route
as ready. The existing `runtime.info.external_grant` fields are sufficient:
`credential_resolver_wired`, `ready_route_modes`, and `strict_ready` change only
when the concrete seam is assembled. No Protocol type/version changes.

The coordinator block may contain receipt and credential endpoints together or
either independently. Receipt batching/reconciliation still requires a receipt
endpoint. `runtime_default` never consults the resolver and needs no external
provider key or catalog.

## Files added or changed

- `internal/llm/credentials` and `sdk/llm/credentials` — canonical contract.
- `internal/llm/credentials/httptransport` — authenticated stock resolver.
- `internal/config`, `internal/runtime/serve`, and `sdk/server` — config,
  stock/public injection, lifecycle, and readiness.
- `docs`, `examples/harbor.yaml`, `CHANGELOG.md`, and
  `scripts/smoke/phase-262.sh` — framework/operator truth.

## Test plan

- Canonical public-package round trips and malformed/unknown/duplicate/order/
  trailing/bound/binding adversaries.
- N=200 concurrent two-organization same-runtime resolutions under `-race`,
  proving two exact remote calls and zero cross-bleed.
- Missing/mismatched verified context, rotation generations, cancellation,
  timeout, redirect, unsafe endpoint, status/body redaction, cache bound/TTL,
  close clearing, and in-flight close epoch tests.
- Config validation for credential-only, combined, unsafe, and receipt-worker
  invalid shapes; stock/injected conflict tests.
- Real authenticated `runtime.info` SDK-server acceptance with the same
  coordinator-bound runtime unwired then wired.
- Focused race/vet, Phase-262 static smoke, docs/mirror, and diff gates. Hosted
  CI, release, and external-coordinator acceptance remain separate.

## Smoke script additions

`scripts/smoke/phase-262.sh` statically asserts the public contract, canonical
bounds, stock config/wiring, verified-context use, bounded cache, cross-org
concurrency test, public server option, runtime-info acceptance, docs, and
decision/register continuity. It reports a non-vacuous summary.

## Coverage target

At least 85% for the new canonical contract and stock HTTP transport, with
branch-oriented tests at authority, response-binding, cache, cancellation, and
shutdown boundaries. Existing unrelated package coverage is not diluted or
claimed.

## Dependencies

- Phase 254 / D-434 — verified grants, credential bindings, reservations, and
  receipts.
- Phase 256 / D-436 — public external-grant SDK and runtime-default route.
- Phase 258 / D-438 — stock authenticated coordinator transport/readiness.
- Phase 260 / D-440 — signed effective AgentID in grant v2.

## Pre-merge checklist

- [x] Focused race and vet pass at the exact candidate head.
- [x] Public external-package and real runtime-info tests pass.
- [x] Phase-262 smoke, docs, mirror, and diff gates pass.
- [x] Adversarial review reports no open P0/P1.
- [x] Committed artifacts contain framework-only vocabulary.
- [x] Hosted CI, release, and downstream acceptance are not overstated.
