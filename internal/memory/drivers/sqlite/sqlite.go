// Package sqlite is Harbor's SQLite-backed `memory.MemoryStore`
// driver (Phase 25). It is the second leg of the memory persistence
// triad (in-memory floor, SQLite, Postgres) defined by RFC §6.6 + §9.
//
// The driver is built on `modernc.org/sqlite` — a CGo-free SQLite
// engine (D-013, AGENTS.md §5). Builds remain `CGO_ENABLED=0`.
//
// Operating model:
//
//   - Database opened against `cfg.DSN`. Bare file paths and the
//     special `:memory:` sentinel are supported. URI-form DSNs
//     (`file:foo.db?...`) pass through with `_pragma` + `_txlock`
//     query params layered on top so per-connection PRAGMAs survive
//     `database/sql`'s connection lifecycle.
//   - WAL journal mode is pinned at open. WAL gives concurrent
//     readers + a single writer with no `SQLITE_BUSY` storms in the
//     read path.
//   - `busy_timeout=5000` (5 s) absorbs `SQLITE_BUSY` retries
//     transparently.
//   - `db.SetMaxOpenConns(1)` pins the pool to a single connection.
//     This matches Phase 15's StateStore + Phase 18's
//     ArtifactStore-blob driver — `BEGIN IMMEDIATE` does not honor
//     `busy_timeout` for inter-connection writer contention, so
//     under high concurrency the conformance suite's N=128 stress
//     would otherwise leak `SQLITE_BUSY` to callers.
//   - The schema is applied via embedded `migrations/*.sql` files
//     (forward-only, AGENTS.md §13). The runner is idempotent.
//
// Memory state lives in its OWN `memory_state` table — the SQLite
// memory driver does NOT piggyback on the SQLite StateStore driver's
// `state_records` table. The injected `state.StateStore` dep is
// accepted to satisfy the shared `memory.Deps` contract but is
// unused by the persistent drivers; the `events.EventBus` dep IS
// used (for the fail-closed identity-rejection emit path).
//
// The driver self-registers under `"sqlite"` from its `init()`. The
// production binary picks it up via blank import in
// `cmd/harbor/main.go`; tests may call `New` directly to skip the
// registry.
//
// Concurrency contract (D-025):
//
//   - The driver struct holds a `*sql.DB` (an internally-synchronized
//     connection pool, pinned to one connection) and an `atomic.Bool`
//     close flag. Both are safe for N concurrent goroutines without
//     external locking.
//   - Per-call state lives on the call stack / supplied `ctx`. Nothing
//     mutable on the driver ever crosses run boundaries.
//   - SQLite's single-writer-per-database invariant is enforced by
//     the engine; `busy_timeout` ensures concurrent writers serialize
//     transparently.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// modernc.org/sqlite registers the "sqlite" driver name with
	// database/sql via its own init(). Blank-importing it here is the
	// idiomatic way to make `sql.Open("sqlite", dsn)` work.
	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// driverName is the name under which this driver self-registers with
// both `memory.Register` and `database/sql` (modernc.org/sqlite uses
// the same string for its `sql.Open` driver name).
const driverName = "sqlite"

// busyTimeoutMs is the PRAGMA busy_timeout value pinned at open. 5 s
// is reasonable for single-binary deployments with light concurrency.
const busyTimeoutMs = 5000

