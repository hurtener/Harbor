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
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// the MCP-Connections-page read API.
//
// Harbor ships one `Provider` per MCP server attachment. Harbor
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
// # Concurrent reuse
//
// Registry is a compiled artifact: the provider set is built once at
// construction and never mutated after Register/Close. Per-server
// mutable stats (state, latency, discovery counts, reconnect history)
// live on `serverStats` guarded by a `sync.RWMutex` with documented
// invariants — no per-call state lives on the Registry struct itself.
// One Registry is safe to share across N concurrent read goroutines;
// concurrent_test.go pins N≥128 under -race.

// Provider-discovery interface — the narrow contract the Registry's read
// API needs from each held provider. The MCP *Provider satisfies it
// structurally; tests inject a deterministic stub.
type serverProvider interface {
	SourceID() tools.ToolSourceID
	Discover(ctx context.Context) ([]tools.ToolDescriptor, error)
	// DisplayModes returns the MCP Apps display modes the connected
	// server advertises via its `io.modelcontextprotocol/ui`
	// capability. An empty result means the server advertises no UI
	// capability — never a fabricated default.
	DisplayModes() []string
	// ReadResource fetches a single resource's content under the
	// request identity triple. Powers `mcp.servers.read_resource` — the
	// MCP Apps `ui://` UI-document fetch.
	ReadResource(ctx context.Context, uri string) (content []byte, mimeType string, err error)
	// Close shuts the provider's transport down gracefully and idempotently.
	// Deregister calls it so a detach-on-reconcile teardown drains the
	// subprocess / connection at the next-turn projection boundary.
	Close(ctx context.Context) error
}

// compile-time assertion: the MCP *Provider satisfies serverProvider.
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
	// OAuthRequirement is the OAuth requirement advertised by the server and
	// discovered on demand — the verbatim RFC 9728 → RFC 8414 chain
	// plus provenance. Nil when no discovery has run. Populated only on the
	// DETAIL read (GetServer); the list projection leaves it nil so the hot
	// list row stays compact (§4.3-recorded — the requirement rides get/probe,
	// not list). It is inert, server-supplied, UNVERIFIED data.
	OAuthRequirement *auth.OAuthRequirement
	// LastScopeShortfall is the most recent downstream insufficient-scope
	// step-up (a `403` + `WWW-Authenticate` marking
	// `error="insufficient_scope"`) observed on this connection. Nil when
	// none seen. Populated only on the DETAIL read (GetServer), mirroring how
	// OAuthRequirement rides get — the list row stays compact. Inert,
	// server-supplied data; the operator acts on it, the runtime never does.
	LastScopeShortfall *ScopeShortfall
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
	// oauthAllowedOrigins is the explicit per-connection cross-origin
	// allowance list for OAuth-requirement discovery fetches. Set at Register
	// and re-written live by SetOAuthDiscoveryOrigins — internally
	// synchronised (written only while r.mu is held for writing, read only
	// while held for reading), so a live allowance write is race-free against
	// a concurrent OAuthDiscoveryTarget read.
	oauthAllowedOrigins []string
	// owner is the (tenant, agent) reconcile-view tag for a runtime-added
	// server. A zero owner marks a boot-declared server, which the
	// owner-scoped reconcile view never enumerates. Set at Register;
	// read-only thereafter. It is a reconcile-view filter, NOT an isolation
	// key — resolution and dispatch stay bare-name and process-global.
	owner auth.Owner

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
	// oauthChallenge is the most recent captured `WWW-Authenticate` Bearer
	// challenge — inert, server-supplied. Nil when none seen.
	oauthChallenge *AuthChallenge
	// oauthRequirement is the discovered OAuth requirement chain.
	// Nil when discovery has not run.
	oauthRequirement *auth.OAuthRequirement
	// scopeShortfall is the most recent captured downstream
	// insufficient-scope step-up — inert, server-supplied. Nil when none seen.
	scopeShortfall *ScopeShortfall
}

