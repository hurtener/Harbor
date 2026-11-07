package protocol

import (
	"container/list"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
)

// ConfigurationCatalogStamper is the optional CatalogViewResolver surface
// that names the input-addressed revision of a configuration inventory
// without building it. The production implementation is
// internal/runtime/agentcfg/projection.CatalogViewResolver.CatalogStamp,
// satisfied structurally so this package never imports the agent-config
// runtime.
//
// revision is a digest over every input the inventory reads (durable slot
// generations plus the process-local catalog and MCP registry generations,
// scoped to the principal). sessionScoped reports that a session overlay
// exists, so a cache entry must be keyed by session. ok=false with a nil
// error means the wired dependencies cannot report generations: the caller
// builds on every request and reports no revision.
type ConfigurationCatalogStamper interface {
	CatalogStamp(ctx context.Context, id identity.Identity, agentID, view string) (revision string, sessionScoped, ok bool, err error)
}

// RevisionedConfigurationProjector is the optional ConfigurationProjector
// surface behind `tools.list` revisions and conditional (`if_revision`)
// reads. ConfigurationRevision costs only the stamp reads; it never builds
// the inventory. An empty revision means "not revisioned on this runtime".
type RevisionedConfigurationProjector interface {
	ListConfigurationToolsWithRevision(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.Tool, string, error)
	ConfigurationRevision(ctx context.Context, id identity.Identity, agentID string) (string, error)
}

// configurationStampView is the view label folded into configuration
// stamps and cache keys.
const configurationStampView = string(prototypes.ToolCatalogViewConfiguration)

// StructuralCacheLimits bounds the configuration-inventory cache. Memory is
// a product constraint (runtimes commonly run in small containers), so every
// dimension has a small, safe default and none is unbounded.
type StructuralCacheLimits struct {
	// MaxBytes is the total estimated size of all cached inventories.
	MaxBytes int64
	// MaxEntryBytes is the largest single inventory that is cached; a larger
	// one is served uncached (each request rebuilds it).
	MaxEntryBytes int64
	// MaxEntries bounds the number of cached principals.
	MaxEntries int
	// IdleTTL evicts an entry not read for this long; a miss costs one build.
	IdleTTL time.Duration
}

// Default structural cache limits. Exported so the config layer documents and
// validates the same values.
const (
	DefaultStructuralCacheMaxBytes      int64 = 8 << 20
	DefaultStructuralCacheMaxEntryBytes int64 = 2 << 20
	DefaultStructuralCacheMaxEntries          = 64
	DefaultStructuralCacheIdleTTL             = 15 * time.Minute
)

// DefaultStructuralCacheLimits returns the safe defaults.
func DefaultStructuralCacheLimits() StructuralCacheLimits {
	return StructuralCacheLimits{
		MaxBytes:      DefaultStructuralCacheMaxBytes,
		MaxEntryBytes: DefaultStructuralCacheMaxEntryBytes,
		MaxEntries:    DefaultStructuralCacheMaxEntries,
		IdleTTL:       DefaultStructuralCacheIdleTTL,
	}
}

// WithStructuralCacheLimits sets the configuration-inventory cache bounds.
// Non-positive fields take their defaults; MaxEntryBytes is clamped to
// MaxBytes.
func WithStructuralCacheLimits(limits StructuralCacheLimits) CatalogProjectorOption {
	return func(p *CatalogProjector) {
		p.structural = newStructuralCache(limits, time.Now)
	}
}

// WithoutStructuralCache disables the configuration-inventory cache: every
// configuration read builds its inventory and still reports its revision.
func WithoutStructuralCache() CatalogProjectorOption {
	return func(p *CatalogProjector) { p.structural = nil }
}

// structuralBuildTimeout bounds one shared structural build. The build runs
// on a context detached from every caller's cancellation (so one caller's
// disconnect cannot fail the others) but never longer than this.
const structuralBuildTimeout = 30 * time.Second

// structuralKey addresses one principal's cached configuration inventory.
// Tenant, user and agent are always present, so a row-visibility rule that
// depends on the principal (user-owned MCP sources) can never be served
// across principals even if two stamps collided. session is set only when a
// session overlay exists. The revision lives in the entry, not the key: a
// principal has at most one entry, replaced by each newer stable build, so
// superseded inventories never pin memory.
type structuralKey struct {
	tenant, user, agent, view, session string
}

func (k structuralKey) flight(revision string) string {
	return strings.Join([]string{
		strconv.Quote(k.tenant), strconv.Quote(k.user), strconv.Quote(k.agent),
		strconv.Quote(k.view), strconv.Quote(k.session), revision,
	}, "\x00")
}

// structuralCache is a bounded LRU of configuration inventories plus a
// per-(key, revision) single-flight. It stores STRUCTURE only — the admitted
// descriptor views a CatalogViewResolver produced. It never stores
// annotations (OAuth, approval, last-used, display modes), which are applied
// per request, and it never stores an authorization decision: admission runs
// on every request before any stamp read. Cached slices are shared read-only.
//
// It is bounded by entry count, by total estimated bytes, by a per-entry byte
// cap, and by idle time. Sizes are conservative estimates of the descriptor
// fields (schemas dominate), not exact heap accounting.
type structuralCache struct {
	mu     sync.Mutex
	limits StructuralCacheLimits
	now    func() time.Time
	bytes  int64
	order  *list.List
	items  map[structuralKey]*list.Element
	group  singleflight.Group
}

