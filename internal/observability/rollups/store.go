package rollups

import (
	"context"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrClosed — the Store has been Closed; every operation returns it.
	ErrClosed = errors.New("rollups: store closed")
	// ErrSessionFenced — a delta targets a session triple that has been
	// erased and fenced; the row cannot be created (or re-created).
	// ApplyBatch rejects the whole batch; the projector drops the event
	// and retries.
	ErrSessionFenced = errors.New("rollups: session is fenced (erased)")
	// ErrQueryInvalid — the Query failed structural/closed-set validation.
	ErrQueryInvalid = errors.New("rollups: invalid query")
	// ErrQueryBudget — the Query exceeds a result budget (MaxBuckets or
	// MaxRowsPerQuery). Fails loudly; never a truncated response.
	ErrQueryBudget = errors.New("rollups: query exceeds a result budget")
	// ErrBadCursor — the page cursor is malformed or was produced by a
	// different query shape. The caller must restart from the first page.
	ErrBadCursor = errors.New("rollups: invalid or incompatible page cursor")
)

// Batch is one atomic write unit: the row deltas derived from a contiguous
// run of consumed events plus the checkpoint they advance to. The Store
// applies the deltas AND moves the checkpoint to Batch.Checkpoint in ONE
// atomic step, which is what makes replay idempotent: a crash between
// applying deltas and checkpointing is impossible, and re-applying a batch
// whose checkpoint does not advance the stored checkpoint is a no-op.
type Batch struct {
	// Checkpoint is the sequence of the last event the batch covers (the
	// existing local durable sequence — the bus Sequence). Must be
	// strictly greater than the stored checkpoint or the batch is a
	// no-op.
	Checkpoint uint64
	// Deltas are the row updates derived from the batch's events.
	Deltas []Delta
}

// Store is the mandatory persistence surface of the rollups domain. It is
// deliberately driver-shaped so indexed implementations can back it — the
// shipped in-memory reference (memstore), plus SQLite and Postgres
// implementations sharing the interface and the conformancetest suite.
//
// Intended SQL shape for the row table (single table, one row per Key):
//
//	rollup_rows(
//	    bucket_start   TIMESTAMPTZ NOT NULL,   -- UTC, StoreGranularity grid
//	    tenant_id      TEXT NOT NULL,
//	    user_id        TEXT NOT NULL,
//	    session_id     TEXT NOT NULL,
//	    model          TEXT NOT NULL DEFAULT '',
//	    agent_id       TEXT NOT NULL DEFAULT '',
//	    llm_cost_usd          DOUBLE PRECISION NOT NULL DEFAULT 0,
//	    llm_tokens_prompt     BIGINT NOT NULL DEFAULT 0,
//	    llm_tokens_completion BIGINT NOT NULL DEFAULT 0,
//	    llm_tokens_total      BIGINT NOT NULL DEFAULT 0,
//	    llm_completions       BIGINT NOT NULL DEFAULT 0,
//	    llm_latency_ms        BIGINT NOT NULL DEFAULT 0,
//	    tasks_completed       BIGINT NOT NULL DEFAULT 0,
//	    tasks_failed          BIGINT NOT NULL DEFAULT 0,
//	    tasks_cancelled       BIGINT NOT NULL DEFAULT 0,
//	    PRIMARY KEY (bucket_start, tenant_id, user_id, session_id, model, agent_id)
//	)
//	CREATE INDEX idx_rollup_bucket_tenant ON rollup_rows (bucket_start, tenant_id);
//	CREATE INDEX idx_rollup_bucket_tenant_user ON rollup_rows (bucket_start, tenant_id, user_id);
//	CREATE INDEX idx_rollup_tenant ON rollup_rows (tenant_id);
//
// The checkpoint and fence state are single-row tables:
//
//	rollup_checkpoint(id INTEGER PRIMARY KEY CHECK (id = 1), sequence BIGINT NOT NULL);
//	rollup_fence(tenant_id, user_id, session_id, PRIMARY KEY (tenant_id, user_id, session_id));
//
// The interface contracts:
//
//   - ApplyBatch is atomic (all deltas + the checkpoint move in one
//     transaction). A batch whose Checkpoint does not advance the stored
//     checkpoint is a no-op — this is the replay-idempotency invariant.
//   - Query is a pure read; it must not mutate stored state and must be
//     safe for concurrent use.
//   - FenceSession erases the triple's rows AND fences it; a later
//     ApplyBatch that touches the triple fails with ErrSessionFenced
//     (the erasure is never resurrected by a late event). Rows for a
//     fenced triple are never returned by Query.
//   - Checkpoint / Retention / Rebuild are the projector's restart and
//     rebuild surfaces.
//
// A Store MUST be safe for concurrent use by N goroutines against a single
// shared instance (the concurrent-reuse contract; the conformance suite
// pins it).
type Store interface {
	// ApplyBatch atomically applies the batch's deltas and advances the
	// checkpoint to batch.Checkpoint. A batch whose Checkpoint does not
	// advance the stored checkpoint is a no-op (idempotent replay: because
	// deltas + checkpoint are atomic, every event at or below the stored
	// checkpoint is already applied, so a non-advancing batch has nothing
	// new to apply — this is what makes concurrent advances and restart
	// replays safe). A delta for a fenced triple rejects the WHOLE batch
	// with ErrSessionFenced — the checkpoint does not advance, and the
	// projector drops the offending event and retries.
	ApplyBatch(ctx context.Context, batch Batch) error

	// Query executes a validated rollup query. The query MUST pass
	// Validate (the store re-validates and returns the wrapped
	// ErrQueryInvalid / ErrQueryBudget sentinels). The response page is
	// deterministic for a stable store: same query + same cursor ⇒ same
	// rows, and pages never skip or repeat a row.
	Query(ctx context.Context, q Query) (Result, error)

	// FenceSession erases every row for the session triple and fences the
	// triple so no future ApplyBatch can create rows for it. Idempotent.
	// This is the rollups side of the session-erasure cascade; the
	// runtime calls it when a session is erased, and the projector drops
	// late events for fenced triples at ingestion time.
	FenceSession(ctx context.Context, id identity.Identity) error

	// UnfenceSession lifts a fence (a reused session id opened afresh).
	// Idempotent.
	UnfenceSession(ctx context.Context, id identity.Identity) error

	// IsFenced reports whether the session triple is fenced (erased).
	IsFenced(ctx context.Context, id identity.Identity) (bool, error)

	// Checkpoint returns the last applied sequence (0 = nothing applied).
	Checkpoint(ctx context.Context) (uint64, error)

	// Retention returns the oldest and newest bucket start currently
	// retained, or (zero, zero) when no rows exist. The two instants are
	// the row-level (StoreGranularity) boundaries; a query coarsens them.
	Retention(ctx context.Context) (oldest, newest time.Time, err error)

	// Rebuild clears every row, every fence, and the checkpoint (reset to
	// 0) so the projector reprocesses the full log from the beginning.
	// The projector's restart catch-up and rebuild paths both rest on it.
	Rebuild(ctx context.Context) error

	// Close releases the store's resources. Idempotent; later calls
	// return ErrClosed.
	Close(ctx context.Context) error
}