// Registry is the process-local MCP-server read API. It is a compiled
// artifact — built once at construction; the provider set is
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
	// ContentShapes lists the canonical content shapes the tools return.
	ContentShapes []string
	// OAuthBindingCount is the configured OAuth binding count.
	OAuthBindingCount int
	// InitialState is the server's starting state. Zero-valued →
	// ServerStateOffline.
	InitialState ServerState
	// OAuthDiscoveryAllowedOrigins is the explicit per-connection cross-origin
	// allowance list for OAuth-requirement discovery fetches. Empty
	// leaves the authorization-server hop needs-allowance (partial discovery).
	OAuthDiscoveryAllowedOrigins []string
	// Owner is the (tenant, agent) reconcile-view tag for a RUNTIME-ADDED
	// server. Boot-declared servers leave it zero (untagged) — the
	// owner-scoped reconcile view never enumerates a zero-owner entry. The
	// runtime-add attach path stamps a non-zero owner (fail-closed there when
	// either component is missing); nothing about resolution or dispatch reads
	// it (those stay bare-name and process-global).
	Owner auth.Owner
}

// Register adds a server to the Registry. Re-registering the same name
// replaces the prior entry (the dev hot-reload path and the runtime
// re-attach path both re-register). A same-name replacement CLOSES the
// prior provider's transport so the replaced session drains instead of
// leaking: the entry is swapped under the write lock, then the displaced
// provider's Close runs OUTSIDE the lock (a transport close can block on
// session teardown and must not stall concurrent reads — mirroring
// Deregister). When the replacement re-registers the very same provider
// instance (an idempotent re-register of the live one), the close is
// skipped so the just-registered transport is not torn down.
func (r *Registry) Register(ctx context.Context, reg ServerRegistration) error {
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
	prior := r.servers[name]
	r.servers[name] = &serverEntry{
		provider:     reg.Provider,
		transport:    reg.Transport,
		urlOrCommand: reg.URLOrCommand,
		policy:       policy,
		// DisplayModes report what THIS HOST can render — the deployment's
		// configured `tools.mcp_app_host.display_modes`, not a value scraped
		// off the server (display modes are not a spec capability field;
		// they ride the `ui/initialize` host-context the host dictates).
		displayModes:        reg.Provider.DisplayModes(),
		contentShape:        append([]string(nil), reg.ContentShapes...),
		oauthAllowedOrigins: append([]string(nil), reg.OAuthDiscoveryAllowedOrigins...),
		owner:               reg.Owner,
		stats: serverStats{
			state:             st,
			oauthBindingCount: reg.OAuthBindingCount,
		},
	}
	r.mu.Unlock()
	// Close the DISPLACED provider's transport outside the lock so a same-name
	// replacement drains the old session instead of leaking it. Skip when the
	// prior entry held the exact same provider (a no-op re-register must not
	// close the transport we just re-registered).
	if prior != nil && prior.provider != nil && prior.provider != reg.Provider {
		if err := prior.provider.Close(ctx); err != nil {
			return fmt.Errorf("mcp: Register %q: close replaced transport: %w", name, err)
		}
	}
	return nil
}

