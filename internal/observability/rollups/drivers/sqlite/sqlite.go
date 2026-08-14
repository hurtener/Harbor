// Package sqlite is the SQLite-backed implementation of the
// observability-rollup `rollups.Store` interface — the durable,
// indexed sibling of the in-memory reference driver, built on
// `modernc.org/sqlite` (CGo-free; builds stay `CGO_ENABLED=0`).
//
// # Durable indexed parity
//
// The schema mirrors the reference driver's indexes 1:1 against the
// fixed-UTC MINUTE grid:
//
//   - `rollup_rows` carries one row per row key (minute bucket start +
//     exactly the closed dimensions tenant / user / session / model).
//     The bucket start is an exact INTEGER of unix nanoseconds, every
//     measure column is INTEGER (exact int64), and there is NO REAL or
//     DOUBLE column anywhere — cost is stored as integer micro-units of
//     USD and nothing is ever accumulated or stored as float64.
//   - The secondary indexes serve the bounded read paths (bucket+tenant,
//     bucket+tenant+user), the erasure-fence delete (the full identity
//     triple), and one axis index per remaining closed dimension. A
//     Query resolves its candidates through these indexes — the bounded
//     window range plus exact IN filters per axis — and never scans the
//     canonical event log (this driver holds no reference to it; the
//     projection rows ARE the rollup store).
//   - `rollup_checkpoint` is the single-row durable watermark (the last
//     applied local durable sequence); `rollup_fence` is the PERMANENT
//     erasure fence. Both are plain tables, so they survive restarts.
//
// # Apply semantics
//
// ApplyBatch is ONE transaction: the deltas and the checkpoint move
// atomically, so a crash between applying deltas and checkpointing is
// impossible and re-applying a batch whose checkpoint does not advance
// the stored checkpoint is a no-op (idempotent replay). Every delta's
// merge is checked in Go against the exact int64 measure bounds BEFORE
// any row is written (a working copy per key, verified via
// `MeasureSet.Add`) — an overflowing or negative delta refuses the WHOLE
// batch and applies nothing. A delta for a fenced (erased) triple
// refuses the WHOLE batch with `rollups.ErrSessionFenced`.
//
// # Erasure fences are permanent
//
// FenceSession deletes the triple's rows AND writes the fence row in one
// transaction; Rebuild deletes rows + checkpoint only — the fence table
// is never touched — so reprojection can never resurrect an erased
// session, and a late event for a fenced triple is refused forever.
//
// # Quality persistence
//
// The durable components of the projector's quality surface live here:
// the watermark is `rollup_checkpoint.sequence`, and the retention
// horizon is the MIN/MAX `bucket_start` of `rollup_rows` (the row-level
// minute grid). The projector's live catch-up state
// (`current` / `catching_up` / `unavailable`) is projector-instance
// state — this driver persists the durable truth the projector re-derives
// it from, so a restart resumes honest quality from the durable
// watermark + source head on the first Advance.
//
// # Operating model
//
//   - `New` accepts a bare file path or a `file:` URI. The special
//     `:memory:` sentinel maps to a per-open uniquely named shared-cache
//     memory database (each store is isolated; the pool shares one DB).
//   - `journal_mode=WAL` and `busy_timeout=5000` are pinned on every
//     connection via DSN pragmas; transactions begin IMMEDIATE. The pool
//     is pinned to a single connection so all access serialises at the
//     Go layer — the driver matches SQLite's single-writer reality and
//     never surfaces SQLITE_BUSY contention.
//   - The schema is applied from embedded `migrations/*.sql` via the
//     shared `internal/persistence/sqlmigrate` runner.
//
// The driver is constructed directly (no registry registration): the
// production driver-aggregator wiring is a runtime-assembly concern, not
// this package's.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	// modernc.org/sqlite registers the "sqlite" driver name with
	// database/sql via its own init(). Blank-importing it here is the
	// idiomatic way to make `sql.Open("sqlite", dsn)` work.
	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// driverName is the name registered with database/sql by
// `modernc.org/sqlite`.
const driverName = "sqlite"

// busyTimeoutMs is the PRAGMA busy_timeout value pinned at open. It
// absorbs transient write-lock contention transparently.
const busyTimeoutMs = 5000

// selectColumns is the fixed column list of a rollup row (the key
// columns + every measure column). Every measure column is exact
// INTEGER; the driver scans them into a rollups.MeasureSet and never
// converts a measure through float64.
const selectColumns = `bucket_start, tenant_id, user_id, session_id, model,
	llm_completions, llm_tokens_prompt, llm_tokens_completion, llm_tokens_reasoning,
	llm_tokens_cache_read, llm_tokens_cache_write, llm_tokens_total, llm_cost_micros,
	llm_latency_count, llm_latency_sum_ms, llm_latency_min_ms, llm_latency_max_ms,
	tasks_completed, tasks_failed, tasks_cancelled`

