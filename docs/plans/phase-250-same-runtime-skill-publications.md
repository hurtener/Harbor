# Phase 250 — Same-runtime organization skill publications (HA-68)

## Summary

Deliver Harbor-local organization skill publications as immutable,
content-addressed revisions with exact references and content-free metadata,
receipts, and Protocol projections. At the integrated base, the publication
domain and persistence contract, additive Protocol surface, strict control
handler, typed clients, conditional capability, generated reference pages, and
run-start composition are present. The one remaining phase gate is the
production/devstack bootstrap that mounts one authorized publication store and
surface together; until that wiring lands, the capability must remain
unadvertised. Every resolved run still uses only its exact same-runtime
reference and fails closed on authority, lifecycle, hash, generation, or
runtime mismatch.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.7
- RFC §6.10
- RFC §6.11
- RFC §6.16
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05
- brief 06
- brief 07

## Brief findings incorporated

- brief 04 §4.5: required-tool metadata filters and redacts content but never
  grants a tool; publication composition must use the existing effective-agent
  capability boundary.
- brief 04 §4.8: skill persistence is identity-scoped and auditable; a
  publication source must not turn tenant visibility into a shared principal.
- brief 05 §4: StateStore `SaveIf` is a mandatory conditional write, not an
  optional capability; successor, retirement, reference, and idempotency
  transitions use the existing atomic contract.
- brief 05 §5: identity and erasure boundaries apply to every durable record;
  references are keyed by the caller's durable tenant/user/agent state and do
  not widen the `(tenant, user, session)` isolation tuple.
- brief 06 §3 and §5: canonical Protocol projections are typed and bounded;
  metadata/receipts must not carry unbounded skill bodies or bypass redaction.
- brief 07 §4 and §5: dispatch and composition validate identity and capability
  before execution, and resolved values remain separate from model-visible
  observations.

## Findings I'm departing from (if any)

None.

## Goals

- Provide one organization-owned publication store with immutable revisions,
  exact generation/hash compare-and-set, explicit `active|retired` state, and
  content-free projections.
- Require verified admin authority for publication lifecycle methods and
  verified identity plus signed effective-agent reach for caller methods.
- Persist exact user/agent references durably through the existing StateStore
  CAS and idempotency contracts, including restart and response-loss replay.
- Resolve a reference only inside the same Harbor runtime that minted it and
  only after rechecking runtime id, publication/revision identity, generation,
  content hash, lifecycle, identity, and reach.
- Expose additive typed Protocol methods/errors/types without a Protocol
  version bump, and generate the checked-in Protocol reference pages.
- Make runtime composition the final gate: no body in list/get/reference/
  receipt metadata, no latest-revision fallback, and no cross-runtime fetch.

## Non-goals

- Cross-runtime publication federation, portable references, or a centralized
  catalog.
- Skill bodies in metadata, list responses, references, receipts, events,
  audit payloads, logs, or model-visible discovery.
- A new isolation principal, a shared service identity, or a scope-label
  shortcut that bypasses verified caller identity.
- A new StateStore driver, migration family, publication broker, or generalized
  catalog abstraction.
- Automatic publication from boot/config, arbitrary Console UI, or a new
  Protocol version.
- Replacing existing operator packs, personal package import, or the shared
  effective-composition resolver.

## Acceptance criteria

- [x] The publication domain enforces canonical names, immutable revisions,
      active/retired lifecycle, exact successor/retire CAS, content-free
      metadata, and bounded receipts.
- [x] Every publication operation requires the full verified identity and
      runtime binding; user references remain scoped to the durable tenant,
      user, and effective agent, with no `agent_id` isolation widening.
- [x] Memory and StateStore implementations cover restart, idempotency,
      response-loss replay, tenant/user isolation, and StateStore `SaveIf`
      contention without partial writes.
- [x] `Resolve` is the only body-bearing operation and rejects retired,
      foreign-runtime, stale-generation, wrong-hash, missing, and unreachable
      references rather than falling back.
- [x] Canonical Protocol methods, types, errors, and control-status mappings
      are additive and ProtocolVersion remains unchanged; generated protocol
      reference pages reflect the canonical source, including the HA-68
      guidance rows.
