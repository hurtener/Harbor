-- Reconcile the artifact key onto the isolation triple (RFC §6.10 + §9,
-- brief 05).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13); further schema changes land as `0003_*.sql`.
--
-- WHY. `task` is a PROVENANCE ANNOTATION, not an isolation principal:
-- the boundary is `(tenant, user, session)` and a task runs within it.
-- With `task` in the primary key the read path was exact on four fields
-- while `List` treated an empty `task` as a wildcard, so a listing
-- enumerated refs a read resolved as not-found. Narrowing the read key
-- to the triple narrows the WRITE key with it, which is what this
-- migration performs.
--
-- CONSEQUENCE, STATED. Two tasks that stored identical bytes in one
-- session held two rows, one stamp each. They collapse to ONE row, so
-- `ArtifactRef.Scope.TaskID` becomes first-writer-wins and a `task`
-- filter no longer returns a row carrying a later writer's stamp. This
-- is inherent to a content-addressed store: `id` is derived from the
-- bytes, so "which run produced these bytes" has no single answer once
-- two runs produce them.
--
-- COLLAPSE RULE. Write order is not recoverable from the stored rows
-- (there is no insertion-order column in the schema), so the survivor is
-- chosen by an explicit, deterministic rule rather than by physical
-- layout: the row with the LEXICOGRAPHICALLY SMALLEST `task` survives.
-- The SQLite migration, the filesystem driver's index rebuild and the
-- object-store driver's key resolution apply the same rule, so all four
-- agree. The bytes are identical across the collapsed rows by
-- content-addressing; only the provenance stamp is at stake.

DELETE FROM artifacts_blobs
WHERE EXISTS (
    SELECT 1 FROM artifacts_blobs AS b
    WHERE b.tenant    = artifacts_blobs.tenant
      AND b."user"    = artifacts_blobs."user"
      AND b.session   = artifacts_blobs.session
      AND b.namespace = artifacts_blobs.namespace
      AND b.id        = artifacts_blobs.id
      AND b.task      < artifacts_blobs.task
);

ALTER TABLE artifacts_blobs DROP CONSTRAINT artifacts_blobs_pkey;

ALTER TABLE artifacts_blobs
    ADD CONSTRAINT artifacts_blobs_pkey
    PRIMARY KEY (tenant, "user", session, namespace, id);

INSERT INTO schema_migrations (version)
VALUES (2)
ON CONFLICT DO NOTHING;