// Store is the SQLite-backed rollups.Store. It is a compiled artifact:
// the *sql.DB (internally synchronised by database/sql) and the atomic
// close flag are the only mutable state, so N goroutines can share one
// instance. The pool is pinned to a single connection, serialising all
// access at the Go layer (SQLite's single-writer reality).
type Store struct {
	db     *sql.DB
	closed atomic.Bool
}

// Compile-time assertion that Store satisfies rollups.Store.
var _ rollups.Store = (*Store)(nil)

// New constructs a fresh, empty SQLite-backed rollups.Store against dsn.
//
// DSN handling:
//
//   - Empty DSN → clear error (no silent default-fallback).
//   - `:memory:` → translated to a PER-OPEN uniquely named shared-cache
//     memory URI (`file:harbor_rollups_mem_<entropy>?mode=memory&cache=shared`)
//     so the pool's single connection sees one in-memory database while
//     two `:memory:` stores opened by different callers stay isolated.
//   - Any other DSN is treated as a file path or URI form and passed
//     through verbatim, with `journal_mode=WAL`, `busy_timeout=5000`,
//     and `_txlock=immediate` appended as query parameters so
//     modernc.org/sqlite applies them on every pooled connection.
//
// Construction fails loudly on an empty DSN, an unparseable URI, a
// non-WAL journal mode (disk-backed), or a migration-apply failure.
func New(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New(`rollups/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	augmented, err := augmentDSNForPragmas(dsn)
	if err != nil {
		return nil, fmt.Errorf("rollups/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, augmented)
	if err != nil {
		return nil, fmt.Errorf("rollups/sqlite: sql.Open(%q): %w", dsn, err)
	}

	// SQLite is a single-writer engine. Pinning the pool to one
	// connection serialises all access at the Go layer — the driver thus
	// matches the engine's single-writer reality and never surfaces
	// SQLITE_BUSY under the conformance suite's concurrent reads or the
	// projector's single-writer applies.
	db.SetMaxOpenConns(1)

	// Use a bounded context for the open-time validation + migrations so
	// a wedged file does not hang construction forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := verifyJournalMode(ctx, db, dsn); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rollups/sqlite: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them to
// every connection the pool opens: `busy_timeout(5000)` absorbs
// SQLITE_BUSY retries, `journal_mode(WAL)` gives concurrent readers +
// a single writer (memory DBs degrade to `memory` mode by SQLite
// design), and `_txlock=immediate` makes every transaction acquire the
// write lock up-front so two transactions can never deadlock upgrading
// from read to write.
//
// The bare `:memory:` DSN is translated to a per-open uniquely named
// `file:`-form shared-cache memory URI so the pool shares one DB within
// this store while two stores never collide.
func augmentDSNForPragmas(dsn string) (string, error) {
	if dsn == ":memory:" {
		unique, err := uniqueMemoryDSN()
		if err != nil {
			return "", err
		}
		dsn = unique
	}

	pragmas := []string{
		"busy_timeout(" + fmt.Sprint(busyTimeoutMs) + ")",
		"journal_mode(WAL)",
	}

	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file: URI: %w", err)
		}
		q := u.Query()
		for _, p := range pragmas {
			q.Add("_pragma", p)
		}
		if err := validateTxlock(q["_txlock"]); err != nil {
			return "", err
		}
		if len(q["_txlock"]) == 0 {
			q.Set("_txlock", "immediate")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	base, rawQuery, hasQuery := strings.Cut(dsn, "?")
	q := make(url.Values)
	if hasQuery {
		var err error
		q, err = url.ParseQuery(rawQuery)
		if err != nil {
			return "", fmt.Errorf("parse path query: %w", err)
		}
	}
	if err := validateTxlock(q["_txlock"]); err != nil {
		return "", err
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if len(q["_txlock"]) == 0 {
		q.Set("_txlock", "immediate")
	}
	return base + "?" + q.Encode(), nil
}

// validateTxlock preserves only transaction modes that acquire a write
// lock before a predicate is read. Multiple values are refused because
// driver-specific precedence would make the write guarantee vague.
func validateTxlock(values []string) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return errors.New("multiple _txlock values are unsafe; expected immediate or exclusive")
	}
	switch strings.ToLower(values[0]) {
	case "immediate", "exclusive":
		return nil
	default:
		return fmt.Errorf("_txlock=%q is unsafe; expected immediate or exclusive", values[0])
	}
}

// uniqueMemoryDSN mints a per-open named in-memory database URI.
// `mode=memory` keeps it off disk; `cache=shared` lets the pool's
// connection see the database; the crypto-random name keeps two
// `:memory:` stores fully isolated within one process. The database
// lives as long as the pool holds a connection to it; the driver pins
// the pool to a single long-lived connection, so the store's lifetime
// bounds the data's.
func uniqueMemoryDSN() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("rollups/sqlite: memory-DSN entropy: %w", err)
	}
	return "file:harbor_rollups_mem_" + hex.EncodeToString(entropy[:]) + "?mode=memory&cache=shared", nil
}

// verifyJournalMode reads back the journal mode after open to confirm
// the per-connection PRAGMA actually took effect. Disk-backed DSNs MUST
// report `wal`; memory DSNs degrade to `memory` mode by design.
func verifyJournalMode(ctx context.Context, db *sql.DB, originalDSN string) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("rollups/sqlite: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("rollups/sqlite: journal_mode=%q after open; expected \"wal\" (DSN=%q)", mode, originalDSN)
	}
	return nil
}

// isMemoryDSN reports whether the caller-supplied DSN routes to an
// in-memory database.
func isMemoryDSN(dsn string) bool {
	if dsn == ":memory:" {
		return true
	}
	return strings.HasPrefix(dsn, "file:") && strings.Contains(dsn, ":memory:")
}

// ApplyBatch implements rollups.Store. The batch's deltas and the
// checkpoint move are atomic (one transaction): a crash between applying
// deltas and checkpointing is impossible, and a batch whose Checkpoint
// does not advance the stored checkpoint is a no-op (idempotent replay —
// every event at or below the stored checkpoint is already applied). A
// delta for a fenced triple rejects the WHOLE batch with
// rollups.ErrSessionFenced (the checkpoint does not advance). Every
// delta's merge is verified against the exact int64 measure bounds on a
// working copy BEFORE any row is written, so a refused batch never
// leaves partial rows and the checkpoint does not advance.
func (s *Store) ApplyBatch(ctx context.Context, batch rollups.Batch) error {
	if s.closed.Load() {
		return rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollups/sqlite: begin apply tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the caller receives the original error
		}
	}()

	stored, err := checkpointInTx(ctx, tx)
	if err != nil {
		return err
	}
	if batch.Checkpoint <= stored {
		// Idempotent replay: the batch covers nothing newer than the
		// stored checkpoint, and deltas + checkpoint are atomic, so every
		// event it covers was already applied. Commit the empty
		// transaction so the pool connection is released.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("rollups/sqlite: commit idempotent no-op: %w", err)
		}
		committed = true
		return nil
	}

	// Erasure-fence gate: every delta's triple must be unfenced or the
	// WHOLE batch is refused with rollups.ErrSessionFenced. The fence
	// table is tiny (one row per erased session), so reading it once per
	// batch into a set is cheaper and clearer than per-delta EXISTS
	// probes.
	fences, err := fencesInTx(ctx, tx)
	if err != nil {
		return err
	}
	for _, del := range batch.Deltas {
		if _, fenced := fences[tripleOfKey(del.Key)]; fenced {
			return rollups.ErrSessionFenced
		}
	}

	// Checked-accumulation pre-pass: fold the batch's deltas into a
	// working copy per key (reading the current stored row once) and
	// verify every measure fits the exact int64 range BEFORE any write.
	// A batch whose accumulation would overflow or carry a negative
	// delta fails loudly with rollups.ErrMeasureOverflow /
	// rollups.ErrNegativeMeasure and applies NOTHING.
	pending := make(map[rollups.Key]rollups.MeasureSet, len(batch.Deltas))
	for _, del := range batch.Deltas {
		merged, ok := pending[del.Key]
		if !ok {
			merged, err = rowInTx(ctx, tx, del.Key)
			if err != nil {
				return err
			}
		}
		merged, err = mergeMeasureSet(merged, del.Add)
		if err != nil {
			return fmt.Errorf("rollups/sqlite: ApplyBatch checkpoint=%d: %w", batch.Checkpoint, err)
		}
		pending[del.Key] = merged
	}

	// Write the merged rows and the checkpoint in the SAME transaction.
	for k, v := range pending {
		if err := upsertRowInTx(ctx, tx, k, v); err != nil {
			return err
		}
	}
	if err := setCheckpointInTx(ctx, tx, batch.Checkpoint); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollups/sqlite: commit apply: %w", err)
	}
	committed = true
	return nil
}

// Query implements rollups.Store. The query is re-validated (the wrapped
// ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels flow
// through), and the candidate rows are resolved through the bucket +
// dimension indexes — the bounded bucket_start window plus exact IN
// filters per closed axis — never a full-table scan of the projection
// rows and never the canonical event log (this driver holds no reference
// to it). Grouping (minute rows coarsened to the query's Bucket), the
// checked measure aggregation, the total sort, and the deterministic
// keyset pagination run in Go after the indexed candidate read. The
// response is deterministic for a stable store: same query + same cursor
// ⇒ same rows, and pages never skip or repeat a row. A group whose
// measure sums would overflow fails loudly with
// rollups.ErrMeasureOverflow.
func (s *Store) Query(ctx context.Context, q rollups.Query) (rollups.Result, error) {
	if err := q.Validate(); err != nil {
		return rollups.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if s.closed.Load() {
		return rollups.Result{}, rollups.ErrClosed
	}

	stmt, args := buildSelectQuery(q)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return rollups.Result{}, fmt.Errorf("rollups/sqlite: query candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Fenced rows cannot be candidates by construction: FenceSession
	// deletes them and ApplyBatch refuses fenced deltas, so no fenced
	// row ever lives in rollup_rows.
	candidates := make(map[rollups.Key]rollups.MeasureSet)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		var k rollups.Key
		var v rollups.MeasureSet
		var bucketNano int64
		if err := rows.Scan(&bucketNano, &k.TenantID, &k.UserID, &k.SessionID, &k.Model,
			&v.LLMCompletions, &v.LLMTokensPrompt, &v.LLMTokensCompletion, &v.LLMTokensReasoning,
			&v.LLMTokensCacheRead, &v.LLMTokensCacheWrite, &v.LLMTokensTotal, &v.LLMCostMicros,
			&v.LLMLatencyCount, &v.LLMLatencySumMS, &v.LLMLatencyMinMS, &v.LLMLatencyMaxMS,
			&v.TasksCompleted, &v.TasksFailed, &v.TasksCancelled); err != nil {
			return rollups.Result{}, fmt.Errorf("rollups/sqlite: scan candidate row: %w", err)
		}
		k.BucketStart = time.Unix(0, bucketNano).UTC()
		candidates[k] = v
	}
	if err := rows.Err(); err != nil {
		return rollups.Result{}, fmt.Errorf("rollups/sqlite: iterate candidates: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if len(candidates) == 0 {
		return rollups.Result{}, nil
	}
	return aggregate(ctx, q, candidates)
}

// buildSelectQuery renders the indexed candidate read for q: the bounded
// bucket_start window plus one exact IN clause per closed filter axis.
// The column names are compile-time constants; every filter value is a
// bound parameter — never interpolated SQL.
func buildSelectQuery(q rollups.Query) (string, []any) {
	stmt := "SELECT " + selectColumns +
		" FROM rollup_rows WHERE bucket_start >= ? AND bucket_start < ?"
	args := []any{q.From.UnixNano(), q.To.UnixNano()}

	axes := []struct {
		column string
		values []string
	}{
		{"tenant_id", distinctStrings(q.Filter.TenantIDs)},
		{"user_id", distinctStrings(q.Filter.UserIDs)},
		{"session_id", distinctStrings(q.Filter.SessionIDs)},
		{"model", distinctStrings(q.Filter.Models)},
	}
	for _, ax := range axes {
		if len(ax.values) == 0 {
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ax.values)), ",")
		stmt += " AND " + ax.column + " IN (" + placeholders + ")"
		for _, v := range ax.values {
			args = append(args, v)
		}
	}
	return stmt, args
}

// FenceSession implements rollups.Store: it erases every row for the
// session triple and fences the triple PERMANENTLY so no future
// ApplyBatch can create rows for it (the erasure is never resurrected by
// a late event or by Rebuild). Both the delete and the fence insert
// happen in ONE transaction. Idempotent. There is no unfence operation.
func (s *Store) FenceSession(ctx context.Context, id identity.Identity) error {
	if s.closed.Load() {
		return rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollups/sqlite: begin fence tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the caller receives the original error
		}
	}()
	// The triple index (tenant_id, user_id, session_id) resolves the
	// delete as an index range, not a table scan.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rollup_rows WHERE tenant_id = ? AND user_id = ? AND session_id = ?`,
		id.TenantID, id.UserID, id.SessionID); err != nil {
		return fmt.Errorf("rollups/sqlite: fence delete rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO rollup_fence (tenant_id, user_id, session_id) VALUES (?, ?, ?)`,
		id.TenantID, id.UserID, id.SessionID); err != nil {
		return fmt.Errorf("rollups/sqlite: fence insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollups/sqlite: commit fence: %w", err)
	}
	committed = true
	return nil
}

// IsFenced implements rollups.Store.
func (s *Store) IsFenced(ctx context.Context, id identity.Identity) (bool, error) {
	if s.closed.Load() {
		return false, rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM rollup_fence WHERE tenant_id = ? AND user_id = ? AND session_id = ?`,
		id.TenantID, id.UserID, id.SessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rollups/sqlite: is fenced: %w", err)
	}
	return true, nil
}

// Checkpoint implements rollups.Store: the durable watermark (the last
// applied local durable sequence), 0 when nothing has been applied.
func (s *Store) Checkpoint(ctx context.Context) (uint64, error) {
	if s.closed.Load() {
		return 0, rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var seq uint64
	err := s.db.QueryRowContext(ctx,
		`SELECT sequence FROM rollup_checkpoint WHERE id = 1`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rollups/sqlite: checkpoint: %w", err)
	}
	return seq, nil
}

// Retention implements rollups.Store: the oldest and newest retained
// bucket start (the row-level minute grid), or (zero, zero) when no rows
// exist. The MIN/MAX scan touches only the bucket_start index.
func (s *Store) Retention(ctx context.Context) (time.Time, time.Time, error) {
	if s.closed.Load() {
		return time.Time{}, time.Time{}, rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	var minN, maxN sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(bucket_start), MAX(bucket_start) FROM rollup_rows`).Scan(&minN, &maxN); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("rollups/sqlite: retention: %w", err)
	}
	if !minN.Valid || !maxN.Valid {
		return time.Time{}, time.Time{}, nil
	}
	return time.Unix(0, minN.Int64).UTC(), time.Unix(0, maxN.Int64).UTC(), nil
}

