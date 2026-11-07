-- Turn materializer coordination: the single-writer lease, the
-- persisted resume cursor, and the identity repair flag.
--
-- Forward-only migration. Editing this file after merge is forbidden.
--
--   - `turn_materializer` holds ONE row (name = 'turns'): the lease
--     holder, its expiry (evaluated against the DATABASE clock so
--     instance clock skew can never split the lease), and the resume
--     cursor the holder persisted so a restart pages only the events
--     after it. A lease row (not a session advisory lock) works
--     through transaction-pooling proxies.
--   - `turn_rows.identity_repair` flags a row whose query or agent was
--     unavailable at append (its task record was not readable then).
--     The materializer fills the missing components once from the task
--     record and clears the flag. Existing rows are backfilled from
--     their stored envelope so rows written before this migration are
--     repaired too. The backfill matches the envelope TEXT instead of
--     parsing it: a stored query may carry escapes (\u0000) that the
--     Postgres JSON types refuse, and a migration must never fail on a
--     row's content. The projector derives an unavailable query from an
--     empty text and an unavailable agent from an empty id + name, and
--     a value inside a JSON string cannot match the unescaped key
--     pattern. The partial index keeps the candidate scan
--     proportional to the flagged rows.

CREATE TABLE IF NOT EXISTS turn_materializer (
    name          TEXT        NOT NULL PRIMARY KEY,
    holder        TEXT        NOT NULL DEFAULT '',
    lease_expires TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    cursor        BIGINT      NOT NULL DEFAULT 0
);

-- Seed the resume cursor from the projection that already exists: the
-- previous (uncoordinated) materializer applied events in global order,
-- so every event at or below the highest per-session checkpoint was
-- processed. Starting 10,000 events earlier (the default open-turn
-- lookback) rebuilds the accumulators of turns still open; older open
-- turns adopt their rows. The first boot after this migration therefore
-- does not re-read the whole event log either.
INSERT INTO turn_materializer (name, cursor)
SELECT 'turns', GREATEST(COALESCE(MAX(checkpoint), 0) - 10000, 0) FROM turn_sessions
ON CONFLICT (name) DO NOTHING;

ALTER TABLE turn_rows ADD COLUMN IF NOT EXISTS identity_repair BOOLEAN NOT NULL DEFAULT false;

UPDATE turn_rows SET identity_repair = true
WHERE row_json LIKE '%"Query":{"Text":"","At":%'
   OR row_json LIKE '%"Agent":{"ID":"","Name":"",%';

CREATE INDEX IF NOT EXISTS turn_rows_identity_repair
    ON turn_rows (tenant_id, user_id, session_id, turn_id)
    WHERE identity_repair;

INSERT INTO schema_migrations (version)
VALUES (2)
ON CONFLICT DO NOTHING;
