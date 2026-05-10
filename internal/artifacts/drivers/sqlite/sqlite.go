// Package sqlite is Harbor's SQLite-backed `artifacts.ArtifactStore`
// driver (Phase 18). It is the third leg of the artifact persistence
// triad (in-memory floor, FS, SQLite, Postgres) defined by
// RFC §6.10 + §9.
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
//     transparently — write contention in the conformance suite's
//     `Concurrent_PutGet_NoRace` (N=128) and our supplemental
//     `concurrent_test.go` does not surface caller-visible errors.
//   - `db.SetMaxOpenConns(1)` pins the pool to a single connection.
//     Phase 15's StateStore driver settled this — `BEGIN IMMEDIATE`
//     does not honor `busy_timeout` for inter-connection writer
//     contention, so under high concurrency the conformance suite's
//     N=128 stress can otherwise leak `SQLITE_BUSY` to callers.
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
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
)

// driverName is the name registered with database/sql by
// `modernc.org/sqlite`.
const driverName = "sqlite"

// busyTimeoutMs is the PRAGMA busy_timeout value pinned at open. 5 s
// is reasonable for single-binary deployments with light concurrency
// (RFC §10 stack-decisions principle — V1 simplicity over an extra
// config knob). Operators with heavy contention can either move to
// the Postgres driver or adjust this constant in a follow-up phase.
const busyTimeoutMs = 5000

const (
	defaultNamespace = "default"
	defaultMimeBytes = "application/octet-stream"
	defaultMimeText  = "text/plain; charset=utf-8"
)

// New constructs a SQLite-backed `artifacts.ArtifactStore` against
// `cfg.DSN`. Production callers go through `artifacts.Open`; tests may
// call `New` directly to skip the registry.
//
// DSN handling:
//
//   - Empty DSN → clear error (no silent default-fallback).
//   - `:memory:` → translated to `file::memory:?cache=shared` so the
//     pool can hand out multiple connections to the same in-memory
//     database (the bare `:memory:` DSN gives every pool connection
//     its own private DB, which would break Put+Get round-trip across
//     pooled connections).
//   - Any other DSN is treated as a file path or URI form and passed
//     through verbatim with the WAL + busy_timeout PRAGMAs appended
//     as `_pragma` query params.
//
// Errors:
//
//   - empty `cfg.DSN`
//   - DSN that cannot be parsed for `_pragma` augmentation
//   - `sql.Open` failure (rare; modernc.org/sqlite's Open is lazy)
//   - migration apply failure
func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	if cfg.DSN == "" {
		return nil, errors.New(`artifacts/sqlite: empty DSN; expected file path or "sqlite:" URI`)
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("artifacts/sqlite: augment DSN: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("artifacts/sqlite: sql.Open(%q): %w", cfg.DSN, err)
	}

	// SQLite is a single-writer engine. Even with WAL +
	// `_txlock=immediate` + `busy_timeout`, a multi-connection pool
	// generates SQLITE_BUSY at BEGIN IMMEDIATE under high contention
	// (the busy handler runs inside the C engine, but `database/sql`
	// can hand out N connections that race for the writer lock).
	// Pinning the pool to a single connection serializes all access
	// at the Go layer — the driver thus matches the engine's
	// single-writer reality. This mirrors Phase 15's StateStore
	// settled choice.
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
		return nil, fmt.Errorf("artifacts/sqlite: migrate: %w", err)
	}

	return &driver{db: db}, nil
}

