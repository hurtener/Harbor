# Phase 248 — Boot-declared resource-free operator skill baseline (HA-66)

## Summary

Add a boot-declared, resource-free operator skill baseline for the resolved
boot/default agent. A config-file-relative strict eager immutable loader runs
before readiness, imports every baseline entry through the ONE existing
importer/validator (fail-loud otherwise), and eagerly copies and freezes the
set for the process lifetime — with no loader persistence, no admin verbs, no
config revisions, and no lifecycle materialization. The baseline binds exactly
to the resolved `(tenant, boot_agent_id)` pair, merges with the agent's active
durable operator-pack revision into ONE combined operator tier FIRST under
strict rules (same canonical name + same semantic hash dedupes as
`source=both`; differing hash fails; exactly 256 unique combined items), and
the combined tier applies LAST over base/user/session skills. The phase
delivers ONE shared strict effective-composition resolver + preview used by
boot preflight, run, and preview alike, completing D-414 on the boot base,
plus the read-only Protocol path, minimal Console and CLI consumers (D-415),
config docs/example, operator skill, and smoke. Headless `RunOnce` is
explicitly unsupported and fails loud when `boot_agent_packs` is configured.
D-427 is the phase authority.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.7
- RFC §6.16
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05

## Brief findings incorporated

- brief 04 §4.2: incomplete identity fails closed; storage never falls back to
  a default identity — the boot baseline never invents an identity.
- brief 04 §4.7: `SKILL.md` parsing, normalization, validation, and indexing
  are one reusable import pipeline; the boot baseline loader uses that ONE
  pipeline, never a second validator.
- brief 04 §4.5: required-tool metadata is filtered at injection time and does
  not grant capabilities; the baseline's required tools never widen a run's
  visible tool set.
- brief 05 §1: durable identity-scoped records are the floor for anything
  durable; the boot baseline deliberately declares none, and the loader's
  zero-durable-write property is asserted by a not-invoked spy, not by timing.

## Findings I'm departing from (if any)

- D-414's composition preview resolves durable pack membership and personal
  skills but has no source for a config-file-declared boot baseline — on the
  boot base the preview is absent/incomplete. D-427 does not build a second
  preview; it extends the ONE D-411/D-414 composition path so the same
  effective-composition resolver + preview includes the boot baseline
  (AGENTS.md §13: one composition path, no second implementation).
- Phase 237's operator pack tier is durable and per-agent; the boot baseline
  is a resource-free supplement declared in boot config, not a replacement.
  Both merge into ONE combined operator tier FIRST (same canonical name +
  same semantic hash dedupes as `source=both`; differing hash fails; exactly
  256 unique combined items) that is then applied last after
  base/user/session skills.

## Goals

- Declare an operator skill baseline in boot configuration, resolved relative
  to the config file, loaded strictly, eagerly, and immutably before
  readiness.
- Validate every baseline entry through the ONE existing importer/validator;
  any malformed, unresolvable, or un-importable entry fails the boot loud.
- Perform zero durable writes and expose zero new mutation Protocol surface:
  no skill store row, no agent-config revision, no lifecycle record, no admin
  verb (the D-414 read-only preview surface is completed for the boot base,
  not added as a new method).
- Bind the baseline exactly to the resolved `(tenant, boot_agent_id)`; never
  invent a boot identity, placeholder, or wildcard.
- Compose the baseline with the agent's active durable operator-pack
  revision into ONE combined operator tier FIRST (same canonical name + same
  semantic hash dedupes as `source=both`; differing hash fails; exactly 256
  unique combined items), pre-reading every declared tenant-agent active
  revision before readiness and retaining the run-start conflict defense, and
  apply the combined tier LAST over base/user/session skills.
- Enforce boot-owned mutation/remove guards on every Protocol write to a
  boot-declared name: `upsert` and every proposal commit
  (replay/prepared/publish) and rollback/activation reject even equal hash;
  removal may delete an actual legacy durable revision shadow while leaving
  boot; a boot-only remove is a typed read-only refusal, never false success;
  `agent_packs.list` remains durable-revision authoring only.
- Carry a deterministic set hash over the normalized baseline entries in the
  run snapshot and the composition preview, which reports `boot|revision|
  both` plus `boot_pack_set_hash` under authority/reach gating with no
  lifecycle materialization.