// Rebuild implements rollups.Store: clears every projection row and the
// checkpoint (reset to 0) so the projector reprocesses the full log from
// the beginning. Erasure fences are PERMANENT and are deliberately NOT
// cleared — rebuilding rows or the checkpoint cannot authorize the
// resurrection of an erased session.
func (s *Store) Rebuild(ctx context.Context) error {
	if s.closed.Load() {
		return rollups.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollups/sqlite: begin rebuild tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the caller receives the original error
		}
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rollup_rows`); err != nil {
		return fmt.Errorf("rollups/sqlite: rebuild delete rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rollup_checkpoint`); err != nil {
		return fmt.Errorf("rollups/sqlite: rebuild reset checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollups/sqlite: commit rebuild: %w", err)
	}
	committed = true
	return nil
}

// Close implements rollups.Store. Setting the atomic flag BEFORE
// `db.Close()` ensures concurrent in-flight callers observe
// `rollups.ErrClosed` rather than racing into a half-closed pool. Close
// is idempotent — repeat calls are safe and return nil.
func (s *Store) Close(_ context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("rollups/sqlite: close: %w", err)
	}
	return nil
}

// --- transaction-local helpers -------------------------------------------

// checkpointInTx reads the stored checkpoint inside tx (0 when the
// single-row table is empty).
func checkpointInTx(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var seq uint64
	err := tx.QueryRowContext(ctx,
		`SELECT sequence FROM rollup_checkpoint WHERE id = 1`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rollups/sqlite: checkpoint read: %w", err)
	}
	return seq, nil
}