func init() {
	artifacts.Register("sqlite", New)
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
//     write, eliminating SQLITE_BUSY_SNAPSHOT under contention.
//
// The bare `:memory:` DSN is translated to a `file:`-form shared-cache
// URI so `database/sql`'s pool can hand out multiple connections to
// the same in-memory database.
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
	// but we tolerate it: the substring after the first `?` is treated
	// as an existing query string.
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

// verifyJournalMode reads back the journal mode after open to confirm
// the per-connection PRAGMA actually took effect. Disk-backed DSNs
// MUST report `wal`; `:memory:` (and shared-cache memory DSNs)
// degrade to `memory` mode by design.
func verifyJournalMode(ctx context.Context, db *sql.DB, originalDSN string) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("artifacts/sqlite: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		// Memory DBs degrade to "memory" journal mode — accepted.
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("artifacts/sqlite: journal_mode=%q after open; expected \"wal\" (DSN=%q)",
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

// driver is the SQLite-backed ArtifactStore. It is safe for concurrent
// use by N goroutines; mutable state is the `atomic.Bool` close flag
// (load-then-act pattern) and the underlying `*sql.DB` (internally
// synchronized by database/sql).
type driver struct {
	db     *sql.DB
	closed atomic.Bool
}

// Compile-time assertion that driver satisfies artifacts.ArtifactStore.
var _ artifacts.ArtifactStore = (*driver)(nil)

// PutBytes implements artifacts.ArtifactStore. Content-addressed:
// `ID = "{namespace}_{sha256[:12]}"`. Re-Put with identical
// (scope, namespace, bytes) returns the existing ref (no duplicate
// row inserted).
//
// SQL flow (single transaction):
//
//  1. SELECT the row at the composite PK. If present, the stored
//     row's SHA matches by construction (the id embeds the truncated
//     hash); return the existing ref.
//  2. Otherwise INSERT the new row using
//     `INSERT ... ON CONFLICT(...) DO NOTHING` so a concurrent Put
//     of the same (scope, namespace, bytes) doesn't fail with a PK
//     violation — the second writer falls through to a final SELECT
//     and returns the existing ref.
func (d *driver) PutBytes(ctx context.Context, scope artifacts.ArtifactScope, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return artifacts.ArtifactRef{}, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return artifacts.ArtifactRef{}, err
	}

	namespace := opts.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	mime := opts.MimeType
	if mime == "" {
		mime = defaultMimeBytes
	}

	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	id := fmt.Sprintf("%s_%s", namespace, hexDigest[:12])

	// Marshal Source up front. Non-encodable values fail loudly here
	// rather than after partial work commits. Per Phase 17's
	// documented behavior.
	sourceJSON, err := marshalSource(opts.Source)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: marshal source: %w", err)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()

	// Pre-insert HEAD check. Read existing row; if the scope+namespace+id
	// already holds a row, return that ref unchanged. SHA matches by
	// construction (the id embeds the truncated hash).
	existing, found, err := selectRefTx(ctx, tx, scope, id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: commit dedup no-op: %w", err)
		}
		committed = true
		return existing, nil
	}

	// INSERT ... ON CONFLICT DO NOTHING — a concurrent Put of the same
	// content-addressed key won't error; the loser falls through to
	// the post-INSERT SELECT below.
	const insert = `
        INSERT INTO artifacts_blobs
            (tenant, user, session, task, namespace, id,
             mime_type, size_bytes, filename, sha256, source_json, bytes)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, session, task, namespace, id) DO NOTHING`
	if _, err := tx.ExecContext(ctx, insert,
		scope.TenantID,
		scope.UserID,
		scope.SessionID,
		scope.TaskID,
		namespace,
		id,
		mime,
		int64(len(data)),
		opts.Filename,
		hexDigest,
		sourceJSON,
		data,
	); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: insert: %w", err)
	}

	// Re-SELECT inside the same transaction. If we just inserted, this
	// returns the row we wrote. If a concurrent Put won the race, it
	// returns the winning row. Either way we return a consistent ref.
	stored, found, err := selectRefTx(ctx, tx, scope, id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if !found {
		// Should be impossible — the INSERT ... DO NOTHING path leaves
		// either a row we wrote or one a concurrent writer wrote.
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: row vanished after insert: id=%q", id)
	}

	if err := tx.Commit(); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: commit: %w", err)
	}
	committed = true
	return stored, nil
}

// PutText implements artifacts.ArtifactStore.
func (d *driver) PutText(ctx context.Context, scope artifacts.ArtifactScope, text string, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if opts.MimeType == "" {
		opts.MimeType = defaultMimeText
	}
	return d.PutBytes(ctx, scope, []byte(text), opts)
}

// Get implements artifacts.ArtifactStore. Found-false is NOT an error.
func (d *driver) Get(ctx context.Context, scope artifacts.ArtifactScope, id string) ([]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}

	const sel = `
        SELECT bytes FROM artifacts_blobs
        WHERE tenant = ? AND user = ? AND session = ? AND task = ? AND id = ?
        LIMIT 1`
	row := d.db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)

	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("artifacts/sqlite: get: %w", err)
	}
	return data, true, nil
}

// GetRef implements artifacts.ArtifactStore. Found-false is NOT an
// error.
func (d *driver) GetRef(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}

	ref, found, err := selectRef(ctx, d.db, scope, id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	return &ref, true, nil
}

// Exists implements artifacts.ArtifactStore.
func (d *driver) Exists(ctx context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}

	const sel = `
        SELECT 1 FROM artifacts_blobs
        WHERE tenant = ? AND user = ? AND session = ? AND task = ? AND id = ?
        LIMIT 1`
	row := d.db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("artifacts/sqlite: exists: %w", err)
	}
	return true, nil
}

