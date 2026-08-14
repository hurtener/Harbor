-- Initial Postgres turn-projection Store schema (the turns Projector's
-- durable driver leg; the v1.28 `sessions.turns.list` /
-- `sessions.turns.get` Protocol surface projects over it).
--
-- Forward-only migration. Editing this file after merge is forbidden
-- (AGENTS.md §9 / §13). Future schema changes land as new files
-- (`0002_*.sql`, `0003_*.sql`, ...). The migration runner gates on
-- the filename version not yet present in `schema_migrations`, and
-- the runner itself is serialised across replicas via
-- `pg_advisory_lock` (see migrations.go) so no two booting replicas
-- race on `CREATE TABLE` / `INSERT INTO schema_migrations`.
--
-- Schema notes:
--   - `turn_rows` is keyed by the EXACT isolation triple +
--     session/root-turn id: `(tenant_id, user_id, session_id,
--     turn_id)` — the row key is the root foreground run's task id
--     WITHIN the triple (turns are session-scoped; run id is NOT a
--     storage identity axis, AGENTS.md §6). The effective agent
--     binding (`effective_agent_id`, the row's `Agent.ID`) is carried
--     as a denormalised, INDEXED column so agent-scoped reads within
--     a session are index-backed — it is selection metadata, never an
--     isolation principal, so it is deliberately NOT part of the
--     primary key.
--   - `sequence` + `turn_id` are the IMMUTABLE newest-first ordering
--     keys; `turn_rows_keyset` backs keyset paging (no OFFSET, no
--     history scan) and retention eviction. `turn_id` is `COLLATE "C"`
--     so the tie-break order is byte-wise, identical to the Go
--     reference ordering the conformance suite pins.
--   - `row_json` is the FULL consumer-safe `turns.TurnRow` envelope
--     (TEXT, the same JSON-as-TEXT convention as the skills Postgres
--     driver): the renderable query / answer, attachment metadata,
--     per-measure honest usage, derived reasoning, content-free
--     activity + exact totals, ordered App refs with availability,
--     the pause component, closed terminal fields, and timing. The
--     scalar columns are DERIVED copies for the indexed /
--     conditional-write paths (identity, ordering, version/sealed
--     guards, last-applied event seq) — the envelope is the
--     transport-authoritative byte shape, so a driver is a transport,
--     never a normalizer.
--   - `turn_sessions` holds the per-session STORE-LOCAL projection
--     state: the atomic local-sequence counter (`next_seq`), the
--     monotonic idempotent checkpoint, the projection snapshot
--     generation (`snapshot`; advanced by erasure so a pre-erase
--     cursor is never confused with a post-erase one), the explicit
--     retention-truncation flag, and the PERMANENT per-session
--     erasure fence (`fenced`). The fence lives in the same table and
--     is checked in the same transactions as the rows it guards;
--     DeleteScope resets everything EXCEPT `fenced` — an erased
--     session stays fenced forever (no replay / restart
--     resurrection).

CREATE TABLE IF NOT EXISTS turn_rows (
    tenant_id              TEXT    NOT NULL,
    user_id                TEXT    NOT NULL,
    session_id             TEXT    NOT NULL,
    turn_id                TEXT    COLLATE "C" NOT NULL,
    effective_agent_id     TEXT    COLLATE "C" NOT NULL DEFAULT '',
    sequence               BIGINT  NOT NULL,
    sealed                 BOOLEAN NOT NULL,
    version                INTEGER NOT NULL,
    last_applied_event_seq BIGINT  NOT NULL,
    row_json               TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_id, turn_id)
);

-- Newest-first keyset paging + retention eviction ordering over the
-- immutable (sequence DESC, turn_id DESC) keys.
CREATE INDEX IF NOT EXISTS turn_rows_keyset
    ON turn_rows (tenant_id, user_id, session_id, sequence DESC, turn_id DESC);

-- Identity + effective agent + session/root-turn lookup.
CREATE INDEX IF NOT EXISTS turn_rows_by_agent
    ON turn_rows (tenant_id, user_id, session_id, effective_agent_id, turn_id);

CREATE TABLE IF NOT EXISTS turn_sessions (
    tenant_id  TEXT    NOT NULL,
    user_id    TEXT    NOT NULL,
    session_id TEXT    NOT NULL,
    next_seq   BIGINT  NOT NULL DEFAULT 0,
    checkpoint BIGINT  NOT NULL DEFAULT 0,
    snapshot   BIGINT  NOT NULL DEFAULT 0,
    truncated  BOOLEAN NOT NULL DEFAULT false,
    fenced     BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (tenant_id, user_id, session_id)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version)
VALUES (1)
ON CONFLICT DO NOTHING;
