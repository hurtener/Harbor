// Package searchcache is Harbor's SQLite FTS5-backed tool search index.
// FTS5 search over each tool's name, description and tags plus the providing
// server's display title and description, ranked by bm25, with a substring
// fallback. The index is process-wide: every Search carries the caller's
// visibility predicate, applied BEFORE the limit, so one principal's rows
// can neither be returned to another nor crowd its results out.
//
// The index is a mirror of the in-memory catalog (the catalog is its only
// writer and reconciles it on every change), so it is emptied at open and
// never outlives the process's own registrations.
//
// The driver self-registers under "searchcache" from its init().
// The production binary picks it up via blank import in
// cmd/harbor/main.go.
package searchcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/tools"
)

const (
	driverName    = "searchcache"
	sqliteDriver  = "sqlite"
	busyTimeoutMs = 5000

	defaultSearchN = 20
	maxSearchN     = 200
)

var errStoreClosed = errors.New("searchcache: store closed")

// New constructs a SQLite-backed SearchCache against cfg.DSN.
func New(cfg Config) (SearchCache, error) {
	if cfg.DSN == "" {
		return nil, errors.New("searchcache: empty DSN")
	}

	dsn, err := augmentDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("searchcache: augment DSN: %w", err)
	}

	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("searchcache: sql.Open(%q): %w", cfg.DSN, err)
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("searchcache: migrate: %w", err)
	}
	// The index mirrors THIS process's catalog, which registers every tool
	// afresh at boot. Rows persisted by an earlier process (an on-disk DSN)
	// describe sources this process may never attach, so they are dropped.
	if _, err := db.ExecContext(ctx, `DELETE FROM tool_cache`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("searchcache: reset: %w", err)
	}

	ftsAvail := detectFTS5(ctx, db)

	return &driver{
		db:           db,
		ftsAvailable: ftsAvail,
	}, nil
}

// Config carries the SearchCache driver configuration.
type Config struct {
	DSN string
}

// SearchCache is the tool search index. Production callers reach
// this through the tools.Catalog and never construct a driver
// directly.
type SearchCache interface {
	Search(ctx context.Context, req tools.ToolSearchRequest) ([]tools.Tool, error)
	Sync(ctx context.Context, tools []tools.Tool) error
	Remove(ctx context.Context, names []string) error
	Close() error
}

var _ tools.ToolSearchCache = (*driver)(nil)

type driver struct {
	db           *sql.DB
	ftsAvailable bool
	closed       atomic.Bool
}

// Search runs the FTS5 (bm25-ranked) → substring ladder. Only rows that
// req.Visible admits and that carry every req.Tags entry count toward the
// limit, so up to limit admitted matches are returned when they exist. A nil
// Visible admits nothing.
func (d *driver) Search(ctx context.Context, req tools.ToolSearchRequest) ([]tools.Tool, error) {
	if d.closed.Load() {
		return nil, errStoreClosed
	}
	if req.Visible == nil {
		return nil, nil
	}
	query := strings.TrimSpace(req.Query)
	limit := req.Limit
	if limit <= 0 || limit > maxSearchN {
		limit = defaultSearchN
	}
	admit := func(t tools.Tool) bool {
		return req.Visible(t.Name) && matchesTagsFilter(t.Tags, req.Tags)
	}
	if query == "" {
		return d.scan(ctx, limit, admit)
	}
	if d.ftsAvailable {
		results, err := d.searchFTS5(ctx, query, limit, admit)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
	}
	return d.searchSubstring(ctx, query, limit, admit)
}

// Sync upserts the tool set into the cache in one transaction.
func (d *driver) Sync(ctx context.Context, toolList []tools.Tool) error {
	if d.closed.Load() {
		return errStoreClosed
	}
	if len(toolList) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("searchcache: begin sync: %w", err)
	}
	for _, t := range toolList {
		if err := upsertTool(ctx, tx, t); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Remove deletes the named rows in one transaction; absent names are
// ignored. The delete trigger keeps the FTS5 index in step.
func (d *driver) Remove(ctx context.Context, names []string) error {
	if d.closed.Load() {
		return errStoreClosed
	}
	if len(names) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("searchcache: begin remove: %w", err)
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_cache WHERE name = ?`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("searchcache: remove: %w", err)
		}
	}
	return tx.Commit()
}

// Close shuts down the underlying SQLite connection.
func (d *driver) Close() error {
	if d.closed.CompareAndSwap(false, true) {
		return d.db.Close()
	}
	return nil
}

// Bounds on provider-supplied server metadata stored per row. The catalog
// already bounds what the driver stamps; these keep the index bounded even
// for a provider that does not.
const (
	maxServerTitleBytes       = 256
	maxServerDescriptionBytes = 1024
)

func upsertTool(ctx context.Context, tx *sql.Tx, t tools.Tool) error {
	tagsJSON := marshalStrings(t.Tags)
	schemaJSON := string(t.ArgsSchema)
	const q = `
        INSERT INTO tool_cache(name, description, tags, args_schema, source, server_title, server_description, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
        ON CONFLICT(name) DO UPDATE SET
            description        = excluded.description,
            tags               = excluded.tags,
            args_schema        = excluded.args_schema,
            source             = excluded.source,
            server_title       = excluded.server_title,
            server_description = excluded.server_description,
            updated_at         = datetime('now')`
	_, err := tx.ExecContext(ctx, q, t.Name, t.Description, tagsJSON, schemaJSON, string(t.Source),
		truncateUTF8(t.SourceTitle, maxServerTitleBytes), truncateUTF8(t.SourceDescription, maxServerDescriptionBytes))
	if err != nil {
		return fmt.Errorf("searchcache: upsert: %w", err)
	}
	return nil
}
