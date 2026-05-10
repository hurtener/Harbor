// Package sqlite is Harbor's SQLite-backed `state.StateStore` driver
// (Phase 15). It is the second leg of the persistence triad
// (in-memory, SQLite, Postgres) defined by RFC §6.11 + §9.
//
// The driver is built on `modernc.org/sqlite` — a CGo-free SQLite
// engine (D-013, AGENTS.md §5). Builds remain `CGO_ENABLED=0`.
//
// Operating model:
//
//   - Database opened against `cfg.DSN`. Bare file paths and the
//     special `:memory:` sentinel are supported. URI-form DSNs
//     (`file:foo.db?...`) are passed through verbatim by the driver
//     but Harbor sets `journal_mode=WAL` and `busy_timeout=5000`
//     itself via PRAGMA after open, so URI-form DSN parameters are
//     not blessed in V1.
//   - WAL journal mode is pinned at open. WAL gives concurrent
//     readers + a single writer with no `SQLITE_BUSY` storms in the
//     read path.
//   - `busy_timeout=5000` (5 s) absorbs `SQLITE_BUSY` retries
//     transparently — write contention in the conformance suite's
//     `Concurrent_SaveLoad_NoRace` and our supplemental
//     `concurrent_test.go` does not surface caller-visible errors.
//   - The schema is applied via embedded `migrations/*.sql` files
//     (forward-only, brief 05 §4 + AGENTS.md §13). The runner is
//     idempotent — re-running on an already-migrated DB is a no-op.
//
// The driver self-registers under `"sqlite"` from its `init()`. The
// production binary picks it up via blank import in
// `cmd/harbor/main.go`; tests may call `New` directly to skip the
// registry.
//
// Concurrency contract (D-025):
//
//   - The driver struct holds a `*sql.DB` (an internally-synchronized
//     connection pool) and an `atomic.Bool` close flag. Both are safe
//     for N concurrent goroutines without external locking.
//   - Per-call state lives on the call stack / supplied `ctx`. Nothing
//     mutable on the driver ever crosses run boundaries.
//   - SQLite's single-writer-per-database invariant is enforced by
//     the engine; `busy_timeout` ensures concurrent writers serialize
//     transparently. No advisory locks needed.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	// modernc.org/sqlite registers the "sqlite" driver name with
	// database/sql via its own init(). Blank-importing it here is the
	// idiomatic way to make `sql.Open("sqlite", dsn)` work.
	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// driverName is the name registered with database/sql by
// `modernc.org/sqlite`. We do not alias it; passing the same string to
// `sql.Open` keeps the seam between Harbor's driver name (also
// `"sqlite"`) and the database/sql driver name purely cosmetic.
const driverName = "sqlite"

// busyTimeoutMs is the PRAGMA busy_timeout value pinned at open. 5 s
// is reasonable for single-binary deployments with light concurrency
// (RFC §10 stack-decisions principle — V1 simplicity over an extra
// config knob). Operators with heavy contention can either move to
// the Postgres driver or adjust this constant in a follow-up phase.
const busyTimeoutMs = 5000

