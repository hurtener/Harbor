# Phase 243 — Verified-caller two-phase skill-package import (HA-61)

## Summary

Add a Protocol-consumable validate/commit workflow for installing a complete
`SKILL.md` package as a durable personal user skill. A caller-owned artifact is
bounded staging input; only an explicit commit may materialize the exact
reviewed body and supporting files into session-independent personal-skill
storage and select the skill for the effective agent.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.7
- RFC §6.10
- RFC §6.11
- RFC §6.16
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05
- brief 07

## Brief findings incorporated

- brief 04 §4.2: incomplete identity fails closed; storage never falls back to
  a default identity.
- brief 04 §4.7: `SKILL.md` parsing, supporting-file resolution, normalization,
  validation, and indexing are one reusable import pipeline; the production
  importer stays the single validator.
- brief 04 §4.5: required-tool metadata is filtered at injection time and does
  not grant capabilities.
- brief 05 §1 and §4: artifacts carry the full identity scope and every durable
  backend implements the same mandatory contract; the persistence triad and
  its conformance suite are the floor for package and proposal records.
- brief 07 §8: validation, deadlines, cancellation, audit, and result shaping
  belong to one runtime dispatch path rather than a caller-specific bypass.

## Findings I'm departing from (if any)

- brief 04 §4.7 and the older RFC §6.7 wording describe supporting files as
  artifact references. That remains correct for validation staging, but is not
  a sufficient installed-package lifetime contract, because an ordinary upload
  is owned by the originating session. D-422 refines the installed form:
  commit must copy the reviewed supporting-file manifest/content into the
  durable personal-skill package representation, so later sessions do not
  dereference the staging artifact. The source artifact remains provenance
  only. The RFC §6.7 wording is updated in the same PR (see the RFC anchor).

## Goals

- Expose separate `agent_config.user.skills.import_validate` and
  `agent_config.user.skills.import_commit` operations, identity-mandatory and
  scoped to the caller's verified triple and signed effective-agent reach.
- Reuse the existing importer and canonical semantic `Skill` validator. One
  normalized package representation and one versioned `PackageHash` algorithm
  serve Protocol import, draft generation, persistence, and runtime reads;
  legacy body-only `ContentHash` remains unchanged.
- Admit ordinary same-scope `artifacts.put` output as staging input without an
  admin grant. The body-scope gate remains the sole reconciliation point.
- Persist a durable proposal containing actor/effective-agent binding, source
  reference, versioned `PackageHash`, supporting-file manifest hashes, expected
  config content hash, configured-ceiling snapshot, expiry/state, and a
  non-authoritative review projection.
- On commit, re-resolve verified identity and effective-agent reach, recheck
  lifecycle, body-scope/method authority, source/package integrity, expected
  config hash, and current server-owned archive/import ceilings; force
  `ScopeUser` and the effective `AgentID`; and durably materialize the complete
  approved package plus membership. This phase adds no separate user-skill
  authoring policy.
- Make response-loss retry, partial staging cleanup, compensation, and
  cross-process races converge through D-411/D-398 conditional-save patterns
  plus a mandatory conditional write on the durable package target key.

## Non-goals

- No cross-tenant import, fleet/admin shortcut for ordinary personal authoring,
  caller-selected tenant/user/session/scope, or `agent_id` isolation axis.
- No implicit replacement, auto-approval, install-on-validate, or mutation
  triggered by reading a proposal.
- No runtime dependency on the source upload or validation-time attachment
  artifacts after commit.
- No capability grant from `required_tools`, namespaces, tags, prose, archive
  names, or metadata.
- No second importer, validator, serializer, package hash, membership writer,
  or local-filesystem-only persistence path.
- No deprecation or semantic change to `agent_config.user.skills.upsert` or
  `skill_propose(persist=true)`, including caller-selected `ScopeUser`.
- No unbounded archive formats, remote URL fetch, recursive package dependency,
  executable package entry, or symlink/hardlink/device interpretation.

