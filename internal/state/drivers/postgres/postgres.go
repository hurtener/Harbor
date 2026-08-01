// Package postgres is Harbor's V1 Postgres-backed StateStore driver.
//
// It is the multi-node production target for the §9 persistence
// triad: the third leg, alongside the in-memory reference
// and the SQLite driver. Harbor inherits
// `internal/state/conformancetest.Run` verbatim — the suite IS the
// gate; this driver ships zero new conformance scenarios.
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Parametric queries everywhere; no string
// concatenation into SQL (AGENTS.md §9). Advisory locks serialise
// the migration runner so multi-replica boots are race-free.
//
// Internal model:
//
//   - One row per (tenant, user, session, run, kind). The composite
//     primary key is the identity quadruple plus Kind. RunID may be
//     empty (session-scoped state); the column is NOT NULL but
//     accepts the empty string.
//   - `bytes` is BYTEA — opaque payload, no JSONB constraint.
//   - `event_id` carries a UNIQUE secondary index for LoadByEventID
//     and to defend against duplicate-id leaks under contention.
//   - Save is a transactional UPSERT (`INSERT ... ON CONFLICT DO
//     UPDATE`) prefaced by an idempotency probe on `event_id`. When
//     a slot already holds a different EventID, the previous EventID
//     row is implicitly evicted because the slot's row is updated in
//     place.
//   - `Close(ctx)` flips an atomic flag BEFORE calling `db.Close()`
//     so subsequent calls fast-fail with `ErrStoreClosed` even while
//     in-flight queries are draining.
//
// Per AGENTS.md §5, the driver is safe for concurrent reuse
// across N goroutines. The conformance suite's `Concurrent_SaveLoad_NoRace`
// + the local `concurrent_test.go` enforce this under -race.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// driverName is the name under which this driver self-registers.
const driverName = "postgres"

// pgxDriverName is the database/sql driver name registered by the
// pgx stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Documented in the phase plan; tuning
// lives in a future config knob, not here.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// Postgres SQLSTATE codes mapped at the boundary so callers compare
// against state.* sentinels, never raw pgx errors.
const (
	pgUniqueViolation = "23505"
	pgDeadlockFound   = "40P01"
)

// New constructs a Postgres-backed state.StateStore against cfg.DSN.
// Production callers go through state.Open; tests may call New
// directly to skip the registry.
//
// Errors:
//   - empty cfg.DSN
//   - sql.Open / migration apply failure
//   - advisory-lock acquisition failure (extremely unusual; would
//     indicate severe DB load or operator misconfiguration)
func New(cfg config.StateConfig) (state.StateStore, error) {
	if cfg.DSN == "" {
		return nil, errors.New("postgres: cfg.DSN is required")
	}
	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first Save.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &driver{db: db}, nil
}

func init() {
	state.Register(driverName, New)
}

// driver is the Postgres-backed state.StateStore implementation.
//
// Fields are immutable after construction except for the atomic
// `closed` flag (compiled artifacts are immutable; per-run
// state lives in ctx).
type driver struct {
	db     *sql.DB
	closed atomic.Bool
}

// Compile-time assertion that driver satisfies state.StateStore.
var _ state.StateStore = (*driver)(nil)

