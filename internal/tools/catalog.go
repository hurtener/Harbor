package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// catalog is the canonical in-memory ToolCatalog implementation.
// Concurrent reuse: the mutex is a RWMutex so List + Resolve
// scale; Register is rare and takes the write side. Descriptors are
// immutable after Register so List can return Tool *values*
// (copies) without locking the underlying descriptor.
type catalog struct {
	mu                sync.RWMutex
	byName            map[string]ToolDescriptor
	sourceGenerations map[ToolSourceID]uint64
	// epoch is minted once at construction; generation is bumped under mu
	// (write) wherever sourceGenerations moves. Both back the optional
	// CatalogGenerationReporter surface.
	epoch      string
	generation atomic.Uint64

	// searchCache is the optional FTS5-backed
	// search index. nil means Search returns empty (honest "discovery
	// unavailable"). Set via WithSearchCache option at construction.
	searchCache ToolSearchCache
	// searchMu serializes search-index reconciliation. Each reconcile reads
	// the CURRENT catalog state for the touched names and applies it, so
	// serialized reconciles converge the index on the latest catalog state
	// regardless of the order concurrent mutations finish in.
	searchMu sync.Mutex
	// searchDirty holds names whose last index write failed (guarded by
	// searchMu, bounded by maxSearchDirty). They are retried on the next
	// reconcile or Search; searchDirtyN mirrors its size for a lock-free
	// check on the Search path.
	searchDirty  map[string]struct{}
	searchDirtyN atomic.Int64

	// bus is the optional canonical event bus. When set, Register wraps
	// every registered descriptor's Invoke in the ONE universal
	// lifecycle-emitting shell (tool.invoked / .completed / .failed /
	// .invalid_args / .policy_exhausted), so all four dispatch call
	// sites inherit tool-lifecycle events by construction — for EVERY
	// transport, not just in-process. nil keeps the descriptor's Invoke
	// verbatim (a pure no-op catalog, used by tests that don't observe
	// the lifecycle surface). Set via WithCatalogBus at construction.
	bus events.EventBus
}

// ToolSearchCache is the tool search index surface the catalog
// delegates to. Defined here to avoid import cycles between
// internal/tools and internal/tools/drivers/searchcache.
//
// The catalog is the index's only writer and keeps it a mirror of the
// current catalog: every registration, replacement, source publication,
// rollback and deregistration reconciles the touched names (upsert when
// present, Remove when absent), so no row outlives its tool.
type ToolSearchCache interface {
	// Search returns up to req.Limit rows admitted by req.Visible, ordered
	// by relevance. Visibility is applied before the limit. Returned rows
	// carry Name and Tags only; callers materialize everything else from
	// their own authoritative view.
	Search(ctx context.Context, req ToolSearchRequest) ([]Tool, error)
	// Sync upserts the given tools.
	Sync(ctx context.Context, tools []Tool) error
	// Remove deletes the named rows; absent names are ignored.
	Remove(ctx context.Context, names []string) error
	Close() error
}

type catalogConfig struct {
	searchCache ToolSearchCache
	bus         events.EventBus
}

// CatalogOption configures a catalog at construction.
type CatalogOption func(*catalogConfig)

// WithSearchCache attaches a search index to the catalog (
// ).
func WithSearchCache(sc ToolSearchCache) CatalogOption {
	return func(cfg *catalogConfig) {
		cfg.searchCache = sc
	}
}

// WithCatalogBus wires the canonical event bus into the catalog so
// Register wraps every registered descriptor's Invoke in the universal
// tool-lifecycle-emitting shell. This is the single seam where tool
// lifecycle events (tool.invoked / .completed / .failed / .invalid_args
// / .policy_exhausted) are produced — every dispatch path that resolves
// and invokes a descriptor inherits them by construction, for every
// transport. A nil bus is the same as not supplying the option (no
// shell). Production wiring supplies the runtime's bus.
func WithCatalogBus(bus events.EventBus) CatalogOption {
	return func(cfg *catalogConfig) {
		cfg.bus = bus
	}
}

// NewCatalog constructs the canonical in-memory ToolCatalog. Safe
// for N concurrent goroutines after construction. The
// catalog is empty; callers register descriptors via Register.
//
// `opts` is reserved for future configuration; Harbor ships
// without options but the variadic surface is stable so future
// fields land without breaking signatures.
func NewCatalog(opts ...CatalogOption) ToolCatalog {
	cfg := catalogConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &catalog{
		byName:            make(map[string]ToolDescriptor),
		sourceGenerations: make(map[ToolSourceID]uint64),
		epoch:             newCatalogEpoch(),
		searchCache:       cfg.searchCache,
		bus:               cfg.bus,
	}
}