## Acceptance criteria

- [ ] An ordinary verified caller can upload and validate an artifact in its
      exact `(tenant,user,session)` scope; `admin` is required only for the
      existing authorized tenant-crossing posture, which this user-import
      surface forbids.
- [ ] Both methods resolve the effective agent through the shared gate and
      require signed reach before artifact lookup, proposal lookup, skill-name
      disclosure, lifecycle lookup, or persistence. `agent_id` remains metadata.
- [ ] Validation accepts bounded UTF-8 Markdown or a bounded archive containing
      exactly one case-sensitive top-level `SKILL.md`. Absolute/traversing
      paths, path normalization escapes, symlink/hardlink/device/FIFO/socket
      entries, duplicate and Unicode/case-colliding normalized paths,
      unsupported MIME, excessive entry count, per-file/total expanded bytes,
      compression ratio, and nested archives fail loud before durable skill
      mutation.
- [ ] The production importer parses, resolves, normalizes, and validates the
      package once. Protocol code does not reimplement frontmatter, CommonMark,
      attachment discovery, validation, serialization, or content hashing.
- [ ] Validation performs zero durable `SkillStore` body/package or agent-config
      membership mutation. Temporary caller-scoped attachment staging is
      allowed only under named count/byte/age limits, with a durable cleanup
      receipt and idempotent cleanup/resume after partial upload or response
      loss.
- [ ] A durable identity-addressed proposal records the source artifact ID and
      hash, versioned `PackageHash`, ordered supporting-file manifest with
      per-file hashes and sizes, effective agent, expected active config hash,
      configured-ceiling snapshot, actor, expiry, and state. Raw package bytes
      are not duplicated into audit/events or an unbounded response.
- [ ] `PackageHash` is a new versioned canonical digest distinct from legacy
      `Skill.ContentHash`. Its input covers a canonical logical semantic body
      whose support-file references are normalized package-relative logical
      paths (or explicit digest placeholders), plus, for every support file in
      canonical path order, normalized relative path, normalized MIME, exact
      size, and full content digest. It never hashes the rendered
      `skillpkg://` URI containing the hash itself. Legacy `ContentHash`
      calculation and equality remain unchanged. Review binding, replacement
      preconditions, target CAS, receipts, and idempotency use `PackageHash`.
- [ ] Validate returns an opaque proposal ID, normalized closed review shape,
      warnings, source/package/manifest hashes, and expected config hash. The
      review shape is bounded and cannot be submitted back as authority.
- [ ] Commit accepts the proposal ID, reviewed hashes, expected config hash,
      and an explicit `replace` choice. It reloads authoritative
      proposal/source state and refuses expiry, identity/agent mismatch, signed
      reach/lifecycle denial, changed source/package/manifest/config hash, a
      package above current configured ceilings, and unapproved replacement
      before visible mutation.
- [ ] Explicit `replace` is necessary but never overrides shipped origin
      precedence: generated input cannot replace `OriginPack`; pack import may
      replace generated or pack only when explicit replace is true. Every
      OriginPack/OriginGenerated incoming/existing pair has create/no-replace/
      replace acceptance rows, and refused pairs leave the exact prior package.
- [ ] Commit forces `ScopeUser`, the verified owner triple with session zeroed
      by the existing personal-skill contract, and the effective `AgentID`;
      none are accepted from package metadata or request fields.
- [ ] The committed SkillStore package contains the complete approved
      `SKILL.md` semantic body and every supporting-file path/content/hash
      required by `skill_get` and runtime injection. A new session can use it
      after restart when the source upload and every validation-time artifact
      are unavailable.
