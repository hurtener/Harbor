# Phase 251 — v1.29.1 event-index and PostgreSQL fleet safety (HA-69)

## Summary

Deliver the combined v1.29.1 emergency hotfix represented by HA-69. Leg A
replaces the durable event read paths' payload-scan amplification with an
exact, first-class event metadata index that preserves the existing
`events.list`, `events.aggregate`, and `sessions.list` counter contracts.
Leg B makes PostgreSQL connection ownership and migration identity runtime
wide across all six Harbor-owned PostgreSQL projections: state, memory,
artifacts, skills, sessions/turns, and observability/rollups. The phase also
ships a non-destructive split-to-unified cutover verifier and operator
procedure. It is a release-blocking v1.29.1 plan, not a claim that the
unreleased candidate is already shipped.

The canonical downstream register already consumes HA-13 for
`flows.runs.list`. This emergency ask is therefore HA-69, with that
historical collision recorded in the register and in D-431; no HA-13
reallocation is implied.

## RFC anchor

- RFC §4
- RFC §5.2
- RFC §6.6
- RFC §6.7
- RFC §6.9
- RFC §6.10
- RFC §6.11
- RFC §6.13
- RFC §6.14
- RFC §6.15
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05
- brief 06

## Brief findings incorporated

- brief 04 §4.2: memory identity is mandatory and fail-closed; no
  configurable fallback may turn an incomplete identity or an unverified
  schema into a default scope.
- brief 04 §5: staged adoption and explicit safeguards are required for a
  durable rollout; the first hotfix boot must remain usable with distinct
  subsystem DSNs before optional consolidation.
- brief 05 §4 and §6: migrations are forward-only, all persistence drivers
  share conformance coverage, and clean-start/existing-database/data
  round-trip tests are binding.
- brief 05 §5: StateStore bytes are opaque to the generic store and identity
  and erasure boundaries apply to every durable record; cutover must preserve
  the owning subsystem's typed data rather than infer it from a database
  name.
- brief 06 §1 and §4: the event bus is the canonical projection and replay
  must use the same records as live delivery; a read index is an optimization
  over canonical rows, not a second source of truth.
- brief 06 §5 and §6: observability projections must remain cardinality-safe,
  bounded, replayable, and honest about loss or unavailability; large
  fixtures, restart, isolation, and failure-injection tests are required.

## Findings I'm departing from (if any)

None. D-431 extends the already-settled decisions rather than changing their
authority boundaries: D-294's cursor/read contract, D-305's shared
`HistoryReplayer` substrate and driver gate, D-426's rebuildable rollup
projection, and D-428's direct-apply/pooled-verify split all remain in force.

## Goals

- Make `events.list`, `events.aggregate`, and session counter enrichment
  perform metadata-first candidate selection, so time/identity/type filters
  do not load payloads that cannot match. A sparse or zero-match 25,000-event
  fixture must load only the bounded matching page, not all 25,000 bodies.
- Define an exact event metadata/index contract containing sequence, complete
  identity, run id, type, occurrence time, and the internal-event marker.
  Preserve tail-first sequence cursors, out-of-order `OccurredAt` semantics,
  redaction, identity isolation, and the existing `truncated`/partial
  honesty rules.
- Make index and canonical event persistence atomic or explicitly
  readiness-fenced: a crash or torn write may never make a published index
  row point at a missing body, hide a committed body from a complete read, or
  resurrect an erased session.
- Provide idempotent, restart-safe, crash-safe backfill from v1.29.0 event
  rows, with a durable source watermark and a catch-up phase before declaring
  the index complete. A corrupt, missing, or stale index fails or reports an
  honest incomplete state; it is never treated as an empty log.
- Include the runtime's observability rollup projection as a consumer of the
  same canonical sequence/index contract without turning rollups into a
  billing ledger or general-purpose TSDB.
- Replace six independent 25-open/5-idle PostgreSQL allowances with one
  runtime-owned pool manager and an operator-configurable aggregate budget.
  Equal canonical DSNs reuse one runtime-owned `*sql.DB` and close it once;
  distinct DSNs retain backward compatibility but share the same runtime-wide
  permits.
