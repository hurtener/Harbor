-- Phase 243 / D-422 — durable installed-package storage.
--
-- The installed-package atomic unit (canonical stored skill + versioned
-- PackageHash + ordered support manifest with bounded immutable support
-- bytes) is the storage floor of the complete skill-package contract.
-- The unit is keyed at the session-zeroed (tenant, user, effective-agent,
-- name) ScopeUser rung and is written, replaced, read, and erased as ONE
-- transaction per package: a reader never sees the skill body without
-- every support byte and never sees a partial replacement.
--
-- Two tables:
--
--   - `installed_packages` holds the unit envelope: the effective-agent
--     key, the winner's origin, the versioned PackageHash, the package
--     Version, the canonical serialization of the package (the
--     identity-bearing manifest WITHOUT the materialized support bytes),
--     and the canonical stored skill as JSON. The skill membership row
--     the legacy read surface sees lives in the existing `skills` table
--     (session-zeroed ScopeUser rung, effective agent bound); it is
--     written and removed in the SAME transaction as this envelope.
--
--   - `installed_support` holds every support file of the installed unit:
--     canonical path, MIME, exact size, digest, and the bounded immutable
--     support bytes (BLOB). The manifest in `installed_packages` and the
--     rows here are one unit: the FK is the structural guarantee that a
--     support row can never outlive its package envelope.
--
-- Forward-only. Editing this file after merge is forbidden (AGENTS.md §13).

CREATE TABLE IF NOT EXISTS installed_packages (
    tenant          TEXT      NOT NULL,
    user            TEXT      NOT NULL,
    agent_id        TEXT      NOT NULL,
    name            TEXT      NOT NULL,
    origin          TEXT      NOT NULL,
    package_hash    TEXT      NOT NULL,
    package_version TEXT      NOT NULL DEFAULT '',
    package_json    TEXT      NOT NULL,
    skill_json      TEXT      NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant, user, agent_id, name)
);

CREATE TABLE IF NOT EXISTS installed_support (
    tenant   TEXT      NOT NULL,
    user     TEXT      NOT NULL,
    agent_id TEXT      NOT NULL,
    name     TEXT      NOT NULL,
    path     TEXT      NOT NULL,
    mime     TEXT      NOT NULL,
    size     INTEGER   NOT NULL,
    digest   TEXT      NOT NULL,
    data     BLOB      NOT NULL,
    PRIMARY KEY (tenant, user, agent_id, name, path),
    FOREIGN KEY (tenant, user, agent_id, name)
        REFERENCES installed_packages (tenant, user, agent_id, name)
        ON DELETE CASCADE
);

-- The composite PK already covers the identity prefix; this index makes
-- the by-package scan for the ordered support manifest cheap and explicit.
CREATE INDEX IF NOT EXISTS installed_support_by_package
    ON installed_support (tenant, user, agent_id, name);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (3);
