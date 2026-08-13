-- Initial SQLite schema for the observability-rollup Store.
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on the
-- filename version not yet present in `schema_migrations`.
--
-- Schema notes:
--
--   - `rollup_rows` carries one row per (fixed-UTC MINUTE bucket start,
--     authoritative dimension values). The bucket start is stored as an
--     exact INTEGER of unix nanoseconds (never a REAL, never a float) so
--     the fixed-UTC minute grid round-trips exactly and range scans are
--     pure integer comparisons. The primary key is the full row key
--     (bucket + tenant/user/session/model), which is exactly the domain
--     row key; a second ApplyBatch touching the same key UPSERTs it.
--   - Every measure column is INTEGER (exact int64): counts, tokens,
--     latency milliseconds, and cost in integer micro-units of USD. There
--     are deliberately NO REAL/DOUBLE columns anywhere in the schema —
--     cost is never stored or accumulated as float64 (see the domain's
--     precision model). `llm_latency_min_ms` / `llm_latency_max_ms` are
--     fold columns (group min/max), all others are sums; both families
--     stay exact integers.
--   - The secondary indexes mirror the reference driver's indexes: the
--     bucket+tenant and bucket+tenant+user prefixes for the common
--     windowed reads, the full (tenant, user, session) triple for the
--     erasure-fence delete, and one axis index per remaining closed
--     dimension so any bounded query resolves its candidates through an
--     index — never a full-table scan of the projection rows.
--   - `rollup_checkpoint` is the durable watermark (the last applied
--     local durable sequence). It is a single-row table: the CHECK
--     constraint pins id to 1 so no second row can ever exist.
--   - `rollup_fence` is the PERMANENT erasure fence: one row per erased
--     (tenant, user, session) triple. There is no unfence operation in
--     this domain, and `Rebuild` must never clear it, so an erased
--     session can never be resurrected by replay or reprojection.
--   - `schema_migrations` is the shared runner's bookkeeping table.

CREATE TABLE IF NOT EXISTS rollup_rows (
    bucket_start           INTEGER NOT NULL,  -- fixed UTC minute-grid bucket start, unix nanoseconds (exact int64)
    tenant_id              TEXT    NOT NULL,
    user_id                TEXT    NOT NULL,
    session_id             TEXT    NOT NULL,
    model                  TEXT    NOT NULL DEFAULT '',
    llm_completions        INTEGER NOT NULL DEFAULT 0,
    llm_tokens_prompt      INTEGER NOT NULL DEFAULT 0,
    llm_tokens_completion  INTEGER NOT NULL DEFAULT 0,
    llm_tokens_reasoning   INTEGER NOT NULL DEFAULT 0,
    llm_tokens_cache_read  INTEGER NOT NULL DEFAULT 0,
    llm_tokens_cache_write INTEGER NOT NULL DEFAULT 0,
    llm_tokens_total       INTEGER NOT NULL DEFAULT 0,
    llm_cost_micros        INTEGER NOT NULL DEFAULT 0,
    llm_latency_count      INTEGER NOT NULL DEFAULT 0,
    llm_latency_sum_ms     INTEGER NOT NULL DEFAULT 0,
    llm_latency_min_ms     INTEGER NOT NULL DEFAULT 0,
    llm_latency_max_ms     INTEGER NOT NULL DEFAULT 0,
    tasks_completed        INTEGER NOT NULL DEFAULT 0,
    tasks_failed           INTEGER NOT NULL DEFAULT 0,
    tasks_cancelled        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_start, tenant_id, user_id, session_id, model)
);

CREATE INDEX IF NOT EXISTS rollup_rows_bucket_tenant_idx
    ON rollup_rows (bucket_start, tenant_id);

CREATE INDEX IF NOT EXISTS rollup_rows_bucket_tenant_user_idx
    ON rollup_rows (bucket_start, tenant_id, user_id);

CREATE INDEX IF NOT EXISTS rollup_rows_triple_idx
    ON rollup_rows (tenant_id, user_id, session_id);

CREATE INDEX IF NOT EXISTS rollup_rows_user_idx
    ON rollup_rows (user_id);

CREATE INDEX IF NOT EXISTS rollup_rows_session_idx
    ON rollup_rows (session_id);

CREATE INDEX IF NOT EXISTS rollup_rows_model_idx
    ON rollup_rows (model);

CREATE TABLE IF NOT EXISTS rollup_checkpoint (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    sequence INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS rollup_fence (
    tenant_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    session_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_id)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER   PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
