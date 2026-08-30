# Phase 267 — Same-runtime agent-pack inspect and copy

## Summary

Add an admin-only, same-runtime Protocol capability for inspecting the complete
effective operator pack of an addressed agent and copying a bounded selection
of packs to another agent. The read projection is body-bearing but bounded and
explicitly authorized; the copy is one all-or-nothing target-revision change
guarded by source/target composition hashes and an idempotency key. It
preserves exact source hashes, boot read-only rules, and server-stamped
lineage. Existing `agent_config.agent_packs.list|upsert|remove|propose|commit`
remains the one governed authoring path for the target after a copy.

This phase makes composition and copy safety observable without introducing a
portable catalog, a new isolation principal, a second resolver, or a Protocol
version change. A copied pack is durable target state; a boot-declared source is
read-only and never becomes writable merely because it was selected as a copy
source.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.7
- RFC §6.11
- RFC §6.16
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05
- brief 06
- brief 07
- brief 09
- brief 11

## Brief findings incorporated

- brief 04 §4.5 and §4.8: required-tool metadata filters and redacts but never
  grants capability; pack bodies remain identity-scoped, durable, and
  content-addressed. Inspect and copy therefore re-use the existing effective
  composition boundary and never turn body metadata into authority.
- brief 05 §4 and §5: every durable mutation is identity-scoped and uses
  conditional/idempotent StateStore writes. Copy pins an immutable source
  composition hash and applies the authoritative target CAS in one target
  write, so a response-loss retry cannot create a second target revision.
- brief 06 §3 and §5: Protocol projections are typed, bounded, and server
  enforced. The body-bearing inspect response is an explicitly admin-scoped
  exception, while copy outcomes remain bounded and body-free.
- brief 07 §4 and §5: validation and capability checks precede effects, and
  resolved values stay separate from model-visible observations. The source
  hash is rechecked before the copy write, with no capability grant through
  pack content.
- brief 09 §3 and §6: `agent_id` is registration metadata rather than an
  isolation principal, and signed reach is checked before an agent-addressed
  operation. Both source and target reach are required under one verified
  tenant.
- brief 11 §2 and §5: the Console is a Protocol client and an operator lens;
  it must not read runtime objects or invent a second composition path. The
  operator guidance therefore advertises capability discovery and the same
  Protocol contract only.

## Findings I'm departing from (if any)

None.

## Decision

D-456 settles the additive shape for same-runtime agent-pack inspection and
copy. `agent_config.agent_packs.inspect` returns distinct complete
`boot_packs`, `revision_packs`, and `effective_packs` projections, with per-entry
source/hash/`editable` metadata and the composition hashes needed to reconcile a
view. `agent_config.agent_packs.copy` selects a bounded `pack_ids` set and
atomically creates or converges one target revision only when the source/target
composition-hash, identity, reach, runtime, and idempotency conditions still
match.

An inspect response has this conceptual shape; the canonical wire types and
generated mirrors use the same fields:

```json
{
  "agent_id": "agent-source",
  "boot_packs": [],
  "revision_packs": [{
    "pack_id": "review",
    "pack": {
      "name": "review",
      "title": "Review",
      "description": "...",
      "trigger": "...",
      "task_type": "...",
      "tags": ["..."],
      "steps": ["..."],
      "preconditions": ["..."],
      "failure_modes": ["..."],
      "required_tools": ["..."],
      "required_ns": ["..."],
      "required_tags": ["..."],
      "origin": "pack",
      "scope": "agent",
      "origin_ref": "server-stamped-ref",
      "extra": {}
    },
    "source": "revision",
    "semantic_hash": "<64 lowercase SHA-256 hex>",
    "editable": true
  }],
  "effective_packs": [{
    "pack_id": "review",
    "pack": {"...": "the complete canonical body"},
    "source": "revision",
    "semantic_hash": "<64 lowercase SHA-256 hex>",
    "editable": true
  }],
  "composition_hash": "<64 lowercase SHA-256 hex>",
  "boot_pack_set_hash": "<64 lowercase SHA-256 hex>",
  "protocol_version": "0.1.0"
}
```

