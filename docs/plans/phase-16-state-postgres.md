# Phase 16 — Postgres StateStore driver

## Summary

Land `internal/state/drivers/postgres/`: the third concrete `state.StateStore` driver in Harbor's persistence triad — and the multi-node production target. Built on `pgx` (RFC §10), forward-only migrations under `internal/state/drivers/postgres/migrations/`, parameterized queries everywhere, advisory locks for migration serialization. Like Phase 15, this driver inherits `internal/state/conformancetest.Run` verbatim — the suite IS the gate; this phase ships zero new conformance scenarios. Closes the persistence-floor work for the StateStore subsystem; downstream consumers (sessions, tasks, governance, planner, memory) get a multi-node persistence option without changing a line of consumer code.

## RFC anchor

- RFC §6.11
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (three drivers from t=0).** "Designing the interface against three backends from t=0 forces clean abstractions." Phase 16 is the third leg of the triad. The conformance suite landed in Phase 07; both SQLite (Phase 15, parallel) and Postgres consuming it verbatim — without driver-specific scenario forks — is the proof that the interface holds against the most divergent backend pair Harbor will ship.
- **brief 05 §4 (idempotency on `EventID`).** `Save` keys idempotency on the caller-supplied ULID; same-ID + byte-equal payload is a no-op, divergent payload returns `ErrIdempotencyConflict`. The Postgres driver enforces this via `INSERT ... ON CONFLICT DO UPDATE` on the composite primary key with a pre-check on the EventID secondary index.
- **brief 05 §4 (forward-only, per-driver migrations).** "Forward-only, numbered monotonically (`0001_init.sql`, `0002_*.sql`, ...) per driver. Each migration ends with a `schema_migrations` insert." Phase 16 ships exactly one migration (`0001_init.sql`) creating the records table + EventID secondary index + `schema_migrations` bookkeeping. Editing a merged migration is forbidden (AGENTS.md §13).
- **brief 05 §4 ("Postgres uses `pgx` migrations. Drivers self-register from `init()`").** Phase 16 follows verbatim.
- **brief 05 §5 (advisory locks for binding semantics).** Postgres advisory locks serialize migration execution across multiple nodes booting simultaneously — the canonical "exactly-one process applies the migration" pattern; without it, `0001_init.sql` could race when N replicas start at the same instant.

## Findings I'm departing from (if any)

- None.

## Goals

- Ship a `state.StateStore` driver named `"postgres"` registered via `init()`, persisted to a Postgres database at the DSN supplied by `config.StateConfig.DSN`.
- Use `pgx` (`github.com/jackc/pgx/v5/stdlib`) as the driver. The `database/sql` shim path keeps the seam consistent with the SQLite driver and the rest of Harbor.
- Forward-only migrations under `internal/state/drivers/postgres/migrations/` driven by an embedded migration runner; migration application protected by a `pg_advisory_lock` so multi-replica boots serialize correctly (no race on `0001_init.sql`).
- Pass `internal/state/conformancetest.Run` end-to-end with zero scenario forks. The suite's `Concurrent_SaveLoad_NoRace` (D-025) IS the concurrent-reuse gate.
- Provide a driver-local migration test that exercises clean DB → migration → round-trip end-to-end against a containerized Postgres (CI matrix; locally skipped if `HARBOR_PG_DSN` is unset, with a clear `t.Skip` reason).
- CI matrix: GitHub Actions runs the driver tests against a `postgres:16` service container; locally, tests gate on `HARBOR_PG_DSN` env var so contributors without Postgres installed don't see spurious failures.
- Boot-log integration: when `cmd/harbor` blank-imports the driver, `state.RegisteredDrivers()` should list `inmem` AND `postgres`. Phase 16 adds the blank import.

## Non-goals

- No SQLite driver (Phase 15, parallel).
- No ArtifactStore work (Phase 17, parallel).
- No `pgxpool.Pool` direct usage. `database/sql`'s pool over `pgx`'s `stdlib` adapter is sufficient at V1; switching to `pgxpool.Pool` would mean diverging from `database/sql.DB` and complicating the migration runner. If perf evidence later demands `pgxpool`, that's a Phase 16.1 / RFC update — not in scope here.
- No `JSONB` column for `Bytes`. The interface is `(Identity, Kind) → opaque bytes`; `BYTEA` is the correct shape. Switching to `JSONB` would (a) constrain `Bytes` to valid JSON (the interface explicitly does not), (b) make audit redaction's job harder (JSONB has Postgres-specific quirks), (c) leak persistence-implementation choices into consumers. Documented departure from the brief's "JSONB payloads" wording — see "Findings I'm departing from."

  *Wait*: re-read brief 05 §4 — "Postgres uses `pgx` migrations" is the operative phrase; the "JSONB payloads" line is from §1's narrative paragraph, not the design. RFC §6.11 settles `Bytes []byte`; the brief's narrative pre-dates D-027's generic surface. No departure to record — the brief's older sketch is superseded by D-027 (which the brief itself does not cite because the brief is older).

