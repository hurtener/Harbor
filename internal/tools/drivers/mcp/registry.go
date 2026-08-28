package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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

type appBindingProvider interface {
	ValidateAppBinding(context.Context, string, string) bool
}

// ValidateAppBinding verifies a runtime-issued callback capability against the
// named server. The token, rather than server_id, is the callback authority.
func (r *Registry) ValidateAppBinding(ctx context.Context, serverID, token, resourceURI string) bool {
	if err := requireIdentity(ctx); err != nil {
		return false
	}
	id, _ := identity.From(ctx)
	r.mu.RLock()
	entry := r.servers[serverID]
	visible := entry != nil && entryVisibleToIdentity(entry, id)
	r.mu.RUnlock()
	if !visible {
		return false
	}
	p, ok := entry.provider.(appBindingProvider)
	return ok && p.ValidateAppBinding(ctx, token, resourceURI)
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

// SourceVisibility is an optional caller-owned admission predicate for
// identity-scoped registry reads. Registry itself supplies the immutable
// owner/logical snapshot; the predicate may consult a durable external
// authority without making the process-local registry a policy store. A false
// result is projected as ErrServerNotFound so an unselected personal source
// cannot become an existence oracle.
type SourceVisibility func(ctx context.Context, source tools.ToolSourceID, owner auth.Owner, logical string) (bool, error)

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
	// ErrDiscoveryStale means a refresh completed for a server generation that
	// is no longer current. Callers may retry against the replacement.
	ErrDiscoveryStale = errors.New("mcp: discovery result is stale")
	// ErrGenerationMismatch is the typed outcome of
	// ResolveAppToolAtGeneration when the server's CURRENT
	// provider/catalog generation cannot be established as the exact
	// expected one — the server is absent, has no established current
	// generation, or was refreshed/replaced between an earlier generation
	// read and the atomic compare+resolve. It is a refusal (the render
	// admission is stale), never a resolution of a newer-generation row.
	ErrGenerationMismatch = errors.New("mcp: App callback resolution raced a catalog generation change")
)

// DiscoveryStaleError is the bounded, retryable outcome for a refresh whose
// captured server entry was replaced while discovery was in flight.
type DiscoveryStaleError struct {
	Retry bool
}

func (e *DiscoveryStaleError) Error() string { return ErrDiscoveryStale.Error() }

func (e *DiscoveryStaleError) Is(target error) bool { return target == ErrDiscoveryStale }

// maxListPageSize / defaultListPageSize bound the ListServers page.
const (
	maxListPageSize          = 200
	defaultListPageSize      = 50
	userPhysicalSourceMarker = "~u-"
)

// PhysicalServerName derives the process-local source id for one logical MCP
// connection. Operator/boot registrations keep their logical name. A
// user-owned registration gets a deterministic owner-derived suffix so two
// users can attach the same signed descriptor/name concurrently without a
// client selecting a destination. The logical descriptor and downstream URL
// are unchanged; this value is only a local registry/catalog key.
func PhysicalServerName(logical string, owner auth.Owner) string {
	if owner.User == "" {
		return logical
	}
	h := sha256.Sum256([]byte("harbor:mcp:user-source:v1\x00" + owner.Tenant + "\x00" + owner.Agent + "\x00" + owner.User))
	return logical + userPhysicalSourceMarker + hex.EncodeToString(h[:16])
}

// serverEntry is the Registry's per-server record — the provider plus
// its operator-supplied static config plus mutable runtime stats.
type serverEntry struct {
	provider     serverProvider
	logicalName  string
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
	// owner-scoped reconcile view never enumerates. A user-scoped registration
	// additionally carries its verified user id. Set at Register;
	// read-only thereafter. For user-owned registrations it also selects the
	// physical registry/catalog namespace; operator/boot behavior remains bare
	// name. The downstream URL and token sink are never derived from this key.
	owner auth.Owner
	// descriptorFingerprint identifies the complete canonical NON-SECRET
	// runtime-added descriptor that produced this registration. Empty for
	// boot-declared registrations. Read-only after registration.
	descriptorFingerprint string
	// appVisible is the per-server App dispatch catalog: the App-visible
	// callbacks (provider-authored `_meta.ui.visibility` containing `app`)
	// discovered on THIS server, keyed by their full Harbor catalog name
	// (`<source>_<tool>`).
	// It is deliberately a SEPARATE view from the ordinary planner/model
	// projection (the tool catalog): the attach path publishes only the
	// non-app-only descriptors to the catalog, so an app-only callback is
	// absent from planner context / tools/list / search / resolve / ordinary
	// invocation by construction. A mixed-visibility tool remains in that
	// ordinary projection and is also resolved here by a rendered App of this
	// SAME server, still under the identity / reach / OAuth / approval /
	// current-state gates. Guarded by the Registry's mu (RWMutex) exactly
	// like stats: written only while mu is held for writing, read only
	// while held for reading. Populated from the SAME discovered descriptor
	// snapshot that seeds the catalog publication, so attach / reconnect /
	// refresh / replacement / detach move both views together.
	appVisible map[string]tools.ToolDescriptor
	// catalog is the ordinary planner/model projection paired with appVisible.
	// Refreshes reconcile both views from one filtered discovery snapshot.
	catalog       tools.ToolCatalog
	toolAllowlist []string
	toolDenylist  []string
	// currentGeneration is the DETERMINISTIC current provider/catalog
	// generation fingerprint for this server: a content digest of the
	// canonical CURRENT descriptor set (resources + App callbacks +
	// ordinary catalog), recomputed whenever a successful discovery
	// snapshot rebuilds either view. It changes on detach (entry removed
	// → unknown → fail closed), replacement (new descriptor set), and
	// every successful discovery change to resources / App callbacks /
	// ordinary catalog — even when the deployment registration
	// descriptor (`descriptorFingerprint`) did not change. It is stable
	// across replicas with the same canonical current descriptor set
	// (content-derived, NEVER a process-local counter), and empty means
	// "unknown" (fail closed: a render admission never binds an empty
	// generation). Guarded by the Registry's mu like every other
	// per-server field.
	currentGeneration string

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
	// closing holds exact registrations after catalog withdrawal has made them
	// unreachable to dispatch but before their owned transport/provider Close
	// has returned success. The entry remains generation-addressable so an
	// exact teardown retry closes the same handle instead of mistaking absence
	// for a receipt, and its name remains reserved against replacement.
	closing map[string]*serverEntry
	// genericClosing retains the exact provider handle removed by ordinary
	// owner-scoped Deregister until Close succeeds. It is separate from closing
	// so signed exact-generation teardown semantics remain unchanged.
	genericClosing map[string]*genericCloseReceipt
	// pending reserves names for reversible registrations without exposing the
	// staged provider to ordinary reads. A receipt remains here until Commit or
	// Rollback, so no concurrent register/deregister can invalidate the exact
	// prior entry captured by the transaction.
	pending map[string]*RegistrationSwap
	// removing is the shared publication/teardown admission fence for one
	// exact runtime-added generation. Removal installs it before desired-state
	// CAS, so a staged publication either linearizes first or is invalidated
	// before pair absence can become durable. A sealed fence remains until exact
	// teardown receipts transport Close; crashes need no recovery record because
	// the durable pair absence is the restart-side publication proof.
	removing map[string]*exactRemovalReservation
	clock    func() time.Time
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
		servers:        map[string]*serverEntry{},
		closing:        map[string]*serverEntry{},
		genericClosing: map[string]*genericCloseReceipt{},
		pending:        map[string]*RegistrationSwap{},
		removing:       map[string]*exactRemovalReservation{},
		clock:          time.Now,
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
	// user field is populated for a user-scoped registration and participates
	// in exact ownership checks.
	// runtime-add attach path stamps a non-zero owner (fail-closed there when
	// either component is missing); nothing about resolution or dispatch reads
	// it for operator-owned entries. User-owned entries use a server-derived
	// physical key; the logical descriptor name remains in LogicalName.
	Owner auth.Owner
	// LogicalName is the signed/config descriptor name before any server-side
	// user namespace is applied. Empty preserves the provider source id for
	// boot and legacy callers.
	LogicalName string
	// DescriptorFingerprint is the canonical digest of the complete
	// NON-SECRET runtime-added descriptor. Empty for boot registrations.
	DescriptorFingerprint string
	// Catalog is the ordinary planner/model projection paired with this
	// registration, allowing refresh to reconcile both views together.
	Catalog tools.ToolCatalog
	// ToolAllowlist is the signed restrictive tool-name policy applied to
	// discovery snapshots before either projection is rebuilt.
	ToolAllowlist []string
	// ToolDenylist is the signed restrictive tool-name policy applied to
	// discovery snapshots before either projection is rebuilt.
	ToolDenylist []string
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
	if err := ambiguousAgainst(r.servers, name); err != nil {
		return err
	}
	if err := ambiguousAgainst(r.closing, name); err != nil {
		return err
	}
	for existing := range r.genericClosing {
		if existing != name && (strings.HasPrefix(name, existing+"_") || strings.HasPrefix(existing, name+"_")) {
			return fmt.Errorf("%w: server id %q is separator-ambiguous with a closing server id", ErrAmbiguousServerID, name)
		}
	}
	return nil
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
		logicalName:  reg.LogicalName,
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
		catalog:               reg.Catalog,
		toolAllowlist:         append([]string(nil), reg.ToolAllowlist...),
		toolDenylist:          append([]string(nil), reg.ToolDenylist...),
		stats: serverStats{
			state:             st,
			oauthBindingCount: reg.OAuthBindingCount,
			toolCount:         tc,
			resourceCount:     rc,
			promptCount:       pc,
		},
	}
	if entry.logicalName == "" {
		entry.logicalName = name
	}
	// Partition the SAME discovered snapshot into the two views: App-visible
	// callbacks stay HERE on the per-server App dispatch catalog (read via
	// ResolveAppTool), while the attach path publishes only the non-app-only
	// descriptors to the ordinary planner/model catalog. Mixed visibility
	// tools therefore appear in BOTH views; exact app-only callbacks appear
	// only here. Both views always derive from one discovered set — a refresh
	// / replacement swaps the appVisible set exactly when it swaps the catalog
	// publication, so no stale callback can survive either direction.
	entry.appVisible = partitionAppVisible(descs)
	entry.currentGeneration = currentGenerationFor(descs)
	if descs != nil {
		entry.stats.lastDiscoveryAt = now
	}
	return entry, name, nil

}

