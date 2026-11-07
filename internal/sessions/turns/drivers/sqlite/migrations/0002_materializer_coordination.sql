-- Turn materializer coordination: the single-writer lease, the
-- persisted resume cursor, and the identity repair flag.
--
-- Forward-only migration. Editing this file after merge is forbidden.
--
--   - `turn_materializer` holds ONE row (name = 'turns'): the lease
--     holder, its generation (advanced on every change of hands; every
--     projection write checks it in its own transaction), its expiry (unix nanoseconds), and the resume cursor the
--     holder persisted so a restart pages only the events after it.
--   - `turn_rows.identity_repair` flags a row whose query or agent was
--     unavailable at append (its task record was not readable then).
--     The materializer fills the missing components once from the task
--     record and clears the flag. Existing rows are backfilled from
--     their stored DTO so rows written before this migration are
--     repaired too. The backfill matches the DTO text (the projector
--     derives an unavailable query from an empty text and an unavailable
--     agent from an empty id + name) so no row content can fail it. The partial index keeps the candidate scan
--     proportional to the flagged rows.

CREATE TABLE IF NOT EXISTS turn_materializer (
    name           TEXT    NOT NULL PRIMARY KEY,
    holder         TEXT    NOT NULL DEFAULT '',
    lease_expires  INTEGER NOT NULL DEFAULT 0,
    cursor         INTEGER NOT NULL DEFAULT 0,
    generation     INTEGER NOT NULL DEFAULT 0,
    bootstrap      TEXT    NOT NULL DEFAULT ''
);

-- A store that already holds rows is marked for the one-time bootstrap
-- (repair-flag backfill and the replay of the sessions that still hold
-- unsealed rows, rebuilding pending completions and task routes).
-- Seed the resume cursor from the projection that already exists: every
-- event at or below the highest per-session checkpoint was processed by
-- the previous materializer (it applied events in global order).
-- Starting 10,000 events earlier (the default open-turn lookback)
-- rebuilds turns still open; older open turns adopt their rows.
INSERT OR IGNORE INTO turn_materializer (name, cursor, bootstrap)
VALUES ('turns',
        MAX(COALESCE((SELECT MAX(checkpoint) FROM turn_session_state), 0) - 10000, 0),
        CASE WHEN EXISTS (SELECT 1 FROM turn_rows) THEN 'pending' ELSE '' END);

ALTER TABLE turn_rows ADD COLUMN identity_repair INTEGER NOT NULL DEFAULT 0;

-- Rows written before this migration are flagged at runtime by the
-- materializer lease holder in short batches (no table rewrite here).

CREATE INDEX IF NOT EXISTS turn_rows_identity_repair
    ON turn_rows (tenant, user, session, turn_id)
    WHERE identity_repair = 1;

-- `turn_rows.pending_complete` durably marks a row whose complete seal is
-- deferred until its answer source converges; a restart rebuilds the
-- pending-completion queue from it. The seal clears it atomically.
ALTER TABLE turn_rows ADD COLUMN pending_complete INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS turn_rows_pending_complete
    ON turn_rows (tenant, user, session, turn_id)
    WHERE pending_complete = 1;

-- `turn_routes` persists the task ancestry the materializer routes by
-- (task -> parent task, run id, root turn), so a restart that resumes
-- past a turn's spawn events still routes its child / run-scoped events
-- exactly instead of guessing. Rows are pruned when their root turn
-- seals and with the session's erasure.
CREATE TABLE IF NOT EXISTS turn_routes (
    tenant         TEXT    NOT NULL,
    user           TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    task_id        TEXT    NOT NULL,
    parent_task_id TEXT    NOT NULL DEFAULT '',
    run_id         TEXT    NOT NULL DEFAULT '',
    root_task_id   TEXT    NOT NULL DEFAULT '',
    event_seq      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, user, session, task_id)
);

CREATE INDEX IF NOT EXISTS turn_routes_by_run
    ON turn_routes (tenant, user, session, run_id, event_seq DESC)
    WHERE run_id <> '';

CREATE INDEX IF NOT EXISTS turn_routes_by_root
    ON turn_routes (tenant, user, session, root_task_id);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (2);