func (c *catalog) Register(d ToolDescriptor) error {
	if d.Tool.Name == "" {
		return wrap(ErrToolDuplicateName, "tool name is empty")
	}
	if d.Invoke == nil {
		return wrap(ErrToolDuplicateName, "tool %q has nil Invoke", d.Tool.Name)
	}
	// a curated example whose Args name a key the
	// tool's args_schema does not declare would teach the planner a
	// shape the catalog edge then rejects. Fail loudly at registration.
	if err := validateExamples(d.Tool); err != nil {
		return err
	}
	// Wrap the descriptor's Invoke in the universal lifecycle-emitting
	// shell (no-op when the catalog carries no bus). Applied ONCE here at
	// registration so every dispatch path that later resolves this
	// descriptor inherits tool.invoked/.completed/.failed by
	// construction — for every transport. Replace (the wiring builder's
	// approval/oauth re-install) does NOT re-wrap: it swaps in
	// descriptors whose inner Invoke is already this shelled one, so
	// there is no double emission.
	d = wrapDescriptorLifecycle(c.bus, d)

	c.mu.Lock()
	if _, exists := c.byName[d.Tool.Name]; exists {
		c.mu.Unlock()
		return wrap(ErrToolDuplicateName, "name=%q", d.Tool.Name)
	}
	c.byName[d.Tool.Name] = d
	c.sourceGenerations[d.Tool.Source]++
	c.generation.Add(1)
	c.mu.Unlock()

	// Propagate registration to the SearchCache (AC-9 sync hook). An index
	// failure is logged at warn: the registration itself succeeded and a
	// missing row only degrades ranked discovery, never dispatch.
	c.reconcileSearch("register", []string{d.Tool.Name})
	return nil
}

// maxSearchDirty bounds the retry set of names whose index write failed.
// Beyond it a failure is only logged: the index is an accelerator, and
// tool_search merges an in-memory pass over the run's own view whenever the
// index returns fewer than the requested results.
const maxSearchDirty = 4096

// reconcileSearch brings the search index in line with the CURRENT catalog
// state for names (plus any earlier failed names): a present name is
// upserted with its current Tool, an absent name is removed. Runs outside
// c.mu (index I/O never blocks Resolve/List) and under searchMu, so
// concurrent reconciles cannot apply an older snapshot after a newer one.
// Bounded by a short timeout; a failed write marks its names dirty for
// retry rather than leaving a silent gap.
func (c *catalog) reconcileSearch(op string, names []string) {
	if c.searchCache == nil || (len(names) == 0 && c.searchDirtyN.Load() == 0) {
		return
	}
	c.searchMu.Lock()
	defer c.searchMu.Unlock()
	c.reconcileSearchLocked(context.Background(), op, names)
}

// retrySearchDirty retries failed index writes without waiting for another
// reconcile (a Search that finds one in progress simply proceeds). The
// retry is bounded by the caller's ctx, so a cancelled or short-deadline
// search is never held by index I/O; names a cancelled retry could not
// write stay dirty.
func (c *catalog) retrySearchDirty(ctx context.Context) {
	if c.searchCache == nil || c.searchDirtyN.Load() == 0 || ctx.Err() != nil || !c.searchMu.TryLock() {
		return
	}
	defer c.searchMu.Unlock()
	c.reconcileSearchLocked(ctx, "retry", nil)
}

// reconcileSearchLocked does the work under parent (additionally bounded by
// a short timeout); caller holds searchMu.
func (c *catalog) reconcileSearchLocked(parent context.Context, op string, names []string) {
	sc := c.searchCache
	if len(c.searchDirty) > 0 {
		for name := range c.searchDirty {
			names = append(names, name)
		}
		c.searchDirty = nil
		c.searchDirtyN.Store(0)
	}
	if len(names) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(names))
	upserts := make([]Tool, 0, len(names))
	var removals []string
	c.mu.RLock()
	for _, name := range names {
		if _, dup := seen[name]; dup || name == "" {
			continue
		}
		seen[name] = struct{}{}
		if d, ok := c.byName[name]; ok {
			upserts = append(upserts, d.Tool)
		} else {
			removals = append(removals, name)
		}
	}
	c.mu.RUnlock()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if len(removals) > 0 {
		if err := sc.Remove(ctx, removals); err != nil {
			slog.Warn("tools/catalog: searchCache.Remove failed; will retry",
				slog.String("op", op), slog.Int("names", len(removals)), slog.String("err", err.Error()))
			c.markSearchDirty(removals)
		}
	}
	if len(upserts) > 0 {
		if err := sc.Sync(ctx, upserts); err != nil {
			slog.Warn("tools/catalog: searchCache.Sync failed; will retry",
				slog.String("op", op), slog.Int("names", len(upserts)), slog.String("err", err.Error()))
			dirty := make([]string, 0, len(upserts))
			for _, t := range upserts {
				dirty = append(dirty, t.Name)
			}
			c.markSearchDirty(dirty)
		}
	}
}

