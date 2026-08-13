# Phase 248 — Boot-declared resource-free operator skill baseline (HA-66)

## Summary

Add a boot-declared, resource-free operator skill baseline for the resolved
boot/default agent. A config-file-relative strict eager immutable loader runs
before readiness, imports every baseline entry through the ONE existing
importer/validator (fail-loud otherwise), and freezes the set for the process
lifetime — with no loader persistence, no admin verbs, no config revisions,
and no lifecycle materialization. The baseline binds exactly to the resolved
`(tenant, boot_agent_id)` pair, composes as the combined operator tier applied
last, and completes D-414's composition preview on the boot base through one
shared effective-composition resolver + preview. D-427 is the phase authority.

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
  Both compose in the combined operator tier, applied last after
  personal/session skills.

## Goals

- Declare an operator skill baseline in boot configuration, resolved relative
  to the config file, loaded strictly, eagerly, and immutably before
  readiness.
- Validate every baseline entry through the ONE existing importer/validator;
  any malformed, unresolvable, or un-importable entry fails the boot loud.
- Perform zero durable writes and expose zero new Protocol surface: no skill
  store row, no agent-config revision, no lifecycle record, no admin verb.
- Bind the baseline exactly to the resolved `(tenant, boot_agent_id)`; never
  invent a boot identity, placeholder, or wildcard.
- Compose the baseline as the combined operator tier applied last
  (personal/session → durable pack tier → boot baseline) under strict shared
  merge/collision/cap rules.
- Enforce boot-owned mutation/remove guards on every Protocol write to a
  boot-declared name.
- Carry a deterministic set hash over the normalized baseline entries in the
  run snapshot and the composition preview.
- Use ONE loader path for production and devstack, and state the
  RunOnce/embed support decision explicitly.
- Deliver ONE shared effective-composition resolver + preview that includes
  the baseline (completing D-414 on the boot base), and keep
  `EnsureBootAgentLifecycle` separate and unchanged.

## Non-goals

- No loader persistence of any kind: no SkillStore rows, no StateStore
  records, no agent-config revisions, no lifecycle materialization, no
  migration.
- No new admin verbs, Protocol methods, or Console surfaces; the baseline is
  config-declared read-only state.
- No invented boot identity: if the deployment's default agent cannot be
  resolved, the runtime fails loud at boot rather than synthesizing one.
- No `agent_id` isolation axis and no widening of the verified identity
  triple; `agent_id` remains a runtime/config entity (D-059).
- No second composition path: the D-411/D-414 resolver stays the single
  effective-composition path.
- No change to `EnsureBootAgentLifecycle`: it remains the separate mechanism
  that materializes the first empty agent-level revision when the lifecycle
  slot is absent.

## Acceptance criteria

- [ ] With a config-file-declared baseline, a fresh runtime boots to
      readiness with the baseline loaded eagerly and immutably, and the
      composition preview for the resolved boot agent shows the baseline
      entries alongside the D-414 durable/personal tiers — one resolver, one
      preview, no parallel path.
- [ ] A malformed, unresolvable, or un-importable baseline entry fails the
      boot loud before readiness; an unresolvable default agent fails loud
      rather than inventing a boot identity.
- [ ] The loader performs zero durable writes: skill rows, agent-config
      revisions, lifecycle records, and admin verbs are all provably untouched
      by boot with a baseline declared (asserted by a not-invoked spy / store
      idempotence, not by timing).
- [ ] The boot baseline binds exactly to the resolved `(tenant, boot_agent_id)`;
      a different tenant or a non-default agent never composes it, and no
      placeholder/wildcard identity is ever served.
- [ ] Composition order is personal/session → durable pack tier → boot
      baseline with the operator tier applied last; a caller-name collision
      resolves by the operator-tier-last rule, a pack-tier collision is a
      typed boot-time conflict, and the shared cap holds.
- [ ] Protocol mutation and removal verbs refuse every boot-declared baseline
      name with the canonical typed error and no partial effect.
- [ ] The deterministic set hash over the normalized baseline entries appears
      in the run snapshot and the composition preview and is stable across
      restarts for an unchanged config file.
