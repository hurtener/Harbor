# Phase 261 — stock authenticated external-grant renewal (D-441)

## Summary

This phase completes Harbor's framework-owned external-grant renewal path.
An opted-in runtime can renew an otherwise valid expired grant or insufficient
lease through one authenticated, boot-pinned HTTP endpoint. Harbor authenticates
the exact predecessor before contacting the coordinator, validates and strictly
verifies one bounded successor, durably applies it, and only then reserves the
attempt and invokes the provider. Disabled and receipt-only runtimes retain
zero idle network or StateStore work.

## RFC anchor

- RFC §6.5 — provider-neutral LLM requests remain behind one client seam.
- RFC §6.11 — governance and durable reservations precede provider execution.
- RFC §6.15 — coordinator exchange and receipts remain content-free.

## Briefs informing this phase

- brief 03 — unified LLM-client, retry, and provider-attempt boundaries.
- brief 08 — public SDK contracts and pure-Go compatibility.

## Goals

- Keep ordinary grant verification strict and expose typed expiry separately
  from every malformed, untrusted, future-issued, revoked, or mismatched case.
- Authenticate expired predecessors without waiving audience, runtime,
  organization, identity/run, v2 AgentID, route/model, reasoning, or output
  ceilings.
- Publish a transport-neutral `sdk/llm/topup` v1 request/response contract with
  canonical codecs, exact public bounds, and deterministic idempotency.
- Add optional stock authenticated HTTP renewal using the existing credential,
  timeout, bounded-body, and no-redirect conventions.
- Apply exact successors durably and replay-idempotently before reservation or
  provider execution.
- Distinguish deadline renewal from capacity top-up: ample expired grants gain
  no compute units; insufficient grants gain at most the requested units.

## Non-goals

- No coordinator policy product, provider catalog, billing model, UI, or
  product-specific protocol.
- No idle polling, proactive renewal, background discovery, or new Protocol
  method/version.
- No runtime invention of grant authority, signatures, units, deadlines, or
  credential bindings.
- No removal of the existing host-injected `LeaseTopUpper` seam.

## Acceptance criteria

- [x] Ordinary verification remains strict and only authenticated expiry has a
  typed renewal path.
- [x] The public canonical contract and all exact bounds are usable by an
  external package without importing `internal/`.
- [x] Expiry-only renewal preserves compute; durable insufficiency adds no more
  than requested and uses a distinct request identity.
- [x] The exact current signed successor survives multiple calls and restarts
  under one immutable root fingerprint.
- [x] Invalid authority makes zero coordinator calls and no provider executes
  before durable successor application plus successful reservation.
- [x] Stock and host-injected transports report truthful readiness and perform
  no idle renewal work.

## Files added or changed

- `sdk/llm/topup` and `sdk/llm` — public renewal contract and seams.
- `internal/llm/grant`, `internal/llm/leases`, and
  `internal/llm/receipts/httptransport` — verification, durable successor, and
  stock exchange.
- `internal/runtime/assemble`, `internal/runtime/serve`, `internal/config`, and
  `internal/protocol/types` — production wiring, configuration, and readiness.
- `docs`, `examples/harbor.yaml`, `CHANGELOG.md`, and
  `scripts/smoke/phase-261.sh` — operator and acceptance truth.

## Ordering invariant

Every grant-bearing provider attempt first resolves any durable current
successor for the exact immutable root bytes, then follows this exact order:

1. Ordinary strict verification.
2. Only for typed expiry or lease insufficiency, authenticated predecessor
   renewal preflight.
3. Current coordinator-bound credential-generation/revocation check.
4. Injected or stock authenticated top-up exchange.
5. Reason-aware immutable successor relationship validation.
6. Ordinary strict verification of the successor and a second current
   credential-generation/revocation check when applicable.
7. Replay-idempotent durable successor application.
8. Durable attempt reservation. A typed stale generation reloads and verifies
   the current successor; typed actual durable insufficiency renews that
   current successor with the explicit `lease_insufficient` reason and retries
   reservation once within the bounded recovery loop.
9. Provider invocation and normal receipt settlement.

Invalid, mismatched, future-issued, malformed, or revoked predecessors make
zero coordinator requests. No provider call can occur before step 7 succeeds.

## Public contract

`sdk/llm/topup` exports version-1 request and response types, strict canonical
marshal/parse helpers, `TopUpIdempotencyKey`, `ValidateRequest`,
`ValidateResponse`, and `ValidateIdempotencyHeader`. The idempotency key hashes
the exact canonical full signed predecessor, requested units, and renewal
reason, so deadline-only renewal cannot replay a capacity-top-up response (or
the reverse). The request
and response body bounds are each 128 KiB; one canonical grant is bounded to
64 KiB; requested units are independently bounded. Unknown, duplicate,
reordered, alternative, trailing, over-bound, or header/body-mismatched input
is refused. A second consumer can implement the coordinator without importing
Harbor's `internal/` tree or recreating private constants.