- Use ONE loader path for production and devstack; state headless `RunOnce`
  as explicitly unsupported, failing loud when `boot_agent_packs` is
  configured.
- Deliver ONE shared strict effective-composition resolver + preview that
  includes the baseline (completing D-414 on the boot base), used by boot
  preflight, run, and preview alike, plus the exact read-only Protocol path
  (clients, manifest, generated docs), minimal Console and CLI consumers
  (D-415), config docs/example, operator skill, and smoke; keep
  `EnsureBootAgentLifecycle` separate and unchanged (it MAY write a
  revision).

## Non-goals

- No loader persistence of any kind: no SkillStore rows, no StateStore
  records, no agent-config revisions, no lifecycle materialization, no
  migration; the loader/composer performs zero persistence, zero admin pack
  verbs, zero lifecycle writes, and zero config revisions.
- No new mutation Protocol method, admin verb, or Console surface beyond the
  D-415 consumers; the baseline is config-declared read-only state, node-local
  and reconstructed at every boot while the durable Postgres `${SKILLS_DSN}`
  `boot_agent_packs` schema persists agent revisions and personal state — no
  convergence claim.
- No invented boot identity: if the deployment's default agent cannot be
  resolved, the runtime fails loud at boot rather than synthesizing one.
- No `agent_id` isolation axis and no widening of the verified identity
  triple; `agent_id` remains a runtime/config entity (D-059).
- No second composition path: the D-411/D-414 resolver stays the single
  effective-composition path, and the boot baseline never diverges from the
  strict merge contract (no last-write-wins, no shared-cap bypass).
- No headless `RunOnce` support: it is explicitly unsupported and fails loud
  when `boot_agent_packs` is configured.
- No change to `EnsureBootAgentLifecycle`: it remains the separate mechanism
  that materializes the first empty agent-level revision when the lifecycle
  slot is absent, and it MAY write a revision.

## Acceptance criteria

- [ ] With a config-file-declared baseline, a fresh runtime boots to
      readiness with the baseline loaded eagerly, copied, and frozen, and the
      composition preview for the resolved boot agent shows the baseline
      entries alongside the D-414 durable/personal tiers — one strict
      resolver, one preview, no parallel path — reporting `boot|revision|
      both` plus `boot_pack_set_hash` under authority/reach gating.
- [ ] A malformed, unresolvable, or un-importable baseline entry fails the
      boot loud before readiness; an unresolvable default agent fails loud
      rather than inventing a boot identity; headless `RunOnce` fails loud
      when `boot_agent_packs` is configured.
- [ ] The loader/composer performs zero durable writes, zero admin pack
      verbs, zero lifecycle writes, and zero config revisions: skill rows,
      agent-config revisions, lifecycle records, and admin verbs are all
      provably untouched by boot with a baseline declared (asserted by a
      not-invoked spy / store idempotence, not by timing);
      `EnsureBootAgentLifecycle` remains separate, unchanged, and MAY write a
      revision.
- [ ] The boot baseline binds exactly to the resolved `(tenant, boot_agent_id)`;
      a different tenant or a non-default agent never composes it, and no
      placeholder/wildcard identity is ever served.
- [ ] The baseline and the agent's active durable operator-pack revision
      merge into ONE combined operator tier FIRST: same canonical name + same
      semantic hash dedupes as `source=both`; differing hash fails loud;
      exactly 256 unique combined items cap; every declared tenant-agent
      active revision is pre-read before readiness; the run-start conflict
      defense is retained; the combined tier applies LAST over base/user/
      session skills, with caller-name collisions resolved by the
      operator-tier-last rule.
- [ ] Protocol mutation and removal verbs refuse every boot-declared baseline
      name with the canonical typed error and no partial effect: `upsert` and
      every proposal commit (replay/prepared/publish) and rollback/activation
      reject a boot-owned name even at equal hash; removal may delete an
      actual legacy durable revision shadow while leaving boot; a boot-only
      remove is a typed read-only refusal, never false success;
      `agent_packs.list` remains durable-revision authoring only; config
      removal removes boot only on the next deployment and a legacy durable
      revision remains; in-flight snapshots retain captured bytes and hash.
