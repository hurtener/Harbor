# Phase 126c — USER-scope tool-policy run-start projection

## Summary

Make a user's durable, narrow-only tool/server disables actually shape the
runs of every session that user opens against an agent. The durable disable
set is ALREADY persisted: Phase 126a's user-scope revision payload
(`AgentConfigUserPayload`) carries `disabled_servers` / `disabled_tools` as
versioned fields, written atomically through the one user-tier
`agent_config.user.set_revision` verb (gated on `agent_config:user`, audited,
keyed under the caller's REAL `(tenant, user)` with `agent_id` in the session
slot). This phase is the PROJECTION-ONLY consumer of that already-durable
field: the run-start tool-exposure projection
(`ActivePlannerCatalogView`) reads the active user-scope revision via 126a's
`reg.Active(..., agentcfg.ConfigScopeUser)` read call and UNIONS its
`PausedServers()` / `DisabledTools()` into the existing grow-only exclusion
set — between the admin baseline and the ephemeral session overlay.

There is NO new store, NO new Protocol verb, NO new authority scope, and NO
binary rewiring in this phase. The narrow-only disable set is persisted and
audited at the user tier in 126a; 126c only teaches the run-start projection
to fold it in. Because the disable set is unioned (never subtracted) into the
exclusion set, the user tier can only ever SHRINK the admin-provisioned
palette, never re-widen it. This is the load-bearing run-start consumer of
126a's durable user-scope tier (the verb-family consumer 126a ships exercises
the WRITE; this projection exercises the read at run start), landing in the
same band — no primitive without a consumer (CLAUDE.md §13).

## RFC anchor

- RFC §6.16 — Agent Registry: the agent-level capability surface a non-admin
  caller may NARROW (the per-tool policy projection) but never widen, and the
  binding rule the user read keys around — `agent_id` is a registration
  identity, a record/key discriminator, NEVER an isolation principal. The
  isolation boundary stays the tuple `(tenant, user, session)`; the user read
  isolates by `(tenant, user)` and carries `agent_id` only as the per-agent
  key, never as a `WHERE`-clause isolation filter.

## Briefs informing this phase

- brief 09
- brief 11

## Brief findings incorporated

- brief 09 (per-attachment policy granularity, §46): a per-attachment /
  per-source-and-tool disable granularity (rather than a coarse per-agent
  flag) is the right shape for a non-admin tool toggle. The durable user tier
  (126a) persists exactly that — a narrow-only set naming the servers/tools to
  turn OFF, scoped to the `(tenant, user)` actor and the agent — and 126c
  projects it at run granularity, server-by-server and tool-by-tool.
- brief 09 (agent-bound durability, §168 + §198): an agent-bound OAuth token
  *"persists keyed by `(agent_id, source)` … every user invoking that agent's
  tool reuses the agent's token"* — capability state legitimately spans a
  user's sessions. The user-scope tool policy mirrors that durability: 126a
  persists a user's narrow-only disable set across sessions, and 126c makes it
  affect every one of that user's runs against the agent.
- brief 11 §404: *"Tool toggle: temporarily disable a tool for this session
  (testing the planner without one source)."* — the existing in-runtime
  surface is SESSION-ephemeral (the session overlay). 126a promotes the same
  narrow-only toggle to a DURABLE user-scope revision; 126c is the projection
  that makes the durable toggle outlive the session at run start.

## Findings I'm departing from (if any)

None. brief 09 §170 floats an open question — whether `agent_id` should become
a peer isolation principal in a `(tenant, agent, user, session)` quadruple.
That recommendation was DECLINED by the settled decision that `agent_id` is a
registration identity, not an isolation principal (RFC §6.16). This phase
follows the settled rule: the user read isolates by `(tenant, user)`
(session + run zeroed inside the registry's `ConfigScopeUser` key derivation, never in
the projection) and carries `agent_id` only as the per-agent record/key
discriminator — never as an isolation `WHERE` clause.

## Goals

