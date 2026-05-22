package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// Phase 73k (D-119) — the MCP-Connections-page read API.
//
// Phase 28 ships one `Provider` per MCP server attachment. Phase 73k
// adds the process-local `Registry` that holds the configured providers
// by name and exposes the projection-only read surface the Console MCP
// Connections page consumes — server list, per-server detail, advertised
// resources / prompts, refresh-discovery, transport probe, and health.
//
// # Projection-only — no MCP-SDK leakage
//
// Every type the Registry returns (ServerView, ResourceView,
// PromptView, DiscoveryResult, ProbeResult, HealthSnapshot,
// BindingView) is a flat projection. The MCP SDK's own types
// (`mcpsdk.Tool`, `mcpsdk.Resource`, ...) never cross the package
// boundary — the Protocol surface translates these projection types
// onto the wire shapes; the Console never sees an SDK type.
//
// # Concurrent reuse (D-025)
//
// Registry is a compiled artifact: the provider set is built once at
// construction and never mutated after Register/Close. Per-server
// mutable stats (state, latency, discovery counts, reconnect history)
// live on `serverStats` guarded by a `sync.RWMutex` with documented
// invariants — no per-call state lives on the Registry struct itself.
// One Registry is safe to share across N concurrent read goroutines;
// concurrent_test.go pins N≥128 under -race.

// Provider-discovery interface — the narrow contract the Registry's read
// API needs from each held provider. The Phase 28 *Provider satisfies it
// structurally; tests inject a deterministic stub.
type serverProvider interface {
	SourceID() tools.ToolSourceID
	Discover(ctx context.Context) ([]tools.ToolDescriptor, error)
}

// compile-time assertion: the Phase 28 *Provider satisfies serverProvider.
var _ serverProvider = (*Provider)(nil)

// ServerState mirrors the canonical state chip the Console renders. The
// V1 set is closed.
type ServerState string

// The canonical MCP server states.
const (
	// ServerStateOnline — transport connected, last discovery / probe
	// succeeded.
	ServerStateOnline ServerState = "online"
	// ServerStateReconnecting — transport dropped, re-establishing.
	ServerStateReconnecting ServerState = "reconnecting"
	// ServerStateOffline — transport down (never connected / closed).
	ServerStateOffline ServerState = "offline"
	// ServerStateAuthPending — server needs an incomplete OAuth binding.
	ServerStateAuthPending ServerState = "auth_pending"
	// ServerStateError — last discovery / probe failed.
	ServerStateError ServerState = "error"
)

// ServerView is the per-server projection the Registry returns. It is a
// flat shape — no MCP-SDK type crosses the package boundary.
type ServerView struct {
	// Name is the unique server / source id.
	Name string
	// Transport is the wire transport string.
	Transport string
	// URLOrCommand is the transport-prefixed endpoint or argv command.
	URLOrCommand string
	// State is the canonical state chip.
	State ServerState
	// LastDiscoveryAt is the last successful discovery instant (zero
	// when discovery has never run).
	LastDiscoveryAt time.Time
	// ToolCount / ResourceCount / PromptCount are the advertised counts.
	ToolCount     int
	ResourceCount int
	PromptCount   int
	// RecentLatencyMs is the most recent observed handshake / probe
	// latency.
	RecentLatencyMs int64
	// ErrorRatePerMin is the transport-error rate over the window.
	ErrorRatePerMin float64
	// OAuthBindingCount is the number of OAuth bindings configured.
	OAuthBindingCount int
	// RawHTMLTrusted reports the per-server raw-HTML trust flag.
	RawHTMLTrusted bool
	// DisplayModes lists the advertised MCP-Apps DisplayMode values.
	DisplayModes []string
	// ContentShapes lists the canonical content shapes the server's
	// tools return.
	ContentShapes []string
	// Policy is the read-only ToolPolicy projection.
	Policy tools.ToolPolicy
}

// ResourceView is one advertised resource.
type ResourceView struct {
	URI       string
	MimeType  string
	SizeBytes int64
	Name      string
	Title     string
}

// PromptView is one advertised prompt.
type PromptView struct {
	Name        string
	Description string
	Arguments   []PromptArgView
}

// PromptArgView is one declared prompt argument.
type PromptArgView struct {
	Name        string
	Description string
	Required    bool
}

// DiscoveryResult is the outcome of a RefreshDiscovery call.
type DiscoveryResult struct {
	DiscoveryID   string
	ToolCount     int
	ResourceCount int
	PromptCount   int
}

