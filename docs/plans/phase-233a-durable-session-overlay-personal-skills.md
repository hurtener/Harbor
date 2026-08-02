# Phase 233a — Durable session overlay and personal-skill correction

## Summary

Correct the session-overlay and personal-skill ownership model before retirement
ships. Session overlays and agent-owned session-personal Skill bodies become
StateStore records fenced by agent lifecycle and session erasure; a single
per-run resolver keeps Directory and all skill read tools consistent during a
safe rolling cutover from legacy `ScopeSession` bodies.

## RFC anchor

- RFC §6.7.
- RFC §6.9.
- RFC §6.11.
- RFC §6.13.
- RFC §6.16.

## Briefs informing this phase

- brief 04
- brief 05
- brief 06

## Brief findings incorporated

- brief 04 §1 and §3: skills remain identity-scoped, capability-filtered, and
  use one planner-facing retrieval surface rather than divergent readers.
- brief 05 §1: StateStore and SessionRegistry are the durable identity
  backbone; every V1 persistence driver carries the same mandatory contract.
- brief 06 §3: erasure/audit outcomes use canonical redacted events and do not
  expose a privileged internal view.

## Findings I'm departing from (if any)

- None. The older process-local-overlay premise is corrected by D-400; it was
  not a brief finding to preserve.

## Permanent implementation deviations

- The resolver does not use a portable frozen-candidate full-text proxy. Its
  base is the configured mandatory `SkillStore`, whose
  `SearchSnapshot(ctx, id, query, candidates, limit)` implementation ranks
  only the immutable composed candidates while retaining the actual selected
  driver's full-text availability, tokenizer, ranking, and fallback ladder.
  The LocalDB driver builds a connection-local FTS5 view; PostgreSQL uses a
  values CTE through its `to_tsvector`/`to_tsquery` path. This avoids labeling
  a custom contains scorer as FTS5. Semantic snapshot search keeps the
  established deterministic most-recent 256-candidate ceiling, so one billed
  embedding batch is at most 257 texts including the query.
- `StateStore.ListKindForIdentityBounded` is a new mandatory public API across
  in-memory, SQLite, and PostgreSQL drivers. The state-only resolver asks for
  its owned-record cap plus one and rejects overflow before candidate search or
  embedding. A resolver-side length check after `ListKindForIdentity` would
  still materialize an unbounded identity prefix and is therefore insufficient.

## Goals

- Make the session overlay a cross-process-safe StateStore CAS consumer,
  fenced by lifecycle and pending/terminal session-erasure state.
- Store each agent-owned session personal skill as a separately validated,
  logically deletable StateStore record under the session triple; that one
  record is authoritative membership as well as the body.
- Give Directory and `skill_get`, `skill_list`, and `skill_search` one trusted
  per-run composite view across owned records, rolling legacy fallback, and
  the existing shared scopes.
- Make exact legacy `ScopeSession` deletion a ledgered, idempotent session
  erasure step without changing durable `ScopeUser` behavior.
- Give all overlay/personal/composite reads a stable before/after lifecycle and
  erasure-fence proof rather than relying on write-side CAS alone.
- Migrate only explicitly admitted tenants through a resumable deterministic
  StateStore scan, and verify a quiescent legacy source before cutover.

## Non-goals

- No StateStore collection-cardinality promise: the store can atomically
  compare slots and save one record, not reserve a bounded collection. This
  phase validates per-record payload/name/body bounds and per-request result
  limits only.
- No migration edits, destructive rewrite of existing shared bodies, or
  retirement/deletion of shared or unattributable legacy bodies.
- No change to `ScopeUser` durability, visibility, or rung-precise deletion.
- No new response field; the required `session_skill_cutover_pending` error is
  nevertheless an additive Protocol surface and receives full lockstep work.
- The default `dual_read` refusal of session-personal mutation is an
  intentional v1.26 compatibility/deployment change, not a transparent
  fallback. Release reviewers must accept it explicitly; the implementation
  PR must document it in config, CHANGELOG, the matching operator skill and
  docs-site stub, and example configuration.

## Acceptance criteria

- [x] Shared exported typed helpers construct the exact lifecycle active-slot
  identity/Kind, pending-erasure ledger identity/Kind, and terminal-erasure
  tombstone identity/Kind. Overlay, personal-record, erasure, and retirement
  callers use those helpers rather than duplicating string construction.
