# Phase 259 — external-grant top-up successor validation (HA-74 / D-439)

## Summary

This phase makes the relationship between an external execution grant and its
lease top-up successor explicit, public, and fail closed. The existing wrapper
uses the validator before replacing the predecessor when its optional top-up
seam is supplied, so an independently valid signed grant cannot substitute
different authority during that exchange. This validator-only phase does not
claim stock live top-up transport or durable successor application.

## RFC anchor

- RFC §6.5 — the provider-neutral LLM edge and its request contract.
- RFC §6.11 — governance remains enforced at the provider boundary.
- RFC §6.15 — usage and bounded provider consumption remain content-free.

## Briefs informing this phase

- brief 03 — unified LLM-client boundaries and provider-neutral accounting.
- brief 08 — public SDK composition, strict validation, and pure-Go delivery.

## Brief findings incorporated

- brief 03 §5: retries and provider calls remain behind one LLM-client seam;
  the relationship check belongs in the existing grant wrapper rather than a
  second provider path.
- brief 03 §7: provider-neutral token usage is the portable capacity unit;
  successor validation does not add provider pricing or billing semantics.
- brief 08 §2: Harbor keeps its pure-Go SDK surface; the public helper aliases
  the internal canonical implementation instead of introducing another wire
  representation.

## Findings I'm departing from (if any)

None.

## Goals

- Define one public/testable successor validator and use it in the wrapper.
- Preserve every immutable signed authority and exact attempt identity.
- Permit only bounded monotonic lease/signing/validity advancement.
- Refuse drift before a second verifier or provider invocation.
- Prove deterministic replay, stale-epoch refusal, concurrent reuse, and both
  signed route shapes.

## Non-goals

- No coordinator transport, receipt parser, quota store, billing model,
  provider catalog, product policy, credential format, or UI.
- No stock live top-up support and no durable reservation-store successor
  application or replay semantics; those must land together in another phase.
- No new grant claim, Protocol method, wire type, or Protocol version.
- No local minting of authority and no replacement for signature/context
  verification.

## Acceptance criteria

- [x] `ValidateExternalGrantTopUpSuccessor` is nameable through `sdk/llm` and
  is the wrapper's sole successor relationship check.
- [x] Version, audience, grant/org/runtime/identity/run/call/attempt, raw route
  mode, provider route, credential generations, policy/ceilings, and lease id
  are exact.
- [x] Key id, issued-at, and signature may rotate only before the configured
  verifier re-authenticates the successor.
- [x] Epoch advances exactly once; total capacity grows positively by no more
  than the requested call; consumption does not rewind; remaining capacity is
  sufficient for the call.
- [x] Grant and lease deadlines never rewind and may remain unchanged or
  advance without lengthening either predecessor lifetime; zero, stale,
  overflow, excessive, and omitted state fail closed.
- [x] Table/fuzz/concurrency tests and wrapper integration prove refusal before
  the provider call for every preserved-field mutation.
- [x] Runtime-default and coordinator-bound routes retain their exact raw
  shapes, including legacy blank-mode non-normalization.
- [x] Protocol version remains unchanged.

## Files added or changed

- `internal/llm/external_grant.go` — canonical successor validator.
- `internal/llm/external_grant_topup_test.go` — table, replay, concurrency,
  overflow, and fuzz coverage.
- `internal/llm/grant/wrapper.go` and tests — production consumer and
  no-provider-call adversarial matrix.
- `sdk/llm/llm.go` and the external SDK consumer — public reachability.
- Phase plan, decision, ask register, changelog, master plan, and smoke truth.

## Public API surface

```go
func ValidateExternalGrantTopUpSuccessor(
    current ExternalGrant,
    successor ExternalGrant,
    requestedUnits int64,
) error
```

The helper returns an error wrapping `ErrExternalGrantInvalid` for every
relationship violation. It authenticates no signature; callers continue to
run their configured `ExternalGrantVerifier` after relationship validation.

## Test plan

- **Unit:** mutate every immutable field independently; exercise valid
  coordinator-bound/runtime-default successors and lease/signing/validity
  adversaries.
- **Integration:** drive the actual wrapper with a top-upper and real rotating
  signing keys; every mutated, untrusted-key, or bad-signature successor must
  fail before a provider call.
- **Conformance:** external-package SDK test names and invokes the public
  helper. No driver conformance surface changes.
- **Concurrency / leak:** N=100 simultaneous calls reuse one immutable pair
  under `-race`; deterministic replay succeeds and a successor reused as its
  own predecessor fails stale. The pure helper starts no goroutines.
- **Fuzz:** arbitrary immutable string-field replacement never becomes an
  accepted successor and never panics.

## Smoke script additions

`scripts/smoke/phase-259.sh` asserts D-439/HA-74/plan truth, public SDK
exposure, wrapper consumption, complete adversarial test names, and the
unchanged Protocol boundary.

## Coverage target

- `internal/llm`: every validator branch covered by table/fuzz seed tests.
- `internal/llm/grant`: every wrapper successor branch covered, including no
  provider invocation.
- `sdk/assemble`: external-package public reachability retained.

## Dependencies

- Phase 254 / D-434 / HA-70 — external grants, leases, and receipts.
- Phase 256 / D-436 — public SDK and explicit route modes.
- D-025 — immutable concurrent reuse.

## Risks / open questions

- The validator deliberately bounds renewed lifetimes to the predecessor's
  signed durations. A future contract that needs a different local maximum
  requires a new explicit bounded input; it must not silently widen this
  helper.
- Relationship validation does not replace coordinator authentication or
  current-time checks; the wrapper always runs the configured verifier next.
- Stock live top-up remains blocked: durable `Reserve` exhaustion is observed
  after the optional wrapper exchange point, and the lease store cannot yet
  apply an authenticated successor idempotently. A future phase must add the
  transport and a post-validation reservation-manager hook together; this
  phase must not advance durable state inside an unverified top-up callback.

## Glossary additions

None. “Successor” and “lease top-up” describe this relationship locally and do
not add Protocol vocabulary.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] Focused preflight-equivalent Phase 259 smoke passes; broad preflight is
  the hosted gate
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages meets the stated branch target
- [x] Multi-isolation state is unchanged; the validator compares exact signed
  identity fields and the wrapper retains existing identity tests
- [x] Concurrent-reuse test passes at N=100 under `-race`
- [x] In-package wrapper integration uses the real production wrapper and
  covers failure before provider invocation
- [x] No new vocabulary requires a glossary entry
- [x] No brief finding is departed from