// ProbeResult is the outcome of a Probe call.
type ProbeResult struct {
	OK        bool
	LatencyMs int64
	Error     string
}

// HealthBucket is one handshake-latency sparkline bucket.
type HealthBucket struct {
	StartMs   int64
	LatencyMs int64
}

// ReconnectEntry is one reconnect-history entry.
type ReconnectEntry struct {
	OccurredAt time.Time
	Reason     string
}

// HealthSnapshot is the Health read result.
type HealthSnapshot struct {
	HandshakeLatencyBuckets []HealthBucket
	ReconnectHistory        []ReconnectEntry
	TransportErrorRate      float64
}

// ListFilter is the filter shape ListServers applies.
type ListFilter struct {
	// State filters to servers in any of the given states. Empty = all.
	State []ServerState
	// Transport filters to servers on any of the given transports.
	Transport []string
	// HasOAuth, when set, filters on OAuth-binding presence.
	HasOAuth *bool
	// HasRecentError, when set, filters on recent-error presence.
	HasRecentError *bool
	// NamePrefix filters to servers whose name has the prefix.
	NamePrefix string
	// PageToken is the opaque cursor from a prior page.
	PageToken string
	// PageSize is the requested max row count (clamped by the Registry).
	PageSize int
}

// Cursor is the opaque pagination cursor a paged read returns.
type Cursor struct {
	// NextPageToken is the cursor for the next page, or empty when the
	// page is the last.
	NextPageToken string
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrServerNotFound — the named server is not registered.
	ErrServerNotFound = fmt.Errorf("mcp: server not found")
	// ErrRegistryIdentityMissing — the read ctx had no identity triple.
	// Identity is mandatory (AGENTS.md §6 rule 9); the read fails closed.
	ErrRegistryIdentityMissing = fmt.Errorf("mcp: identity missing from ctx")
)

// maxListPageSize / defaultListPageSize bound the ListServers page.
const (
	maxListPageSize     = 200
	defaultListPageSize = 50
)

// serverEntry is the Registry's per-server record — the provider plus
// its operator-supplied static config plus mutable runtime stats.
type serverEntry struct {
	provider     serverProvider
	transport    string
	urlOrCommand string
	policy       tools.ToolPolicy
	displayModes []string
	contentShape []string

	// stats is the mutable per-server runtime state. Guarded by the
	// Registry's mu (RWMutex). Documented invariants: every field is
	// written only while mu is held for writing, read only while mu is
	// held for reading.
	stats serverStats
}

// serverStats is the mutable runtime state for one server. Guarded by
// Registry.mu.
type serverStats struct {
	state             ServerState
	lastDiscoveryAt   time.Time
	toolCount         int
	resourceCount     int
	promptCount       int
	recentLatencyMs   int64
	errorRatePerMin   float64
	oauthBindingCount int
	rawHTMLTrusted    bool
	latencyBuckets    []HealthBucket
	reconnects        []ReconnectEntry
	discoveryCounter  int
}

// Registry is the process-local MCP-server read API. It is a compiled
// artifact (D-025) — built once at construction; the provider set is
// write-once after Register; per-server stats are guarded by mu.
type Registry struct {
	mu      sync.RWMutex
	servers map[string]*serverEntry
	clock   func() time.Time
}

// RegistryOption configures a Registry at construction.
type RegistryOption func(*Registry)

// WithRegistryClock overrides the Registry's wall clock — tests inject a
// deterministic clock so latency / timestamps are stable.
func WithRegistryClock(now func() time.Time) RegistryOption {
	return func(r *Registry) {
		if now != nil {
			r.clock = now
		}
	}
}

// NewRegistry builds an empty Registry. Servers are added via Register.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		servers: map[string]*serverEntry{},
		clock:   time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ServerRegistration is the operator-supplied static descriptor for one
// MCP server attachment the Registry tracks.
type ServerRegistration struct {
	// Provider is the live MCP provider. Required.
	Provider serverProvider
	// Transport is the wire transport string ("stdio" / "http+sse" /
	// "streamable-http" / "websocket"). Required.
	Transport string
	// URLOrCommand is the transport-prefixed endpoint or argv command.
	URLOrCommand string
	// Policy is the server's ToolPolicy. Zero-valued → DefaultPolicy.
	Policy tools.ToolPolicy
	// DisplayModes lists the advertised MCP-Apps DisplayMode values.
	DisplayModes []string
	// ContentShapes lists the canonical content shapes the tools return.
	ContentShapes []string
	// OAuthBindingCount is the configured OAuth binding count.
	OAuthBindingCount int
	// InitialState is the server's starting state. Zero-valued →
	// ServerStateOffline.
	InitialState ServerState
}