// Save implements state.StateStore.
//
// The implementation runs in a single transaction:
//
//  1. Look up any existing row with the same EventID via the unique
//     secondary index. If found and (Identity, Kind, Bytes, Version)
//     all match, the call is an idempotent no-op. If found but any
//     field differs, return ErrIdempotencyConflict.
//  2. Otherwise UPSERT on the composite primary key. If a different
//     EventID previously held the slot, the ON CONFLICT DO UPDATE
//     overwrites it; the unique constraint on event_id then naturally
//     evicts the previous EventID's secondary visibility (since the
//     row's event_id column changed in place).
//
// The transaction is REPEATABLE READ so the idempotency probe and
// the UPSERT see a consistent snapshot. Under contention (two
// concurrent Saves at the same slot) one may observe a
// unique_violation on event_id when both inserts pick different
// EventIDs targeting different slots — we map that to
// ErrIdempotencyConflict, since it indicates a routing mistake
// upstream.
func (d *driver) Save(ctx context.Context, r state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateRecord(r); err != nil {
		return err
	}
	updatedAt := r.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	payload := r.Bytes
	if payload == nil {
		// database/sql binds a nil []byte as SQL NULL, but StateStore's
		// byte-equality contract treats nil and an allocated empty slice as
		// the same valid zero-length payload. Preserve that contract while
		// satisfying the Postgres BYTEA NOT NULL storage invariant.
		payload = []byte{}
	}

	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return d.translateErr(err, "postgres: begin tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; surfaced error is the original failure
		}
	}()

	// 1. Idempotency probe by EventID.
	prev, prevOK, err := loadByEventIDTx(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	if prevOK {
		if prev.Identity != r.Identity || prev.Kind != r.Kind {
			return fmt.Errorf("%w: EventID %q already routes to a different (Quadruple, Kind)",
				state.ErrIdempotencyConflict, r.ID)
		}
		if !bytesEqual(prev.Bytes, r.Bytes) {
			return fmt.Errorf("%w: EventID %q already saved with different Bytes",
				state.ErrIdempotencyConflict, r.ID)
		}
		if prev.Version != r.Version {
			return fmt.Errorf("%w: EventID %q already saved with different Version",
				state.ErrIdempotencyConflict, r.ID)
		}
		// Idempotent no-op.
		if err := tx.Commit(); err != nil {
			return d.translateErr(err, "postgres: commit idempotent no-op")
		}
		committed = true
		return nil
	}

	// 2. UPSERT on the composite PK. ON CONFLICT (pk) overwrites
	// event_id, version, bytes, updated_at — which is exactly the
	// "evict previous EventID" semantics from the inmem reference.
	const upsert = `
		INSERT INTO state_records
			(tenant_id, user_id, session_id, run_id, kind, event_id, version, bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, user_id, session_id, run_id, kind) DO UPDATE
			SET event_id   = EXCLUDED.event_id,
			    version    = EXCLUDED.version,
			    bytes      = EXCLUDED.bytes,
			    updated_at = EXCLUDED.updated_at
	`
	if _, err := tx.ExecContext(ctx, upsert,
		r.Identity.TenantID, r.Identity.UserID, r.Identity.SessionID, r.Identity.RunID,
		r.Kind, string(r.ID), r.Version, payload, updatedAt,
	); err != nil {
		return d.translateUpsertErr(err)
	}

	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "postgres: commit upsert")
	}
	committed = true
	return nil
}

// SaveIf atomically verifies exact event-ID generations and writes next.
// Advisory transaction locks cover both present rows and absent slots; plain
// SELECT FOR UPDATE cannot lock the latter and would permit a first-write
// phantom. Sorting gives multi-slot callers one lock order.
func (d *driver) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateSaveIf(expectations, next); err != nil {
		return err
	}
	// The ordered advisory locks provide the serialisation guarantee, including
	// absent slots. Read committed is intentional: a waiter must observe the
	// winner that released its advisory lock, rather than retain a serializable
	// snapshot from before the wait and report a retry-class database error.
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return d.translateErr(err, "postgres: begin conditional tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()
	lockIDs, err := conditionalAdvisoryLockIDs(ctx, tx, expectations, postgresAdvisoryLockID)
	if err != nil {
		return d.translateErr(err, "postgres: conditional advisory lock ID")
	}
	for _, lockID := range lockIDs {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
			return d.translateErr(err, "postgres: conditional advisory lock")
		}
	}
	for _, expectation := range expectations {
		var actual string
		err := tx.QueryRowContext(ctx, `SELECT event_id FROM state_records WHERE tenant_id=$1 AND user_id=$2 AND session_id=$3 AND run_id=$4 AND kind=$5`,
			expectation.Identity.TenantID, expectation.Identity.UserID, expectation.Identity.SessionID, expectation.Identity.RunID, expectation.Kind).Scan(&actual)
		if expectation.ExpectedEventID == "" {
			if err == nil {
				return fmt.Errorf("postgres: %w: expected absent slot is present", state.ErrConditionFailed)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return d.translateErr(err, "postgres: conditional read")
			}
			continue
		}
		if errors.Is(err, sql.ErrNoRows) || (err == nil && actual != string(expectation.ExpectedEventID)) {
			return fmt.Errorf("postgres: %w: expected event_id %q", state.ErrConditionFailed, expectation.ExpectedEventID)
		}
		if err != nil {
			return d.translateErr(err, "postgres: conditional read")
		}
	}
	prev, prevOK, err := loadByEventIDTx(ctx, tx, next.ID)
	if err != nil {
		return err
	}
	if prevOK {
		if prev.Identity != next.Identity || prev.Kind != next.Kind || !bytesEqual(prev.Bytes, next.Bytes) || prev.Version != next.Version {
			return fmt.Errorf("postgres: %w: next EventID %q conflicts", state.ErrIdempotencyConflict, next.ID)
		}
		if err := tx.Commit(); err != nil {
			return d.translateErr(err, "postgres: commit conditional idempotent no-op")
		}
		committed = true
		return nil
	}
	payload := next.Bytes
	if payload == nil {
		payload = []byte{}
	}
	updatedAt := next.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	const upsert = `INSERT INTO state_records (tenant_id, user_id, session_id, run_id, kind, event_id, version, bytes, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, user_id, session_id, run_id, kind) DO UPDATE SET event_id=EXCLUDED.event_id, version=EXCLUDED.version, bytes=EXCLUDED.bytes, updated_at=EXCLUDED.updated_at`
	if _, err := tx.ExecContext(ctx, upsert, next.Identity.TenantID, next.Identity.UserID, next.Identity.SessionID, next.Identity.RunID, next.Kind, string(next.ID), next.Version, payload, updatedAt); err != nil {
		return d.translateUpsertErr(err)
	}
	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "postgres: commit conditional save")
	}
	committed = true
	return nil
}