// partitionAppVisible splits a discovered descriptor snapshot into the
// per-server App dispatch catalog — every descriptor whose Tool carries
// the provider-authored App-visible classification (including mixed
// `_meta.ui.visibility` such as `["model", "app"]`), keyed by full catalog
// name. A duplicate name within the set is impossible for a single
// provider's discovery (the MCP server's own tool names are unique), so a
// collision is skipped defensively rather than panicking; the ordinary
// catalog's StageSource enforces the real uniqueness gate for the other
// view.
func partitionAppVisible(descs []tools.ToolDescriptor) map[string]tools.ToolDescriptor {
	var out map[string]tools.ToolDescriptor
	for _, d := range descs {
		if !d.Tool.AppOnly && !d.Tool.AppVisible {
			continue
		}
		if out == nil {
			out = make(map[string]tools.ToolDescriptor, 1)
		}
		if _, dup := out[d.Tool.Name]; dup {
			continue
		}
		out[d.Tool.Name] = d
	}
	return out
}

// currentGenerationFor computes the deterministic current
// provider/catalog generation fingerprint for a canonical descriptor
// set: a SHA-256 digest over the canonical encoding of every STABLE
// semantic field of every current tools.Tool descriptor.
//
// # Covered fields — every stable semantic field that can affect
// ordinary / App / resource / prompt catalog meaning
//
// name, description, ArgsSchema and OutSchema, SideEffects, Tags,
// AuthScopes, CostHint, LatencyHint, SafetyNotes, Loading, Examples (in
// order, each with its canonical JSON Args, Description, and Tags),
// Source, Transport, Policy (TimeoutMS, MaxRetries, BackoffBase,
// BackoffMult, BackoffMax, RetryOn, Validate), HandlesMIME, Form, and
// AppOnly and AppVisible.
//
// # Excluded
//
// The descriptor's Invoke and Validate function values (process code,
// not catalog meaning), secrets, timestamps, process-local counters,
// and mutable runtime stats — none of which live on Tool or are hashed.
//
// # Honest canonicalization
//
// Set-like fields (Tags, AuthScopes, HandlesMIME, Policy.RetryOn,
// example Tags) are sorted before encoding, so element ORDER never
// changes the generation. Order-bearing fields (the Examples list) keep
// their order. ArgsSchema and OutSchema are canonicalized by SEMANTIC
// JSON VALUE before the length-prefixed write: each valid document is
// re-encoded compactly, with deterministic object-key order and an exact
// canonical number form, so a replica whose discovery serialization
// differs only in whitespace or object-key order hashes identically,
// while a meaningful schema change — and array-order changes — still
// moves the generation. A schema that is not a single canonical JSON
// document (invalid JSON, non-UTF-8 bytes, trailing data, duplicate
// object members at any nesting depth, a non-finite literal, or a number
// with no exact canonical decimal form) fails the row, so the whole
// generation becomes unknown: raw invalid bytes are never hashed as
// authoritative state.
// Empty schema bytes — the MCP driver's encoding of "no schema
// declared" — are preserved as the fixed empty marker instead of being
// rejected. Example Args maps are marshalled with encoding/json's
// deterministic key-sorted form (a nil map encodes as the empty map:
// both mean "no args"). Descriptors are sorted by their canonical row
// bytes, so discovery ORDER never changes the generation.
//
// Identical semantic catalogs across replicas hash identically; changing
// any covered semantic field changes the generation. The digest is NEVER
// a process-local counter. An empty/nil set, or a set containing a
// descriptor whose stable fields cannot be canonically encoded (a
// non-serializable example args value, or a non-canonical schema),
// yields "" — unknown, fail closed: a render admission never binds an
// empty generation, and a non-canonically determinable catalog is
// refused rather than guessed.
func currentGenerationFor(descs []tools.ToolDescriptor) string {
	if len(descs) == 0 {
		return ""
	}
	rows := make([][]byte, 0, len(descs))
	for _, d := range descs {
		row, err := canonicalDescriptorRow(d)
		if err != nil {
			// A descriptor whose stable fields cannot be canonically
			// encoded is not deterministically classifiable: unknown,
			// fail closed (the admission gate refuses the tuple).
			return ""
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })
	h := sha256.New()
	for _, row := range rows {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(row)))
		h.Write(lenBuf[:])
		h.Write(row)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalDescriptorRow encodes the stable semantic fields of ONE