- [ ] Commit rewrites or removes validation-time `artifact://` references from
      the canonical logical body. It first computes `PackageHash`, then
      materializes the stored/rendered body with immutable
      `skillpkg://<PackageHash>/<encoded-canonical-path>` references. Package
      assets use one mandatory resolver that verifies selected user/effective
      agent, normalizes the path, checks package/file hashes, and applies
      MIME/per-read byte ceilings. `skill_get`, injection, export, and other
      consumers use that resolver; heavy content stays artifact/by-reference
      under existing redaction and mandatory-artifact behavior rather than
      being inlined into model or Protocol payloads.
- [ ] Package persistence has one mandatory interface contract and conformance
      rows for every registered SkillStore driver. Existing skills without
      package assets remain readable with byte-identical semantics; schema
      migration is additive and restart-safe.
- [ ] Body/package and agent-config membership publish as one coordinated
      operation using the existing process lock/compensation plus durable
      proposal CAS and a mandatory conditional target write/fence keyed by
      `(tenant,user,session-zeroed ScopeUser,agent_id,name)` with expected
      prior package version/`PackageHash`. A later failure restores the exact
      previous package or deletes only the exact package version proven created
      by this operation's receipt; another proposal's winner is never
      overwritten or deleted. Ambiguity is retained and loud.
- [ ] A commit that lands but loses its response is recognized from exact
      proposal/package/config receipts and returns the same terminal result.
      Competing commits/replacements have one winner across two Runtime
      processes.
- [ ] `required_tools` and related metadata are stored as applicability
      requirements only. Missing or disallowed tools filter/redact the skill
      and never widen catalog visibility, approval, OAuth, or tool-exposure
      policy.
- [ ] Unknown, expired, erased, cross-session, cross-user, cross-tenant, and
      cross-agent proposal/source references return typed closed errors without
      revealing names, paths, hashes, or existence across the authority
      boundary.
- [ ] Canonical methods/types/errors, body-scope registration, REST transport,
      client helper, generated docs, operator skill, and Console TypeScript
      lockstep artifacts land together without a Protocol version bump.
- [ ] N>=128 concurrent validate/commit/cleanup operations on shared reusable
      artifacts across mixed triples and agents pass under `-race` with no data
      race, context bleed, cancellation cross-talk, goroutine leak, double
      publish, or cross-identity cleanup.

## Files added or changed

- `internal/skills/{skills.go,package.go,importer/,conformancetest/}`
- `internal/skills/packageuri/` mandatory `skillpkg://` parser/resolver and
  runtime/`skill_get`/export consumer integration
- `internal/skills/drivers/{localdb,postgres}/` and additive migrations
- `internal/runtime/agentcfg/protocol/` import validation/commit and tests
- `internal/state/` typed proposal/cleanup records using the mandatory driver
  contract and conformance suite
- `internal/protocol/{types,methods,errors,bodyscope,singlesource,transports}/`
- `internal/protocol/client/` and `web/console/src/lib/protocol/` lockstep
- `test/integration/user_skill_package_import_test.go`
- `docs/skills/`, generated Protocol docs, `docs/glossary.md`,
  `docs/decisions.md`, `docs/plans/README.md`, `RFC-001-Harbor.md`, and
  `CHANGELOG.md`
- `scripts/smoke/phase-243.sh`

## Public API surface

- `agent_config.user.skills.import_validate`: caller-owned artifact reference,
  requested/effective agent selection, and bounded validation options in;
  opaque proposal ID plus bounded normalized review/hashes/warnings out.
- `agent_config.user.skills.import_commit`: proposal ID, reviewed hash set,
  expected config content hash, and explicit replacement choice in; durable
  skill/config receipt out.
- A mandatory complete-package representation behind `SkillStore`; supporting
  assets are addressed by versioned `PackageHash`, canonical relative path, and
  full digest, not by the staging session's artifact identity.
- Mandatory conditional package mutation compares the exact durable target key
  and expected prior package version/`PackageHash`; successful mutation returns
  an exact receipt usable by conditional compensation.
- `skillpkg://<PackageHash>/<encoded-canonical-path>` plus one mandatory
  authorized, bounded resolver/read path for all package consumers.
