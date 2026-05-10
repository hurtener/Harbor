// Package postgres is Harbor's V1 Postgres-backed StateStore driver.
//
// It is the multi-node production target for the §9 persistence
// triad: the third leg, alongside the in-memory reference (Phase 07)
// and the SQLite driver (Phase 15). Phase 16 inherits
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
// Per AGENTS.md §5 (D-025), the driver is safe for concurrent reuse
// across N goroutines. The conformance suite's `Concurrent_SaveLoad_NoRace`
// + the local `concurrent_test.go` enforce this under -race.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// lives in a future Phase 16.1 / config knob, not here.
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
// `closed` flag (D-025: compiled artifacts are immutable; per-run
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
		r.Kind, string(r.ID), r.Version, r.Bytes, updatedAt,
	); err != nil {
		return d.translateUpsertErr(err)
	}

	if err := tx.Commit(); err != nil {
		return d.translateErr(err, "postgres: commit upsert")
	}
	committed = true
	return nil
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