// tools.ToolDescriptor into a self-delimiting byte row. Every field is
// length-prefixed (big-endian), so no value bytes — including NULs and
// JSON — can fuse with a neighbour or alias a different field. The
// descriptor's Invoke / Validate function values are deliberately not
// encoded: they are process code, not catalog meaning.
//
// The row is a pure function of the Tool's stable fields: two replicas
// with identical semantic content produce identical bytes, and changing
// any covered field changes the bytes. An error means the descriptor's
// stable fields cannot be canonically encoded (a non-serializable
// example args map, or an ArgsSchema / OutSchema that is not a single
// canonical JSON document) — the caller treats the catalog as
// non-determinable and the generation as unknown.
func canonicalDescriptorRow(d tools.ToolDescriptor) ([]byte, error) {
	t := d.Tool
	var buf bytes.Buffer
	// Every write below targets *bytes.Buffer (or the sha256 in
	// currentGenerationFor), which never fails; the bare calls match the
	// registry's established digest pattern.
	writeField := func(s string) {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		buf.Write(lenBuf[:])
		buf.WriteString(s)
	}
	writeInt := func(v int64) error {
		if v < 0 {
			// Every caller passes a length, count, or duration — all
			// non-negative by construction; a negative value would be a
			// corrupt field and must fail the row closed, never wrap
			// into a garbage prefix byte.
			return fmt.Errorf("mcp: canonical row field is negative: %d", v)
		}
		var fieldBuf [8]byte
		binary.BigEndian.PutUint64(fieldBuf[:], uint64(v))
		buf.Write(fieldBuf[:])
		return nil
	}
	writeFloat := func(f float64) {
		var fieldBuf [8]byte
		binary.BigEndian.PutUint64(fieldBuf[:], math.Float64bits(f))
		buf.Write(fieldBuf[:])
	}
	writeSorted := func(values []string) error {
		if err := writeInt(int64(len(values))); err != nil {
			return err
		}
		sorted := append([]string(nil), values...)
		sort.Strings(sorted)
		for _, v := range sorted {
			writeField(v)
		}
		return nil
	}

	writeField(t.Name)
	writeField(t.Description)
	argsSchema, err := canonicalJSONSchema(t.ArgsSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: args schema: %w", err)
	}
	writeField(argsSchema)
	outSchema, err := canonicalJSONSchema(t.OutSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: out schema: %w", err)
	}
	writeField(outSchema)
	writeField(string(t.SideEffects))
	if err := writeSorted(t.Tags); err != nil {
		return nil, err
	}
	if err := writeSorted(t.AuthScopes); err != nil {
		return nil, err
	}
	writeField(t.CostHint)
	if err := writeInt(int64(t.LatencyHint)); err != nil {
		return nil, err
	}
	writeField(t.SafetyNotes)
	writeField(string(t.Loading))
	// Examples are order-bearing (the planner sees them in order), so
	// they keep their order.
	if err := writeInt(int64(len(t.Examples))); err != nil {
		return nil, err
	}
	for _, ex := range t.Examples {
		writeField(ex.Description)
		if err := writeSorted(ex.Tags); err != nil {
			return nil, err
		}
		args := ex.Args
		if args == nil {
			// nil and the empty map are semantically the same "no args".
			args = map[string]any{}
		}
		argsJSON, err := json.Marshal(args) // deterministic: encoding/json sorts map keys
		if err != nil {
			return nil, err
		}
		writeField(string(argsJSON))
	}
	writeField(string(t.Source))
	writeField(string(t.Transport))
	// Policy — the reliability shell, a stable semantic field family.
	if err := writeInt(int64(t.Policy.TimeoutMS)); err != nil {
		return nil, err
	}
	if err := writeInt(int64(t.Policy.MaxRetries)); err != nil {
		return nil, err
	}
	if err := writeInt(int64(t.Policy.BackoffBase)); err != nil {
		return nil, err
	}
	writeFloat(t.Policy.BackoffMult)
	if err := writeInt(int64(t.Policy.BackoffMax)); err != nil {
		return nil, err
	}
	// RetryOn has a behaviorally meaningful nil-versus-explicit-empty
	// distinction that the digest MUST preserve: a nil RetryOn (zero
	// value) inherits the default retry allowlist ([transient, timeout,
	// 5xx]) at dispatch, while a non-nil empty slice means "retry on
	// nothing" (one attempt only). Encoding only the sorted members would
	// collapse the two policies into one generation, so a presence marker
	// rides ahead of the sorted member list. Sorting the members of a
	// non-empty set stays order-independent — the marker is what keeps the
	// two policies distinct.
	if t.Policy.RetryOn == nil {
		writeField("nil")
	} else {
		writeField("explicit")
	}
	retryOn := make([]string, len(t.Policy.RetryOn))
	for i, class := range t.Policy.RetryOn {
		retryOn[i] = string(class)
	}
	if err := writeSorted(retryOn); err != nil {
		return nil, err
	}
	writeField(string(t.Policy.Validate))
	if err := writeSorted(t.HandlesMIME); err != nil {
		return nil, err
	}
	writeField(string(t.Form))
	if t.AppOnly {
		writeField("1")
	} else {
		writeField("0")
	}
	if t.AppVisible {
		writeField("1")
	} else {
		writeField("0")
	}
	return buf.Bytes(), nil
}

// canonicalJSONSchema canonicalizes ONE JSON document by SEMANTIC value:
// compact output, deterministic object-key order, an exact canonical
// number form, exactly one document, and no ambiguous duplicate object
// members. It returns the canonical re-encoding, or an error when the
// raw bytes are not a single canonical JSON document. Empty bytes — the
// MCP driver's encoding of "no schema declared" (marshalSchema(nil) →
// nil) — canonicalize to the empty string: a legitimate semantic state,
// not a JSON document, so schema-free tools keep a deterministic
// replica-stable row without failing closed.
//
// Non-empty bytes that are not valid UTF-8 are rejected BEFORE any JSON
// token decoding: encoding/json replacement-normalizes invalid bytes to
// U+FFFD instead of failing, so without the UTF-8 gate two distinct
// corrupt documents would collapse into one authoritative generation.
// The gate makes each corrupt document fail the row closed instead.
func canonicalJSONSchema(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", errors.New("schema bytes are not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Preserve number literals verbatim so canonicalJSONNumber can reduce
	// them exactly; without UseNumber the decoder would parse to float64
	// and silently lose precision on large integers.
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	var out strings.Builder
	if err := canonicalJSONValue(dec, first, &out); err != nil {
		return "", err
	}
	// Exactly one document: the walker consumed the first value, so any
	// remaining bytes form a trailing document (or invalid trailing data).
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("trailing JSON document")
		}
		return "", fmt.Errorf("trailing JSON document: %w", err)
	}
	return out.String(), nil
}

// canonicalJSONValue walks one JSON value starting at the token the
// caller already read, appending its canonical re-encoding to out.
// Objects re-emit members in sorted key order; arrays keep their element
// order (array order is order-bearing in JSON Schema). The walk rejects
// duplicate object members at any nesting depth, so last-key-wins
// coercion can never hide an ambiguous document.
func canonicalJSONValue(dec *json.Decoder, first json.Token, out *strings.Builder) error {
	if delim, composite := first.(json.Delim); composite {
		switch delim {
		case '{':
			return canonicalJSONObject(dec, out)
		case '[':
			return canonicalJSONArray(dec, out)
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	switch v := first.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		return writeJSONString(out, v)
	case json.Number:
		canon, err := canonicalJSONNumber(v)
		if err != nil {
			return err
		}
		out.WriteString(canon)
	default:
		return fmt.Errorf("unexpected JSON token of type %T", first)
	}
	return nil
}

// canonicalMember is one object member's canonical re-encoding, sorted
// by name before emission.
type canonicalMember struct {
	name  string
	value string
}

func canonicalJSONObject(dec *json.Decoder, out *strings.Builder) error {
	seen := make(map[string]struct{})
	var members []canonicalMember
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok {
			return errors.New("expected JSON object member name")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate JSON object member %q", name)
		}
		seen[name] = struct{}{}
		valueToken, err := dec.Token()
		if err != nil {
			return err
		}
		var value strings.Builder
		if err := canonicalJSONValue(dec, valueToken, &value); err != nil {
			return err
		}
		members = append(members, canonicalMember{name: name, value: value.String()})
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("unterminated JSON object")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	out.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			out.WriteByte(',')
		}
		if err := writeJSONString(out, m.name); err != nil {
			return err
		}
		out.WriteByte(':')
		out.WriteString(m.value)
	}
	out.WriteByte('}')
	return nil
}

