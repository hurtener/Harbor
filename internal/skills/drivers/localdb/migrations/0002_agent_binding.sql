-- Rebuild the SQLite table so agent_id participates in the namespace key.
DROP TRIGGER IF EXISTS skills_ai;
DROP TRIGGER IF EXISTS skills_ad;
DROP TRIGGER IF EXISTS skills_au;
DROP TABLE IF EXISTS skills_fts;
ALTER TABLE skills RENAME TO skills_legacy;
CREATE TABLE skills (
    tenant TEXT NOT NULL, user TEXT NOT NULL, session TEXT NOT NULL,
    scope TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', trigger TEXT NOT NULL,
    task_type TEXT NOT NULL DEFAULT '', tags_json TEXT NOT NULL DEFAULT '[]', tags_text TEXT NOT NULL DEFAULT '',
    steps_json TEXT NOT NULL DEFAULT '[]', preconditions_json TEXT NOT NULL DEFAULT '[]',
    failure_modes_json TEXT NOT NULL DEFAULT '[]', required_tools_json TEXT NOT NULL DEFAULT '[]',
    required_ns_json TEXT NOT NULL DEFAULT '[]', required_tags_json TEXT NOT NULL DEFAULT '[]',
    origin TEXT NOT NULL, origin_ref TEXT NOT NULL DEFAULT '', scope_tenant TEXT NOT NULL DEFAULT '',
    scope_project TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, use_count INTEGER NOT NULL DEFAULT 0,
    extra_json TEXT NOT NULL DEFAULT '{}', PRIMARY KEY (tenant, user, session, scope, agent_id, name)
);
INSERT INTO skills (tenant,user,session,scope,name,title,description,trigger,task_type,tags_json,tags_text,steps_json,preconditions_json,failure_modes_json,required_tools_json,required_ns_json,required_tags_json,origin,origin_ref,scope_tenant,scope_project,content_hash,created_at,updated_at,last_used,use_count,extra_json)
SELECT tenant,user,session,scope,name,title,description,trigger,task_type,tags_json,tags_text,steps_json,preconditions_json,failure_modes_json,required_tools_json,required_ns_json,required_tags_json,origin,origin_ref,scope_tenant,scope_project,content_hash,created_at,updated_at,last_used,use_count,extra_json FROM skills_legacy;
DROP TABLE skills_legacy;
CREATE INDEX skills_by_origin ON skills (tenant,user,session,agent_id,origin,name);
CREATE INDEX skills_by_updated ON skills (tenant,user,session,agent_id,updated_at DESC,name ASC);
CREATE VIRTUAL TABLE skills_fts USING fts5(name,title,trigger,description,tags_text,content='skills',content_rowid='rowid',tokenize='porter unicode61 remove_diacritics 1');
CREATE TRIGGER skills_ai AFTER INSERT ON skills BEGIN INSERT INTO skills_fts(rowid,name,title,trigger,description,tags_text) VALUES (new.rowid,new.name,new.title,new.trigger,new.description,new.tags_text); END;
CREATE TRIGGER skills_ad AFTER DELETE ON skills BEGIN INSERT INTO skills_fts(skills_fts,rowid,name,title,trigger,description,tags_text) VALUES ('delete',old.rowid,old.name,old.title,old.trigger,old.description,old.tags_text); END;
CREATE TRIGGER skills_au AFTER UPDATE ON skills BEGIN INSERT INTO skills_fts(skills_fts,rowid,name,title,trigger,description,tags_text) VALUES ('delete',old.rowid,old.name,old.title,old.trigger,old.description,old.tags_text); INSERT INTO skills_fts(rowid,name,title,trigger,description,tags_text) VALUES (new.rowid,new.name,new.title,new.trigger,new.description,new.tags_text); END;
INSERT INTO skills_fts(skills_fts) VALUES ('rebuild');
INSERT OR IGNORE INTO schema_migrations(version) VALUES (2);
