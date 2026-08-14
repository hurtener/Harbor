// Package postgres is Harbor's Postgres-backed `turns.Store` driver —
// the durable, indexed leg of the turn-projection persistence triad
// (in-memory / SQLite / Postgres) the v1.28 `sessions.turns.list` /
// `sessions.turns.get` Protocol surface projects over (RFC §6.9,
// AGENTS.md §9).
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Every query is parameterised; no string
// concatenation into SQL (AGENTS.md §9). An advisory lock serialises
// the migration runner so multi-replica boots are race-free.
//
// # Contract parity
//
// Behavioural parity with the SQLite turn-store driver is proven by
// the shared `internal/sessions/turns/conformancetest` suite, which
// this driver passes unchanged — no interface change, no `Supports*`
// capability ceremony (AGENTS.md §4.4). The store contract's
// non-negotiable guarantees all ride Postgres primitives:
//
//   - EXACT identity + effective-agent + session/root-turn row key:
//     `turn_rows` is keyed by the isolation triple + the root turn id
//     (the primary key), and the row's effective agent binding is
//     carried as a denormalised, INDEXED column
//     (`turn_rows_by_agent`) — agent id is selection metadata, never
//     an isolation principal (AGENTS.md §6), so it is deliberately
//     not part of the key.
//   - Transactional idempotent local-sequence application: the next
//     per-session sequence is minted in the SAME transaction as the
//     row write by an `INSERT ... ON CONFLICT DO UPDATE` over the
//     session's counter row (row-level serialisation), and the row
//     insert is `ON CONFLICT DO NOTHING` idempotent — a replay of an
//     already-applied append returns the stored row unchanged. The
//     monotonic, idempotent checkpoint is the same upsert shape with
//     `GREATEST`, so concurrent projectors converge to the max. This
//     is per-session conditional-write concurrency control, NOT an
//     active-active exactly-once claim: exactly-once application of
//     observations is the Projector's job (idempotent append by turn
//     id + version guards + monotonic checkpoint), and the driver
//     never claims cross-process consensus it does not provide.
//   - Stable tail / keyset / snapshot paging with no OFFSET and no
//     history scan: `ListTurns` pages newest-first over the immutable
//     `(sequence, turn_id)` keys through the `turn_rows_keyset`
//     index, fetching `limit+1` rows (so it knows exactly whether
//     older rows remain) and computing the exact older-row count as
//     an INDEX-ONLY count over the same keyset predicate — never a
//     sequential scan, never an OFFSET. The count is stable under
//     concurrent appends (per-session sequences only ever increase),
//     and the retained window is bounded by eviction.
//   - Indexed get: `GetTurn` resolves through the primary key.
//   - Full renderable DTO / collections / availability / overflow
//     persistence: `row_json` carries the complete consumer-safe
//     `turns.TurnRow` byte-for-byte (query / answer / attachment
//     metadata / per-measure usage / derived reasoning / content-free
//     activity + exact totals / ordered App refs + availability /
//     pause / closed terminal fields / timing), so a driver is a
//     transport, never a normalizer.
//   - Permanent per-session erasure fence: `turn_sessions.fenced` is
//     set by `FenceSession` in the driver's own backend, checked in
//     the same transaction as every write (the write-path statements
//     refuse a fenced session atomically), and NEVER cleared by
//     `DeleteScope` — an erased session stays fenced across replay
//     and restart, so erasure can never be resurrected.
//
// # Concurrency
//
// The driver struct is immutable after construction (a `*sql.DB`
// pool, an atomic close flag, and the configured retention bound) and
// is safe for N concurrent goroutines. Per-session write
// serialisation is the row locks on `turn_sessions` +
// `turn_rows`; per-row conditional writes are the `version` /
// `sealed` guards in the UPDATE's WHERE clause. Cancellation rides
// the per-call `ctx` (a cancelled call can never leak into a sibling
// call); Close joins nothing — the driver spawns no goroutines beyond
// the `database/sql` pool, which Close tears down.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// pgxDriverName is the database/sql driver name registered by the pgx
// stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Values mirror the StateStore + MemoryStore
// + SkillsStore Postgres drivers for consistency; tuning lives in a
// future config knob, not here.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// Config configures a Postgres-backed turns.Store.
type Config struct {
	// DSN is the Postgres connection string (pgx URI or keyword form).
	DSN string
	// Retention bounds the number of turn rows a session retains
	// (newest kept; the oldest are evicted and the session's explicit
	// truncation flag is set). <= 0 means the documented default
	// turns.MaxRetainedTurns (200).
	Retention int
}

