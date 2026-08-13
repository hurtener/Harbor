// Package postgres is the V1 Postgres-backed implementation of the
// observability rollups.Store interface.
//
// It is the durable, multi-node production leg of the persistence triad —
// the third driver alongside the in-memory reference (memstore) and the
// SQLite driver. All three share the SAME Store interface and the SAME
// conformancetest suite; `internal/observability/rollups/conformancetest.Run`
// is the gate, so this driver ships zero new conformance scenarios.
//
// Storage model (the shape documented in rollups/store.go):
//
//   - rollup_rows — one row per (bucket_start, tenant_id, user_id,
//     session_id, model); bucket_start is on the fixed-UTC MINUTE grid and
//     every measure is exact BIGINT (cost in integer micro-units of USD,
//     latency min/max as the per-row folds). The secondary indexes
//     (bucket_start, tenant), (bucket_start, tenant, user), and (tenant)
//     give the bounded window + dimension queries their indexed access path
//     — a Query resolves through WHERE bucket_start >= $1 AND bucket_start <
//     $2 [AND dim = ANY(...)] and coarsens the minute rows in SQL, never a
//     full-table scan.
//   - rollup_checkpoint — the single-row (id = 1) durable local sequence.
//     ApplyBatch advances it in the SAME transaction as the row deltas, and
//     every mutating operation (ApplyBatch, FenceSession, Rebuild) takes
//     `SELECT ... FOR UPDATE` on this row as its serialization point, so
//     concurrent applies coordinate on the stored sequence: a batch whose
//     checkpoint does not advance the stored sequence is an idempotent no-op,
//     and the whole batch is applied atomically or not at all. This is the
//     conditional sequence/version logic that makes replay safe — it makes
//     NO active-active exactly-once claim (rollups.go: the Store has a
//     single writer, the Projector, and no cross-runtime consistency is
//     promised).
//   - rollup_fence — the PERMANENT erasure fences. FenceSession deletes the
//     triple's rows AND records the fence; Rebuild clears rows and the
//     checkpoint but NEVER the fence, so an erased session cannot be
//     resurrected by a late event or by reprojection.
//
// All queries are parameterized (no string concatenation into SQL). The
// driver is safe for concurrent use by N goroutines against a single shared
// instance: writes serialize on the checkpoint row; reads are plain
// statements under Postgres MVCC snapshots.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// pgxDriverName is the database/sql driver name registered by the pgx
// stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Tuning lives in a future config knob, not
// here.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// Config configures the Postgres rollups driver. Only DSN is mandatory;
// every other field is a pool-tuning knob with a documented default.
type Config struct {
	// DSN is the Postgres connection string (pgx URL or key-value form).
	DSN string
}

// New constructs a Postgres-backed rollups.Store against cfg.DSN,
// applying the embedded forward-only migrations and probing the
// connection eagerly so a misconfigured DSN fails loudly at construction,
// not on the first write.
//
// Errors:
//   - empty cfg.DSN
//   - sql.Open / ping / migration-apply failure
//
// The driver is constructed directly (no registration): the production
// driver aggregator home is a wiring concern for the phase that wires the
// projector into the runtime (same posture as memstore).
func New(cfg Config) (rollups.Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("rollups/postgres: cfg.DSN is required")
	}
	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("rollups/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rollups/postgres: ping: %w", err)
	}
	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &driver{db: db}, nil
}

// driver is the Postgres-backed rollups.Store implementation.
//
// Fields are immutable after construction except for the atomic `closed`
// flag (compiled artifacts are immutable; per-run state lives in ctx).
type driver struct {
	db     *sql.DB
	closed atomic.Bool
}

// Compile-time assertion that driver satisfies rollups.Store.
var _ rollups.Store = (*driver)(nil)

