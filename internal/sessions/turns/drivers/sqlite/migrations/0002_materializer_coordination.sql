-- Turn materializer coordination: the single-writer lease, the
-- persisted resume cursor, and the identity repair flag.
--
-- Forward-only migration. Editing this file after merge is forbidden.
--
--   - `turn_materializer` holds ONE row (name = 'turns'): the lease
--     holder, its expiry (unix nanoseconds), and the resume cursor the
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
    cursor         INTEGER NOT NULL DEFAULT 0
);

-- Seed the resume cursor from the projection that already exists: every
-- event at or below the highest per-session checkpoint was processed by
-- the previous materializer (it applied events in global order).
-- Starting 10,000 events earlier (the default open-turn lookback)
-- rebuilds turns still open; older open turns adopt their rows.
INSERT OR IGNORE INTO turn_materializer (name, cursor)
VALUES ('turns', MAX(COALESCE((SELECT MAX(checkpoint) FROM turn_session_state), 0) - 10000, 0));

ALTER TABLE turn_rows ADD COLUMN identity_repair INTEGER NOT NULL DEFAULT 0;

UPDATE turn_rows SET identity_repair = 1
WHERE CAST(dto AS TEXT) LIKE '%"Query":{"Text":"","At":%'
   OR CAST(dto AS TEXT) LIKE '%"Agent":{"ID":"","Name":"",%';

CREATE INDEX IF NOT EXISTS turn_rows_identity_repair
    ON turn_rows (tenant, user, session, turn_id)
    WHERE identity_repair = 1;

INSERT OR IGNORE INTO schema_migrations(version) VALUES (2);