- [x] `sessionoverlay` reads/writes the session-scoped overlay from StateStore
  and uses `SaveIf`, not a process-local lock, for every mutation. Its
  expectation set contains exactly the target overlay slot, agent lifecycle,
  pending erasure ledger, and terminal erasure tombstone; stale/lifecycle/
  erasure failures persist nothing and fail loud.
- [x] Uncertain `SaveIf` convergence is write-class-specific. Overlay/personal
  body writes reread target + lifecycle + pending/tombstone erasure pair;
  cutover target/progress/final writes reread cutover target + exact
  epoch/digest/generation preconditions; retirement tombstone/progress writes
  reread lifecycle target + operation/progress expectations; cleanup item
  writes reread item target + applicable session fences. Each accepts only its
  intended EventID/content and otherwise returns uncertainty without
  unconditional compensation. Commit-then-error rows cover every class.
- [x] Overlay, personal-record, and composite resolver reads capture lifecycle,
  pending-erasure, and terminal-erasure EventIDs before point loads or
  enumeration and re-read them after. Only an equal, non-terminal
  before/after generation may return. `MaxSessionSkillReadAttempts = 3` is a
  named constant; each attempt honors context cancellation/deadline. Perpetual
  deterministic fence churn exhausts with typed `ErrSessionSkillReadUnstable`,
  never a partial result. If surfaced from an external overlay/session-skill
  endpoint, it maps to canonical `session_skill_read_unstable` (HTTP 409) and
  carries the same registry/transport/docs/Console-manifest lockstep as the
  cutover-pending code.
- [x] A per-agent session-personal record holds the complete validated Skill
  body plus schema, canonical name, ownership metadata, content hash, and
  live/tombstone state. Kind construction encodes the agent ID and hashes the
  canonical name; decoded payload identity/name/agent mismatches or a hash
  collision fail loud instead of aliasing a skill.
- [x] New session-personal writes, updates, and logical deletes write only
  agent-owned StateStore records after the fleet cutover is `state_only`. A
  tombstone then prevents a deleted name from reappearing through legacy
  fallback; each record body and mutation input is bounded and validated
  before persistence. The owned body/tombstone record is the authoritative
  membership, so each verb performs exactly one successful `SaveIf` and never
  also mutates `Overlay.PersonalSkills`; that schema-1 field is read-only
  legacy migration-eligibility input.
- [x] `skills.session_personal_cutover.tenants` is a bounded unique static
  declaration list of `{tenant_id, epoch, roster_digest,
  legacy_writers_drained}`. Empty/invalid fields, duplicate tenants, and an
  over-bound list fail boot loud. Boot iterates only valid declared tenants;
  an unlisted tenant or valid `legacy_writers_drained=false` declaration stays
  read-only `dual_read`. It creates no runtime-membership or unknown-tenant
  discovery subsystem. Boot CASes each declaration to
  `agentcfg.session_personal.cutover.<base64url(epoch)>` under exported
  `CutoverScope(tenant)`, exactly `{TenantID: tenant, UserID:
  "__agentcfg__", SessionID: "__session_personal_cutover__", RunID: ""}`.
  A malformed or declaration-mismatched durable cutover record never
  authorizes `state_only`: it remains mutation-refusing `dual_read` and emits
  a bounded loud diagnostic/error. The existing `ErrReservedUser` guard
  rejects a verified real `__agentcfg__` user; an agent ID equal to the
  session sentinel cannot alias the cutover record because the Kind namespace
  is disjoint from lifecycle/config Kinds. That bounded record has only mode,
  epoch, digest, current opaque scan continuation, counters, and generation.
- [x] The migration consumes mandatory `StateStore.ScanKindForTenant` rather
  than `ListKind`: storage-side tenant and literal `agentcfg.session_overlay.`
  filtering, bounded page limit, stable lexicographic composite-slot cursor,
  and opaque validated continuation. It is a resumable page sequence, not a
  database snapshot. A schema-1 overlay Kind is raw prefix plus agent ID with
  no delimiter, so it is never called collision-safe or selected by an
  agent-specific prefix: scan the common prefix then require exact
  `record.Kind == LegacyOverlayKind(agentID)` before load/mutation. Existing
  raw Kind/payload remain readable/migratable, and `a`/`ab` is a no-overmatch
  adversarial row. Only new encoded personal-record Kind helpers are
  collision-safe exact per-agent prefixes.
