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
	// ErrAmbiguousServerID — the server id being registered would make the
	// `<sourceID>_<tool>` catalog key space ambiguous against an
	// already-registered id. See CheckServerIDUnambiguous for why that
	// matters and what the rule is.
	ErrAmbiguousServerID = fmt.Errorf("mcp: ambiguous server id (separator collision with a registered server)")
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
	// descriptorFingerprint identifies the complete canonical NON-SECRET
	// runtime-added descriptor that produced this registration. Empty for
	// boot-declared registrations. Read-only after registration.
	descriptorFingerprint string

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
	// DescriptorFingerprint is the canonical digest of the complete
	// NON-SECRET runtime-added descriptor. Empty for boot registrations.
	DescriptorFingerprint string
}

// CheckServerIDUnambiguous reports whether registering `name` would leave the
// `<sourceID>_<tool>` catalog key space ambiguous against an already-registered
// MCP server id, returning a wrapped ErrAmbiguousServerID when it would.
//
// # Why the key space has to be unambiguous
//
// A tool discovered from server S is registered in the catalog as
// `S_<toolName>` — a single underscore join in which NEITHER side is
// charset-constrained: a server id may contain underscores, and server-side
// tool names routinely do. The join is therefore not injective across arbitrary
// id pairs. When two ids are separator-ambiguous, a single catalog key can be
// parsed as belonging to either of them, and a consumer that BUILDS a key by
// prefixing a server id cannot know which server it just addressed.
//
// That matters because a key prefix is used as a confinement boundary: the
// Console's MCP-Apps host scopes a sandboxed App to its own server by
// qualifying every app-supplied tool name with the App's host-derived server
// id. The qualification is unconditional and the App cannot choose the id, so
// the boundary holds exactly as well as the key space is unambiguous — and no
// better. Downstream gates evaluate the posture of whichever server the key
// resolved to, so an ambiguous resolution is not visible to them either.
//
// Refusing an ambiguous pairing at registration is what makes the key space
// unambiguous AMONG MCP SERVER IDS, which is the precondition that boundary
// depends on.
//
// # Scope of the guarantee — MCP ids only
//
// This registry sees MCP servers. The tool catalog is SHARED: in-proc and HTTP
// tools register operator-chosen names into the same namespace with no source
// prefix at all, and this check never sees them. A bare tool name that happens
// to look like `<mcpServerID>_<something>` therefore reintroduces the same
// ambiguity through a door this guard does not cover.
//
// So the honest statement is: `<sourceID>_<tool>` identifies exactly one
// server AMONG MCP SERVER IDS. Closing the non-MCP door needs the resolved
// descriptor's `Source` compared exactly at dispatch — a runtime-side check, not
// a naming rule — which is recorded as a follow-up rather than bolted on here.
//
// # The rule
//
// Registering `N` is refused when a registered id `E` (E != N) satisfies either
// direction: `N` is under `E`'s namespace (`N == E + "_" + …`), or `E` is under
// `N`'s (`E == N + "_" + …`). Both directions are checked so ORDER does not
// matter — whichever of an ambiguous pair lands second is refused, and the
// runtime never ends up in a state the boot order alone decided.
//
// Re-registering the SAME id is always allowed: it is the hot-reload and
// runtime re-attach path, and replacing an entry cannot create an ambiguity
// that the id did not already have.
func (r *Registry) CheckServerIDUnambiguous(name string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return ambiguousAgainst(r.servers, name)
}