The `boot_packs` and `revision_packs` projections are both complete canonical
`AgentConfigAgentPackItem` bodies wrapped in
`AgentConfigAgentPackInspection` (`pack_id`, `pack`, `source`,
`semantic_hash`, and boolean `editable`), not summaries or a latest-revision
shortcut.
When both layers contribute, they remain separately visible with their own
exact body, source, semantic hash, and `editable` boolean; the
`effective_packs` view is the deterministic merged/deduped projection used by
the composition hash. `source` in that effective view is exactly one of
`boot`, `revision`, or `both`; equal boot and revision entries are represented
once as `both`, while differing same-name layers fail closed rather than being
silently merged. `editable` is false when the effective entry includes the
boot-owned source and true for a revision-only entry. Boot-owned entries remain
guarded by the existing mutation/removal rules.

Hashing is deterministic and cross-driver. `semantic_hash` is the
SHA-256 digest, rendered as 64 lowercase hexadecimal characters, of the
canonical attachment-free body fields only: object keys are canonicalized,
unordered arrays/maps use their existing normalized forms, array order is
preserved where it is semantic, and source/revision/provenance fields are
excluded. `composition_hash` is the existing effective-composition resolver's
framed SHA-256 digest over canonical ordered `{pack_id, semantic_hash}` pairs;
the effective source and `editable` values are response metadata and do not
introduce a second hash algorithm. It is therefore stable across an
equal-content revision-number change. `boot_pack_set_hash` is the existing
D-427 boot-set digest over ordered `{pack_id, semantic_hash}` pairs and is
present even for an empty boot set. The implementation must reuse the shared
canonicalization/hash helper rather than duplicate it across drivers.

The copy request is conceptually:

```json
{
  "source_agent_id": "agent-source",
  "target_agent_id": "agent-target",
  "pack_ids": ["review", "safe-defaults"],
  "expected_source_composition_hash": "<64 lowercase SHA-256 hex>",
  "expected_target_composition_hash": "<64 lowercase SHA-256 hex>",
  "idempotency_key": "opaque-client-key"
}
```

The request must also carry the normal verified identity envelope. The server
rejects caller-selected tenant/user/authority fields, requires admin scope,
requires signed `agent_reach` for both agent IDs, resolves both registrations
within the same runtime, and verifies the immutable source composition hash
before any target write. The target composition hash is the authoritative
CAS for the target's current effective revision; an empty effective target has
the deterministic empty-composition hash rather than an omitted or wildcard
value. Cross-runtime, foreign-tenant, missing-reach,
retired/unresolvable, malformed, or stale source/target values fail closed
before mutation. The source and target remain separately identity-scoped: the
operation must not pretend that a cross-identity StateStore transaction is
available. It rechecks the immutable source revision/hash and applies the
authoritative target CAS in the target write transaction.

The copy transaction records all selected target bodies, one new target
revision, server-stamped provenance, and the idempotency outcome together or
nothing. The bounded Protocol response contains only per-pack `copied`/`noop`
outcomes and the exact resulting target `composition_hash` and
`boot_pack_set_hash` values, including deterministic empty-set values; it never
carries pack bodies or provenance. An exact retry returns the original bounded
outcome; reusing the same key with
different source, target, `pack_ids`, or expected hashes is a typed idempotency
conflict. If every selected target body already equals its source semantic hash,
the result is `no_op`: no target revision, generation, active pointer, history,
or audit mutation advances, even when equal bodies were independently authored.
If any selected target has a different independently authored body, or a prior
server copy was edited since its last applied hash, the entire multi-pack
operation is a typed collision/conflict and the server never overwrites one
selected item while applying another.