- [x] Once the operator drains old writers, new code never mutates schema-1
  `Overlay.PersonalSkills`; each paged overlay copy handles every eligible
  referenced legacy body under that overlay's own triple with target,
  lifecycle, pending-erasure, and terminal-erasure expectations. The owned
  personal record itself persists the per-name copy marker `{epoch,
  legacy_content_hash}`. The cutover record never stores per-overlay results;
  retirement/erasure outcomes are derived from durable terminal fences. A
  restart resumes the continuation. Before the final `state_only` CAS, a fresh
  paged verification pass proves every currently eligible schema-1 reference
  has an exact copied marker or a terminal fence.
- [x] Until that verification succeeds, legacy `ScopeSession` rows remain
  authoritative and requests stay read-only `dual_read`: per-agent copies are
  non-authoritative and every session-personal mutation returns the canonical
  `session_skill_cutover_pending` Protocol error (HTTP 409). It maps from a
  Go sentinel and is added to the canonical error registry, stream mapping,
  error matrix, generated Protocol docs, Console types, and regenerated
  `wire-manifest.gen.json`. An old writer after an early copy is therefore
  still visible, never silently lost.
- [x] A narrow read-only `SessionSkillResolver` replaces direct `SkillStore`
  reads at Directory and `skill_get`, `skill_list`, and `skill_search`. Runtime
  assembly builds an immutable per-run resolver snapshot from the run's
  identity, selected agent ID, and active config revision; it is passed as an
  argument or `RunContext` value, never stored mutably on a shared artifact.
  `ActiveSkillViews` is removed or relocated into this resolver's one
  membership calculation, so neither overlay names nor a second projection
  can become an independent membership authority. General Directory/tools
  compose `ScopeUser` and higher rungs; the session-only Protocol method does
  not.
- [x] `agent_config.session.skills.list` returns only owned/legacy session
  personal rows, never a `ScopeUser` union. All session-overlay responses
  dynamically project current personal names: owned record names in
  `state_only`, eligible legacy names in `dual_read`. Upsert/delete reload the
  resolver view before responding, and never persist that projection back to
  `Overlay.PersonalSkills`, so responses cannot become stale.
- [x] The resolver delegates frozen-candidate search to the configured
  mandatory `SkillStore` driver, never a portable contains scorer. Default
  retrieval preserves that driver's true full-text availability and behavior
  (LocalDB FTS5 or fallback regex/exact; PostgreSQL full-text then the same
  tail), with result paths truthful to the producing engine. Semantic retrieval
  is opt-in, ranks only the frozen composed view, deterministically selects the
  most-recent 256 candidates, honors cancellation through candidate/text/cosine
  loops, and never bills an embedding batch above 256 candidates plus query.
  State-only owned-record enumeration is storage-bounded to that cap plus one
  and rejects overflow before candidate search/embedding.
- [x] `SkillStore.DeleteSessionScope(ctx, identity.Quadruple)` is mandatory,
  identity-exact, and idempotent across every driver. Session erasure records
  its completion in the durable ledger before `StateStore.DeleteScope`; it
  deletes only legacy `ScopeSession` rows and leaves `ScopeUser`, project,
  tenant, and global rows intact.
- [x] Retirement's fixed manifest distinguishes cleanup classes. New encoded
  personal records use a collision-safe exact per-agent Kind prefix; legacy
  overlays use the tenant-bounded common overlay prefix plus exact
  `LegacyOverlayKind(agentID)` equality, never an agent-specific raw prefix.
  After the lifecycle tombstone freezes the owned keyset, it uses paged
  `ScanKindForTenant`, then logically changes each result under its own full
  identity; the agent ID is never an identity predicate, and unowned/shared
  legacy bodies are retained outside session erasure's exact sweep. The
  overlay remains schema-1-compatible with no separate retirement tombstone:
  lifecycle is terminal, and no retirement path uses unconditional `Delete`.

## Files added or changed

- `internal/agentcfg/sessionoverlay/`
- `internal/skills/` including the per-run composite resolver and Directory
- `internal/skills/{drivers,conformancetest}/`
- `internal/skills/tools/` and `internal/runtime/agentcfg/` skill read paths
- `internal/config/{config.go,loader.go}`, `examples/dev.yaml`, and
  `CHANGELOG.md`