- [ ] Production and the devstack resolve the same loader path; the devstack's
      synthetic boot agent composes the baseline exactly like a production
      boot agent.
- [ ] The RunOnce/embed support decision is stated explicitly in this plan and
      pinned by a test on the chosen path.
- [ ] N>=100 concurrent compositions under `-race` against one shared resolver
      show no context bleed, no cancellation cross-talk, no goroutine leak,
      and byte-identical snapshots for identical inputs.
- [ ] `EnsureBootAgentLifecycle` is separate and unchanged; the baseline loader
      itself performs no revision writes, and no wording in this phase claims
      startup performs no revision writes whatsoever.

## Files added or changed

- `internal/config/` — boot-declared baseline schema (config-file-relative
  paths, entries, bounds) + validation
- `internal/skills/` — the strict eager immutable baseline loader using the
  ONE existing importer/validator; normalized entry set + deterministic set
  hash
- `internal/runtime/serve/` — boot wiring (loader runs before readiness),
  boot-owned mutation/remove guards, `EnsureBootAgentLifecycle` separation
- `internal/runtime/agentcfg/` — the shared effective-composition resolver +
  preview extension (D-411/D-414 path) that folds in the boot baseline
- `harbortest/devstack/` — single-path verification (devstack resolves the
  same loader)
- `test/integration/boot_operator_skill_baseline_test.go`
- `docs/glossary.md`, `docs/decisions.md` (D-427), `docs/plans/README.md`,
  `RFC-001-Harbor.md`, and `CHANGELOG.md`
- `scripts/smoke/phase-248.sh`

## Public API surface

- No new Protocol method, admin verb, or Console surface: the baseline is
  boot-config-declared read-only state.
- One shared effective-composition resolver + preview (the D-411/D-414
  surface) now includes the boot baseline for the resolved boot/default
  agent; the preview's typed not-found/denied/unavailable states are
  unchanged (D-311).
- A config schema addition for the boot-declared baseline (restart-required,
  config-file-relative paths), documented in `examples/`.

## Test plan

- **Unit:** loader strictness matrix (malformed/unresolvable/un-importable
  entries fail loud); config-file-relative path resolution; immutability of
  the frozen set; deterministic set hash stability; merge/collision/cap rules
  (caller collision → operator-tier-last, pack collision → typed conflict,
  shared cap holds); boot-owned mutation/remove guard matrix; zero-durable-
  write spy; RunOnce/embed decision test.
- **Integration:** a fresh runtime with a declared baseline boots to
  readiness and the composition preview shows the baseline for the resolved
  boot agent (real durable store + real config file); a non-default agent and
  a foreign tenant never compose it; an unresolvable default agent fails loud
  at boot; preview idempotence (byte-identical after N previews); a failure
  mode (forced importer failure) under `-race`.
- **Conformance:** every registered skill/config driver combination passes the
  baseline loader conformance rows; driver conformance does not claim method
  auth.
- **Concurrency / leak:** N>=100 concurrent compositions against one shared
  resolver under `-race`, with cancellation barriers and a final goroutine
  baseline; byte-identical snapshots for identical inputs.
- **Fuzz:** config-file-relative path resolution and baseline entry decoding
  with bounded allocations and no panics.

## Smoke script additions

- Before implementation, a pending static skeleton records this plan.
- When implemented, boot a runtime with a declared baseline and assert the
  resolved boot agent's composition preview includes the baseline entries;
  assert a non-default agent and a foreign tenant do not compose it; assert a
  Protocol mutation/removal verb refuses a boot-declared name with the
  canonical typed error; assert an unresolvable default agent fails loud at
  boot.

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
- The effective-composition resolver must fold the boot baseline in without
  changing the D-411/D-414 composition order or typed states; a pack-tier
  collision is a typed boot-time conflict, not last-write-wins.
- Boot-owned mutation/remove guards must refuse every Protocol write to a
  boot-declared name with the canonical typed error before any side effect,
  mirroring the boot-declared connection guard precedent (D-350/D-355).

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
      the single loader path, and a failure mode under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] The D-427 contract — including the `EnsureBootAgentLifecycle`
      separation — is recorded before implementation merges.
