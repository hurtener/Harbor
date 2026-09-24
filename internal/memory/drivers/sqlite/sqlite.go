// Package sqlite provides the sqlite driver for cumulative session-memory access.
// The injected StateStore remains the authoritative memory owner.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
// Cumulative memory uses the injected StateStore owner. This adapter has no
// summary engine, private transcript or recovery goroutine. Note writes use
// the same redactor as execution.
//
// DSN handling mirrors the SQLite StateStore + ArtifactStore drivers:
// bare file paths and the special `:memory:` sentinel are supported;
// the driver appends `_pragma=busy_timeout(5000)` +
// `_pragma=journal_mode(WAL)` + `_txlock=immediate` query params so
// every pooled connection sees the same per-connection PRAGMAs.
//
// `deps.Bus` is required (for the fail-closed identity-rejection emit
// path). `deps.State` is required — it is the persistence floor the
// cumulative owner writes through.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/sqlite: deps.Bus is required")
	}
	if deps.State == nil {
		return nil, fmt.Errorf("memory/sqlite: deps.State is required (strategy executor persists through it)")
	}
	if cfg.DSN == "" {
		return nil, errors.New(`memory/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	if err := memory.ValidateStrategy(cfg.Strategy); err != nil {
		return nil, err
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: sql.Open(%q): %w", cfg.DSN, err)
	}

	// Pin the pool to a single connection — see the
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
		access: memory.NewAccess(cfg, deps),
		db:     db,
		bus:    deps.Bus,
	}, nil
}

func init() {
	memory.Register(driverName, New)
}

// driver is the SQLite-backed MemoryStore. It is safe for concurrent
// use by N goroutines; mutable state is the `atomic.Bool` close flag
// (load-then-act pattern), plus the underlying `*sql.DB` (internally
// synchronized by database/sql). Nothing per-run lives on
// the driver — every method reads identity from its arguments.
type driver struct {
	access *memory.Access
	db     *sql.DB
	bus    events.EventBus

	// mu serialises Close itself so it idempotently observes
	// "already closed" rather than racing on the write. The atomic
	// is the operational gate for every other method.
	mu     sync.Mutex
	closed atomic.Bool
}

// Inspect implements memory.MemoryStore using the execution-memory owner.
func (d *driver) Inspect(ctx context.Context, id identity.Quadruple) (memory.Inspection, error) {
	if d.closed.Load() {
		return memory.Inspection{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Inspection{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Inspect")
	}
	return d.access.Inspect(ctx, id)
}

// Put implements memory.MemoryStore and returns a committed item identity.
func (d *driver) Put(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) (string, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Put")
	}
	return d.access.Put(ctx, id, turn)
}

// Delete implements memory.MemoryStore through its conditional owner mutation.
func (d *driver) Delete(ctx context.Context, id identity.Quadruple, key string) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "Delete")
	}
	return d.access.Delete(ctx, id, key)
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// Close implements memory.MemoryStore. Setting the atomic flag BEFORE
// closing `db` rejects new operations before pool teardown. Already admitted
// operations use the separately owned StateStore; this adapter does not close
// that shared owner. Close is idempotent.
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	dbErr := d.db.Close()
	if dbErr != nil {
		dbErr = fmt.Errorf("memory/sqlite: close: %w", dbErr)
	}

	return dbErr
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them
// to every new connection the pool opens. The implementation mirrors
// the SQLite StateStore + ArtifactStore drivers verbatim (
// settled the shape; a follow-up added the per-Open `:memory:` isolation).
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

// uniqueMemoryDSN mints a per-Open named in-memory database URI.
// `mode=memory` keeps it off disk; `cache=shared` lets every
// connection in THIS store's pool see the same database; the
// crypto-random name keeps two `:memory:` stores — this subsystem's or
// any other's — fully isolated within one process (the previous
// process-wide `file::memory:?cache=shared` translation made every
// subsystem's `:memory:` store collide on one shared
// `schema_migrations` table). The database lives as long as the pool's
// single pinned connection (SetMaxOpenConns(1)) holds it open.
func uniqueMemoryDSN() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("memory/sqlite: memory-DSN entropy: %w", err)
	}
	return "file:harbor_memory_mem_" + hex.EncodeToString(entropy[:]) + "?mode=memory&cache=shared", nil
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
