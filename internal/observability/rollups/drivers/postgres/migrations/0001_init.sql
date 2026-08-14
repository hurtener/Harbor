-- Initial Postgres rollups schema.
--
-- The row table mirrors the shared contract documented in
-- internal/observability/rollups/store.go: one row per
-- (bucket_start, tenant_id, user_id, session_id, model), where bucket_start
-- is on the fixed-UTC MINUTE grid (the storage granularity) and every
-- measure is exact BIGINT — never DOUBLE PRECISION. Cost is integer
-- micro-units of USD; latency min/max are the per-row folds.
--
-- The three secondary indexes give the bounded window + dimension queries
-- their indexed access path (the memstore parity pin): the planner resolves
-- WHERE bucket_start >= $1 AND bucket_start < $2 AND tenant_id = ANY(...)
-- against idx_rollup_bucket_tenant, never a full-table scan.
--
-- The checkpoint and fence state are single-row tables:
--   rollup_checkpoint — the single writer's durable local sequence (id=1);
--                       the conditional-advance serialization point.
--   rollup_fence      — PERMANENT erasure fences (never cleared by Rebuild),
--                       so an erased session is never resurrected.
--
-- Forward-only: editing this file post-merge is forbidden (AGENTS.md §13).

CREATE TABLE IF NOT EXISTS rollup_rows (
    bucket_start           TIMESTAMPTZ NOT NULL,
    tenant_id              TEXT        NOT NULL,
    user_id                TEXT        NOT NULL,
    session_id             TEXT        NOT NULL,
    model                  TEXT        NOT NULL DEFAULT '',
    llm_completions        BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_prompt      BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_completion  BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_reasoning   BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_cache_read  BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_cache_write BIGINT      NOT NULL DEFAULT 0,
    llm_tokens_total       BIGINT      NOT NULL DEFAULT 0,
    llm_cost_micros        BIGINT      NOT NULL DEFAULT 0,
    llm_latency_count      BIGINT      NOT NULL DEFAULT 0,
    llm_latency_sum_ms     BIGINT      NOT NULL DEFAULT 0,
    llm_latency_min_ms     BIGINT      NOT NULL DEFAULT 0,
    llm_latency_max_ms     BIGINT      NOT NULL DEFAULT 0,
    tasks_completed        BIGINT      NOT NULL DEFAULT 0,
    tasks_failed           BIGINT      NOT NULL DEFAULT 0,
    tasks_cancelled        BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_start, tenant_id, user_id, session_id, model)
);

CREATE INDEX IF NOT EXISTS idx_rollup_bucket_tenant
    ON rollup_rows (bucket_start, tenant_id);

CREATE INDEX IF NOT EXISTS idx_rollup_bucket_tenant_user
    ON rollup_rows (bucket_start, tenant_id, user_id);

CREATE INDEX IF NOT EXISTS idx_rollup_tenant
    ON rollup_rows (tenant_id);

CREATE TABLE IF NOT EXISTS rollup_checkpoint (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    sequence BIGINT NOT NULL
);

INSERT INTO rollup_checkpoint (id, sequence) VALUES (1, 0)
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS rollup_fence (
    tenant_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    session_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_id)
);

INSERT INTO schema_migrations (version) VALUES (1)
ON CONFLICT DO NOTHING;