- Extend the run-start tool-exposure projection (`ActivePlannerCatalogView`)
  so the active USER-scope revision's narrow-only disable set
  (`disabled_servers` / `disabled_tools`, persisted by 126a) is UNIONED into
  the exclusion set alongside the admin baseline and the session overlay. The
  user disable set is read through the EXISTING `agentcfg.Registry` parameter
  via 126a's `ConfigScopeUser` arm — so the projection signature is UNCHANGED
  (no new param), and no run-loop driver or binary needs rewiring.
- The three disable sets (admin, user, session) are UNIONED (order-independent)
  into a single grow-only exclusion set: `paused = admin ∪ user ∪ session`,
  `disabled = admin ∪ user ∪ session`, all via `unionSorted`. There is NO
  precedence for tool exposure — union is commutative and idempotent, the
  exclusion set can only ever GROW, so neither the user nor the session tier
  can re-widen past the admin-provisioned palette.
- Read the durable user revision with the run's FULL verified
  `(tenant, user, session)` identity; the `ConfigScopeUser` storage key is
  derived (session + run zeroed) ONLY inside the registry, never in the
  projection. A run reaching the projection with an incomplete triple fails
  loud at the user read (`identity_required`), never silently skips the user
  tier.
- Preserve the proven privilege boundary intact: adding a NEW MCP connection
  stays admin-only + fail-closed (`CodeScopeMismatch`). This phase adds NO new
  Protocol verb, NO new authority scope, NO new store, and NO connection-add
  path — it is purely a read+union in the run-start projection.

## Non-goals

- **The durable user-scope WRITE surface, the user authority scope, the user
  verb family, and the user revision audit** — those ship in Phase 126a (the
  one durable user-scope write surface for the band). 126c writes nothing and
  audits nothing; it reads 126a's active user revision and projects the
  disable set. (126a audits every user-scope write under the real
  `(tenant, user)` author anchor.)
- A new `useroverlay` store, a `WithUserOverlay` wiring option, an
  `ErrUserOverlayUnavailable` typed error, or new `get/set_tool_policy` verbs.
  The earlier draft of this phase proposed a duplicate user-scope store and a
  second write path; both are DELETED — 126a's user revision already persists
  the narrow-only disable set, so a second store would be a §13
  "two parallel implementations" smell and a second auth-tier seam.
- A durable user-scope PROMPT layer — that is the sibling Phase 126b
  (also projection-only, projecting `user_prompt`). 126c is tool exposure only.
- Any widening / enable capability at the user tier. The user payload
  (`AgentConfigUserPayload`, 126a) carries no enable field; the projection is
  union-only (narrow-only is structural).
- A tenant-scope tool policy (a still-broader tier) — out of scope.
- Changing the admin tool-exposure surface (`agent_config.set_tool_exposure`)
  or the connection-add lifecycle — both unchanged.
- Introducing `agent_id` as an isolation principal (declined — RFC §6.16).

## Protocol version

No `ProtocolVersion` bump, and in fact no Protocol-surface change at all in
this phase. 126c adds no method, no wire type, and no error code — it consumes
126a's already-additive `AgentConfigUserPayload.DisabledServers` /
`DisabledTools` fields at run start. Per `internal/protocol/types/version.go`,
a new method / capability / optional wire field is a Minor-class,
backward-compatible addition and a Major bump is reserved for a breaking
change; 126c does neither, so `ProtocolVersion` holds at `0.1.0`. RFC §5.3
governs ONLY the trip-wire that *bumping* the pinned constant is an RFC change
— which this phase does not do.

## Acceptance criteria

- [ ] `ActivePlannerCatalogView` reads the active USER-scope revision via
      `reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID,
      agentcfg.ConfigScopeUser)` and unions its `PausedServers()` /
      `DisabledTools()` into the exclusion set alongside the admin
      (`ConfigScopeAgent`) baseline and the session overlay. The projection's
      EXPORTED signature is UNCHANGED — the user disable set flows through the
      existing `reg agentcfg.Registry` parameter, so no run-loop driver
      (`cmd/harbor/cmd_dev_runloop.go`, `harbortest/devstack/devstack.go`) and
      no binary needs rewiring.