type structuralEntry struct {
	key      structuralKey
	revision string
	tools    []tools.Tool
	bytes    int64
	lastUsed time.Time
}

func newStructuralCache(limits StructuralCacheLimits, now func() time.Time) *structuralCache {
	def := DefaultStructuralCacheLimits()
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = def.MaxBytes
	}
	if limits.MaxEntryBytes <= 0 {
		limits.MaxEntryBytes = def.MaxEntryBytes
	}
	if limits.MaxEntryBytes > limits.MaxBytes {
		limits.MaxEntryBytes = limits.MaxBytes
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = def.MaxEntries
	}
	if limits.IdleTTL <= 0 {
		limits.IdleTTL = def.IdleTTL
	}
	if now == nil {
		now = time.Now
	}
	return &structuralCache{limits: limits, now: now, order: list.New(), items: make(map[structuralKey]*list.Element)}
}

// estimateInventoryBytes conservatively estimates the memory an inventory
// pins: every variable-length descriptor field plus a fixed per-tool and
// per-example overhead for struct headers, maps and small fields.
func estimateInventoryBytes(inventory []tools.Tool) int64 {
	const perTool, perExample, perArg = 512, 128, 64
	total := int64(64)
	for i := range inventory {
		t := &inventory[i]
		n := perTool + len(t.Name) + len(t.Description) + len(t.ArgsSchema) + len(t.OutSchema) +
			len(t.CostHint) + len(t.SafetyNotes) + len(t.Source)
		for _, v := range t.Tags {
			n += len(v) + 16
		}
		for _, v := range t.AuthScopes {
			n += len(v) + 16
		}
		for _, v := range t.HandlesMIME {
			n += len(v) + 16
		}
		for _, ex := range t.Examples {
			n += perExample + len(ex.Description) + perArg*len(ex.Args)
			for _, v := range ex.Tags {
				n += len(v) + 16
			}
		}
		total += int64(n)
	}
	return total
}

// get returns the principal's inventory only when it was built under exactly
// revision and has not been idle past IdleTTL.
func (c *structuralCache) get(key structuralKey, revision string) ([]tools.Tool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := entryOf(el)
	if entry.revision != revision {
		return nil, false
	}
	entry.lastUsed = now
	c.order.MoveToFront(el)
	return entry.tools, true
}

// put stores (or replaces) the principal's inventory for revision. An
// inventory larger than MaxEntryBytes is not cached (and evicts any older
// entry for the principal, which it supersedes).
func (c *structuralCache) put(key structuralKey, revision string, inventory []tools.Tool) {
	size := estimateInventoryBytes(inventory)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	if size > c.limits.MaxEntryBytes {
		return
	}
	c.items[key] = c.order.PushFront(&structuralEntry{key: key, revision: revision, tools: inventory, bytes: size, lastUsed: now})
	c.bytes += size
	c.expireLocked(now)
	for c.order.Len() > c.limits.MaxEntries || c.bytes > c.limits.MaxBytes {
		c.removeLocked(c.order.Back())
	}
}

// expireLocked drops entries idle past IdleTTL. The LRU back is the least
// recently used, so the scan stops at the first fresh entry.
func (c *structuralCache) expireLocked(now time.Time) {
	for el := c.order.Back(); el != nil; el = c.order.Back() {
		if now.Sub(entryOf(el).lastUsed) < c.limits.IdleTTL {
			return
		}
		c.removeLocked(el)
	}
}

func (c *structuralCache) removeLocked(el *list.Element) {
	entry := entryOf(el)
	c.order.Remove(el)
	delete(c.items, entry.key)
	c.bytes -= entry.bytes
}

// entryOf returns the cache entry an LRU element holds. Only put inserts
// elements, always with a *structuralEntry.
func entryOf(el *list.Element) *structuralEntry {
	entry, ok := el.Value.(*structuralEntry)
	if !ok {
		panic("tools/protocol: structural cache element is not a *structuralEntry")
	}
	return entry
}