// Deregister removes the named server from the Registry and closes its
// transport gracefully — the physical inverse of Register. It is the
// registry leg of detach-on-reconcile: when a run-start reconciliation finds
// a server that is attached but no longer declared by the agent's active
// config revision (a removed connection, or a rollback past an add), it
// deregisters the server so observability surfaces (mcp.servers.list) no
// longer list it and the subprocess / connection drains.
//
// The map entry is deleted under the write lock (so the server vanishes from
// the Registry atomically), then the provider's Close runs OUTSIDE the lock
// (a transport close can block on session teardown and must not stall
// concurrent reads). An unknown name returns ErrServerNotFound. Idempotent
// in effect: a second Deregister of the same name returns ErrServerNotFound,
// which the reconcile caller treats as already-detached.
func (r *Registry) Deregister(ctx context.Context, name string) error {
	r.mu.Lock()
	e, ok := r.servers[name]
	if ok {
		delete(r.servers, name)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if err := e.provider.Close(ctx); err != nil {
		return fmt.Errorf("mcp: deregister %q: close transport: %w", name, err)
	}
	return nil
}

// OwnerOf returns the (tenant, agent) owner tag of the named registration and
// whether a registration by that name currently exists. It is the read the
// same-name replace consults to keep an atomic upsert scoped to the caller's
// OWN registration: a re-attach that supersedes a still-live connection is the
// operator replacing their own, so tearing the old one down first is intended;
// a same-name attach by a DIFFERENT owner must never tear down another owner's
// live tools/transport. A boot-declared server carries the zero owner. The
// returned owner is a value copy, safe to read without the registry lock.
func (r *Registry) OwnerOf(name string) (auth.Owner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[name]
	if !ok {
		return auth.Owner{}, false
	}
	return e.owner, true
}

// SourceIDs returns the source ids of every currently-registered server —
// boot-declared AND runtime-added, across every owner. It is the PROCESS-GLOBAL
// enumeration (the deployment-shared attached set), NOT the owner-scoped
// reconcile view: the run-start reconcile uses [Registry.RuntimeAddedSources]
// instead so one owner's reconcile never sees (and never detaches) a boot
// server or another owner's runtime-add. Identity-free (a process-local read of
// the attached set, not an identity-scoped projection like ListServers); the
// result is a fresh sorted slice, safe to retain.
func (r *Registry) SourceIDs() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.servers))
	for name := range r.servers {
		out = append(out, name)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// RuntimeAddedSources returns the source ids of the runtime-added servers whose
// owner tag equals owner — the OWNER-SCOPED reconcile VIEW. Boot-declared
// (zero-owner) servers and every OTHER owner's runtime-adds are excluded, so a
// run-start reconcile for one owner enumerates only its own runtime-added set
// and can never detach a boot server or another owner's connection. A zero
// owner returns nil (a reconcile with no owner has nothing of its own to
// reconcile — it never falls back to the whole registry). The result is a
// fresh sorted slice, safe to retain.
//
// This is the ONLY owner-aware read on the Registry: the bare-name read /
// dispatch paths (ListServers, GetServer, OAuthDiscoveryTarget, ReadResource)
// stay process-global and untouched, so boot servers remain visible to every
// session regardless of the reconciling owner.
func (r *Registry) RuntimeAddedSources(owner auth.Owner) []string {
	if owner.IsZero() {
		return nil
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.servers))
	for name, e := range r.servers {
		if e.owner == owner {
			out = append(out, name)
		}
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// ReadResource fetches a single resource's content from the named MCP
// server under the request identity triple — the runtime-side leg of
// the `mcp.servers.read_resource` Protocol method. Identity is
// mandatory: a ctx without a full triple fails closed with
// ErrRegistryIdentityMissing. An unknown server name returns
// ErrServerNotFound.
func (r *Registry) ReadResource(ctx context.Context, name, uri string) (content []byte, mimeType string, err error) {
	if idErr := requireIdentity(ctx); idErr != nil {
		return nil, "", idErr
	}
	if err := ctx.Err(); err != nil {
		return nil, "", fmt.Errorf("mcp: ReadResource cancelled: %w", err)
	}
	e, eErr := r.entry(name)
	if eErr != nil {
		return nil, "", eErr
	}
	content, mimeType, err = e.provider.ReadResource(ctx, uri)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: ReadResource %q from %q: %w", uri, name, err)
	}
	return content, mimeType, nil
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
// shapes are projection-only; no per-call state lives on the Registry.
// Identity is mandatory.
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
	// The OAuth requirement rides the DETAIL read only (get/probe), never the
	// hot list row — see ServerView.OAuthRequirement.
	v.OAuthRequirement = e.stats.oauthRequirement
	// The last scope shortfall likewise rides the DETAIL read only — see
	// ServerView.LastScopeShortfall.
	if e.stats.scopeShortfall != nil {
		sf := *e.stats.scopeShortfall
		v.LastScopeShortfall = &sf
	}
	r.mu.RUnlock()
	return &v, nil
}

// RecordAuthChallenge records a captured `WWW-Authenticate` Bearer challenge
// on a server's state. It is invoked from the HTTP transport's
// challenge-capture callback whenever an MCP call answers `401`. Pure
// observation: it records inert, server-supplied data and never alters
// transport state or call semantics. An unknown name is a no-op (the
// connection may have been deregistered mid-flight) — recording a challenge is
// best-effort observability, never a hard failure on the call path.
func (r *Registry) RecordAuthChallenge(name string, ch AuthChallenge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return
	}
	captured := ch
	e.stats.oauthChallenge = &captured
}