// setCheckpointInTx upserts the single-row checkpoint inside tx.
func setCheckpointInTx(ctx context.Context, tx *sql.Tx, seq uint64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rollup_checkpoint (id, sequence) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET sequence = excluded.sequence`, seq); err != nil {
		return fmt.Errorf("rollups/sqlite: checkpoint write: %w", err)
	}
	return nil
}

// fencesInTx loads every erasure-fence triple inside tx into a set.
func fencesInTx(ctx context.Context, tx *sql.Tx) (map[rollups.SessionTriple]struct{}, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT tenant_id, user_id, session_id FROM rollup_fence`)
	if err != nil {
		return nil, fmt.Errorf("rollups/sqlite: load fences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[rollups.SessionTriple]struct{})
	for rows.Next() {
		var t rollups.SessionTriple
		if err := rows.Scan(&t.TenantID, &t.UserID, &t.SessionID); err != nil {
			return nil, fmt.Errorf("rollups/sqlite: scan fence: %w", err)
		}
		out[t] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollups/sqlite: iterate fences: %w", err)
	}
	return out, nil
}

// tripleOfKey extracts the comparable session triple of a row key.
func tripleOfKey(k rollups.Key) rollups.SessionTriple {
	return rollups.SessionTriple{TenantID: k.TenantID, UserID: k.UserID, SessionID: k.SessionID}
}

// rowInTx reads the stored row for key inside tx, or the zero MeasureSet
// when the row does not exist yet. The latency fold identity is
// re-derived from the stored count.
func rowInTx(ctx context.Context, tx *sql.Tx, k rollups.Key) (rollups.MeasureSet, error) {
	var v rollups.MeasureSet
	var bucketNano int64
	err := tx.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM rollup_rows
		 WHERE bucket_start = ? AND tenant_id = ? AND user_id = ? AND session_id = ? AND model = ?`,
		k.BucketStart.UnixNano(), k.TenantID, k.UserID, k.SessionID, k.Model).Scan(
		&bucketNano, &k.TenantID, &k.UserID, &k.SessionID, &k.Model,
		&v.LLMCompletions, &v.LLMTokensPrompt, &v.LLMTokensCompletion, &v.LLMTokensReasoning,
		&v.LLMTokensCacheRead, &v.LLMTokensCacheWrite, &v.LLMTokensTotal, &v.LLMCostMicros,
		&v.LLMLatencyCount, &v.LLMLatencySumMS, &v.LLMLatencyMinMS, &v.LLMLatencyMaxMS,
		&v.TasksCompleted, &v.TasksFailed, &v.TasksCancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return rollups.MeasureSet{}, nil
	}
	if err != nil {
		return rollups.MeasureSet{}, fmt.Errorf("rollups/sqlite: read row: %w", err)
	}
	return v, nil
}

// upsertRowInTx writes the merged row for key inside tx. The value is
// the already-checked accumulation of the stored row and the batch's
// deltas, so the write is a plain upsert of exact integers.
func upsertRowInTx(ctx context.Context, tx *sql.Tx, k rollups.Key, v rollups.MeasureSet) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO rollup_rows (
			bucket_start, tenant_id, user_id, session_id, model,
			llm_completions, llm_tokens_prompt, llm_tokens_completion, llm_tokens_reasoning,
			llm_tokens_cache_read, llm_tokens_cache_write, llm_tokens_total, llm_cost_micros,
			llm_latency_count, llm_latency_sum_ms, llm_latency_min_ms, llm_latency_max_ms,
			tasks_completed, tasks_failed, tasks_cancelled
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (bucket_start, tenant_id, user_id, session_id, model) DO UPDATE SET
			llm_completions = excluded.llm_completions,
			llm_tokens_prompt = excluded.llm_tokens_prompt,
			llm_tokens_completion = excluded.llm_tokens_completion,
			llm_tokens_reasoning = excluded.llm_tokens_reasoning,
			llm_tokens_cache_read = excluded.llm_tokens_cache_read,
			llm_tokens_cache_write = excluded.llm_tokens_cache_write,
			llm_tokens_total = excluded.llm_tokens_total,
			llm_cost_micros = excluded.llm_cost_micros,
			llm_latency_count = excluded.llm_latency_count,
			llm_latency_sum_ms = excluded.llm_latency_sum_ms,
			llm_latency_min_ms = excluded.llm_latency_min_ms,
			llm_latency_max_ms = excluded.llm_latency_max_ms,
			tasks_completed = excluded.tasks_completed,
			tasks_failed = excluded.tasks_failed,
			tasks_cancelled = excluded.tasks_cancelled`,
		k.BucketStart.UnixNano(), k.TenantID, k.UserID, k.SessionID, k.Model,
		v.LLMCompletions, v.LLMTokensPrompt, v.LLMTokensCompletion, v.LLMTokensReasoning,
		v.LLMTokensCacheRead, v.LLMTokensCacheWrite, v.LLMTokensTotal, v.LLMCostMicros,
		v.LLMLatencyCount, v.LLMLatencySumMS, v.LLMLatencyMinMS, v.LLMLatencyMaxMS,
		v.TasksCompleted, v.TasksFailed, v.TasksCancelled)
	if err != nil {
		return fmt.Errorf("rollups/sqlite: upsert row: %w", err)
	}
	return nil
}

