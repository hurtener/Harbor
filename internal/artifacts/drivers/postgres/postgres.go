// Package postgres is Harbor's V1 Postgres-backed ArtifactStore driver.
//
// It is the fourth leg of the artifact persistence triad (in-memory
// floor, FS, SQLite, Postgres) defined by RFC §6.10 + §9. Phase 18
// inherits `internal/artifacts/conformancetest.Run` verbatim — the
// suite IS the gate; this driver ships zero new conformance scenarios.
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Parametric queries everywhere; no string
// concatenation into SQL (AGENTS.md §9). Advisory locks serialise
// the migration runner so multi-replica boots are race-free; the lock
// key is FNV-64a("harbor-artifacts-migrations"), distinct from Phase
// 16's StateStore lock so the two subsystems do not serialise against
// each other.
//
// Internal model:
//
//   - One row per (tenant, user, session, task, namespace, id). The
//     composite primary key is the artifact scope plus namespace + id.
//     `task` may be empty (session-scoped artifacts); the column is
//     NOT NULL but accepts the empty string.
//   - `bytes` is BYTEA — opaque payload, no JSONB constraint.
//   - `source_json` is BYTEA holding `json.Marshal`ed `Source
//     map[string]any` from `PutOpts`.
//   - Put is a transactional flow: pre-INSERT SELECT of the existing
//     row (returns it unchanged on dedup) followed by `INSERT ...
//     ON CONFLICT DO NOTHING` followed by a final SELECT inside the
//     same transaction. Concurrent writers of identical content land
//     on the same row.
//   - `Close(ctx)` flips an atomic flag BEFORE calling `db.Close()`
//     so subsequent calls fast-fail with `ErrStoreClosed` even while
//     in-flight queries are draining.
//
// Per AGENTS.md §5 (D-025), the driver is safe for concurrent reuse
// across N goroutines. The conformance suite's
// `Concurrent_PutGet_NoRace` + the local `concurrent_test.go` enforce
// this under -race.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
)

// driverName is the name under which this driver self-registers.
const driverName = "postgres"

// pgxDriverName is the database/sql driver name registered by the
// pgx stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Mirrors Phase 16's StateStore choices.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// Postgres SQLSTATE codes mapped at the boundary so callers compare
// against artifacts.* sentinels, never raw pgx errors.
const (
	pgUniqueViolation = "23505"
	pgDeadlockFound   = "40P01"
)

// Defaults applied when callers omit metadata.
const (
	defaultNamespace = "default"
	defaultMimeBytes = "application/octet-stream"
	defaultMimeText  = "text/plain; charset=utf-8"
)

// New constructs a Postgres-backed artifacts.ArtifactStore against
// cfg.DSN. Production callers go through artifacts.Open; tests may
// call New directly to skip the registry.
//
// Errors:
//   - empty cfg.DSN
//   - sql.Open / migration apply failure
//   - advisory-lock acquisition failure (extremely unusual; would
//     indicate severe DB load or operator misconfiguration)
func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	if cfg.DSN == "" {
		return nil, errors.New("artifacts/postgres: cfg.DSN is required")
	}
	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("artifacts/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first Put.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifacts/postgres: ping: %w", err)
	}

	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &driver{db: db}, nil
}

func init() {
	artifacts.Register(driverName, New)
}