The server stamps copy provenance rather than accepting it from the caller.
Each copied body receives an `origin_ref` lineage binding source agent, source
revision (or boot source), selected pack ID, source semantic hash, operation
digest, and the target revision/content binding. The durable operation record
also binds source/target IDs, the sorted selection, both expected composition
hashes, and the idempotency digest; same-runtime binding is checked before this
record is written. Reconciliation may update an untouched target only while
every selected target entry still equals its stamped last-applied hash and its
lineage still matches. It may remove untouched copied entries when their source
is removed or the selection is narrowed; it leaves any edited target in place
and returns a bounded conflict signal. Existing target edits through `propose`,
`commit`, or `remove` are the governed way to detach or change a copy. No
reconciliation operation may overwrite an independently authored or edited
target.

The boot source is inspectable but never mutable. A copy from boot materializes
the exact selected bodies as one durable, editable target revision; it does not
write boot state or weaken boot-owned guards on the source. Copy has no body in
its receipt; callers use inspect for complete bodies. Both methods are additive
and capability-discovered; Protocol `0.1.0` is unchanged.

## Goals

- Add the typed, admin-only, same-runtime `agent_config.agent_packs.inspect`
  read projection with distinct complete `boot_packs`/`revision_packs` bodies,
  a merged `effective_packs` view, and exact source, `semantic_hash`,
  `composition_hash`, `boot_pack_set_hash`, and `editable` semantics.
- Add the typed `agent_config.agent_packs.copy` CAS/idempotent operation for a
  bounded `pack_ids` selection and one all-or-nothing target revision, with
  signed reach checks to both agents under one verified tenant.
- Make equal semantic content a true no-op and make independently authored or
  user-modified target collisions fail closed without partial writes.
- Stamp server-owned lineage sufficient for reconciliation to update/remove
  only untouched copies, while preserving a modified target and reporting the
  conflict.
- Reuse the existing effective-composition resolver, boot baseline hash,
  StateStore transaction, revision authoring, and target propose/commit/remove
  governance rather than creating parallel storage or resolution paths.
- Keep canonical Protocol, SDK, generated reference, and operator surfaces in
  lockstep with the implemented methods, while preserving the existing
  Protocol version and Console-as-client boundary.

## Non-goals

- No cross-runtime federation, portable pack/reference format, shared catalog,
  organization publication, or new isolation principal.
- No ordinary-user or non-admin inspect/copy authority, reach inference from
  request body, or agent_id-based replacement of `(tenant, user, session)`.
- No mutation of boot-declared packs, boot persistence, config revisions, or
  a second effective-composition resolver.
- No automatic overwrite of an independently authored or edited target, and no
  pack body in the public copy response, event, audit metadata, or log line.
- No changes to existing pack authoring semantics, personal/session skill
  surfaces, planner exposure, model capability grants, or Protocol version.
- No portable body/catalog surface, unbounded bulk copy, cross-runtime
  federation, or Console-internal runtime access.

## Acceptance criteria

- [x] Capability discovery and canonical wire registration define additive
      `AgentConfigAgentPacksInspectRequest/Response` and
      `AgentConfigAgentPacksCopyRequest/Response` methods at
      `agent_config.agent_packs.inspect` and `.copy`, with Protocol `0.1.0`
      unchanged and strict REST routes documented.
- [x] Inspect is admin-only and body-bearing only after verified identity,
      tenant-local source resolution, and signed reach to the addressed agent;
      missing identity, scope, reach, runtime, or agent fails closed without a
      body oracle.
- [x] Inspect returns complete, distinct `boot_packs` and `revision_packs`
      bodies whenever those layers exist, plus a deterministic
      `effective_packs` view. Every `AgentConfigAgentPackInspection` carries
      `pack_id`, complete `pack`, exact `source`, `semantic_hash`, and boolean
      `editable`; the response also carries `composition_hash`,
      `boot_pack_set_hash`, and `protocol_version`. Equal boot+revision content
      is deduped only in `effective_packs` as `source=both`; differing same-name
      layers fail closed; all hash bytes are cross-driver deterministic.
