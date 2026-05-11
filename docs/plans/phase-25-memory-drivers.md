# Phase 25 — SQLite + Postgres MemoryStore drivers

## Summary

Land the persistent legs of Harbor's memory persistence triad: `internal/memory/drivers/sqlite` (modernc.org/sqlite, CGo-free, WAL + busy_timeout, single-conn pool) and `internal/memory/drivers/postgres` (pgx/v5, advisory-lock-serialised migrations). Both drivers pass the same `internal/memory/conformancetest.Run` suite that Phase 23 shipped, and both round-trip the `Snapshot/Restore` byte-stably with the InMem driver. The wire envelope (`memory.Record`) is promoted to an exported type at `internal/memory/wire.go` so all three drivers marshal byte-identical bytes — the cross-driver round-trip is the load-bearing acceptance criterion.

## RFC anchor

- RFC §6.6
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05

## Brief findings incorporated

- **brief 04 §1 (memory shape; identity is first-class via `context.Context`).** Phase 25 inherits the Phase 23 surface verbatim — every `MemoryStore` method on the SQLite + Postgres drivers takes `ctx context.Context` + `identity.Quadruple`, validates the triple at the boundary, and emits `memory.identity_rejected` on the configured bus when any component is missing. The shared conformance suite drives this contract uniformly across all three drivers.
- **brief 04 §6 (Isolation conformance + cross-session no-leak).** The SQLite driver schema's composite primary key `(tenant, user, session, run, kind)` and Postgres equivalent `(tenant_id, user_id, session_id, run_id, kind)` make per-identity isolation an engine-enforced property: no Go-level filtering, every query is parameterised on the full triple. The conformance suite's `Concurrent_AllMethods_NoRace` runs N≥128 goroutines (D-025) and asserts no cross-identity bleed against each driver.
- **brief 05 §4 (forward-only migrations; embedded SQL).** Both drivers ship their schema as embedded `migrations/*.sql` files driven by an idempotent runner that records applied versions in a `schema_migrations` table. The trailing `INSERT OR IGNORE INTO schema_migrations(version) VALUES (N);` (SQLite) / `ON CONFLICT DO NOTHING` (Postgres) keeps the runner safe to re-execute. The Postgres runner additionally guards the entire run with `pg_advisory_lock` so multi-replica boots cannot race on `CREATE TABLE` / `INSERT INTO schema_migrations`.
- **brief 05 §4 (SQLite single-writer engine + busy_timeout).** The SQLite driver pins `db.SetMaxOpenConns(1)` (matching Phase 15's StateStore and Phase 18's ArtifactStore-blob settled choice) so BEGIN IMMEDIATE serialises across the Go pool — busy_timeout cannot help with inter-connection writer contention, and the conformance suite's N=128 stress would otherwise leak `SQLITE_BUSY` to callers. `journal_mode=WAL` + `busy_timeout=5000` + `_txlock=immediate` survive `database/sql`'s connection lifecycle via `_pragma=...` query params appended at open.

## Findings I'm departing from (if any)