// New opens a Postgres-backed turns.Store against cfg.DSN, applies
// the forward-only migrations, and returns a driver safe for N
// concurrent goroutines. A missing DSN fails loud; a misconfigured
// DSN fails at boot via an eager ping, never on the first write.
func New(cfg Config) (turns.Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("turns/postgres: cfg.DSN is required")
	}
	retention := cfg.Retention
	if retention <= 0 {
		retention = turns.MaxRetainedTurns
	}
	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("turns/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first AppendTurnIf.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("turns/postgres: ping: %w", err)
	}
	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &driver{db: db, retention: retention}, nil
}

// driver is the Postgres-backed turns.Store implementation. Safe for
// concurrent use by N goroutines.
type driver struct {
	db        *sql.DB
	retention int
	closed    atomic.Bool
}

// Compile-time assertion that *driver satisfies turns.Store.
var _ turns.Store = (*driver)(nil)

// Durable reports true: Postgres rows, checkpoints AND the store-local
// erasure fence all survive a process restart.
func (d *driver) Durable() bool { return true }

// check fast-fails a closed store and honors a cancelled context.
func (d *driver) check(ctx context.Context) error {
	if d.closed.Load() {
		return turns.ErrStoreClosed
	}
	return ctx.Err()
}

// checkIdentity maps the mandatory-triple validation onto the store's
// identity sentinel (AGENTS.md §6; identity is mandatory, no opt-out).
func (d *driver) checkIdentity(id identity.Identity) error {
	if identity.Validate(id) != nil {
		return turns.ErrIdentityRequired
	}
	return nil
}

// dbSeq converts a uint64 runtime event sequence to the int64 stored
// in the BIGINT `last_applied_event_seq` / `checkpoint` columns.
// Runtime event sequences are monotonic counters bounded far below
// MaxInt64 in every reachable domain (2^63 applied observations is not
// a real deployment), so the conversion is lossless there.
func dbSeq(u uint64) int64 {
	return int64(u) //nolint:gosec // G115: uint64→int64 is lossless below MaxInt64, the reachable event-sequence domain
}

// fromDBSeq converts the int64 checkpoint read from the BIGINT column
// back to the runtime uint64 event sequence. Checkpoints are monotonic
// non-negative counters, so the sign bit is never set.
func fromDBSeq(i int64) uint64 {
	return uint64(i) //nolint:gosec // G115: checkpoints are non-negative monotonic counters
}

