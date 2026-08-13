-- Agent binding is selection metadata, never an isolation principal.
ALTER TABLE skills ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE skills DROP CONSTRAINT IF EXISTS skills_pkey;
ALTER TABLE skills ADD PRIMARY KEY (tenant_id, user_id, session_id, scope, agent_id, name);
DROP INDEX IF EXISTS skills_by_origin;
CREATE INDEX skills_by_origin ON skills (tenant_id, user_id, session_id, agent_id, origin, name);
DROP INDEX IF EXISTS skills_by_updated;
CREATE INDEX skills_by_updated ON skills (tenant_id, user_id, session_id, agent_id, updated_at DESC, name ASC);
INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT DO NOTHING;
