# Phase 18 — ArtifactStore SQLite-blob + Postgres-blob drivers

## Summary

Land `internal/artifacts/drivers/sqlite/` and `internal/artifacts/drivers/postgres/`: the two durable artifact drivers that close out the V1 persistence triad for the ArtifactStore subsystem. Both inherit `internal/artifacts/conformancetest.Run` verbatim — the suite IS the gate; this phase ships zero new conformance scenarios. Persistent artifact lifetimes survive process restart; reuses the SQLite + Postgres infrastructure already shipped by Phases 15 + 16 (driver shape, migration runner, advisory-lock pattern, CI gates).

## RFC anchor

- RFC §6.10
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (mandatory artifacts policy + persistence triad).** "Artifacts-2: SQLite-blob and Postgres-blob drivers, plus S3-style driver. Persistent artifact lifetimes that survive restart; matches the StateStore driver triad." Phase 18 ships the SQLite + Postgres halves; Phase 19 ships the S3 third. The InMem + FS drivers from Phase 17 remain the floor + single-binary-no-DB option.
- **brief 05 §4 (forward-only, per-driver migrations + advisory-lock concurrency).** Each driver ships exactly one migration (`0001_init.sql`) creating its `artifacts_blobs` table + `schema_migrations` bookkeeping. SQLite uses the same `modernc.org/sqlite` + WAL + busy_timeout pattern as Phase 15. Postgres uses the same `pgx/v5/stdlib` + `pg_advisory_lock`-serialised migration runner as Phase 16.
- **brief 05 §4 (artifact dedup, content addressing).** "IDs are `{namespace}_{sha256[:12]}`. Re-uploading identical bytes returns the existing ref." Both drivers store the canonical ref + bytes and reuse the conformance suite's dedup tests.
- **brief 05 §6 (artifact cleanup, scope-mismatch rejection, cross-tenant isolation).** All three covered by the conformance suite already; both drivers inherit the gate.

## Findings I'm departing from (if any)

- None.

## Goals

