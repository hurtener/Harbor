-- Phase 15 — initial SQLite StateStore schema (RFC §6.11 + §9, brief 05).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--   - `state_records` mirrors the abstract slot key
--     `(tenant, user, session, run, kind)` directly. RunID may be empty
--     (session-scoped state); the composite primary key still serializes
--     the slot uniquely.
--   - `event_id` carries the ULID-shaped idempotency key (state.EventID)
--     and is uniquely indexed so `LoadByEventID` resolves in O(log n).
--   - `bytes` is the opaque caller payload (BLOB); the driver does not
--     interpret or re-redact it (audit redaction is upstream of Save —
--     see internal/state/state.go's package godoc).
--   - `version` is a hint for optimistic-concurrency at the
--     typed-wrapper layer; the StateStore does not enforce CAS.
--   - `updated_at` defaults to CURRENT_TIMESTAMP when callers leave it
--     zero; callers MAY override (useful for tests with controllable
--     clocks).

CREATE TABLE IF NOT EXISTS state_records (
    tenant     TEXT      NOT NULL,
    user       TEXT      NOT NULL,
    session    TEXT      NOT NULL,
    run        TEXT      NOT NULL,
    kind       TEXT      NOT NULL,
    event_id   TEXT      NOT NULL,
    version    INTEGER   NOT NULL DEFAULT 0,
    bytes      BLOB,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant, user, session, run, kind)
);

CREATE UNIQUE INDEX IF NOT EXISTS state_records_event_id_idx
    ON state_records (event_id);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