func canonicalJSONArray(dec *json.Decoder, out *strings.Builder) error {
	var elements []string
	for dec.More() {
		valueToken, err := dec.Token()
		if err != nil {
			return err
		}
		var value strings.Builder
		if err := canonicalJSONValue(dec, valueToken, &value); err != nil {
			return err
		}
		elements = append(elements, value.String())
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim(']') {
		return errors.New("unterminated JSON array")
	}
	out.WriteByte('[')
	for i, e := range elements {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(e)
	}
	out.WriteByte(']')
	return nil
}

// writeJSONString appends s as a JSON string literal. encoding/json
// cannot fail on a plain string; the error is returned for shape
// uniformity with the other canonical encoders.
func writeJSONString(out *strings.Builder, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	out.Write(b)
	return nil
}

// canonicalJSONNumber reduces a JSON number literal to its exact
// canonical decimal form: no exponent, no leading or trailing zeros, no
// sign on zero. Semantically equal literals (1, 1.0, 1e0) converge, so
// a replica whose discovery serialization renders the same number
// differently hashes identically; a literal with no exact canonical
// decimal form (SetString failure — e.g. an astronomically large
// exponent) is rejected so the document fails closed instead of hashing
// a lossy rendering.
func canonicalJSONNumber(n json.Number) (string, error) {
	rat, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return "", fmt.Errorf("JSON number %q has no exact canonical decimal form", n.String())
	}
	// A JSON number literal is a finite decimal, so its reduced
	// denominator is always of the form 2^a · 5^b. Anything else would
	// mean SetString accepted a form the JSON grammar cannot produce.
	a, b, ok := finiteDecimalFactors(rat.Denom())
	if !ok {
		return "", fmt.Errorf("JSON number %q has no exact canonical decimal form", n.String())
	}
	// FloatString is exact when the value terminates at or before prec
	// digits; a terminating decimal with denominator 2^a · 5^b terminates
	// at max(a, b) digits.
	prec := a
	if b > a {
		prec = b
	}
	s := rat.FloatString(prec)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s, nil
}

// finiteDecimalFactors reports whether d has no prime factors other than
// 2 and 5, returning their exponents. Every terminating decimal's
// reduced denominator satisfies this; any other denominator means the
// value is not a finite decimal.
func finiteDecimalFactors(d *big.Int) (twos, fives int, ok bool) {
	div := new(big.Int).Set(d)
	quot := new(big.Int)
	rem := new(big.Int)
	factors := []struct {
		prime *big.Int
		exp   *int
	}{
		{big.NewInt(2), &twos},
		{big.NewInt(5), &fives},
	}
	for _, f := range factors {
		for {
			quot.QuoRem(div, f.prime, rem)
			if rem.Sign() != 0 {
				break
			}
			div.Set(quot)
			*f.exp++
		}
	}
	return twos, fives, div.Cmp(big.NewInt(1)) == 0
}

// ResolveAppTool resolves an App-visible callback from the named server's App
// dispatch catalog — the ONLY authority a rendered App may invoke an
// App callback through. The server identity is the host-derived key:
// a callback name is NEVER resolvable here without its own server, and a
// name that lives on a DIFFERENT server answers not-found, so no string
// prefix or remembered global tool name can select another server's
// callback. Identity is mandatory (every other read path — AGENTS.md §6
// rule 9); the App dispatch surface has already reconciled a verified
// triple before this is reachable.
//
// The returned descriptor is the SAME wrapped descriptor the ordinary
// catalog would hold for a mixed-visibility tool, so invoking it re-enters the
// existing approval / OAuth / identity / current-state path — the App
// dispatch surface applies those gates exactly as a planner call does.
func (r *Registry) ResolveAppTool(serverID, toolName string) (tools.ToolDescriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[serverID]
	if !ok {
		return tools.ToolDescriptor{}, false
	}
	d, ok := e.appVisible[toolName]
	return d, ok
}

// ResolveAppToolForIdentity resolves an App-visible callback only when the
// named server is visible to the verified request identity. It is the
// identity-aware counterpart to ResolveAppTool, which remains for the
// operator/boot compatibility seam and has no identity context to evaluate.
func (r *Registry) ResolveAppToolForIdentity(ctx context.Context, serverID, toolName string) (tools.ToolDescriptor, bool, error) {
	if err := requireIdentity(ctx); err != nil {
		return tools.ToolDescriptor{}, false, err
	}
	r.mu.RLock()
	e, ok := r.servers[serverID]
	id, _ := identity.From(ctx)
	if !ok || !entryVisibleToIdentity(e, id) {
		r.mu.RUnlock()
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: %q", ErrServerNotFound, serverID)
	}
	d, found := e.appVisible[toolName]
	r.mu.RUnlock()
	return d, found, nil
}

// ResolveAppToolAtGeneration resolves an App-visible callback from the named
// server's App dispatch catalog ONLY when the server's CURRENT generation
// exactly equals expectedGeneration. The generation compare and the
// descriptor lookup happen under ONE registry read lock, so a
// refresh/replacement between an earlier CurrentGeneration read and this
// call cannot splice a descriptor from a newer generation — the exact
// compare+resolve the admission-aware AppsAccessor performs after
// verifying the call-local render-admission proof (HA-56 TOCTOU
// correction).
//
// Outcomes are typed:
//
//   - (descriptor, true, nil): the server's current generation equals
//     expectedGeneration and it holds an App-visible callback under
//     toolName — resolved under the exact expected generation.
//   - ("", false, error wrapping ErrGenerationMismatch): the server is
//     absent, has no established current generation, or its current
//     generation differs from expectedGeneration. A refusal — the
//     admission is stale — never a resolution of the new row. The
//     refusal's error text deliberately carries neither generation
//     digest: the accessor surfaces it verbatim on the wire (a scope
//     refusal), and a digest in the message would leak catalog state to
//     whoever probes the refusal (CLAUDE.md §7).
//   - ("", false, nil): the server's current generation equals
//     expectedGeneration, but the server does not hold an App-visible
//     callback under toolName (a plain not-found within the exact
//     generation).
func (r *Registry) ResolveAppToolAtGeneration(serverID, toolName, expectedGeneration string) (tools.ToolDescriptor, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[serverID]
	if !ok || e.currentGeneration == "" {
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: server %q has no current provider/catalog generation to compare",
			ErrGenerationMismatch, serverID)
	}
	if e.currentGeneration != expectedGeneration {
		// The typed mismatch (ErrGenerationMismatch) is the whole verdict.
		// Neither the current nor the expected generation digest is echoed
		// into the text — the accessor wraps this error verbatim into the
		// wire-facing CodeScopeMismatch message.
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: server %q current generation changed while resolving",
			ErrGenerationMismatch, serverID)
	}
	d, ok := e.appVisible[toolName]
	return d, ok, nil
}