// AppendTurnIf creates the mutable row for id / row.TurnID, minting
// the next immutable per-session sequence ATOMICALLY with the write
// and the erasure-fence check (one transaction, one upsert over the
// session counter row). Idempotent on the turn id: a replay of an
// already-applied append returns the stored row unchanged — the row
// insert is `ON CONFLICT DO NOTHING` and the minted sequence of a
// no-op replay is simply consumed, never attached to the stored row.
func (d *driver) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.check(ctx); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TurnRow{}, err
	}
	if row.TurnID == "" {
		return turns.TurnRow{}, fmt.Errorf("%w: turn id is empty", turns.ErrInvalidInput)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: append begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one

	// 1. Mint the next per-session sequence ATOMICALLY with the
	//    erasure-fence check. The `DO UPDATE ... WHERE NOT fenced` arm
	//    is skipped for a fenced session, so the statement returns no
	//    row and the append fails with ErrErasureFenced in the same
	//    transaction as the (refused) write. Concurrent appends
	//    serialize on the session counter row and each see a distinct
	//    sequence — no duplicate sequence is ever observable.
	var seq int64
	err = tx.QueryRowContext(ctx, `
        INSERT INTO turn_sessions (tenant_id, user_id, session_id, next_seq, fenced)
        VALUES ($1, $2, $3, 1, false)
        ON CONFLICT (tenant_id, user_id, session_id) DO UPDATE
            SET next_seq = turn_sessions.next_seq + 1
            WHERE NOT turn_sessions.fenced
        RETURNING next_seq`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, fmt.Errorf("%w: session %q is erasure-fenced",
			turns.ErrErasureFenced, id.SessionID)
	}
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: mint sequence: %w", err)
	}

	// 2. Build the row: the store mints the immutable ordering keys
	//    and the initial version; the caller's content is preserved
	//    as-is (a driver is a transport, never a normalizer).
	next := row
	next.Sequence = turns.Seq(seq)
	next.TieBreaker = row.TurnID
	next.Version = 1
	jsonBytes, err := json.Marshal(next)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: encode row: %w", err)
	}

	// 3. Idempotent row write. A concurrent append (or a replay) wins
	//    the slot: the insert no-ops and the STORED row is returned.
	res, err := tx.ExecContext(ctx, `
        INSERT INTO turn_rows
            (tenant_id, user_id, session_id, turn_id, effective_agent_id,
             sequence, sealed, version, last_applied_event_seq, row_json)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        ON CONFLICT (tenant_id, user_id, session_id, turn_id) DO NOTHING`,
		id.TenantID, id.UserID, id.SessionID, string(row.TurnID), row.Agent.ID,
		int64(next.Sequence), next.Sealed, next.Version, dbSeq(next.LastAppliedEventSeq),
		string(jsonBytes),
	)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: insert row: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: insert rowcount: %w", err)
	}
	if affected == 0 {
		existing, err := d.getTurnTx(ctx, tx, id, row.TurnID)
		if err != nil {
			return turns.TurnRow{}, fmt.Errorf("turns/postgres: idempotent replay read-back: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return turns.TurnRow{}, fmt.Errorf("turns/postgres: commit replay: %w", err)
		}
		return existing, nil
	}

	// 4. Retention: evict the session's oldest rows past the bound and
	//    surface the eviction as the session's explicit truncation flag.
	//    The session counter row is still locked by step 1, so no
	//    concurrent append can interleave the count + eviction.
	if err := d.enforceRetentionTx(ctx, tx, id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: commit append: %w", err)
	}
	// Deep-copy the return: decode the stored envelope so the returned
	// row never aliases caller memory (the store contract's no-alias
	// gate, pinned by Row_DeepCopy_NoAliasing).
	return decodeRow(jsonBytes)
}