func postgresSlotKey(expectation state.SlotExpectation) string {
	q := expectation.Identity
	// Length prefixes make this an injective representation even if a caller
	// supplies delimiters inside an identity component or kind.
	return fmt.Sprintf("%d:%s%d:%s%d:%s%d:%s%d:%s", len(q.TenantID), q.TenantID, len(q.UserID), q.UserID, len(q.SessionID), q.SessionID, len(q.RunID), q.RunID, len(expectation.Kind), expectation.Kind)
}

// advisoryLockID derives the exact signed bigint used as a PostgreSQL
// advisory lock key. Keeping it injectable makes collision ordering testable
// without requiring a deliberately collided PostgreSQL hash.
type advisoryLockID func(context.Context, *sql.Tx, string) (int64, error)

// conditionalAdvisoryLockIDs derives every PostgreSQL lock key before any
// acquisition, then sorts and deduplicates the actual signed bigint values.
// A hash collision is therefore only additional serialisation: it cannot
// cause duplicate acquisition or reverse the multi-slot lock order.
func conditionalAdvisoryLockIDs(ctx context.Context, tx *sql.Tx, expectations []state.SlotExpectation, lockID advisoryLockID) ([]int64, error) {
	ids := make([]int64, 0, len(expectations))
	for _, expectation := range expectations {
		id, err := lockID(ctx, tx, postgresSlotKey(expectation))
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return slices.Compact(ids), nil
}

// postgresAdvisoryLockID asks PostgreSQL for the same hash value that the
// advisory-lock call uses, so Go never assumes hash ordering matches slot-key
// ordering.
func postgresAdvisoryLockID(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT hashtextextended($1, 0)`, key).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// Load implements state.StateStore.
func (d *driver) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return state.StateRecord{}, err
	}
	if kind == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	const q1 = `
		SELECT event_id, version, bytes, updated_at
		FROM state_records
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4 AND kind = $5
	`
	row := d.db.QueryRowContext(ctx, q1,
		q.TenantID, q.UserID, q.SessionID, q.RunID, kind)
	var (
		eventID   string
		version   int
		buf       []byte
		updatedAt time.Time
	)
	if err := row.Scan(&eventID, &version, &buf, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state.StateRecord{}, fmt.Errorf("%w: %s/%s/%s/%s kind=%s",
				state.ErrNotFound, q.TenantID, q.UserID, q.SessionID, q.RunID, kind)
		}
		return state.StateRecord{}, d.translateErr(err, "postgres: load")
	}
	return state.StateRecord{
		ID:        state.EventID(eventID),
		Identity:  q,
		Kind:      kind,
		Version:   version,
		Bytes:     buf,
		UpdatedAt: updatedAt,
	}, nil
}

// LoadByEventID implements state.StateStore.
func (d *driver) LoadByEventID(ctx context.Context, eventID state.EventID) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if eventID == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	const q1 = `
		SELECT tenant_id, user_id, session_id, run_id, kind, version, bytes, updated_at
		FROM state_records
		WHERE event_id = $1
	`
	row := d.db.QueryRowContext(ctx, q1, string(eventID))
	var (
		tenantID, userID, sessionID, runID, kind string
		version                                  int
		buf                                      []byte
		updatedAt                                time.Time
	)
	if err := row.Scan(&tenantID, &userID, &sessionID, &runID, &kind, &version, &buf, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state.StateRecord{}, fmt.Errorf("%w: event_id=%s", state.ErrNotFound, eventID)
		}
		return state.StateRecord{}, d.translateErr(err, "postgres: load by event_id")
	}
	return state.StateRecord{
		ID: eventID,
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID},
			RunID:    runID,
		},
		Kind:      kind,
		Version:   version,
		Bytes:     buf,
		UpdatedAt: updatedAt,
	}, nil
}

// Delete implements state.StateStore. Returns nil whether or not a
// row was matched (idempotent).
func (d *driver) Delete(ctx context.Context, q identity.Quadruple, kind string) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return err
	}
	if kind == "" {
		return state.ErrInvalidRecord
	}

	const q1 = `
		DELETE FROM state_records
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4 AND kind = $5
	`
	if _, err := d.db.ExecContext(ctx, q1,
		q.TenantID, q.UserID, q.SessionID, q.RunID, kind); err != nil {
		return d.translateErr(err, "postgres: delete")
	}
	return nil
}

// ListKind implements state.StateStore — the explicitly-elevated
// maintenance scan (RFC §6.11). The prefix matches literally:
// LIKE metacharacters in kindPrefix are escaped so a prefix containing
// `%` or `_` cannot widen the scan.
// DeleteScope implements state.StateStore — the kind-agnostic cascade
// primitive. A single DELETE removes every row whose (tenant, user,
// session) matches id, regardless of run or kind. Identity-scoped and
// idempotent: an absent scope affects zero rows and returns (0, nil).
func (d *driver) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if d.closed.Load() {
		return 0, state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(identity.Quadruple{Identity: id}); err != nil {
		return 0, err
	}

	const q1 = `
		DELETE FROM state_records
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
	`
	res, err := d.db.ExecContext(ctx, q1, id.TenantID, id.UserID, id.SessionID)
	if err != nil {
		return 0, d.translateErr(err, "postgres: delete scope")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, d.translateErr(err, "postgres: delete scope rows affected")
	}
	return int(n), nil
}

func (d *driver) ListKind(ctx context.Context, scope state.ListScope, kindPrefix string) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKind(scope, kindPrefix); err != nil {
		return nil, err
	}

	const q1 = `
		SELECT tenant_id, user_id, session_id, run_id, kind, event_id, version, bytes, updated_at
		FROM state_records
		WHERE kind LIKE $1 ESCAPE '\'
	`
	rows, err := d.db.QueryContext(ctx, q1, escapeLikePrefix(kindPrefix)+"%")
	if err != nil {
		return nil, d.translateErr(err, "postgres: list kind")
	}
	defer rows.Close()

	var out []state.StateRecord
	for rows.Next() {
		var (
			tenantID, userID, sessionID, runID, kind, eventID string
			version                                           int
			buf                                               []byte
			updatedAt                                         time.Time
		)
		if err := rows.Scan(&tenantID, &userID, &sessionID, &runID, &kind, &eventID, &version, &buf, &updatedAt); err != nil {
			return nil, d.translateErr(err, "postgres: list kind scan")
		}
		out = append(out, state.StateRecord{
			ID: state.EventID(eventID),
			Identity: identity.Quadruple{
				Identity: identity.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID},
				RunID:    runID,
			},
			Kind:      kind,
			Version:   version,
			Bytes:     buf,
			UpdatedAt: updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, d.translateErr(err, "postgres: list kind rows")
	}
	return out, nil
}

// ListKindForIdentity implements StateStore's identity-scoped enumeration.
func (d *driver) ListKindForIdentity(ctx context.Context, id identity.Quadruple, kindPrefix string) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKindForIdentity(id, kindPrefix); err != nil {
		return nil, err
	}
	const q1 = `
		SELECT tenant_id, user_id, session_id, run_id, kind, event_id, version, bytes, updated_at
		FROM state_records
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4
		  AND kind LIKE $5 ESCAPE '\'
	`
	rows, err := d.db.QueryContext(ctx, q1, id.TenantID, id.UserID, id.SessionID, id.RunID, escapeLikePrefix(kindPrefix)+"%")
	if err != nil {
		return nil, d.translateErr(err, "postgres: list kind for identity")
	}
	defer rows.Close()
	var out []state.StateRecord
	for rows.Next() {
		var tenantID, userID, sessionID, runID, kind, eventID string
		var version int
		var buf []byte
		var updatedAt time.Time
		if err := rows.Scan(&tenantID, &userID, &sessionID, &runID, &kind, &eventID, &version, &buf, &updatedAt); err != nil {
			return nil, d.translateErr(err, "postgres: list kind for identity scan")
		}
		out = append(out, state.StateRecord{ID: state.EventID(eventID), Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID}, RunID: runID}, Kind: kind, Version: version, Bytes: buf, UpdatedAt: updatedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, d.translateErr(err, "postgres: list kind for identity rows")
	}
	return out, nil
}

// escapeLikePrefix escapes the SQL LIKE metacharacters (`%`, `_`, and
// the escape character itself) so a caller-supplied kind prefix
// matches literally under `LIKE $1 ESCAPE '\'`.
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Close implements state.StateStore. Idempotent — a second call is a
// no-op and returns nil. The atomic flag is set BEFORE db.Close() so
// concurrent in-flight calls fast-fail at the entry guard with
// ErrStoreClosed instead of racing on a closed *sql.DB.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("postgres: db.Close: %w", err)
	}
	return nil
}

