// Package sqlite is Harbor's SQLite-backed `memory.MemoryStore`
// driver. It is the second leg of the memory persistence triad
// (in-memory floor, SQLite, Postgres) defined by RFC §6.6 + §9.
//
// The driver is built on `modernc.org/sqlite` — a CGo-free SQLite
// engine (D-013, AGENTS.md §5). Builds remain `CGO_ENABLED=0`.
//
// # Strategy delegation (Phase 25a, D-174)
//
// All three memory strategies (`none`, `truncation`,
// `rolling_summary`) are implemented by the driver-agnostic
// `internal/memory/strategy` executor package; this driver is a thin
// shell that owns the boundary (identity validation + the
// `memory.identity_rejected` emit + the `closed` flag) and delegates
// every `MemoryStore` method to a `strategy.StrategyExecutor`. The
// executor persists state through the injected `state.StateStore`
// (D-027 typed wrapper, `Kind = "memory.state"`). When that
// StateStore is itself SQLite-backed (the operator's
// `state.driver: sqlite`), the memory strategies persist durably to
// disk — which is what makes `truncation` / `rolling_summary` survive
// a restart. No strategy algorithm is reimplemented in SQL here; the
// SQLite + Postgres memory drivers gain all three strategies through
// the same executor the InMem driver uses.
//
// The driver still opens its own `*sql.DB` against `cfg.DSN`. The
// connection + the embedded `memory_state` migration are retained so
// the driver fails loudly on a misconfigured DSN at boot (and so the
// schema exists for any out-of-band tooling), but the live read/write
// path rides entirely on the executor's `state.StateStore` writes.
// The driver's own `memory_state` table is vestigial under
// delegation; it is kept (never edited — migrations are forward-only,
// AGENTS.md §9 / §13) for back-compat with rows written by the
// pre-25a strategy=none path.
//
// Operating model for the retained connection:
//
//   - Database opened against `cfg.DSN`. Bare file paths and the
//     special `:memory:` sentinel are supported. URI-form DSNs
//     (`file:foo.db?...`) pass through with `_pragma` + `_txlock`
//     query params layered on top so per-connection PRAGMAs survive
//     `database/sql`'s connection lifecycle.
//   - WAL journal mode is pinned at open.
//   - `busy_timeout=5000` (5 s) absorbs `SQLITE_BUSY` retries.
//   - `db.SetMaxOpenConns(1)` pins the pool to a single connection.
//   - The schema is applied via embedded `migrations/*.sql` files
//     (forward-only, AGENTS.md §13). The runner is idempotent.
//
// The driver self-registers under `"sqlite"` from its `init()`. The
// production binary picks it up via blank import in
// `cmd/harbor/main.go`; tests may call `New` directly to skip the
// registry.
//
// Concurrency contract (D-025):
//
//   - The driver struct holds the strategy executor (internally
//     synchronised per-key), a `*sql.DB` (an internally-synchronised
//     pool), and an `atomic.Bool` close flag. All are safe for N
//     concurrent goroutines without external locking.
//   - Per-call state lives on the call stack / supplied `ctx`. Nothing
//     mutable on the driver ever crosses run boundaries.
package sqlite

import (
	"context"
	"database/sql"
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
	"github.com/hurtener/Harbor/internal/memory/strategy"
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
// The configured strategy is resolved by the shared
// `strategy.StrategyExecutor` (Phase 25a, D-174): `none`,
// `truncation`, and `rolling_summary` all delegate to the executor,
// which persists through `deps.State`. `rolling_summary` requires a
// non-nil `deps.Summarizer`; the executor's `New` rejects a nil
// summariser for that strategy with a wrapped error — fail loudly,
// never a stub fallback (AGENTS.md §13).
//
// DSN handling mirrors the SQLite StateStore + ArtifactStore drivers:
// bare file paths and the special `:memory:` sentinel are supported;
// the driver appends `_pragma=busy_timeout(5000)` +
// `_pragma=journal_mode(WAL)` + `_txlock=immediate` query params so
// every pooled connection sees the same per-connection PRAGMAs.
//
// `deps.Bus` is required (for the fail-closed identity-rejection emit
// path). `deps.State` is required — it is the persistence floor the
// strategy executor writes through.
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

	strategyName := cfg.Strategy
	if strategyName == "" {
		strategyName = memory.StrategyNone
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

	// Build the strategy executor. Persistence rides on deps.State
	// (typically the SQLite StateStore), giving truncation +
	// rolling_summary durable, restart-surviving state.
	exec, err := strategy.New(strategyName, strategy.Deps{
		State:              deps.State,
		Bus:                deps.Bus,
		Summarizer:         deps.Summarizer,
		BudgetTokens:       cfg.BudgetTokens,
		RecoveryBacklogMax: cfg.RecoveryBacklogMax,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &driver{
		strategy: strategyName,
		db:       db,
		bus:      deps.Bus,
		exec:     exec,
	}, nil
}

func init() {
	memory.Register(driverName, New)
}

// driver is the SQLite-backed MemoryStore. It is safe for concurrent
// use by N goroutines; mutable state is the `atomic.Bool` close flag
// (load-then-act pattern), the strategy executor (internally
// synchronised), plus the underlying `*sql.DB` (internally
// synchronized by database/sql). Per D-025 nothing per-run lives on
// the driver — every method reads identity from its arguments.
type driver struct {
	strategy memory.Strategy
	db       *sql.DB
	bus      events.EventBus
	exec     strategy.StrategyExecutor

	// mu serialises Close itself so it idempotently observes
	// "already closed" rather than racing on the write. The atomic
	// is the operational gate for every other method.
	mu     sync.Mutex
	closed atomic.Bool
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// AddTurn implements memory.MemoryStore. Identity validated at the
// boundary; missing triple → fail-closed with bus emit. The strategy
// executor owns turn-handling.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	return d.exec.AddTurn(ctx, id, turn)
}

// GetLLMContext implements memory.MemoryStore.
func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return d.exec.GetLLMContext(ctx, id)
}

// EstimateTokens implements memory.MemoryStore.
func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return d.exec.EstimateTokens(ctx, id)
}

// Flush implements memory.MemoryStore.
func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}
	return d.exec.Flush(ctx, id)
}

// Health implements memory.MemoryStore.
func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return d.exec.Health(ctx, id)
}

// Snapshot implements memory.MemoryStore.
func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}
	return d.exec.Snapshot(ctx, id)
}

// Restore implements memory.MemoryStore.
func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}
	return d.exec.Restore(ctx, id, snap)
}

// Close implements memory.MemoryStore. Setting the atomic flag BEFORE
// tearing down the executor + closing `db` ensures concurrent
// in-flight callers observe `ErrStoreClosed` rather than racing into a
// half-closed pool. Close is idempotent and joins the strategy
// executor's per-strategy resources (the rolling_summary recovery
// loop goroutine) so the goroutine baseline is restored (AC-9).
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	execErr := d.exec.Close(ctx)
	dbErr := d.db.Close()
	if dbErr != nil {
		dbErr = fmt.Errorf("memory/sqlite: close: %w", dbErr)
	}
	if execErr != nil {
		execErr = fmt.Errorf("memory/sqlite: executor close: %w", execErr)
	}
	return errors.Join(dbErr, execErr)
}

// augmentDSNForPragmas appends the open-time PRAGMA + transaction
// settings Harbor requires to dsn so modernc.org/sqlite applies them
// to every new connection the pool opens. The implementation mirrors
// the SQLite StateStore + ArtifactStore drivers verbatim (Phase 15
// settled the shape).
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