- Make the supported default one logical PostgreSQL database per runtime for
  state, memory, artifacts, skills, sessions/turns, and rollups. Existing
  split DSNs remain valid for the first hotfix boot and can be consolidated
  one runtime at a time later.
- Add finite idle expiry, bounded open/idle settings, and safe defaults whose
  explicit connection math fits the existing Render Basic-4GB instance:
  `max_connections=103`, nine durable Harbor runtimes, one overlapping
  old/new generation, bounded direct migration sessions, a documented
  Pengui/capabilities allowance, and an operator reserve, without requiring a
  plan upgrade.
- Namespace and checksum migration ledgers for all six PostgreSQL stores.
  Bind subsystem, migration filename, version, and immutable checksum; prove
  required schema objects in verify mode; adopt a correctly-shaped legacy
  database only after inspection; refuse a wrong ledger/schema loudly.
- Detect the exact legacy false-readiness shape `version=1` plus
  `state_records` and no `memory_state` when memory verify is requested, and
  classify split sources by observed schema rather than DSN/env names.
- Ship a non-destructive split-to-unified cutover verifier/tool/procedure that
  freezes or drains writes, fingerprints every source and destination
  subsystem, copies all six supported projections, reconciles row counts and
  canonical content hashes, and leaves old databases untouched until the
  operator removes them independently.

## Non-goals

- Reconfiguring, deleting, dropping, truncating, or otherwise mutating the
  Pengui fleet databases. The Harbor task supplies configuration, commands,
  checksums, and verification evidence for the parent Pengui cutover only.
- Requiring a larger Render/PostgreSQL plan, raising the server connection
  limit, or treating PgBouncer configuration as a Harbor-side fleet action.
- Making PgBouncer transaction mode session-affine. Migration apply remains
  direct/session-capable PostgreSQL on port 5432; ordinary traffic and
  read-only verification may use compatible transaction pooling on port 6432.
- Replacing the canonical event log, making the metadata index a second
  source of truth, or claiming exactly-once rollup application/billing
  accuracy.
- Removing support for distinct per-subsystem DSNs, forcing same-DSN
  consolidation at first hotfix boot, or silently allocating the full pool
  budget to each DSN.
- Editing merged migration SQL files or rewriting old migration numbers.
  Corrections and ledger adoption land through append-only runner changes.
- Adding a generic persistence framework, a new isolation principal, a
  general-purpose connection broker, a cross-runtime event catalog, or a
  public Protocol method solely for operator migration internals.

## Acceptance criteria

- [ ] **Event metadata contract:** a first-class typed projection/index stores
      sequence, `(tenant_id,user_id,session_id)`, run id, event type,
      `OccurredAt`, and the internal-notice marker, with an index on the
      supported filter/order dimensions. The canonical redacted event body
      remains authoritative and is loaded only after metadata selection.
- [ ] **Bounded event reads:** `events.list` and `events.aggregate` preserve
      D-294/D-305 response, cursor, scope, audit, filter, and `truncated`
      semantics while using metadata-first selection. Session counter
      enrichment preserves honest partial/lower-bound behavior. A deterministic
      25,000-event fixture with 168 matches in one hour and a zero-match
      fixture proves payload loads are bounded by the requested page/aggregate
      contract rather than total candidate count; default/cost sorts are
      covered too.
- [ ] **Atomicity and recovery:** event body + metadata publication is one
      mandatory StateStore conditional batch covering global sequence
      authority, immutable body, and conditional head. The triad passes one
      atomic rollback conformance contract; two independent buses publishing
      N=100+ events produce exact unique contiguous sequences, and injected
      failures at every write position expose no partial state. Legacy adoption
      floors but never lowers authority; restart, bounded conflict retry, and
      cancellation are covered. No index row references a missing body; no
      complete read omits a committed body; backfill/replay is idempotent.
- [ ] **Mixed-version event-writer barrier:** adopting a non-empty legacy
      durable event store fails closed until the operator sets
      `events.legacy_writers_drained: true` after every v1.29.0 event writer
      sharing that StateStore scope has stopped. Ordinary rolling or
      zero-downtime deployment is explicitly non-compliant; use true
      stop-before-start, suspend-then-resume, or an equivalent guarantee.
      Migration-only processes that cannot publish events are not writers;
      fresh empty and already-adopted stores remain restart-compatible.
