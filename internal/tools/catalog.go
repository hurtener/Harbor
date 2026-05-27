package tools

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// catalog is the canonical in-memory ToolCatalog implementation.
// Concurrent reuse (D-025): the mutex is a RWMutex so List + Resolve
// scale; Register is rare and takes the write side. Descriptors are
// immutable after Register so List can return Tool *values*
// (copies) without locking the underlying descriptor.
type catalog struct {
	mu     sync.RWMutex
	byName map[string]ToolDescriptor

	// searchCache (Phase 107c / D-167) is the optional FTS5-backed
	// search index. nil means Search returns empty (honest "discovery
	// unavailable"). Set via WithSearchCache option at construction.
	searchCache ToolSearchCache
}

// ToolSearchCache is the tool search index surface the catalog
// delegates to. Defined here to avoid import cycles between
// internal/tools and internal/tools/drivers/searchcache.
type ToolSearchCache interface {
	Search(ctx context.Context, query string, tags []string, limit int) ([]Tool, error)
	Sync(ctx context.Context, tools []Tool) error
	Close() error
}

type catalogConfig struct {
	searchCache ToolSearchCache
}

// CatalogOption configures a catalog at construction.
type CatalogOption func(*catalogConfig)

// WithSearchCache attaches a search index to the catalog (Phase
// 107c / D-167).
func WithSearchCache(sc ToolSearchCache) CatalogOption {
	return func(cfg *catalogConfig) {
		cfg.searchCache = sc
	}
}

// NewCatalog constructs the canonical in-memory ToolCatalog. Safe
// for N concurrent goroutines after construction (D-025). The
// catalog is empty; callers register descriptors via Register.
//
// `opts` is reserved for future configuration; Phase 26 ships
// without options but the variadic surface is stable so future
// fields land without breaking signatures.
func NewCatalog(opts ...CatalogOption) ToolCatalog {
	cfg := catalogConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &catalog{
		byName:      make(map[string]ToolDescriptor),
		searchCache: cfg.searchCache,
	}
}

func (c *catalog) Register(d ToolDescriptor) error {
	if d.Tool.Name == "" {
		return wrap(ErrToolDuplicateName, "tool name is empty")
	}
	if d.Invoke == nil {
		return wrap(ErrToolDuplicateName, "tool %q has nil Invoke", d.Tool.Name)
	}
	// Phase 83b (D-144): a curated example whose Args name a key the
	// tool's args_schema does not declare would teach the planner a
	// shape the catalog edge then rejects. Fail loudly at registration.
	if err := validateExamples(d.Tool); err != nil {
		return err
	}
	c.mu.Lock()
	if _, exists := c.byName[d.Tool.Name]; exists {
		c.mu.Unlock()
		return wrap(ErrToolDuplicateName, "name=%q", d.Tool.Name)
	}
	c.byName[d.Tool.Name] = d
	sc := c.searchCache
	c.mu.Unlock()

	// Phase 107c / D-167 — propagate registration to the SearchCache
	// (AC-9 sync hook). Use Background ctx with a brief timeout so
	// registration latency on the FTS5 path stays bounded; an error
	// here is logged at warn (the registration itself succeeded; a
	// missing cache row only degrades discovery, not dispatch).
	if sc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sc.Sync(ctx, []Tool{d.Tool}); err != nil {
			slog.Warn("tools/catalog: searchCache.Sync failed",
				slog.String("tool", d.Tool.Name),
				slog.String("err", err.Error()))
		}
	}
	return nil
}

func (c *catalog) Resolve(name string) (ToolDescriptor, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.byName[name]
	return d, ok
}

func (c *catalog) List(filter CatalogFilter) []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Tool, 0, len(c.byName))
	for _, d := range c.byName {
		if filter.matches(d.Tool) {
			out = append(out, d.Tool)
		}
	}
	// Deterministic order: sort by name. Lets callers / tests
	// compare slice content without flakiness from map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Replace implements the CatalogReplacer interface. It atomically
// swaps each named descriptor in `wrapped` with its wrapped version
// under the catalog's write lock — concurrent Resolve / List callers
// see either every old descriptor OR every new descriptor, never a
// partial mix.
//
// Phase 64a / D-090 — the catalog wiring builder calls this once at
// boot AFTER every underlying tool registration has landed.
//
// Replace returns ErrToolNotFound (wrapped) when any `wrapped[i]`
// names a tool not currently in the catalog. In that case NO
// replacement happens — the failure is all-or-nothing.
func (c *catalog) Replace(wrapped []ToolDescriptor) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Validate first; mutate second.
	for _, d := range wrapped {
		if d.Tool.Name == "" {
			return wrap(ErrToolNotFound, "Replace: descriptor has empty Tool.Name")
		}
		if d.Invoke == nil {
			return wrap(ErrToolNotFound, "Replace: descriptor %q has nil Invoke", d.Tool.Name)
		}
		if _, exists := c.byName[d.Tool.Name]; !exists {
			return wrap(ErrToolNotFound, "Replace: tool %q is not registered", d.Tool.Name)
		}
	}
	for _, d := range wrapped {
		c.byName[d.Tool.Name] = d
	}
	return nil
}

// Search implements ToolCatalog (Phase 107c / D-167). Delegates to
// the attached SearchCache when present; returns an empty slice when
// no cache is configured (honest "discovery unavailable" — no panic).
//
// A cache error logs at warn and returns an empty slice — the
// signature can't propagate (planners pass through `RunContext.Catalog`,
// which is also error-free by design). Operators watch
// `tools.search_cache_error` log entries to detect cache health.
func (c *catalog) Search(ctx context.Context, query string, tags []string, limit int) []Tool {
	c.mu.RLock()
	sc := c.searchCache
	c.mu.RUnlock()
	if sc == nil {
		return nil
	}
	results, err := sc.Search(ctx, query, tags, limit)
	if err != nil {
		slog.Warn("tools/catalog: search cache returned an error",
			slog.String("query", query),
			slog.Any("tags", tags),
			slog.Int("limit", limit),
			slog.String("err", err.Error()))
		return nil
	}
	return results
}