// Delete implements artifacts.ArtifactStore. Idempotent — `DELETE`
// against a missing row returns `(false, nil)`.
func (d *driver) Delete(ctx context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}

	const del = `
        DELETE FROM artifacts_blobs
        WHERE tenant = ? AND user = ? AND session = ? AND task = ? AND id = ?`
	res, err := d.db.ExecContext(ctx, del,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	if err != nil {
		return false, fmt.Errorf("artifacts/sqlite: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("artifacts/sqlite: rows affected: %w", err)
	}
	return n > 0, nil
}

// List implements artifacts.ArtifactStore. Empty fields in `filter`
// are wildcards.
//
// We construct the WHERE clause from the non-empty filter fields and
// pass each value as a bound parameter — never concatenate values
// into SQL (AGENTS.md §9).
func (d *driver) List(ctx context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
	}

	var (
		conds []string
		args  []any
	)
	if filter.TenantID != "" {
		conds = append(conds, "tenant = ?")
		args = append(args, filter.TenantID)
	}
	if filter.UserID != "" {
		conds = append(conds, "user = ?")
		args = append(args, filter.UserID)
	}
	if filter.SessionID != "" {
		conds = append(conds, "session = ?")
		args = append(args, filter.SessionID)
	}
	if filter.TaskID != "" {
		conds = append(conds, "task = ?")
		args = append(args, filter.TaskID)
	}

	q := `
        SELECT tenant, user, session, task, namespace, id,
               mime_type, size_bytes, filename, sha256, source_json
        FROM artifacts_blobs`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts/sqlite: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]artifacts.ArtifactRef, 0)
	for rows.Next() {
		ref, scanErr := scanRefRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts/sqlite: list iterate: %w", err)
	}
	return out, nil
}

// Close implements artifacts.ArtifactStore. Setting the atomic flag
// BEFORE `db.Close()` ensures concurrent in-flight callers observe
// `ErrStoreClosed` rather than racing into a half-closed pool. Close
// is idempotent — repeat calls are safe and return nil.
func (d *driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("artifacts/sqlite: close: %w", err)
	}
	return nil
}

// rowScanner is the minimal contract satisfied by both *sql.Row and
// *sql.Rows so scanRef* helpers serve both query shapes.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRefRow scans one row of the GetRef / List SELECT shape into an
// ArtifactRef. Source is decoded from the source_json BLOB column.
func scanRefRow(s rowScanner) (artifacts.ArtifactRef, error) {
	var (
		tenant, user, session, task   string
		namespace, id, mime, filename string
		sha256Hex                     string
		sizeBytes                     int64
		sourceJSON                    []byte
	)
	if err := s.Scan(
		&tenant, &user, &session, &task, &namespace, &id,
		&mime, &sizeBytes, &filename, &sha256Hex, &sourceJSON,
	); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: scan: %w", err)
	}
	source, err := unmarshalSource(sourceJSON)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/sqlite: decode source: %w", err)
	}
	return artifacts.ArtifactRef{
		ID:        id,
		MimeType:  mime,
		SizeBytes: sizeBytes,
		Filename:  filename,
		SHA256:    sha256Hex,
		Scope: artifacts.ArtifactScope{
			TenantID:  tenant,
			UserID:    user,
			SessionID: session,
			TaskID:    task,
		},
		Namespace: namespace,
		Source:    source,
	}, nil
}

// selectRef returns the ref at (scope, id) if present.
func selectRef(ctx context.Context, db *sql.DB, scope artifacts.ArtifactScope, id string) (artifacts.ArtifactRef, bool, error) {
	const sel = `
        SELECT tenant, user, session, task, namespace, id,
               mime_type, size_bytes, filename, sha256, source_json
        FROM artifacts_blobs
        WHERE tenant = ? AND user = ? AND session = ? AND task = ? AND id = ?
        LIMIT 1`
	row := db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	ref, err := scanRefRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return artifacts.ArtifactRef{}, false, nil
		}
		// The Scan error from a missing row may be wrapped — unwrap.
		var unwrap error = err
		for unwrap != nil {
			if errors.Is(unwrap, sql.ErrNoRows) {
				return artifacts.ArtifactRef{}, false, nil
			}
			unwrap = errors.Unwrap(unwrap)
		}
		return artifacts.ArtifactRef{}, false, err
	}
	return ref, true, nil
}

// selectRefTx is the in-transaction version of selectRef.
func selectRefTx(ctx context.Context, tx *sql.Tx, scope artifacts.ArtifactScope, id string) (artifacts.ArtifactRef, bool, error) {
	const sel = `
        SELECT tenant, user, session, task, namespace, id,
               mime_type, size_bytes, filename, sha256, source_json
        FROM artifacts_blobs
        WHERE tenant = ? AND user = ? AND session = ? AND task = ? AND id = ?
        LIMIT 1`
	row := tx.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	ref, err := scanRefRow(row)
	if err != nil {
		var unwrap error = err
		for unwrap != nil {
			if errors.Is(unwrap, sql.ErrNoRows) {
				return artifacts.ArtifactRef{}, false, nil
			}
			unwrap = errors.Unwrap(unwrap)
		}
		return artifacts.ArtifactRef{}, false, err
	}
	return ref, true, nil
}

// marshalSource encodes Source map[string]any to its `source_json`
// column representation. A nil map encodes to a JSON `null` literal so
// the column stays NOT NULL while still round-tripping to nil on
// decode.
func marshalSource(src map[string]any) ([]byte, error) {
	if src == nil {
		// Use the canonical 4-byte JSON null so unmarshalSource can
		// distinguish "no source" from an empty object.
		return []byte("null"), nil
	}
	out, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// unmarshalSource decodes `source_json` back into Source. JSON null
// (or a literal nil/empty buffer) produces a nil map.
func unmarshalSource(data []byte) (map[string]any, error) {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