// enforceRetentionTx evicts the session's oldest rows past d.retention
// and sets the explicit truncation flag. Caller holds the session
// counter row lock (or the transaction is otherwise serialized).
func (d *driver) enforceRetentionTx(ctx context.Context, tx *sql.Tx, id identity.Identity) error {
	var n int
	if err := tx.QueryRowContext(ctx, `
        SELECT count(*) FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&n); err != nil {
		return fmt.Errorf("turns/postgres: retention count: %w", err)
	}
	if n <= d.retention {
		return nil
	}
	evict := n - d.retention
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
          AND turn_id IN (
              SELECT turn_id FROM turn_rows
              WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
              ORDER BY sequence ASC, turn_id ASC
              LIMIT $4
          )`,
		id.TenantID, id.UserID, id.SessionID, evict,
	); err != nil {
		return fmt.Errorf("turns/postgres: retention evict: %w", err)
	}
	// Retention eviction is explicit, never silent.
	if _, err := tx.ExecContext(ctx, `
        UPDATE turn_sessions SET truncated = true
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	); err != nil {
		return fmt.Errorf("turns/postgres: retention flag: %w", err)
	}
	return nil
}

// mutate is the shared UpdateTurnIf / SealTurnIf conditional-write
// path: fence check under the session row lock, row load under the row
// lock, guard evaluation, conditional UPDATE. The row locks serialize
// concurrent writers (a stale version is refused with ErrStaleVersion,
// a sealed row with ErrTurnSealed, a racing erasure with
// ErrErasureFenced); the UPDATE's WHERE re-guards version + sealed so
// the conditional write is exact even across a pool connection.
func (d *driver) mutate(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, sealed bool) (turns.TurnRow, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: mutate begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one

	// 1. Erasure-fence check under the session row lock — serializes
	//    against FenceSession (which upserts the same row), so a fence
	//    committed before this check is seen, and one committed after
	//    this write is a write that strictly precedes the erasure.
	var fenced bool
	err = tx.QueryRowContext(ctx, `
        SELECT fenced FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&fenced)
	if errors.Is(err, sql.ErrNoRows) {
		fenced = false // no session row → never fenced (appends mint it; erasure keeps it)
	} else if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: fence check: %w", err)
	}
	if fenced {
		return turns.TurnRow{}, fmt.Errorf("%w: session %q is erasure-fenced",
			turns.ErrErasureFenced, id.SessionID)
	}

	// 2. Load the stored row under its row lock.
	var (
		curSeq    int64
		curSealed bool
		curVer    int
	)
	err = tx.QueryRowContext(ctx, `
        SELECT sequence, sealed, version FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID, string(turnID),
	).Scan(&curSeq, &curSealed, &curVer)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, fmt.Errorf("%w: turn %q", turns.ErrTurnNotFound, turnID)
	}
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: load row: %w", err)
	}
	if curSealed {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnSealed, turnID)
	}
	if curVer != expectedVersion {
		return turns.TurnRow{}, fmt.Errorf("%w: stored version %d, expected %d",
			turns.ErrStaleVersion, curVer, expectedVersion)
	}

	// 3. Build the next row: the immutable ordering keys are
	//    preserved; sealed / version are store-owned.
	next := row
	next.TurnID = turnID
	next.Sequence = turns.Seq(curSeq)
	next.TieBreaker = turnID // the immutable secondary order key is the turn id
	next.Sealed = sealed
	next.Version = curVer + 1
	jsonBytes, err := json.Marshal(next)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: encode row: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
        UPDATE turn_rows SET
            effective_agent_id     = $5,
            sealed                 = $6,
            version                = $7,
            last_applied_event_seq = $8,
            row_json               = $9
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4
          AND version = $10 AND NOT sealed`,
		id.TenantID, id.UserID, id.SessionID, string(turnID),
		next.Agent.ID, sealed, next.Version, dbSeq(next.LastAppliedEventSeq),
		string(jsonBytes), expectedVersion,
	)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: conditional update: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: update rowcount: %w", err)
	}
	if affected == 0 {
		// Under the row lock this is impossible by construction; fail
		// loud rather than silently report a write that did not land.
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: conditional update did not converge for turn %q (version %d)", turnID, expectedVersion)
	}
	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: commit mutate: %w", err)
	}
	// Deep-copy the return (see AppendTurnIf).
	return decodeRow(jsonBytes)
}

// UpdateTurnIf atomically replaces a MUTABLE row at an expected
// version.
func (d *driver) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.check(ctx); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TurnRow{}, err
	}
	return d.mutate(ctx, id, turnID, expectedVersion, row, false)
}

// SealTurnIf atomically replaces a MUTABLE row with its SEALED
// terminal form.
func (d *driver) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.check(ctx); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TurnRow{}, err
	}
	return d.mutate(ctx, id, turnID, expectedVersion, row, true)
}