- [x] Copy requires verified admin authority and signed reach to both source
      and target agent IDs, resolves both in the same runtime and tenant, and
      verifies bounded `pack_ids`, expected source composition hash, and
      authoritative target expected composition-hash CAS before any write.
- [x] Copy atomically records every selected target body in one all-or-nothing
      target revision, server-stamped per-pack provenance, and idempotency
      result; the bounded response exposes only `copied`/`noop` outcomes and
      exact target `composition_hash`/`boot_pack_set_hash` values, exact
      response-loss retry converges, while same-key/different-argument retry
      returns a typed conflict.
- [x] Equal source/target semantic content for every selected `pack_id` returns
      a bounded `no_op` without advancing target revision, active pointer,
      generation, history, or audit mutation; this remains true for equal
      independently authored content.
- [x] If any selected target is a differing independently authored pack or an
      edited prior copy, the complete multi-pack operation fails closed with no
      overwrite or partial StateStore mutation.
- [x] Reconciliation updates or removes only a target whose current semantic
      hash still equals the server's last-applied hash and whose lineage
      matches; edited targets remain intact and produce a bounded conflict.
- [x] Boot source entries remain read-only for all existing mutation/remove
      verbs; copying one materializes an editable durable target governed by
      existing `propose`, `commit`, and `remove` semantics.
- [x] Both shipped StateStore drivers and all Protocol/SDK/reference mirrors
      use one canonical hashing/provenance implementation; generated surfaces
      and capability discovery are lockstep-checked at the implementation head.
- [x] The integration matrix proves no cross-tenant, cross-runtime, or
      cross-agent reach leak under concurrent requests and no capability grant
      from required-tool metadata or copied body content.

## Release evidence (2026-08-30)

Implementation PR #764 merged as
`f6d87b27d8381ed4438e74f75348343729294c8e`. Exact post-merge main CI run
`33297306154` completed successfully. The annotated `v1.31.0` tag object
`dc009bb544ac1381ff6fa23b3e7aa867685adb27` peels to that commit; release
workflow `33299904384` succeeded with 13 published assets. Downstream runtime
deployment and acceptance are not claimed. PR #765 subsequently corrected
explicit-empty reconciliation at both Protocol and internal Service.Copy wire
seams while keeping omitted or `null` selections invalid. It merged as
`8bc070c1dbc144939f4b8980f5ef74c59bff0a07` and remains Unreleased at this
cleanup head.

## Files added or changed

- `internal/protocol/methods/methods.go`,
  `internal/protocol/types/agentconfig.go`, and
  `internal/protocol/singlesource/` — additive methods/types and lockstep
  registration.
- `internal/runtime/agentcfg/protocol/agentpacks_effective.go`, the shared
  effective resolver, and the existing StateStore-backed revision path —
  inspect/copy authorization, canonical hashes, target CAS, provenance, and
  reconciliation.
- `internal/protocol/transports/stream/agentconfig_handler.go` and typed SDK
  client surfaces — strict routes, identity/reach gates, and bounded errors.
- `web/console/src/lib/protocol/agentconfig.ts`, generated manifest/reference
  pages, and operator docs — Protocol-client/reference lockstep (no Console
  internal object access).
- `test/integration/` and the existing agent-pack/protocol test packages —
  driver, restart, isolation, CAS, and race evidence.
- `docs/plans/phase-267-agent-pack-inspect-copy.md` — binding contract.
- `docs/plans/README.md` — Phase 267 master-plan row and detail block.
- `docs/decisions.md` — D-456 decision record.
- `docs/glossary.md` — semantic/composition/provenance vocabulary.
- `docs/skills/use-the-harbor-protocol/SKILL.md` — operator procedure; the
  existing site include mirrors this source into `docs/site/`.
- `docs/site/protocol/index.md` and `docs/site/concepts/memory-and-skills.md` —
  reader-facing Protocol and skills references.