// ApplyBatch implements rollups.Store.
//
// The batch's deltas and the checkpoint move are applied in ONE
// transaction, serialized on the checkpoint row (`SELECT ... FOR UPDATE`):
//
//  1. Idempotent no-op: a batch whose Checkpoint does not advance the
//     stored sequence is a no-op (replay safety — deltas + checkpoint are
//     atomic, so everything the batch covers was already applied). The
//     fence check is deliberately skipped for non-advancing batches,
//     matching the reference driver: a stale replay touching an erased
//     session is a no-op, not an error.
//  2. Fence check: a delta for a fenced (erased) triple rejects the WHOLE
//     batch with rollups.ErrSessionFenced — the checkpoint does not
//     advance, and the projector drops the offending event and retries.
//  3. Checked merge: every delta is accumulated into a working copy of its
//     row in exact integer arithmetic — a negative additive delta fails
//     loudly with rollups.ErrNegativeMeasure and an int64 overflow fails
//     loudly with rollups.ErrMeasureOverflow, BEFORE any write (a refused
//     batch never leaves partial rows, a shrunk counter, or a wrapped sum).
//  4. Write pass: the merged rows are upserted and the checkpoint is set to
//     batch.Checkpoint in the same transaction.
func (d *driver) ApplyBatch(ctx context.Context, batch rollups.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed.Load() {
		return rollups.ErrClosed
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return d.translateErr(err, "rollups/postgres: begin apply tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()

	// 1. The serialization point: every mutating operation takes the
	// checkpoint row lock, so concurrent applies (and fences / rebuilds)
	// coordinate on the stored sequence.
	stored, err := lockCheckpoint(ctx, tx)
	if err != nil {
		return d.translateErr(err, "rollups/postgres: lock checkpoint")
	}
	if batch.Checkpoint <= stored {
		if err := tx.Commit(); err != nil {
			return d.translateErr(err, "rollups/postgres: commit idempotent no-op")
		}
		committed = true
		return nil
	}

	// 2. Fence check — any delta for a fenced triple rejects the whole
	// batch and leaves the checkpoint untouched.
	fenced, err := batchFencedTriple(ctx, tx, batch.Deltas)
	if err != nil {
		return d.translateErr(err, "rollups/postgres: fence check")
	}
	if fenced != nil {
		return fmt.Errorf("%w: triple (tenant=%q user=%q session=%q) is erased",
			rollups.ErrSessionFenced, fenced.TenantID, fenced.UserID, fenced.SessionID)
	}

	// 3 + 4. Checked merge + write in one transaction.
	merged, err := mergeBatchRows(ctx, tx, batch.Deltas)
	if err != nil {
		return fmt.Errorf("rollups/postgres: ApplyBatch checkpoint=%d: %w", batch.Checkpoint, err)
	}
	if len(merged) > 0 {
		if err := upsertRows(ctx, tx, merged); err != nil {
			return d.translateErr(err, "rollups/postgres: upsert rows")
		}
	}
	if err := setCheckpoint(ctx, tx, batch.Checkpoint); err != nil {
		return d.translateErr(err, "rollups/postgres: advance checkpoint")
	}
	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "rollups/postgres: commit apply")
	}
	committed = true
	return nil
}

// Query implements rollups.Store. The query is re-validated (the wrapped
// ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels flow through),
// then resolved as a single bounded SQL statement: the mandatory window
// predicate (WHERE bucket_start >= $1 AND bucket_start < $2) plus the
// closed-dimension filters drive the index, the minute rows are coarsened
// to the query's Bucket in SQL (fixed-UTC date_trunc), the requested
// measures are aggregated as exact BIGINT sums / folds, and the page is
// returned in the query's total order with deterministic keyset
// pagination. Measure values are exact integers — never float64.
func (d *driver) Query(ctx context.Context, q rollups.Query) (rollups.Result, error) {
	if err := q.Validate(); err != nil {
		return rollups.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if d.closed.Load() {
		return rollups.Result{}, rollups.ErrClosed
	}
	spec, err := buildQuery(q)
	if err != nil {
		return rollups.Result{}, err
	}
	rows, err := d.db.QueryContext(ctx, spec.sql, spec.args...)
	if err != nil {
		return rollups.Result{}, d.translateErr(err, "rollups/postgres: query")
	}
	defer func() { _ = rows.Close() }()

	out := make([]rollups.Row, 0, q.Limit+1)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		var bk time.Time
		scanArgs := []any{&bk}
		groupVals := make([]string, len(spec.groupCols))
		for i := range spec.groupCols {
			scanArgs = append(scanArgs, &groupVals[i])
		}
		measureVals := make([]int64, len(spec.measures))
		for i := range spec.measures {
			scanArgs = append(scanArgs, &measureVals[i])
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return rollups.Result{}, d.translateErr(err, "rollups/postgres: scan row")
		}
		dimensions := make(rollups.DimensionValues, len(spec.groupCols))
		for i, c := range spec.groupCols {
			dimensions[dimOfColumn(c)] = groupVals[i]
		}
		measures := make(map[rollups.Measure]rollups.MeasureValue, len(spec.measures))
		for i, m := range spec.measures {
			measures[m] = rollups.MeasureValue{N: measureVals[i], Scale: measureScale(m)}
		}
		out = append(out, rollups.Row{
			BucketStart: bk.UTC(),
			Dimensions:  dimensions,
			Measures:    measures,
		})
		if len(out) > q.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return rollups.Result{}, d.translateErr(err, "rollups/postgres: iterate rows")
	}

	if len(out) <= q.Limit {
		return rollups.Result{Rows: out}, nil
	}
	last := out[q.Limit-1]
	next := rollups.PageCursor{
		ShapeVersion: rollups.CursorShapeVersion,
		Fingerprint:  rollups.QueryShapeFingerprint(q),
		BucketNano:   last.BucketStart.UnixNano(),
		Group:        last.Dimensions,
	}
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = rollups.SortKeyBucketAsc
	}
	if sortKey == rollups.SortKeyMeasureAsc || sortKey == rollups.SortKeyMeasureDesc {
		next.MeasureVal = last.Measures[q.SortMeasure].N
	}
	cursorStr, err := rollups.EncodeCursor(next)
	if err != nil {
		return rollups.Result{}, err
	}
	return rollups.Result{Rows: out[:q.Limit], NextCursor: cursorStr}, nil
}