// FenceSession marks id's session as ERASURE-FENCED in the driver's
// own backend. Idempotent; the fence is NEVER removed by DeleteScope —
// an erased session stays fenced across replay and restart (no
// resurrection).
func (d *driver) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	if err := d.checkIdentity(id); err != nil {
		return err
	}
	_, err := d.db.ExecContext(ctx, `
        INSERT INTO turn_sessions (tenant_id, user_id, session_id, fenced)
        VALUES ($1, $2, $3, true)
        ON CONFLICT (tenant_id, user_id, session_id) DO UPDATE SET fenced = true`,
		id.TenantID, id.UserID, id.SessionID,
	)
	if err != nil {
		return fmt.Errorf("turns/postgres: fence session: %w", err)
	}
	return nil
}

// queryer is the minimal row-query surface shared by *sql.DB and
// *sql.Tx so getTurnTx serves both the direct read and in-transaction
// read-back paths.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// getTurnTx reads one retained row by its primary key (identity triple
// + turn id) through q.
func (d *driver) getTurnTx(ctx context.Context, q queryer, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	var raw string
	err := q.QueryRowContext(ctx, `
        SELECT row_json FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4`,
		id.TenantID, id.UserID, id.SessionID, string(turnID),
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, turns.ErrTurnNotFound
	}
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: get row: %w", err)
	}
	return decodeRowString(raw)
}

// GetTurn reads one retained row; ErrTurnNotFound when the turn was
// never created, was evicted past the retention bound, or was erased.
func (d *driver) GetTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	if err := d.check(ctx); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TurnRow{}, err
	}
	return d.getTurnTx(ctx, d.db, id, turnID)
}

