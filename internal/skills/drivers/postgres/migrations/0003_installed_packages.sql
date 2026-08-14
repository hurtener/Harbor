-- Complete installed-package storage (the atomic durable unit).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files.
--
-- This migration adds the durable installed form of a complete skill
-- package (RFC §6.7 + §9): the canonical
-- semantic skill PLUS the versioned PackageHash PLUS the ordered
-- normalized support manifest with bounded immutable support bytes.
-- The installed form is self-contained — later sessions never
-- dereference the source/staging artifacts the package was validated
-- from; the source artifact is provenance only.
--
-- Schema notes:
--   - `installed_packages` is keyed by the session-zeroed
--     `(tenant_id, user_id, agent_id, name)` target on the ScopeUser
--     rung. `package_hash` is the versioned PackageHash
--     ("v1:<64-hex>") — the replacement precondition, the receipt
--     identity, and the `skillpkg://` authority of every support URI
--     the stored body materializes. `package_version` is the
--     package's Version string (the additional exact condition
--     constraint). `origin` mirrors the membership skill row's origin
--     ('pack' | 'generated') so the origin-precedence gate can be
--     applied without a join. `skill_json` is the JSON encoding of
--     the canonical stored semantic skill (the same fields the
--     `skills` membership row carries — the installed unit is
--     self-contained). `canonical` is the canonical deterministic
--     serialization of the Package (identity bytes, manifest WITHOUT
--     materialized data — the manifest's digest + size identify each
--     support file).
--   - `installed_package_supports` carries one row per ordered
--     support-manifest entry: canonical path, MIME, exact size,
--     digest, and the bounded immutable support BYTES (BYTEA). The
--     manifest ORDER lives in the canonical package serialization
--     (ordered by canonical path); the PK is the exact file identity
--     so a package can never hold two entries for one path and
--     ResolveSupport addresses a file directly.
--   - Both tables are written in ONE transaction together with the
--     `skills` membership row (session-zeroed ScopeUser
--     agent-bound), so a reader never observes the skill body without
--     every support byte and never observes a partial replacement.
--   - The membership row is an ordinary `skills` row; the legacy
--     mutation surface is fenced off it at the driver boundary
--     (ErrInstalledPackageReadOnly) so legacy Upsert / Delete /
--     DeleteAgent can never tear or silently overwrite the unit.

CREATE TABLE IF NOT EXISTS installed_packages (
    tenant_id       TEXT        NOT NULL,
    user_id         TEXT        NOT NULL,
    agent_id        TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    package_hash    TEXT        NOT NULL,
    package_version TEXT        NOT NULL DEFAULT '',
    origin          TEXT        NOT NULL,
    skill_json      TEXT        NOT NULL,
    canonical       TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, agent_id, name)
);

CREATE TABLE IF NOT EXISTS installed_package_supports (
    tenant_id TEXT   NOT NULL,
    user_id   TEXT   NOT NULL,
    agent_id  TEXT   NOT NULL,
    name      TEXT   NOT NULL,
    path      TEXT   NOT NULL,
    mime      TEXT   NOT NULL,
    size      BIGINT NOT NULL,
    digest    TEXT   NOT NULL,
    data      BYTEA  NOT NULL,
    PRIMARY KEY (tenant_id, user_id, agent_id, name, path)
);

-- Every lookup (Get / Resolve / fence probe / compensation) is keyed by
-- the exact (tenant_id, user_id, agent_id, name[, path]) prefix, which
-- both PRIMARY KEY indexes cover; no secondary index is needed.

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT DO NOTHING;