func (c *structuralCache) usedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *structuralCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// configurationInventory returns the admitted configuration descriptors for
// (id, agentID) and the revision they were built under ("" when the runtime
// cannot stamp). Callers have already admitted the request; the stamp itself
// applies the session erasure and agent lifecycle refusals, so a refused
// session never reaches the cache.
//
// On a miss it stamps, builds, then stamps again and inserts only when the
// two stamps agree (the ABA guard): a mutation that lands between the first
// stamp and the build's reads would otherwise be cached under a revision that
// does not describe it. Deletions of stamped slots are preceded by a durable
// deletion-generation write, so "absent → present → absent" cycles also move
// the stamp. The pre-build stamp is the one reported, so a concurrent mutation
// can only make the client's next conditional read miss.
//
// Concurrent misses for the same principal and revision share one build. The
// build runs detached from every caller's cancellation, bounded by
// structuralBuildTimeout; each caller waits only as long as its own context
// allows.
func (p *CatalogProjector) configurationInventory(ctx context.Context, id identity.Identity, agentID string) ([]tools.Tool, string, error) {
	resolver, ok := p.view.(ConfigurationCatalogViewResolver)
	if !ok {
		return nil, "", ErrAdminUnsupported
	}
	build := func(ctx context.Context) ([]tools.Tool, error) {
		view, err := resolver.ConfigurationCatalogView(ctx, id, agentID)
		if err != nil {
			return nil, err
		}
		return view.List(), nil
	}
	stamper, ok := p.view.(ConfigurationCatalogStamper)
	if !ok {
		descriptors, err := build(ctx)
		return descriptors, "", err
	}
	revision, sessionScoped, stampable, err := stamper.CatalogStamp(ctx, id, agentID, configurationStampView)
	if err != nil {
		return nil, "", err
	}
	if !stampable {
		descriptors, err := build(ctx)
		return descriptors, "", err
	}
	if p.structural == nil {
		// Cache disabled: still report the pre-build revision so
		// conditional reads keep working; nothing is stored.
		descriptors, err := build(ctx)
		if err != nil {
			return nil, "", err
		}
		return descriptors, revision, nil
	}
	key := structuralKey{tenant: id.TenantID, user: id.UserID, agent: agentID, view: configurationStampView}
	if sessionScoped {
		key.session = id.SessionID
	}
	if descriptors, hit := p.structural.get(key, revision); hit {
		return descriptors, revision, nil
	}
	led := false
	results := p.structural.group.DoChan(key.flight(revision), func() (value any, err error) {
		led = true
		// A panicking build must not crash the process: singleflight would
		// re-panic it on a fresh goroutine. Recover it into an error.
		defer func() {
			if recovered := recover(); recovered != nil {
				value, err = nil, fmt.Errorf("%w: structural build panicked: %v", ErrMisconfigured, recovered)
			}
		}()
		if descriptors, hit := p.structural.get(key, revision); hit {
			return descriptors, nil
		}
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), structuralBuildTimeout)
		defer cancel()
		descriptors, err := build(bctx)
		if err != nil {
			return nil, err
		}
		if p.afterStructuralBuild != nil {
			p.afterStructuralBuild()
		}
		after, _, stillStampable, err := stamper.CatalogStamp(bctx, id, agentID, configurationStampView)
		if err == nil && stillStampable && after == revision {
			p.structural.put(key, revision, descriptors)
		}
		return descriptors, nil
	})
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case result := <-results:
		if result.Err != nil {
			if !led {
				// The shared build ran for another caller's identity (its
				// session, its context values). Its failure is not this
				// caller's outcome: build once with this caller's own
				// context and identity, uncached.
				return p.uncachedInventory(ctx, build, revision)
			}
			return nil, "", result.Err
		}
		descriptors, ok := result.Val.([]tools.Tool)
		if !ok {
			return nil, "", fmt.Errorf("%w: structural build returned %T", ErrMisconfigured, result.Val)
		}
		return descriptors, revision, nil
	}
}

// uncachedInventory builds with the caller's own context and reports the
// caller's pre-build revision; nothing is stored. A panic is recovered into an
// error so a misbehaving resolver cannot crash the process.
func (p *CatalogProjector) uncachedInventory(ctx context.Context, build func(context.Context) ([]tools.Tool, error), revision string) (descriptors []tools.Tool, rev string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			descriptors, rev, err = nil, "", fmt.Errorf("%w: structural build panicked: %v", ErrMisconfigured, recovered)
		}
	}()
	descriptors, err = build(ctx)
	if err != nil {
		return nil, "", err
	}
	return descriptors, revision, nil
}

// ListConfigurationToolsWithRevision implements RevisionedConfigurationProjector.
func (p *CatalogProjector) ListConfigurationToolsWithRevision(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.Tool, string, error) {
	descriptors, revision, err := p.configurationInventory(ctx, id, agentID)
	if err != nil {
		return nil, "", err
	}
	// Annotations are applied per request over the cached structure through
	// one request-local batch; they are never cached.
	rows, err := p.projectRows(ctx, id, descriptors)
	if err != nil {
		return nil, "", err
	}
	return rows, revision, nil
}

// ConfigurationRevision implements RevisionedConfigurationProjector. It
// performs only the stamp reads.
func (p *CatalogProjector) ConfigurationRevision(ctx context.Context, id identity.Identity, agentID string) (string, error) {
	if _, ok := p.view.(ConfigurationCatalogViewResolver); !ok {
		return "", ErrAdminUnsupported
	}
	stamper, ok := p.view.(ConfigurationCatalogStamper)
	if !ok {
		return "", nil
	}
	revision, _, stampable, err := stamper.CatalogStamp(ctx, id, agentID, configurationStampView)
	if err != nil {
		return "", err
	}
	if !stampable {
		return "", nil
	}
	return revision, nil
}

var _ RevisionedConfigurationProjector = (*CatalogProjector)(nil)
