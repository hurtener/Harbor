-- Artifact notes: a short, mutable annotation on an artifact (RFC §6.10).
--
-- Forward-only migration. The note never participates in the key or the
-- content address; an empty string means no note. Existing rows get ''.

ALTER TABLE artifacts_blobs ADD COLUMN IF NOT EXISTS note TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version)
VALUES (3)
ON CONFLICT DO NOTHING;
