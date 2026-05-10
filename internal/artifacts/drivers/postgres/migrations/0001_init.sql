-- Phase 18: initial Postgres ArtifactStore schema (RFC §6.10 + §9, brief 05).
--
-- The composite primary key is the artifact scope plus namespace + id:
--   (tenant, user, session, task, namespace, id)
--
-- This mirrors the inmem + fs drivers' (scope, id) shape and the
-- SQLite driver's table shape, so cross-driver conformance is
-- identical. Task may be empty for session-scoped artifacts; the
-- column is NOT NULL but accepts the empty string (parallel to the
-- state driver's empty RunID rule).
--
-- `id` is the content-addressed identifier
-- `{namespace}_{sha256_hex[:12]}`; embedding the namespace in the id
-- keeps namespaces distinct in the dedup key.
--
-- `bytes` is BYTEA — opaque payload; the driver does not interpret
-- or re-redact it (audit redaction is upstream of Put — see
-- internal/artifacts/artifacts.go's package godoc).
--
-- `source_json` is the `json.Marshal`ed `Source map[string]any` from
-- `PutOpts`; non-encodable values fail at marshal time per Phase 17's
-- documented behavior. Stored as BYTEA rather than JSONB so the
-- payload is opaque to Postgres (matches SQLite's BLOB shape).
--
-- `sha256` carries the full hex digest (64 chars). `id` only embeds
-- the truncated 12-char prefix.
--
-- Forward-only: editing this file post-merge is forbidden (AGENTS.md §13).

CREATE TABLE IF NOT EXISTS artifacts_blobs (
    tenant      TEXT     NOT NULL,
    "user"      TEXT     NOT NULL,
    session     TEXT     NOT NULL,
    task        TEXT     NOT NULL,
    namespace   TEXT     NOT NULL,
    id          TEXT     NOT NULL,
    mime_type   TEXT     NOT NULL,
    size_bytes  BIGINT   NOT NULL,
    filename    TEXT     NOT NULL,
    sha256      TEXT     NOT NULL,
    source_json BYTEA    NOT NULL,
    bytes       BYTEA    NOT NULL,
    PRIMARY KEY (tenant, "user", session, task, namespace, id)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version)
VALUES (1)
ON CONFLICT DO NOTHING;