// markSearchDirty records names for retry, up to maxSearchDirty. Caller
// holds searchMu.
func (c *catalog) markSearchDirty(names []string) {
	if c.searchDirty == nil {
		c.searchDirty = make(map[string]struct{}, min(len(names), maxSearchDirty))
	}
	dropped := 0
	for _, name := range names {
		if _, ok := c.searchDirty[name]; ok {
			continue
		}
		if len(c.searchDirty) >= maxSearchDirty {
			dropped++
			continue
		}
		c.searchDirty[name] = struct{}{}
	}
	c.searchDirtyN.Store(int64(len(c.searchDirty)))
	if dropped > 0 {
		slog.Warn("tools/catalog: search index retry set full; dropped names rely on the in-memory search fallback",
			slog.Int("dropped", dropped))
	}
}

// Epoch implements CatalogGenerationReporter.
func (c *catalog) Epoch() string { return c.epoch }

// Generation implements CatalogGenerationReporter. The counter is bumped
// while the write lock is held, so a reader that observes a mutated byName
// set and then reads Generation sees the bumped value.
func (c *catalog) Generation() uint64 { return c.generation.Load() }

// newCatalogEpoch mints a random instance identifier. crypto/rand failure is
// not recoverable for a process that must not reuse identifiers, so it panics
// at construction (the same posture as identity minting).
func newCatalogEpoch() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tools: catalog epoch: " + err.Error())
	}
	return hex.EncodeToString(b[:])
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
// the catalog wiring builder calls this once at
// boot AFTER every underlying tool registration has landed.
//
// Replace returns ErrToolNotFound (wrapped) when any `wrapped[i]`
// names a tool not currently in the catalog. In that case NO
// replacement happens — the failure is all-or-nothing.
func (c *catalog) Replace(wrapped []ToolDescriptor) error {
	if err := c.replaceLocked(wrapped); err != nil {
		return err
	}
	names := make([]string, 0, len(wrapped))
	for _, d := range wrapped {
		names = append(names, d.Tool.Name)
	}
	c.reconcileSearch("replace", names)
	return nil
}

func (c *catalog) replaceLocked(wrapped []ToolDescriptor) error {
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
		prior := c.byName[d.Tool.Name]
		c.byName[d.Tool.Name] = d
		c.sourceGenerations[prior.Tool.Source]++
		if d.Tool.Source != prior.Tool.Source {
			c.sourceGenerations[d.Tool.Source]++
		}
	}
	if len(wrapped) > 0 {
		c.generation.Add(1)
	}
	return nil
}

