# Phase 15 — SQLite StateStore driver

## Summary

Land `internal/state/drivers/sqlite/`: the second concrete `state.StateStore` driver in Harbor's persistence triad. Built on `modernc.org/sqlite` (CGo-free, AGENTS.md §5 + RFC §10), WAL journal mode, forward-only migrations under `internal/state/drivers/sqlite/migrations/`. The driver passes `internal/state/conformancetest.Run` verbatim — that suite IS the gate; this phase ships zero new conformance scenarios. Makes durable single-binary deployments viable for every consumer (sessions, tasks, governance, planner, memory) that already saves through Phase 07's interface.

## RFC anchor

- RFC §6.11
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (three drivers from t=0).** "Designing the interface against three backends from t=0 forces clean abstractions." Phase 15 is the second leg of the triad. The conformance suite landed in Phase 07; SQLite consuming it verbatim — without driver-specific scenario forks — is the proof that the interface holds.
- **brief 05 §4 (idempotency on `EventID`, audit redaction upstream).** `Save` keys idempotency on the caller-supplied ULID; same-ID + byte-equal payload is a no-op, divergent payload returns `ErrIdempotencyConflict`. Bytes are stored opaquely (`BLOB`); the SQLite driver does NOT re-redact, parse, or interpret them.
- **brief 05 §4 (forward-only migrations, per-driver).** "Forward-only, numbered monotonically (`0001_init.sql`, `0002_*.sql`, ...) per driver. Each migration ends with a `schema_migrations` insert. SQLite uses WAL." Phase 15 ships exactly one migration (`0001_init.sql`) creating the records table + EventID secondary index + `schema_migrations` bookkeeping. Editing a merged migration is forbidden (AGENTS.md §13); future schema changes land as `0002_*.sql`.
- **brief 05 §5 ("No production StateStore backend ships").** The predecessor's biggest gap. Phase 15 closes it for single-binary deployments; Phase 16 closes it for multi-node.

## Findings I'm departing from (if any)

- None.

## Goals

- Ship a `state.StateStore` driver named `"sqlite"` registered via `init()`, persisted to a SQLite database at the DSN supplied by `config.StateConfig.DSN`.
- Use `modernc.org/sqlite` (CGo-free per AGENTS.md §5 + D-013 + RFC §10). The build remains `CGO_ENABLED=0`.
- WAL journal mode pinned at open time (AGENTS.md §9 + RFC §6.11). Busy timeout configured so concurrent writers see `SQLITE_BUSY` upgraded to short retries rather than spurious failures.
- Forward-only migrations under `internal/state/drivers/sqlite/migrations/` driven by an embedded migration runner (no external migrate tool — V1 keeps the toolchain footprint minimal).
- Pass `internal/state/conformancetest.Run` end-to-end with zero scenario forks. The suite's `Concurrent_SaveLoad_NoRace` (D-025) IS the concurrent-reuse gate.
- Provide a driver-local migration test that exercises clean DB → migration → round-trip end-to-end against a `t.TempDir()` SQLite file.
- Boot-log integration: when `cmd/harbor` blank-imports the driver, `state.RegisteredDrivers()` should list `inmem` AND `sqlite`. Phase 15 adds the blank import.

## Non-goals

- No Postgres driver (Phase 16, parallel).
- No ArtifactStore work (Phase 17, parallel).
- No per-Kind table sharding, no JSON-extracted indices on `Bytes`, no full-text search. The interface is `(Identity, Kind) → Bytes`; the schema mirrors that minimally.
- No new `state.StateStore` methods. The conformance suite is exhaustive; introducing a method here would have to land via a Phase 07 RFC PR.
- No connection-pool tuning. Default `sql.DB` settings are sufficient at V1 (single-binary, low concurrency); operators can tune via DSN params if needed.
- No automatic backup / VACUUM / WAL checkpointing on close. SQLite's defaults are correct; periodic maintenance is operator's domain.
- No driver-specific dialect quirks leaking into the conformance suite. If SQLite cannot satisfy a scenario the suite already pins, that's a contract bug — escalate, do not weaken the suite.