// ResolveAppToolAtGenerationForIdentity is the identity-aware, atomic
// generation compare + App callback lookup. A foreign user-owned server is
// indistinguishable from an absent server to the caller.
func (r *Registry) ResolveAppToolAtGenerationForIdentity(ctx context.Context, serverID, toolName, expectedGeneration string) (tools.ToolDescriptor, bool, error) {
	if err := requireIdentity(ctx); err != nil {
		return tools.ToolDescriptor{}, false, err
	}
	r.mu.RLock()
	e, ok := r.servers[serverID]
	id, _ := identity.From(ctx)
	if !ok || !entryVisibleToIdentity(e, id) {
		r.mu.RUnlock()
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: %q", ErrServerNotFound, serverID)
	}
	if e.currentGeneration == "" {
		r.mu.RUnlock()
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: server %q has no current provider/catalog generation to compare", ErrGenerationMismatch, serverID)
	}
	if e.currentGeneration != expectedGeneration {
		r.mu.RUnlock()
		return tools.ToolDescriptor{}, false, fmt.Errorf("%w: server %q current generation changed while resolving", ErrGenerationMismatch, serverID)
	}
	d, found := e.appVisible[toolName]
	r.mu.RUnlock()
	return d, found, nil
}

// CurrentGeneration returns the deterministic current provider/catalog
// generation fingerprint for the named server: a content digest of the
// canonical CURRENT descriptor set (resources + App callbacks +
// ordinary catalog). It changes on detach (server absent → unknown),
// replacement, and every successful discovery change — even when the
// deployment registration descriptor did not change. It is stable across
// replicas with the same canonical current descriptor set (content-
// derived, never a process-local counter).
//
// Unknown/empty fails closed: the second result is false when the server
// is absent or no successful discovery has established its current
// descriptor set yet, and a render admission never binds an empty
// generation. This is the value a render-admission gate binds into the
// sealed tuple; a stale generation after refresh or replacement must
// execute zero callbacks.
func (r *Registry) CurrentGeneration(serverID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[serverID]
	if !ok || e.currentGeneration == "" {
		return "", false
	}
	return e.currentGeneration, true
}

// CurrentGenerationForIdentity returns the current descriptor generation only
// for a server visible to the verified request identity. It returns false for
// an absent or foreign user-owned source and therefore cannot mint render
// authority for another user's attachment.
func (r *Registry) CurrentGenerationForIdentity(ctx context.Context, serverID string) (string, bool, error) {
	if err := requireIdentity(ctx); err != nil {
		return "", false, err
	}
	id, _ := identity.From(ctx)
	r.mu.RLock()
	e, ok := r.servers[serverID]
	if !ok || !entryVisibleToIdentity(e, id) || e.currentGeneration == "" {
		r.mu.RUnlock()
		return "", false, nil
	}
	generation := e.currentGeneration
	r.mu.RUnlock()
	return generation, true, nil
}

