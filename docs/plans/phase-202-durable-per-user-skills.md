# Phase 202 — durable-by-default per-user skills

## Summary

Completes the half-wired user-durable skill seam with a CLAIM-FREE
`agent_config.user.skills.*` verb family (`list` / `upsert` / `delete`) so a
plain authenticated user can author PERSONAL skills that persist across ALL of
their conversations, not just the originating session. It adds a `ScopeUser`
visibility to the skills subsystem (rows stored session-zeroed, resolved across
every session of the same `(tenant, user)`), teaches the three skills drivers to
resolve that rung, and unions durable user-skill membership into the run-start
projection so an authored personal skill stays visible even when an admin pins a
skills membership.

## RFC anchor

- RFC §6.7

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- brief 04 (§"identity triple incomplete"): "if the identity triple is
  incomplete, the operation behaves as if memory is disabled and emits an audit
  event, never returns data scoped to a default. A `require_explicit_key` toggle
  is the wrong shape — Harbor makes fail-closed the only mode, with no toggle."
  Adopted directly: `ScopeUser` rows are stored session-zeroed but identity is
  still validated at every driver boundary (`ValidateIdentity` before deriving
  the storage session); there is NO durability knob — durability rides the scope
  and the driver (in-mem ephemeral, sqlite/postgres durable).
- brief 04 (skills conflict policy): "refuse to overwrite a `Origin=PackImport`
  skill … For `Origin=Generated → Origin=Generated`, last-write-wins gated by
  `content_hash` change." Preserved unchanged for user-scope rows — a durable
  user skill is a single row per `(tenant, user, name)` at user scope (session
  zeroed in the PK), so the existing LWW/idempotency probe fires correctly
  across sessions.
- brief 04 (virtual directory): "a small, identity-scoped, capability-filtered
  snapshot … redacted before injection." The claim-free safety rests on this:
  the directory + `internal/skills/tools/redactor.go` scrub any tool a skill
  names outside the run's allowed set, so a personal skill cannot widen
  capability (`RequiredTools` is provenance, never a grant).

## Findings I'm departing from (if any)

- brief 04 defines the skill `Scope` enum as `Project | Tenant | Global` (§ the
  `Skill` struct sketch). The shipped code already extended it with
  `ScopeSession`; this phase inserts `ScopeUser` between `ScopeSession` and
  `ScopeProject`. Justification: the brief predates the session/user personal
  rungs; a user rung keyed `(tenant, user)` is the durable analogue of the
  already-shipped session rung and does NOT violate brief 04's "cross-session
  reads require an admin scope" note — a user reads only their OWN skills across
  their OWN sessions, never another user's or another tenant's. Documented in
  D-345.

## Goals

- A plain authenticated user (no admin, no `agent_config:user` claim) can author
  personal skills that survive across all of their conversations.
- The three skills drivers resolve `ScopeUser` rows across a user's sessions
  with full conformance parity.
- The run-start projection keeps durable user skills visible even under an admin
  membership pin.
- The existing ephemeral `agent_config.session.skills.*` family keeps its
  semantics unchanged.

## Non-goals

- A separate durability toggle/knob (durability is the scope + the driver).
- Cross-user or cross-tenant skill sharing / promotion (the isolation principal
  stays `(tenant, user)`).
- A Console page for durable user skills (Protocol surface only this phase).

## Acceptance criteria

- [x] `skills.ScopeUser` added between `ScopeSession` and `ScopeProject`;
  `Validate` accepts it; `StorageSessionID` zeroes the session for user scope.
- [x] localdb + postgres + the in-memory test fixture store user-scope rows
  session-zeroed and resolve them across sessions of the same `(tenant, user)`;
  non-user scopes stay session-pinned.
- [x] The skills conformance suite gains a `user_scope_cross_session` subtest all
  drivers pass (author under session A → visible from session B; NOT visible
  cross-user; NOT visible cross-tenant; cross-session delete removes it).
- [x] New CLAIM-FREE methods `agent_config.user.skills.{list,upsert,delete}` in
  `methods.go` + wire types in `internal/protocol/types` + stream routes in the
  claim-free session-safe route set.
- [x] `UserSkillsUpsert` forces `Scope=user`, writes the body, and records a
  durable membership revision at `ConfigScopeUser`.
- [x] `projection.ActiveSkillViews` unions durable user-scope membership names
  so a durable user skill survives an admin membership pin.
- [x] Capability-safety test: a user-authored skill naming a disallowed tool is
  scrubbed at injection (does NOT grant access).
- [x] Concurrent-reuse (N≥128) + cross-user isolation test under `-race`.
- [x] Integration test wires real drivers across the projection + drivers seam,
  proves cross-session durability + identity propagation + a fail-loud
  missing-identity mode.