// New constructs a SQLite-backed `memory.MemoryStore` against
// `cfg.DSN`. Production callers go through `memory.Open`; tests may
// call `New` directly to skip the registry.
//
// Strategy unsupported at Phase 23/25 (anything other than
// `StrategyNone` or empty-equivalent) returns
// `ErrStrategyNotImplemented` rather than silently coercing — Phase
// 24 widens the supported set, and the SQLite driver will inherit
// that widening automatically through the shared conformance suite.
//
// DSN handling mirrors the SQLite StateStore + ArtifactStore drivers:
// bare file paths and the special `:memory:` sentinel are supported;
// the driver appends `_pragma=busy_timeout(5000)` +
// `_pragma=journal_mode(WAL)` + `_txlock=immediate` query params so
// every pooled connection sees the same per-connection PRAGMAs.
//
// `deps.Bus` is required (for the fail-closed identity-rejection
// emit path). `deps.State` is accepted to satisfy `memory.Deps` but
// is unused — persistent memory state lives in this driver's own
// `memory_state` table.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/sqlite: deps.Bus is required")
	}
	// deps.State is accepted but unused; we intentionally do not gate
	// on it being non-nil here because the registry's validateDeps
	// already enforces non-nil at the public Open entry point.
	if cfg.DSN == "" {
		return nil, errors.New(`memory/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	strategy := cfg.Strategy
	if strategy == "" {
		strategy = memory.StrategyNone
	}
	if strategy != memory.StrategyNone {
		return nil, fmt.Errorf("%w: %q (Phase 25 supports %q only; Phase 24 will widen)",
			memory.ErrStrategyNotImplemented, strategy, memory.StrategyNone)
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: sql.Open(%q): %w", cfg.DSN, err)
	}

	// Pin the pool to a single connection — see Phase 15's
	// `internal/state/drivers/sqlite/sqlite.go` for the rationale.
	// SQLite's BEGIN IMMEDIATE does not honor busy_timeout across
	// pool connections; pinning serialises writers at the Go layer.
	db.SetMaxOpenConns(1)

	// Bounded context for open-time validation + migrations so a
	// wedged file doesn't hang construction forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := verifyJournalMode(ctx, db, cfg.DSN); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memory/sqlite: migrate: %w", err)
	}

	return &driver{
		strategy: strategy,
		db:       db,
		bus:      deps.Bus,
		state:    deps.State, // accepted-but-unused; held to keep the dep visible in panics/snapshots
	}, nil
}

func init() {
	memory.Register(driverName, func(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
		return New(cfg, deps)
	})
}

// driver is the SQLite-backed MemoryStore. It is safe for concurrent
// use by N goroutines; mutable state is the `atomic.Bool` close flag
// (load-then-act pattern) plus the underlying `*sql.DB` (internally
// synchronized by database/sql). Per D-025 nothing per-run lives on
// the driver — every method reads identity from its arguments.
type driver struct {
	strategy memory.Strategy
	db       *sql.DB
	bus      events.EventBus
	state    state.StateStore // accepted but unused (see package doc)

	// mu serialises Close itself so it idempotently observes
	// "already closed" rather than racing on the write. The atomic
	// is the operational gate for every other method.
	mu     sync.Mutex
	closed atomic.Bool
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// AddTurn implements memory.MemoryStore.
//
// Strategy=none: no-op. Identity validated at the boundary; missing
// triple → fail-closed with bus emit.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, _ memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	// Strategy=none: nothing persisted. Phase 24 will append the
	// turn to the memory_state record here.
	return nil
}

// GetLLMContext implements memory.MemoryStore. Strategy=none returns
// an empty patch.
func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return memory.LLMContextPatch{Strategy: d.strategy}, nil
}

// EstimateTokens implements memory.MemoryStore. Strategy=none
// returns 0.
func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return 0, nil
}

// Flush implements memory.MemoryStore. Strategy=none idempotent
// delete-by-slot (no-op when no row exists for the identity).
func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}

	const del = `
        DELETE FROM memory_state
        WHERE tenant = ? AND user = ? AND session = ? AND run = ? AND kind = ?`
	if _, err := d.db.ExecContext(ctx, del,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState); err != nil {
		return fmt.Errorf("memory/sqlite: Flush delete: %w", err)
	}
	return nil
}

// Health implements memory.MemoryStore. Strategy=none always
// reports `HealthHealthy`.
func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return memory.HealthHealthy, nil
}

// Snapshot implements memory.MemoryStore.
//
// Reads the row at `(id, KindMemoryState)`. A missing row returns an
// empty Strategy=none snapshot (no record has been written yet for
// this identity). Present rows return their stored bytes verbatim —
// the cross-driver byte-stability guarantee.
func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}

	const sel = `
        SELECT strategy, bytes
        FROM memory_state
        WHERE tenant = ? AND user = ? AND session = ? AND run = ? AND kind = ?
        LIMIT 1`
	row := d.db.QueryRowContext(ctx, sel,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState)

	var strategy string
	var data []byte
	if err := row.Scan(&strategy, &data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Snapshot{Strategy: d.strategy}, nil
		}
		return memory.Snapshot{}, fmt.Errorf("memory/sqlite: Snapshot load: %w", err)
	}
	return memory.Snapshot{Strategy: memory.Strategy(strategy), Bytes: data}, nil
}

// Restore implements memory.MemoryStore.
//
// Strategy=none accepts only empty snapshots (or the canonical
// `{"strategy":"none"}` envelope). The snapshot's Strategy MUST match
// the driver's; mismatched strategies return `ErrInvalidSnapshot`
// — fail loudly, never silently coerce.
func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}

	// Empty snapshot (zero value, no strategy / bytes) is always
	// acceptable and round-trips the initial state.
	if snap.IsEmpty() {
		return d.persistRecord(ctx, id, memory.Record{Strategy: d.strategy})
	}
	if snap.Strategy != d.strategy {
		return fmt.Errorf("%w: snapshot strategy=%q driver strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, d.strategy)
	}
	if len(snap.Bytes) == 0 {
		return d.persistRecord(ctx, id, memory.Record{Strategy: d.strategy})
	}
	if d.strategy == memory.StrategyNone {
		// Strategy=none accepts only the canonical empty record.
		// Decode + reject anything that carries actual turns.
		var rec memory.Record
		if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
			return fmt.Errorf("%w: %v", memory.ErrInvalidSnapshot, err)
		}
		if rec.Strategy != memory.StrategyNone {
			return fmt.Errorf("%w: record strategy=%q", memory.ErrInvalidSnapshot, rec.Strategy)
		}
		if len(rec.Turns) > 0 {
			return fmt.Errorf("%w: Strategy=none cannot carry %d turn(s)",
				memory.ErrInvalidSnapshot, len(rec.Turns))
		}
		return d.persistRecord(ctx, id, rec)
	}
	// Phase 24 will branch on d.strategy for truncation +
	// rolling_summary. Today, only Strategy=none with empty bytes is
	// reachable.
	return fmt.Errorf("%w: unsupported snapshot for strategy %q",
		memory.ErrInvalidSnapshot, d.strategy)
}

// persistRecord marshals the typed record and writes it through an
// UPSERT against the dedicated `memory_state` table.
func (d *driver) persistRecord(ctx context.Context, id identity.Quadruple, rec memory.Record) error {
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory/sqlite: marshal record: %w", err)
	}
	const upsert = `
        INSERT INTO memory_state
            (tenant, user, session, run, kind, strategy, bytes, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, session, run, kind) DO UPDATE SET
            strategy   = excluded.strategy,
            bytes      = excluded.bytes,
            updated_at = excluded.updated_at`
	if _, err := d.db.ExecContext(ctx, upsert,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState,
		string(rec.Strategy), bytes, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("memory/sqlite: persist record: %w", err)
	}
	return nil
}

// Close implements memory.MemoryStore. Setting the atomic flag BEFORE
// `db.Close()` ensures concurrent in-flight callers observe
// `ErrStoreClosed` rather than racing into a half-closed pool. Close
// is idempotent.
func (d *driver) Close(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("memory/sqlite: close: %w", err)
	}
	return nil
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them
// to every new connection the pool opens. The implementation mirrors
// the SQLite StateStore + ArtifactStore drivers verbatim (Phase 15
// settled the shape; Phase 25 inherits).
func augmentDSNForPragmas(dsn string) (string, error) {
	if dsn == ":memory:" {
		dsn = "file::memory:?cache=shared"
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
		if q.Get("_txlock") == "" {
			q.Set("_txlock", "immediate")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

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
		return fmt.Errorf("memory/sqlite: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("memory/sqlite: journal_mode=%q after open; expected \"wal\" (DSN=%q)",
			mode, originalDSN)
	}
	return nil
}

// isMemoryDSN reports whether the caller-supplied DSN routes to an
// in-memory database (no disk-backed file).
func isMemoryDSN(dsn string) bool {
	if dsn == ":memory:" {
		return true
	}
	if strings.HasPrefix(dsn, "file:") && strings.Contains(dsn, ":memory:") {
		return true
	}
	return false
}
