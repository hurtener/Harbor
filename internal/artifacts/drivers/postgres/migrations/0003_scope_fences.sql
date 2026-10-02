CREATE TABLE IF NOT EXISTS artifact_scope_fences (
    tenant TEXT NOT NULL,
    "user" TEXT NOT NULL,
    session TEXT NOT NULL,
    fenced BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (tenant, "user", session)
);
