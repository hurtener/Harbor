-- Phase 18 — initial SQLite ArtifactStore schema (RFC §6.10 + §9, brief 05).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--   - `artifacts_blobs` mirrors the inmem + fs drivers' (scope, id)
--     primary key directly. The composite primary key
--     `(tenant, user, session, task, namespace, id)` keeps cross-scope
--     dedup decoupled per RFC §6.10 ("same bytes under different scopes
--     intentionally produce two rows").
--   - `task` may be empty (session-scoped artifacts, parallel to the
--     state driver's empty RunID rule); the column is NOT NULL but
--     accepts the empty string.
--   - `id` is the content-addressed identifier
--     `{namespace}_{sha256_hex[:12]}`; embedding the namespace in the
--     id keeps namespaces distinct in the dedup key.
--   - `bytes` is the opaque caller payload (BLOB); the driver does not
--     interpret or re-redact it (audit redaction is upstream of Put —
--     see internal/artifacts/artifacts.go's package godoc).
--   - `source_json` is `json.Marshal`ed `Source map[string]any` from
--     `PutOpts`; non-encodable values fail at marshal time per
--     Phase 17's documented behavior.
--   - `sha256` carries the full hex digest (64 chars). `id` only
--     embeds the truncated 12-char prefix.

CREATE TABLE IF NOT EXISTS artifacts_blobs (
    tenant      TEXT      NOT NULL,
    user        TEXT      NOT NULL,
    session     TEXT      NOT NULL,
    task        TEXT      NOT NULL,
    namespace   TEXT      NOT NULL,
    id          TEXT      NOT NULL,
    mime_type   TEXT      NOT NULL,
    size_bytes  INTEGER   NOT NULL,
    filename    TEXT      NOT NULL,
    sha256      TEXT      NOT NULL,
    source_json BLOB      NOT NULL,
    bytes       BLOB      NOT NULL,
    PRIMARY KEY (tenant, user, session, task, namespace, id)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
