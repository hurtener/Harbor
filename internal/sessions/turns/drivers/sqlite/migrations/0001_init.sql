-- Initial SQLite turns driver schema — the durable tail-paged
-- conversation-turn projection store (turns.Store).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--   - `turn_rows` is keyed by the EXACT isolation triple
--     (tenant, user, session) plus the root foreground turn key
--     (TurnID = the run's task id). `sequence` is the immutable
--     per-session order key minted at append; the
--     (tenant, user, session, sequence, turn_id) index is the
--     keyset-paging backbone — every tail page is a bounded index
--     RANGE scan, never an OFFSET over the session's history.
--   - `dto` is the COMPLETE renderable turn DTO (JSON), persisted
--     byte-exact. The two DYNAMIC BOUNDED collections — content-free
--     activity rows and MCP App references — live in child tables;
--     the row DTO carries the row-level component metadata and the
--     honest availability / overflow fields (Activity.Complete /
--     More / Dropped / Totals, Reasoning.Dropped, per-app and
--     per-attachment availability).
--   - `turn_activity_rows` holds the ordered content-free activity
--     window (position = the immutable 0-based feed ordinal, oldest
--     first). Rows are replaced wholesale on each accepted write.
--   - `turn_apps` is keyed by the EXACT App replacement identity
--     (effective_agent_id, server_id, resource_uri) within the turn:
--     two refs with the same identity in one turn are impossible at
--     the storage level (the write fails loudly instead of silently
--     corrupting order), and `position` preserves first-declaration
--     order on the ordered read path.
--   - `turn_session_state` holds per-session write state: the next
--     sequence counter, the monotonic idempotent checkpoint, and the
--     explicit truncation flag (retention eviction is never silent).
--     Deleted by DeleteScope.
--   - `turn_fences` is the STORE-LOCAL durable erasure fence /
--     tombstone. DeleteScope NEVER touches it — an erased session
--     stays fenced across replay and restart (no resurrection).
--   - `turn_snapshot_gens` holds the projection snapshot generation
--     (the as-of retention generation) every page cursor binds to. It
--     ALSO survives DeleteScope (and is advanced by it), so a cursor
--     minted before an erase can never page the post-erase projection.

CREATE TABLE IF NOT EXISTS turn_rows (
    tenant               TEXT    NOT NULL,
    user                 TEXT    NOT NULL,
    session              TEXT    NOT NULL,
    turn_id              TEXT    NOT NULL,
    sequence             INTEGER NOT NULL,
    effective_agent_id   TEXT    NOT NULL DEFAULT '',
    sealed               INTEGER NOT NULL DEFAULT 0,
    version              INTEGER NOT NULL DEFAULT 0,
    dto                  BLOB    NOT NULL,
    PRIMARY KEY (tenant, user, session, turn_id)
);

-- Sequence uniqueness within the session: the immutable order key
-- (per-session counters; a duplicate sequence is a driver bug and
-- must fail loudly, never page silently).
CREATE UNIQUE INDEX IF NOT EXISTS turn_rows_session_seq_uidx
    ON turn_rows (tenant, user, session, sequence);

-- Keyset-paging backbone: newest-first tail reads without OFFSET.
CREATE INDEX IF NOT EXISTS turn_rows_keyset_idx
    ON turn_rows (tenant, user, session, sequence, turn_id);

-- Agent axis: a session's turns under one effective agent.
CREATE INDEX IF NOT EXISTS turn_rows_agent_idx
    ON turn_rows (tenant, user, session, effective_agent_id);

CREATE TABLE IF NOT EXISTS turn_activity_rows (
    tenant    TEXT    NOT NULL,
    user      TEXT    NOT NULL,
    session   TEXT    NOT NULL,
    turn_id   TEXT    NOT NULL,
    position  INTEGER NOT NULL,
    dto       BLOB    NOT NULL,
    PRIMARY KEY (tenant, user, session, turn_id, position)
);

CREATE TABLE IF NOT EXISTS turn_apps (
    tenant              TEXT    NOT NULL,
    user                TEXT    NOT NULL,
    session             TEXT    NOT NULL,
    turn_id             TEXT    NOT NULL,
    position            INTEGER NOT NULL,
    effective_agent_id  TEXT    NOT NULL,
    server_id           TEXT    NOT NULL,
    resource_uri        TEXT    NOT NULL,
    dto                 BLOB    NOT NULL,
    PRIMARY KEY (tenant, user, session, turn_id, effective_agent_id, server_id, resource_uri)
);

-- Ordered read of one turn's App collection (first-declaration order).
CREATE INDEX IF NOT EXISTS turn_apps_position_idx
    ON turn_apps (tenant, user, session, turn_id, position);

-- The App identity's effective-agent axis within the session.
CREATE INDEX IF NOT EXISTS turn_apps_agent_idx
    ON turn_apps (tenant, user, session, effective_agent_id);

CREATE TABLE IF NOT EXISTS turn_session_state (
    tenant      TEXT    NOT NULL,
    user        TEXT    NOT NULL,
    session     TEXT    NOT NULL,
    next_seq    INTEGER NOT NULL DEFAULT 1,
    checkpoint  INTEGER NOT NULL DEFAULT 0,
    truncated   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, user, session)
);

CREATE TABLE IF NOT EXISTS turn_fences (
    tenant     TEXT      NOT NULL,
    user       TEXT      NOT NULL,
    session    TEXT      NOT NULL,
    fenced_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant, user, session)
);

CREATE TABLE IF NOT EXISTS turn_snapshot_gens (
    tenant    TEXT    NOT NULL,
    user      TEXT    NOT NULL,
    session   TEXT    NOT NULL,
    gen       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, user, session)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