// ListTurns returns one newest-first keyset page of at most limit
// retained rows strictly older than before (nil before = the newest
// page), ordered by (Sequence DESC, TurnID DESC). The page is an
// index-served scan over the immutable keys (fetching limit+1 to know
// exactly whether older rows remain) and the exact older-row count is
// an index-only count over the same keyset predicate — no OFFSET, no
// history scan. Cursor binding (session / projection snapshot /
// authoritative boundary row) is enforced before paging, each failure
// a distinct domain error.
func (d *driver) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, turns.ListPageInfo, error) {
	var zero turns.ListPageInfo
	if err := d.check(ctx); err != nil {
		return nil, nil, zero, err
	}
	if err := d.checkIdentity(id); err != nil {
		return nil, nil, zero, err
	}
	if limit < 1 {
		return nil, nil, zero, fmt.Errorf("%w: limit %d", turns.ErrInvalidInput, limit)
	}

	// Projection snapshot generation + explicit truncation flag.
	var snapshot uint64
	var truncated bool
	err := d.db.QueryRowContext(ctx, `
        SELECT snapshot, truncated FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&snapshot, &truncated)
	if errors.Is(err, sql.ErrNoRows) {
		snapshot, truncated = 0, false // a fresh session: initial generation, no truncation
	} else if err != nil {
		return nil, nil, zero, fmt.Errorf("turns/postgres: session state: %w", err)
	}

	// Opaque-cursor BINDING: the cursor is only valid for this
	// session, against this projection snapshot, with a retained
	// boundary row whose immutable sequence matches the cursor's.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, zero, fmt.Errorf("%w: cursor names session %q, request is %q",
				turns.ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, zero, fmt.Errorf("%w: cursor snapshot %d, current %d",
				turns.ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		var seq int64
		err := d.db.QueryRowContext(ctx, `
            SELECT sequence FROM turn_rows
            WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4`,
			id.TenantID, id.UserID, id.SessionID, string(before.TurnID),
		).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, zero, fmt.Errorf("%w: boundary row %q is no longer retained",
				turns.ErrCursorExpired, before.TurnID)
		}
		if err != nil {
			return nil, nil, zero, fmt.Errorf("turns/postgres: cursor boundary: %w", err)
		}
		// The cursor is BOUND to the AUTHORITATIVE boundary row: a
		// forged / altered cursor that names a retained row but
		// carries a sequence that does not equal the stored row's
		// immutable sequence is refused with ErrInvalidCursor — it
		// would otherwise silently skip or repeat rows.
		if turns.Seq(seq) != before.Seq {
			return nil, nil, zero, fmt.Errorf("%w: cursor sequence %d does not match the stored boundary row %q (sequence %d) — forged or altered cursor",
				turns.ErrInvalidCursor, before.Seq, before.TurnID, seq)
		}
	}

	// Keyset page: fetch limit+1 rows strictly older than before
	// (nil before = the newest page), newest-first over the immutable
	// (sequence, turn_id) keys. The ORDER BY matches the
	// turn_rows_keyset index exactly, so the page is served by an
	// index scan with no OFFSET and no history scan. The query is
	// built with a strings.Builder + fmt.Fprintf over a fixed SQL
	// skeleton (the only interpolated value is the integer page size
	// from the validated limit); every identity / keyset value is a
	// parameter.
	var sb strings.Builder
	sb.WriteString(`SELECT row_json FROM turn_rows
                WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`)
	args := []any{id.TenantID, id.UserID, id.SessionID}
	if before != nil {
		sb.WriteString(` AND (sequence < $4 OR (sequence = $4 AND turn_id < $5))`)
		args = append(args, int64(before.Seq), string(before.TurnID))
	}
	fmt.Fprintf(&sb, ` ORDER BY sequence DESC, turn_id DESC LIMIT %d`, limit+1)

	rows, err := d.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, nil, zero, fmt.Errorf("turns/postgres: list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page []turns.TurnRow
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, nil, zero, fmt.Errorf("turns/postgres: list scan: %w", err)
		}
		row, err := decodeRowString(raw)
		if err != nil {
			return nil, nil, zero, err
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, zero, fmt.Errorf("turns/postgres: list iterate: %w", err)
	}

	info := turns.ListPageInfo{Snapshot: snapshot, Truncated: truncated}
	if len(page) <= limit {
		info.Remaining = 0
		info.CountExact = true
		return page, nil, info, nil
	}
	page = page[:limit]
	last := page[limit-1]

	// Exact older-row count: the page returned limit+1 rows, so more
	// rows exist; count the retained rows strictly older than the
	// page's boundary. The count's WHERE columns are all inside the
	// turn_rows_keyset index, so it resolves as an index-only scan —
	// never a sequential history scan. It is also STABLE under
	// concurrent appends: per-session sequences only ever increase, so
	// a concurrent append can never mint a row that satisfies the
	// older-than-boundary predicate (only concurrent retention
	// eviction could shrink it, and that is the explicit, Truncated-
	// flagged truncation contract).
	var older int
	err = d.db.QueryRowContext(ctx, `
        SELECT count(*) FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
          AND (sequence < $4 OR (sequence = $4 AND turn_id < $5))`,
		id.TenantID, id.UserID, id.SessionID,
		int64(last.Sequence), string(last.TurnID),
	).Scan(&older)
	if err != nil {
		return nil, nil, zero, fmt.Errorf("turns/postgres: list count: %w", err)
	}
	info.Remaining = older
	info.CountExact = true
	next := &turns.Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	return page, next, info, nil
}

// LoadCheckpoint returns the session's last-applied runtime event
// sequence; 0 when none was ever saved. Reads are not fenced — a
// fenced session's checkpoint is still readable (it reads 0 after
// erasure).
func (d *driver) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if err := d.check(ctx); err != nil {
		return 0, err
	}
	if err := d.checkIdentity(id); err != nil {
		return 0, err
	}
	var cp int64
	err := d.db.QueryRowContext(ctx, `
        SELECT checkpoint FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&cp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("turns/postgres: load checkpoint: %w", err)
	}
	return fromDBSeq(cp), nil
}