// RegistrationSwap is a reversible live-registry publication. Commit makes
// the staged entry final and drains the exact displaced provider. Rollback
// restores the exact prior entry only while this staged entry is still current.
type RegistrationSwap struct {
	mu        sync.Mutex
	registry  *Registry
	name      string
	prior     *serverEntry
	staged    *serverEntry
	done      bool
	published bool
	// invalidated is set by exact teardown while the staged provider is still
	// private. It is atomic because teardown owns the registry lock while the
	// publication receipt owns mu; neither path may invert those locks.
	invalidated atomic.Bool
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

// StageRegistration privately reserves one registry entry and returns an
// exact publication/rollback receipt. The staged provider is deliberately not
// inserted into servers: direct registry reads must keep reaching the exact
// prior provider until the catalog's dispatch publication has succeeded.
func (r *Registry) StageRegistration(reg ServerRegistration, descs []tools.ToolDescriptor) (*RegistrationSwap, error) {
	entry, name, err := registrationEntry(reg, descs, r.clock())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if _, removing := r.removing[name]; removing {
		r.mu.Unlock()
		return nil, fmt.Errorf("mcp: registration for %q refused while exact teardown is admitted", name)
	}
	if err := ambiguousAgainst(r.servers, name); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err := ambiguousAgainst(r.closing, name); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if _, closing := r.closing[name]; closing {
		r.mu.Unlock()
		return nil, fmt.Errorf("mcp: registration for %q refused while its exact prior generation is closing", name)
	}
	for closingName := range r.genericClosing {
		if closingName == name {
			r.mu.Unlock()
			return nil, fmt.Errorf("mcp: registration for %q refused while its prior generation is closing", name)
		}
		if strings.HasPrefix(name, closingName+"_") || strings.HasPrefix(closingName, name+"_") {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: server id %q is separator-ambiguous with a closing server id", ErrAmbiguousServerID, name)
		}
	}
	for pendingName := range r.pending {
		if pendingName == name {
			r.mu.Unlock()
			return nil, fmt.Errorf("mcp: registration for %q is already staged", name)
		}
		if strings.HasPrefix(name, pendingName+"_") || strings.HasPrefix(pendingName, name+"_") {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: server id %q is separator-ambiguous with a staged server id", ErrAmbiguousServerID, name)
		}
	}
	prior := r.servers[name]
	if prior != nil && prior.owner != reg.Owner {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: a connection named %q is already registered to a different owner", ErrConnectionNameOwnerConflict, name)
	}
	swap := &RegistrationSwap{registry: r, name: name, prior: prior, staged: entry}
	r.pending[name] = swap
	r.mu.Unlock()
	return swap, nil
}

type exactRemovalReservation struct {
	owner                 auth.Owner
	descriptorFingerprint string
	holders               int
	sealed                bool
	invalidatedStage      *serverEntry
}

// ExactRemovalFence is a process-local admission receipt shared by signed
// publication and exact teardown. Seal records that desired pair absence has
// committed; Cancel is valid only while that durable transition is unproven.
type ExactRemovalFence struct {
	mu          sync.Mutex
	registry    *Registry
	name        string
	reservation *exactRemovalReservation
	done        bool
}

// BeginExactRemoval prevents the exact generation from becoming newly
// dispatchable until its durable removal either fails definitively or exact
// teardown completes. A publication already holding the registry lock wins
// first; otherwise a matching private stage is invalidated before this method
// returns. An absent generation is still fenced so a stale preparation cannot
// publish between admission and the desired-state CAS.
func (r *Registry) BeginExactRemoval(name string, owner auth.Owner, descriptorFingerprint string) (*ExactRemovalFence, error) {
	name = PhysicalServerName(name, owner)
	if name == "" || owner.IsZero() || descriptorFingerprint == "" {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reservation := r.removing[name]
	if reservation != nil {
		if reservation.owner != owner || reservation.descriptorFingerprint != descriptorFingerprint {
			return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		reservation.holders++
		return &ExactRemovalFence{registry: r, name: name, reservation: reservation}, nil
	}
	if e := r.closing[name]; e != nil && (e.owner != owner || e.descriptorFingerprint != descriptorFingerprint) {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if staged := r.pending[name]; staged != nil && (staged.staged.owner != owner || staged.staged.descriptorFingerprint != descriptorFingerprint) {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if e := r.servers[name]; e != nil && (e.owner != owner || e.descriptorFingerprint != descriptorFingerprint) {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	reservation = &exactRemovalReservation{owner: owner, descriptorFingerprint: descriptorFingerprint, holders: 1}
	if staged := r.pending[name]; staged != nil {
		delete(r.pending, name)
		staged.invalidated.Store(true)
		r.closing[name] = staged.staged
		reservation.invalidatedStage = staged.staged
	}
	r.removing[name] = reservation
	return &ExactRemovalFence{registry: r, name: name, reservation: reservation}, nil
}

// BeginExactPublisherRemoval is the durable-publisher variant used only after
// the shared operation record has entered removal_admitted. A runtime may hold
// an older same-owner publisher epoch while another runtime owns the durable
// current epoch. That stale handle is already bearer-inert, so it is not a
// local mismatch that may block durable teardown; a foreign owner still fails
// closed. A matching local generation retains the exact close behavior of
// [BeginExactRemoval].
func (r *Registry) BeginExactPublisherRemoval(name string, owner auth.Owner, descriptorFingerprint string) (*ExactRemovalFence, error) {
	fence, err := r.BeginExactRemoval(name, owner, descriptorFingerprint)
	if err == nil || !errors.Is(err, ErrServerNotFound) {
		return fence, err
	}
	name = PhysicalServerName(name, owner)
	if name == "" || owner.IsZero() || descriptorFingerprint == "" {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reservation := r.removing[name]; reservation != nil {
		if reservation.owner != owner || reservation.descriptorFingerprint != descriptorFingerprint {
			return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		reservation.holders++
		return &ExactRemovalFence{registry: r, name: name, reservation: reservation}, nil
	}
	if e := r.closing[name]; e != nil && e.owner != owner {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if staged := r.pending[name]; staged != nil && staged.staged.owner != owner {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if e := r.servers[name]; e != nil && e.owner != owner {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	reservation := &exactRemovalReservation{owner: owner, descriptorFingerprint: descriptorFingerprint, holders: 1}
	r.removing[name] = reservation
	return &ExactRemovalFence{registry: r, name: name, reservation: reservation}, nil
}

// Seal keeps the admission fence installed after desired pair absence commits.
// Exact teardown removes it only after the exact transport has closed.
func (f *ExactRemovalFence) Seal() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return
	}
	f.registry.mu.Lock()
	if f.registry.removing[f.name] == f.reservation {
		f.reservation.sealed = true
		f.reservation.holders--
	}
	f.registry.mu.Unlock()
	f.done = true
}

// Cancel releases an unsealed admission after the desired-state CAS is proven
// not to have committed. If admission invalidated a private stage, Cancel
// closes that exact never-dispatchable provider before releasing the name.
func (f *ExactRemovalFence) Cancel(ctx context.Context) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	f.registry.mu.Lock()
	reservation := f.registry.removing[f.name]
	if reservation != f.reservation {
		f.registry.mu.Unlock()
		f.done = true
		return nil
	}
	if reservation.sealed || reservation.holders > 1 {
		reservation.holders--
		f.registry.mu.Unlock()
		f.done = true
		return nil
	}
	invalidated := reservation.invalidatedStage
	if invalidated == nil {
		delete(f.registry.removing, f.name)
		reservation.holders = 0
		f.registry.mu.Unlock()
		f.done = true
		return nil
	}
	f.registry.mu.Unlock()
	if err := invalidated.provider.Close(ctx); err != nil {
		return fmt.Errorf("mcp: cancel exact removal %q: close invalidated transport: %w", f.name, err)
	}
	f.registry.mu.Lock()
	if f.registry.removing[f.name] == reservation && !reservation.sealed && reservation.holders == 1 {
		if f.registry.closing[f.name] == invalidated {
			delete(f.registry.closing, f.name)
		}
		delete(f.registry.removing, f.name)
		reservation.holders = 0
	}
	f.registry.mu.Unlock()
	f.done = true
	return nil
}

// Commit finalizes a staged registration without an external publication
// callback. Prepared MCP attachments use [RegistrationSwap.Publish] so the
// catalog dispatch swap and live-registry publication share one exact
// reservation linearization.
func (s *RegistrationSwap) Commit(ctx context.Context) error {
	_, err := s.Publish(ctx, nil)
	return err
}

// Publish atomically validates this exact private reservation, runs publish
// while the registry write lock excludes exact teardown, and installs the
// staged handle in the live registry. A teardown that wins before this method
// invalidates and closes the staged handle; Publish then fails with
// published=false. A teardown that starts after publish necessarily observes
// the live exact handle. publish must make the external dispatch state visible
// only after all durable authority checks have completed.
//
// The returned boolean distinguishes a cleanup error after irreversible
// publication from a pre-publication refusal. Callers log the former and must
// close/rollback the latter.
func (s *RegistrationSwap) Publish(ctx context.Context, publish func() error) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		if s.published {
			return true, nil
		}
		return false, fmt.Errorf("mcp: registration publication refused for %q: private stage was invalidated", s.name)
	}
	s.registry.mu.Lock()
	if s.registry.pending[s.name] != s {
		s.registry.mu.Unlock()
		return false, fmt.Errorf("mcp: registration publication refused for %q: private stage is no longer current", s.name)
	}
	if s.registry.servers[s.name] != s.prior {
		s.registry.mu.Unlock()
		return false, fmt.Errorf("mcp: registration publication refused for %q: prior entry changed while staged", s.name)
	}
	if s.invalidated.Load() {
		s.registry.mu.Unlock()
		return false, fmt.Errorf("mcp: registration publication refused for %q: private stage was invalidated", s.name)
	}
	if publish != nil {
		if err := publish(); err != nil {
			s.registry.mu.Unlock()
			return false, err
		}
	}
	s.registry.servers[s.name] = s.staged
	delete(s.registry.pending, s.name)
	s.registry.mu.Unlock()
	s.done = true
	s.published = true
	if s.prior != nil && s.prior.provider != nil && s.prior.provider != s.staged.provider {
		if err := s.prior.provider.Close(ctx); err != nil {
			return true, fmt.Errorf("close replaced transport %q: %w", s.name, err)
		}
	}
	return true, nil
}

// Rollback drops the private reservation iff it is still current. Ordinary
// reads never stopped seeing the prior entry, and the staged provider is closed
// later through PreparedAttachment.Close.
func (s *RegistrationSwap) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	if s.invalidated.Load() {
		s.done = true
		return nil
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if s.registry.pending[s.name] != s {
		return fmt.Errorf("mcp: registration rollback refused for %q: private stage is no longer current", s.name)
	}
	delete(s.registry.pending, s.name)
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
// concurrent reads). If Close fails, the exact withdrawn handle and its name
// reservation remain private: a matching-owner retry closes that same handle,
// another owner observes ErrServerNotFound, and no replacement may publish
// until Close returns success. An unknown name with no close debt returns
// ErrServerNotFound.
type genericCloseReceipt struct {
	mu     sync.Mutex
	entry  *serverEntry
	closed bool
}

func (r *Registry) Deregister(ctx context.Context, name string, owner auth.Owner) error {
	name = PhysicalServerName(name, owner)
	r.mu.Lock()
	if _, staged := r.pending[name]; staged {
		r.mu.Unlock()
		return fmt.Errorf("mcp: deregister %q refused while an exact replacement is staged", name)
	}
	receipt := r.genericClosing[name]
	if receipt != nil {
		if receipt.entry.owner != owner {
			r.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
	} else {
		e, ok := r.servers[name]
		if !ok || e.owner != owner {
			r.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		delete(r.servers, name)
		receipt = &genericCloseReceipt{entry: e}
		r.genericClosing[name] = receipt
	}
	r.mu.Unlock()

	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	if receipt.closed {
		return nil
	}
	if err := receipt.entry.provider.Close(ctx); err != nil {
		return fmt.Errorf("mcp: deregister %q: close transport: %w", name, err)
	}
	receipt.closed = true
	r.mu.Lock()
	if r.genericClosing[name] == receipt {
		delete(r.genericClosing, name)
	}
	r.mu.Unlock()
	return nil
}

// DeregisterExact withdraws one live registration from dispatch only after
// atomically proving its owner and complete descriptor fingerprint. The exact
// provider handle then remains in a private retryable closing state until
// Close returns success. withdrawCatalog runs while the registry write lock
// holds the name against replacement, so catalog withdrawal can never race a
// same-name registration from another owner. A retry addresses the same
// closing generation and never converts an absent-after-error observation into
// a teardown receipt.
func (r *Registry) DeregisterExact(ctx context.Context, name string, owner auth.Owner, descriptorFingerprint string, withdrawCatalog func() int) (int, error) {
	name = PhysicalServerName(name, owner)
	if owner.IsZero() || descriptorFingerprint == "" || withdrawCatalog == nil {
		return 0, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	r.mu.Lock()
	removal := r.removing[name]
	if removal != nil {
		if removal.owner != owner || removal.descriptorFingerprint != descriptorFingerprint {
			r.mu.Unlock()
			return 0, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		if !removal.sealed {
			r.mu.Unlock()
			return 0, fmt.Errorf("mcp: exact teardown for %q is admitted but desired removal is not sealed", name)
		}
	}
	e, retryingClose := r.closing[name]
	removed := 0
	if retryingClose {
		if e.owner != owner || e.descriptorFingerprint != descriptorFingerprint {
			r.mu.Unlock()
			return 0, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
	} else if staged := r.pending[name]; staged != nil {
		e = staged.staged
		if e.owner != owner || e.descriptorFingerprint != descriptorFingerprint {
			r.mu.Unlock()
			return 0, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		// The staged provider has never entered catalog dispatch. Exact removal
		// invalidates its reservation and retains the same handle in closing so
		// a close failure is retryable and replacement remains blocked.
		delete(r.pending, name)
		staged.invalidated.Store(true)
		r.closing[name] = e
	} else {
		var ok bool
		e, ok = r.servers[name]
		if !ok && removal != nil {
			delete(r.removing, name)
			r.mu.Unlock()
			return 0, nil
		}
		if !ok || e.owner != owner || e.descriptorFingerprint != descriptorFingerprint {
			r.mu.Unlock()
			return 0, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
		removed = withdrawCatalog()
		delete(r.servers, name)
		r.closing[name] = e
	}
	r.mu.Unlock()
	if err := e.provider.Close(ctx); err != nil {
		return removed, fmt.Errorf("mcp: deregister %q: close transport: %w", name, err)
	}
	r.mu.Lock()
	if r.closing[name] == e {
		delete(r.closing, name)
	}
	if r.removing[name] == removal {
		delete(r.removing, name)
	}
	r.mu.Unlock()
	return removed, nil
}

// DeregisterExactPublisher closes a matching current publisher generation. If
// this process has only an older same-owner epoch, it leaves that already-inert
// handle untouched and returns success: the durable removal phase, not local
// absence, is the security revocation receipt. Foreign-owner state still fails
// closed and a matching generation retains retryable exact-close semantics.
func (r *Registry) DeregisterExactPublisher(ctx context.Context, name string, owner auth.Owner, descriptorFingerprint string, withdrawCatalog func() int) (int, error) {
	removed, err := r.DeregisterExact(ctx, name, owner, descriptorFingerprint, withdrawCatalog)
	if err == nil || !errors.Is(err, ErrServerNotFound) {
		return removed, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name = PhysicalServerName(name, owner)
	reservation := r.removing[name]
	if reservation == nil || reservation.owner != owner || reservation.descriptorFingerprint != descriptorFingerprint || !reservation.sealed {
		return 0, err
	}
	if e := r.closing[name]; e != nil && e.owner != owner {
		return 0, err
	}
	if staged := r.pending[name]; staged != nil && staged.staged.owner != owner {
		return 0, err
	}
	if e := r.servers[name]; e != nil && e.owner != owner {
		return 0, err
	}
	delete(r.removing, name)
	return 0, nil
}

// reservationState is a test/diagnostic snapshot of one exact name. It is
// intentionally package-private: authority remains the registry mutation
// methods, not a caller-observed state classification.
func (r *Registry) reservationState(name string) (pending, live, closing bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, pending = r.pending[name]
	_, live = r.servers[name]
	_, closing = r.closing[name]
	return pending, live, closing
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

// OwnerOfSource is the source-oriented spelling of OwnerOf used by generic
// run projection seams. It returns the immutable registration owner for a
// physical source without exposing any mutable provider state.
func (r *Registry) OwnerOfSource(source tools.ToolSourceID) (auth.Owner, bool) {
	return r.OwnerOf(string(source))
}

// SourceAccess reports whether a physical source is registered and whether the
// verified caller may reach it. Unknown non-MCP sources are represented by
// registered=false so generic catalog callers can preserve their existing
// behavior. A foreign user-owned source is registered=true but returns
// ErrServerNotFound, without returning its owner or logical name.
func (r *Registry) SourceAccess(ctx context.Context, source tools.ToolSourceID) (owner auth.Owner, logical string, registered bool, err error) {
	if err := requireIdentity(ctx); err != nil {
		return auth.Owner{}, "", false, err
	}
	id, _ := identity.From(ctx)
	r.mu.RLock()
	e, ok := r.servers[string(source)]
	if !ok {
		r.mu.RUnlock()
		return auth.Owner{}, "", false, nil
	}
	if !entryVisibleToIdentity(e, id) {
		r.mu.RUnlock()
		return auth.Owner{}, "", true, fmt.Errorf("%w: %q", ErrServerNotFound, source)
	}
	owner = e.owner
	logical = e.logicalName
	if logical == "" {
		logical = string(source)
	}
	r.mu.RUnlock()
	return owner, logical, true, nil
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
			logical := e.logicalName
			if logical == "" {
				logical = name
			}
			out = append(out, logical)
		}
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// LogicalNameOfSource returns the signed/config descriptor name associated
// with one physical source id. It is used only by an identity-scoped
// projection to translate the private local key back to desired-state names.
func (r *Registry) LogicalNameOfSource(source tools.ToolSourceID) (string, bool) {
	r.mu.RLock()
	e, ok := r.servers[string(source)]
	if !ok {
		e, ok = r.closing[string(source)]
	}
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	logical := e.logicalName
	if logical == "" {
		logical = string(source)
	}
	return logical, true
}

// RegistrationIdentityForOwner returns the live registration identity for a
// logical connection under owner. User-owned names resolve through the
// server-derived physical namespace; operator/boot names remain unchanged.
func (r *Registry) RegistrationIdentityForOwner(name string, owner auth.Owner) (auth.Owner, string, bool) {
	return r.RegistrationIdentity(PhysicalServerName(name, owner))
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
	e, eErr := r.identityVisibleEntry(ctx, name)
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

// entryVisibleToIdentity is the registry's read/dispatch visibility rule.
// Boot-declared and agent-scoped registrations remain deployment/tenant-wide,
// while a user-scoped registration is visible only to the exact verified
// tenant and user that owns it. Session is deliberately not part of this
// durable attachment boundary: a later session for the same user can use its
// own attachment. The caller holds r.mu when reading entry fields.
func entryVisibleToIdentity(e *serverEntry, id identity.Identity) bool {
	if e == nil || e.owner.User == "" {
		return e != nil
	}
	return e.owner.Tenant == id.TenantID && e.owner.User == id.UserID
}

// identityVisibleEntry resolves a server through the identity-scoped read
// boundary. Foreign user-owned registrations answer ErrServerNotFound just as
// an absent registration does, so the read surface cannot be used as an
// existence oracle. Caller must have already supplied a complete identity.
func (r *Registry) identityVisibleEntry(ctx context.Context, name string) (*serverEntry, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return nil, ErrRegistryIdentityMissing
	}
	r.mu.RLock()
	e, found := r.servers[name]
	visible := found && entryVisibleToIdentity(e, id)
	r.mu.RUnlock()
	if !visible {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	return e, nil
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
// Identity is mandatory. The process-local owner filter protects cache
// privacy; callers that have a durable effective-source authority should use
// ListServersWithVisibility so filtering happens before pagination.
func (r *Registry) ListServers(ctx context.Context, f ListFilter) ([]ServerView, *Cursor, error) {
	return r.listServers(ctx, f, nil)
}

// ListServersWithVisibility applies an identity-aware source visibility
// predicate before filters and pagination. The registry remains a cache: the
// callback is the caller's durable policy projection, and its false result is
// indistinguishable from an absent server.
func (r *Registry) ListServersWithVisibility(ctx context.Context, f ListFilter, visible SourceVisibility) ([]ServerView, *Cursor, error) {
	return r.listServers(ctx, f, visible)
}

func (r *Registry) listServers(ctx context.Context, f ListFilter, visible SourceVisibility) ([]ServerView, *Cursor, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("mcp: ListServers cancelled: %w", err)
	}

	id, _ := identity.From(ctx)
	type snapshot struct {
		view    ServerView
		source  tools.ToolSourceID
		owner   auth.Owner
		logical string
	}
	r.mu.RLock()
	all := make([]snapshot, 0, len(r.servers))
	for _, e := range r.servers {
		if !entryVisibleToIdentity(e, id) {
			continue
		}
		logical := e.logicalName
		if logical == "" {
			logical = string(e.provider.SourceID())
		}
		all = append(all, snapshot{view: e.viewLocked(), source: e.provider.SourceID(), owner: e.owner, logical: logical})
	}
	r.mu.RUnlock()
	if visible != nil {
		admitted := all[:0]
		for _, row := range all {
			ok, err := visible(ctx, row.source, row.owner, row.logical)
			if err != nil {
				return nil, nil, fmt.Errorf("mcp: source visibility for %q: %w", row.source, err)
			}
			if ok {
				admitted = append(admitted, row)
			}
		}
		all = admitted
	}

	views := make([]ServerView, 0, len(all))
	for _, row := range all {
		views = append(views, row.view)
	}

	// Deterministic order — sort by name so the cursor is stable.
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	filtered := views[:0:0]
	for _, v := range views {
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

// GetServer returns the per-server detail view. Identity is mandatory. The
// process-local owner filter protects cache privacy; callers that have a
// durable effective-source authority should use GetServerWithVisibility.
func (r *Registry) GetServer(ctx context.Context, name string) (*ServerView, error) {
	return r.getServer(ctx, name, nil)
}

// GetServerWithVisibility applies an identity-aware source visibility
// predicate to the exact current server detail. A false result is projected
// as ErrServerNotFound so an unselected personal source cannot become an
// existence oracle.
func (r *Registry) GetServerWithVisibility(ctx context.Context, name string, visible SourceVisibility) (*ServerView, error) {
	return r.getServer(ctx, name, visible)
}

func (r *Registry) getServer(ctx context.Context, name string, visible SourceVisibility) (*ServerView, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.identityVisibleEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	v := e.viewLocked()
	owner := e.owner
	logical := e.logicalName
	if logical == "" {
		logical = string(e.provider.SourceID())
	}
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
	if visible != nil {
		ok, err := visible(ctx, e.provider.SourceID(), owner, logical)
		if err != nil {
			return nil, fmt.Errorf("mcp: source visibility for %q: %w", name, err)
		}
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrServerNotFound, name)
		}
	}
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

// OAuthDiscoveryTargetForIdentity is the identity-aware OAuth discovery read.
// It preserves the legacy no-context helper for operator-only internals while
// preventing a user request from probing another user's connection metadata.
func (r *Registry) OAuthDiscoveryTargetForIdentity(ctx context.Context, name string) (challenge *AuthChallenge, serverURL string, allowedOrigins []string, err error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, "", nil, err
	}
	e, err := r.identityVisibleEntry(ctx, name)
	if err != nil {
		return nil, "", nil, err
	}
	r.mu.RLock()
	if e.stats.oauthChallenge != nil {
		ch := *e.stats.oauthChallenge
		challenge = &ch
	}
	serverURL = e.urlOrCommand
	allowedOrigins = append([]string(nil), e.oauthAllowedOrigins...)
	r.mu.RUnlock()
	return challenge, serverURL, allowedOrigins, nil
}

// ListResources returns the advertised resources for a server. It runs a
// Discover and projects the synthetic resource descriptors. Identity is
// mandatory.
func (r *Registry) ListResources(ctx context.Context, name string) ([]ResourceView, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.identityVisibleEntry(ctx, name)
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
	e, err := r.identityVisibleEntry(ctx, name)
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
//
// # The App dispatch view rebuilds with the snapshot
//
// The fresh descriptor set rebuilds BOTH projections under the registry write
// lock: the ordinary planner/model catalog and the server's App dispatch
// catalog (entry.appVisible). A refresh therefore picks up and removes App
// callbacks in either visibility direction without leaving a stale projection
// behind.
func (r *Registry) RefreshDiscovery(ctx context.Context, name string) (*DiscoveryResult, error) {
	if err := requireIdentity(ctx); err != nil {
		return nil, err
	}
	e, err := r.identityVisibleEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	start := r.clock()
	descs, derr := e.provider.Discover(ctx)
	latency := r.clock().Sub(start).Milliseconds()
	r.mu.Lock()
	if r.servers[name] != e {
		r.mu.Unlock()
		return nil, &DiscoveryStaleError{Retry: true}
	}
	if derr != nil {
		e.stats.errorRatePerMin++
		e.stats.state = ServerStateError
		r.mu.Unlock()
		return nil, fmt.Errorf("mcp: RefreshDiscovery %q: %w", name, derr)
	}
	// Reapply the registration policy to every fresh snapshot before either
	// catalog view is derived. A refresh must never resurrect a denied tool.
	descs = filterDiscoveredTools(descs, name, e.toolAllowlist, e.toolDenylist)
	tc, rc, pc := classifyDescriptors(descs, name)

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
	if e.catalog != nil {
		catalogSwap, cerr := e.catalog.StageSource(tools.ToolSourceID(name), ordinaryDescriptors(descs), true)
		if cerr != nil {
			r.mu.Unlock()
			return nil, fmt.Errorf("mcp: refresh catalog %q: %w", name, cerr)
		}
		catalogSwap.Commit()
	}
	// Rebuild the App dispatch view from the SAME fresh snapshot that just
	// re-derived the counts — one discovered set, two views. The
	// deterministic current provider/catalog generation recomputes with
	// the snapshot, so a refresh that changes resources / App callbacks /
	// ordinary catalog rows bumps the generation even when the deployment
	// registration descriptor did not change.
	e.appVisible = partitionAppVisible(descs)
	e.currentGeneration = currentGenerationFor(descs)
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
	e, err := r.identityVisibleEntry(ctx, name)
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
	e, err := r.identityVisibleEntry(ctx, name)
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
	name = PhysicalServerName(name, owner)
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
	// The boot-time descriptor snapshot also establishes BOTH projections
	// from the SAME set: the App dispatch catalog (entry.appVisible) and the
	// deterministic current provider/catalog generation. RecordDiscovery is
	// the no-network counterpart to RefreshDiscovery, so it must seed the
	// same two views the refresh path maintains together, under one write
	// lock — a subsequent generation-bound App callback resolution
	// (ResolveAppToolAtGeneration) must see the refreshed partition under
	// the refreshed generation, never stale descriptors from the
	// pre-discovery stage.
	e.appVisible = partitionAppVisible(descs)
	e.currentGeneration = currentGenerationFor(descs)
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