- [x] The strict Protocol transport adapter dispatches all ten methods to the
      authorized SkillPublicationsSurface with verified admin versus caller
      authority, signed effective-agent reach, and bounded content-free
      responses; focused tests exercise the surface with a real publication
      store.
- [x] Runtime composition resolves the exact reference at run start and
      fails closed on every binding mismatch; the resulting body is confined
      to the immutable run snapshot and never enters metadata or receipts.
- [x] Focused MemoryStore, StateStore, Protocol-surface, control-handler, and
      run-start tests cover restart, idempotency, response-loss replay,
      isolation, CAS, authority, strict decoding, and failure modes. The
      source-level concurrent tuple-isolation tests use N=128; the generic
      conformance Stack intentionally does not assemble a publication store.
- [x] A static-only Phase 250 smoke checks the landed domain, strict handler,
      typed clients, conditional capability, authorized store surface,
      generated reference pages, and immutable run-start composition.
- [ ] Production/devstack bootstrap mounts one authorized publication store
      into the Protocol surface and run loop, and advertises
      `CapSkillPublications` only when that same construction is complete. This
      is the sole remaining integration follow-up at this checkpoint.

## Files added or changed

- `internal/skills/publication/publication.go` — publication, revision,
  reference, receipt, validation, memory store, and StateStore contract.
- `internal/skills/publication/publication_test.go` — lifecycle, isolation,
  restart, idempotency, CAS, and concurrent resolve coverage.
- `internal/protocol/types/skill_publications.go` — canonical wire DTOs.
- `internal/protocol/methods/methods.go` — ten publication method names and
  authority classifiers.
- `internal/protocol/errors/errors.go` and
  `internal/protocol/transports/control/status.go` — typed error/status map.
- `internal/protocol/singlesource/*` and protocol-doc generator type indexes
  — canonical lockstep registration.
- `internal/protocol/skill_publications.go` — transport-agnostic surface and
  verified admin/caller/reach dispatch.
- `internal/protocol/transports/control/skill_publications_handler.go` and
  `internal/protocol/transports/{control,transports}.go` — strict REST decode
  and optional control/mux mounting.
- `internal/protocol/client/client.go` and
  `internal/protocol/client/client_skill_publications_test.go` — typed client
  methods and canonical-route/identity tests.
- `sdk/protocolclient/protocolclient.go` — public client aliases for the ten
  additive methods.
- `internal/protocol/posture.go` and `internal/protocol/types/version.go` —
  conditional `skill_publications` capability advertisement and registration.
- `internal/runtime/serve/runloop.go` and
  `internal/runtime/serve/runloop_publication_test.go` — exact run-start
  reference resolution and immutable snapshot/concurrency evidence.
- `docs/site/protocol/{methods,events,errors,types}.md` — generated Protocol
  reference pages containing the HA-68 method/type/error rows.
- `docs/site/protocol/index.md` — protocol-site status note for the landed
  surface and the remaining bootstrap follow-up.
- `docs/skills/configure-memory-and-skills/SKILL.md` and
  `docs/skills/use-the-harbor-protocol/SKILL.md` — operator procedures.
- `docs/site/concepts/memory-and-skills.md` and the relevant site skill
  mirrors — reader-facing documentation.
- `examples/harbor.yaml`, `CHANGELOG.md`, `docs/glossary.md`,
  `docs/decisions.md`, `docs/notes/downstream-asks.md`, and
  `docs/plans/README.md` — operator/config, release, vocabulary, decision,
  register, and master-index records.
- `scripts/smoke/phase-250.sh` — static-only phase smoke.

## Public API surface

The additive Harbor Protocol surface is:

- `skills.publications.publish`, `.list`, `.get`, `.publish_successor`, and
  `.retire` for verified organization administrators.
- `skills.publications.available`, `.install`, `.update`, `.remove`, and
  `.references.list` for verified callers with signed effective-agent reach.
- Content-free publication metadata, exact revision references, bounded
  operation receipts, and typed conflict/not-found/retired/runtime-mismatch/
  idempotency-conflict errors. The body is intentionally absent from these
  projections and is available only through the runtime's authorized internal
  resolve path.

