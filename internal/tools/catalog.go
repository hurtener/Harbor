package tools

import (
	"sort"
	"sync"
)

// catalog is the canonical in-memory ToolCatalog implementation.
// Concurrent reuse (D-025): the mutex is a RWMutex so List + Resolve
// scale; Register is rare and takes the write side. Descriptors are
// immutable after Register so List can return Tool *values*
// (copies) without locking the underlying descriptor.
type catalog struct {
	byName map[string]ToolDescriptor
	mu     sync.RWMutex
}

// CatalogOption configures a catalog at construction.
type CatalogOption func(*catalogConfig)

type catalogConfig struct {
	// reserved for future config knobs (e.g. capacity caps).
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
		byName: make(map[string]ToolDescriptor),
	}
}

func (c *catalog) Register(d ToolDescriptor) error {
	if d.Tool.Name == "" {
		return wrap(ErrToolDuplicateName, "tool name is empty")
	}
	if d.Invoke == nil {
		return wrap(ErrToolDuplicateName, "tool %q has nil Invoke", d.Tool.Name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.byName[d.Tool.Name]; exists {
		return wrap(ErrToolDuplicateName, "name=%q", d.Tool.Name)
	}
	c.byName[d.Tool.Name] = d
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