// ambiguousAgainst is the lock-free core of CheckServerIDUnambiguous. The
// caller holds the appropriate lock.
func ambiguousAgainst(servers map[string]*serverEntry, name string) error {
	for existing := range servers {
		if existing == name {
			// A same-id replacement (hot reload / re-attach) is not an
			// ambiguity — the id's relationship to every OTHER id is unchanged.
			continue
		}
		// The message names ONLY the id the caller supplied. The id it collided
		// with belongs to a server the caller may have no business knowing
		// exists — this error is surfaced to the runtime-attach response, so
		// echoing the other id would leak another owner's server name to
		// whoever probes for it (AGENTS.md §6: existence is never revealed
		// across identities). The operator diagnosing it has the boot log and
		// the full server list; the API caller does not need either.
		if strings.HasPrefix(name, existing+"_") || strings.HasPrefix(existing, name+"_") {
			return fmt.Errorf("%w: server id %q is separator-ambiguous with an already-registered "+
				"server id, so a %q catalog key would not identify one server; choose an id that is "+
				"not an underscore-extension of, and not extended by, another registered id",
				ErrAmbiguousServerID, name, "<serverID>_<tool>")
		}
	}
	return nil
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
	swap, err := r.StageRegistration(reg, nil)
	if err != nil {
		return err
	}
	if err := swap.Commit(ctx); err != nil {
		return fmt.Errorf("mcp: Register: %w", err)
	}
	return nil
}

func registrationEntry(reg ServerRegistration, descs []tools.ToolDescriptor, now time.Time) (*serverEntry, string, error) {
	if reg.Provider == nil {
		return nil, "", fmt.Errorf("mcp: Register requires a non-nil Provider")
	}
	name := string(reg.Provider.SourceID())
	if name == "" {
		return nil, "", fmt.Errorf("mcp: Register requires a non-empty provider source id")
	}
	if reg.Transport == "" {
		return nil, "", fmt.Errorf("mcp: Register requires a non-empty Transport")
	}
	policy := reg.Policy
	if isZeroPolicy(policy) {
		policy = tools.DefaultPolicy()
	}
	st := reg.InitialState
	if st == "" {
		st = ServerStateOffline
	}
	tc, rc, pc := classifyDescriptors(descs, name)
	entry := &serverEntry{
		provider:     reg.Provider,
		transport:    reg.Transport,
		urlOrCommand: reg.URLOrCommand,
		policy:       policy,
		// DisplayModes report what THIS HOST can render — the deployment's
		// configured `tools.mcp_app_host.display_modes`, not a value scraped
		// off the server (display modes are not a spec capability field;
		// they ride the `ui/initialize` host-context the host dictates).
		displayModes:          reg.Provider.DisplayModes(),
		contentShape:          append([]string(nil), reg.ContentShapes...),
		oauthAllowedOrigins:   append([]string(nil), reg.OAuthDiscoveryAllowedOrigins...),
		owner:                 reg.Owner,
		descriptorFingerprint: reg.DescriptorFingerprint,
		stats: serverStats{
			state:             st,
			oauthBindingCount: reg.OAuthBindingCount,
			toolCount:         tc,
			resourceCount:     rc,
			promptCount:       pc,
		},
	}
	if descs != nil {
		entry.stats.lastDiscoveryAt = now
	}
	return entry, name, nil

}

// RegistrationSwap is a reversible live-registry publication. Commit makes
// the staged entry final and drains the exact displaced provider. Rollback
// restores the exact prior entry only while this staged entry is still current.
type RegistrationSwap struct {
	mu       sync.Mutex
	registry *Registry
	name     string
	prior    *serverEntry
	staged   *serverEntry
	done     bool
}

// RecordAuthChallenge records on this receipt's exact staged entry, never on a
// same-name healthy prior registration while preparation is unpublished.
func (s *RegistrationSwap) RecordAuthChallenge(ch AuthChallenge) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	captured := ch
	s.staged.stats.oauthChallenge = &captured
}

// RecordScopeShortfall records a defensive copy on the exact staged entry.
func (s *RegistrationSwap) RecordScopeShortfall(sf ScopeShortfall) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	captured := sf
	captured.RequiredScopes = append([]string(nil), sf.RequiredScopes...)
	captured.GrantedScopes = append([]string(nil), sf.GrantedScopes...)
	s.staged.stats.scopeShortfall = &captured
}

