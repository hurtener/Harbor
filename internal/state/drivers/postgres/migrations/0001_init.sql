-- Phase 16: initial Postgres StateStore schema.
--
-- The composite primary key is the identity quadruple plus Kind:
--   (tenant_id, user_id, session_id, run_id, kind)
--
-- This mirrors the inmem driver's `indexKey` struct and the SQLite
-- driver's table shape, so cross-driver conformance is identical.
-- RunID is allowed to be empty for session-scoped state (RFC §6.11);
-- the column is NOT NULL but accepts the empty string.
--
-- `bytes` is BYTEA, not JSONB. The state.StateStore interface stores
-- opaque payloads (RFC §6.11 + D-027); JSONB would constrain payloads
-- to valid JSON and leak a persistence-implementation choice into
-- consumers. See docs/plans/phase-16-state-postgres.md "Non-goals".
--
-- `event_id` carries a unique secondary index — LoadByEventID needs
-- it, and the constraint guards against duplicate-id leaks if the
-- driver's idempotency check ever races (it does not, but defense in
-- depth is cheap here).
--
-- Forward-only: editing this file post-merge is forbidden (AGENTS.md §13).

CREATE TABLE IF NOT EXISTS state_records (
    tenant_id  TEXT        NOT NULL,
    user_id    TEXT        NOT NULL,
    session_id TEXT        NOT NULL,
    run_id     TEXT        NOT NULL,
    kind       TEXT        NOT NULL,
    event_id   TEXT        NOT NULL,
    version    INTEGER     NOT NULL,
    bytes      BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_id, run_id, kind)
);

CREATE UNIQUE INDEX IF NOT EXISTS state_records_event_id_idx
    ON state_records (event_id);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version)
VALUES (1)
ON CONFLICT DO NOTHING;