- [ ] The deterministic set hash over the normalized baseline entries appears
      in the run snapshot and the composition preview and is stable across
      restarts for an unchanged config file.
- [ ] Production and the devstack resolve the same loader path; the devstack's
      synthetic boot agent composes the baseline exactly like a production
      boot agent.
- [ ] Required-tool validation applies only after the static catalog/policy
      wrapping and against the granted-scope ceiling, with no invented
      identity; the read-only preview Protocol path,
      clients/manifest/generated docs, minimal Console and CLI consumers
      (D-415), config docs/example, operator skill, and smoke ship with the
      phase.
- [ ] N>=100 concurrent mixed-run compositions under `-race` against one
      shared resolver show no context bleed, no cancellation cross-talk, no
      goroutine leak, and byte-identical snapshots for identical inputs, with
      identity, reach, and retirement gates included.
- [ ] `EnsureBootAgentLifecycle` is separate and unchanged; the baseline loader
      and composer themselves perform no revision writes, and no wording in
      this phase claims startup performs no revision writes whatsoever.

## Files added or changed

- `internal/config/` — boot-declared baseline schema (config-file-relative
  paths, include directory, entries, declaration/item/file/aggregate bounds)
  + validation
- `internal/skills/` — the strict eager immutable baseline loader using the
  ONE existing importer/validator; normalized entry set + deterministic set
  hash; the strict merge of boot baseline + active durable revision
  (`source=both` dedupe, differing-hash conflict, 256-item cap)
- `internal/runtime/serve/` — boot wiring (loader runs before readiness,
  pre-read of every declared tenant-agent active revision, run-start conflict
  defense), boot-owned mutation/remove guards, `EnsureBootAgentLifecycle`
  separation
- `internal/runtime/agentcfg/` — the shared strict effective-composition
  resolver + preview extension (D-411/D-414 path) used by boot preflight,
  run, and preview alike, folding in the boot baseline
- `internal/protocol/` — the read-only composition-preview Protocol path for
  the boot base (clients, manifest, generated docs)
- `web/console/` and `cmd/harbor/` — minimal Console and CLI consumers
  (D-415)
- `harbortest/devstack/` — single-path verification (devstack resolves the
  same loader)
- `docs/CONFIG.md`, `examples/` — config docs and example
- `docs/skills/` — operator skill
- `test/integration/boot_operator_skill_baseline_test.go`
- `docs/glossary.md`, `docs/decisions.md` (D-427), `docs/plans/README.md`,
  `RFC-001-Harbor.md`, and `CHANGELOG.md`
- `scripts/smoke/phase-248.sh`

## Public API surface

- No new mutation Protocol method or admin verb: the D-414 read-only
  composition-preview Protocol surface (typed not-found/denied/unavailable
  states, D-311) is completed for the resolved boot/default agent, with
  clients/manifest/generated docs and minimal Console and CLI consumers
  (D-415).
- One shared strict effective-composition resolver + preview used by boot
  preflight, run, and preview alike; the preview reports `boot|revision|
  both` and `boot_pack_set_hash` under authority/reach gating, with no
  lifecycle materialization.
- A config schema addition for the boot-declared baseline (restart-required,
  config-file-relative paths; one relative include directory with one
  case-sensitive top-level regular UTF-8 `SKILL.md`; resource-free),
  documented in `examples/`.

## Test plan

- **Unit:** loader strictness matrix (malformed/unresolvable/un-importable
  entries fail loud); config-file-relative path resolution (never CWD);
  include-directory rules (one case-sensitive top-level regular UTF-8
  `SKILL.md`, resource-free); traversal/recursive-discovery/symlink/hardlink/
  special/duplicate/canonical-name-collision rejection under
  declaration/item/file/aggregate bounds; immutability of the eagerly copied
  frozen set; strict merge matrix (same name + same hash → `source=both`;
  differing hash → conflict; 256-item cap); deterministic set hash stability;
  boot-owned mutation/remove guard matrix (upsert/proposal commits/
  rollback/activation reject even equal hash; boot-only remove is typed
  read-only refusal; removal may delete an actual legacy durable revision
  shadow); zero-durable-write spy; `RunOnce` fail-loud test; pre-read of
  every declared tenant-agent active revision.