## Acceptance criteria

- [ ] `internal/state/drivers/sqlite/sqlite.go` defines a `driver` struct implementing `state.StateStore`. Compile-time assertion: `var _ state.StateStore = (*driver)(nil)`.
- [ ] `init()` calls `state.Register("sqlite", New)` exactly once. Re-registration panics per the registry contract (Phase 07 `registry.go`).
- [ ] `New(cfg config.StateConfig) (state.StateStore, error)` opens a `*sql.DB` against `cfg.DSN` (mapped to `modernc.org/sqlite`'s driver name), runs migrations idempotently to the latest version, sets `PRAGMA journal_mode=WAL`, sets `PRAGMA busy_timeout=5000` (5 s).
- [ ] DSN handling: empty DSN returns a clear error (`fmt.Errorf("state/sqlite: empty DSN; expected file path or sqlite://...")`). `:memory:` is supported as a degenerate dev case (and useful for tests). File paths are passed verbatim to the underlying driver.
- [ ] `internal/state/drivers/sqlite/migrations/0001_init.sql` creates:
  - `state_records` — primary table, columns `(tenant TEXT, user TEXT, session TEXT, run TEXT, kind TEXT, event_id TEXT, version INTEGER, bytes BLOB, updated_at TIMESTAMP)`. Composite primary key `(tenant, user, session, run, kind)`. Index on `event_id` (unique).
  - `schema_migrations` — `(version INTEGER PRIMARY KEY, applied_at TIMESTAMP)` with the `INSERT OR IGNORE INTO schema_migrations(version) VALUES (1)` trailing statement (RFC §9).
- [ ] Migration runner is internal to the package: reads `migrations/*.sql` via `embed.FS`, sorts by filename, applies in-order in a transaction per file, gates each file on its filename version not yet present in `schema_migrations`. Idempotent on re-run.
- [ ] All five `state.StateStore` methods implemented:
  - `Save` — single transaction; uses `INSERT INTO ... ON CONFLICT(tenant,user,session,run,kind) DO UPDATE` (SQLite UPSERT). Idempotency check (same EventID + identical Bytes / Version → no-op; same EventID + divergent → `ErrIdempotencyConflict`) is performed BEFORE the upsert by reading the previous row at the slot OR the row resolved by EventID. Eviction of the previous EventID happens within the same transaction when the slot's EventID changes.
  - `Load` — `SELECT ... WHERE` on the composite primary key. `ErrNotFound` (wrapped, with key fragments in the message) on miss. Identity validation (`state.ValidateIdentity`) + empty-Kind check at the boundary, before the SQL.
  - `LoadByEventID` — `SELECT ... WHERE event_id = ?`. `ErrNotFound` (wrapped) on miss.
  - `Delete` — `DELETE FROM state_records WHERE` on the composite primary key. Returns nil whether or not a row matched (idempotent — matches the InMem driver). Deletion of the secondary EventID index entry is automatic (it's a column, not a separate index — the row deletion removes both).
  - `Close` — `db.Close()`; subsequent calls return `state.ErrStoreClosed`.
- [ ] After `Close`, every method returns `state.ErrStoreClosed` (wrapped). Concurrent `Close` is safe (sentinel guarded by `atomic.Bool` set BEFORE `db.Close()`).
- [ ] `internal/state/drivers/sqlite/sqlite_test.go` runs `conformancetest.Run` against a fresh `t.TempDir()` SQLite file, with cleanup that closes + removes the file. Same test runs against `:memory:` to exercise the degenerate case.
- [ ] `internal/state/drivers/sqlite/migration_test.go`:
  - `TestMigrate_CleanDB_StartsClean` — fresh tempdir DB, run migrations, verify `schema_migrations` row at version 1.
  - `TestMigrate_Idempotent` — run migrations twice; second run is a no-op (no error, no duplicate rows).
  - `TestMigrate_Roundtrip_AcrossMigration` — Save records → close → reopen (re-runs migration; idempotent) → Load round-trips byte-equal.
- [ ] `internal/state/drivers/sqlite/concurrent_test.go` — supplemental concurrent test mirroring `Concurrent_SaveLoad_NoRace` but exercising the SQLite-specific behavior: `SQLITE_BUSY` retries do NOT escape as caller-visible errors (the busy_timeout PRAGMA handles them transparently). N≥64 goroutines on a single shared `*driver` against a single tempdir file under `-race`. Asserts no errors, no goroutine leak.
- [ ] `cmd/harbor/main.go` adds `_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"` (additive blank import alongside the existing `inmem` import).
- [ ] Coverage on `internal/state/drivers/sqlite` ≥ 90%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-15.sh` present and executable. Reports OK for the conformance + migration test pair under preflight; SKIP for the HTTP surface (Phase 60+).
- [ ] `docs/plans/README.md` Status column for Phase 15 flips from `Pending` to `Shipped` in the same PR (per AGENTS.md §4.2 #11).

## Files added or changed

- `internal/state/drivers/sqlite/sqlite.go` (new) — driver struct, `init()` registration, `New`, all five `state.StateStore` methods.
- `internal/state/drivers/sqlite/migrations.go` (new) — embedded `migrations/*.sql` via `embed.FS`, migration runner.
- `internal/state/drivers/sqlite/migrations/0001_init.sql` (new) — initial schema.
- `internal/state/drivers/sqlite/sqlite_test.go` (new) — runs `conformancetest.Run` against tempdir file + `:memory:`.
- `internal/state/drivers/sqlite/migration_test.go` (new) — clean-start, idempotency, round-trip-across-migration.
- `internal/state/drivers/sqlite/concurrent_test.go` (new) — N≥64 concurrent goroutines under `-race`.
- `cmd/harbor/main.go` (modified) — additive blank import.
- `scripts/smoke/phase-15.sh` (new) — assertions described under "Smoke script additions".
- `docs/plans/phase-15-state-sqlite.md` (this file).
- `docs/plans/README.md` (modified) — Phase 15 row Status flip.
- `go.mod` / `go.sum` (modified) — add `modernc.org/sqlite` if not already pinned.

No top-level directory additions; `internal/state/drivers/` already exists per AGENTS.md §3.

## Public API surface

```go
package sqlite

import (
    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/state"
)

// New constructs a SQLite-backed state.StateStore against cfg.DSN.
// Production callers go through state.Open; tests may call New
// directly to skip the registry.
//
// Errors:
//   - empty cfg.DSN
//   - sql.Open / migration apply failure
//   - PRAGMA journal_mode=WAL fail (extremely unusual)
func New(cfg config.StateConfig) (state.StateStore, error)
```

The driver registers itself under `"sqlite"` via `init()`; `cmd/harbor` blank-imports the package. Beyond `New`, the driver exposes nothing — every consumer talks to it through `state.StateStore`.

## Test plan

- **Unit:** DSN validation (empty / malformed); migration runner happy-path + re-run idempotency; sentinel error wrapping (`Save` after `Close` → `ErrStoreClosed`; `Load` miss → `ErrNotFound`; identity-empty rejection).
- **Integration:** `conformancetest.Run` against tempdir file + `:memory:`. Real driver, real migration, real `*sql.DB`. Identity propagation already covered by the suite's cross-tenant / cross-session subtests.
- **Conformance:** `conformancetest.Run` is the load-bearing test surface — Phase 15 adds zero scenarios. If a scenario fails, the contract is wrong, not the driver.
- **Concurrency / leak (D-025 concurrent-reuse contract):** the conformance suite's `Concurrent_SaveLoad_NoRace` runs N≥128 against the SQLite driver; supplemental `concurrent_test.go` runs an additional, SQLite-specific N≥64 case proving `SQLITE_BUSY` retries are absorbed by `busy_timeout` and don't escape. `GoroutineLeak_AfterClose` (in the suite) covers leak detection — the driver has no goroutines of its own; `db.Close()` joins the `*sql.DB`'s connection pool.

## Smoke script additions

- `scripts/smoke/phase-15.sh` runs:
  - `go test -race -count=1 -timeout 90s ./internal/state/drivers/sqlite/...` → OK on green, FAIL otherwise.
  - `skip "phase 15: state/sqlite has no HTTP/Protocol surface yet (lands in Phase 60+)"`.

The smoke is package-test driven (no protocol surface yet, same shape as phase-08/09/etc.). Pattern matches phase-13/phase-14's two-OK + one-SKIP layout where there are unit tests but no HTTP surface — except Phase 15 has no integration test under `test/integration/` yet (the seam is closed at Phase 17 + a wave-5 wave-end test, parallel to wave2/wave3/wave4).

## Coverage target

- `internal/state/drivers/sqlite`: 90%. The conformance suite drives the public API to 100%; the small remainder is migration-error paths only reachable on disk failure (acceptable to exclude).

## Dependencies

- Phase 07 (StateStore interface + InMem + conformance suite) — Phase 15 consumes `state.StateStore`, `state.StateRecord`, `state.ErrXxx`, `state.Register`, and `state/conformancetest.Run` verbatim.
- Phase 02 (config) — `config.StateConfig.DSN` is the open-time argument.
- Phase 01 (identity) — `identity.Quadruple` is the storage key (transitive through Phase 07).

## Risks / open questions

- **`modernc.org/sqlite` driver name.** The package's registered driver name is `"sqlite"` (per the modernc upstream). Phase 15 must use that exact name in `sql.Open`. Document on godoc + tested by the migration smoke (`sql.Open("sqlite", dsn)` succeeds).
- **DSN format.** Bare file paths work. The modernc driver also accepts URI-form (`file:foo.db?_journal=WAL`) — Phase 15 sets WAL via PRAGMA after open, so URI-form is not needed and not blessed in V1. Documented.
- **`busy_timeout` of 5 s.** Reasonable default for a single-binary deployment with light concurrency. Operators with heavier write contention will tune; the value is a constant in `sqlite.go` for V1, not a config knob (V1 simplicity, RFC §10 stack-decisions principle).
- **No connection-pool tuning.** `sql.DB` defaults (unlimited connections, idle pool) are fine for SQLite at V1. Adding `db.SetMaxOpenConns(1)` to serialize writers is tempting but the conformance suite's concurrent test passes without it (busy_timeout absorbs contention); leaving the pool defaulted is the simpler call.
- **Embedded migrations file count.** One file (`0001_init.sql`). Future schema changes land as new files; the migration runner's invariant (forward-only, `INSERT OR IGNORE` trailer) is tested.
- **No open RFC §11 questions block this phase.** Q-1..Q-6 are unrelated; the StateStore generic surface is settled (D-027); the SQLite + Postgres co-shipping decision is settled (RFC §6.11 "Build-tag strategy — Settled").
- **Linter / lint config.** `internal/state/drivers/sqlite/migrations/0001_init.sql` is SQL, not Go — golangci-lint ignores it. The embed declaration in `migrations.go` should not trigger any lint (`embed.FS` is stdlib).

## Glossary additions

- None. `StateStore`, `StateRecord`, `EventID` already in the glossary from Phase 07. SQLite-specific vocabulary (WAL, busy_timeout) is technology terminology, not Harbor vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/state/drivers/sqlite` ≥ 90%
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; the conformance suite's `Save_CrossTenant_Isolation` + `Save_CrossSession_Isolation` cover it; this driver inherits both verbatim.
- [ ] **Concurrent-reuse test passes** — `Concurrent_SaveLoad_NoRace` (in the conformance suite) runs against this driver with N≥128 goroutines under `-race`; the driver-local `concurrent_test.go` adds a SQLite-specific N≥64 case that proves `SQLITE_BUSY` retries are absorbed without leaking caller errors. (D-025.)
- [ ] If new vocabulary: glossary updated — N/A (no new Harbor vocabulary; SQLite terminology is technology-specific).
- [ ] If a brief finding was departed from: N/A — none.
