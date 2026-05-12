-- Phase 37 — initial LocalDB SQLite SkillStore schema (RFC §6.7, brief 04).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--   - `skills` is keyed by `(tenant, user, session, scope, name)`.
--     Identity-mandatory: every row carries the full triple. RunID
--     is NOT part of the key (skills are session-scoped at the
--     storage layer per brief 04 §4.3; the run id only appears
--     inside OriginRef for generator provenance).
--   - JSON-encoded slice columns (`tags_json`, `steps_json`, ...)
--     are stored as TEXT. The denormalised `tags_text` column is
--     whitespace-joined for the FTS5 virtual table to index without
--     a join (brief 04 §4.4 — porter tokenizer over a single text
--     column per skill).
--   - `origin` is `'pack'` | `'generated'`. The Upsert path's
--     pack-overwrite refusal short-circuits on `existing.origin =
--     'pack' AND incoming != 'pack'` (brief 04 §4.8, RFC §6.7).
--   - `content_hash` is the canonical sha256 hex computed by
--     `skills.CanonicalContentHash` (D-046). Used for LWW + idem-
--     potency.
--   - `created_at` / `updated_at` / `last_used` default to
--     CURRENT_TIMESTAMP; the driver writes UTC `time.Now()`
--     explicitly so the value is independent of the SQLite engine's
--     clock.
--   - `extra_json` is the JSON encoding of `Skill.Extra`. Drivers
--     accept `NULL` and the empty object equivalently.

CREATE TABLE IF NOT EXISTS skills (
    tenant            TEXT      NOT NULL,
    user              TEXT      NOT NULL,
    session           TEXT      NOT NULL,
    scope             TEXT      NOT NULL,
    name              TEXT      NOT NULL,
    title             TEXT      NOT NULL DEFAULT '',
    description       TEXT      NOT NULL DEFAULT '',
    trigger           TEXT      NOT NULL,
    task_type         TEXT      NOT NULL DEFAULT '',
    tags_json         TEXT      NOT NULL DEFAULT '[]',
    tags_text         TEXT      NOT NULL DEFAULT '',
    steps_json        TEXT      NOT NULL DEFAULT '[]',
    preconditions_json TEXT     NOT NULL DEFAULT '[]',
    failure_modes_json TEXT     NOT NULL DEFAULT '[]',
    required_tools_json TEXT    NOT NULL DEFAULT '[]',
    required_ns_json   TEXT     NOT NULL DEFAULT '[]',
    required_tags_json TEXT     NOT NULL DEFAULT '[]',
    origin            TEXT      NOT NULL,
    origin_ref        TEXT      NOT NULL DEFAULT '',
    scope_tenant      TEXT      NOT NULL DEFAULT '',
    scope_project     TEXT      NOT NULL DEFAULT '',
    content_hash      TEXT      NOT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    use_count         INTEGER   NOT NULL DEFAULT 0,
    extra_json        TEXT      NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant, user, session, scope, name)
);

CREATE INDEX IF NOT EXISTS skills_by_origin
    ON skills (tenant, user, session, origin, name);

CREATE INDEX IF NOT EXISTS skills_by_updated
    ON skills (tenant, user, session, updated_at DESC, name ASC);

-- FTS5 virtual table over the indexable fields. External-content
-- model (`content='skills' content_rowid='rowid'`) avoids storing the
-- text twice; triggers below keep it in sync. The driver creates
-- this table inside a SAVEPOINT at open and rolls back if the SQLite
-- build lacks FTS5 (`_ensure_fts`-style detection per brief 04 §4.4);
-- the regex/exact fallback path is the ladder's safety net.
CREATE VIRTUAL TABLE IF NOT EXISTS skills_fts USING fts5(
    name,
    title,
    trigger,
    description,
    tags_text,
    content='skills',
    content_rowid='rowid',
    tokenize='porter unicode61 remove_diacritics 1'
);

CREATE TRIGGER IF NOT EXISTS skills_ai AFTER INSERT ON skills BEGIN
    INSERT INTO skills_fts(rowid, name, title, trigger, description, tags_text)
    VALUES (new.rowid, new.name, new.title, new.trigger, new.description, new.tags_text);
END;

CREATE TRIGGER IF NOT EXISTS skills_ad AFTER DELETE ON skills BEGIN
    INSERT INTO skills_fts(skills_fts, rowid, name, title, trigger, description, tags_text)
    VALUES ('delete', old.rowid, old.name, old.title, old.trigger, old.description, old.tags_text);
END;

CREATE TRIGGER IF NOT EXISTS skills_au AFTER UPDATE ON skills BEGIN
    INSERT INTO skills_fts(skills_fts, rowid, name, title, trigger, description, tags_text)
    VALUES ('delete', old.rowid, old.name, old.title, old.trigger, old.description, old.tags_text);
    INSERT INTO skills_fts(rowid, name, title, trigger, description, tags_text)
    VALUES (new.rowid, new.name, new.title, new.trigger, new.description, new.tags_text);
END;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