- [ ] **Fence and commit integrity:** every durable history/index/projection
      read observes persisted cross-runtime fences and rechecks before
      exposure, using bounded fleet snapshots rather than per-event loads.
      Internal authority/fence Kinds cannot be written or erased externally
      and survive session `DeleteScope`. Only an explicit
      `state.ErrCommitOutcomeUnknown` poisons a bus; definite conflicts,
      cancellation, validation errors, and known rollbacks leave it usable.
- [ ] **Backfill, erasure, cursor:** existing v1.29.0 rows backfill by
      sequence in bounded, restart-safe batches; concurrent writes catch up
      before the index is marked complete; restart resumes without duplicates;
      malformed rows fail loudly; session erasure deletes/tombstones metadata
      and bodies together and rebuild never resurrects them. Out-of-order
      `OccurredAt`, tail-first cursors, filters, identity isolation, and
      retention/truncation are covered.
- [ ] **Rollup integration:** observability/rollups consumes canonical/index
      metadata without raw payload scans for supported queries, retains D-426
      watermark/completeness/erasure semantics, and never reports zero for
      unavailable or stale data.
- [ ] **Exhaustive six-store registry:** state, memory, artifacts, skills,
      sessions/turns, and observability/rollups each register their PostgreSQL
      pool/migration contract. A registry/AST/contract test fails when a
      future PostgreSQL store opens an independent pool, bypasses the runtime
      budget, or declares a bare/unqualified migration ledger.
- [ ] **Runtime-owned pool topology:** equal canonical DSN/database identity
      reuses one runtime-owned `*sql.DB`; stores receive non-owning handles;
      shutdown closes the shared pool exactly once after store users stop;
      post-close calls fail loudly. Distinct DSNs continue to open separate
      pools but all connection acquisition counts against one aggregate
      runtime budget. Direct standalone constructors remain available for
      backward-compatible embedders/tests when no runtime manager is passed.
- [ ] **Exact operator configuration:** add and document the restart-required
      fields `postgres.pool.max_open` (aggregate runtime permits),
      `postgres.pool.max_idle` (aggregate idle target),
      `postgres.pool.conn_max_lifetime`, and
      `postgres.pool.conn_max_idle_time` (finite, non-zero production
      default). Defaults are max-open `3`, max-idle `1`, lifetime `5m`, and
      idle-time `30s`; values are validated, observable, and cannot be
      negative, zero where forbidden, or silently expanded per store. The
      six direct 5432 migration sessions in the budget below are an
      operator/orchestrator rollout ceiling, not a runtime configuration
      field. Existing per-store `dsn` and `migration_mode` keys remain
      accepted.
- [ ] **Basic-4GB budget proof:** the operator docs and deterministic
      pool-accounting/many-runtime test pin this worst planned overlap for
      `max_connections=103`: nine runtimes × two generations × 3 open permits
      = 54; six concurrent direct 5432 migration sessions = 6; reserved
      Pengui/capabilities allowance = 12; operator reserve = 25; planned
      total = 97, leaving 6 connections below the hard cap. Steady state is
      nine × 3 + 6 + 12 + 25 = 70. The manager rejects a configuration or
      rollout reservation that would exceed 103 and the test exercises all
      nine runtimes, both DSN topologies, overlapping generations, and
      shutdown. No plan upgrade is an accepted mitigation.
- [ ] **One logical database default:** examples and operator docs show one
      canonical database/DSN per runtime for all six compatible projections,
      with explicit namespaced ledgers. Distinct DSNs remain a supported
      first-boot topology and are documented as the safe stage-one rollout;
      same-DSN consolidation is optional until the cutover for that runtime.
- [ ] **Migration connectivity:** `apply` uses direct/session-affine 5432,
      advisory lock, and append-only migrations; `verify` is read-only,
      takes no advisory lock/DDL/transaction, and may use 6432. Detect and
      fail loudly when an apply DSN is transaction-pooled or has an
      unrecognizable PgBouncer transaction-mode posture. The procedure never
      claims that a 6432 advisory lock is safe.
