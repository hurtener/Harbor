-- 0002_tools_search_server_metadata.sql — index the providing server's
-- display metadata alongside each tool and rank by relevance.
--
-- The cache is a derived mirror of the in-memory tool catalog: the catalog
-- re-registers every tool at boot and reconciles the index on each change,
-- so the old rows carry nothing worth preserving. The FTS5 external-content
-- table cannot gain columns in place, so the schema is rebuilt.

DROP TRIGGER IF EXISTS tool_cache_ai;
DROP TRIGGER IF EXISTS tool_cache_ad;
DROP TRIGGER IF EXISTS tool_cache_au;
DROP TABLE IF EXISTS tool_cache_fts;
DROP TABLE IF EXISTS tool_cache;

CREATE TABLE tool_cache (
    name               TEXT NOT NULL PRIMARY KEY,
    description        TEXT NOT NULL DEFAULT '',
    tags               TEXT NOT NULL DEFAULT '[]',
    args_schema        TEXT NOT NULL DEFAULT '{}',
    source             TEXT NOT NULL DEFAULT '',
    server_title       TEXT NOT NULL DEFAULT '',
    server_description TEXT NOT NULL DEFAULT '',
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE tool_cache_fts USING fts5(
    name,
    description,
    tags,
    server_title,
    server_description,
    content='tool_cache',
    content_rowid='rowid'
);

CREATE TRIGGER tool_cache_ai AFTER INSERT ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(rowid, name, description, tags, server_title, server_description)
    VALUES (new.rowid, new.name, new.description, new.tags, new.server_title, new.server_description);
END;

CREATE TRIGGER tool_cache_ad AFTER DELETE ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(tool_cache_fts, rowid, name, description, tags, server_title, server_description)
    VALUES ('delete', old.rowid, old.name, old.description, old.tags, old.server_title, old.server_description);
END;

CREATE TRIGGER tool_cache_au AFTER UPDATE ON tool_cache BEGIN
    INSERT INTO tool_cache_fts(tool_cache_fts, rowid, name, description, tags, server_title, server_description)
    VALUES ('delete', old.rowid, old.name, old.description, old.tags, old.server_title, old.server_description);
    INSERT INTO tool_cache_fts(rowid, name, description, tags, server_title, server_description)
    VALUES (new.rowid, new.name, new.description, new.tags, new.server_title, new.server_description);
END;

INSERT OR IGNORE INTO tool_cache_migrations(version) VALUES (2);
