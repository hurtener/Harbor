// Package sqlite is Harbor's SQLite-backed `turns.Store` driver — the
// durable leg of the turn projection persistence triad (in-memory
// floor, SQLite, Postgres) behind the `sessions.turns.list` /
// `sessions.turns.get` Protocol surface.
//
// The driver is built on `modernc.org/sqlite` — a CGo-free SQLite
// engine (AGENTS.md §5). Builds remain `CGO_ENABLED=0`.
//
// # Operating model
//
//   - Database opened against `cfg.DSN`. Bare file paths and the
//     special `:memory:` sentinel are supported. URI-form DSNs
//     (`file:foo.db?...`) are passed through verbatim with the WAL +
//     busy_timeout + immediate-transaction PRAGMAs appended as
//     `_pragma` / `_txlock` query params so every pooled connection
//     sees the same per-connection settings (see augmentDSNForPragmas).
//   - WAL journal mode is pinned at open (disk-backed). WAL gives
//     concurrent readers + a single writer with no `SQLITE_BUSY`
//     storms in the read path; `busy_timeout=5000` absorbs `SQLITE_BUSY`
//     retries transparently; `_txlock=immediate` acquires the write
//     lock at BEGIN so two transactions can never race a stale read.
//   - The pool is pinned to a SINGLE connection (`SetMaxOpenConns(1)`),
//     matching SQLite's single-writer reality and the settled choice of
//     the StateStore + ArtifactStore SQLite drivers: `database/sql`
//     serializes concurrent callers at the pool instead of surfacing
//     SQLITE_BUSY at BEGIN IMMEDIATE under contention.
//   - The schema is applied via embedded `migrations/*.sql` files
//     (forward-only, AGENTS.md §13) through the shared runner
//     (`internal/persistence/sqlmigrate`). Re-running on an
//     already-migrated DB is a no-op.
//
// # Durable indexed parity
//
// The schema is built for INDEXED access on every axis the store
// contract reads, never a scan:
//
//   - `turn_rows` is keyed by the EXACT isolation triple + the root
//     foreground turn key (TurnID = the run's task id): indexed
//     append-idempotency lookup and indexed get are primary-key
//     probes.
//   - The (tenant, user, session, sequence, turn_id) keyset index is
//     the paging backbone: every `ListTurns` page is a bounded index
//     RANGE scan strictly older than the cursor, ordered newest-first
//     — no OFFSET, no history scan. The per-session `COUNT(*)` of
//     older retained rows (the exact Remaining field) rides the same
//     index range.
//   - `turn_apps` is keyed by the exact App replacement identity
//     (effective_agent_id, server_id, resource_uri) within the turn
//     (the effective-agent + session axis), with `position` preserving
//     first-declaration order on the ordered read.
//   - The agent axis (a session's turns under one effective agent) is
//     indexed on `turn_rows` as derived metadata written from the DTO
//     at every accepted write.
//
// Every row write (append / update / seal) runs in ONE transaction
// that covers the row + its children (activity rows, App refs) +
// the per-session sequence mint, and every write is fenced against
// session erasure in that same transaction: `turn_fences` is the
// STORE-LOCAL durable erasure fence / tombstone, and `DeleteScope`
// deletes the projections (rows, children, checkpoint, sequence
// state) but NEVER the fence — an erased session stays fenced across
// replay and restart, so replay can never resurrect it. The projection
// snapshot generation (`turn_snapshot_gens`) also survives erasure and
// advances with it, so a cursor minted before an erase is rejected as
// stale. `Durable()` reports whether the backing DSN survives a
// process restart (file-backed true; `:memory:` false — explicit
// restart loss, never a silent claim).
//
// # Concurrency contract
//
// The driver struct holds a `*sql.DB` (an internally-synchronized pool
// pinned to one connection), an `atomic.Bool` close flag, and
// immutable configuration. It is safe for N concurrent goroutines
// without external locking: per-call state lives on the call stack /
// supplied `ctx`, the deep-copy obligation is satisfied by JSON
// marshaling on every write boundary and unmarshaling on every read
// boundary (caller memory never reaches or escapes durable state), and
// no mutable field on the driver ever crosses run boundaries.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// driverName is the name registered with database/sql by
// `modernc.org/sqlite`. We do not alias it; passing the same string to
// `sql.Open` keeps the seam between Harbor's driver name (also
// `"sqlite"`) and the database/sql driver name purely cosmetic.
const driverName = "sqlite"

// busyTimeoutMs is the PRAGMA busy_timeout value pinned at open. 5 s
// is reasonable for single-binary deployments with light concurrency
// (RFC §10 stack-decisions principle — V1 simplicity over an extra
// config knob).
const busyTimeoutMs = 5000

// Config configures the SQLite-backed `turns.Store`.
type Config struct {
	// DSN is the SQLite database path ("/var/lib/harbor/turns.sqlite"),
	// a `file:` URI form, or the ":memory:" sentinel. Empty fails
	// loudly (no silent default-fallback).
	DSN string
	// Retention bounds the number of NEWEST turn rows each session
	// retains: older rows are evicted (children cascade in the same
	// transaction) and the session's explicit truncation flag is set —
	// retention eviction is never silent (AGENTS.md §13). <= 0 means
	// the documented projection default (turns.MaxRetainedTurns).
	Retention int
}