- `internal/sessions/erasure.go` and erasure tests
- `internal/state/` typed record helpers plus `ScanKindForTenant` and
  `ListKindForIdentityBounded` mandatory-driver triads and conformance; no
  migration files
- `test/integration/session_personal_skills_test.go`
- `scripts/smoke/phase-233a.sh`
- `RFC-001-Harbor.md`, `docs/decisions.md`, `docs/glossary.md`, and plan index
- `docs/CONFIG.md`, `docs/skills/configure-memory-and-skills/SKILL.md`, and
  its docs-site stub/navigation
- `internal/protocol/{errors,conformance,singlesource,transports/stream}/`,
  generated Protocol docs, and `web/console/src/lib/protocol/` manifest/types

## Public API surface

- `SkillStore.DeleteSessionScope(ctx context.Context, id identity.Quadruple) error`.
- A typed session-personal record/resolver interface injected at the runtime
  boundary; callers supply the identity triple and agent ownership metadata,
  never an agent-scoped storage identity.
- Exported typed slot/fence helper(s) shared by sessionoverlay, session
  erasure, and retirement; they return the precise StateStore identities and
  Kinds used in `SaveIf` expectations.
- `ScanKindForTenant` and its opaque continuation/page types as a mandatory
  StateStore API.
- `ListKindForIdentityBounded(ctx, id, literalKindPrefix, limit)` as a
  mandatory StateStore API for storage-enforced identity-local admission caps.
- Mandatory `SkillStore.SearchSnapshot` driver seam for immutable
  run-start candidate views; no token URL, credential, or runtime
  configuration surface is added by this phase.
- `skills.session_personal_cutover.tenants` plus `CutoverScope(tenant)` and a
  bounded typed cutover record. `CutoverScope(tenant)` is exactly
  `{TenantID: tenant, UserID: "__agentcfg__", SessionID:
  "__session_personal_cutover__", RunID: ""}`; no Protocol method or response
  field is added.
- Read-only `SessionSkillResolver` and immutable `RunSkillSnapshot` injection
  seam for Directory and generic skill tools.
- `ErrSessionSkillCutoverPending` and canonical
  `CodeSessionSkillCutoverPending` / `session_skill_cutover_pending` Protocol
  code (HTTP 409).
- `MaxSessionSkillReadAttempts`, `ErrSessionSkillReadUnstable`, and canonical
  `CodeSessionSkillReadUnstable` / `session_skill_read_unstable` Protocol code
  (HTTP 409) for externally observable retry exhaustion.

## Test plan

- **Unit:** Kind encode/decode/collision guards; shared slot/fence helper
  identities and Kinds; body/name/request bounds;
  static cutover configuration rejects empty/invalid fields, duplicate tenants,
  and over-bound lists at boot; a malformed or declaration-mismatched durable
  cutover record remains mutation-refusing `dual_read` with a loud diagnostic;
  `CutoverScope(tenant)` has the exact reserved quadruple and its disjoint Kind
  namespace prevents an agent ID equal to the session sentinel from aliasing;
  overlay and personal four-slot expectation construction; tombstone
  precedence; single-`SaveIf` personal mutation with no overlay mutation;
  before/after read-generation retry/fail-closed rows; actual driver-owned
  frozen-view full-text versus regex/exact fallback behavior, semantic
  256-candidate/257-text batch cap and cancellation, bounded state-only owned
  admission, stable pagination, `ScopeUser` preservation,
  raw-agent schema-1 overlay compatibility including `a`/`ab`, dynamic overlay
  projection, class-specific commit-then-error exact-re-read convergence, and
  three-attempt perpetual-fence-churn exhaustion with cancellation/deadline.