// loadByEventIDTx is the in-transaction version of LoadByEventID,
// used by Save's idempotency probe. It returns (record, true, nil)
// on hit, (zero, false, nil) on miss, and (zero, false, err) on
// driver error.
func loadByEventIDTx(ctx context.Context, tx *sql.Tx, eventID state.EventID) (state.StateRecord, bool, error) {
	const q1 = `
		SELECT tenant_id, user_id, session_id, run_id, kind, version, bytes, updated_at
		FROM state_records
		WHERE event_id = $1
	`
	row := tx.QueryRowContext(ctx, q1, string(eventID))
	var (
		tenantID, userID, sessionID, runID, kind string
		version                                  int
		buf                                      []byte
		updatedAt                                time.Time
	)
	if err := row.Scan(&tenantID, &userID, &sessionID, &runID, &kind, &version, &buf, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state.StateRecord{}, false, nil
		}
		return state.StateRecord{}, false, fmt.Errorf("postgres: idempotency probe: %w", err)
	}
	return state.StateRecord{
		ID: eventID,
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID},
			RunID:    runID,
		},
		Kind:      kind,
		Version:   version,
		Bytes:     buf,
		UpdatedAt: updatedAt,
	}, true, nil
}

// translateErr maps low-level driver errors to Harbor sentinels at the
// boundary. Callers compare via errors.Is against state.ErrXxx; raw
// pgx errors must never leak.
//
// Currently: a closed *sql.DB surfaces as a non-typed error from
// database/sql; once Close has set the atomic flag, callers will hit
// the entry guard before reaching here. This helper exists for the
// tiny race window where Close runs between the entry-guard check
// and the actual query.
func (d *driver) translateErr(err error, ctxMsg string) error {
	if err == nil {
		return nil
	}
	// If we have already closed (or are racing with Close), surface
	// ErrStoreClosed so the caller sees the canonical sentinel.
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("%s: %w", ctxMsg, state.ErrStoreClosed)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
}

// translateUpsertErr maps Postgres-specific UPSERT errors to Harbor
// sentinels. The most relevant case is unique_violation on event_id
// — that means a different slot already owns the EventID, which is
// an idempotency conflict at the routing layer.
func (d *driver) translateUpsertErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return fmt.Errorf("%w: event_id collides with a different slot: %v",
				state.ErrIdempotencyConflict, pgErr.Message)
		case pgDeadlockFound:
			// Retry policy lives upstream; surface as a generic error
			// wrapped with context. Don't mask deadlocks as success.
			return fmt.Errorf("postgres: upsert deadlock: %w", err)
		}
	}
	return d.translateErr(err, "postgres: upsert")
}

// bytesEqual is a local helper for byte-slice equality. We avoid
// `bytes.Equal` to keep the dependency surface tight in this file —
// callers already get `bytes` indirectly via stdlib.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
