-- Artifact notes: a short, mutable annotation on an artifact (RFC §6.10).
--
-- Forward-only migration. The note never participates in the key or the
-- content address; an empty string means no note. Existing rows get ''.

ALTER TABLE artifacts_blobs ADD COLUMN note TEXT NOT NULL DEFAULT '';

INSERT OR IGNORE INTO schema_migrations(version) VALUES (3);