## Durable successor state

The existing StateStore lease record gains the exact root grant fingerprint,
current grant fingerprint, and bounded canonical current signed successor
bytes. These records are content-free. `LeaseSuccessorApplier` has four
outcomes:

- Absent lease: create the authenticated successor generation.
- Exact predecessor: CAS to the successor while preserving local reserved and
  consumed units and the complete binding.
- Exact successor replay: no-op.
- Stale or different predecessor/successor: typed conflict with no mutation.

An expiry-only successor advances epoch and deadlines while preserving signed
capacity and consumption exactly. An insufficient-lease successor adds no more
than the requested units. Before every grant-bearing call, the resolver accepts
only the exact root fingerprint and returns the current canonical successor;
Harbor reparses, checks lineage, strictly verifies signature and request
authority, and rechecks coordinator credential generation before reservation.
This lets one immutable run-start grant safely reuse successive capacity across
multiple calls and process restarts. Concurrent callers converge on one durable
successor; losing stale or mixed-unit candidates reload that winner rather than
blindly topping up from an old predecessor.

## Configuration and readiness

`llm.external_grant.coordinator.top_up_url` is optional inside the existing
authenticated coordinator block. It shares `auth_token_env` and `timeout`,
requires HTTPS except loopback HTTP, and rejects user info, query, and fragment.
Construction performs no request and starts no timer or goroutine.

`runtime.info.external_grant.top_up_transport` truthfully reports exactly
`unsupported`, `host_injected`, or `stock_authenticated_http`. Both
`runtime_default` and `coordinator_bound`, grant v1 and v2, remain supported;
runtime-default renewal requires no coordinator provider credential or model
catalog.

## Test plan

- Strict verifier tests cover typed expiry, future-issued/malformed/signature,
  audience/runtime/organization/T-U-S-run, route/model/reasoning/output, and v2
  AgentID refusal before renewal.
- Wrapper tests cover expiry-only no-widen renewal, insufficient bounded
  increase, credential revocation, apply-before-provider, persisted expired
  admission recovery, immutable-root multi-call reuse, and SQLite restart into
  an actual completion followed by durable exhaustion renewal.
- Public external-package tests cover canonical round trips, body/unit bounds,
  duplicate/unknown/reordered/trailing input, deterministic keys, and
  header/body equality.
- Stock HTTP tests cover authentication, response loss, N=100 concurrent
  reuse, cancellation, timeout, redirect refusal, strict successor responses,
  and zero idle calls.
- Durable successor conformance runs on in-memory and SQLite, including exact
  successor N=128 convergence, mixed requested-unit winner/reload, corrupt
  canonical/hash refusal, and restart resolution, with the hosted
  `HARBOR_PG_DSN` acceptance extending the real PostgreSQL lease suite.
- Focused race/vet, Protocol generation/check, docs, TypeScript, Phase-261
  smoke, drift, and diff gates are required. Hosted CI, release, and external
  coordinator acceptance remain separate and unclaimed.

## Smoke script additions

`scripts/smoke/phase-261.sh` asserts the public contract and bounds, typed
expiry, predecessor verifier, durable apply/resolve seams, stock readiness,
driver and PostgreSQL acceptance ownership, zero-renewal invalid authority,
immutable-root restart/run-loop coverage, and mixed-unit convergence. It is
static-only and must report a non-vacuous summary.

## Coverage target

At least 85% for the new `sdk/llm/topup` and focused renewal transport logic,
with branch-oriented tests at every fail-closed ordering boundary. Existing
package coverage outside the changed renewal seams is not diluted or claimed.

## Dependencies

- Phase 254 / D-434 — external grants, durable reservations, and receipts.
- Phase 256 / D-436 — public SDK and runtime-default route.
- Phase 258 / D-438 — stock authenticated coordinator transport/readiness.
- Phase 259 / D-439 — bounded immutable successor relationship.
- Phase 260 / D-440 — v2 effective-AgentID binding.

## Pre-merge checklist

- [x] Focused race and vet pass.
- [x] In-memory/SQLite conformance passes; the DSN-gated real-PostgreSQL
  acceptance compiles and skips honestly when `HARBOR_PG_DSN` is absent.
- [x] Protocol docs/types and TypeScript checks pass without a version bump.
- [x] Phase-261 smoke, drift audit, mirror/docs, and diff checks pass.
- [x] Public coordinator contract has no `internal/` dependency.
- [x] Disabled and receipt-only paths perform zero idle renewal work.
