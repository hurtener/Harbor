-- Phase 25 — initial SQLite MemoryStore schema (RFC §6.6 + §9, brief 04).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--   - `memory_state` mirrors the abstract slot key
--     `(tenant, user, session, run, kind)`. RunID may be empty
--     (memory is session-scoped per RFC §6.6); the composite primary
--     key still serializes the slot uniquely. `kind` is always
--     `"memory.state"` at Phase 25 (the constant
--     `memory.KindMemoryState`) but is kept as a column so the table
--     can evolve to additional memory-shape slots in later phases
--     without a schema change.
--   - `bytes` is the JSON-serialised `memory.Record` envelope (BLOB).
--     The wire shape lives at `internal/memory/wire.go` so SQLite +
--     Postgres + InMem all marshal byte-stable data (cross-driver
--     `Snapshot/Restore` round-trips are part of Phase 25's
--     acceptance criteria).
--   - The strategy of the persisted record is denormalised onto
--     `strategy` so operators can grep memory state by strategy
--     without parsing the JSON. The same value lives inside `bytes`.
--   - `updated_at` defaults to CURRENT_TIMESTAMP; the driver writes
--     UTC `time.Now()` explicitly so the value is independent of the
--     SQLite engine's clock.

CREATE TABLE IF NOT EXISTS memory_state (
    tenant     TEXT      NOT NULL,
    user       TEXT      NOT NULL,
    session    TEXT      NOT NULL,
    run        TEXT      NOT NULL,
    kind       TEXT      NOT NULL,
    strategy   TEXT      NOT NULL,
    bytes      BLOB      NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant, user, session, run, kind)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