- **Memory state lives in its OWN tables (`memory_state`), not in the StateStore's `state_records` table.** Phase 23's InMem driver writes memory records through the injected `state.StateStore` per D-027 (typed-wrapper-over-generic). At Phase 25 the persistent drivers maintain their own dedicated table per the master plan: "Your SQLite/PG drivers persist memory state to their OWN tables (not piggybacking on the state-store driver tables) — but the byte serialisation contract is the same shape so cross-driver Snapshot/Restore round-trips byte-stable." This means the SQLite/Postgres memory drivers accept the `memory.Deps.State` (mandatory per the registry's `validateDeps`) but do not use it; only `memory.Deps.Bus` is wired. This is recorded in D-034 so a later auditor doesn't flag the accepted-but-unused dep as drift. The wire envelope (`memory.Record`) is exported at `internal/memory/wire.go` so all three drivers marshal byte-identical JSON; cross-driver `Snapshot/Restore` byte-stable round-trip is a Phase 25 acceptance criterion verified by `TestSQLite_CrossDriver_ByteStableRoundTrip` + `TestPostgres_CrossDriver_ByteStableRoundTrip`.
- **Strategy=none is the only operational strategy at Phase 25.** Phase 24 (parallel) widens the InMem driver to `truncation` + `rolling_summary`; when it merges the SQLite + Postgres drivers will inherit that widening through the shared `conformancetest.Run` suite without driver-side code changes (the strategy switch in `New` will need to widen, but the on-disk wire shape already supports `Turns` via the shared envelope). Today both persistent drivers reject `truncation` / `rolling_summary` with `ErrStrategyNotImplemented`.

## Goals

- `internal/memory/drivers/sqlite/sqlite.go` — modernc.org/sqlite-backed `memory.MemoryStore` driver with WAL + `busy_timeout=5000` + single-conn pool + embedded forward-only migrations + self-registration via `init()` under name `sqlite`.
- `internal/memory/drivers/postgres/postgres.go` — pgx/v5-backed `memory.MemoryStore` driver with default connection-pool tuning + advisory-lock-serialised migrations + self-registration via `init()` under name `postgres`.
- `internal/memory/wire.go` — exported `Record` envelope + `KindMemoryState` constant; the wire format is centralised so all three drivers marshal byte-identical bytes.
- Both drivers pass the shared `internal/memory/conformancetest.Run` suite verbatim.
- `Snapshot/Restore` byte-stable round-trip across drivers: an InMem snapshot must restore into the SQLite/Postgres driver and re-read as the same canonical record (and vice versa). Pinned by per-driver `TestXxx_CrossDriver_ByteStableRoundTrip` tests.
- `internal/config/config.go::MemoryConfig` gains a `DSN string` field (`secret:"true"`); `internal/config/validate.go::validateMemory` allowlist widens to `{inmem, sqlite, postgres}` and requires `DSN` when the driver is `sqlite` or `postgres`.
- `internal/memory/registry.go::ConfigSnapshot` gains a `DSN string` field so the config-translation seam carries the value to the persistent drivers.
- `cmd/harbor/main.go` blank-imports the two new drivers (additive).
- CI: a `memory-postgres` job that mirrors `state-postgres` — postgres:16 service container, env `HARBOR_PG_DSN`, runs `go test -race -timeout 240s ./internal/memory/drivers/postgres/...`.
- `scripts/smoke/phase-25.sh` runs both drivers' Go tests under `-race` (the postgres subset skip-cleans without `HARBOR_PG_DSN`).

## Non-goals

- No `truncation` / `rolling_summary` strategies — Phase 24 owns the strategy widening (parallel phase); the persistent drivers inherit it automatically when Phase 24 lands because the wire envelope already carries `Turns`.
- No Protocol / HTTP surface — memory remains a Go-only subsystem at Phase 25 (lands in Phase 60+).
- No piggybacking on the StateStore's `state_records` table — the persistent drivers maintain their own `memory_state` table per the master plan. The injected `state.StateStore` dep is accepted but unused (D-034).
- No advisory-lock conflict detection between the state-postgres and memory-postgres migration runners — each subsystem hashes a distinct stable string (`harbor-state-migrations` vs `harbor-memory-migrations`) for `pg_advisory_lock`'s key so they never race for the same lock.

## Acceptance criteria

- [ ] `internal/memory/wire.go` exports `Record` (with JSON tags pinning the wire shape) and `KindMemoryState`. The InMem driver's previously-internal `memoryStateRecord` type aliases `memory.Record` so the byte layout is unchanged from Phase 23.
- [ ] `internal/memory/drivers/sqlite/sqlite.go` registers under `"sqlite"` via `init()`. `New` requires non-empty `cfg.DSN` and non-nil `deps.Bus`; rejects strategies other than `none` with `ErrStrategyNotImplemented`. Opens the DB with `journal_mode=WAL` + `busy_timeout=5000` + `_txlock=immediate` + single-conn pool. Runs embedded migrations idempotently.
- [ ] `internal/memory/drivers/sqlite/migrations/0001_init.sql` creates `memory_state` (composite PK on the identity quadruple + kind) + `schema_migrations` (forward-only).
- [ ] `internal/memory/drivers/postgres/postgres.go` registers under `"postgres"` via `init()`. `New` requires non-empty `cfg.DSN` and non-nil `deps.Bus`; rejects unsupported strategies; Pings eagerly; runs advisory-lock-serialised migrations.
- [ ] `internal/memory/drivers/postgres/migrations/0001_init.sql` creates `memory_state` (BYTEA bytes column, TIMESTAMPTZ updated_at) + `schema_migrations`.
- [ ] Both drivers pass `internal/memory/conformancetest.Run` — every subtest the InMem driver passes also passes against SQLite + Postgres (skip-clean without `HARBOR_PG_DSN`).
- [ ] Per-driver `TestXxx_PersistsAcrossReopens` proves the driver actually durably persists state (an explicit Snapshot/Restore on driver-instance A is visible to driver-instance B opened against the same DB).
- [ ] Per-driver `TestXxx_CrossDriver_ByteStableRoundTrip` proves InMem → SQLite + InMem → Postgres Restore is byte-stable through the shared `memory.Record` envelope.
- [ ] Per-driver `TestXxx_Migrations_AppliedOnFreshDB` + `TestXxx_Migrations_IdempotentOnReopen` exercise the migration runner.
- [ ] Per-driver concurrency stress (`TestSQLite_Memory_Concurrent_*` + `TestPostgres_Memory_Concurrent`) runs ≥100 goroutines under `-race`, asserts no errors, asserts goroutine baseline restored (D-025 supplement to the conformance suite's `Concurrent_AllMethods_NoRace`).
- [ ] `internal/config/config.go::MemoryConfig` gains `DSN string` (`secret:"true"`). The example config (`examples/harbor.yaml`) documents the new field with a commented-out template DSN.
- [ ] `internal/config/validate.go::allowedMemoryDrivers` widens to `{inmem, sqlite, postgres}`; `memoryDriversRequiringDSN` enforces non-empty DSN for the persistent drivers. Positive + negative tests in `internal/config/validate_test.go`.
- [ ] `internal/memory/registry.go::ConfigSnapshot` gains `DSN string`.
- [ ] `cmd/harbor/main.go` blank-imports the new drivers (additive).
- [ ] `.github/workflows/ci.yml` gains a `memory-postgres` job mirroring `state-postgres`. Runs `go test -race -count=1 -timeout 240s ./internal/memory/drivers/postgres/...` against the postgres:16 service container.
- [ ] `scripts/smoke/phase-25.sh` runs both drivers' Go tests under `-race`. Preflight reports `OK >= 1` for Phase 25.
- [ ] `docs/glossary.md` no new vocabulary at Phase 25 (the new symbols are concrete instances of `MemoryStore`, `MemorySnapshot`, etc., already defined in Phase 23). The wire envelope `memory.Record` is internal to the memory package and not glossary-worthy.
- [ ] `docs/decisions.md` gains entry D-034 documenting (a) memory state living in its own `memory_state` table not the StateStore's `state_records`, (b) the wire envelope being exported at `internal/memory/wire.go`, and (c) the persistent drivers accepting-but-not-using the `Deps.State` field.
- [ ] `docs/plans/README.md` flips Phase 25's status from `Pending` to `Shipped`.
- [ ] `README.md` Status table reflects Phase 25 shipped.

## Files added or changed

- `internal/memory/wire.go` (new) — exported `Record` + `KindMemoryState` constant.
- `internal/memory/drivers/sqlite/sqlite.go` (new) — driver.
- `internal/memory/drivers/sqlite/migrations.go` (new) — runner.
- `internal/memory/drivers/sqlite/migrations/0001_init.sql` (new) — schema.
- `internal/memory/drivers/sqlite/sqlite_test.go` (new) — conformance + driver-specific cases.
- `internal/memory/drivers/sqlite/migration_test.go` (new) — migration runner tests.
- `internal/memory/drivers/sqlite/concurrent_test.go` (new) — N=64 concurrency stress.
- `internal/memory/drivers/postgres/postgres.go` (new) — driver.
- `internal/memory/drivers/postgres/migrations.go` (new) — advisory-lock runner.
- `internal/memory/drivers/postgres/migrations/0001_init.sql` (new) — schema.
- `internal/memory/drivers/postgres/postgres_test.go` (new) — conformance + driver-specific cases.
- `internal/memory/drivers/postgres/migration_test.go` (new) — migration runner tests.
- `internal/memory/drivers/postgres/concurrent_test.go` (new) — N=100 concurrency stress.
- `internal/memory/drivers/postgres/testhelpers_test.go` (new) — `defaultTestTimeout` constant.
- `internal/memory/drivers/inmem/inmem.go` (modified) — aliases `memory.KindMemoryState` + `memoryStateRecord = memory.Record`.
- `internal/memory/registry.go` (modified) — `ConfigSnapshot.DSN`.
- `internal/config/config.go` (modified) — `MemoryConfig.DSN`.
- `internal/config/validate.go` (modified) — widened allowlist + DSN requirement.
- `internal/config/validate_test.go` (modified) — positive + negative coverage.
- `cmd/harbor/main.go` (modified) — additive blank imports.
- `examples/harbor.yaml` (modified) — documented new DSN field.
- `.github/workflows/ci.yml` (modified) — `memory-postgres` job.
- `scripts/smoke/phase-25.sh` (new) — driver tests under `-race`.
- `docs/plans/phase-25-memory-drivers.md` (this file).
- `docs/plans/README.md` (modified) — flip Phase 25 to Shipped.
- `docs/decisions.md` (modified) — add D-034.
- `README.md` (modified) — Status table update.

No top-level directory additions — `internal/memory/drivers/{sqlite,postgres}/` are already enumerated in AGENTS.md §3.

## Public API surface

```go
package memory

// KindMemoryState is the canonical record-kind string Harbor uses
// to route memory state in the persistence layer.
const KindMemoryState = "memory.state"

// Record is the JSON envelope every driver persists as the opaque
// Snapshot.Bytes payload. Exported so cross-driver tests + later
// driver implementations share a single source of truth.
type Record struct {
    Strategy Strategy           `json:"strategy"`
    Turns    []ConversationTurn `json:"turns,omitempty"`
}

// ConfigSnapshot gains a DSN field consumed by the SQLite + Postgres
// drivers.
type ConfigSnapshot struct {
    Driver       string
    DSN          string
    Strategy     Strategy
    BudgetTokens int
}
```

The SQLite + Postgres driver packages expose only their `New(cfg, deps)` constructors; everything else is internal.

## Test plan

- **Unit:**
  - `validateMemory` positive + negative: `sqlite` / `postgres` driver names accepted with DSN, rejected without; `inmem` still works.
- **Integration:**
  - Phase 23's `test/integration/memory_state_test.go` continues to pass (the InMem driver's contract is unchanged). The cross-driver byte-stable Restore tests live in each persistent driver's `*_test.go` since they need both drivers wired against the same `memory.Record` envelope.
- **Conformance:**
  - `internal/memory/conformancetest.Run` invoked against the SQLite driver in `internal/memory/drivers/sqlite/sqlite_test.go::TestSQLite_ConformanceSuite`.
  - Same against the Postgres driver in `internal/memory/drivers/postgres/postgres_test.go::TestPostgres_ConformanceSuite` (skips when `HARBOR_PG_DSN` unset).
- **Concurrency / leak:**
  - SQLite: `TestSQLite_Memory_Concurrent_BusyTimeoutAbsorbsContention` — N=64 goroutines × 12 ops, asserts `busy_timeout=5000` absorbs SQLITE_BUSY transparently + goroutine baseline restored.
  - Postgres: `TestPostgres_Memory_Concurrent` — N=100 goroutines × 8 ops × every method, asserts no caller-visible errors + goroutine baseline restored.

## Smoke script additions

- `scripts/smoke/phase-25.sh`:
  - `OK` when `go test -race ./internal/memory/drivers/sqlite/...` passes.
  - `OK` when `go test -race ./internal/memory/drivers/postgres/...` passes (skip-cleans without `HARBOR_PG_DSN`).
  - `SKIP` for the Protocol surface (lands in Phase 60+).

## Coverage target

- `internal/memory/drivers/sqlite`: 85%.
- `internal/memory/drivers/postgres`: 80% (the migration-runner branches that require advisory-lock contention are not exercised locally; CI's postgres:16 container exercises the happy path).

## Dependencies

- Phase 23 — `memory.MemoryStore` interface + InMem driver + conformancetest harness.
- Phase 15 — SQLite reference implementation patterns (modernc.org, WAL, busy_timeout, single-conn pool).
- Phase 16 — Postgres reference implementation patterns (pgx, advisory-lock-serialised migrations).
- Phase 18 — additional blob-style persistence patterns.

## Risks / open questions

- **Phase 24 ships in parallel.** Depending on merge order, the strategy widening may land before or after Phase 25. The Phase 25 design is robust either way: the wire envelope (`memory.Record`) already carries `Turns`, the persistent drivers reject `truncation` / `rolling_summary` today with `ErrStrategyNotImplemented`, and the shared conformance suite will widen automatically when Phase 24 merges (no driver-side change required for the strategy-none subtests; the new strategy subtests will run against all three drivers verbatim).
- **`Deps.State` accepted-but-unused is awkward.** The registry's `validateDeps` requires non-nil `State`; the SQLite + Postgres drivers do not use it. Loosening the registry-side requirement would break the existing InMem contract (which DOES use `State` per D-027). Keeping the requirement preserves backward compatibility; the persistent drivers simply hold the reference. Recorded in D-034 so a later auditor doesn't flag it as drift.
- **Advisory-lock key collision.** The Postgres state driver uses `pg_advisory_lock(fnv64aSigned("harbor-state-migrations"))`; this phase's memory driver uses `fnv64aSigned("harbor-memory-migrations")`. The keys are distinct so the runners never compete; if a future phase adds another Postgres-backed subsystem, the same per-subsystem-stable-string pattern keeps the keys non-colliding. No open RFC §11 question blocks this.

## Glossary additions

N/A — Phase 25 adds concrete driver implementations of vocabulary already defined in Phase 23 (`MemoryStore`, `MemorySnapshot`, `LLMContextPatch`, etc.). The new exported `memory.Record` envelope is a concrete in-package type, not a cross-cutting concept worth glossary-ing.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the conformance suite's `CrossTenant_Isolation` + `CrossSession_Isolation` cover this against both new drivers).
- [ ] **Concurrent-reuse test passes** — both new drivers ship N≥64 (SQLite) / N=100 (Postgres) concurrent invocation tests under `-race` plus the conformance suite's N=128 `Concurrent_AllMethods_NoRace` runs against each (D-025).
- [ ] **Cross-subsystem integration test exists** — Phase 23's `test/integration/memory_state_test.go` continues to pass. The cross-driver byte-stable Restore tests live in the persistent drivers' own `*_test.go` files and exercise both drivers + the wire envelope on the seam.
- [ ] If new vocabulary: glossary updated (N/A this phase)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-034)
