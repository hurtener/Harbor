CREATE TABLE IF NOT EXISTS artifact_scope_fences (
    tenant TEXT NOT NULL,
    user TEXT NOT NULL,
    session TEXT NOT NULL,
    fenced INTEGER NOT NULL DEFAULT 0 CHECK (fenced IN (0, 1)),
    PRIMARY KEY (tenant, user, session)
);