- [x] `make protocol-ts-gen` + `make protocol-docs-gen` regenerated and committed.

## Files added or changed

- `internal/skills/skills.go` — `ScopeUser`, `Validate`, `StorageSessionID`.
- `internal/skills/drivers/localdb/{localdb,search,search_semantic}.go` — store
  session-zeroed, resolve `(session = ? OR scope = 'user')`.
- `internal/skills/drivers/postgres/{postgres,search}.go` — same for pgx.
- `internal/skills/conformancetest/conformancetest.go` — `user_scope_cross_session`.
- `internal/skills/tools/userscope_capsafety_test.go` — capability-safety.
- `internal/skills/drivers/localdb/userscope_concurrency_test.go` — D-025.
- `internal/protocol/methods/methods.go` — three methods + predicates.
- `internal/protocol/types/agentconfig.go` — six wire types.
- `internal/protocol/singlesource/singlesource.go` — canonical registration.
- `internal/protocol/transports/stream/agentconfig_handler.go` — routes.
- `internal/runtime/agentcfg/protocol/userskills.go` — service verbs.
- `internal/runtime/agentcfg/projection/projection.go` — durable-user union.
- `cmd/harbor-gen-protocol-docs`, `cmd/harbor-protocol-ts-lockstep`,
  `cmd/harbor-protocol-ts-types` — type-index + method-table rows.
- `web/console/src/lib/protocol/{agentconfig,client}.ts` + regenerated manifest.
- `docs/site/protocol/{methods,types}.md` (regenerated), `docs/skills/use-the-harbor-protocol/SKILL.md`.
- `test/integration/durable_user_skills_test.go`.
- `scripts/smoke/phase-202.sh`.

## Public API surface

- `skills.ScopeUser skills.Scope` and `skills.StorageSessionID(id, scope) string`.
- Protocol methods `agent_config.user.skills.{list,upsert,delete}` and their
  request/response wire types.
- `(*agentcfgprotocol.Service).UserSkills{List,Upsert,Delete}`.

## Test plan

- **Unit:** enum/validate; service verbs (durable across sessions, cross-user
  isolation, missing-identity fail-loud); projection durable-user union;
  capability-safety scrub.
- **Integration:** `test/integration/durable_user_skills_test.go` — real localdb
  store + StateStore-backed registry + overlay + wire handler + projection;
  cross-session durability, cross-user isolation, admin-pin survival,
  incomplete-identity → `identity_required` (401).
- **Conformance:** `user_scope_cross_session` + `delete_rung_independence` run
  against localdb + postgres (the latter pins rung-precise deletes both
  directions so an ephemeral delete can never destroy a durable user skill).
- **Concurrency / leak:** N≥128 concurrent user upserts/reads against one shared
  store, no cross-user bleed, no goroutine leak, under `-race`.

## Smoke script additions

- `scripts/smoke/phase-202.sh`: `agent_config.user.skills.upsert` (claim-free)
  then `.list` returns the skill; a cross-session `.list` still returns it;
  `.delete` removes it; each with `skip_if_404` so pre-202 builds skip.

## Coverage target

- `internal/skills`: 80%. `internal/runtime/agentcfg/protocol`: 80%.
  `internal/skills/drivers/localdb`: 80%.

## Dependencies

- Phase(s) that shipped the skills subsystem (§6.7), the agent-config registry +
  session-safe subset, and the run-start skills projection. All already shipped.

## Risks / open questions

- **Destructive ops must not cross the durability boundary (RESOLVED).** The
  READ filter unions the session + user rungs, but `SkillStore.Delete` takes a
  target `Scope` so it stays RUNG-PRECISE: an ephemeral `session.skills.delete`
  can never destroy a durable user skill of the same name, and a
  `user.skills.delete` never removes a same-named session row. The conformance
  `delete_rung_independence` subtest pins both directions across all drivers.
  (An earlier draft used a union delete — a cross-durability data-loss bug the
  review caught; fixed here.)
- Name collision across the session and user scopes for reads: `Get` prefers the
  exact-session row (deterministic); `List` returns both as distinct entries. No
  isolation risk (tenant + user stay pinned).

## Glossary additions

- **Durable-by-default per-user skill** — a personal skill authored at
  `ScopeUser`, persisting across all of a user's conversations.
- **`ScopeUser`** — skills visibility keyed `(tenant, user)`, session zeroed in
  storage.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse test passes — N≥128 concurrent invocations under `-race`.
- [x] Integration test wires real drivers, asserts identity propagation, covers
  a failure mode, runs under `-race`.
- [x] If new vocabulary: glossary updated
- [x] Brief departure justified above + D-345 filed