- Typed proposal lifecycle and cleanup receipts over `StateStore.SaveIf`; no
  new optional driver capability.

## Test plan

- **Unit:** archive/path/type/limit matrix; `PackageHash` v1 goldens proving
  logical-reference normalization, path/MIME/size/digest ordering, distinction
  from legacy `ContentHash`, and no fixed-point/self-reference; one golden
  performs logical hash computation -> URI materialization -> resolver lookup
  -> export -> re-import and proves identical logical body/manifest/hash;
  proposal state machine; reach/lifecycle/config/ceiling rechecks; full
  origin-pair replacement matrix; target-key conditional mutation; exact-receipt
  compensation; `skillpkg://` authorization/path/read bounds; response-loss
  convergence; cleanup receipts; required-tool non-grant; legacy compatibility.
- **Integration:** real ArtifactStore staging + StateStore proposal ledger +
  SkillStore package + agent-config membership through both Protocol methods;
  restart into another session, delete/expire staging, then resolve and inject
  the complete skill. Include signed-reach/lifecycle denial, a changed
  configured ceiling, competing proposals, and a storage failure.
- **Conformance:** ArtifactStore drivers preserve bytes/metadata, SkillStore
  drivers preserve package bytes/ordering/hashes and conditional target-version
  semantics, and StateStore drivers preserve proposal/cleanup CAS. Protocol
  integration — run across every registered driver combination — owns verified
  identity/body-scope, effective-agent reach/lifecycle, and cross-principal
  authorization assertions; driver conformance does not claim method auth.
- **Concurrency / leak:** N>=128 mixed-identity calls on one shared importer and
  handler set plus two-Runtime same-proposal races under `-race`; explicit
  cancellation barriers and final goroutine baseline.
- **Fuzz:** archive headers/path normalization/Unicode collisions, Markdown
  frontmatter, manifest decoding, and proposal request decoding with bounded
  allocations and no panics.

## Smoke script additions

- Before implementation, a pending static skeleton records this plan.
- When implemented, validate a same-scope uploaded fixture, assert no skill is
  visible before commit, commit exact reviewed hashes, open a new session, and
  assert the skill and supporting file remain available after staging cleanup.
- Assert stale `PackageHash`/reach/lifecycle, cross-session proposal use,
  traversal archive, forbidden origin pair, and implicit replacement fail with
  canonical typed errors and no mutation.

## Coverage target

- `internal/skills` and importer: 90%; touched SkillStore drivers: 90%;
  `internal/runtime/agentcfg/protocol`: 90%; new Protocol authority paths:
  100%; integration package: 85%.

## Dependencies

- Depends on Phases 40, 202, 205, 209, 221, 226, 232, 233, 233a, and 237.
- Gates Phase 244.

## Risks / open questions

- The durable package schema must choose a bounded blob/child-record layout
  that all SkillStore drivers can commit, conditionally replace, and resolve
  consistently. A session artifact reference is not an acceptable substitute.
- SkillStore and agent-config remain separate durable systems, so the guarantee
  is coordinated compensation and exact convergence, not cross-store ACID.
- Proposal and temporary-staging retention need named ceilings and lifecycle
  cleanup. Session erasure must remove staging/proposals without deleting an
  already committed user skill.
- Existing local `ImportAndStore` behavior may need to delegate to the same
  package materializer without inheriting Protocol approval semantics.

## Glossary additions

- **Skill package staging artifact**
- **Reviewed skill-package proposal**
- **Durable personal skill package**
- **PackageHash**
- **`skillpkg://` reference**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires ArtifactStore, StateStore, SkillStore, and
      agent-config end to end with identity propagation and a failure mode.
- [ ] If new vocabulary: glossary updated
- [ ] The brief/RFC attachment-lifetime refinement is recorded in D-422 and
      the RFC wording is updated before implementation merges.
