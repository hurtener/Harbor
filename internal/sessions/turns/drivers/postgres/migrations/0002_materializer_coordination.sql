-- Turn materializer coordination: the single-writer lease, the persisted
-- resume cursor, the identity repair and pending-completion flags, and the
-- persisted task routes.
--
-- Forward-only migration. Editing this file after merge is forbidden.
--
-- This migration is METADATA-ONLY on the existing turn_rows table: it adds
-- columns with constant defaults (no table rewrite on PostgreSQL 11+) and
-- creates new tables, and it rewrites or scans no existing rows. The
-- lock_timeout bounds how long the brief ACCESS EXCLUSIVE lock of the
-- ALTER waits behind a reader; the migration runner retries a timed-out
-- attempt, so a briefly busy table never fails a boot and never queues
-- every reader behind a long wait. The row-level work happens at runtime,
-- owned by the materializer lease holder in short batches:
--   - the identity_repair backfill of rows written before this release,
--   - the partial indexes over the two flags (CREATE INDEX CONCURRENTLY),
--   - the one-time reconstruction of pending completions and task routes
--     for the sessions that still hold unsealed rows (bootstrap below).
--
--   - `turn_materializer` holds ONE row (name = 'turns'): the lease holder,
--     its generation (advanced on every change of hands; every projection
--     write checks it in its own transaction), its expiry (evaluated
--     against the DATABASE clock so instance clock skew can never split the
--     lease), the resume cursor, and the bootstrap progress. A lease row
--     (not a session advisory lock) works through transaction poolers.
--   - `turn_rows.identity_repair` flags a row whose query or agent was
--     unavailable at append; `turn_rows.pending_complete` marks a row whose
--     complete seal is deferred until its answer converges (cleared by the
--     seal).
--   - `turn_routes` persists task ancestry (task -> parent, run, root) so a
--     restart that resumes past a turn's spawn still routes exactly.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS turn_materializer (
    name          TEXT        NOT NULL PRIMARY KEY,
    holder        TEXT        NOT NULL DEFAULT '',
    lease_expires TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    cursor        BIGINT      NOT NULL DEFAULT 0,
    generation    BIGINT      NOT NULL DEFAULT 0,
    bootstrap     TEXT        NOT NULL DEFAULT ''
);

-- Seed the resume cursor from the projection that already exists: the
-- previous (uncoordinated) materializer applied events in global order, so
-- every event at or below the highest per-session checkpoint was processed.
-- Starting 10,000 events earlier (the default open-turn lookback) rebuilds
-- recently open turns by replay. A store that already holds rows is marked
-- for the one-time bootstrap, which replays only the sessions that still
-- hold unsealed rows (their own history, never the global log) to rebuild
-- pending completions and task routes, and backfills the repair flag.
INSERT INTO turn_materializer (name, cursor, bootstrap)
SELECT 'turns',
       GREATEST(COALESCE((SELECT MAX(checkpoint) FROM turn_sessions), 0) - 10000, 0),
       CASE WHEN EXISTS (SELECT 1 FROM turn_rows) THEN 'pending' ELSE '' END
ON CONFLICT (name) DO NOTHING;

ALTER TABLE turn_rows
    ADD COLUMN IF NOT EXISTS identity_repair  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS pending_complete BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS turn_routes (
    tenant_id      TEXT   NOT NULL,
    user_id        TEXT   NOT NULL,
    session_id     TEXT   NOT NULL,
    task_id        TEXT   COLLATE "C" NOT NULL,
    parent_task_id TEXT   COLLATE "C" NOT NULL DEFAULT '',
    run_id         TEXT   COLLATE "C" NOT NULL DEFAULT '',
    root_task_id   TEXT   COLLATE "C" NOT NULL DEFAULT '',
    event_seq      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, user_id, session_id, task_id)
);

-- turn_routes is new and empty: its indexes build instantly.
CREATE INDEX IF NOT EXISTS turn_routes_by_run
    ON turn_routes (tenant_id, user_id, session_id, run_id, event_seq DESC)
    WHERE run_id <> '';

CREATE INDEX IF NOT EXISTS turn_routes_by_root
    ON turn_routes (tenant_id, user_id, session_id, root_task_id);

INSERT INTO schema_migrations (version)
VALUES (2)
ON CONFLICT DO NOTHING;