// New constructs a SQLite-backed `turns.Store` against cfg.DSN.
//
// DSN handling:
//
//   - Empty DSN → clear error.
//   - `:memory:` → translated to a PER-OPEN uniquely named shared-cache
//     memory URI (`file:harbor_turns_mem_<entropy>?mode=memory&cache=shared`)
//     so `database/sql`'s pool can hand out multiple connections to the
//     SAME in-memory database while two `:memory:` stores opened by
//     different subsystems stay fully isolated.
//   - Any other DSN is treated as a file path or URI form and passed
//     through verbatim, with the WAL + busy_timeout + immediate-tx
//     PRAGMAs appended as query params.
//
// Errors: empty DSN, unparseable DSN, journal-mode verification
// failure (disk-backed DBs must be WAL), or a migration-apply failure.
func New(cfg Config) (turns.Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New(`sessions/turns/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: sql.Open(%q): %w", cfg.DSN, err)
	}

	// SQLite is a single-writer engine. Even with WAL + `_txlock=immediate`
	// + `busy_timeout`, a multi-connection pool generates SQLITE_BUSY at
	// BEGIN IMMEDIATE under high contention (the busy handler runs inside
	// the engine, but database/sql can hand out N connections that race
	// for the writer lock). Pinning the pool to a single connection
	// serializes all access at the Go layer — the driver thus matches the
	// engine's single-writer reality (settled choice of the StateStore +
	// ArtifactStore SQLite drivers).
	db.SetMaxOpenConns(1)

	// Use a bounded context for the open-time validation + migrations so
	// a wedged file doesn't hang construction forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := verifyJournalMode(ctx, db, cfg.DSN); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sessions/turns/sqlite: migrate: %w", err)
	}

	retention := cfg.Retention
	if retention <= 0 {
		retention = turns.MaxRetainedTurns
	}
	return &driver{
		db:      db,
		retain:  retention,
		durable: !isMemoryDSN(cfg.DSN),
	}, nil
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them
// to every new connection the pool opens.
//
// What gets added:
//
//   - `_pragma=busy_timeout(5000)` — absorbs SQLITE_BUSY retries
//     transparently across the whole pool.
//   - `_pragma=journal_mode(WAL)` — concurrent readers + single writer.
//     Disk-backed only (memory DBs degrade silently to `memory` mode by
//     SQLite design).
//   - `_txlock=immediate` — every Begin acquires the RESERVED lock
//     up-front instead of deferring until the first write, eliminating
//     the SQLITE_BUSY_SNAPSHOT (517) errors that otherwise surface when
//     two transactions started as readers race to upgrade.
//
// The bare `:memory:` DSN is translated to a PER-OPEN uniquely named
// `file:`-form shared-cache memory URI (see uniqueMemoryDSN) so
// `database/sql`'s pool shares one DB within this store while two
// `:memory:` stores opened by different subsystems never collide.
func augmentDSNForPragmas(dsn string) (string, error) {
	// Translate bare `:memory:` to a per-Open uniquely named
	// shared-cache memory URI: shared across the pool, isolated
	// across Opens.
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

	// Determine the input shape. modernc.org/sqlite supports:
	//   1. bare file path: "/var/lib/harbor/turns.sqlite"
	//   2. file: URI:      "file:/var/lib/harbor/turns.sqlite?cache=shared"
	// We need to append `_pragma` + `_txlock` query params in both cases.
	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file: URI: %w", err)
		}
		q := u.Query()
		for _, p := range pragmas {
			q.Add("_pragma", p)
		}
		if len(q["_txlock"]) == 0 {
			q.Set("_txlock", "immediate")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	// Bare file path. We don't expect a `?` in a normal POSIX path,
	// but preserve the historic treatment of a suffix as query
	// parameters so URI-form DSNs keep their own parameters.
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

// uniqueMemoryDSN mints a per-Open named in-memory database URI.
// `mode=memory` keeps it off disk; `cache=shared` lets every
// connection in THIS store's pool see the same database; the
// crypto-random name keeps two `:memory:` stores — this subsystem's or
// any other's — fully isolated within one process. The database lives
// as long as the pool's single pinned connection holds it open; the
// store's lifetime bounds the data's.
func uniqueMemoryDSN() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("sessions/turns/sqlite: memory-DSN entropy: %w", err)
	}
	return "file:harbor_turns_mem_" + hex.EncodeToString(entropy[:]) + "?mode=memory&cache=shared", nil
}

// verifyJournalMode reads back the journal mode after open to confirm
// the per-connection PRAGMA actually took effect. Disk-backed DSNs
// MUST report `wal`; `:memory:` (and shared-cache memory DSNs) degrade
// to `memory` mode by design.
func verifyJournalMode(ctx context.Context, db *sql.DB, originalDSN string) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		// Memory DBs degrade to "memory" journal mode — accepted.
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("sessions/turns/sqlite: journal_mode=%q after open; expected \"wal\" (DSN=%q)",
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

// driver is the SQLite-backed turns.Store. It is safe for concurrent
// use by N goroutines: mutable state is the `atomic.Bool` close flag
// (load-then-act pattern) and the underlying `*sql.DB` (internally
// synchronized by database/sql, pinned to one connection). `retain` /
// `durable` are immutable after construction.
type driver struct {
	db      *sql.DB
	closed  atomic.Bool
	retain  int
	durable bool
}

// Compile-time assertion that driver satisfies turns.Store.
var _ turns.Store = (*driver)(nil)

// Durable implements turns.Store: file-backed stores survive a process
// restart; `:memory:` stores report false (explicit restart loss —
// rows, checkpoint, and erasure fences all vanish, so the projection
// rebuilds from sequence zero gated on the runtime's durable erasure
// probe).
func (d *driver) Durable() bool { return d.durable }

// AppendTurnIf implements turns.Store.
//
// One transaction covers the STORE-LOCAL erasure-fence check, the
// idempotency lookup, the per-session sequence mint, the row + child
// (activity / Apps) write, and the retention eviction. Idempotent on
// the turn id: an existing row is returned unchanged (a replay no-op).
func (d *driver) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	if d.closed.Load() {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	if row.TurnID == "" {
		return turns.TurnRow{}, fmt.Errorf("%w: empty turn id", turns.ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: begin append tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()

	// 1. STORE-LOCAL erasure fence first, always: an erased session
	//    admits no turn write (checked in the same tx as the write).
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return turns.TurnRow{}, err
	}
	if fenced {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}

	// 2. Idempotent append: an existing row returns unchanged (a
	//    replay of an already-applied append is a no-op, never an
	//    error and never an overwrite).
	if existing, found, err := loadTurn(ctx, tx, id, row.TurnID); err != nil {
		return turns.TurnRow{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: commit idempotent append: %w", err)
		}
		committed = true
		return existing, nil
	}

	// 3. Mint the next immutable per-session sequence atomically with
	//    the write (the single-writer serialization + this tx make the
	//    read-then-update race-free).
	seq, err := mintSeq(ctx, tx, id)
	if err != nil {
		return turns.TurnRow{}, err
	}

	// 4. Build the stored row: the store owns Sequence / TieBreaker /
	//    Version and the session denormalization; everything else is
	//    the caller's renderable DTO.
	row.Sequence = seq
	row.TieBreaker = row.TurnID
	row.SessionID = id.SessionID
	row.Sealed = false
	row.Version = 1
	if err := writeRowAndChildren(ctx, tx, id, row); err != nil {
		return turns.TurnRow{}, err
	}

	// 5. Retention: evict the session's oldest rows past the bound;
	//    the eviction is surfaced as the session's explicit truncation
	//    flag (never silent).
	if err := d.enforceRetention(ctx, tx, id, row.Sequence); err != nil {
		return turns.TurnRow{}, err
	}

	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: commit append: %w", err)
	}
	committed = true

	// Deep-copy the stored row back to the caller. The stored bytes
	// are exactly the marshal of `row` (writeRowAndChildren), so a
	// JSON round-trip is byte-identical to a reload — and fresh memory,
	// so caller mutation can never alias durable state.
	return cloneRowViaJSON(row)
}

// UpdateTurnIf implements turns.Store: atomically replaces a MUTABLE
// row at an expected version (row + children in one transaction).
func (d *driver) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	return d.mutate(ctx, id, turnID, expectedVersion, row, false)
}

// SealTurnIf implements turns.Store: atomically replaces a MUTABLE row
// with its SEALED terminal form (row + children in one transaction).
// Sealed rows are immutable thereafter.
func (d *driver) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	return d.mutate(ctx, id, turnID, expectedVersion, row, true)
}

// mutate applies the UpdateTurnIf / SealTurnIf conditional-write
// pattern: fence check, row guard (not found / sealed / stale
// version), row + children write, all in ONE transaction (the
// single-writer serialization makes the guard read and the write
// atomic).
func (d *driver) mutate(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, sealed bool) (turns.TurnRow, error) {
	if d.closed.Load() {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	if turnID == "" {
		return turns.TurnRow{}, fmt.Errorf("%w: empty turn id", turns.ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: begin mutate tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()

	// 1. The store-local erasure fence is checked in the same
	//    transaction as the write (no race between check and write).
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return turns.TurnRow{}, err
	}
	if fenced {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}

	// 2. Guard against the authoritative stored row.
	var curSeq int64
	var curSealed bool
	var curVersion int
	err = tx.QueryRowContext(ctx, guardRowSQL, id.TenantID, id.UserID, id.SessionID, string(turnID)).
		Scan(&curSeq, &curSealed, &curVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: guard read: %w", err)
	}
	if curSealed {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnSealed, turnID)
	}
	if curVersion != expectedVersion {
		return turns.TurnRow{}, fmt.Errorf("%w: stored version %d, expected %d", turns.ErrStaleVersion, curVersion, expectedVersion)
	}

	// 3. Build the stored row: the ordering keys / sequence / tie-break
	//    are IMMUTABLE; the version bumps by exactly one per accepted
	//    write; the caller's remaining renderable DTO is replaced
	//    wholesale.
	next := row
	next.TurnID = turnID
	next.SessionID = id.SessionID
	next.Sequence = turns.Seq(curSeq)
	next.TieBreaker = turnID
	next.Sealed = sealed
	next.Version = curVersion + 1
	if err := writeRowAndChildren(ctx, tx, id, next); err != nil {
		return turns.TurnRow{}, err
	}

	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: commit mutate: %w", err)
	}
	committed = true

	// Deep-copy the stored row back to the caller (see AppendTurnIf:
	// the stored bytes are exactly the marshal of `next`).
	return cloneRowViaJSON(next)
}

// cloneRowViaJSON returns a deep copy of a turn row (fresh memory from
// a JSON round-trip). It is used on every write boundary so a caller
// mutating the returned row — or its input row — can never alias
// durable state (the deep-copy obligation of the store contract).
func cloneRowViaJSON(row turns.TurnRow) (turns.TurnRow, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: clone row %q: %w", row.TurnID, err)
	}
	var out turns.TurnRow
	if err := json.Unmarshal(b, &out); err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: decode cloned row %q: %w", row.TurnID, err)
	}
	return out, nil
}

// FenceSession implements turns.Store: marks id's session as
// ERASURE-FENCED in this driver's own durable backend. The fence is
// the tombstone the erasure cascade sets BEFORE DeleteScope; it is
// never removed by DeleteScope, so an erased session stays fenced
// across replay and restart (no resurrection). Idempotent.
func (d *driver) FenceSession(ctx context.Context, id identity.Identity) error {
	if d.closed.Load() {
		return fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, fenceInsertSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: fence session: %w", err)
	}
	return nil
}

// GetTurn implements turns.Store: reads one retained row by its exact
// (identity triple, turn id) primary key; ErrTurnNotFound when the
// turn was never created, was evicted past the retention bound, or was
// erased. Cross-session turns are not addressable (the identity triple
// is part of the key).
func (d *driver) GetTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	if d.closed.Load() {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	if turnID == "" {
		return turns.TurnRow{}, fmt.Errorf("%w: empty turn id", turns.ErrInvalidInput)
	}
	row, found, err := loadTurn(ctx, d.db, id, turnID)
	if err != nil {
		return turns.TurnRow{}, err
	}
	if !found {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	return row, nil
}

// ListTurns implements turns.Store: one newest-first keyset page of at
// most limit retained rows strictly older than before (nil before =
// the newest page), ordered by (Sequence DESC, TurnID DESC). Every
// page is a bounded index RANGE scan — never an OFFSET over the
// session's history — plus an exact index-range COUNT of the older
// retained rows (Remaining). The cursor is BOUND to its owning session,
// the projection snapshot generation it was minted against, and its
// authoritative boundary row: foreign-session, stale-snapshot,
// expired-boundary, and forged / altered cursors are each refused with
// their distinct domain error.
func (d *driver) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, turns.ListPageInfo, error) {
	var zero turns.ListPageInfo
	if d.closed.Load() {
		return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return nil, nil, zero, turns.ErrIdentityRequired
	}
	if limit < 1 {
		return nil, nil, zero, fmt.Errorf("%w: limit %d", turns.ErrInvalidInput, limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, zero, err
	}

	// The projection snapshot generation (as-of retention generation)
	// this page — and its minted cursors — bind to. Starts at 0 for a
	// fresh session; advances on every erasure.
	snapshot, err := d.loadSnapshot(ctx, id)
	if err != nil {
		return nil, nil, zero, err
	}

	// Opaque-cursor BINDING against the authoritative state.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, zero, fmt.Errorf("%w: cursor names session %q, request is %q",
				turns.ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, zero, fmt.Errorf("%w: cursor snapshot %d, current %d",
				turns.ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		var boundarySeq int64
		err := d.db.QueryRowContext(ctx, boundarySeqSQL, id.TenantID, id.UserID, id.SessionID, string(before.TurnID)).Scan(&boundarySeq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, zero, fmt.Errorf("%w: boundary row %q is no longer retained",
				turns.ErrCursorExpired, before.TurnID)
		}
		if err != nil {
			return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: boundary row lookup: %w", err)
		}
		// The cursor is BOUND to the AUTHORITATIVE boundary row: a
		// forged / altered cursor that names a retained row but carries
		// a sequence that does not equal the stored row's immutable
		// sequence is refused — the keyset filter would otherwise page
		// from a sequence no stored row owns, silently skipping or
		// repeating rows.
		if turns.Seq(boundarySeq) != before.Seq {
			return nil, nil, zero, fmt.Errorf("%w: cursor sequence %d does not match the stored boundary row %q (sequence %d) — forged or altered cursor",
				turns.ErrInvalidCursor, before.Seq, before.TurnID, boundarySeq)
		}
	}

	// Fetch limit+1 candidate rows to know exactly whether older rows
	// remain. Both the newest page and the cursor page are bounded
	// index range scans on (tenant, user, session, sequence, turn_id).
	var q string
	var args []any
	if before != nil {
		q = listPageSQL
		args = []any{id.TenantID, id.UserID, id.SessionID, int64(before.Seq), string(before.TurnID), limit + 1}
	} else {
		q = listPageNewestSQL
		args = []any{id.TenantID, id.UserID, id.SessionID, limit + 1}
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: list page: %w", err)
	}
	page := make([]pageRow, 0, limit+1)
	for rows.Next() {
		var pr pageRow
		var dto []byte
		if err := rows.Scan(&pr.turnID, &pr.sequence, &dto); err != nil {
			_ = rows.Close()
			return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: list page scan: %w", err)
		}
		row, err := unmarshalRowDTO(dto)
		if err != nil {
			_ = rows.Close()
			return nil, nil, zero, err
		}
		pr.row = row
		page = append(page, pr)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: list page rows: %w", err)
	}
	_ = rows.Close()

	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}

	// Assemble the full renderable rows (splice the child collections
	// back in). Fresh memory on every read: caller mutation can never
	// alias durable state.
	out := make([]turns.TurnRow, 0, len(page))
	for _, pr := range page {
		row, err := attachChildren(ctx, d.db, id, pr.turnID, pr.row)
		if err != nil {
			return nil, nil, zero, err
		}
		out = append(out, row)
	}

	// The exact number of older RETAINED rows beyond this page rides
	// the same keyset index range — CountExact is true, never a
	// history scan.
	var remaining int64
	if hasMore {
		last := out[len(out)-1]
		if err := d.db.QueryRowContext(ctx, countOlderSQL,
			id.TenantID, id.UserID, id.SessionID,
			int64(last.Sequence), string(last.TurnID),
		).Scan(&remaining); err != nil {
			return nil, nil, zero, fmt.Errorf("sessions/turns/sqlite: count older rows: %w", err)
		}
	}

	truncated, err := d.loadTruncated(ctx, id)
	if err != nil {
		return nil, nil, zero, err
	}

	info := turns.ListPageInfo{
		Snapshot:   snapshot,
		Remaining:  int(remaining),
		CountExact: true,
		Truncated:  truncated,
	}
	var next *turns.Cursor
	if hasMore {
		last := out[len(out)-1]
		next = &turns.Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	}
	return out, next, info, nil
}

// LoadCheckpoint implements turns.Store: returns the session's
// last-applied runtime event sequence (0 when none was ever saved — a
// fresh store, an erased session, or an in-memory store after
// restart). Reads are not fenced: a fenced session's checkpoint is
// still readable (it reads 0 after erasure).
func (d *driver) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if d.closed.Load() {
		return 0, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return 0, turns.ErrIdentityRequired
	}
	var seq uint64
	err := d.db.QueryRowContext(ctx, loadCheckpointSQL, id.TenantID, id.UserID, id.SessionID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: load checkpoint: %w", err)
	}
	return seq, nil
}

// SaveCheckpoint implements turns.Store: records the session's
// last-applied runtime event sequence. MONOTONIC and IDEMPOTENT — a
// sequence at or below the stored checkpoint is a no-op (never a
// regression), so a reconcile retry cannot rewind the checkpoint.
// Refused with ErrErasureFenced when the session is fenced — a rebuild
// must not advance the checkpoint of an erased session (no
// resurrection). The fence check and the conditional write share one
// transaction.
func (d *driver) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if d.closed.Load() {
		return fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: begin checkpoint tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()

	// A fenced (erased) session must never advance its checkpoint — no
	// resurrection after replay / restart.
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return err
	}
	if fenced {
		return turns.ErrErasureFenced
	}

	// Ensure the per-session state row exists, then advance only when
	// the new sequence is strictly greater (the WHERE clause makes the
	// monotonic advance atomic; concurrent writers converge to the max).
	if _, err := tx.ExecContext(ctx, ensureSessionStateSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: ensure session state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, saveCheckpointSQL, seq, id.TenantID, id.UserID, id.SessionID, seq); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: save checkpoint: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: commit checkpoint: %w", err)
	}
	committed = true
	return nil
}

// DeleteScope implements turns.Store: removes every retained turn row
// (children cascade in the same transaction) and the per-session write
// state (checkpoint, sequence counter, truncation flag) under id.
// Idempotent: an absent scope returns (0, nil). The erasure FENCE is
// deliberately NOT removed, and the projection SNAPSHOT generation IS
// advanced, so an erased session stays fenced (no resurrection) and a
// cursor minted before the erase can never page the post-erase
// projection.
func (d *driver) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if d.closed.Load() {
		return 0, fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	if err := identity.Validate(id); err != nil {
		return 0, turns.ErrIdentityRequired
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: begin delete-scope tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()

	// Delete ONLY this projection's owned records — rows (with their
	// child activity / App rows) and the per-session state. The erasure
	// FENCE table and the SNAPSHOT generation table are never deleted.
	deleted := 0
	for _, del := range []string{deleteActivityScopeSQL, deleteAppsScopeSQL, deleteRowsScopeSQL, deleteSessionStateSQL} {
		res, err := tx.ExecContext(ctx, del, id.TenantID, id.UserID, id.SessionID)
		if err != nil {
			return 0, fmt.Errorf("sessions/turns/sqlite: delete scope: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("sessions/turns/sqlite: delete scope rows affected: %w", err)
		}
		deleted += int(n)
	}

	// Advance the projection SNAPSHOT generation so any cursor minted
	// before the erase is rejected as stale — a pre-erase cursor must
	// never page the post-erase (or rebuilt) projection. Not counted as
	// a deleted record.
	if _, err := tx.ExecContext(ctx, bumpSnapshotSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: bump snapshot generation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: commit delete scope: %w", err)
	}
	committed = true
	return deleted, nil
}

// Close implements turns.Store. Setting the atomic flag BEFORE
// `db.Close()` ensures concurrent in-flight callers observe
// ErrStoreClosed rather than racing into a half-closed pool. Close is
// idempotent.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: close: %w", err)
	}
	return nil
}

// --- SQL -------------------------------------------------------------

// The SQL constants are package-level so the index-plan tests assert
// the SAME statements the driver executes (no test/driver SQL drift).

const (
	// listPageSQL pages one keyset page strictly older than the cursor
	// boundary (Seq, TurnID): the documented keyset predicate
	// `(Seq < C.Seq) || (Seq == C.Seq && TurnID < C.TurnID)` is exactly
	// the row-value comparison `(sequence, turn_id) < (?, ?)`, which
	// SQLite range-optimizes against the (tenant, user, session,
	// sequence, turn_id) index — a bounded index RANGE scan, never an
	// OFFSET over the session's history. The immutable order keys make
	// the page stable under concurrent appends (a newly appended row
	// can never satisfy an issued cursor, an already-returned row can
	// never be returned again). LIMIT is limit+1 so the caller learns
	// exactly whether older rows remain.
	listPageSQL = `
        SELECT turn_id, sequence, dto
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ?
          AND (sequence, turn_id) < (?, ?)
        ORDER BY sequence DESC, turn_id DESC
        LIMIT ?`

	// listPageNewestSQL is the first page (no cursor).
	listPageNewestSQL = `
        SELECT turn_id, sequence, dto
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ?
        ORDER BY sequence DESC, turn_id DESC
        LIMIT ?`

	// countOlderSQL is the exact count of older RETAINED rows beyond a
	// page — the same keyset index range (row-value form), so
	// CountExact is true without a history scan.
	countOlderSQL = `
        SELECT COUNT(*)
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ?
          AND (sequence, turn_id) < (?, ?)`

	// boundarySeqSQL resolves a cursor's authoritative boundary row by
	// its exact primary key (the cursor-expired / forged-cursor guards).
	boundarySeqSQL = `
        SELECT sequence
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`

	// guardRowSQL is the mutate path's authoritative guard read: the
	// immutable sequence, the sealed flag, and the current version, all
	// from one primary-key probe.
	guardRowSQL = `
        SELECT sequence, sealed, version
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`

	// getRowSQL is the indexed get: one primary-key probe.
	getRowSQL = `
        SELECT dto
        FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`

	// scopeFencedSQL is the STORE-LOCAL erasure-fence probe every write
	// transaction runs first.
	scopeFencedSQL = `
        SELECT 1 FROM turn_fences
        WHERE tenant = ? AND user = ? AND session = ?`

	// fenceInsertSQL is the durable erasure tombstone. Idempotent: an
	// already-fenced session stays fenced.
	fenceInsertSQL = `
        INSERT OR IGNORE INTO turn_fences (tenant, user, session)
        VALUES (?, ?, ?)`

	// mintSeqSQL (a) ensures the per-session state row exists and (b)
	// atomically advances the next-sequence counter inside the caller's
	// write transaction. The single-writer serialization + this
	// transaction make the read-then-update race-free.
	ensureSessionStateSQL = `
        INSERT OR IGNORE INTO turn_session_state (tenant, user, session)
        VALUES (?, ?, ?)`
	readSeqSQL = `
        SELECT next_seq FROM turn_session_state
        WHERE tenant = ? AND user = ? AND session = ?`
	advanceSeqSQL = `
        UPDATE turn_session_state SET next_seq = next_seq + 1
        WHERE tenant = ? AND user = ? AND session = ?`

	// upsertRowSQL replaces a row's immutable ordering keys + renderable
	// DTO wholesale. The version bump and sealed flag are driven by the
	// caller (append = 1/false; update = current+1/false; seal =
	// current+1/true).
	upsertRowSQL = `
        INSERT INTO turn_rows
            (tenant, user, session, turn_id, sequence, effective_agent_id, sealed, version, dto)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, session, turn_id) DO UPDATE SET
            sequence           = excluded.sequence,
            effective_agent_id = excluded.effective_agent_id,
            sealed             = excluded.sealed,
            version            = excluded.version,
            dto                = excluded.dto`

	// Activity + App children are replaced wholesale on every accepted
	// write (the accumulated component is replaced atomically under one
	// version bump). Order is restored by position.
	deleteActivityRowsSQL = `
        DELETE FROM turn_activity_rows
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
	insertActivityRowSQL = `
        INSERT INTO turn_activity_rows (tenant, user, session, turn_id, position, dto)
        VALUES (?, ?, ?, ?, ?, ?)`
	selectActivityRowsSQL = `
        SELECT dto FROM turn_activity_rows
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?
        ORDER BY position ASC`

	deleteAppsRowsSQL = `
        DELETE FROM turn_apps
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
	insertAppRowSQL = `
        INSERT INTO turn_apps
            (tenant, user, session, turn_id, position,
             effective_agent_id, server_id, resource_uri, dto)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	selectAppsRowsSQL = `
        SELECT dto FROM turn_apps
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?
        ORDER BY position ASC`

	// Retention eviction: delete the session's oldest rows past the
	// bound (children first, parents second, one transaction) and mark
	// the session's explicit truncation flag. The boundary is found via
	// the keyset index (LIMIT 1 OFFSET retain from the newest row), so
	// the eviction is O(bound), never a history scan.
	evictChildrenSQL = `
        DELETE FROM turn_activity_rows
        WHERE tenant = ? AND user = ? AND session = ?
          AND turn_id IN (
              SELECT turn_id FROM turn_rows
              WHERE tenant = ? AND user = ? AND session = ?
              ORDER BY sequence DESC, turn_id DESC
              LIMIT -1 OFFSET ?
          )`
	evictAppsSQL = `
        DELETE FROM turn_apps
        WHERE tenant = ? AND user = ? AND session = ?
          AND turn_id IN (
              SELECT turn_id FROM turn_rows
              WHERE tenant = ? AND user = ? AND session = ?
              ORDER BY sequence DESC, turn_id DESC
              LIMIT -1 OFFSET ?
          )`
	evictRowsSQL = `
        DELETE FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ?
          AND sequence <= (
              SELECT sequence FROM turn_rows
              WHERE tenant = ? AND user = ? AND session = ?
              ORDER BY sequence DESC, turn_id DESC
              LIMIT 1 OFFSET ?
          )`
	setTruncatedSQL = `
        UPDATE turn_session_state SET truncated = 1
        WHERE tenant = ? AND user = ? AND session = ?`

	// Checkpoint: monotonic + idempotent (the WHERE clause refuses a
	// regression atomically).
	loadCheckpointSQL = `
        SELECT checkpoint FROM turn_session_state
        WHERE tenant = ? AND user = ? AND session = ?`
	saveCheckpointSQL = `
        UPDATE turn_session_state SET checkpoint = ?
        WHERE tenant = ? AND user = ? AND session = ? AND checkpoint < ?`

	// DeleteScope: this projection's owned records only.
	deleteActivityScopeSQL = `
        DELETE FROM turn_activity_rows
        WHERE tenant = ? AND user = ? AND session = ?`
	deleteAppsScopeSQL = `
        DELETE FROM turn_apps
        WHERE tenant = ? AND user = ? AND session = ?`
	deleteRowsScopeSQL = `
        DELETE FROM turn_rows
        WHERE tenant = ? AND user = ? AND session = ?`
	deleteSessionStateSQL = `
        DELETE FROM turn_session_state
        WHERE tenant = ? AND user = ? AND session = ?`

	// Snapshot generation: read (absent = 0, the fresh-session initial
	// generation) and bump (advances on every erasure, survives it).
	loadSnapshotSQL = `
        SELECT gen FROM turn_snapshot_gens
        WHERE tenant = ? AND user = ? AND session = ?`
	bumpSnapshotSQL = `
        INSERT INTO turn_snapshot_gens (tenant, user, session, gen)
        VALUES (?, ?, ?, 1)
        ON CONFLICT(tenant, user, session) DO UPDATE SET gen = gen + 1`

	// Truncation flag read: absent session state = never truncated.
	loadTruncatedSQL = `
        SELECT truncated FROM turn_session_state
        WHERE tenant = ? AND user = ? AND session = ?`
)