// --- query aggregation ----------------------------------------------------

// aggregate groups, sorts, and pages the filtered candidate rows. Runs
// outside the store's database access (the candidates are already
// materialised). The group accumulation is the same checked
// accumulation as writes: a group whose sum would overflow fails loudly
// with rollups.ErrMeasureOverflow.
func aggregate(ctx context.Context, q rollups.Query, rows map[rollups.Key]rollups.MeasureSet) (rollups.Result, error) {
	// groupKey is the comparable grouping key: the coarsened bucket start
	// plus one fixed slot per AllDimensions member. Slots beyond the
	// query's GroupBy set are unused (the empty string), so distinct
	// groups never collide.
	type groupKey struct {
		bucketNano int64
		dims       [len(rollups.AllDimensions)]string
	}
	type group struct {
		bucketStart time.Time
		values      rollups.DimensionValues
		sum         rollups.MeasureSet
	}

	groups := make(map[groupKey]*group)
	for k, v := range rows {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		b := rollups.BucketStart(k.BucketStart, q.Bucket)
		var gk groupKey
		gk.bucketNano = b.UnixNano()
		values := make(rollups.DimensionValues, len(q.GroupBy))
		for _, d := range q.GroupBy {
			gk.dims[dimensionSlot(d)] = k.DimensionValue(d)
			values[d] = gk.dims[dimensionSlot(d)]
		}
		g, ok := groups[gk]
		if !ok {
			g = &group{bucketStart: b, values: values}
			groups[gk] = g
		}
		// Group aggregation is the same checked accumulation as writes
		// (the latency min/max fold driven by the stored count): a group
		// whose sum would overflow fails loudly with
		// rollups.ErrMeasureOverflow instead of returning a wrapped or
		// clamped total.
		merged, mergeErr := mergeMeasureSet(g.sum, v)
		if mergeErr != nil {
			return rollups.Result{}, fmt.Errorf("rollups/sqlite: aggregate: %w", mergeErr)
		}
		g.sum = merged
	}

	if len(groups) == 0 {
		return rollups.Result{}, nil
	}

	out := make([]rollups.Row, 0, len(groups))
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		measures := make(map[rollups.Measure]rollups.MeasureValue, len(q.Measures))
		for _, m := range q.Measures {
			measures[m] = g.sum.Get(m)
		}
		out = append(out, rollups.Row{
			BucketStart: g.bucketStart,
			Dimensions:  g.values,
			Measures:    measures,
		})
	}

	sort.Slice(out, func(i, j int) bool { return rowLess(out[i], out[j], q) })

	// Keyset pagination: skip everything up to and including the cursor
	// position, then emit at most Limit rows; a Limit+1-th row means
	// there is a next page. The cursor's shape binding (version +
	// fingerprint) was verified by q.Validate() before the candidate
	// scan; the group-shape re-check here guards a hand-crafted cursor
	// with a correct fingerprint but an unrelated Group map.
	var cursor rollups.PageCursor
	if q.Cursor != "" {
		decoded, err := rollups.DecodeCursor(q.Cursor)
		if err != nil {
			return rollups.Result{}, err
		}
		if !cursorShapeMatches(decoded.Group, q.GroupBy) {
			return rollups.Result{}, fmt.Errorf("%w: cursor group shape does not match the query's GroupBy", rollups.ErrBadCursor)
		}
		cursor = decoded
	}
	page := make([]rollups.Row, 0, q.Limit+1)
	for _, r := range out {
		if len(page) > q.Limit {
			break
		}
		if q.Cursor != "" && !rowAfter(r, q, cursor) {
			continue
		}
		page = append(page, r)
	}
	if len(page) <= q.Limit {
		return rollups.Result{Rows: page}, nil
	}
	last := page[q.Limit-1]
	next := rollups.PageCursor{
		ShapeVersion: rollups.CursorShapeVersion,
		Fingerprint:  rollups.QueryShapeFingerprint(q),
		BucketNano:   last.BucketStart.UnixNano(),
		Group:        last.Dimensions,
	}
	if q.Sort == rollups.SortKeyMeasureAsc || q.Sort == rollups.SortKeyMeasureDesc {
		next.MeasureVal = last.Measures[q.SortMeasure].N
	}
	cursorStr, err := rollups.EncodeCursor(next)
	if err != nil {
		return rollups.Result{}, err
	}
	return rollups.Result{Rows: page[:q.Limit], NextCursor: cursorStr}, nil
}