// StageRegistration replaces one registry entry without closing the prior
// provider and returns an exact rollback receipt. All validation precedes the
// map mutation.
func (r *Registry) StageRegistration(reg ServerRegistration, descs []tools.ToolDescriptor) (*RegistrationSwap, error) {
	entry, name, err := registrationEntry(reg, descs, r.clock())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if err := ambiguousAgainst(r.servers, name); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	prior := r.servers[name]
	if prior != nil && prior.owner != reg.Owner {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: a connection named %q is already registered to a different owner", ErrConnectionNameOwnerConflict, name)
	}
	r.servers[name] = entry
	r.mu.Unlock()
	return &RegistrationSwap{registry: r, name: name, prior: prior, staged: entry}, nil
}

// Commit finalizes a staged registration. A displaced provider close error is
// cleanup failure after publication; callers must log it but must not roll the
// already-committed state back.
func (s *RegistrationSwap) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	if s.prior != nil && s.prior.provider != nil && s.prior.provider != s.staged.provider {
		if err := s.prior.provider.Close(ctx); err != nil {
			return fmt.Errorf("close replaced transport %q: %w", s.name, err)
		}
	}
	return nil
}

// Rollback restores the exact displaced registry entry iff the staged entry is
// still current. It never closes the displaced provider; it closes the staged
// provider later through PreparedAttachment.Close.
func (s *RegistrationSwap) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if s.registry.servers[s.name] != s.staged {
		return fmt.Errorf("mcp: registration rollback refused for %q: staged entry is no longer current", s.name)
	}
	if s.prior == nil {
		delete(s.registry.servers, s.name)
	} else {
		s.registry.servers[s.name] = s.prior
	}
	s.done = true
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
// The write is OWNER-SCOPED: owner is the (tenant, agent) tag of the
// registration the caller is entitled to remove, and the entry is removed only
// when its own tag equals it. A name nobody registered and a name another
// owner registered answer identically (ErrServerNotFound), so resolution never
// reveals which case applied. The ZERO owner matches the boot-declared
// (untagged) registrations and nothing else — the boot loader's same-name
// hot-reload replace is the one caller that legitimately holds it, and it is
// removing its own boot-declared entry. This is strictly narrower than a bare
// name, which reached every registration regardless of owner.
//
// The comparison is spelled out here rather than reusing a resolver like
// [Registry.ownedEntry] because it must run under the SAME write lock as the
// delete: a resolve-then-delete leaves a window a concurrent same-name replace
// by another owner can land in. (It is also not the same comparison —
// ownedEntry refuses the zero owner outright, which would break the boot
// loader's own hot-reload replace.) Both production callers already hold the
// matching owner — the
// attach replace has just compared it via [Registry.OwnerOf], and the run-start
// detach leg enumerates through [Registry.RuntimeAddedSources] — so moving the
// guard to this single choke point closes the mutator shape without changing
// any live path's outcome.
//
// The map entry is deleted under the write lock (so the server vanishes from
// the Registry atomically), then the provider's Close runs OUTSIDE the lock
// (a transport close can block on session teardown and must not stall
// concurrent reads). An unknown name returns ErrServerNotFound. Idempotent
// in effect: a second Deregister of the same name returns ErrServerNotFound,
// which the reconcile caller treats as already-detached.
func (r *Registry) Deregister(ctx context.Context, name string, owner auth.Owner) error {
	r.mu.Lock()
	e, ok := r.servers[name]
	if ok && e.owner != owner {
		ok = false // another owner's registration — answer as if absent.
	}
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
	owner, _, ok := r.RegistrationIdentity(name)
	return owner, ok
}