- `scripts/smoke/phase-267.sh` — static contract smoke.
- `CHANGELOG.md` — published v1.31.0 scope and release evidence.

## Public API surface

The additive Harbor Protocol surface is:

- `agent_config.agent_packs.inspect` at
  `POST /v1/agent_config/agent_packs/inspect`, returning distinct complete
  bounded `boot_packs`/`revision_packs` bodies plus the merged
  `effective_packs` body, per-entry source/hash/`editable`, and the two
  composition hashes. It is admin-only, same-runtime, and signed-reach gated.
- `agent_config.agent_packs.copy` at
  `POST /v1/agent_config/agent_packs/copy`, accepting source/target agent IDs,
  bounded `pack_ids`, expected source/target composition hashes, and an opaque
  idempotency key. It returns only bounded per-pack `copied`/`noop` outcomes and
  exact target `composition_hash`/`boot_pack_set_hash` values; it never returns
  a pack body or provenance.

The exact wire types must include a normal identity envelope, reject caller
chosen authority/tenant fields, and map malformed input, missing verified
identity/reach, missing agents, runtime failures, and stale/collision/idempotency
conditions to the existing typed Protocol classes (`invalid_request`, identity
or scope errors, `not_found`, `revision_conflict`, and `runtime_error`) as
appropriate. They are mirrored into the canonical generated/reference
surfaces. Existing pack list/authoring methods and Protocol `0.1.0` remain
unchanged.

## Test plan

- **Unit:** canonical body normalization and hash vectors; distinct boot and
  revision layer projections plus effective source merge (`boot`, `revision`,
  `both`); differing same-name layer rejection; boolean `editable`; the
  deterministic empty boot-layer hash;
  strict request validation; provenance transition; no-op and collision
  decisions; typed errors and bounded receipts.
- **Integration:** real Protocol control handler plus in-memory and durable
  StateStore drivers, with verified admin identity, signed reach to both
  agents, same-runtime source/target resolution, boot source copy, existing
  target propose/commit/remove, restart, response-loss retry, stale source
  composition hash or target CAS, and at least one failed transaction proving
  no partial write across a multi-pack `pack_ids` request. The test must prove
  source pinning plus target CAS rather than assuming a cross-identity
  StateStore transaction.
- **Conformance:** one shared matrix across all shipped drivers verifies
  canonical hash bytes, exact body projection, idempotency, equal-content
  no-op, independent collision, modified-copy conflict, reconciliation
  update/remove preservation, boot read-only enforcement, and replay after
  restart. The canonical wire generator, TypeScript lockstep, reference docs,
  and capability discovery are checked in the same implementation change.
- **Concurrency / leak:** N>=128 concurrent source/target operations across
  tenants, users, sessions, and agent IDs against one shared artifact under
  `-race`; assert no cross-tenant/reach bleed, no duplicate idempotency winner,
  no partial multi-pack body, no goroutine leak, and no mutation of an
  unrelated target.
  Include a concurrent source removal/reconciliation versus target edit race;
  exactly one CAS winner may change the target.

## Smoke script additions

`scripts/smoke/phase-267.sh` is `static-only` because the phase's focused
Protocol/runtime checks run in the unit and integration suites. It asserts the
plan/index/decision/glossary/operator-source files, both method names and
routes, complete distinct layer bodies, bounded plural `pack_ids`, expected
source/target composition-hash CAS, idempotency, collision/reconciliation,
boot-read-only wording, the explicit no-portable-catalog and Protocol-version
boundary, the site include, and the published v1.31.0 changelog
note. It also fails if the new plan contains the forbidden predecessor/product
vocabulary, and pins the implementation files and focused test names so the
smoke cannot pass on documentation alone.

## Coverage target

- `internal/runtime/agentcfg/protocol` and pack domain: retain existing
  package floors and add ≥90% coverage for hash/CAS/provenance/error branches;
  no unrelated package floor may regress.
- Protocol/transport/client adapters: retain existing floors and cover every
  strict route, capability-discovery posture, and typed failure.