- Two new drivers under `internal/artifacts/drivers/` registered as `"sqlite"` and `"postgres"` via `init()`. Both implement the full `artifacts.ArtifactStore` interface.
- Each driver passes `internal/artifacts/conformancetest.Run` end-to-end with zero scenario forks.
- SQLite driver: `modernc.org/sqlite`, WAL, busy_timeout, `db.SetMaxOpenConns(1)` — same configuration matrix Phase 15 settled (deviation reason: `BEGIN IMMEDIATE` doesn't honor `busy_timeout` at the `database/sql` pool layer under N≥128 contention).
- Postgres driver: `pgx/v5/stdlib`, advisory-lock-serialised migration runner keyed on FNV-64a hash of `"harbor-artifacts-migrations"` (distinct from Phase 16's StateStore lock so the two subsystems don't serialise against each other).
- DSN handling: empty DSN returns clear error; bare file paths supported by SQLite; standard `postgres://` URL by Postgres. Tests gate on `HARBOR_PG_DSN` for Postgres conformance (skip-clean otherwise; CI matrix runs against the `postgres:16` service container Phase 16 already wired).
- Boot-log integration: `cmd/harbor/main.go` blank-imports both drivers; `artifacts.RegisteredDrivers()` returns `[fs, inmem, postgres, sqlite]` (sorted) once they self-register.
- Coverage: each driver ≥85% (matches Phase 17's FS driver coverage target; the conformance suite drives the public API to ~100% anyway).

## Non-goals

- No S3-style driver (Phase 19, parallel within Wave 6).
- No new `ArtifactStore` methods. Conformance suite is exhaustive.
- No driver-specific Search / FullText query API. The interface is `(scope, id) → bytes`; richer queries land with their first consumer (likely Phase 39's virtual-directory subsystem if it surfaces).
- No streaming `PutBytesStream` / `GetStream`. The InMem + FS drivers already accept byte slices; switching to streaming for SQLite/PG-blob is a perf optimisation that lands when LLM-side streaming consumers (Phase 12 → Phase 32) need it.
- No automatic blob deduplication ACROSS scopes. Same bytes under different scopes intentionally produce two rows (the conformance suite's `Put_DistinguishesByScope` test pins this; doing global dedup would weaken cross-tenant isolation).
- No connection-pool tuning beyond what each backend's Phase 15/16 driver settled.
- No backup / VACUUM / PG `CLUSTER` automation. Operator concern.

## Acceptance criteria

- [ ] `internal/artifacts/drivers/sqlite/sqlite.go` defines a `driver` struct implementing `artifacts.ArtifactStore`. Compile-time assertion: `var _ artifacts.ArtifactStore = (*driver)(nil)`. `init()` calls `artifacts.Register("sqlite", New)` exactly once.
- [ ] `internal/artifacts/drivers/postgres/postgres.go` mirrors the SQLite driver shape on `pgx/v5/stdlib`. `init()` calls `artifacts.Register("postgres", New)`.
- [ ] Each driver's `New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error)`:
  - SQLite: opens against `cfg.DSN` (mapped to the modernc driver name `"sqlite"`); empty DSN returns a clear error; runs migrations idempotently; sets `PRAGMA journal_mode=WAL` + `PRAGMA busy_timeout=5000`.
  - Postgres: opens against `cfg.DSN`; sets `MaxOpenConns(25) / MaxIdleConns(5) / ConnMaxLifetime(5*time.Minute)`; runs migrations under `pg_advisory_lock`.
- [ ] **Config additions** (`internal/config/config.go`): `ArtifactsConfig` gains a new optional `DSN string` field (used when `Driver` ∈ {`sqlite`, `postgres`}). The existing `FSRoot` field stays (used when `Driver = "fs"`). Validator rejects empty `DSN` when driver is `sqlite` / `postgres`. Defaults unchanged.
- [ ] `internal/artifacts/drivers/sqlite/migrations/0001_init.sql` creates:
  - `artifacts_blobs` — primary table, columns `(tenant TEXT NOT NULL, user TEXT NOT NULL, session TEXT NOT NULL, task TEXT NOT NULL, namespace TEXT NOT NULL, id TEXT NOT NULL, mime_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, filename TEXT NOT NULL, sha256 TEXT NOT NULL, source_json BLOB NOT NULL, bytes BLOB NOT NULL)`. Composite primary key `(tenant, user, session, task, namespace, id)`.
  - `schema_migrations` — `(version INTEGER PRIMARY KEY, applied_at TIMESTAMP)` with `INSERT OR IGNORE INTO schema_migrations(version) VALUES (1)`.
- [ ] `internal/artifacts/drivers/postgres/migrations/0001_init.sql` creates the equivalent shape with `BYTEA` for `bytes`/`source_json`, `TIMESTAMPTZ` for `applied_at`, and the trailing `INSERT INTO schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING`.
- [ ] Both drivers parameterise every query (no `+`-into-SQL anywhere — AGENTS.md §9). `Source map[string]any` is JSON-encoded into `source_json` on Put and decoded on Get/GetRef.
- [ ] All eight `ArtifactStore` methods implemented per the conformance contract:
  - `PutBytes` / `PutText` — single transaction; `INSERT ... ON CONFLICT(tenant,user,session,task,namespace,id) DO NOTHING` (re-Put of identical content-addressed ID under the same scope is a no-op; the conformance suite's `Put_DedupOnIdenticalBytes` tests this).
  - `Get` / `GetRef` — `SELECT ... WHERE` on the composite key. Found-false returns `(nil, false, nil)` — NOT an error (matches the InMem/FS drivers).
  - `Exists` — `SELECT 1 ... LIMIT 1` form.
  - `Delete` — `DELETE FROM artifacts_blobs WHERE` on the composite key. Returns `(true, nil)` if a row matched, `(false, nil)` otherwise. Idempotent.
  - `List` — `SELECT ... WHERE` with empty fields treated as wildcards (the conformance suite's `List_NilFieldsAreWildcards` pins this).
  - `Close` — `db.Close()`; subsequent calls return `artifacts.ErrStoreClosed`.
- [ ] After `Close`, every method returns `artifacts.ErrStoreClosed` (wrapped). Sentinel guarded by `atomic.Bool` set BEFORE `db.Close()`.
- [ ] `internal/artifacts/drivers/sqlite/sqlite_test.go` runs `conformancetest.Run` against `t.TempDir()` SQLite file + against `:memory:` (degenerate dev case).
- [ ] `internal/artifacts/drivers/sqlite/migration_test.go` covers clean-start, idempotent re-run, round-trip-across-migration.
- [ ] `internal/artifacts/drivers/sqlite/concurrent_test.go` — supplemental N≥64 stress proving SQLite-specific behavior holds: `SQLITE_BUSY` does not escape as caller-visible errors; same scope+namespace contention dedups correctly under `Put`.
- [ ] `internal/artifacts/drivers/postgres/postgres_test.go` runs `conformancetest.Run` against a per-test schema (`harbor_artifacts_test_<random_hex>`), gated on `HARBOR_PG_DSN`. Cleanup drops the schema.
- [ ] `internal/artifacts/drivers/postgres/migration_test.go` — clean-start, idempotent, advisory-lock concurrent boot serialisation.
- [ ] `internal/artifacts/drivers/postgres/concurrent_test.go` — supplemental N≥64 Postgres-specific stress; gated on `HARBOR_PG_DSN`.
- [ ] `cmd/harbor/main.go` adds blank imports for both new drivers (additive; alphabetic ordering).
- [ ] `.github/workflows/ci.yml` — extend the existing `state-postgres` job (or add a sibling `artifacts-postgres` job) to run the artifact-postgres tests against the same `postgres:16` service container. Decision: add a SECOND step inside the existing `state-postgres` job for symmetry with how Phase 16 wired it (the container is already up; running both packages keeps CI overhead minimal).
- [ ] Coverage: `internal/artifacts/drivers/sqlite` ≥ 85%; `internal/artifacts/drivers/postgres` ≥ 85% (CI run with Postgres available; locally lower without DSN — by design, parallels Phase 16).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-18.sh` present and executable. Reports OK once both drivers' tests pass.
- [ ] `docs/plans/README.md` Phase 18 row Status flips from `Pending` to `Shipped`.

## Files added or changed

- `internal/artifacts/drivers/sqlite/sqlite.go` (new)
- `internal/artifacts/drivers/sqlite/migrations.go` (new) — embedded migrations runner
- `internal/artifacts/drivers/sqlite/migrations/0001_init.sql` (new)
- `internal/artifacts/drivers/sqlite/sqlite_test.go` (new) — conformance suite
- `internal/artifacts/drivers/sqlite/migration_test.go` (new)
- `internal/artifacts/drivers/sqlite/concurrent_test.go` (new)
- `internal/artifacts/drivers/postgres/postgres.go` (new)
- `internal/artifacts/drivers/postgres/migrations.go` (new)
- `internal/artifacts/drivers/postgres/migrations/0001_init.sql` (new)
- `internal/artifacts/drivers/postgres/postgres_test.go` (new) — conformance suite (gated)
- `internal/artifacts/drivers/postgres/migration_test.go` (new)
- `internal/artifacts/drivers/postgres/concurrent_test.go` (new)
- `internal/config/config.go` (modified) — `ArtifactsConfig.DSN` added
- `internal/config/loader.go` / `validate.go` (modified) — defaults + DSN validation when driver is sqlite/postgres
- `cmd/harbor/main.go` (modified) — additive blank imports
- `.github/workflows/ci.yml` (modified) — `artifacts-postgres` test step
- `scripts/smoke/phase-18.sh` (new)
- `docs/plans/phase-18-artifacts-blob.md` (this file)
- `docs/plans/README.md` (modified) — Status row flip
- `examples/harbor.yaml` (modified) — document the new `artifacts.dsn` field
- `go.mod` / `go.sum` (modified if needed) — both deps already pinned by Phases 15 + 16, so no new direct requires expected

No top-level directory additions.

## Public API surface

```go
package sqlite // internal/artifacts/drivers/sqlite

import (
    "github.com/hurtener/Harbor/internal/artifacts"
    "github.com/hurtener/Harbor/internal/config"
)

func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error)
```

```go
package postgres // internal/artifacts/drivers/postgres

import (
    "github.com/hurtener/Harbor/internal/artifacts"
    "github.com/hurtener/Harbor/internal/config"
)

func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error)
```

Both drivers register themselves under their canonical names via `init()`; consumers depend ONLY on `artifacts.ArtifactStore`.

## Test plan

- **Unit:** DSN validation; migration runner happy-path + re-run idempotency; sentinel error wrapping (`Put` after `Close` → `ErrStoreClosed`; identity-empty rejection wraps `ErrIdentityRequired`).
- **Integration:** `conformancetest.Run` against each driver. Real driver, real migration, real `*sql.DB`. Identity propagation already covered by the suite's cross-tenant / cross-session subtests.
- **Conformance:** Phase 18 adds zero scenarios. The suite is the gate.
- **Concurrency / leak (D-025):** the conformance suite's `Concurrent_PutGet_NoRace` runs against each driver with N≥128 goroutines under `-race`; supplemental driver-local `concurrent_test.go` adds an N≥64 case proving backend-specific contention is absorbed.

## Smoke script additions

- `scripts/smoke/phase-18.sh` runs:
  - `go test -race -count=1 -timeout 120s ./internal/artifacts/drivers/sqlite/... ./internal/artifacts/drivers/postgres/...` — OK on green; FAIL otherwise. Without `HARBOR_PG_DSN`, Postgres tests skip cleanly; SQLite tests still run; smoke shows OK.
  - `skip "phase 18: artifact-blob has no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/artifacts/drivers/sqlite`: 85%.
- `internal/artifacts/drivers/postgres`: 85% (in CI with Postgres available).

## Dependencies

- Phase 17 (ArtifactStore interface + InMem + FS) — Phase 18 inherits the interface, conformance suite, registry pattern.
- Phase 15 (SQLite StateStore) — Phase 18's SQLite driver reuses the modernc driver, WAL pattern, busy_timeout, and `MaxOpenConns(1)` shape settled there.
- Phase 16 (Postgres StateStore) — Phase 18's Postgres driver reuses the pgx pattern, advisory-lock migration runner, CI service container.
- Phase 02 (config) — `config.ArtifactsConfig.DSN` is the open-time argument.

## Risks / open questions

- **`ON CONFLICT DO NOTHING` semantics for content-addressed IDs.** Putting an artifact with the same content-addressed ID under the same scope+namespace must be a no-op (return the existing ref). With `DO NOTHING`, the driver's `Put` flow is: (1) attempt insert; (2) on conflict, `SELECT` the existing row and return its ref. Tested by the conformance suite's `Put_DedupOnIdenticalBytes`.
- **`source_json` JSON encoding in SQLite.** SQLite's BLOB column accepts arbitrary bytes; the driver `json.Marshal`s `Source map[string]any` and stores. Postgres's `BYTEA` works identically. Non-encodable values fail at marshal time per Phase 17's documented behavior.
- **Single advisory-lock per subsystem.** Phase 18's Postgres lock key is `FNV-64a("harbor-artifacts-migrations")` — distinct from Phase 16's `FNV-64a("harbor-state-migrations")`. Documented in `migrations.go`.
- **CI cost.** Adding artifact-postgres tests to the same `state-postgres` job adds ~10-20s. Acceptable; alternative (separate job) doubles container spin-up cost.
- **No open RFC §11 questions block this phase.**

## Glossary additions

- None. `ArtifactStore`, `ArtifactRef`, `HeavyOutputThreshold`, `ScopedArtifacts` already in glossary from Phase 17. SQL/PG terminology is technology-specific.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/artifacts/drivers/sqlite` ≥ 85%; `drivers/postgres` ≥ 85% (CI)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; the conformance suite's `Get_CrossTenant_Isolation` + `Delete_CrossTenant_Isolation` cover it; both drivers inherit verbatim.
- [ ] **Concurrent-reuse test passes** — `Concurrent_PutGet_NoRace` (in the conformance suite) runs against both drivers with N≥128 goroutines under `-race`; supplemental driver-local `concurrent_test.go` adds backend-specific N≥64 cases (D-025).
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: N/A.
