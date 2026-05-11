-- Phase 25 — initial Postgres MemoryStore schema (RFC §6.6 + §9, brief 04).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`, and
-- the runner itself is serialised across replicas via
-- `pg_advisory_lock` (see migrations.go) so no two booting replicas
-- race on `CREATE TABLE` / `INSERT INTO schema_migrations`.
--
-- Schema notes (mirrors `internal/memory/drivers/sqlite/migrations/0001_init.sql`):
--   - `memory_state` keyed on `(tenant_id, user_id, session_id, run_id, kind)`.
--     `run_id` may be empty (memory is session-scoped per RFC §6.6);
--     the column is NOT NULL but accepts the empty string.
--   - `bytes` is BYTEA — the JSON-serialised `memory.Record`
--     envelope. The wire shape lives at `internal/memory/wire.go` so
--     SQLite + Postgres + InMem all marshal byte-stable data.
--   - `strategy` is denormalised onto its own column so operators can
--     grep memory state by strategy without parsing the JSON. The
--     same value lives inside `bytes`.
--   - `updated_at` is TIMESTAMPTZ; the driver writes UTC `time.Now()`
--     explicitly.

CREATE TABLE IF NOT EXISTS memory_state (
    tenant_id  TEXT        NOT NULL,
    user_id    TEXT        NOT NULL,
    session_id TEXT        NOT NULL,
    run_id     TEXT        NOT NULL,
    kind       TEXT        NOT NULL,
    strategy   TEXT        NOT NULL,
    bytes      BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_id, run_id, kind)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version)
VALUES (1)
ON CONFLICT DO NOTHING;