// driver is the Postgres-backed artifacts.ArtifactStore implementation.
//
// Fields are immutable after construction except for the atomic
// `closed` flag (D-025: compiled artifacts are immutable; per-run
// state lives in ctx).
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
//  1. SELECT the row at the composite PK. If present, return the
//     existing ref unchanged (the SHA matches by construction since
//     the id embeds the truncated hash).
//  2. Otherwise INSERT the new row using
//     `INSERT ... ON CONFLICT (...) DO NOTHING` so a concurrent Put
//     of the same content-addressed id under the same scope doesn't
//     fail with a PK violation — the loser falls through to the
//     final SELECT and returns the existing (winning) ref.
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
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/postgres: marshal source: %w", err)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return artifacts.ArtifactRef{}, d.translateErr(err, "artifacts/postgres: begin tx")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; surfaced error is the original failure
		}
	}()

	// Pre-insert HEAD check. Read existing row; if the scope+namespace+id
	// already holds a row, return that ref unchanged.
	existing, found, err := selectRefTx(ctx, tx, scope, id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return artifacts.ArtifactRef{}, d.translateErr(err, "artifacts/postgres: commit dedup no-op")
		}
		committed = true
		return existing, nil
	}

	// INSERT ... ON CONFLICT DO NOTHING — a concurrent Put of the same
	// content-addressed key won't error; the loser falls through to
	// the final SELECT below.
	const insert = `
		INSERT INTO artifacts_blobs
			(tenant, "user", session, task, namespace, id,
			 mime_type, size_bytes, filename, sha256, source_json, bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant, "user", session, task, namespace, id) DO NOTHING
	`
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
		return artifacts.ArtifactRef{}, d.translateInsertErr(err)
	}

	// Re-SELECT inside the same transaction. If we just inserted, this
	// returns the row we wrote. If a concurrent Put won the race, it
	// returns the winning row. Either way we return a consistent ref.
	stored, found, err := selectRefTx(ctx, tx, scope, id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if !found {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/postgres: row vanished after insert: id=%q", id)
	}

	if err := tx.Commit(); err != nil {
		return artifacts.ArtifactRef{}, d.translateErr(err, "artifacts/postgres: commit insert")
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
		WHERE tenant = $1 AND "user" = $2 AND session = $3 AND task = $4 AND id = $5
	`
	row := d.db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)

	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, d.translateErr(err, "artifacts/postgres: get")
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
		return nil, false, d.translateErr(err, "artifacts/postgres: get ref")
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
		WHERE tenant = $1 AND "user" = $2 AND session = $3 AND task = $4 AND id = $5
		LIMIT 1
	`
	row := d.db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, d.translateErr(err, "artifacts/postgres: exists")
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
		WHERE tenant = $1 AND "user" = $2 AND session = $3 AND task = $4 AND id = $5
	`
	res, err := d.db.ExecContext(ctx, del,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	if err != nil {
		return false, d.translateErr(err, "artifacts/postgres: delete")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("artifacts/postgres: rows affected: %w", err)
	}
	return n > 0, nil
}

// List implements artifacts.ArtifactStore. Empty fields in `filter`
// are wildcards.
//
// The WHERE clause is built from the non-empty filter fields and each
// value is passed as a bound parameter — never concatenate values
// into SQL (AGENTS.md §9).
func (d *driver) List(ctx context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
	}

	var (
		conds []string
		args  []any
	)
	addCond := func(col, val string) {
		if val == "" {
			return
		}
		conds = append(conds, fmt.Sprintf(`%s = $%d`, col, len(args)+1))
		args = append(args, val)
	}
	addCond("tenant", filter.TenantID)
	addCond(`"user"`, filter.UserID)
	addCond("session", filter.SessionID)
	addCond("task", filter.TaskID)

	q := `
		SELECT tenant, "user", session, task, namespace, id,
			   mime_type, size_bytes, filename, sha256, source_json
		FROM artifacts_blobs
	`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, d.translateErr(err, "artifacts/postgres: list")
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
		return nil, fmt.Errorf("artifacts/postgres: list iterate: %w", err)
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
		return fmt.Errorf("artifacts/postgres: db.Close: %w", err)
	}
	return nil
}

// rowScanner is the minimal contract satisfied by both *sql.Row and
// *sql.Rows so scanRefRow serves both query shapes.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRefRow scans one row of the GetRef / List SELECT shape into an
// ArtifactRef. Source is decoded from the source_json BYTEA column.
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
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/postgres: scan: %w", err)
	}
	source, err := unmarshalSource(sourceJSON)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/postgres: decode source: %w", err)
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

// selectRef returns the ref at (scope, id) if present. Found-false is
// not an error.
func selectRef(ctx context.Context, db *sql.DB, scope artifacts.ArtifactScope, id string) (artifacts.ArtifactRef, bool, error) {
	const sel = `
		SELECT tenant, "user", session, task, namespace, id,
			   mime_type, size_bytes, filename, sha256, source_json
		FROM artifacts_blobs
		WHERE tenant = $1 AND "user" = $2 AND session = $3 AND task = $4 AND id = $5
	`
	row := db.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	ref, err := scanRefRow(row)
	if err != nil {
		if errIsNoRows(err) {
			return artifacts.ArtifactRef{}, false, nil
		}
		return artifacts.ArtifactRef{}, false, err
	}
	return ref, true, nil
}

// selectRefTx is the in-transaction version of selectRef.
func selectRefTx(ctx context.Context, tx *sql.Tx, scope artifacts.ArtifactScope, id string) (artifacts.ArtifactRef, bool, error) {
	const sel = `
		SELECT tenant, "user", session, task, namespace, id,
			   mime_type, size_bytes, filename, sha256, source_json
		FROM artifacts_blobs
		WHERE tenant = $1 AND "user" = $2 AND session = $3 AND task = $4 AND id = $5
	`
	row := tx.QueryRowContext(ctx, sel,
		scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, id)
	ref, err := scanRefRow(row)
	if err != nil {
		if errIsNoRows(err) {
			return artifacts.ArtifactRef{}, false, nil
		}
		return artifacts.ArtifactRef{}, false, err
	}
	return ref, true, nil
}

// errIsNoRows reports whether err wraps `sql.ErrNoRows` at any depth.
// scanRefRow wraps the original Scan error so callers can't simply
// `errors.Is` against `sql.ErrNoRows` once; the helper unwraps.
func errIsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// translateErr maps low-level driver errors to Harbor sentinels at
// the boundary. Callers compare via errors.Is against artifacts.*;
// raw pgx errors must never leak.
func (d *driver) translateErr(err error, ctxMsg string) error {
	if err == nil {
		return nil
	}
	// If we have already closed (or are racing with Close), surface
	// ErrStoreClosed so the caller sees the canonical sentinel.
	if d.closed.Load() {
		return artifacts.ErrStoreClosed
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("%s: %w", ctxMsg, artifacts.ErrStoreClosed)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
}

// translateInsertErr maps Postgres-specific INSERT errors to Harbor
// sentinels. With ON CONFLICT DO NOTHING the unique-violation path
// is normally absorbed; this helper exists for the rare cases where
// an unrelated constraint fires (e.g. a deadlock under heavy
// contention).
func (d *driver) translateInsertErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			// ON CONFLICT DO NOTHING should absorb this; if we still
			// see it the constraint hit a column outside the PK.
			// Surface as a generic insert error so the caller can see
			// the underlying constraint name in the message.
			return fmt.Errorf("artifacts/postgres: insert unique violation: %v", pgErr.Message)
		case pgDeadlockFound:
			return fmt.Errorf("artifacts/postgres: insert deadlock: %w", err)
		}
	}
	return d.translateErr(err, "artifacts/postgres: insert")
}

// marshalSource encodes Source map[string]any to its `source_json`
// column representation. A nil map encodes to a JSON `null` literal so
// the column stays NOT NULL while still round-tripping to nil on
// decode.
func marshalSource(src map[string]any) ([]byte, error) {
	if src == nil {
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
