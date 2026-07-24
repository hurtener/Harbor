-- Initial Postgres SkillStore schema (RFC §6.7 + §9, brief 04).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`, and
-- the runner itself is serialised across replicas via
-- `pg_advisory_lock` (see migrations.go) so no two booting replicas
-- race on `CREATE TABLE` / `INSERT INTO schema_migrations`.
--
-- Schema notes (mirrors
-- `internal/skills/drivers/localdb/migrations/0001_init.sql`):
--   - `skills` is keyed by `(tenant_id, user_id, session_id, scope,
--     name)`. Identity-mandatory: every row carries the full triple.
--     RunID is NOT part of the key (skills are session-scoped at the
--     storage layer per brief 04 §4.3; the run id only appears inside
--     origin_ref for generator provenance).
--   - Postgres reserves `user` and `trigger`; the columns are named
--     `user_id` and `trigger_text` to avoid quoting reserved words.
--   - JSON-encoded slice columns (`tags_json`, `steps_json`, ...) are
--     stored as TEXT — the driver marshals/unmarshals the same byte
--     shape the localdb driver uses, so cross-driver round-trips are
--     byte-stable. The denormalised `tags_text` column is
--     whitespace-joined for the full-text index to consume without a
--     join.
--   - `origin` is `'pack'` | `'generated'`. The Upsert path's
--     pack-overwrite refusal short-circuits on `existing.origin =
--     'pack' AND incoming != 'pack'` (brief 04 §4.8, RFC §6.7).
--   - `content_hash` is the canonical sha256 hex computed by
--     `skills.CanonicalContentHash`. Used for LWW + idempotency.
--   - `created_at` / `updated_at` / `last_used` are TIMESTAMPTZ; the
--     driver writes UTC `time.Now()` explicitly so the value is
--     independent of the engine's clock.
--   - `extra_json` is the JSON encoding of `Skill.Extra`. The driver
--     accepts the empty object.
--   - `search_tsv` is a STORED generated `tsvector` over the same
--     indexable fields the SQLite driver's FTS5 table indexes
--     (name | title | trigger | description | tags). The GIN index
--     backs the full-text tier of the backend-agnostic ranking
--     ladder (FTS -> regex -> exact); the regex/exact tiers are the
--     ladder's safety net and never touch this column.

CREATE TABLE IF NOT EXISTS skills (
    tenant_id           TEXT        NOT NULL,
    user_id             TEXT        NOT NULL,
    session_id          TEXT        NOT NULL,
    scope               TEXT        NOT NULL,
    name                TEXT        NOT NULL,
    title               TEXT        NOT NULL DEFAULT '',
    description         TEXT        NOT NULL DEFAULT '',
    trigger_text        TEXT        NOT NULL,
    task_type           TEXT        NOT NULL DEFAULT '',
    tags_json           TEXT        NOT NULL DEFAULT '[]',
    tags_text           TEXT        NOT NULL DEFAULT '',
    steps_json          TEXT        NOT NULL DEFAULT '[]',
    preconditions_json  TEXT        NOT NULL DEFAULT '[]',
    failure_modes_json  TEXT        NOT NULL DEFAULT '[]',
    required_tools_json TEXT        NOT NULL DEFAULT '[]',
    required_ns_json    TEXT        NOT NULL DEFAULT '[]',
    required_tags_json  TEXT        NOT NULL DEFAULT '[]',
    origin              TEXT        NOT NULL,
    origin_ref          TEXT        NOT NULL DEFAULT '',
    scope_tenant        TEXT        NOT NULL DEFAULT '',
    scope_project       TEXT        NOT NULL DEFAULT '',
    content_hash        TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    use_count           INTEGER     NOT NULL DEFAULT 0,
    extra_json          TEXT        NOT NULL DEFAULT '{}',
    search_tsv          TSVECTOR    GENERATED ALWAYS AS (
        to_tsvector('english',
            coalesce(name, '')         || ' ' ||
            coalesce(title, '')        || ' ' ||
            coalesce(trigger_text, '') || ' ' ||
            coalesce(description, '')  || ' ' ||
            coalesce(tags_text, ''))
    ) STORED,
    PRIMARY KEY (tenant_id, user_id, session_id, scope, name)
);

CREATE INDEX IF NOT EXISTS skills_by_origin
    ON skills (tenant_id, user_id, session_id, origin, name);

CREATE INDEX IF NOT EXISTS skills_by_updated
    ON skills (tenant_id, user_id, session_id, updated_at DESC, name ASC);

CREATE INDEX IF NOT EXISTS skills_search_tsv_gin
    ON skills USING GIN (search_tsv);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version)
VALUES (1)
ON CONFLICT DO NOTHING;