// SaveCheckpoint records the session's last-applied runtime event
// sequence. MONOTONIC and IDEMPOTENT (GREATEST — a save at or below
// the stored checkpoint never regresses it), serialized per session
// by the counter-row lock, and refused atomically with
// ErrErasureFenced when the session is fenced (a rebuild must never
// advance the checkpoint of an erased session — no resurrection).
func (d *driver) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	if err := d.checkIdentity(id); err != nil {
		return err
	}
	var fenced bool
	err := d.db.QueryRowContext(ctx, `
        INSERT INTO turn_sessions (tenant_id, user_id, session_id, checkpoint, fenced)
        VALUES ($1, $2, $3, $4, false)
        ON CONFLICT (tenant_id, user_id, session_id) DO UPDATE
            SET checkpoint = GREATEST(turn_sessions.checkpoint, excluded.checkpoint)
            WHERE NOT turn_sessions.fenced
        RETURNING fenced`,
		id.TenantID, id.UserID, id.SessionID, dbSeq(seq),
	).Scan(&fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %q is erasure-fenced", turns.ErrErasureFenced, id.SessionID)
	}
	if err != nil {
		return fmt.Errorf("turns/postgres: save checkpoint: %w", err)
	}
	return nil
}

// DeleteScope removes every retained turn row and clears the
// checkpoint / local-sequence counter under id (the erasure cascade's
// projection leg). Idempotent: an absent scope returns (0, nil). The
// erasure FENCE is deliberately NOT cleared — the caller sets it via
// FenceSession before calling DeleteScope, and this method never
// removes it, so an erased session stays fenced. The projection
// snapshot generation ADVANCES so any cursor minted before the erase
// is rejected as stale.
func (d *driver) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if err := d.check(ctx); err != nil {
		return 0, err
	}
	if err := d.checkIdentity(id); err != nil {
		return 0, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("turns/postgres: erase begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one

	// 1. Ensure the session row exists (the fence — if any — is left
	//    untouched), then lock it so the row delete + state reset are
	//    serialized against concurrent writes and fences.
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO turn_sessions (tenant_id, user_id, session_id)
        VALUES ($1, $2, $3)
        ON CONFLICT (tenant_id, user_id, session_id) DO NOTHING`,
		id.TenantID, id.UserID, id.SessionID,
	); err != nil {
		return 0, fmt.Errorf("turns/postgres: erase ensure session: %w", err)
	}
	var (
		prevCheckpoint int64
		prevNextSeq    int64
	)
	if err := tx.QueryRowContext(ctx, `
        SELECT checkpoint, next_seq FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&prevCheckpoint, &prevNextSeq); err != nil {
		return 0, fmt.Errorf("turns/postgres: erase lock session: %w", err)
	}

	// 2. Delete ONLY this projection's owned rows. The erasure FENCE
	//    (fenced) is never cleared here.
	res, err := tx.ExecContext(ctx, `
        DELETE FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("turns/postgres: erase rows: %w", err)
	}
	rowCount, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("turns/postgres: erase rowcount: %w", err)
	}

	// 3. Reset the session's projection state and advance the snapshot
	//    generation (as-of retention generation) so a pre-erase cursor
	//    is rejected as stale. fenced is deliberately NOT touched.
	if _, err := tx.ExecContext(ctx, `
        UPDATE turn_sessions SET
            next_seq   = 0,
            checkpoint = 0,
            truncated  = false,
            snapshot   = turn_sessions.snapshot + 1
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`,
		id.TenantID, id.UserID, id.SessionID,
	); err != nil {
		return 0, fmt.Errorf("turns/postgres: erase reset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("turns/postgres: commit erase: %w", err)
	}

	deleted := int(rowCount)
	if prevCheckpoint > 0 {
		deleted++
	}
	if prevNextSeq > 0 {
		deleted++
	}
	return deleted, nil
}

// Close releases the connection pool. Idempotent; subsequent calls
// fail with ErrStoreClosed.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("turns/postgres: close: %w", err)
	}
	return nil
}

// decodeRow decodes a stored JSON envelope into a fresh TurnRow — a
// byte-serializing driver never lets caller memory reach (or escape)
// durable state, and every read boundary returns an unaliased copy.
func decodeRow(raw []byte) (turns.TurnRow, error) {
	var row turns.TurnRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return turns.TurnRow{}, fmt.Errorf("turns/postgres: decode row: %w", err)
	}
	return row, nil
}

func decodeRowString(raw string) (turns.TurnRow, error) {
	return decodeRow([]byte(raw))
}