// --- helpers ---------------------------------------------------------

// pageRow is one raw page candidate: the immutable ordering keys plus
// the decoded row DTO (children spliced separately on assembly).
type pageRow struct {
	turnID   turns.TurnID
	sequence turns.Seq
	row      turns.TurnRow
}

// queryer is the minimal query surface shared by *sql.DB and *sql.Tx
// so the read/write helpers work on both a pool and an open
// transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// scopeFenced reports whether the session carries the STORE-LOCAL
// durable erasure fence (the tombstone). Caller holds an open
// transaction on write paths so the check and the write are atomic.
func scopeFenced(ctx context.Context, q queryer, id identity.Identity) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, scopeFencedSQL, id.TenantID, id.UserID, id.SessionID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("sessions/turns/sqlite: fence probe: %w", err)
}

// mintSeq advances the session's immutable sequence counter by one and
// returns the new value. Caller holds an open write transaction (the
// single-writer serialization makes the read-then-update atomic).
func mintSeq(ctx context.Context, tx *sql.Tx, id identity.Identity) (turns.Seq, error) {
	if _, err := tx.ExecContext(ctx, ensureSessionStateSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: ensure session state: %w", err)
	}
	var next int64
	if err := tx.QueryRowContext(ctx, readSeqSQL, id.TenantID, id.UserID, id.SessionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: read next sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, advanceSeqSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: advance sequence: %w", err)
	}
	return turns.Seq(next), nil
}