- StateStore driver conformance: the shared matrix must execute against every
  shipped driver, with the Postgres leg gated only by its configured DSN.
- Static contract: every acceptance boundary has at least one deterministic
  smoke assertion; drift-audit, mirror, markdown, and focused smoke checks
  must pass.

## Dependencies

- Phase 37 and 40 — skills interfaces/importer and canonical body validation.
- Phases 202, 205, and 206 — StateStore identity/CAS and signed effective-agent
  reach.
- Phases 232, 237, 240, and 248 — effective skill composition, boot baseline,
  and preview/hash semantics.
- Phase 250 / D-430 — same-runtime immutable content-addressed boundaries.
- Phase 266 / D-453–D-455 — shipped v1.31.0 documentation baseline.

## Risks / open questions

- Inspect is intentionally body-bearing and therefore must remain bounded,
  admin-only, same-runtime, and reach-checked. A metadata-only shortcut would
  not satisfy forensic/operator use; a broad body endpoint would create a
  disclosure oracle.
- The implementation must agree with D-427's existing boot hash and effective
  resolver byte-for-byte. Any duplicate canonicalization helper would make
  equal content look different across drivers and must be rejected in review.
- A target edit can race source reconciliation. The target's semantic hash and
  immutable server lineage are the only authority for untouched status; caller
  timestamps, labels, or a copied body must never authorize overwrite.
- Copying boot content is useful only as a durable target snapshot. It must not
  make a boot entry writable or cause later boot changes to mutate a copied
  target without the provenance reconciliation guard.
- Exact Protocol error names and field-level generated mirrors must remain
  additive, typed, strict, and documented alongside the advertised capability;
  an unavailable capability must be reported through normal discovery rather
  than a misleading partial route.

## Glossary additions

- **Agent-pack semantic hash** — the lowercase hexadecimal SHA-256 digest of
  canonical attachment-free pack body fields, excluding source, revision, and
  copy-provenance metadata; equal bodies therefore compare independently of
  authoring lineage.
- **Agent-pack composition hash** — the existing effective resolver's framed
  lowercase hexadecimal SHA-256 digest over canonical ordered pack ID and
  semantic-hash pairs.
- **Server-stamped pack-copy provenance** — the immutable `origin_ref` source
  agent/revision/pack/hash lineage plus operation and target binding, and the
  last-applied target semantic hash used to update or remove only an untouched
  server copy during reconciliation.

## Pre-merge checklist

- [x] `make drift-audit` passes.
- [x] Hosted full preflight passed in exact-main CI run `33297306154`; local
      preflight remains intentionally unclaimed for the implementation checkout.
- [x] `make check-mirror` passes.
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve.
- [ ] Coverage on touched packages ≥ stated target.
- [x] If multi-isolation paths changed: cross-session isolation test passes.
- [x] **Reusable-artifact concurrent-reuse test:** N≥100 concurrent
      inspect/copy invocations against one shared implementation under
      `-race`, asserting no data races, context bleed, cancellation cross-talk,
      or goroutine leaks.
- [x] **Cross-subsystem integration test:** real Protocol transport, signed
      reach, and each shipped StateStore driver exercise at least one failure
      mode under `-race`.
- [x] New vocabulary: glossary updated.
- [x] No brief finding was departed from; no deviation decision beyond D-456.

Implementation evidence at the integrated release head includes the
focused core, Protocol, runtime-assembly, and transport tests; `-race` coverage
for the pack service and Protocol paths; generated Protocol lockstep checks;
Console type-check, lint, and regression coverage; and the Phase 267 static
smoke. Exact post-merge hosted CI, full preflight, the annotated v1.31.0 tag,
and its 13-asset release are complete. Local preflight in the implementation
checkout, downstream deployment, and downstream acceptance remain unclaimed.
The current main correction adds explicit nil-versus-empty wire coverage and
real BuildMux integration for both shipped state drivers; it remains
Unreleased at this cleanup head.