- [ ] **Namespaced, checksummed identity:** each of the six migration
      histories has a subsystem-qualified ledger/lock authority and records
      subsystem, migration filename, version, and immutable checksum (or an
      equivalent cryptographically-bound identity). Verify checks both ledger
      identity and required schema objects; a different subsystem's integer
      version cannot satisfy verification. Migration history is independent
      across all six stores and restart/idempotence tests prove it.
- [ ] **Legacy adoption and wrong-ledger refusal:** a correctly-shaped
      legacy database may be adopted only after the expected subsystem schema
      and old migration bodies/checksums are inspected, then the namespaced
      ledger is seeded explicitly. The exact adversarial fixture with
      `schema_migrations(version=1)`, `state_records`, and no `memory_state`
      passed to memory verify fails with an error naming expected subsystem,
      observed ledger/tables, and remediation. MPR/TAA-like sources are
      classified by schema, treated as empty/misprovisioned memory unless
      stronger evidence identifies preserved data, and never silently marked
      migrated.
- [ ] **All-six real PostgreSQL lifecycle:** a real PostgreSQL integration
      boot opens all six stores against one logical database, runs independent
      migrations, restarts idempotently, exercises concurrent workloads, and
      verifies namespaced ledgers/required tables. Separate-DSN PostgreSQL
      compatibility, shared-pool cap/close-once/post-close, race tests, and
      PgBouncer acceptance are included where the environment supplies them;
      missing external services fail or skip with an explicit non-TODO reason.
- [ ] **Safe cutover:** a CLI/tool or exact operator procedure supports
      dry-run/inspect, freeze-or-drain, direct apply, schema classification,
      bounded copy, source/destination row counts, canonical content hashes,
      receipt/body/state/revision/identity/turn-order/cursor/activity/usage/
      rollup reconciliation, and final 6432 verify. It is non-destructive,
      never infers a source from a DSN/env name, reports omitted/misprovisioned
      sources, and cannot declare success until every in-scope subsystem
      matches. Old databases are not removed by Harbor.
- [ ] **Release lifecycle:** HA-69/Phase 251/D-431 docs, config examples,
      migration/cutover commands, compatibility notes, focused tests, hosted
      CI, two independent Terra High reviews, immutable annotated v1.29.1
      tag/release/provenance/checksums, and post-tag version pin/cleanup are
      complete. Local `make preflight` is explicitly deferred per the
      emergency instruction; hosted CI is the broad gate.

## Files added or changed

- `internal/events/` — metadata/index interface, durable/in-memory/SQLite/
  PostgreSQL implementations or adapters, atomic publication, bounded query,
  backfill, erasure, watermark, and conformance tests.
- `internal/events/drivers/durable/`, `internal/events/aggregate.go`, and
  `internal/sessions/protocol/enricher.go` — metadata-first list/aggregate/
  counter consumers and honest partial behavior.
- `internal/persistence/sqlmigrate/` — namespaced checksummed ledgers,
  subsystem lock authority, legacy schema adoption, direct-apply/verify
  modes, unsafe pool detection, and tests.
- `internal/persistence/postgrespool/` (or the repository-equivalent
  persistence package) — runtime-owned DSN identity, shared `*sql.DB` handles,
  aggregate permits, lifecycle/close-once accounting, and conformance tests.
- `internal/state/drivers/postgres/`, `internal/memory/drivers/postgres/`,
  `internal/artifacts/drivers/postgres/`,
  `internal/skills/drivers/postgres/`,
  `internal/sessions/turns/drivers/postgres/`, and
  `internal/observability/rollup/drivers/postgres/` — all six registrations
  and pool/migration injection; no independent hardcoded 25/5 pools.
- `internal/runtime/assemble/`, `internal/runtime/serve/`, `cmd/harbor/`,
  `internal/config/` — runtime ownership, shutdown ordering, config loading,
  deterministic budget accounting, migration commands, and operator errors.
- `internal/persistence/cutover/` or the repository-equivalent tool package —
  schema classification, dry-run/copy/reconciliation, row/hash manifests,
  and non-destructive verification.
- `test/integration/` and affected package tests — real PostgreSQL, separate
  DSN, all-six boot/restart, race, pool-budget, migration identity, cutover,
  and optional PgBouncer acceptance fixtures.