// FenceSession implements rollups.Store: it erases every row for the
// session triple and fences the triple PERMANENTLY so no future
// ApplyBatch can create rows for it, and no Rebuild can clear it. There
// is no unfence operation. Idempotent.
//
// FenceSession takes the same checkpoint-row serialization point as
// ApplyBatch, so it never interleaves with an in-flight apply: either the
// apply lands first (and the fence then erases it) or the fence lands
// first (and the apply's fence check refuses the batch) — a fenced
// triple's row can never be written after the fence.
func (d *driver) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed.Load() {
		return rollups.ErrClosed
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return d.translateErr(err, "rollups/postgres: begin fence tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()
	if _, err := lockCheckpoint(ctx, tx); err != nil {
		return d.translateErr(err, "rollups/postgres: lock checkpoint")
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rollup_rows
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID); err != nil {
		return d.translateErr(err, "rollups/postgres: erase rows")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rollup_fence (tenant_id, user_id, session_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`,
		id.TenantID, id.UserID, id.SessionID); err != nil {
		return d.translateErr(err, "rollups/postgres: insert fence")
	}
	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "rollups/postgres: commit fence")
	}
	committed = true
	return nil
}

// IsFenced implements rollups.Store.
func (d *driver) IsFenced(ctx context.Context, id identity.Identity) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if d.closed.Load() {
		return false, rollups.ErrClosed
	}
	var f bool
	err := d.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM rollup_fence
			WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
		)`, id.TenantID, id.UserID, id.SessionID).Scan(&f)
	if err != nil {
		return false, d.translateErr(err, "rollups/postgres: is fenced")
	}
	return f, nil
}

// Checkpoint implements rollups.Store. Returns 0 when the checkpoint row
// has never been advanced (a defensive fallback for a schema that predates
// the migration seed).
func (d *driver) Checkpoint(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if d.closed.Load() {
		return 0, rollups.ErrClosed
	}
	var seq sql.NullInt64
	err := d.db.QueryRowContext(ctx,
		`SELECT sequence FROM rollup_checkpoint WHERE id = 1`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, d.translateErr(err, "rollups/postgres: checkpoint")
	}
	if !seq.Valid {
		return 0, nil
	}
	if seq.Int64 < 0 {
		return 0, fmt.Errorf("rollups/postgres: stored checkpoint %d is negative (corrupted)", seq.Int64)
	}
	return uint64(seq.Int64), nil
}

// Retention implements rollups.Store: the oldest and newest row-level
// (MINUTE-grid) bucket starts currently retained, or (zero, zero) when no
// rows exist.
func (d *driver) Retention(ctx context.Context) (time.Time, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if d.closed.Load() {
		return time.Time{}, time.Time{}, rollups.ErrClosed
	}
	var oldest, newest sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT MIN(bucket_start), MAX(bucket_start) FROM rollup_rows`).
		Scan(&oldest, &newest)
	if err != nil {
		return time.Time{}, time.Time{}, d.translateErr(err, "rollups/postgres: retention")
	}
	if !oldest.Valid || !newest.Valid {
		return time.Time{}, time.Time{}, nil
	}
	return oldest.Time.UTC(), newest.Time.UTC(), nil
}

// Rebuild implements rollups.Store: clears every row and resets the
// checkpoint to 0 so the projector reprocesses the full log. Erasure
// fences are PERMANENT and are deliberately NOT cleared — rebuilding
// projection rows or the checkpoint cannot authorize the resurrection of
// an erased session. Rebuild takes the same checkpoint-row serialization
// point as ApplyBatch, so it never interleaves with an in-flight apply.
func (d *driver) Rebuild(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed.Load() {
		return rollups.ErrClosed
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return d.translateErr(err, "rollups/postgres: begin rebuild tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()
	if _, err := lockCheckpoint(ctx, tx); err != nil {
		return d.translateErr(err, "rollups/postgres: lock checkpoint")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rollup_rows`); err != nil {
		return d.translateErr(err, "rollups/postgres: clear rows")
	}
	if err := setCheckpoint(ctx, tx, 0); err != nil {
		return d.translateErr(err, "rollups/postgres: reset checkpoint")
	}
	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "rollups/postgres: commit rebuild")
	}
	committed = true
	return nil
}