- **Integration:** a fresh runtime with a declared baseline boots to
  readiness and the composition preview shows the baseline for the resolved
  boot agent (real durable store + real config file); a non-default agent and
  a foreign tenant never compose it; an unresolvable default agent fails loud
  at boot; headless `RunOnce` fails loud when `boot_agent_packs` is
  configured; preview idempotence (byte-identical after N previews,
  `boot|revision|both` + `boot_pack_set_hash` stable); config removal leaves
  a legacy durable revision and removes boot only on the next deployment;
  a failure mode (forced importer failure) under `-race`.
- **Conformance:** every registered skill/config driver combination passes the
  baseline loader conformance rows; driver conformance does not claim method
  auth.
- **Concurrency / leak:** N>=100 concurrent mixed-run compositions against one
  shared resolver under `-race`, with cancellation barriers and a final
  goroutine baseline; byte-identical snapshots for identical inputs;
  identity, reach, and retirement gates included.
- **Fuzz:** config-file-relative path resolution and baseline entry decoding
  with bounded allocations and no panics.

## Smoke script additions

- Before implementation, a pending static skeleton records this plan.
- When implemented, boot a runtime with a declared baseline and assert the
  resolved boot agent's composition preview includes the baseline entries and
  reports `boot|revision|both` plus `boot_pack_set_hash`; assert a
  non-default agent and a foreign tenant do not compose it; assert a
  Protocol mutation/removal verb refuses a boot-declared name with the
  canonical typed error (and a boot-only remove is a typed read-only
  refusal); assert an unresolvable default agent fails loud at boot; assert
  headless `RunOnce` fails loud when `boot_agent_packs` is configured.

## Coverage target

- `internal/skills` baseline loader: 90%; touched `internal/config` paths:
  90%; `internal/runtime/serve` boot wiring: 90%; the shared resolver/preview
  extension: 90%; integration package: 85%.

## Dependencies

- Depends on Phases 2, 40, 232, 237, and 240.
- Gates no later phase in this wave.

## Risks / open questions

- The baseline schema must be restart-required and fail-closed: a declared
  baseline that cannot load must refuse readiness, never a silent empty set.
- The config-file-relative path rule must be identical to the existing config
  loader's relative-path resolution (the config file's directory, not the
  process CWD); reuse the existing helper rather than reinventing it.
- The strict merge of the boot baseline with the agent's active durable
  revision is load-bearing: same canonical name + same semantic hash dedupes
  as `source=both`; a differing hash is a typed boot-time conflict, never
  last-write-wins; exactly 256 unique combined items cap; every declared
  tenant-agent active revision is pre-read before readiness and the run-start
  conflict defense is retained; config removal removes boot only on the next
  deployment and a legacy durable revision remains.
- The effective-composition resolver must fold the boot baseline in without
  changing the D-411/D-414 composition order or typed states, and boot
  preflight, run, and preview must all use the same strict resolver.
- Boot-owned mutation/remove guards must refuse every Protocol write to a
  boot-declared name with the canonical typed error before any side effect —
  including `upsert` and every proposal commit (replay/prepared/publish) and
  rollback/activation at equal hash, and a boot-only remove as a typed
  read-only refusal — mirroring the boot-declared connection guard precedent
  (D-350/D-355).
- Headless `RunOnce` is explicitly unsupported and must fail loud when
  `boot_agent_packs` is configured; the preview must report
  `boot|revision|both` and `boot_pack_set_hash` under authority/reach gating
  with no lifecycle materialization.

## Glossary additions

- **Boot-declared operator skill baseline**
- **Boot baseline loader**
- **Boot baseline set hash**
- **Effective-composition resolver**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires the boot baseline, identity propagation,
      the single loader path, the strict merge (source=both / differing-hash
      conflict / 256-item cap), the `boot|revision|both` +
      `boot_pack_set_hash` preview, the headless `RunOnce` fail-loud, and a
      failure mode under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] The D-427 contract — including the `EnsureBootAgentLifecycle`
      separation, the strict merge rules, the read-only preview consumers
      (D-415), and the headless `RunOnce` decision — is recorded before
      implementation merges.