- `docs/CONFIG.md`, `examples/harbor.yaml`, `docs/recipes/` or operator
  migration guidance, and release-facing compatibility notes — exact fields,
  connection math, 5432/6432 procedure, staged rollout, and rollback.
- `docs/plans/README.md`, `docs/decisions.md`,
  `docs/notes/downstream-asks.md`, `docs/glossary.md`, and
  `scripts/smoke/phase-251.sh` — governance, vocabulary, and static/focused
  smoke coverage.

## Public API surface

- Existing per-store `dsn` and `migration_mode: apply|verify` configuration
  remains backward-compatible; the additive operator block is:

  ```yaml
  postgres:
    pool:
      max_open: 3
      max_idle: 1
      conn_max_lifetime: 5m
      conn_max_idle_time: 30s
  ```

  All fields are restart-required. `max_open` and `max_idle` are aggregate
  runtime budgets, not six independent per-store allowances.
- The migration/cutover CLI is operator-facing and must expose explicit
  `--source`, `--destination`, `--subsystem`, `--mode`/`--dry-run`, and
  `--manifest`/verification output; exact command spelling is implementation
  owned but must be recorded in `docs/CONFIG.md` and the release handoff.
- No new wire Protocol method or identity axis is required. Existing event,
  aggregate, session, turn, rollup, and state contracts remain additive and
  backward-compatible; any incidental wire field must follow D-223/D-209
  lockstep and be listed in the implementation PR.

## Test plan

- **Unit:** metadata schema/query predicates; cursor and `OccurredAt`
  ordering; event type/internal marker; checksum canonicalization; ledger
  identity/lock names; legacy schema classifier; config validation; DSN
  canonicalization; close-once and post-close sentinels; cutover manifests and
  canonical row hashing.
- **Integration:** real PostgreSQL boots all six stores against one logical
  database, migrates/restarts/idempotently verifies, writes identity-isolated
  state/memory/artifacts/skills/turns/rollups, and exercises event list,
  aggregate, and session counters. A separate-DSN matrix proves stage-one
  rollout compatibility. Cutover runs against populated split fixtures,
  including turns' ordering/cursors/activity/usage and rollup watermarks.
  Each boundary test uses real drivers, full identity propagation, and at
  least one forced failure (wrong ledger, failed copy/hash, close, or unsafe
  DSN).
- **Conformance:** a six-store registration test requires every PostgreSQL
  store to use the runtime pool/migration contract and namespaced ledger;
  migration histories are independent. Pool accounting runs nine runtimes ×
  two generations plus direct migration reservations and Pengui/capability
  allowance under the 103-connection budget. Shared DSN reuse and distinct
  DSN aggregate permits are both asserted. Optional live PgBouncer tests use
  6432 for verify/ordinary traffic and 5432 for apply, with no transaction
  pool advisory-lock claim.
- **Concurrency / leak:** `go test -race` covers N≥100 concurrent event
  reads against one immutable index/query artifact, N≥10 concurrent runtime
  pool users, same-DSN store sharing, distinct-DSN permits, concurrent
  close/post-close rejection, backfill/restart, and no cross-session/tenant
  content bleed. Goroutine and connection counters return to baseline after
  shutdown; the shared `*sql.DB` close count is exactly one.
- **Fuzz / failure injection:** malformed cursors, metadata/body divergence,
  duplicate/unknown migration versions, checksum changes, legacy wrong-ledger
  tables, torn event/index commits, interrupted backfill, erasure during
  backfill, and source/destination hash mismatch fail loudly and leave a
  resumable, non-destructive state.

## Smoke script additions

- `scripts/smoke/phase-251.sh` is a static-only skeleton during planning and
  asserts the Phase 251 plan, HA-69 register row, D-431 decision, and exact
  operator configuration vocabulary. It intentionally skips implementation
  assertions until the runtime/index/pool surfaces land.
- The implementation update must replace the skip with static guards for all
  six Postgres registrations, namespaced/checksummed ledger helpers, cutover
  command/docs, Basic-4GB budget math, and acceptance-named tests. If a new
  Protocol/CLI route is introduced, add its focused smoke assertion in the
  same PR; no live preflight is claimed in this planning commit.

## Coverage target

- `internal/events` and durable/index query paths: ≥90% on new code, with the
  25k sparse/no-match and erasure/backfill cases counted in the package gate.