// cursorShapeMatches reports whether the cursor's group values carry
// exactly the query's GroupBy dimensions — a cursor produced by a query
// with a different GroupBy (or hand-crafted) must be rejected loudly
// rather than silently mis-paginating.
func cursorShapeMatches(group rollups.DimensionValues, groupBy []rollups.Dimension) bool {
	if len(group) != len(groupBy) {
		return false
	}
	for _, d := range groupBy {
		if _, ok := group[d]; !ok {
			return false
		}
	}
	return true
}

// dimensionSlot maps a closed dimension to its AllDimensions slot.
func dimensionSlot(d rollups.Dimension) int {
	for i, cd := range rollups.AllDimensions {
		if d == cd {
			return i
		}
	}
	return 0 // unreachable for validated GroupBy members
}

// rowLess is the query's total order: primary key, then bucket start,
// then the grouped dimension values (canonical order). Deterministic.
// Measure comparisons use the exact integer MeasureValue.N — never
// float.
func rowLess(a, b rollups.Row, q rollups.Query) bool {
	switch q.Sort {
	case rollups.SortKeyBucketDesc:
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.After(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureAsc:
		av := a.Measures[q.SortMeasure].N
		bv := b.Measures[q.SortMeasure].N
		if av != bv {
			return av < bv
		}
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureDesc:
		av := a.Measures[q.SortMeasure].N
		bv := b.Measures[q.SortMeasure].N
		if av != bv {
			return av > bv
		}
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	default: // SortKeyBucketAsc
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	}
}

// rowAfter reports whether the row sorts strictly after the cursor
// position — the keyset "next page starts here" predicate. Uses the same
// total order as rowLess.
func rowAfter(r rollups.Row, q rollups.Query, c rollups.PageCursor) bool {
	bNano := r.BucketStart.UnixNano()
	groupAfter := c.Group.Less(r.Dimensions)
	switch q.Sort {
	case rollups.SortKeyBucketDesc:
		return bNano < c.BucketNano || (bNano == c.BucketNano && groupAfter)
	case rollups.SortKeyMeasureAsc:
		v := r.Measures[q.SortMeasure].N
		return v > c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	case rollups.SortKeyMeasureDesc:
		v := r.Measures[q.SortMeasure].N
		return v < c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	default: // SortKeyBucketAsc
		return bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)
	}
}

