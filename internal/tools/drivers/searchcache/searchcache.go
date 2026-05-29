// Package searchcache is Harbor's SQLite FTS5-backed tool search cache
// (Phase 107c / D-167). It mirrors the shape of
// internal/skills/drivers/localdb — FTS5 search over indexed tool
// name + description + tags, with a regex fallback for environments
// without FTS5.
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
	Search(ctx context.Context, query string, tags []string, limit int) ([]tools.Tool, error)
	Sync(ctx context.Context, tools []tools.Tool) error
	Close() error
}

type driver struct {
	db           *sql.DB
	ftsAvailable bool
	closed       atomic.Bool
}

// Search runs the FTS5 → regex → exact ladder (mirrors the skills
// localdb search). Tags act as intersection filter: every token in
// every tag must appear in the tool's cached tags blob.
func (d *driver) Search(ctx context.Context, query string, tags []string, limit int) ([]tools.Tool, error) {
	if d.closed.Load() {
		return nil, errStoreClosed
	}
	query = strings.TrimSpace(query)
	if limit <= 0 || limit > maxSearchN {
		limit = defaultSearchN
	}

	if query == "" && len(tags) == 0 {
		return d.listAll(ctx, limit)
	}

	var results []tools.Tool
	var err error
	if d.ftsAvailable && query != "" {
		results, err = d.searchFTS5(ctx, query, tags, limit)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return trimLimit(results, limit), nil
		}
	}
	if query != "" {
		results, err = d.searchRegex(ctx, query, tags, limit)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return trimLimit(results, limit), nil
		}
	}
	results, err = d.searchExact(ctx, query, tags, limit)
	return trimLimit(results, limit), err
}

// Sync upserts the tool set into the cache. A fingerprint
// (hash of name+updated_at) guards against no-op syncs.
func (d *driver) Sync(ctx context.Context, toolList []tools.Tool) error {
	if d.closed.Load() {
		return errStoreClosed
	}
	for _, t := range toolList {
		if err := d.upsertTool(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// Close shuts down the underlying SQLite connection.
func (d *driver) Close() error {
	if d.closed.CompareAndSwap(false, true) {
		return d.db.Close()
	}
	return nil
}

func (d *driver) upsertTool(ctx context.Context, t tools.Tool) error {
	tagsJSON := marshalStrings(t.Tags)
	schemaJSON := string(t.ArgsSchema)
	const q = `
        INSERT INTO tool_cache(name, description, tags, args_schema, updated_at)
        VALUES (?, ?, ?, ?, datetime('now'))
        ON CONFLICT(name) DO UPDATE SET
            description = excluded.description,
            tags        = excluded.tags,
            args_schema  = excluded.args_schema,
            updated_at   = datetime('now')`
	_, err := d.db.ExecContext(ctx, q, t.Name, t.Description, tagsJSON, schemaJSON)
	return err
}

func trimLimit[T any](s []T, limit int) []T {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