// writeRowAndChildren persists the complete renderable row DTO plus its
// two dynamic bounded child collections (activity rows, App refs) in
// the caller's transaction. The children are replaced wholesale on
// every accepted write. The row-level scalar mirrors (sequence,
// effective agent, sealed, version) are derived from the DTO at write
// time and serve the query axes only — the DTO is authoritative on
// reads.
func writeRowAndChildren(ctx context.Context, tx *sql.Tx, id identity.Identity, row turns.TurnRow) error {
	dto, err := marshalRowDTO(row)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, upsertRowSQL,
		id.TenantID, id.UserID, id.SessionID,
		string(row.TurnID), int64(row.Sequence), row.Agent.ID,
		row.Sealed, row.Version, dto,
	); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: upsert row: %w", err)
	}

	// Activity children — replaced wholesale, order restored by
	// position on read.
	if _, err := tx.ExecContext(ctx, deleteActivityRowsSQL, id.TenantID, id.UserID, id.SessionID, string(row.TurnID)); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: clear activity rows: %w", err)
	}
	for i, ar := range row.Activity.Rows {
		b, err := json.Marshal(ar)
		if err != nil {
			return fmt.Errorf("sessions/turns/sqlite: encode activity row %d: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx, insertActivityRowSQL,
			id.TenantID, id.UserID, id.SessionID, string(row.TurnID), i, b,
		); err != nil {
			return fmt.Errorf("sessions/turns/sqlite: insert activity row %d: %w", i, err)
		}
	}

	// App children — replaced wholesale; the exact App replacement
	// identity (effective_agent_id, server_id, resource_uri) is the
	// table's primary key, so a feed that repeats one identity in a
	// single write fails loudly instead of silently duplicating order.
	if _, err := tx.ExecContext(ctx, deleteAppsRowsSQL, id.TenantID, id.UserID, id.SessionID, string(row.TurnID)); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: clear app refs: %w", err)
	}
	for i, app := range row.Apps {
		b, err := json.Marshal(app)
		if err != nil {
			return fmt.Errorf("sessions/turns/sqlite: encode app ref %d: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx, insertAppRowSQL,
			id.TenantID, id.UserID, id.SessionID, string(row.TurnID), i,
			app.EffectiveAgentID, app.ServerID, app.ResourceURI, b,
		); err != nil {
			return fmt.Errorf("sessions/turns/sqlite: insert app ref %d: %w", i, err)
		}
	}
	return nil
}