// Register adds a server to the Registry. Re-registering the same name
// replaces the prior entry (the dev hot-reload path re-registers).
func (r *Registry) Register(reg ServerRegistration) error {
	if reg.Provider == nil {
		return fmt.Errorf("mcp: Register requires a non-nil Provider")
	}
	name := string(reg.Provider.SourceID())
	if name == "" {
		return fmt.Errorf("mcp: Register requires a non-empty provider source id")
	}
	if reg.Transport == "" {
		return fmt.Errorf("mcp: Register requires a non-empty Transport")
	}
	policy := reg.Policy
	if isZeroPolicy(policy) {
		policy = tools.DefaultPolicy()
	}
	st := reg.InitialState
	if st == "" {
		st = ServerStateOffline
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers[name] = &serverEntry{
		provider:     reg.Provider,
		transport:    reg.Transport,
		urlOrCommand: reg.URLOrCommand,
		policy:       policy,
		displayModes: append([]string(nil), reg.DisplayModes...),
		contentShape: append([]string(nil), reg.ContentShapes...),
		stats: serverStats{
			state:             st,
			oauthBindingCount: reg.OAuthBindingCount,
		},
	}
	return nil
}

// requireIdentity fails closed when ctx carries no identity triple.
// Identity is mandatory on every read path (AGENTS.md §6 rule 9).
func requireIdentity(ctx context.Context) error {
	id, ok := identity.From(ctx)
	if !ok {
		return ErrRegistryIdentityMissing
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return ErrRegistryIdentityMissing
	}
	return nil
}

// viewLocked builds a ServerView snapshot from an entry. Caller MUST
// hold r.mu (read or write).
func (e *serverEntry) viewLocked() ServerView {
	return ServerView{
		Name:              string(e.provider.SourceID()),
		Transport:         e.transport,
		URLOrCommand:      e.urlOrCommand,
		State:             e.stats.state,
		LastDiscoveryAt:   e.stats.lastDiscoveryAt,
		ToolCount:         e.stats.toolCount,
		ResourceCount:     e.stats.resourceCount,
		PromptCount:       e.stats.promptCount,
		RecentLatencyMs:   e.stats.recentLatencyMs,
		ErrorRatePerMin:   e.stats.errorRatePerMin,
		OAuthBindingCount: e.stats.oauthBindingCount,
		RawHTMLTrusted:    e.stats.rawHTMLTrusted,
		DisplayModes:      append([]string(nil), e.displayModes...),
		ContentShapes:     append([]string(nil), e.contentShape...),
		Policy:            e.policy,
	}
}

// ListServers returns the filtered, paginated server list. The view
// shapes are projection-only; no per-call state lives on the Registry
// (D-025). Identity is mandatory.
func (r *Registry) ListServers(ctx context.Context, f ListFilter) ([]ServerView, *Cursor, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("mcp: ListServers cancelled: %w", err)
	}

	r.mu.RLock()
	all := make([]ServerView, 0, len(r.servers))
	for _, e := range r.servers {
		all = append(all, e.viewLocked())
	}
	r.mu.RUnlock()

	// Deterministic order — sort by name so the cursor is stable.
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	filtered := all[:0:0]
	for _, v := range all {
		if !matchesFilter(v, f) {
			continue
		}
		filtered = append(filtered, v)
	}

	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}
	if pageSize > maxListPageSize {
		pageSize = maxListPageSize
	}

	start := 0
	if f.PageToken != "" {
		// The cursor is the last name on the prior page; resume past it.
		for i, v := range filtered {
			if v.Name > f.PageToken {
				start = i
				break
			}
			start = i + 1
		}
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	page := append([]ServerView(nil), filtered[start:end]...)
	cur := &Cursor{}
	if end < len(filtered) {
		cur.NextPageToken = filtered[end-1].Name
	}
	return page, cur, nil
}