// RecordScopeShortfall records a captured downstream insufficient-scope
// step-up on a server's state (mirrors RecordAuthChallenge for the 403 path).
// It is invoked from the HTTP transport's shortfall-capture callback whenever
// an MCP call answers `403` + `error="insufficient_scope"`. Pure observation:
// it records inert, server-supplied data and never alters transport state or
// call semantics. An unknown name is a no-op (the connection may have been
// deregistered mid-flight) — recording is best-effort observability, never a
// hard failure on the call path.
func (r *Registry) RecordScopeShortfall(name string, sf ScopeShortfall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return
	}
	captured := sf
	e.stats.scopeShortfall = &captured
}

// RecordOAuthRequirement records the discovered OAuth requirement chain on a
// server's state. Invoked by the on-demand discovery orchestrator
// after a probe walks the chain. An unknown name returns ErrServerNotFound.
func (r *Registry) RecordOAuthRequirement(name string, req *auth.OAuthRequirement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return ErrServerNotFound
	}
	e.stats.oauthRequirement = req
	return nil
}

// OAuthDiscoveryTarget returns the inputs the on-demand discovery walker needs
// for a server: the captured challenge (nil when none seen), the server URL,
// and the per-connection cross-origin allowance list. An unknown name returns
// ErrServerNotFound. The returned slices/pointers are copies — the caller may
// read them without holding the registry lock.
func (r *Registry) OAuthDiscoveryTarget(name string) (challenge *AuthChallenge, serverURL string, allowedOrigins []string, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[name]
	if !ok {
		return nil, "", nil, ErrServerNotFound
	}
	if e.stats.oauthChallenge != nil {
		ch := *e.stats.oauthChallenge
		challenge = &ch
	}
	return challenge, e.urlOrCommand, append([]string(nil), e.oauthAllowedOrigins...), nil
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
// runtime-side mirror (the legitimate carve-out for a preference
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

// SetOAuthDiscoveryOrigins FULL-REPLACES a server's OAuth-discovery
// cross-origin allowance list on the live registry and returns the prior set so
// the caller can compute the granted / revoked delta. It is the live half of
// the `agent_config.set_mcp_discovery_origins` write: the very next discovery
// walk reads the new set via OAuthDiscoveryTarget, so a grant lets a previously
// refused authorization-server hop through and a revoke refuses it.
//
// Revoke is symmetric: dropping an origin also PRUNES the recorded OAuth
// requirement's authorization-server entries whose provenance origin
// (SourceURL) is no longer allowed — by building a FRESH requirement and
// swapping the stored pointer under the lock. The registry hands the
// requirement out BY POINTER (GetServer returns it directly), so mutating the
// pointee in place would be a data race against a concurrent reader; the swap
// leaves any reader holding the prior (immutable) pointer with a consistent
// value. Origin matching reuses the discovery walker's exported origin
// normaliser (auth.OriginOf), so a port-differing origin never spuriously
// matches.
//
// The registry stays PROCESS-GLOBAL bare-name — identity is mandatory for
// AUTHORIZATION (a caller with no identity triple is refused), NOT for keying.
// An unknown name returns ErrServerNotFound.
func (r *Registry) SetOAuthDiscoveryOrigins(ctx context.Context, name string, origins []string) (prev []string, err error) {
	if idErr := requireIdentity(ctx); idErr != nil {
		return nil, idErr
	}
	e, eerr := r.entry(name)
	if eerr != nil {
		return nil, eerr
	}
	next := append([]string(nil), origins...)
	r.mu.Lock()
	prev = append([]string(nil), e.oauthAllowedOrigins...)
	e.oauthAllowedOrigins = next
	if e.stats.oauthRequirement != nil {
		e.stats.oauthRequirement = pruneRequirementToAllowed(e.stats.oauthRequirement, next)
	}
	r.mu.Unlock()
	return prev, nil
}

// pruneRequirementToAllowed returns req unchanged when every recorded
// authorization-server entry's provenance origin is still in the allowed set,
// or a FRESH requirement (a shallow copy with a filtered AuthorizationServers
// slice) when one or more entries were fetched from a now-revoked origin. The
// caller MUST swap the stored pointer under the registry lock — the returned
// value is a new pointer so a concurrent reader holding the prior pointer keeps
// a consistent, immutable snapshot (no in-place mutation of a handed-out
// pointer). Origins are normalised with the walker's normaliser so a
// port-differing origin does not spuriously survive.
func pruneRequirementToAllowed(req *auth.OAuthRequirement, allowedOrigins []string) *auth.OAuthRequirement {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if origin, ok := auth.OriginOf(o); ok {
			allowed[origin] = true
		}
	}
	kept := make([]auth.AuthorizationServerMeta, 0, len(req.AuthorizationServers))
	for _, as := range req.AuthorizationServers {
		origin, ok := auth.OriginOf(as.SourceURL)
		if ok && allowed[origin] {
			kept = append(kept, as)
		}
	}
	if len(kept) == len(req.AuthorizationServers) {
		return req // nothing revoked — keep the existing (immutable) pointer.
	}
	fresh := *req
	fresh.AuthorizationServers = kept
	return &fresh
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

// RecordDiscovery seeds the per-server stats from an already-fetched
// descriptor slice without re-calling provider.Discover. The boot-time
// dev attach path uses this so the Console MCP-page wire surface
// (`mcp.servers.list`) reports the actual tool count + a real
// `last_discovery_at`, not zero values.
//
// Pre-RecordDiscovery the boot-time path called Register() with
// initial-zero stats; the only API that updated stats was
// RefreshDiscovery, which re-runs the network call. Operators saw
// `tool_count: 0` and `last_discovery_at: 0001-01-01T00:00:00Z` on
// every just-booted Runtime because the boot-time discovery never
// reached the registry's stats — its result went straight to the tool
// catalog. a walkthrough fix.
//
// RecordDiscovery is a no-network counterpart to RefreshDiscovery:
// caller already has the descriptors (from a previous provider.Discover
// at boot), so the method just classifies them + writes the stats.
// State is set to Online (the descriptors arrived successfully) and
// recentLatencyMs is set to 0 (the boot-time latency is not threaded
// through; a follow-up RefreshDiscovery from the Console will populate
// it).
//
// Identity is NOT required — this is a server-side seeding gesture
// from the boot path, not a Protocol-edge read.
func (r *Registry) RecordDiscovery(name string, descs []tools.ToolDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.servers[name]
	if !ok {
		return fmt.Errorf("mcp: RecordDiscovery %q: unknown server (Register first)", name)
	}
	tc, rc, pc := classifyDescriptors(descs, name)
	e.stats.toolCount = tc
	e.stats.resourceCount = rc
	e.stats.promptCount = pc
	e.stats.lastDiscoveryAt = r.clock()
	e.stats.state = ServerStateOnline
	return nil
}

// classifyDescriptors counts tools / resources / prompts in a descriptor
// slice, using the synthetic-name markers.
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

// resourceURIFromToolName extracts the resource URI from a
// synthetic resource tool name (`<server>__resource.<uri>`).
func resourceURIFromToolName(toolName, server string) (string, bool) {
	prefix := server + resourceTypeSeparator + resourceNamePrefix
	if !strings.HasPrefix(toolName, prefix) {
		return "", false
	}
	return strings.TrimPrefix(toolName, prefix), true
}

// promptNameFromToolName extracts the prompt name from a
// synthetic prompt tool name (`<server>__prompt.<name>`).
func promptNameFromToolName(toolName, server string) (string, bool) {
	prefix := server + resourceTypeSeparator + promptNamePrefix
	if !strings.HasPrefix(toolName, prefix) {
		return "", false
	}
	return strings.TrimPrefix(toolName, prefix), true
}