// distinctStrings returns vals with adjacent-preserving deduplication
// (set semantics: order is preserved, duplicates collapse to one
// occurrence).
func distinctStrings(vals []string) []string {
	if len(vals) < 2 {
		return vals
	}
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// mergeMeasureSet accumulates src into dst with the same checked
// semantics as the domain's MeasureSet.Add — every additive field is
// verified against the exact int64 bounds BEFORE any write, a negative
// delta fails loudly with rollups.ErrNegativeMeasure, and an overflow
// fails loudly with rollups.ErrMeasureOverflow, leaving both inputs
// untouched. The one deliberate difference: the latency min/max fold is
// driven by the exported LLMLatencyCount field (a stored row carries no
// separate fold-presence flag; count > 0 IS the fold identity), because
// the domain's fold-presence flag is unexported and cannot be
// reconstructed by a driver reading its own rows back. The result is
// observationally identical: LLMLatencyMinMS / LLMLatencyMaxMS are
// exact whenever LLMLatencyCount > 0, and the returned set is safe to
// hand back to the driver's own merge (never to MeasureSet.Add, which
// would treat the missing fold flag as "no latency folded yet").
func mergeMeasureSet(dst, src rollups.MeasureSet) (rollups.MeasureSet, error) {
	var out rollups.MeasureSet

	sums := []struct {
		dst, src int64
		measure  rollups.Measure
		out      *int64
	}{
		{dst.LLMCompletions, src.LLMCompletions, rollups.MeasureLLMCompletions, &out.LLMCompletions},
		{dst.LLMTokensPrompt, src.LLMTokensPrompt, rollups.MeasureLLMTokensPrompt, &out.LLMTokensPrompt},
		{dst.LLMTokensCompletion, src.LLMTokensCompletion, rollups.MeasureLLMTokensCompletion, &out.LLMTokensCompletion},
		{dst.LLMTokensReasoning, src.LLMTokensReasoning, rollups.MeasureLLMTokensReasoning, &out.LLMTokensReasoning},
		{dst.LLMTokensCacheRead, src.LLMTokensCacheRead, rollups.MeasureLLMTokensCacheRead, &out.LLMTokensCacheRead},
		{dst.LLMTokensCacheWrite, src.LLMTokensCacheWrite, rollups.MeasureLLMTokensCacheWrite, &out.LLMTokensCacheWrite},
		{dst.LLMTokensTotal, src.LLMTokensTotal, rollups.MeasureLLMTokensTotal, &out.LLMTokensTotal},
		{dst.LLMCostMicros, src.LLMCostMicros, rollups.MeasureLLMCostMicros, &out.LLMCostMicros},
		{dst.LLMLatencyCount, src.LLMLatencyCount, rollups.MeasureLLMLatencyCount, &out.LLMLatencyCount},
		{dst.LLMLatencySumMS, src.LLMLatencySumMS, rollups.MeasureLLMLatencySumMS, &out.LLMLatencySumMS},
		{dst.TasksCompleted, src.TasksCompleted, rollups.MeasureTasksCompleted, &out.TasksCompleted},
		{dst.TasksFailed, src.TasksFailed, rollups.MeasureTasksFailed, &out.TasksFailed},
		{dst.TasksCancelled, src.TasksCancelled, rollups.MeasureTasksCancelled, &out.TasksCancelled},
	}
	for _, s := range sums {
		v, err := addInt64(s.dst, s.src, s.measure)
		if err != nil {
			return rollups.MeasureSet{}, err
		}
		*s.out = v
	}

	// Latency min/max are folds — exact comparisons, never sums, so they
	// cannot overflow. The fold identity is the stored count: a source
	// with count > 0 contributes its min/max; a source with count == 0
	// (a task-outcome delta, an empty group) contributes nothing.
	out.LLMLatencyMinMS = dst.LLMLatencyMinMS
	out.LLMLatencyMaxMS = dst.LLMLatencyMaxMS
	if src.LLMLatencyCount > 0 {
		if dst.LLMLatencyCount == 0 || src.LLMLatencyMinMS < dst.LLMLatencyMinMS {
			out.LLMLatencyMinMS = src.LLMLatencyMinMS
		}
		if dst.LLMLatencyCount == 0 || src.LLMLatencyMaxMS > dst.LLMLatencyMaxMS {
			out.LLMLatencyMaxMS = src.LLMLatencyMaxMS
		}
	}
	return out, nil
}

// addInt64 returns a+b when the exact int64 sum is representable,
// failing loudly when it is not — the driver's counterpart of the
// domain's checked sum: a NEGATIVE delta is refused up front with
// wrapped rollups.ErrNegativeMeasure (measures are non-negative
// additive source aggregates), and a non-negative delta that would
// overflow the int64 range fails with wrapped rollups.ErrMeasureOverflow
// naming the measure.
func addInt64(a, b int64, measure rollups.Measure) (int64, error) {
	if b < 0 {
		return 0, fmt.Errorf("%w: %s delta %d is negative", rollups.ErrNegativeMeasure, measure, b)
	}
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %s sum would overflow the int64 range", rollups.ErrMeasureOverflow, measure)
	}
	return a + b, nil
}