// New constructs a SQLite-backed `state.StateStore` against
// `cfg.DSN`. Production callers go through `state.Open`; tests may
// call `New` directly to skip the registry.
//
// DSN handling:
//
//   - Empty DSN → clear error (no silent default-fallback).
//   - `:memory:` → translated to the URI form
//     `file::memory:?cache=shared` so `database/sql`'s pool can hand
//     out multiple shared connections to the SAME in-memory database
//     (the bare `:memory:` DSN gives every pool connection its own
//     private DB, which would break Save+Load round-trip across
//     pooled connections).
//   - Any other DSN is treated as a file path or URI form and passed
//     through verbatim, with the WAL + busy_timeout PRAGMAs appended
//     as `_pragma` query params (see below).
//
// PRAGMA discipline:
//
//   - `journal_mode=WAL` and `busy_timeout=5000` are appended to the
//     DSN as `_pragma=...` query params. modernc.org/sqlite applies
//     them on every new connection the pool opens, so the values
//     survive `database/sql`'s connection lifecycle. This is the
//     load-bearing fix for SQLITE_BUSY contention under concurrent
//     writes — running `PRAGMA busy_timeout` once on the *sql.DB
//     handle would only set it on the single connection the call
//     happened to use.
//   - For disk-backed DSNs WAL is required; `:memory:` falls back to
//     `memory` journal mode, which is correct (see SQLite docs).
//
// Errors:
//
//   - empty `cfg.DSN`
//   - DSN that cannot be parsed for `_pragma` augmentation
//   - `sql.Open` failure (rare; modernc.org/sqlite's Open is lazy)
//   - migration apply failure
func New(cfg config.StateConfig) (state.StateStore, error) {
	if cfg.DSN == "" {
		return nil, errors.New(`state/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("state/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("state/sqlite: sql.Open(%q): %w", cfg.DSN, err)
	}

	// SQLite is a single-writer engine. Even with WAL +
	// `_txlock=immediate` + `busy_timeout`, a multi-connection pool
	// generates SQLITE_BUSY at BEGIN IMMEDIATE under high contention
	// (the busy handler runs inside the C engine, but `database/sql`
	// can hand out N connections that race for the writer lock).
	// Pinning the pool to a single connection serializes all access
	// at the Go layer — the driver thus matches the engine's
	// single-writer reality. This deviates from the Phase 15 plan's
	// "leave pool defaulted" guidance (AGENTS.md §4.3 reasonable
	// deviation): the conformance suite's
	// `Concurrent_SaveLoad_NoRace` (N=128) does not pass with the
	// default pool because BEGIN IMMEDIATE doesn't honor busy_timeout
	// for inter-connection writer contention.
	db.SetMaxOpenConns(1)

	// Use a bounded context for the open-time validation + migrations
	// so a wedged file doesn't hang construction forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := verifyJournalMode(ctx, db, cfg.DSN); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state/sqlite: migrate: %w", err)
	}

	return &driver{db: db}, nil
}

func init() {
	state.Register("sqlite", New)
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them
// to every new connection the pool opens.
//
// What gets added:
//
//   - `_pragma=busy_timeout(5000)`  — absorbs SQLITE_BUSY retries
//     transparently across the whole pool.
//   - `_pragma=journal_mode(WAL)`   — concurrent readers + single
//     writer per RFC §6.11. Disk-backed only (memory DBs degrade
//     silently to `memory` mode by SQLite design).
//   - `_txlock=immediate`           — every Begin acquires the
//     RESERVED lock up-front instead of deferring until the first
//     write, eliminating the SQLITE_BUSY_SNAPSHOT (517) errors that
//     otherwise surface when two transactions started as readers
//     race to upgrade. Without this, busy_timeout cannot help —
//     SQLite returns SQLITE_BUSY_SNAPSHOT immediately because the
//     conflict is logical, not physical.
//
// The bare `:memory:` DSN is translated to a `file:`-form shared-cache
// URI so `database/sql`'s pool can hand out multiple connections to
// the same in-memory database (the bare `:memory:` sentinel gives
// every pool connection its OWN private database, which would break
// Save+Load round-trip across pooled connections).
//
// Documented behavior (DSN format, RFC §10 stack-decisions): bare
// file paths and the `:memory:` sentinel are the V1 supported inputs.
// Operators who want richer URI forms can supply them directly; we
// only add to whatever query string is present.
func augmentDSNForPragmas(dsn string) (string, error) {
	// Translate bare `:memory:` to a shared-cache file: URI so the
	// pool sees the same DB across connections.
	if dsn == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}

	pragmas := []string{
		"busy_timeout(" + fmt.Sprint(busyTimeoutMs) + ")",
		"journal_mode(WAL)",
	}

	// Determine the input shape. modernc.org/sqlite supports:
	//   1. bare file path: "/var/lib/harbor/state.sqlite"
	//   2. file: URI:      "file:/var/lib/harbor/state.sqlite?cache=shared"
	// We need to append `_pragma` + `_txlock` query params in both
	// cases.
	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file: URI: %w", err)
		}
		q := u.Query()
		for _, p := range pragmas {
			q.Add("_pragma", p)
		}
		if q.Get("_txlock") == "" {
			q.Set("_txlock", "immediate")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	// Bare file path. We don't expect a `?` in a normal POSIX path,
	// but we tolerate it: the substring after the first `?` is
	// treated as an existing query string.
	sep := "?"
	if idx := strings.IndexByte(dsn, '?'); idx >= 0 {
		sep = "&"
	}
	parts := make([]string, 0, len(pragmas)+1)
	for _, p := range pragmas {
		parts = append(parts, "_pragma="+url.QueryEscape(p))
	}
	parts = append(parts, "_txlock=immediate")
	return dsn + sep + strings.Join(parts, "&"), nil
}

// verifyJournalMode reads back the journal mode after open to
// confirm the per-connection PRAGMA actually took effect. Disk-backed
// DSNs MUST report `wal`; `:memory:` (and shared-cache memory DSNs)
// degrade to `memory` mode by design.
func verifyJournalMode(ctx context.Context, db *sql.DB, originalDSN string) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("state/sqlite: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		// Memory DBs degrade to "memory" journal mode — accepted.
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("state/sqlite: journal_mode=%q after open; expected \"wal\" (DSN=%q)",
			mode, originalDSN)
	}
	return nil
}

// isMemoryDSN reports whether the caller-supplied DSN routes to an
// in-memory database (no disk-backed file). The sentinel `:memory:`
// and any `file:` URI containing the `:memory:` host both qualify.
func isMemoryDSN(dsn string) bool {
	if dsn == ":memory:" {
		return true
	}
	if strings.HasPrefix(dsn, "file:") && strings.Contains(dsn, ":memory:") {
		return true
	}
	return false
}

// driver is the SQLite-backed StateStore. It is safe for concurrent
// use by N goroutines; mutable state is the `atomic.Bool` close flag
// (load-then-act pattern) and the underlying `*sql.DB` (internally
// synchronized by database/sql).
type driver struct {
	db     *sql.DB
	closed atomic.Bool
}

// Compile-time assertion that driver satisfies state.StateStore.
var _ state.StateStore = (*driver)(nil)

// Save implements state.StateStore.
//
// SQL flow (single transaction, brief 05 §4 idempotency contract):
//
//  1. Look up the row at the slot key (composite PK) AND/OR the row
//     resolved by EventID. If the EventID was previously seen:
//     - same slot + identical bytes/version → no-op
//     - any divergence → ErrIdempotencyConflict
//  2. Otherwise UPSERT (`INSERT INTO ... ON CONFLICT(...) DO UPDATE`).
//     The unique index on `event_id` ensures the new EventID's slot
//     is the only place it lives; if an older record at the same slot
//     existed under a different EventID, the UPSERT replaces it
//     (the old EventID is no longer LoadByEventID-resolvable, which
//     matches the InMem driver's "evicted" semantics).
func (d *driver) Save(ctx context.Context, r state.StateRecord) error {
	if d.closed.Load() {
		return fmt.Errorf("state/sqlite: %w", state.ErrStoreClosed)
	}
	if err := state.ValidateRecord(r); err != nil {
		return err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state/sqlite: begin tx: %w", err)
	}
	rolled := false
	defer func() {
		if !rolled {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()

	// Idempotency precheck — does this EventID already exist?
	prevSlot, prevBytes, prevVersion, prevExists, err := lookupByEventID(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	if prevExists {
		newSlot := slotKey{
			Tenant:  r.Identity.TenantID,
			User:    r.Identity.UserID,
			Session: r.Identity.SessionID,
			Run:     r.Identity.RunID,
			Kind:    r.Kind,
		}
		if prevSlot != newSlot {
			return fmt.Errorf("%w: EventID %q already routes to a different (Quadruple, Kind)",
				state.ErrIdempotencyConflict, r.ID)
		}
		if !bytes.Equal(prevBytes, r.Bytes) {
			return fmt.Errorf("%w: EventID %q already saved with different Bytes",
				state.ErrIdempotencyConflict, r.ID)
		}
		if prevVersion != r.Version {
			return fmt.Errorf("%w: EventID %q already saved with different Version",
				state.ErrIdempotencyConflict, r.ID)
		}
		// Idempotent no-op — commit the (empty) tx so we don't leak
		// the open transaction handle.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state/sqlite: commit (idempotent no-op): %w", err)
		}
		rolled = true
		return nil
	}

	updatedAt := r.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	// UPSERT on the composite primary key. The unique index on
	// `event_id` enforces global uniqueness; if the new record's slot
	// overlaps an existing row whose EventID differs, the ON CONFLICT
	// path overwrites all columns including event_id (the previous
	// EventID is evicted). Because that eviction collides with the
	// unique index ONLY when both rows targeted the same EventID, and
	// we already returned ErrIdempotencyConflict in that case above,
	// the upsert is safe.
	const upsert = `
        INSERT INTO state_records
            (tenant, user, session, run, kind, event_id, version, bytes, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, session, run, kind) DO UPDATE SET
            event_id   = excluded.event_id,
            version    = excluded.version,
            bytes      = excluded.bytes,
            updated_at = excluded.updated_at`
	if _, err := tx.ExecContext(ctx, upsert,
		r.Identity.TenantID,
		r.Identity.UserID,
		r.Identity.SessionID,
		r.Identity.RunID,
		r.Kind,
		string(r.ID),
		r.Version,
		r.Bytes,
		updatedAt,
	); err != nil {
		return fmt.Errorf("state/sqlite: upsert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state/sqlite: commit: %w", err)
	}
	rolled = true
	return nil
}

// slotKey mirrors `internal/state/drivers/inmem`'s indexKey: a
// struct-typed composite primary key that cannot be confused by
// delimiters in tenant / user / session strings.
type slotKey struct {
	Tenant, User, Session, Run, Kind string
}

// lookupByEventID returns the slot, bytes, and version for the row
// whose `event_id` column equals eventID. The boolean `found` is true
// iff the row exists. Errors propagate verbatim; a missing row is
// `found=false, err=nil`.
func lookupByEventID(ctx context.Context, tx *sql.Tx, eventID state.EventID) (slotKey, []byte, int, bool, error) {
	const q = `
        SELECT tenant, user, session, run, kind, version, bytes
        FROM state_records
        WHERE event_id = ?
        LIMIT 1`
	row := tx.QueryRowContext(ctx, q, string(eventID))
	var slot slotKey
	var version int
	var data []byte
	if err := row.Scan(&slot.Tenant, &slot.User, &slot.Session, &slot.Run, &slot.Kind, &version, &data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return slotKey{}, nil, 0, false, nil
		}
		return slotKey{}, nil, 0, false, fmt.Errorf("state/sqlite: lookup by event_id: %w", err)
	}
	return slot, data, version, true, nil
}

// Load implements state.StateStore.
func (d *driver) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, fmt.Errorf("state/sqlite: %w", state.ErrStoreClosed)
	}
	if err := state.ValidateIdentity(q); err != nil {
		return state.StateRecord{}, err
	}
	if kind == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	const sel = `
        SELECT event_id, version, bytes, updated_at
        FROM state_records
        WHERE tenant = ? AND user = ? AND session = ? AND run = ? AND kind = ?
        LIMIT 1`
	row := d.db.QueryRowContext(ctx, sel,
		q.TenantID, q.UserID, q.SessionID, q.RunID, kind)

	var eventID string
	var version int
	var data []byte
	var updatedAt time.Time
	if err := row.Scan(&eventID, &version, &data, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state.StateRecord{}, fmt.Errorf("%w: %s/%s/%s/%s kind=%s",
				state.ErrNotFound, q.TenantID, q.UserID, q.SessionID, q.RunID, kind)
		}
		return state.StateRecord{}, fmt.Errorf("state/sqlite: load: %w", err)
	}

	return state.StateRecord{
		ID:        state.EventID(eventID),
		Identity:  q,
		Kind:      kind,
		Version:   version,
		Bytes:     data,
		UpdatedAt: updatedAt,
	}, nil
}

// LoadByEventID implements state.StateStore.
func (d *driver) LoadByEventID(ctx context.Context, eventID state.EventID) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, fmt.Errorf("state/sqlite: %w", state.ErrStoreClosed)
	}
	if eventID == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	const sel = `
        SELECT tenant, user, session, run, kind, version, bytes, updated_at
        FROM state_records
        WHERE event_id = ?
        LIMIT 1`
	row := d.db.QueryRowContext(ctx, sel, string(eventID))

	var rec state.StateRecord
	var tenant, user, session, run, kind string
	var version int
	var data []byte
	var updatedAt time.Time
	if err := row.Scan(&tenant, &user, &session, &run, &kind, &version, &data, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state.StateRecord{}, fmt.Errorf("%w: event_id=%s", state.ErrNotFound, eventID)
		}
		return state.StateRecord{}, fmt.Errorf("state/sqlite: load by event_id: %w", err)
	}
	rec.ID = eventID
	rec.Identity = identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    user,
			SessionID: session,
		},
		RunID: run,
	}
	rec.Kind = kind
	rec.Version = version
	rec.Bytes = data
	rec.UpdatedAt = updatedAt
	return rec, nil
}

// Delete implements state.StateStore. Idempotent — `DELETE` against
// a missing row returns nil per the InMem reference. The unique index
// on `event_id` is part of the same row, so deletion is atomic for
// both the slot key and the secondary-by-EventID lookup.
func (d *driver) Delete(ctx context.Context, q identity.Quadruple, kind string) error {
	if d.closed.Load() {
		return fmt.Errorf("state/sqlite: %w", state.ErrStoreClosed)
	}
	if err := state.ValidateIdentity(q); err != nil {
		return err
	}
	if kind == "" {
		return state.ErrInvalidRecord
	}

	const del = `
        DELETE FROM state_records
        WHERE tenant = ? AND user = ? AND session = ? AND run = ? AND kind = ?`
	if _, err := d.db.ExecContext(ctx, del,
		q.TenantID, q.UserID, q.SessionID, q.RunID, kind); err != nil {
		return fmt.Errorf("state/sqlite: delete: %w", err)
	}
	return nil
}

// Close implements state.StateStore. Setting the atomic flag BEFORE
// `db.Close()` ensures concurrent in-flight callers observe
// `ErrStoreClosed` rather than racing into a half-closed pool. Close
// is idempotent — repeat calls are safe and return nil.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("state/sqlite: close: %w", err)
	}
	return nil
}