// marshalRowDTO encodes the row's COMPLETE renderable DTO with the two
// child collections removed (they live in their indexed child tables).
// Everything else — including the honest availability / overflow
// fields (Activity.Complete / More / Dropped / Totals, Reasoning
// Complete / Dropped / Seq, per-app and per-attachment availability) —
// is persisted byte-exact.
func marshalRowDTO(row turns.TurnRow) ([]byte, error) {
	row.Activity.Rows = nil
	row.Apps = nil
	b, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: encode row DTO %q: %w", row.TurnID, err)
	}
	return b, nil
}

// unmarshalRowDTO decodes a stored row DTO into fresh memory (the
// driver never returns driver-internal buffers; JSON decoding on every
// read boundary satisfies the deep-copy obligation).
func unmarshalRowDTO(b []byte) (turns.TurnRow, error) {
	var row turns.TurnRow
	if err := json.Unmarshal(b, &row); err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: decode row DTO: %w", err)
	}
	return row, nil
}

// loadTurn reads one complete turn (row DTO + both child collections)
// by its exact (identity triple, turn id) primary key. found=false when
// the row is absent (never created, evicted, or erased).
func loadTurn(ctx context.Context, q queryer, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, bool, error) {
	var dto []byte
	err := q.QueryRowContext(ctx, getRowSQL, id.TenantID, id.UserID, id.SessionID, string(turnID)).Scan(&dto)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, false, nil
	}
	if err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("sessions/turns/sqlite: get row: %w", err)
	}
	row, err := unmarshalRowDTO(dto)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	row, err = attachChildren(ctx, q, id, turnID, row)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	return row, true, nil
}