// Close implements rollups.Store. Idempotent — a second call is a no-op
// and returns nil. The atomic flag is set BEFORE db.Close() so concurrent
// in-flight calls fast-fail at the entry guard with rollups.ErrClosed
// instead of racing on a closed *sql.DB.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("rollups/postgres: db.Close: %w", err)
	}
	return nil
}

// --- serialization + transaction helpers ---------------------------------

// lockCheckpoint takes the single-writer serialization point (the
// checkpoint row, FOR UPDATE) and returns the stored sequence. A missing
// row reads as 0 (defensive: the migration seeds (1, 0)).
func lockCheckpoint(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var seq sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT sequence FROM rollup_checkpoint WHERE id = 1 FOR UPDATE`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	if seq.Int64 < 0 {
		return 0, fmt.Errorf("rollups/postgres: stored checkpoint %d is negative (corrupted)", seq.Int64)
	}
	return uint64(seq.Int64), nil
}

// setCheckpoint writes the checkpoint row (insert-or-update on the
// single-row id = 1 key). The sequence is a domain uint64; the column is
// BIGINT, so a value beyond the int64 range is refused loudly (a wrapped
// conversion would silently corrupt the durable watermark).
func setCheckpoint(ctx context.Context, tx *sql.Tx, sequence uint64) error {
	if sequence > math.MaxInt64 {
		return fmt.Errorf("rollups/postgres: checkpoint %d exceeds the BIGINT range", sequence)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rollup_checkpoint (id, sequence)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET sequence = EXCLUDED.sequence`, int64(sequence))
	return err
}

// batchFencedTriple returns the first fenced session triple among the
// batch's deltas, or nil when none is fenced. The check is a single
// parameterized query over the batch's distinct triples.
func batchFencedTriple(ctx context.Context, tx *sql.Tx, deltas []rollups.Delta) (*rollups.SessionTriple, error) {
	triples := distinctTriples(deltas)
	if len(triples) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`SELECT tenant_id, user_id, session_id FROM rollup_fence WHERE `)
	args := make([]any, 0, len(triples)*3)
	n := 1
	for i, t := range triples {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		fmt.Fprintf(&sb, "(tenant_id = $%d AND user_id = $%d AND session_id = $%d)", n, n+1, n+2)
		n += 3
		args = append(args, t.TenantID, t.UserID, t.SessionID)
	}
	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var t rollups.SessionTriple
		if err := rows.Scan(&t.TenantID, &t.UserID, &t.SessionID); err != nil {
			return nil, err
		}
		return &t, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// distinctTriples returns the batch's session triples with set semantics,
// in first-seen order.
func distinctTriples(deltas []rollups.Delta) []rollups.SessionTriple {
	seen := make(map[rollups.SessionTriple]struct{}, len(deltas))
	out := make([]rollups.SessionTriple, 0, len(deltas))
	for _, d := range deltas {
		t := rollups.SessionTriple{TenantID: d.Key.TenantID, UserID: d.Key.UserID, SessionID: d.Key.SessionID}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// translateErr maps low-level driver errors at the boundary. Once Close
// has set the atomic flag, callers hit the entry guard before reaching
// here; this helper covers the tiny race window where Close runs between
// the entry-guard check and the actual query.
func (d *driver) translateErr(err error, ctxMsg string) error {
	if err == nil {
		return nil
	}
	if d.closed.Load() {
		return rollups.ErrClosed
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("%s: %w", ctxMsg, rollups.ErrClosed)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
}