// matchesFilter reports whether a server view passes the list filter.
func matchesFilter(v ServerView, f ListFilter) bool {
	if len(f.State) > 0 {
		found := false
		for _, s := range f.State {
			if v.State == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Transport) > 0 {
		found := false
		for _, t := range f.Transport {
			if v.Transport == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.HasOAuth != nil {
		if *f.HasOAuth != (v.OAuthBindingCount > 0) {
			return false
		}
	}
	if f.HasRecentError != nil {
		hasErr := v.State == ServerStateError || v.ErrorRatePerMin > 0
		if *f.HasRecentError != hasErr {
			return false
		}
	}
	if f.NamePrefix != "" && !strings.HasPrefix(v.Name, f.NamePrefix) {
		return false
	}
	return true
}

// entry returns the named server entry, or ErrServerNotFound. Caller
// must NOT hold r.mu.
func (r *Registry) entry(name string) (*serverEntry, error) {
	r.mu.RLock()
	e, ok := r.servers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	return e, nil
}

// GetServer returns the per-server detail view. Identity is mandatory.
func (r *Registry) GetServer(ctx context.Context, name string) (*ServerView, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	v := e.viewLocked()
	r.mu.RUnlock()
	return &v, nil
}

// ListResources returns the advertised resources for a server. It runs a
// Discover and projects the synthetic resource descriptors. Identity is
// mandatory.
func (r *Registry) ListResources(ctx context.Context, name string) ([]ResourceView, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	descs, derr := e.provider.Discover(ctx)
	if derr != nil {
		r.recordError(name)
		return nil, fmt.Errorf("mcp: ListResources discover %q: %w", name, derr)
	}
	out := []ResourceView{}
	for _, d := range descs {
		uri, ok := resourceURIFromToolName(d.Tool.Name, name)
		if !ok {
			continue
		}
		out = append(out, ResourceView{
			URI:   uri,
			Name:  uri,
			Title: d.Tool.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

// ListPrompts returns the advertised prompts for a server. Identity is
// mandatory.
func (r *Registry) ListPrompts(ctx context.Context, name string) ([]PromptView, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	descs, derr := e.provider.Discover(ctx)
	if derr != nil {
		r.recordError(name)
		return nil, fmt.Errorf("mcp: ListPrompts discover %q: %w", name, derr)
	}
	out := []PromptView{}
	for _, d := range descs {
		pname, ok := promptNameFromToolName(d.Tool.Name, name)
		if !ok {
			continue
		}
		out = append(out, PromptView{
			Name:        pname,
			Description: d.Tool.Description,
			Arguments:   []PromptArgView{},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RefreshDiscovery re-runs the named server's discovery and updates the
// per-server counts + state. Identity is mandatory.
func (r *Registry) RefreshDiscovery(ctx context.Context, name string) (*DiscoveryResult, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	start := r.clock()
	descs, derr := e.provider.Discover(ctx)
	latency := r.clock().Sub(start).Milliseconds()
	if derr != nil {
		r.recordError(name)
		return nil, fmt.Errorf("mcp: RefreshDiscovery %q: %w", name, derr)
	}
	tc, rc, pc := classifyDescriptors(descs, name)

	r.mu.Lock()
	e.stats.discoveryCounter++
	discoveryID := fmt.Sprintf("%s-disc-%d", name, e.stats.discoveryCounter)
	e.stats.toolCount = tc
	e.stats.resourceCount = rc
	e.stats.promptCount = pc
	e.stats.lastDiscoveryAt = r.clock()
	e.stats.recentLatencyMs = latency
	e.stats.state = ServerStateOnline
	e.stats.latencyBuckets = appendBucket(e.stats.latencyBuckets, HealthBucket{
		StartMs:   start.UnixMilli(),
		LatencyMs: latency,
	})
	r.mu.Unlock()

	return &DiscoveryResult{
		DiscoveryID:   discoveryID,
		ToolCount:     tc,
		ResourceCount: rc,
		PromptCount:   pc,
	}, nil
}

// Probe runs a transport round-trip (a Discover acting as a tools/list
// ping) and returns the latency. Identity is mandatory.
func (r *Registry) Probe(ctx context.Context, name string) (*ProbeResult, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	start := r.clock()
	_, derr := e.provider.Discover(ctx)
	latency := r.clock().Sub(start).Milliseconds()
	if derr != nil {
		r.recordError(name)
		// A probe failure is a successful probe with a failed result:
		// derr is surfaced inside ProbeResult.Error, not as a top-level
		// error, so callers always get a populated ProbeResult.
		return &ProbeResult{OK: false, LatencyMs: latency, Error: derr.Error()}, nil //nolint:nilerr // probe failure is reported in ProbeResult.Error, not as a return error
	}
	r.mu.Lock()
	e.stats.recentLatencyMs = latency
	if e.stats.state == ServerStateOffline || e.stats.state == ServerStateError {
		e.stats.state = ServerStateOnline
	}
	r.mu.Unlock()
	return &ProbeResult{OK: true, LatencyMs: latency}, nil
}

// Health returns the per-server handshake-latency sparkline + reconnect
// history + transport-error rate. The window argument bounds the
// reconnect-history slice. Identity is mandatory.
func (r *Registry) Health(ctx context.Context, name string, window time.Duration) (*HealthSnapshot, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.entry(name)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	buckets := append([]HealthBucket(nil), e.stats.latencyBuckets...)
	errRate := e.stats.errorRatePerMin
	var reconnects []ReconnectEntry
	cutoff := r.clock().Add(-window)
	for _, rc := range e.stats.reconnects {
		if window <= 0 || rc.OccurredAt.After(cutoff) {
			reconnects = append(reconnects, rc)
		}
	}
	r.mu.RUnlock()
	if buckets == nil {
		buckets = []HealthBucket{}
	}
	if reconnects == nil {
		reconnects = []ReconnectEntry{}
	}
	return &HealthSnapshot{
		HandshakeLatencyBuckets: buckets,
		ReconnectHistory:        reconnects,
		TransportErrorRate:      errRate,
	}, nil
}

// SetRawHTMLTrust persists the per-server raw-HTML trust flag in the
// runtime-side mirror (the legitimate D-061 carve-out for a preference
// with audit consequences). It returns the prior value so a caller can
// detect a no-op toggle. Identity is mandatory.
func (r *Registry) SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (prev bool, err error) {
	if err := requireIdentity(ctx); err != nil {
		return false, err
	}
	e, eerr := r.entry(name)
	if eerr != nil {
		return false, eerr
	}
	r.mu.Lock()
	prev = e.stats.rawHTMLTrusted
	e.stats.rawHTMLTrusted = trusted
	r.mu.Unlock()
	return prev, nil
}

// recordError bumps a server's error rate and flips it to the error
// state. Used by the read paths when a Discover fails.
func (r *Registry) recordError(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return
	}
	e.stats.errorRatePerMin++
	e.stats.state = ServerStateError
}

// RecordReconnect appends a reconnect-history entry. The runtime wires
// this to the transport-reconnect path; tests call it directly.
func (r *Registry) RecordReconnect(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return
	}
	e.stats.reconnects = append(e.stats.reconnects, ReconnectEntry{
		OccurredAt: r.clock(),
		Reason:     reason,
	})
	e.stats.state = ServerStateReconnecting
}

// isZeroPolicy reports whether a ToolPolicy is the zero value across the
// fields the Registry projection cares about (TimeoutMS / MaxRetries /
// backoff / Validate). A zero policy resolves to DefaultPolicy.
func isZeroPolicy(p tools.ToolPolicy) bool {
	return p.TimeoutMS == 0 &&
		p.MaxRetries == 0 &&
		p.BackoffBase == 0 &&
		p.BackoffMult == 0 &&
		p.BackoffMax == 0 &&
		len(p.RetryOn) == 0 &&
		p.Validate == tools.ValidateNone
}

// appendBucket appends a latency bucket, keeping at most 60 entries.
func appendBucket(buckets []HealthBucket, b HealthBucket) []HealthBucket {
	buckets = append(buckets, b)
	const maxBuckets = 60
	if len(buckets) > maxBuckets {
		buckets = buckets[len(buckets)-maxBuckets:]
	}
	return buckets
}

// classifyDescriptors counts tools / resources / prompts in a descriptor
// slice, using the Phase 28 synthetic-name markers.
func classifyDescriptors(descs []tools.ToolDescriptor, server string) (toolCount, resourceCount, promptCount int) {
	for _, d := range descs {
		name := d.Tool.Name
		if _, ok := resourceURIFromToolName(name, server); ok {
			resourceCount++
			continue
		}
		if _, ok := promptNameFromToolName(name, server); ok {
			promptCount++
			continue
		}
		toolCount++
	}
	return toolCount, resourceCount, promptCount
}

// resourceURIFromToolName extracts the resource URI from a Phase 28
// synthetic resource tool name (`<server>__resource.<uri>`).
func resourceURIFromToolName(toolName, server string) (string, bool) {
	prefix := server + resourceTypeSeparator + resourceNamePrefix
	if !strings.HasPrefix(toolName, prefix) {
		return "", false
	}
	return strings.TrimPrefix(toolName, prefix), true
}

// promptNameFromToolName extracts the prompt name from a Phase 28
// synthetic prompt tool name (`<server>__prompt.<name>`).
func promptNameFromToolName(toolName, server string) (string, bool) {
	prefix := server + resourceTypeSeparator + promptNamePrefix
	if !strings.HasPrefix(toolName, prefix) {
		return "", false
	}
	return strings.TrimPrefix(toolName, prefix), true
}