- **Integration:** real in-memory/SQLite StateStore plus real SkillStore and
  session erasure ledger prove owned writes, schema-1 fallback/copy, restart,
  exact legacy sweep, identity propagation, and one forced condition/error
  path under `-race`. Missing and valid-but-undrained cutover declarations
  remain `dual_read`; empty/invalid/duplicate/over-bound declarations fail
  boot. A malformed or declaration-mismatched durable record stays
  mutation-refusing `dual_read` with a loud diagnostic/error; a valid
  epoch/digest/drained declaration is persisted and restart-stable. A
  scope/key-collision row pins the exact control quadruple and proves an agent
  ID equal to its session sentinel cannot alias the disjoint cutover Kind.
  A fault/restart during the deterministic paged tenant scan
  proves continuation validation, no duplicate/missed copy, and no snapshot
  assumption. A final fresh verification pass proves all currently eligible
  schema-1 references copied or terminal before `state_only`. Commit-then-
  error injections separately cover overlay/personal, cutover, retirement,
  and cleanup-item convergence, accepting only each class's exact intended
  bytes/event IDs after reread. Session list is tier-only, general resolver
  tools still compose `ScopeUser`, and every overlay response dynamically
  projects names. Both canonical 409 sentinels appear in the error-matrix/
  Console-manifest lockstep after `make protocol-ts-gen` and
  `make protocol-docs-gen`. A fence change during resolver enumeration cannot
  return an inaccessible row; perpetual churn exhausts after three attempts.
- **Conformance:** every SkillStore driver passes `DeleteSessionScope` exact
  identity/idempotency/rung-preservation rows; all StateStore drivers exercise
  the same four-slot expectation failures.
- **Concurrency / leak:** N>=100 shared overlay/resolver operations and two
  independent SQLite/Postgres instances race update/delete/retirement/erasure
  with barriers (no sleeps), asserting one winner, no cross-session bleed, no
  cancellation cross-talk, page/restart convergence, and goroutine baseline.

## Smoke script additions

- Run the focused resolver, sessionoverlay, SkillStore erasure, and
  session-erasure ledger tests with a no-match-fails guard.
- Assert the plan retains no migration-file target and that the smoke reports
  its static test names before Phase 233a ships real live assertions.

## Coverage target

- `internal/agentcfg/sessionoverlay`: 90%; `internal/skills` and resolver/
  tools packages: 85%; `internal/sessions`: 90%; touched SkillStore drivers
  and conformance package: 85%.
- As built, `internal/agentcfg/sessionoverlay` is 90.3% and LocalDB is 85.4%,
  including `SearchSnapshot` at 94.1%. The conformance harness is 86.7% when
  its real LocalDB happy paths and adversarial self-tests are merged in one
  `-coverpkg=./internal/skills/conformancetest` profile. The adversarial matrix
  injects contract violations and asserts the harness rejects each one, so its
  failure-reporting branches are exercised without accepting a broken driver.
  PostgreSQL coverage remains CI-only: without `HARBOR_PG_DSN`, its
  real-driver tests skip and direct local coverage is not representative.

## Dependencies

- 130, 221, 230, 233.

## Risks / open questions

- Legacy session bodies lack agent ownership. Fallback is therefore limited to
  overlay-referenced eligible names and is copied per agent under a global
  epoch. `dual_read` retains legacy read authority and rejects session
  mutations until an admitted tenant's operator has drained old writers and a
  final fresh verification completes; the runtime intentionally does not guess
  distributed membership or unknown tenants. This is an intentional deployment
  compatibility change that must receive explicit v1.26 release-review
  acceptance and be covered in config, CHANGELOG, operator skill, docs-site
  stub, and examples. Retirement must not infer ownership and sweep legacy
  bodies.
- StateStore identity enumeration returns records rather than reserving a
  collection. Request/result/body limits protect work, but an operator-scale
  stored-record quota needs a separately designed atomic primitive.
- Semantic ranking must use the same composed view as lexical ranking; a
  separate semantic-only source would reintroduce divergent tool behavior.

## Glossary additions

- Agent-owned session personal skill record.
- Composite skill resolver.
- Tenant-bounded StateStore scan.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] Authoritative cloud `make preflight` is pending; full local preflight was
  skipped per maintainer process
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages >= stated target; sessionoverlay is 90.3%,
  LocalDB is 85.4% (`SearchSnapshot` 94.1%), and the conformance harness is
  86.7% across its real-driver and adversarial self-tests. PostgreSQL is
  measured only in its real-driver CI job.
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Reusable overlay/resolver concurrent-reuse test passes — N>=100 shared
  invocations under `-race`, with no data races, context bleed, cancellation
  cross-talk, or goroutine leak
- [x] Real-driver integration test covers identity, restart, erasure-ledger,
  and condition-failed behavior
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A; no brief departure is recorded
  above