// attachChildren splices the turn's ordered activity rows and App refs
// (fresh memory from JSON decoding) onto the decoded row DTO.
func attachChildren(ctx context.Context, q queryer, id identity.Identity, turnID turns.TurnID, row turns.TurnRow) (turns.TurnRow, error) {
	rows, err := q.QueryContext(ctx, selectActivityRowsSQL, id.TenantID, id.UserID, id.SessionID, string(turnID))
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: list activity rows: %w", err)
	}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			_ = rows.Close()
			return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: scan activity row: %w", err)
		}
		var ar turns.ActivityRow
		if err := json.Unmarshal(b, &ar); err != nil {
			_ = rows.Close()
			return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: decode activity row: %w", err)
		}
		row.Activity.Rows = append(row.Activity.Rows, ar)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: activity rows: %w", err)
	}
	_ = rows.Close()

	apps, err := q.QueryContext(ctx, selectAppsRowsSQL, id.TenantID, id.UserID, id.SessionID, string(turnID))
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: list app refs: %w", err)
	}
	for apps.Next() {
		var b []byte
		if err := apps.Scan(&b); err != nil {
			_ = apps.Close()
			return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: scan app ref: %w", err)
		}
		var app turns.AppRef
		if err := json.Unmarshal(b, &app); err != nil {
			_ = apps.Close()
			return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: decode app ref: %w", err)
		}
		row.Apps = append(row.Apps, app)
	}
	if err := apps.Err(); err != nil {
		_ = apps.Close()
		return turns.TurnRow{}, fmt.Errorf("sessions/turns/sqlite: app rows: %w", err)
	}
	_ = apps.Close()
	return row, nil
}