// DeregisterSource implements CatalogSourceDeregisterer. It removes every
// tool whose Source equals source from the catalog under the write lock and
// returns the number removed. Concurrent Resolve / List callers see either
// the full set or the pruned set, never a partial mix. A source with no
// registered tools removes nothing and returns 0 (idempotent — a second
// deregister of the same source is a no-op).
//
// This is the physical inverse of the boot-time per-source Register loop the
// MCP attach path runs: detach-on-reconcile deregisters a no-longer-declared
// MCP server's tools so the next run's projected catalog excludes them
// (mcp.connection.removed / rollback-past-add). The removed names are then
// pruned from the optional SearchCache, so a detached source leaves no index
// rows behind. Deregistration is rare (a control-plane removal), so a full
// scan under the write lock is cheaper than maintaining a source→names index
// on every Register.
func (c *catalog) DeregisterSource(source ToolSourceID) int {
	c.mu.Lock()
	var removed []string
	for name, d := range c.byName {
		if d.Tool.Source == source {
			delete(c.byName, name)
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		c.sourceGenerations[source]++
		c.generation.Add(1)
	}
	c.mu.Unlock()
	c.reconcileSearch("deregister_source", removed)
	return len(removed)
}

// StageSource implements ToolCatalog. Validation and collision checks happen
// before the catalog map is changed, and the complete source set is swapped
// under one write lock. Its receipt can restore the exact prior set while this
// publication remains current.
func (c *catalog) StageSource(source ToolSourceID, replacements []ToolDescriptor, replaceExisting bool) (CatalogSourceSwap, error) {
	if source == "" {
		return nil, wrap(ErrToolNotFound, "StageSource: source is empty")
	}
	staged := make([]ToolDescriptor, len(replacements))
	for i, d := range replacements {
		if d.Tool.Name == "" || d.Invoke == nil || d.Tool.Source != source {
			return nil, wrap(ErrToolNotFound, "StageSource: invalid descriptor %q for source %q", d.Tool.Name, source)
		}
		if err := validateExamples(d.Tool); err != nil {
			return nil, err
		}
		staged[i] = wrapDescriptorLifecycle(c.bus, d)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]struct{}, len(staged))
	if !replaceExisting {
		for _, prior := range c.byName {
			if prior.Tool.Source == source {
				return nil, wrap(ErrToolDuplicateName, "StageSource: source %q is already registered", source)
			}
		}
	}
	for _, d := range staged {
		if d.Tool.Name == "" || d.Invoke == nil || d.Tool.Source != source {
			return nil, wrap(ErrToolNotFound, "StageSource: invalid descriptor %q for source %q", d.Tool.Name, source)
		}
		if _, duplicate := seen[d.Tool.Name]; duplicate {
			return nil, wrap(ErrToolDuplicateName, "StageSource: duplicate replacement %q", d.Tool.Name)
		}
		seen[d.Tool.Name] = struct{}{}
		if prior, exists := c.byName[d.Tool.Name]; exists && prior.Tool.Source != source {
			return nil, wrap(ErrToolDuplicateName, "StageSource: tool %q belongs to source %q", d.Tool.Name, prior.Tool.Source)
		}
	}
	priorSet := make([]ToolDescriptor, 0)
	for name, prior := range c.byName {
		if prior.Tool.Source == source {
			priorSet = append(priorSet, prior)
			delete(c.byName, name)
		}
	}
	for _, d := range staged {
		c.byName[d.Tool.Name] = d
	}
	c.sourceGenerations[source]++
	c.generation.Add(1)
	return &catalogSourceSwap{catalog: c, source: source, prior: priorSet, replacements: staged, generation: c.sourceGenerations[source]}, nil
}

type catalogSourceSwap struct {
	mu           sync.Mutex
	catalog      *catalog
	source       ToolSourceID
	prior        []ToolDescriptor
	replacements []ToolDescriptor
	generation   uint64
	done         bool
}

func (s *catalogSourceSwap) Commit() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	names := s.touchedNames()
	s.mu.Unlock()
	// The prior set's names that the publication dropped are removed and the
	// replacements upserted — reconciled against the CURRENT catalog, so a
	// later publication of the same source is never overwritten.
	s.catalog.reconcileSearch("source_publication", names)
}

// touchedNames is every name the swap may have changed: the prior set plus
// the replacements. Caller holds s.mu.
func (s *catalogSourceSwap) touchedNames() []string {
	names := make([]string, 0, len(s.prior)+len(s.replacements))
	for _, d := range s.prior {
		names = append(names, d.Tool.Name)
	}
	for _, d := range s.replacements {
		names = append(names, d.Tool.Name)
	}
	return names
}

func (s *catalogSourceSwap) Rollback() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	if err := s.rollbackLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	names := s.touchedNames()
	s.mu.Unlock()
	s.catalog.reconcileSearch("source_rollback", names)
	return nil
}

func (s *catalogSourceSwap) rollbackLocked() error {
	s.catalog.mu.Lock()
	defer s.catalog.mu.Unlock()
	if s.catalog.sourceGenerations[s.source] != s.generation {
		return fmt.Errorf("tools: StageSource rollback refused for source %q: staged generation is no longer current", s.source)
	}
	for name, d := range s.catalog.byName {
		if d.Tool.Source == s.source {
			delete(s.catalog.byName, name)
		}
	}
	for _, d := range s.prior {
		s.catalog.byName[d.Tool.Name] = d
	}
	s.catalog.sourceGenerations[s.source]++
	s.catalog.generation.Add(1)
	s.done = true
	return nil
}

// Search implements ToolCatalog. Delegates to the attached SearchCache when
// present; returns an empty slice when no cache is configured (honest "index
// unavailable" — no panic) or when req.Visible is nil (fail closed: the index
// is process-wide and holds every principal's tools).
//
// A cache error logs at warn and returns an empty slice — the signature
// can't propagate. The query text is not logged (it is model-authored).
func (c *catalog) Search(ctx context.Context, req ToolSearchRequest) []Tool {
	sc := c.searchCache
	if sc == nil || req.Visible == nil {
		return nil
	}
	c.retrySearchDirty(ctx)
	results, err := sc.Search(ctx, req)
	if err != nil {
		slog.Warn("tools/catalog: search cache returned an error",
			slog.Int("limit", req.Limit),
			slog.String("err", err.Error()))
		return nil
	}
	return results
}

var _ CatalogGenerationReporter = (*catalog)(nil)