- [ ] The three disable sets are UNIONED (order-independent) into a grow-only
      exclusion set: `paused = admin ∪ user ∪ session`,
      `disabled = admin ∪ user ∪ session`, all via `unionSorted` (commutative,
      idempotent — the set can only GROW). A user `disable` of a tool the
      admin already excluded is inert; a user `disable` of an admin-exposed
      tool removes it from the next-run planner view; the user tier CANNOT
      re-enable an admin-disabled tool (there is no enable field and union
      never subtracts — narrow-only is structural).
- [ ] **`agent_id` is a record/key discriminator on the `ConfigScopeUser`
      read, NEVER an isolation `WHERE` filter.** The isolation principal stays
      the run's `(tenant, user)`; the session + run components are zeroed ONLY
      inside the registry's `ConfigScopeUser` key derivation (126a's pinned
      keying), never in the projection. A test asserts two distinct users
      sharing ONE `agent_id` get independent projections — user A's disable set
      never reaches user B's run — proving isolation keys by `(tenant, user)`,
      not by `agent_id`.
- [ ] The projection passes the run's FULL verified `(tenant, user, session)`
      triple to the user read. The run-loop wire edge already enforces
      `identity.MustFrom` (the full triple is mandatory to start a run); a run
      reaching the projection with an INCOMPLETE triple fails loud at the user
      read (`identity_required` from the registry's identity-mandatory guard),
      never silently skips the user tier or falls through to the unfiltered
      view (CLAUDE.md §13). A reject-missing-session test covers this.
- [ ] Persistence-across-sessions: a user revision carrying `disabled_tools`
      set (via 126a's `user/set_revision`) under `(tenantA, userA, sessionX)`
      is reflected in a run started under `(tenantA, userA, sessionY)` (a
      DIFFERENT session, same user) — the toggle survives the session, because
      the `ConfigScopeUser` key zeroes the session.
- [ ] Cross-user / cross-tenant isolation: user A's user-scope disable set
      never affects user B's runs (different `user_id`) and never crosses
      tenants. A concurrent N≥10 multi-user run stress asserts no cross-talk.
- [x] Adding a NEW MCP connection (`agent_config.add_mcp_connection`, esp.
      stdio) stays admin-only + fail-closed (`CodeScopeMismatch`): a non-admin
      caller is rejected before dispatch — unchanged by this phase. 126c adds
      NO verb, NO scope, NO store, and NO connection-add path.
      **Deviation (§4.3):** the shipped `scripts/smoke/phase-126c.sh` is
      intentionally STATIC-ONLY — this projection-only phase owns no new route,
      so the live connection-add boundary is exercised upstream by
      `scripts/smoke/phase-92f.sh` / `92h.sh` / `92n.sh` / `92m.sh` (the phases
      that own the connection-add surface), not re-asserted here. No coverage
      hole; the live appendix below is retained as design intent.
- [ ] 126c adds NO new Protocol method / wire type / error code and NO new Go
      store: the single-source discipline (`internal/protocol/{methods,types,
      singlesource}`) and the TS client + wire manifest are UNTOUCHED. No
      `make protocol-ts-gen` regen is needed for 126c (the disable fields are
      126a's `AgentConfigUserPayload`).
- [ ] `scripts/smoke/phase-126c.sh` statically asserts the projection's
      `ConfigScopeUser` read + the three-set union, live-asserts that
      connection-add stays admin-only, FAIL = 0, and SKIPs on builds without
      the surface (404/405/501 → SKIP).

## Files added or changed

```text
internal/runtime/agentcfg/projection/
  projection.go            # ActivePlannerCatalogView: + ConfigScopeUser read, three-set union into exclusion
  user_tier_test.go        # user-tier narrow-only + admin-survives + cannot-re-enable + agent_id-not-isolation + reject-missing-session
test/integration/agentcfg_user_policy_test.go # E2E: persistence-across-sessions via projection + cross-user isolation + narrow-only + ≥1 failure mode + N>=10 multi-user stress
scripts/smoke/phase-126c.sh
docs/plans/phase-126c-user-scope-tool-policy-overlay.md
docs/decisions.md          # D-258
docs/glossary.md           # "user-scope tool-exposure projection"
docs/plans/README.md       # Phase 126c row Pending (V1.6) -> Shipped (on land) + detail-block stub
```

No new package, no new top-level directory; AGENTS.md §3 unchanged. No
`internal/protocol/*`, no `web/console/*`, no `cmd/harbor/*`, no
`harbortest/devstack/*` change — the projection signature is unchanged and the
user read rides the existing `agentcfg.Registry` parameter.

## Public API surface

No new EXPORTED Go symbols and no signature change. The whole phase is an
internal extension of `ActivePlannerCatalogView` in
`internal/runtime/agentcfg/projection/projection.go`. The signature stays
exactly as it is today (126a already threaded the `ConfigScope` parameter
through `reg.Active` internally; the admin call site passes `ConfigScopeAgent`):

```go
// UNCHANGED signature — the user disable set rides the existing reg parameter.
func ActivePlannerCatalogView(
    ctx context.Context,
    reg agentcfg.Registry,
    ov sessionoverlay.Store,
    agentID string,
    id identity.Quadruple,
    cat tools.ToolCatalog,
    filter tools.CatalogFilter,
) (tools.PlannerCatalogView, error)
```

The internal change folds a second registry read (the user tier) between the
admin baseline and the session overlay:

```go
// Admin exposure (ConfigScopeAgent) — unchanged baseline read.
var adminPaused, adminDisabled []string
if reg != nil && agentID != "" {
    rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
    if err != nil {
        return nil, err
    }
    if ok && rev.Payload.ToolExposure != nil {
        adminPaused = rev.Payload.PausedServers()
        adminDisabled = rev.Payload.DisabledTools()
    }
}

// User exposure (ConfigScopeUser): the durable user-scope narrow-only disable
// set. The run's full triple is passed; the ConfigScopeUser key (session + run
// zeroed, agent_id in the session slot) is derived INSIDE the registry. A read
// error fails the run loudly; a missing active user revision is the ungated
// path. agent_id is a key here, not an isolation filter — isolation stays the
// run's (tenant, user).
var userPaused, userDisabled []string
if reg != nil && agentID != "" {
    urev, uok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
    if err != nil {
        return nil, err // identity_required on an incomplete triple — fail loud, never skip the tier.
    }
    if uok && urev.Payload.ToolExposure != nil {
        userPaused = urev.Payload.PausedServers()
        userDisabled = urev.Payload.DisabledTools()
    }
}

// Session overlay (narrow-only), then the order-independent three-set union.
overlay, oerr := loadOverlay(ctx, ov, agentID, id)
if oerr != nil {
    return nil, oerr
}

// unionSorted is commutative + idempotent, so admin ∪ user ∪ session is
// order-independent; the exclusion set can only GROW.
paused := unionSorted(unionSorted(adminPaused, userPaused), overlay.DisabledServers)
disabled := unionSorted(unionSorted(adminDisabled, userDisabled), overlay.DisabledTools)
if len(paused) == 0 && len(disabled) == 0 {
    return base, nil
}
return tools.NewExclusionView(base, paused, disabled), nil
```

> **Implementation note (godoc hygiene, CLAUDE.md §13):** the comments the
> implementor lands in `projection.go` must name the FEATURE (e.g. "the durable
> user-scope narrow-only disable set"), NEVER an internal phase number,
> `D-NNN`, or `brief NN` — `internal/` source is godoc-visible and the
> drift-audit (`scripts/drift-audit.sh` + `scripts/smoke/phase-102.sh`) rejects
> phase/decision/brief references in non-test Go source. Plan prose may cite
> phase numbers; the shipped code comments may not.

The user read consumes 126a's pinned contract verbatim:

- Read call: `reg.Active(ctx, id, agentID, agentcfg.ConfigScopeUser)` (the
  scope-parameterised registry method 126a ships).
- Keying: `ConfigScopeUser` keys under the caller's REAL `(tenant, user)` with
  `agent_id` in the `SessionID` slot and the run zeroed, under the distinct
  `agentcfg.user.active` / `agentcfg.user.revision.<id>` record kinds (126a's
  pinned keying table) — derived inside the registry, not the projection.
- Field mapping: 126a's `agent_config.user.set_revision` maps
  `AgentConfigUserPayload.DisabledServers` / `DisabledTools` onto
  `ConfigPayload.ToolExposure` (`PausedServers` / `DisabledTools`), so
  `urev.Payload.PausedServers()` / `DisabledTools()` return the user disable
  set exactly as they do for the admin revision.

No wire types, method constants, or scope constants are added in this phase.

## Test plan

- **Unit (`projection/user_tier_test.go`):**
  - A user `disable` of an admin-EXPOSED tool excludes it from the next-run
    view; an admin `pause` survives an empty user revision (admin baseline is
    independent of the user tier); a user revision CANNOT re-enable an
    admin-disabled tool (the union only grows — narrow-only).
  - Order-independence: the union of admin ∪ user ∪ session is identical
    regardless of which tier is read first (assert the resulting exclusion set
    is byte-identical across permutations) — there is no precedence.
  - `agent_id`-is-not-isolation: two distinct users (`userA`, `userB`) sharing
    ONE `agent_id` get independent projections — `userA`'s disable set does not
    appear in `userB`'s view — proving the user read isolates by
    `(tenant, user)`, not by `agent_id`.
  - reject-missing-session / incomplete identity: a run identity missing a
    component makes the `ConfigScopeUser` read fail loud (`identity_required`)
    and the projection returns the error (never the unfiltered view, never a
    silent tier skip).
  - A nil registry / empty `agentID` / no active user revision is the
    backward-compatible ungated path (base view or admin-only exclusion).
- **Integration (`test/integration/agentcfg_user_policy_test.go`):** REAL
  StateStore (in-mem) + REAL `agentcfg.Registry` (with the `ConfigScopeUser`
  arm) + REAL projection wired exactly as the run loop wires it. Asserts: (1)
  persistence-across-sessions — a user revision set under session X is observed
  in a run under session Y, same user; (2) cross-user isolation — user B's run
  is unaffected by user A's disable set, and cross-tenant likewise; (3)
  narrow-only — a user revision cannot widen past the admin palette; (4)
  failure mode — a run reaching the projection with an incomplete identity
  fails closed; identity propagation asserted end-to-end; N≥10 multi-user
  concurrent-run stress; under `-race`.
- **Conformance:** N/A — 126c adds no Protocol method / wire type, so the
  `internal/protocol/singlesource` checker has nothing new to gate. (126a's
  conformance suite already runs the registry's revision matrix under both
  `ConfigScopeAgent` and `ConfigScopeUser`.)
- **Concurrency / leak:** 126c builds NO new reusable artifact (no new store —
  the registry it reads is 126a's, whose D-025 N≥100 concurrent-reuse test
  already covers it). The cross-package concurrency guarantee is the
  integration test's N≥10 multi-user concurrent-run stress (no cross-talk,
  goroutine-leak baseline restored after teardown).

## Smoke script additions

- `scripts/smoke/phase-126c.sh` (skips per the 404/405/501 → SKIP convention
  on builds without the surface):
  - **Static** — the projection folds the user tier: assert
    `agentcfg.ConfigScopeUser` is referenced in
    `internal/runtime/agentcfg/projection/projection.go`, and that the
    three-set union (`admin ∪ user ∪ session`) is present in
    `ActivePlannerCatalogView`.
  - **Live (skip-if-404)** — connection-add stays admin-only: a NON-admin
    token on `POST /v1/agent_config/add_mcp_connection` is rejected
    `scope_mismatch` (403) — the privilege boundary 126c did NOT widen (the
    negative assertion that proves the projection opened no write path).
  - **Live (skip-if-404, dependency check)** — the durable field 126c projects
    is writable through 126a's user tier: a `USER_TOKEN`
    `POST /v1/agent_config/user/set_revision` carrying `disabled_tools`
    returns 200 (skips cleanly if 126a's surface is absent on the build).
  - Uses `assert_status`, `assert_json_path`, `assert_grep_present`,
    `protocol_call`, `api_url`, `skip_if_404` from `scripts/smoke/common.sh` —
    no new curl wrappers and no new helpers (all already exist; 126a added
    `assert_grep_present` usage to the agent-config smokes).

## Coverage target

- `internal/runtime/agentcfg/projection`: ≥ 85% (maintain — the user-tier
  read + union + the new tests land in the existing package).

## Dependencies

- Phase 126a — the durable user-scope tier. This phase CONSUMES 126a's
  `reg.Active(..., agentcfg.ConfigScopeUser)` read call, the
  `AgentConfigUserPayload.DisabledServers` / `DisabledTools` versioned fields,
  and the `ConfigScopeUser` keying (real `(tenant, user)`, `agent_id` in the
  session slot, distinct `agentcfg.user.*` kinds). 126a also threads the
  `ConfigScope` parameter through `reg.Active`'s existing call sites (the admin
  read passes `ConfigScopeAgent`), so 126c only ADDS the `ConfigScopeUser`
  sibling read. This is 126a's run-start consumer, in the same band (no
  primitive without a consumer, CLAUDE.md §13). HARD dep: if 126a has not
  landed when this phase is dispatched, this phase BLOCKS on it.
- Phase 92d — the MCP pause/resume + per-tool policy projection (the
  `ActivePlannerCatalogView` union-into-exclusion narrow-only projection this
  phase extends with the user tier) and the session-overlay precedent the user
  tier mirrors.

## Risks / open questions

- **Two registry reads per run.** After this phase the projection reads the
  active revision twice (admin `ConfigScopeAgent` + user `ConfigScopeUser`).
  Each is read ONCE per run and the returned view is fresh (the
  concurrent-reuse contract). The cost is one extra StateStore point read at
  run start; acceptable and symmetric with the existing admin read. If a
  future profile shows it material, a combined-read registry helper is a clean
  follow-up — out of scope here.
- **Order-independence is the invariant, not precedence.** Folding three tiers
  into one exclusion set is safe ONLY because the operation is union: the
  earlier draft's "precedence admin ∪ user ∪ session" wording was wrong (union
  is commutative and has no precedence). The plan and the projection comment
  now say "the three disable sets are unioned (order-independent) into a
  grow-only exclusion set." A future "enable" at any tier would break this and
  is explicitly out of scope; the data model (no enable field in
  `AgentConfigUserPayload` or the session overlay) is the structural guard, the
  union is the second.
- **`ConfigScopeUser` keying lives in 126a.** This plan consumes 126a's pinned
  keying verbatim (real `(tenant, user)`, `agent_id` in the session slot, run
  zeroed, distinct `agentcfg.user.*` kinds, `__agentcfg__` sentinel rejection).
  The projection passes the run's full triple and lets the registry derive the
  key — it does NOT zero identity by hand. Confirm the `ConfigScopeUser`
  constant + the scope-parameterised `reg.Active` signature match 126a's landed
  API at authoring; adjust the read call only if 126a's symbol names differ.
- **No write, no audit in this phase.** The cross-session-promotion audit
  (CLAUDE.md §6 rule 4) is satisfied at the WRITE — 126a audits every
  user-scope `set_revision` under the real `(tenant, user)` author anchor. 126c
  is a read-only projection; it neither writes nor audits, so it adds no audit
  obligation.
- Full §16 brief pass (brief 09 + 11 + RFC §6.16 / §5.3) when dispatched.

## Glossary additions

- **user-scope tool-exposure projection** — the run-start projection step
  (`ActivePlannerCatalogView`) that reads the active USER-scope agent-config
  revision (Phase 126a's `ConfigScopeUser` durable variant) and unions its
  narrow-only disable set (`disabled_servers` / `disabled_tools`) into the
  grow-only tool-exposure exclusion set, between the admin baseline and the
  ephemeral session overlay. It is read-only (the write + audit are the user
  tier's, Phase 126a) and narrow-only by construction (union never subtracts).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits expected)
- [ ] All cross-references (`RFC §6.16`, `RFC §5.3`, `brief 09`, `brief 11`)
      resolve
- [ ] Coverage on `internal/runtime/agentcfg/projection` ≥ 85% (maintained)
- [ ] If multi-isolation paths changed: cross-session/cross-user isolation test
      passes — YES: persistence-across-sessions + cross-user + cross-tenant
      isolation + the `agent_id`-is-not-an-isolation-filter assertion + N≥10
      multi-user concurrency stress.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes —
      N/A: 126c builds NO new artifact (the registry it reads is 126a's,
      already covered by 126a's N≥100 D-025 test). The cross-package guarantee
      is the integration test's N≥10 multi-user stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists, wires real drivers
      end-to-end, asserts identity propagation, covers ≥1 failure mode, runs
      under `-race`.** YES — `test/integration/agentcfg_user_policy_test.go`
      wires the real StateStore + registry (`ConfigScopeUser`) + projection.
- [ ] `make protocol-ts-gen` / `make protocol-ts-gen-check` — N/A: 126c adds no
      wire type or method, so the TS client + manifest are untouched.
- [ ] If new vocabulary: glossary updated (`user-scope tool-exposure
      projection`)
- [ ] If a brief finding was departed from: N/A (no departures) — D-258 filed
      for the decision record regardless.

---

## Implementation handoff

This appendix is turnkey for the implementing agent: the exact index row, the
decisions entry, the smoke assertions, and the master-plan detail-block stub.

### (a) Master-plan `docs/plans/README.md` index row

```text
| 126c | USER-scope tool-policy run-start projection | agentcfg | §6.16 | 126a, 92d | 85% | Pending (V1.6) |
```

(Match the existing column order/format of the index table; the `Cov.` cell is
the touched-package floor stated above.)

### (b) `docs/decisions.md` entry (markdownlint-clean — blank lines around the heading and the rules)

```markdown

---

## D-258 — Phase 126c: the USER-scope tool policy is a projection-only run-start consumer of 126a's durable disable set, unioned (order-independent) into the grow-only exclusion set

A user's tool/server toggles must survive the session that set them AND shape
that user's runs against the agent. The durable disable set is ALREADY
persisted: Phase 126a's user-scope revision payload (`AgentConfigUserPayload`)
carries `disabled_servers` / `disabled_tools` as versioned fields, written
atomically through the one user-tier `agent_config.user.set_revision` verb
(gated on `agent_config:user`, audited, keyed under the caller's REAL
`(tenant, user)` with `agent_id` in the session slot). What was missing was the
run-start effect.

The decision: 126c is a PROJECTION-ONLY consumer — no new store, no new verb,
no new authority scope, no binary rewiring. The run-start tool-exposure
projection (`ActivePlannerCatalogView`) reads the active user-scope revision
via 126a's `reg.Active(..., agentcfg.ConfigScopeUser)` and unions its
`PausedServers()` / `DisabledTools()` into the existing exclusion set.

Threefold rationale. (1) **Narrow-only by construction, reusing the existing
projection.** The three disable sets — admin (`ConfigScopeAgent`), user
(`ConfigScopeUser`), session (the overlay) — are UNIONED (order-independent;
union is commutative and idempotent) into a single grow-only exclusion set.
There is NO precedence for tool exposure; the set can only GROW, so neither the
user nor the session tier can re-widen past the admin-provisioned palette. The
data model (no enable field anywhere) is the first guard, the union the second.
(2) **No duplicate store, no second write path, no extra auth tier.** An
earlier draft proposed a separate `useroverlay` store and `get/set_tool_policy`
verbs; both are DROPPED. The narrow-only disable set is persisted and audited
once, at 126a's user tier. A second store would be a §13 "two parallel
implementations" smell; a second write verb would duplicate 126a's auth gate.
(3) **`agent_id` is a record/key discriminator, NEVER an isolation filter.**
The `ConfigScopeUser` read isolates by the run's `(tenant, user)`; the session
+ run components are zeroed ONLY inside the registry's key derivation (126a's
pinned keying), never in the projection; `agent_id` rides the session slot as
the per-agent key, never a `WHERE`-clause isolation filter (RFC §6.16).
brief 09 §170's peer-principal recommendation is declined.

The privilege boundary is untouched: adding a NEW MCP connection (esp. stdio)
stays admin-only + fail-closed (`CodeScopeMismatch`); 126c opens no widening
path. This is the run-start consumer of 126a's durable user-scope tier and
lands in the same band, satisfying the no-primitive-without-a-consumer rule.
126c adds no Protocol method or wire type — it consumes 126a's already-additive
fields — so `ProtocolVersion` holds at `0.1.0` (per
`internal/protocol/types/version.go`; RFC §5.3 governs only that *bumping* the
constant is an RFC change, not done here).

```

### (c) `scripts/smoke/phase-126c.sh` assertions to add

> **As-shipped note (§4.3 deviation):** only the two STATIC assertions below
> landed in `scripts/smoke/phase-126c.sh`. The live connection-add /
> dependency-check block that follows is design intent, not shipped — the
> connection-add boundary is a pre-existing surface owned by phases
> 92f/92h/92n/92m and is asserted live in THEIR smokes; re-running it here
> would duplicate coverage for a route 126c does not own. The static checks
> guard the one thing 126c actually changes (the projection folding the user
> tier into the exclusion set).

```bash
# Static — the projection folds the user tier into the exclusion set.
assert_grep_present 'agentcfg.ConfigScopeUser' \
  internal/runtime/agentcfg/projection/projection.go \
  "projection reads the durable user-scope disable set"
assert_grep_present 'unionSorted(unionSorted(' \
  internal/runtime/agentcfg/projection/projection.go \
  "admin union user union session — order-independent, grow-only"

# Live (skip-if-404). Use the non-admin dev token for the negative
# connection-add assertion and the USER_TOKEN (carries agent_config:user, 126a)
# for the dependency check; reuse the bootstrap/issuance helper the
# agent-config smokes already use. skip_if_404 guards each call so the script
# passes on a pre-surface build.

# 1. connection-add stays admin-only — a non-admin token is rejected
#    scope_mismatch (403). The boundary 126c did NOT widen.
protocol_call POST "$(api_url /v1/agent_config/add_mcp_connection)" \
  '{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent_demo","connection":{"name":"x","transport":"stdio","command":["echo"]}}' \
  && assert_status 403 \
  && assert_json_path '.code' 'scope_mismatch'

# 2. dependency check — the field 126c projects is writable at 126a's user
#    tier (skips cleanly if 126a's surface is absent on the build).
protocol_call POST "$(api_url /v1/agent_config/user/set_revision)" \
  '{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent_demo","payload":{"disabled_tools":["noisy_tool"]}}' \
  && assert_status 200
```

(`assert_grep_present`, `assert_status`, `assert_json_path`, `protocol_call`,
`api_url`, and `skip_if_404` already live in `scripts/smoke/common.sh` — no new
helpers. The user-tier write in assertion 2 uses 126a's `USER_TOKEN`; the
connection-add in assertion 1 uses the non-admin dev token.)

### (d) Master-plan per-phase detail-block stub (`docs/plans/README.md`)

```markdown
#### Phase 126c — USER-scope tool-policy run-start projection

- **Subsystem:** agentcfg (run-start tool-exposure projection)
- **RFC:** §6.16 (agent-level capability surface; agent_id is not an isolation principal)
- **Deps:** 126a (durable user-scope tier + ConfigScopeUser read + disable-set fields — consumed), 92d (MCP pause/resume + per-tool policy projection — extended)
- **Decision:** D-258
- **Delivers:** a PROJECTION-ONLY extension of the run-start tool-exposure
  projection (`ActivePlannerCatalogView`): it reads the active USER-scope
  revision via 126a's `reg.Active(..., ConfigScopeUser)` and unions its
  narrow-only disable set into the exclusion set
  (`admin ∪ user ∪ session`, order-independent, grow-only). No new store, no
  new verb, no new authority scope, no binary rewiring — the user disable set
  is persisted + audited at 126a's user tier.
- **Boundary preserved:** adding a NEW MCP connection stays admin-only +
  fail-closed; `agent_id` is a record/key discriminator on the user read,
  never an isolation `WHERE` filter (isolation stays the run's
  `(tenant, user)`).
- **Coverage:** `internal/runtime/agentcfg/projection` ≥ 85% (maintained).
- **Status:** Pending (V1.6)
```