// enforceRetention evicts the session's oldest rows past d.retain
// (children first, parents second, one transaction) and sets the
// session's explicit truncation flag. Caller holds an open write
// transaction.
//
// The guard is cheap and sound: per-session sequences are contiguous
// (minted inside the same transaction under the single-writer
// serialization; a failed append rolls the mint back with the whole
// transaction), so the just-minted sequence IS the session's current
// row count. Eviction only runs once the count exceeds the bound —
// the steady state past the bound is one eviction per append, and the
// first `retain` appends of a session pay no eviction statements at
// all.
func (d *driver) enforceRetention(ctx context.Context, tx *sql.Tx, id identity.Identity, seq turns.Seq) error {
	if int64(seq) <= int64(d.retain) {
		return nil
	}
	// Children of evicted turns first.
	if _, err := tx.ExecContext(ctx, evictChildrenSQL,
		id.TenantID, id.UserID, id.SessionID, id.TenantID, id.UserID, id.SessionID, d.retain,
	); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: evict activity rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, evictAppsSQL,
		id.TenantID, id.UserID, id.SessionID, id.TenantID, id.UserID, id.SessionID, d.retain,
	); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: evict app refs: %w", err)
	}
	res, err := tx.ExecContext(ctx, evictRowsSQL,
		id.TenantID, id.UserID, id.SessionID, id.TenantID, id.UserID, id.SessionID, d.retain,
	)
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: evict rows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: evict rows affected: %w", err)
	}
	if n == 0 {
		return nil
	}
	// Retention eviction is explicit, never silent: mark the session's
	// truncation flag. The state row exists (the sequence mint created
	// it), so the UPDATE is authoritative.
	if _, err := tx.ExecContext(ctx, setTruncatedSQL, id.TenantID, id.UserID, id.SessionID); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: set truncation flag: %w", err)
	}
	return nil
}

// loadSnapshot returns the session's projection snapshot generation
// (0 when never advanced — the fresh-session initial generation).
func (d *driver) loadSnapshot(ctx context.Context, id identity.Identity) (uint64, error) {
	var gen uint64
	err := d.db.QueryRowContext(ctx, loadSnapshotSQL, id.TenantID, id.UserID, id.SessionID).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sessions/turns/sqlite: load snapshot generation: %w", err)
	}
	return gen, nil
}

// loadTruncated returns the session's explicit truncation flag (false
// when never truncated — absent state is never truncated).
func (d *driver) loadTruncated(ctx context.Context, id identity.Identity) (bool, error) {
	var truncated int64
	err := d.db.QueryRowContext(ctx, loadTruncatedSQL, id.TenantID, id.UserID, id.SessionID).Scan(&truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sessions/turns/sqlite: load truncation flag: %w", err)
	}
	return truncated != 0, nil
}
