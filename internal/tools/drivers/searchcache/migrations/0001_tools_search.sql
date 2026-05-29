-- 0001_tools_search.sql — Phase 107c / D-167 initial schema.
-- Mirrors skills/localdb/migrations/0001_skills.sql shape.

CREATE TABLE IF NOT EXISTS tool_cache (
    name        TEXT NOT NULL PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    tags        TEXT NOT NULL DEFAULT '[]',
    args_schema TEXT NOT NULL DEFAULT '{}',
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE IF NOT EXISTS tool_cache_fts USING fts5(
    name,
    description,
    tags,
    content='tool_cache',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS tool_cache_ai AFTER INSERT ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(rowid, name, description, tags)
    VALUES (new.rowid, new.name, new.description, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS tool_cache_ad AFTER DELETE ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(tool_cache_fts, rowid, name, description, tags)
    VALUES ('delete', old.rowid, old.name, old.description, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS tool_cache_au AFTER UPDATE ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(tool_cache_fts, rowid, name, description, tags)
    VALUES ('delete', old.rowid, old.name, old.description, old.tags);
    INSERT INTO tool_cache_fts(rowid, name, description, tags)
    VALUES (new.rowid, new.name, new.description, new.tags);
END;

CREATE TABLE IF NOT EXISTS tool_cache_migrations (
    version INTEGER PRIMARY KEY
);

INSERT OR IGNORE INTO tool_cache_migrations(version) VALUES (1);
