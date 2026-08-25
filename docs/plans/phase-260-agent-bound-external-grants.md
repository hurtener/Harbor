# Phase 260 — reach-admitted agent-bound external grants (HA-75 / D-440)

## Summary

This phase adds version 2 of Harbor's public external execution grant. Every
v2 grant signs the exact effective agent configuration admitted by the normal
runtime reach gate, and the stock verifier matches that claim against the
private run context before reservation, credential resolution, or a provider
call. Version 1 signatures and blank-agent receipt bytes remain exactly
compatible, but v1 cannot pretend that an AgentID was signed.

## RFC anchor

- RFC §6.5 — provider-neutral LLM requests remain behind one client seam.
- RFC §6.11 — governance is enforced before provider execution.
- RFC §6.15 — receipts remain content-free usage facts.

## Briefs informing this phase

- brief 03 — unified LLM-client and retry boundaries.
- brief 08 — public SDK contracts and pure-Go compatibility.

## Brief findings incorporated

- brief 03 §5: every governed provider attempt remains behind the existing
  LLM-client wrapper; agent binding does not create a second provider path.
- brief 03 §7: receipts carry only provider-neutral identity and consumption
  facts, never prompts, responses, reasoning traces, or credential material.
- brief 08 §2: the public SDK aliases the canonical internal grant and receipt
  types instead of re-defining their wire contracts.

## Findings I'm departing from (if any)

None.

## Goals

- Sign a required AgentID into version 2 grants for both route modes.
- Match that AgentID to the reach-admitted effective agent carried by the
  canonical run context before any provider-side effect.
- Preserve exact version 1 signature and receipt compatibility.
- Include AgentID in new content-free attempt receipts and top-up immutability.
- Advertise grant-version and agent-binding support through runtime readiness.

## Non-goals

- No provider catalog, quota product, billing model, provider credential
  format, coordinator product policy, or UI.
- No change to agent reach, agent configuration selection, storage isolation,
  invoking-agent provenance, or the control-start authority model.
- No claim that arbitrary ungranted auxiliary or embedder LLM calls carry an
  agent binding. Required grant mode refuses ungranted calls before a provider;
  optional mode retains its documented compatibility behavior.
- No stock live lease top-up transport or durable successor application.

## Acceptance criteria

- [x] Public grant-version constants distinguish exact legacy v1 from
  agent-bound v2; v2 requires a non-empty signed AgentID and v1 rejects one.
- [x] The reference verifier matches v2 AgentID to
  `tools.EffectiveAgentConfigFrom(ctx)` after signature and identity/run checks
  and before reservation, credential resolution, or provider invocation.
- [x] Explicit and default agent selection both pass through the real durable
  reach receipt, run loop, reference verifier, reservation store, and provider
  wrapper; missing, mismatched, or unadmitted context fails closed.
- [x] Runtime-default and coordinator-bound v2 grants share the same binding;
  no external provider credential or catalog is required for runtime-default.
- [x] New receipts carry `agent_id`; v2 grant-bound validation requires an
  exact match, while v1 receipts reject unsigned agent attribution.
- [x] Blank-agent v1.30.0 legacy and v1.30.1 canonical receipt bytes and hashes
  remain unchanged; the strict parser round-trips the new modern field exactly.
- [x] Top-up successor validation treats AgentID as immutable and requires it
  on v2 successors.
- [x] `runtime.info.external_grant` advertises `[1,2]` and `required_v2` agent
  binding without changing Protocol version `0.1.0`.

## Files added or changed

- `internal/llm/external_grant.go` — v2 grant, receipt, validator, and codec.
- `internal/llm/grant` — canonical signer/verifier and wrapper receipt binding.
- `internal/runtime/serve` — real run-loop acceptance and readiness projection.
- `internal/protocol/types/posture.go` and Console mirror — additive readiness.
- `sdk/llm` and `sdk/assemble` — public constants and external compilation.
- Focused unit/integration tests, phase smoke, decision, ask, changelog, and
  master-plan truth.

## Public API surface

```go
const ExternalGrantVersionLegacy = 1
const ExternalGrantVersionAgentBound = 2

type ExternalGrant struct {
    Version int
    AgentID string
    // existing signed claims
}

type AttemptUsageReceipt struct {
    AgentID string
    // existing content-free facts
}
```

## Test plan

- **Unit:** signer/verifier v1/v2 shape, signature mutation, both route modes,
  receipt binding/parser/hash, top-up drift, and pre-provider refusal.
- **Integration:** the existing `control.start` explicit/default acceptance
  proves the sealed reach-receipt producer; the new real in-memory task
  registry, run loop, reference verifier, durable reservation store, receipt
  sink, and provider client prove its consumer through provider execution.
- **Conformance:** public external-package signer/constants/receipt compilation;
  Protocol single-source and TypeScript generation/check.
- **Concurrency / leak:** N=100 reuse of one verifier across two agent-bound
  grants under `-race`, asserting no cross-agent acceptance or provider bleed;
  the feature starts no new goroutine.

## Smoke script additions

`scripts/smoke/phase-260.sh` pins D-440/HA-75/plan truth, public v2 constants,
the reference context check, real run-loop test, receipt binding, readiness
projection, compatibility tests, and the unchanged Protocol version.

## Coverage target

- `internal/llm` and `internal/llm/grant`: every new version/binding branch.
- `internal/runtime/serve`: explicit and default reach-admitted integration.
- `internal/protocol/types` and public SDK: additive wire/compile coverage.

## v1.30.2 release-candidate evidence

PR #747 exact head `0992356db24b43776a10a6572e3df56b610cf50e` was
squash-merged as `459278f7ce599aa6a66f83c3ffbaeb42bb6b7f0c` (tree
`5b7583150d8e7cd3149da1eb77eda4e68ff63f64`). Candidate docs run
`32850686252` and post-merge docs run `32852635507` succeeded. At this cut,
candidate CI `32850686237` and post-merge CI `32852635451` had no failed jobs
but each final preflight gate was still in progress. Due the one-hour pause
deadline, the owner authorized proceeding with the release and tag despite
those pending gates; any late failure will be fixed later that day. As of this
release-cut commit, no tag, release, assets, module provenance, post-tag
cleanup, or downstream deployment or acceptance is claimed.

## Dependencies

- Phase 254 / D-434 — external grants and receipts.
- Phase 256 / D-436 — route modes and public SDK.
- Phase 257 / D-437 — strict canonical receipt parser.
- Phase 258 / D-438 — stock transport and readiness.
- Phase 259 / D-439 — immutable top-up successors.

## Risks / open questions

- Auxiliary naming, trajectory compression, and rolling-summary calls do not
  currently carry per-call external grants. Required mode blocks those calls;
  optional mode permits the established compatibility path. A future phase
  may issue distinct grants for auxiliary work, but must not reuse a logical
  provider-attempt identity.
- AgentID is an execution-authority binding for the selected configuration,
  not a new storage-isolation key and not the boot-derived invoking principal.

## Glossary additions

None. Agent reach and external grants are existing vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] Focused preflight-equivalent Phase 260 smoke passes; broad preflight is
  the hosted gate
- [ ] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages meets the stated target
- [ ] Multi-agent concurrent verifier test passes under `-race`
- [ ] Real run-loop integration uses durable reach and reservation drivers
- [x] No new vocabulary requires a glossary entry
- [x] No brief finding is departed from