// RegistrationIdentity atomically returns the reconcile owner and canonical
// descriptor fingerprint of one live registration. The pair must be read under
// one lock: separate owner/fingerprint reads could compare fields from two
// same-name replacements and incorrectly classify a stale registration as
// current.
func (r *Registry) RegistrationIdentity(name string) (auth.Owner, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[name]
	if !ok {
		return auth.Owner{}, "", false
	}
	return e.owner, e.descriptorFingerprint, true
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

// ownedEntry returns the named server entry ONLY when its owner tag equals
// owner — the OWNER-SCOPED resolution the registry's write paths use. A name
// that resolves to a boot-declared (zero-owner) entry, or to an entry another
// (tenant, agent) owner registered, returns ErrServerNotFound: a write applies
// to the caller's OWN registration, and a caller that does not own the name
// sees the same answer it would see for a name nobody registered. Caller must
// NOT hold r.mu; the owner tag is set at Register and read-only thereafter, so
// the read lock is sufficient.
//
// A ZERO owner owns nothing and always returns ErrServerNotFound — it never
// resolves to the boot-declared (zero-owner) entries, which is what a bare
// equality check would do. Boot state is not a runtime owner's to write, and
// the guard lives HERE, at the single resolution choke point, so no present or
// future caller can reach a boot entry by omitting its owner. This mirrors
// [Registry.RuntimeAddedSources], which likewise returns nothing for a zero
// owner rather than falling back to the whole registry.
//
// The owner tag stays a WRITE-SCOPE filter, exactly as it is a reconcile-view
// filter for [Registry.RuntimeAddedSources]: it is never an isolation key and
// never a dispatch key. Bare-name resolution, dispatch, and every read
// projection (ListServers, GetServer, OAuthDiscoveryTarget, ReadResource) stay
// process-global and untouched, so boot servers remain visible to every
// session.
func (r *Registry) ownedEntry(name string, owner auth.Owner) (*serverEntry, error) {
	if owner.IsZero() {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	r.mu.RLock()
	e, ok := r.servers[name]
	var entryOwner auth.Owner
	if ok {
		entryOwner = e.owner
	}
	r.mu.RUnlock()
	if !ok || entryOwner != owner {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	return e, nil
}

// tenantEntry returns the named server entry ONLY when the caller's TENANT may
// write it — the resolution the wire-reachable per-server admin writes use.
//
// It resolves when the registration is boot-declared (the zero owner) or when
// the registration's owner tenant equals tenant. It returns ErrServerNotFound
// for an empty tenant, for a name nobody registered, and for a registration
// another TENANT owns — the same answer in every refused case, so resolution
// never reveals which one applied.
//
// # Why the tenant and not the (tenant, agent) owner
//
// The wire door for these writes carries the caller's verified identity triple
// (tenant, user, session) and no agent id, so the (tenant, agent) owner tag
// [Registry.ownedEntry] compares is not derivable at that edge. The tenant IS,
// and it is the boundary that matters: co-tenant admins already share the
// runtime-added connection namespace by construction, while a registration
// another TENANT owns is outside anything the caller's verified identity
// reaches. Scoping to the tenant is therefore the strongest comparison the
// caller's own identity supports, and it is the whole comparison the boundary
// needs.
//
// # Why boot-declared registrations stay writable
//
// A boot-declared server is deployment-global infrastructure: it is declared in
// the deployment's own configuration, it resolves and dispatches by bare name
// across every session, and every session's read surface already lists it.
// Its per-server admin preferences have no per-owner home and no other
// door that can set them, so refusing the write here would not scope the
// preference — it would delete it. The honest, bounded guarantee is therefore:
// a write lands on the caller's own tenant's registration, or on the
// deployment's own boot-declared infrastructure; never on another tenant's
// runtime-added registration.
//
// Caller must NOT hold r.mu.
func (r *Registry) tenantEntry(name, tenant string) (*serverEntry, error) {
	if tenant == "" {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	r.mu.RLock()
	e, ok := r.servers[name]
	var entryOwner auth.Owner
	if ok {
		entryOwner = e.owner
	}
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if !entryOwner.IsZero() && entryOwner.Tenant != tenant {
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
//
// # It is a READ, and its bare-name resolution is deliberate
//
// RefreshDiscovery writes registry state, but everything it writes is an
// OBSERVATION derived from the round-trip it just performed — tool / resource /
// prompt counts, the discovery timestamp, the measured latency, the reachable
// state. Nothing it writes is chosen by the caller, and nothing it writes is
// consulted as policy on any later authorization or rendering decision. The
// same fields are written unsolicited by the transport's own callbacks
// ([Registry.RecordDiscovery], [Registry.RecordReconnect], recordError) from
// any session's ordinary traffic, so owner- or tenant-scoping this call would
// not change who can affect the state — it would only make a boot-declared
// server's refresh unreachable.
//
// Resolution therefore stays bare-name and process-global, like every other
// read projection (ListServers, GetServer, ListResources, ListPrompts, Health,
// OAuthDiscoveryTarget). The connection WRITES — the ones that persist
// caller-chosen policy or remove the registration itself — are the scoped ones
// (see [Registry.SetRawHTMLTrust], [Registry.SetOAuthDiscoveryOrigins],
// [Registry.Deregister]).
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
//
// # It is a READ, and its bare-name resolution is deliberate
//
// Like [Registry.RefreshDiscovery], Probe records only what the round-trip it
// just performed OBSERVED — the measured latency, and a reachable/unreachable
// state transition. A failed probe's recordError bump is likewise a truthful
// observation: it fires only when the server genuinely failed to answer, and
// ordinary dispatch traffic writes the same fields. No caller-chosen value is
// persisted and nothing recorded here is consulted as policy later, so
// resolution stays bare-name and process-global alongside the other read
// projections. See [Registry.SetRawHTMLTrust] for the contrasting WRITE shape.
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
//
// The write is TENANT-SCOPED. The flag governs the sandbox posture a rendered
// MCP App is given, so it is caller-chosen policy consulted on a later render
// — a connection WRITE, not an observation. It therefore resolves through
// [Registry.tenantEntry] rather than the bare-name [Registry.entry]: it lands
// on a registration the caller's own tenant owns, or on a boot-declared
// (deployment-global) one, and answers ErrServerNotFound for a registration
// another tenant owns — indistinguishably from a name nobody registered.
//
// The scoping tenant is read from ctx, NOT taken as a parameter. ctx already
// carries the verified triple this method requires, and it is the identity the
// Protocol edge reconciled against the request body before dispatching; taking
// it as an argument would add a seam a caller could populate with a tenant it
// does not hold. Deriving it here also makes the write and any COMPENSATING
// REVERT of that write resolve identically, since both run on the same ctx —
// an admin write whose audit emit fails must be revertible, and a revert that
// could fail to resolve where the apply succeeded would leave the toggle
// observably applied but unrecorded.
//
// Registry READS stay bare-name and process-global — boot servers and
// runtime-added servers alike remain visible to every session, and resolution
// and dispatch are untouched.
func (r *Registry) SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (prev bool, err error) {
	if err := requireIdentity(ctx); err != nil {
		return false, err
	}
	// requireIdentity already proved the triple is present and complete.
	id, _ := identity.From(ctx)
	e, eerr := r.tenantEntry(name, id.TenantID)
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
// The WRITE, however, is OWNER-SCOPED: owner is the caller's (tenant, agent)
// tag and the allow-list is replaced only on the registration carrying that
// same tag, so an allowance write lands on the caller's OWN connection. A name
// that is unregistered, boot-declared (zero owner), or registered to a
// different owner all return ErrServerNotFound — resolution and dispatch are
// unaffected and stay bare-name (see [Registry.ownedEntry]). This mirrors the
// owner comparison the same-name attach replace already performs via
// [Registry.OwnerOf].
func (r *Registry) SetOAuthDiscoveryOrigins(ctx context.Context, name string, owner auth.Owner, origins []string) (prev []string, err error) {
	if idErr := requireIdentity(ctx); idErr != nil {
		return nil, idErr
	}
	e, eerr := r.ownedEntry(name, owner)
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
