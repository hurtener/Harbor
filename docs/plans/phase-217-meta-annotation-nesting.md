# Phase 217 — `meta_annotations` honour `_meta` path nesting

## Summary

Two mechanisms write into the same outbound MCP `_meta` map on the same call and they
disagree about what a dotted key means. `injection.meta_key` is a dot-separated PATH —
`injectMeta` walks it creating intermediate maps, so `vendor.apiKey` lands at
`_meta.vendor.apiKey`. `meta_annotations` merge FLAT — `buildIdentityMeta`
(`mcp.go:1391-1396`) does `meta[k] = v` verbatim, so `vendor.accountId` lands as the
single literal key `_meta["vendor.accountId"]`. A receiver-style server reading one
nested namespace can therefore be handed a credential but not its non-secret companion
value, by any route. This phase makes the annotation merge honour the same nesting,
using the same helper and the same guard.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 14
- brief 09

## Brief findings incorporated

- brief 14 (MCP client/host compliance): `_meta` is a single namespace shared by
  everything the host stamps. Two writers into one namespace with divergent key
  semantics is a host-side inconsistency the server cannot compensate for, because the
  server sees only the merged result.
- brief 09 (MCP OAuth from bifrost): credential-target validation must be one predicate
  consulted by every door, so a key an operator may declare is exactly a key the
  redactor holds. This phase must not widen that predicate — the companion value is not
  a secret and does not belong in the channel built for secrets.

## Findings I'm departing from (if any)

None.

## Goals

- An annotation key containing `.` writes into a nested `_meta` map rather than becoming
  a literal flat key, matching what the injection mapping already does.
- Both mechanisms share one helper and one reserved-key guard.
- The reserved-key guard becomes per-segment, which is strictly tighter than today.
- The depth cap that today applies only to the injection path extends to the annotation
  path, since only that path could previously nest.

## Non-goals

- **Widening `receiverInjectionCredentialSegments`.** It is a security control — the
  guarantee that any key an operator may declare for an injected value is a key the
  audit redactor holds to `***`. This phase changes only the annotation merge's key
  semantics, on a path that never carries a pulled credential.
- A second injection mapping per connection. That would reopen "one auth mode per
  connection", which is load-bearing.
- Persisting attach-time `Headers`. Explicitly rejected: it would put a
  documented-as-secret field into a durable record.

## Acceptance criteria

- [ ] An annotation key containing `.` produces a nested `_meta` map, byte-identical in
      shape to what the same path produces through `injection.meta_key`.
- [ ] The nesting uses the SAME helper the injection path uses — not a second
      implementation (§13).
- [ ] `config.IsReservedMCPMetaKey` is applied PER SEGMENT rather than to the whole key,
      so `tenant.foo` is refused where today it is admitted.
- [ ] The identity triple and agent provenance are still stamped LAST, so no annotation
      can shadow identity — asserted by a test that attempts exactly that.
- [ ] `maxInjectionMetaKeyDepth` applies to annotation keys, so a pathologically nested
      annotation cannot sit below the audit redactor's deep-walk cap.
- [ ] An annotation key with no `.` behaves exactly as today.
- [ ] An annotation and an injection mapping writing into the SAME namespace on one call
      compose into one nested map without either clobbering the other's siblings.
- [ ] **The backward-compatibility question is answered explicitly, not assumed.** See
      below.
- [ ] Mutation-verified: reverting the per-segment guard turns a smoke `OK` into a
      `FAIL`.

## The backward-compatibility question (§10)

Today `validate.go:1780-1787` rejects only an empty annotation key and a whole-key
reserved match. **A dotted annotation key is therefore legal today and lands flat.** Two
consequences follow and both need an answer in this phase, not a discovery later:

1. An existing config with `vendor.foo` validates today and lands flat; after this phase
   it nests. That is a silent semantic change to a shipped, validated surface.
2. An existing config with `tenant.foo` validates today; under per-segment checking it
   becomes a boot-time validation failure.