- No new `state.StateStore` methods.
- No connection-pool tuning beyond conservative defaults (`SetMaxOpenConns(25)`, `SetMaxIdleConns(5)`, `SetConnMaxLifetime(5 * time.Minute)`). Tuning is operator-visible via the future config knobs, not Phase 16's scope.
- No multi-region / read-replica logic.
- No automatic schema introspection. `schema_migrations` is the authoritative version source; the driver does not query `information_schema.tables` to "guess" state.

## Acceptance criteria

- [ ] `internal/state/drivers/postgres/postgres.go` defines a `driver` struct implementing `state.StateStore`. Compile-time assertion: `var _ state.StateStore = (*driver)(nil)`.
- [ ] `init()` calls `state.Register("postgres", New)` exactly once.
- [ ] `New(cfg config.StateConfig) (state.StateStore, error)` opens a `*sql.DB` against `cfg.DSN` (mapped to the `pgx` driver name). Empty DSN returns a clear error. Connection pool defaults: `SetMaxOpenConns(25)`, `SetMaxIdleConns(5)`, `SetConnMaxLifetime(5 * time.Minute)`. Migrations run on first open under a `pg_advisory_lock` keyed by a stable hash of `"harbor-state-migrations"`; lock released after migrations complete.
- [ ] `internal/state/drivers/postgres/migrations/0001_init.sql` creates:
  - `state_records` — primary table, columns `(tenant_id TEXT NOT NULL, user_id TEXT NOT NULL, session_id TEXT NOT NULL, run_id TEXT NOT NULL, kind TEXT NOT NULL, event_id TEXT NOT NULL, version INTEGER NOT NULL, bytes BYTEA NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`. Composite primary key `(tenant_id, user_id, session_id, run_id, kind)`. Unique index on `event_id`.
  - `schema_migrations` — `(version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())` with the trailing `INSERT INTO schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;` (RFC §9; Postgres-flavored equivalent of SQLite's `INSERT OR IGNORE`).
- [ ] Migration runner is internal to the package: reads `migrations/*.sql` via `embed.FS`, sorts by filename, applies in-order in a transaction per file, gates each file on its filename version not yet present in `schema_migrations`. Idempotent on re-run. **Wraps the entire run in a `pg_advisory_lock` so concurrent `New()` invocations across replicas serialize cleanly.**
- [ ] All five `state.StateStore` methods implemented:
  - `Save` — single transaction. Idempotency check (same EventID + identical Bytes / Version → no-op; same EventID + divergent → `ErrIdempotencyConflict`) is performed BEFORE the upsert by reading the previous row at the slot OR via `event_id` lookup. Eviction of the previous EventID happens within the same transaction when the slot's EventID changes (the unique constraint on `event_id` enforces consistency at the row level).
  - `Load` — `SELECT ... WHERE` on the composite primary key. `ErrNotFound` (wrapped) on miss. Identity validation + empty-Kind check at the boundary.
  - `LoadByEventID` — `SELECT ... WHERE event_id = $1`. `ErrNotFound` (wrapped) on miss.
  - `Delete` — `DELETE FROM state_records WHERE` on the composite primary key. Returns nil whether or not a row matched.
  - `Close` — `db.Close()`; subsequent calls return `state.ErrStoreClosed`.
- [ ] Every query is parameterized (`$1`, `$2`, ...) — zero string concatenation into SQL (AGENTS.md §9). Reviewers' eyes: any `+` adjacent to SQL is a bug.
- [ ] After `Close`, every method returns `state.ErrStoreClosed` (wrapped). Concurrent `Close` is safe (sentinel guarded by `atomic.Bool` set BEFORE `db.Close()`).
- [ ] `internal/state/drivers/postgres/postgres_test.go` runs `conformancetest.Run` against a Postgres connection. The test:
  - Reads `HARBOR_PG_DSN` from env; if absent, `t.Skip("HARBOR_PG_DSN not set; skipping postgres conformance — see docs/plans/phase-16-state-postgres.md")`.
  - On test start: drops + recreates a per-test schema (`harbor_test_<random_suffix>`) so concurrent test runs don't collide. Cleanup drops the schema.
  - Runs the full `conformancetest.Run` factory.
- [ ] `internal/state/drivers/postgres/migration_test.go`:
  - `TestMigrate_CleanDB_StartsClean` — fresh schema, run migrations, verify `schema_migrations` row at version 1.
  - `TestMigrate_Idempotent` — run migrations twice; second run is a no-op.
  - `TestMigrate_Concurrent_AdvisoryLockSerializes` — N goroutines call `New(cfg)` simultaneously against the SAME schema; verify all succeed and `schema_migrations` has exactly one row at version 1 (no duplicate insertions, no SQL errors). Demonstrates the advisory lock works.
  - All gated on `HARBOR_PG_DSN`.
- [ ] `internal/state/drivers/postgres/concurrent_test.go` — supplemental concurrent test: N≥64 goroutines on a single shared `*driver` against a single schema under `-race`. Asserts no errors (concurrent-write contention does NOT escape to caller-visible errors), no goroutine leak. Gated on `HARBOR_PG_DSN`.
- [ ] `cmd/harbor/main.go` adds `_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"` (additive blank import alongside the existing `inmem` import + Phase 15's `sqlite` import).
- [ ] `.github/workflows/ci.yml` adds a job (or a step in an existing job) that runs the Postgres driver tests against a `postgres:16` service container. Service container env: `POSTGRES_PASSWORD=postgres`, `POSTGRES_DB=harbor_test`. Step env: `HARBOR_PG_DSN=postgres://postgres:postgres@localhost:5432/harbor_test?sslmode=disable`. Same job runs `go test -race ./internal/state/drivers/postgres/...`.
- [ ] Coverage on `internal/state/drivers/postgres` ≥ 90% (in CI; local runs without `HARBOR_PG_DSN` will trip the skip and report lower coverage — that's by design).
- [ ] `make drift-audit` and `make preflight` green at commit time. Preflight runs without Postgres locally → tests skip cleanly → preflight stays green.
- [ ] `scripts/smoke/phase-16.sh` present and executable. Reports OK when the Postgres tests pass (or SKIP cleanly when `HARBOR_PG_DSN` unset); SKIP for the HTTP surface (Phase 60+).
- [ ] `docs/plans/README.md` Status column for Phase 16 flips from `Pending` to `Shipped` in the same PR (per AGENTS.md §4.2 #11).

## Files added or changed

- `internal/state/drivers/postgres/postgres.go` (new) — driver struct, `init()` registration, `New`, all five `state.StateStore` methods.
- `internal/state/drivers/postgres/migrations.go` (new) — embedded `migrations/*.sql` via `embed.FS`, migration runner with advisory lock.
- `internal/state/drivers/postgres/migrations/0001_init.sql` (new) — initial schema.
- `internal/state/drivers/postgres/postgres_test.go` (new) — runs `conformancetest.Run` against `HARBOR_PG_DSN`-supplied connection (gated).
- `internal/state/drivers/postgres/migration_test.go` (new) — clean-start, idempotency, concurrent-replica advisory-lock serialization.
- `internal/state/drivers/postgres/concurrent_test.go` (new) — N≥64 concurrent goroutines under `-race`.
- `cmd/harbor/main.go` (modified) — additive blank import.
- `.github/workflows/ci.yml` (modified) — Postgres service container job/step.
- `scripts/smoke/phase-16.sh` (new) — assertions described under "Smoke script additions".
- `docs/plans/phase-16-state-postgres.md` (this file).
- `docs/plans/README.md` (modified) — Phase 16 row Status flip.
- `go.mod` / `go.sum` (modified) — add `github.com/jackc/pgx/v5` if not already pinned.

No top-level directory additions.

## Public API surface

```go
package postgres

import (
    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/state"
)

// New constructs a Postgres-backed state.StateStore against cfg.DSN.
// Production callers go through state.Open; tests may call New
// directly to skip the registry.
//
// Errors:
//   - empty cfg.DSN
//   - sql.Open / migration apply failure
//   - advisory-lock acquisition failure (extremely unusual; would
//     indicate severe DB load or operator misconfiguration)
func New(cfg config.StateConfig) (state.StateStore, error)
```

The driver registers itself under `"postgres"` via `init()`; `cmd/harbor` blank-imports the package.

## Test plan

- **Unit:** DSN validation; migration runner happy-path + re-run idempotency; sentinel error wrapping.
- **Integration:** `conformancetest.Run` against a real Postgres (CI service container; locally gated on `HARBOR_PG_DSN`). Real driver, real migration, real `*sql.DB` over `pgx`. Identity propagation already covered by the suite.
- **Conformance:** `conformancetest.Run` is the load-bearing test surface — Phase 16 adds zero scenarios.
- **Concurrency / leak (D-025 concurrent-reuse contract):** the conformance suite's `Concurrent_SaveLoad_NoRace` runs N≥128 against the Postgres driver; supplemental `concurrent_test.go` runs an additional Postgres-specific N≥64 case. `TestMigrate_Concurrent_AdvisoryLockSerializes` proves multi-replica boot is safe. `GoroutineLeak_AfterClose` (in the suite) covers leak detection — `db.Close()` joins the `*sql.DB`'s connection pool.

## Smoke script additions

- `scripts/smoke/phase-16.sh` runs:
  - `go test -race -count=1 -timeout 120s ./internal/state/drivers/postgres/...` — OK on green, FAIL otherwise. Without `HARBOR_PG_DSN` set in the environment, the conformance / migration tests will `t.Skip` cleanly; the package still builds and reports a passing test exit code, so the smoke shows OK (the smoke does not differentiate "all skipped" from "all passed" — that's `go test`'s job).
  - `skip "phase 16: state/postgres has no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/state/drivers/postgres`: 90% (CI run with Postgres available). Local-without-Postgres coverage will be lower; the gate is CI.

## Dependencies

- Phase 07 (StateStore interface + InMem + conformance suite) — Phase 16 consumes `state.StateStore`, `state.StateRecord`, `state.ErrXxx`, `state.Register`, and `state/conformancetest.Run` verbatim.
- Phase 02 (config) — `config.StateConfig.DSN` is the open-time argument.
- Phase 01 (identity) — `identity.Quadruple` is the storage key (transitive through Phase 07).

## Risks / open questions

- **`pgx` driver name.** `github.com/jackc/pgx/v5/stdlib` registers under `"pgx"`. Phase 16 must use that name in `sql.Open`. Documented + verified by the migration smoke.
- **DSN format.** Standard `postgres://user:pass@host:port/db?sslmode=...`. The `pgx` stdlib adapter accepts both URL-form and key-value-form; URL-form is canonical.
- **Advisory lock key.** `pg_advisory_lock(int8)` takes an int64. We hash the literal string `"harbor-state-migrations"` (FNV-64a) and pass the result. Stable across replicas without coordination. Documented on the migration runner's godoc.
- **Connection pool defaults.** `SetMaxOpenConns(25)`, `SetMaxIdleConns(5)`, `SetConnMaxLifetime(5*time.Minute)`. Reasonable for a single-app-instance multi-tenant SDK; operators with heavier load configure via `pgx`'s connection-string params or a future Phase 16.1 RFC if pool tuning needs to live in `StateConfig`.
- **CI Postgres availability.** The `postgres:16` service container is well-maintained on GitHub Actions; per RFC §6.11 settled "CI matrix exercises against a containerized Postgres."
- **`schema_migrations` insert vs `pgcrypto`.** No `pgcrypto`/UUID dependency — `version INTEGER` is sufficient. Migration files are filesystem-numbered.
- **Race between `Close` and in-flight queries.** `db.Close()` waits for outstanding queries to finish (it cancels their connections at most once they're returned to the pool). The driver's atomic `closed` flag is set BEFORE `db.Close()` so subsequent calls fast-fail with `ErrStoreClosed`. In-flight calls already past the flag check will hit `db.Close()`'s cancellation; their error will be wrapped at the boundary as `ErrStoreClosed` (or pgx's natural cancellation error, which we map).
- **`identity.Quadruple` field naming on the SQL side.** `tenant_id`, `user_id`, `session_id`, `run_id` (snake_case) on the table; the driver translates from the Go struct's fields. Documented in `0001_init.sql`'s comments.
- **No open RFC §11 questions block this phase.** Q-1..Q-6 are unrelated.

## Glossary additions

- None. SQL-/Postgres-specific terminology (advisory lock, `BYTEA`) is technology vocabulary, not Harbor vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/state/drivers/postgres` ≥ 90% (in CI; locally lower if Postgres unavailable)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; the conformance suite's `Save_CrossTenant_Isolation` + `Save_CrossSession_Isolation` cover it; this driver inherits both verbatim.
- [ ] **Concurrent-reuse test passes** — `Concurrent_SaveLoad_NoRace` (in the conformance suite) runs against this driver with N≥128 goroutines under `-race`; the driver-local `concurrent_test.go` adds a Postgres-specific N≥64 case; `TestMigrate_Concurrent_AdvisoryLockSerializes` covers replica-boot serialization. (D-025.)
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: N/A — the apparent "JSONB payloads" departure is resolved by D-027 (the brief's older narrative is superseded; current RFC §6.11 says `Bytes []byte`).