- `internal/persistence/sqlmigrate` and runtime PostgreSQL pool manager:
  ≥90% on new code, including legacy refusal, checksum/ledger identity,
  close-once, permit accounting, and direct-vs-pooled mode checks.
- Each of the six PostgreSQL driver adapters: existing package target plus
  complete registration/migration/pool injection coverage; no new independent
  25-open/5-idle path is permitted.
- `internal/persistence/cutover`, CLI/config, and runtime assembly: ≥85% on
  new code, including deterministic manifests and operator failure paths.
- Existing package coverage must not regress; hosted CI remains the broad
  coverage gate.

## Dependencies

- Phases 57, 162, 163, 171, and 174: durable event log, `events.list`,
  retention/cursor honesty, `events.aggregate`, and session counter
  enrichment.
- Phase 201 and the six-store releases: PostgreSQL skills plus state, memory,
  artifacts, turns, and rollup drivers.
- Phase 246 / D-425: durable turns ordering/cursor/activity/usage projection.
- Phase 247 / D-426: rollup watermark, completeness, rebuild, and erasure
  contract.
- D-294, D-305, D-426, D-428, and D-430 are settled neighboring decisions;
  D-431 records this emergency extension. The phase is release-blocking but
  does not alter the existing RFC or prior phase ownership.

## Risks / open questions

- **Index atomicity:** the existing body/head persistence path may not be one
  SQL transaction. Implementation must choose a transaction/batch or a
  readiness watermark and prove crash states; an index that is merely
  eventually consistent cannot claim complete `events.list`/aggregate data.
- **Generic StateStore opacity:** if event bodies remain opaque bytes, the
  metadata projection needs a typed durable companion owned by the event
  driver; it must not make StateStore a domain-specific event schema.
- **Pool permit enforcement:** `database/sql` may establish connections on
  implicit paths. The manager must account for every connection acquisition
  (including ping/bootstrap) and tests must observe actual open counts, not
  just configured `SetMaxOpenConns` values.
- **Legacy migrations:** old SQL files self-record bare integer versions.
  They are immutable. Adoption must validate real schema and checksums before
  seeding the new ledger; unknown or mismatched legacy state is a loud
  operator decision, not an automatic rename.
- **PgBouncer detection:** not every endpoint exposes its mode. Where mode
  cannot be proven, `apply` must require an explicit direct/session-affine
  declaration or fail closed; `verify` may use 6432 only because it is
  read-only and lock-free.
- **Cutover concurrency:** a copied database can diverge while writes remain
  live. The tool must require a freeze/drain barrier or produce a manifest
  marked non-authoritative; it cannot silently claim equality from a moving
  source.
- **Release timing:** hosted CI and real PostgreSQL/PgBouncer services are
  external gates. Local preflight is intentionally deferred under the
  emergency instruction; no local omission is reported as product success.

## Glossary additions

- Event metadata index.
- Runtime-wide PostgreSQL pool budget.
- Namespaced checksummed migration ledger.
- Split-to-unified PostgreSQL cutover.
- Direct-apply / pooled-verify migration posture.

## Pre-merge checklist

- [ ] `make drift-audit` passes.
- [ ] `make preflight` is **deferred to hosted CI per the emergency user
      instruction**; local preflight is not run and must not be marked green.
- [ ] `make check-mirror` passes.
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve.
- [ ] Coverage on touched packages meets the targets above.
- [ ] Multi-isolation event, persistence, cutover, and pool tests pass.
- [ ] Shared event-index and pool-manager artifacts have N≥100 concurrent
      reuse/race coverage and shutdown leak evidence.
- [ ] All six PostgreSQL stores are in the registry contract test and share
      migration/pool ownership rules.
- [ ] Wrong-ledger memory verification and legacy adoption tests fail loudly
      with remediation diagnostics.
- [ ] Basic-4GB 103-connection budget math and nine-runtime deterministic test
      pass without a plan-upgrade dependency.
- [ ] Cutover manifests prove source/destination counts and canonical hashes
      before any operator removes old databases; Harbor itself performs no
      destructive cleanup.
- [ ] New vocabulary is present in `docs/glossary.md`.
- [ ] No brief finding was departed from.