The blast radius is plausibly zero — `MetaAnnotations` is mirrored on the wire type and
the TS mirror, and no in-tree caller populates it — but §10 requires this be established
rather than assumed. **The phase author verifies the claim by grep across the repo, the
example configs and the Console, records the result in D-362, and if any populating
caller exists, the change ships with a documented migration path rather than silently.**

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` — `buildIdentityMeta` uses the nesting helper;
  the per-segment guard.
- `internal/config/validate.go` — per-segment reserved check + depth cap on annotations.
- `internal/runtime/agentcfg/protocol/addconnection.go` — the same validation at the
  wire door.
- `internal/tools/drivers/mcp/mcp_test.go`, `internal/config/validate_test.go`
- `test/integration/mcp_meta_nesting_test.go`
- `examples/harbor.yaml` — the nesting semantics documented on the annotation field.
- `scripts/smoke/phase-217.sh`
- `docs/decisions.md` — D-362.

## Public API surface

No exported signature changes. The behaviour change is in the merge semantics of an
existing config field, documented on `config.MCPServerConfig.MetaAnnotations`.

## Test plan

- **Unit:** flat key unchanged; dotted key nests; deep key nests to depth; depth cap
  refuses beyond the bound; per-segment reserved refusal for each reserved segment in
  each position; identity stamps win over a colliding annotation; annotation and
  injection composing into one namespace.
- **Integration:** `test/integration/mcp_meta_nesting_test.go` — a real MCP fixture
  server (§17.8, driven from the official SDK's `_meta` shape) receiving a call with
  both an injected credential and a nested annotation in one namespace; assert the
  server observes the exact nested JSON shape. Assert the audit redactor still holds the
  credential leaf to `***` and does NOT redact the companion. Identity propagation;
  failure mode = an over-deep annotation refused at the door.
- **Conformance:** N/A.
- **Concurrency / leak:** the MCP provider is a compiled artifact under D-025.
  `buildIdentityMeta` is called per-RPC and must not mutate shared annotation state —
  N≥128 concurrent calls under `-race` asserting the source annotation map is
  unmodified and each call's `_meta` is independent. The nesting helper allocates
  intermediate maps, which is exactly where an accidental shared-map mutation would
  hide.

## Smoke script additions

- Attach a connection with a dotted annotation; drive a call against the fixture server;
  assert the observed `_meta` is nested.
- Assert a reserved first segment (`tenant.foo`) is refused at the attach door.
- Assert an over-deep annotation key is refused.
- Assert a flat annotation is unchanged.
- Skip-if-404 across the block.

## Coverage target

- `internal/tools/drivers/mcp`: 85% (no regression)
- `internal/config`: 90% (no regression)

## Dependencies

- 206, 211 (the owner-scoped connection mutators whose attach door this validation joins)

## Risks / open questions

- **The concurrency risk is specific and easy to miss.** The flat merge copies values
  into a fresh map per call. A nesting helper that walks and creates intermediate maps
  can accidentally share or mutate structure across calls if it is handed the config's
  own map rather than building fresh. Called out as a named concurrency assertion for
  that reason.
- **Per-segment checking is strictly tighter and that is a behaviour change** — see the
  backward-compatibility section, which is an acceptance criterion rather than a note.
- **Two mechanisms sharing one helper is the point, and it must not become two helpers
  again.** If `injectMeta`'s signature does not fit the annotation path directly, the
  correct move is to adapt the call site, not to fork the helper (§13).

## Glossary additions

- **Annotation path** — a dot-separated `meta_annotations` key, interpreted as a nested
  `_meta` location rather than a literal key. Shares its interpretation with
  `injection.meta_key`, so one namespace has one meaning regardless of which mechanism
  wrote into it.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes — N≥128 concurrent `_meta` builds under `-race`,
      asserting no shared-map mutation
- [ ] Integration test wires real drivers and a spec-derived MCP fixture (§17.8),
      asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] The backward-compatibility survey is recorded in D-362
- [ ] If new vocabulary: glossary updated