## Test plan

- **Unit:** publication validation, canonical names/content hashes, lifecycle
  transitions, exact generation/hash CAS, content-free projections, and typed
  errors.
- **Integration:** focused Protocol-surface and strict control-handler tests
  exercise a real publication store with verified admin and caller identities,
  signed reach, content-free responses, and foreign-runtime/retired-reference
  failures. The generic conformance Stack remains intentionally unmounted;
  production/devstack bootstrap is the separate unchecked follow-up.
- **Conformance:** MemoryStore and StateStore execute the same publication,
  reference, idempotency, erasure, and CAS matrix; all required StateStore
  drivers remain behind the existing conformance seam. The HA-68 generic
  conformance Stack does not claim a mounted publication store.
- **Concurrency / leak:** the publication domain and run-start composition
  each contain N=128 concurrent tuple-isolation coverage against one shared
  store/driver. These are source-level evidence in this docs finalization; no
  broad test or `-race` run is claimed here.

## Smoke script additions

- Static guards assert the Phase 250 plan, D-430 vocabulary, publication
  domain tests, strict control handler, typed clients, conditional capability,
  authorized store surface, run-start composition, N=128 tests, canonical
  method/error/type registrations, generated Protocol reference pages,
  config/operator guidance, and CHANGELOG entry.
- One explicit guard preserves the unchecked production/devstack bootstrap
  criterion; the smoke remains static-only and does not claim broad preflight
  or a live HTTP route.

## Coverage target

- `internal/skills/publication`: ≥90%.
- `internal/protocol`: existing package target plus focused publication
  method/type/error lockstep coverage.
- Runtime composition adapter: ≥85% for the landed run-start composition;
  bootstrap wiring receives its own focused gate when added.

## Dependencies

- Phases 37 and 40 (skills interfaces/importer).
- Phases 202 and 205 (StateStore identity/CAS and agent reach).
- Phases 232, 237, 240, and 248 (signed reach and effective skill composition).
- Phase 214 (artifact/reference-by-value discipline).

## Risks / open questions

- Runtime-local references are intentionally non-portable; deployment/runtime
  identity must be durable enough to reject stale or foreign references after
  restart without becoming a new isolation axis.
- Reference and publication CAS contention must preserve one-winner
  idempotency under response loss; an ambiguous retry cannot return a fresh
  receipt or a different revision.
- Content-free metadata makes operator inspection intentionally indirect;
  authorized resolve is the only body route and must retain redaction/audit
  boundaries.
- Production/devstack bootstrap is not established on this checkpoint. Until
  the authorized store, Protocol surface, run-loop dependencies, and
  conditional capability are mounted from one construction path, the phase
  remains Pending for that one integration item.

## Glossary additions

- Optional artifact-egress parameter (D-429).
- Organization skill publication (D-430).
- Publication revision reference (D-430).
- Same-runtime publication binding (D-430).
- Content-free publication receipt (D-430).

## Pre-merge checklist

- [ ] `make drift-audit` passes (broad local preflight is forbidden by the
      handoff; run only the safe focused/static checks listed in the handoff).
- [ ] `make preflight` passes (not run and not claimed; handoff forbids broad
      local preflight).
- [x] `make check-mirror` passes after rule-file inspection (no AGENTS/CLAUDE
      edits in this phase).
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve by inspection.
- [x] Generated Protocol reference pages are present and include the HA-68
      methods, types, and error guidance rows (the generator check is not
      rerun in this docs-only finalization).
- [ ] Coverage on touched packages ≥ stated target (focused tests only; broad
      coverage gate not run).
- [x] Multi-isolation domain tests cover tenant/user/session and runtime
      mismatch fail-closed behavior.
- [x] Source-level concurrent-reuse test covers the shared publication store
      with N=128; this docs-only finalization does not claim a local `-race`
      execution.
- [x] Focused Protocol surface, strict control-handler, typed-client, and
      run-start composition tests are present.
- [ ] Production/devstack bootstrap mounts the authorized store/surface and
      conditional capability from one construction path (sole remaining
      integration follow-up).
- [x] New vocabulary is present in `docs/glossary.md`.
- [x] No brief finding was departed from.
