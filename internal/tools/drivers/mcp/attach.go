// attach.go — the exported boot-time MCP server attachment helper
// (absorbs cmd/harbor's attachDevMCPServer from
// INCLUDING the config→ToolPolicy
// projection that the devstack mirror had silently dropped).
//
// Attach wires ONE configured MCP server into a running stack: it
// projects the operator-facing policy YAML onto the driver's runtime
// ToolPolicy fields, spawns the transport, opens the MCP session,
// discovers tools, registers each ToolDescriptor on the tool catalog,
// surfaces the live Provider on the Registry (with its configured
// per-server policy + seeded discovery stats), and threads the
// Provider's Close into the caller's closer chain so stack teardown
// drains the subprocess. Fail-loud on every step: a misconfigured /
// unreachable MCP server must not boot silently (CLAUDE.md §13).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// AttachDeps bundles the collaborators Attach wires the server into.
// Catalog, Registry, Closers, and Bus are mandatory — a nil Bus fails
// loud at mcp.New (Config.validate rejects it; the driver publishes
// mcp.resource_updated). Only Logger is optional (a nil Logger silences
// the attachment log line — test stacks omit it).
type AttachDeps struct {
	// Catalog receives one ToolDescriptor per discovered tool.
	Catalog tools.ToolCatalog
	// Registry receives the live Provider registration so observability
	// surfaces (the Console MCP Connections page) can project it.
	Registry *Registry
	// Bus carries the driver's mcp.* events. Mandatory — the driver's
	// own constructor validates it (mcp.resource_updated emission).
	Bus events.EventBus
	// Logger receives the per-server attachment Info line. Optional.
	Logger *slog.Logger
	// DefaultIdentity is the FALLBACK identity stamped on server-pushed
	// events that arrive without an inflight call (transport-side
	// notifications — Item 1). Per-call subscriptions
	// stamp the inflight caller's ctx-resident identity via the
	// driver's pushIdentity helper; this default only covers
	// transport-level events.
	DefaultIdentity identity.Identity
	// Closers is the caller's ordered closer chain. Attach appends the
	// Provider's Close immediately after a successful Connect so a
	// later Discover/Register failure still drains the live subprocess.
	Closers *[]func(context.Context) error
	// HostDisplayModes lists the MCP App (`io.modelcontextprotocol/ui`)
	// display modes the host can render. Projected onto the Provider's
	// Config.HostDisplayModes so the provider advertises the UI extension
	// during the initialize handshake. The boot loader sources this once
	// from the deployment-level `tools.mcp_app_host.display_modes` config
	// (defaulting to inline); empty leaves the SDK's default advertisement
	// untouched. This is the programmatic seam an embedder sets without YAML.
	HostDisplayModes []string
	// ToolContext is the optional MCP Apps tool-context capturer. When set,
	// the Provider persists the input + lowered result behind a declared
	// `ui://` app so the host can deliver it to the rendered app. A nil
	// capturer leaves tool-context delivery unwired (the host read returns
	// not-found). Optional.
	ToolContext ToolContextCapturer
	// Owner is the (tenant, agent) reconcile-view tag stamped on the
	// registry entry for a RUNTIME-ADDED connection. The boot loader leaves it
	// zero (boot-declared servers are untagged and never reconciled); the
	// runtime-add attach path sets a non-zero owner so the run-start reconcile
	// view scopes to it. It is a reconcile-view filter, never a dispatch or
	// isolation key.
	Owner auth.Owner
	// DescriptorFingerprint is the canonical digest of the NON-SECRET
	// runtime-added descriptor. It is retained on the live registration so
	// run-start reconciliation can distinguish an exact no-op from a same-name
	// descriptor replacement. Boot-declared attachments leave it empty.
	DescriptorFingerprint string
	// OAuthProviders is the declared OAuth-provider registry (keyed by the
	// non-secret provider NAME) Attach resolves a connection's
	// `oauth_provider` binding against. Populated by the runtime assembler
	// from its constructed provider map (and the devstack twin). A binding
	// naming a provider absent from this map fails the attach loud, listing
	// the registered names (§4.4 factory-error convention). Nil / empty is
	// valid when no connection binds a provider. The driver depends ONLY on
	// the `auth.OAuthProvider` interface — no concrete driver import (§13).
	OAuthProviders map[string]auth.OAuthProvider
	// OAuthProviderSet is the runtime provider SET a RUNTIME-ADDED connection's
	// `oauth_provider` binding resolves against, so a Protocol-installed
	// (owner-tagged) provider is bindable in addition to the boot map. When set
	// it TAKES PRECEDENCE over OAuthProviders (the set is seeded from the same
	// boot map at assembly, so boot providers stay resolvable). Optional — nil
	// leaves resolution on the OAuthProviders map (the boot catalog path). The
	// driver depends only on the narrow resolver interface (bare-name Get +
	// Names for the fail-loud message) — no concrete import.
	OAuthProviderSet OAuthProviderResolver
	// OAuthProviderOverride is a privately prepared provider used for this
	// attachment's named binding before it is published to the shared set.
	OAuthProviderOverride auth.OAuthProvider
	// OwnOAuthProvider transfers teardown ownership of the override to the MCP
	// provider. General provider-set bindings leave this false.
	OwnOAuthProvider bool
	// ToolAllowlist/ToolDenylist project a signed restrictive policy onto the
	// discovered tool descriptors before catalog publication.
	ToolAllowlist []string
	ToolDenylist  []string
	// ArtifactEgressMaxBytes bounds ONE substituted artifact value on one
	// outbound call for connections this attach wires. Sourced by the boot
	// loader (and the runtime attacher) from the deployment-level
	// `tools.mcp_artifact_egress_max_bytes`, which carries a documented
	// default. Zero resolves to config.DefaultMCPArtifactEgressMaxBytes so
	// an embedder that does not set it still gets a real ceiling rather
	// than an unbounded one. Optional.
	ArtifactEgressMaxBytes int
}

// ErrPreparationAuthRequired marks a private MCP prepare that observed a
// structured HTTP authentication challenge before discovery could complete.
var ErrPreparationAuthRequired = errors.New("mcp: preparation requires authorization")

// PreparationAuthRequiredError carries the parsed, defensive challenge without
// exposing it through Error text or relying on transport error strings.
type PreparationAuthRequiredError struct{ Challenge AuthChallenge }

func (e *PreparationAuthRequiredError) Error() string { return ErrPreparationAuthRequired.Error() }

func (e *PreparationAuthRequiredError) Is(target error) bool {
	return target == ErrPreparationAuthRequired
}

type preparationObservations struct {
	mu        sync.Mutex
	challenge *AuthChallenge
	shortfall *ScopeShortfall
	sink      *RegistrationSwap
}

func (o *preparationObservations) recordChallenge(ch AuthChallenge) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sink != nil {
		o.sink.RecordAuthChallenge(ch)
		return
	}
	captured := ch
	o.challenge = &captured
}

func (o *preparationObservations) recordShortfall(sf ScopeShortfall) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sink != nil {
		o.sink.RecordScopeShortfall(sf)
		return
	}
	captured := sf
	captured.RequiredScopes = append([]string(nil), sf.RequiredScopes...)
	captured.GrantedScopes = append([]string(nil), sf.GrantedScopes...)
	o.shortfall = &captured
}

func (o *preparationObservations) authRequired() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.challenge == nil {
		return nil
	}
	return &PreparationAuthRequiredError{Challenge: defensiveAuthChallenge(*o.challenge)}
}

func defensiveAuthChallenge(ch AuthChallenge) AuthChallenge {
	// Raw is deliberately omitted: a hostile downstream controls the complete
	// header and may place credential-like bytes in extensions. Parsed fields
	// are the bounded, protocol-relevant challenge surfaced to callers.
	return AuthChallenge{
		Scheme: ch.Scheme, ResourceMetadataURL: ch.ResourceMetadataURL,
		Realm: ch.Realm, Error: ch.Error, Scope: ch.Scope, CapturedAt: ch.CapturedAt,
	}
}

func (o *preparationObservations) transfer(sink *RegistrationSwap) {
	o.mu.Lock()
	o.sink = sink
	challenge, shortfall := o.challenge, o.shortfall
	o.challenge, o.shortfall = nil, nil
	o.mu.Unlock()
	if challenge != nil {
		sink.RecordAuthChallenge(*challenge)
	}
	if shortfall != nil {
		sink.RecordScopeShortfall(*shortfall)
	}
}

// OAuthProviderResolver is the narrow bare-name resolution seam Attach uses to
// resolve a connection's `oauth_provider` binding — satisfied by
// `auth.ProviderSet` (the runtime provider set) and by the boot map adapter.
// Bare-name resolution across every session; Names feeds the fail-loud "registered: …"
// message.
type OAuthProviderResolver interface {
	// Get resolves a provider by bare name; the bool reports presence.
	Get(name string) (auth.OAuthProvider, bool)
	// Names returns every resolvable provider name, sorted, for a fail-loud
	// error message.
	Names() []string
}

// mapProviderResolver adapts a boot provider map to the OAuthProviderResolver
// seam so the boot path and the runtime-set path share one resolveOAuthBinding.
type mapProviderResolver map[string]auth.OAuthProvider

func (m mapProviderResolver) Get(name string) (auth.OAuthProvider, bool) {
	p, ok := m[name]
	return p, ok
}

func (m mapProviderResolver) Names() []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type overrideProviderResolver struct {
	base     OAuthProviderResolver
	name     string
	provider auth.OAuthProvider
}

func (r overrideProviderResolver) Get(name string) (auth.OAuthProvider, bool) {
	if name == r.name && r.provider != nil {
		return r.provider, true
	}
	return r.base.Get(name)
}

func (r overrideProviderResolver) Names() []string {
	names := r.base.Names()
	for _, name := range names {
		if name == r.name {
			return names
		}
	}
	return append(names, r.name)
}

// PreparedAttachment is a connected and discovered MCP provider that has not
// yet been published to the tool catalog or live registry. Activate publishes
// it once; Close drains it on refusal or shutdown. Its state is internally
// synchronized so a cancellation/cleanup race cannot publish after close.
type PreparedAttachment struct {
	mu            sync.Mutex
	ms            config.MCPServerConfig
	deps          AttachDeps
	mode          MCPTransportMode
	defaultPolicy tools.ToolPolicy
	provider      *Provider
	closeFn       func(context.Context) error
	descriptors   []tools.ToolDescriptor
	observations  *preparationObservations
	registrySwap  *RegistrationSwap
	activated     bool
	closed        bool
}

// Attach preserves the boot-time one-shot API by preparing and immediately
// activating. Runtime control-plane callers use Prepare directly so durable
// desired state can be written between those stages.
func Attach(ctx context.Context, ms config.MCPServerConfig, deps AttachDeps) error {
	prepared, err := Prepare(ctx, ms, deps)
	if err != nil {
		return err
	}
	if err := prepared.Activate(ctx); err != nil {
		*deps.Closers = append(*deps.Closers, prepared.Close)
		return closePreparedAfterFailure(ctx, prepared, err)
	}
	*deps.Closers = append(*deps.Closers, prepared.Close)
	return nil
}

// Prepare validates, connects, and discovers one MCP server without changing
// the shared catalog or registry. The returned attachment owns the connected
// provider until Activate or Close.
func Prepare(ctx context.Context, ms config.MCPServerConfig, deps AttachDeps) (*PreparedAttachment, error) {
	if deps.Catalog == nil {
		return nil, fmt.Errorf("mcp attach: Catalog is required")
	}
	if deps.Registry == nil {
		return nil, fmt.Errorf("mcp attach: Registry is required")
	}
	if deps.Closers == nil {
		return nil, fmt.Errorf("mcp attach: Closers chain is required (the Provider's subprocess must drain on teardown)")
	}
	// Separator safety, checked BEFORE any side effect (no transport spawned,
	// no catalog rows written), so an ambiguous id is refused cleanly rather
	// than after a subprocess is live and tools are half-registered. The
	// Registry re-checks under its own write lock — that is the structural
	// gate, since it is the single choke point every attach path funnels
	// through; this is the early, clean-failure copy. Both the boot-declared
	// and the runtime-attach path reach here.
	if err := deps.Registry.CheckServerIDUnambiguous(ms.Name); err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, err)
	}
	if priorOwner, exists := deps.Registry.OwnerOf(ms.Name); exists && priorOwner != deps.Owner {
		return nil, fmt.Errorf("%w: a connection named %q is already registered to a different owner", ErrConnectionNameOwnerConflict, ms.Name)
	}
	mode := MCPTransportMode(ms.TransportMode)
	if mode == "" {
		mode = TransportAuto
	}
	// project the operator-facing policy YAML onto the
	// driver's runtime ToolPolicy fields. A nil ms.Policy leaves
	// DefaultPolicy zero-valued, so every tool inherits
	// tools.DefaultPolicy() at dispatch. A projection error (e.g. an
	// unknown retry_on class that slipped past validation) fails the
	// boot loud (CLAUDE.md §5).
	defaultPolicy, toolPolicies, policyErr := ProjectToolPolicies(ms)
	if policyErr != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, policyErr)
	}
	// Resolve the non-secret `oauth_provider` binding (per-identity southbound
	// bearer) against the declared registry, and re-enforce the binding rules
	// at attach time — the primary gate for a runtime-added connection, which
	// never passes through `harbor validate` (config-time validation is the
	// boot gate). An unknown name / stdio binding / static-Authorization
	// conflict / reserved annotation key fails the attach loud (§13), never a
	// silent unauthenticated attach.
	// Prefer the runtime provider SET (owner-tagged installs + boot seed) when
	// wired; fall back to the boot map. The set is seeded from the same boot map
	// at assembly, so a boot provider stays resolvable either way.
	resolver := deps.OAuthProviderSet
	if resolver == nil {
		resolver = mapProviderResolver(deps.OAuthProviders)
	}
	overrideProviderName := privateOverrideProviderName(ms)
	if deps.OAuthProviderOverride != nil && overrideProviderName != "" {
		resolver = overrideProviderResolver{base: resolver, name: overrideProviderName, provider: deps.OAuthProviderOverride}
	}
	oauthProvider, bindErr := resolveOAuthBinding(ms, mode, resolver)
	if bindErr != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, bindErr)
	}
	// Resolve the per-tool oauth_provider overrides (CallTool granularity),
	// re-enforcing every binding rule per entry against the same resolver.
	toolProviders, toolBindErr := resolveToolOAuthBindings(ms, mode, resolver)
	if toolBindErr != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, toolBindErr)
	}
	// Resolve the per-user credential-injection binding (receiver-style server),
	// re-enforcing one-auth-mode + the downstream-sink allow-list at attach time
	// for the runtime-add path exactly like the bearer binding.
	injection, injErr := resolveInjectionBinding(ms, mode, resolver)
	if injErr != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, injErr)
	}
	// Resolve the egress-substitution declaration (byte-eligibility + the
	// per-tool artifact-parameter mapping), re-enforcing every rule the
	// boot validator enforces so the runtime-add path — which never
	// passes through `harbor validate` — is held to the same bar. The
	// mapped parameters are additionally checked against the server's OWN
	// discovered inputSchema at Discover, which is the half only the live
	// session can perform.
	egressMapping, egressErr := resolveArtifactEgress(ms, mode)
	if egressErr != nil {
		return nil, fmt.Errorf("mcp server %q: %w", ms.Name, egressErr)
	}
	egressMaxBytes := deps.ArtifactEgressMaxBytes
	if egressMaxBytes <= 0 {
		egressMaxBytes = config.DefaultMCPArtifactEgressMaxBytes
	}
	observations := &preparationObservations{}
	provider, err := New(Config{
		Name:                   ms.Name,
		TransportMode:          mode,
		URL:                    ms.URL,
		Command:                append([]string(nil), ms.Command...),
		Headers:                cloneHeaderMap(ms.Headers),
		KeepAlive:              ms.KeepAlive,
		Logger:                 deps.Logger,
		Bus:                    deps.Bus,
		DefaultPolicy:          defaultPolicy,
		ToolPolicies:           toolPolicies,
		DefaultIdentity:        deps.DefaultIdentity,
		HostDisplayModes:       append([]string(nil), deps.HostDisplayModes...),
		ToolContext:            deps.ToolContext,
		OAuthProvider:          oauthProvider,
		OwnOAuthProvider:       deps.OAuthProviderOverride != nil && ms.OAuthProvider != "" && deps.OwnOAuthProvider,
		OwnedInjectionProvider: ownedInjectionProvider(ms, deps),
		ToolOAuthProviders:     toolProviders,
		Injection:              injection,
		MetaAnnotations:        cloneHeaderMap(ms.MetaAnnotations),
		// Egress substitution: the compiled, immutable per-tool mapping and
		// the operator's ceiling. Empty leaves every outbound call
		// byte-identical to a build without the feature.
		ArtifactEgress:         egressMapping,
		ArtifactEgressMaxBytes: egressMaxBytes,
		// Record any `WWW-Authenticate` OAuth step-up challenge on the
		// registry state so an operator can inspect the advertised requirement
		// Best-effort observability — never alters the call.
		OnAuthChallenge: func(ch AuthChallenge) {
			observations.recordChallenge(ch)
		},
		// Record any downstream insufficient-scope step-up on the registry
		// state (mirrors OnAuthChallenge for the 403 path). Best-effort
		// observability — never alters the call.
		OnScopeShortfall: func(sf ScopeShortfall) {
			observations.recordShortfall(sf)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("mcp.New: %w", err)
	}
	if connectErr := provider.Connect(ctx); connectErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr := provider.Close(cleanupCtx)
		cancel()
		return nil, errors.Join(fmt.Errorf("provider.Connect: %w", connectErr), observations.authRequired(), cleanupErr)
	}
	descriptors, discoverErr := provider.Discover(ctx)
	if discoverErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr := provider.Close(cleanupCtx)
		cancel()
		return nil, errors.Join(fmt.Errorf("provider.Discover: %w", discoverErr), observations.authRequired(), cleanupErr)
	}
	descriptors = filterDiscoveredTools(descriptors, ms.Name, deps.ToolAllowlist, deps.ToolDenylist)
	return &PreparedAttachment{
		ms: ms, deps: deps, mode: mode, defaultPolicy: defaultPolicy,
		provider: provider, closeFn: provider.Close, descriptors: descriptors, observations: observations,
	}, nil
}

func privateOverrideProviderName(ms config.MCPServerConfig) string {
	if name := strings.TrimSpace(ms.OAuthProvider); name != "" {
		return name
	}
	if ms.Injection != nil {
		return strings.TrimSpace(ms.Injection.Provider)
	}
	return ""
}

func ownedInjectionProvider(ms config.MCPServerConfig, deps AttachDeps) auth.OAuthProvider {
	if deps.OwnOAuthProvider && deps.OAuthProviderOverride != nil && ms.OAuthProvider == "" && ms.Injection != nil {
		return deps.OAuthProviderOverride
	}
	return nil
}

func filterDiscoveredTools(descriptors []tools.ToolDescriptor, source string, allowlist, denylist []string) []tools.ToolDescriptor {
	if len(allowlist) == 0 && len(denylist) == 0 {
		return descriptors
	}
	allowed := make(map[string]struct{}, len(allowlist))
	denied := make(map[string]struct{}, len(denylist))
	for _, name := range allowlist {
		allowed[name] = struct{}{}
	}
	for _, name := range denylist {
		denied[name] = struct{}{}
	}
	prefix := source + "_"
	out := make([]tools.ToolDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Tool.Form != tools.ToolFormTool {
			out = append(out, descriptor)
			continue
		}
		name := strings.TrimPrefix(descriptor.Tool.Name, prefix)
		if _, deniedName := denied[name]; deniedName {
			continue
		}
		if len(allowed) > 0 {
			if _, allowedName := allowed[name]; !allowedName {
				continue
			}
		}
		out = append(out, descriptor)
	}
	return out
}

// Activate privately reserves the reversible registry replacement first, then
// swaps the catalog source as the dispatch linearization point. The old
// same-owner provider remains callable through both the old catalog descriptors
// and direct registry reads until that point and is closed only after both
// shared structures publish successfully.
func (p *PreparedAttachment) Activate(ctx context.Context) error {
	return p.ActivateIf(ctx, nil)
}

// ActivateIf reserves the exact provider handle in the non-dispatchable MCP
// registry, then runs prove immediately before the catalog publication. Exact
// teardown can address and close that staged handle while prove performs
// durable reads. Publication commits through the same reservation, so either
// teardown invalidates it first or every later teardown sees the live handle;
// there is no catalog-only generation between those outcomes.
func (p *PreparedAttachment) ActivateIf(ctx context.Context, prove func(context.Context) error) error {
	return p.ActivateUnder(ctx, func(ctx context.Context, publish func() error) error {
		if prove != nil {
			if err := prove(ctx); err != nil {
				return err
			}
		}
		return publish()
	})
}

// ActivateUnder stages the exact private registry handle, then delegates the
// final local publication callback to admit. Signed capability callers use an
// exact durable operation-slot fence in admit, so removal CAS and local catalog
// visibility have one cross-runtime ordering point.
func (p *PreparedAttachment) ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if admit == nil {
		return errors.New("mcp prepared attachment activation admission is nil")
	}
	if p.closed {
		return fmt.Errorf("mcp prepared attachment %q is closed", p.ms.Name)
	}
	if p.activated {
		return nil
	}
	if err := p.deps.Registry.CheckServerIDUnambiguous(p.ms.Name); err != nil {
		return fmt.Errorf("mcp server %q: %w", p.ms.Name, err)
	}
	priorOwner, priorExists := p.deps.Registry.OwnerOf(p.ms.Name)
	if priorExists && priorOwner != p.deps.Owner {
		return fmt.Errorf("%w: a connection named %q is already registered to a different owner", ErrConnectionNameOwnerConflict, p.ms.Name)
	}
	urlOrCommand := p.ms.URL
	if urlOrCommand == "" {
		urlOrCommand = strings.Join(p.ms.Command, " ")
	}
	if p.registrySwap == nil {
		registrySwap, err := p.deps.Registry.StageRegistration(ServerRegistration{
			Provider: p.provider, Transport: string(p.mode), URLOrCommand: urlOrCommand,
			InitialState: ServerStateOnline, Policy: p.defaultPolicy,
			OAuthDiscoveryAllowedOrigins: append([]string(nil), p.ms.OAuthDiscoveryAllowedOrigins...),
			Owner:                        p.deps.Owner,
			DescriptorFingerprint:        p.deps.DescriptorFingerprint,
		}, p.descriptors)
		if err != nil {
			return fmt.Errorf("registry.StageRegistration: %w", err)
		}
		p.registrySwap = registrySwap
		p.observations.transfer(registrySwap)
	}
	var catalogSwap tools.CatalogSourceSwap
	published := false
	var publishErr error
	admitErr := admit(ctx, func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		published, publishErr = p.registrySwap.Publish(cleanupCtx, func() error {
			var err error
			catalogSwap, err = p.deps.Catalog.StageSource(tools.ToolSourceID(p.ms.Name), p.descriptors, priorExists)
			if err != nil {
				return fmt.Errorf("catalog.Register source %q: %w", p.ms.Name, err)
			}
			// Commit synchronizes the optional search cache before exact teardown may
			// proceed. Once this callback returns, registry publication cannot fail.
			catalogSwap.Commit()
			return nil
		})
		if !published {
			return publishErr
		}
		return nil
	})
	if admitErr != nil {
		if published {
			// Catalog and registry publication is the irreversible local commit.
			// A durable read-only fence may still report a transaction-release
			// error afterwards; treating that ambiguity as pre-publication would
			// close a handle that is already dispatchable. Preserve the live
			// generation and let durable reconciliation resolve the receipt.
			p.activated = true
			if p.deps.Logger != nil {
				p.deps.Logger.Warn("mcp: server activated despite post-publication admission error", slog.String("name", p.ms.Name), slog.String("error", admitErr.Error()))
			}
			return nil
		}
		rollbackErr := p.registrySwap.Rollback()
		return errors.Join(admitErr, rollbackErr)
	}
	if !published {
		rollbackErr := p.registrySwap.Rollback()
		return errors.Join(publishErr, rollbackErr)
	}
	p.activated = true
	if publishErr != nil && p.deps.Logger != nil {
		p.deps.Logger.Warn("mcp: server activated but displaced transport cleanup failed", slog.String("name", p.ms.Name), slog.String("error", publishErr.Error()))
	}
	if p.deps.Logger != nil {
		p.deps.Logger.Info("mcp: server attached", slog.String("name", p.ms.Name), slog.String("transport", string(p.mode)), slog.Int("tools_registered", len(p.descriptors)))
	}
	return nil
}

// Close drains the prepared provider. It is idempotent.
func (p *PreparedAttachment) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var rollbackErr error
	if p.registrySwap != nil && !p.activated {
		rollbackErr = p.registrySwap.Rollback()
	}
	return errors.Join(rollbackErr, p.closeFn(ctx))
}

func closePreparedAfterFailure(ctx context.Context, prepared *PreparedAttachment, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(cause, prepared.Close(cleanupCtx))
}

// ProjectToolPolicies converts an MCPServerConfig's operator-facing
// policy YAML into the driver's runtime ToolPolicy fields:
// the per-server default and the per-tool override map (keyed by the
// MCP server-side tool name). The config package owns the single
// config→policy translation seam (config.ToolPolicyConfig.ToToolPolicy);
// this helper performs only the trivial primitive→tools.ToolPolicy copy.
// It lives next to the driver (— promoted from
// cmd/harbor, where it was stranded because internal/config cannot
// import internal/tools). Any projection error (e.g. an unknown
// retry_on class) is returned so the boot path fails loud (CLAUDE.md §5).
//
// A nil ms.Policy yields a zero-valued default policy, so the driver
// applies tools.DefaultPolicy() per-field at dispatch — preserving the
// no-policy behaviour exactly.
func ProjectToolPolicies(ms config.MCPServerConfig) (tools.ToolPolicy, map[string]tools.ToolPolicy, error) {
	var defaultPolicy tools.ToolPolicy
	if ms.Policy != nil {
		projected, err := ms.Policy.ToToolPolicy()
		if err != nil {
			return tools.ToolPolicy{}, nil, fmt.Errorf("policy: %w", err)
		}
		defaultPolicy = toolPolicyFromProjected(projected)
	}

	var toolPolicies map[string]tools.ToolPolicy
	if len(ms.ToolPolicies) > 0 {
		toolPolicies = make(map[string]tools.ToolPolicy, len(ms.ToolPolicies))
		for toolName, tp := range ms.ToolPolicies {
			projected, err := tp.ToToolPolicy()
			if err != nil {
				return tools.ToolPolicy{}, nil, fmt.Errorf("tool_policies[%q]: %w", toolName, err)
			}
			toolPolicies[toolName] = toolPolicyFromProjected(projected)
		}
	}

	return defaultPolicy, toolPolicies, nil
}

// toolPolicyFromProjected copies the cycle-free config.ProjectedToolPolicy
// image into the runtime tools.ToolPolicy. Fields the operator omitted
// stay zero so tools.ToolPolicy's own per-field resolved() fall-through
// fills them with the package default at dispatch (per-field semantics,
// the policy layer). RetryOn strings become tools.ErrorClass values; they were
// already validated against the allowlist by ToToolPolicy.
func toolPolicyFromProjected(p config.ProjectedToolPolicy) tools.ToolPolicy {
	var retryOn []tools.ErrorClass
	switch {
	case len(p.RetryOn) > 0:
		retryOn = make([]tools.ErrorClass, 0, len(p.RetryOn))
		for _, class := range p.RetryOn {
			retryOn = append(retryOn, tools.ErrorClass(class))
		}
	case p.RetryOnEmpty:
		// Explicit empty, non-nil slice → "retry on nothing" (exactly
		// one attempt), surviving tools.ToolPolicy.resolved()'s
		// MaxRetries fall-through. See config.ToolPolicyConfig.ToToolPolicy.
		retryOn = []tools.ErrorClass{}
	}
	return tools.ToolPolicy{
		TimeoutMS:   p.TimeoutMS,
		MaxRetries:  p.MaxRetries,
		BackoffBase: p.BackoffBase,
		BackoffMult: p.BackoffMult,
		BackoffMax:  p.BackoffMax,
		RetryOn:     retryOn,
	}
}

// ErrConnectionNameOwnerConflict — a same-name attach collided with a live
// registration owned by a DIFFERENT (tenant, agent). The idempotent same-name
// replace is scoped to the caller's OWN registration, so a cross-owner
// collision is rejected loud rather than tearing down another owner's live
// tools and transport. Callers compare with errors.Is.
var ErrConnectionNameOwnerConflict = errors.New("mcp: connection name already registered to a different owner")

// ErrOAuthBinding — a connection's `oauth_provider` binding is invalid: it
// names an unregistered provider, sits on a stdio transport, or conflicts
// with a static `Authorization` header. Callers compare with errors.Is.
var ErrOAuthBinding = errors.New("mcp: invalid oauth_provider binding")

// resolveOAuthBinding resolves a connection's non-secret `oauth_provider`
// name against the declared registry and re-enforces the binding rules
// (mirroring config-time validation for the runtime-add path). Returns the
// resolved provider (nil when the connection binds none) or a loud error.
func resolveOAuthBinding(ms config.MCPServerConfig, mode MCPTransportMode, providers OAuthProviderResolver) (auth.OAuthProvider, error) {
	// `meta_annotations` keys are declared `_meta` PATHS and are re-validated
	// here — the shared boot + runtime-set attach door — so a runtime-added
	// connection cannot smuggle an identity-shadowing key at the whole key OR
	// at any path segment, declare a path deeper than the audit redactor can
	// walk, or declare a path that collides with another annotation or with
	// the credential-injection `_meta` write (which would silently discard one
	// of them at merge time). Every rule comes from the single shared
	// authority in `internal/config`.
	annotationKeys := make([]string, 0, len(ms.MetaAnnotations))
	for k := range ms.MetaAnnotations {
		if err := config.ValidateMCPMetaAnnotationKey(k); err != nil {
			return nil, fmt.Errorf("%w: meta_annotations %s", ErrOAuthBinding, err.Error())
		}
		annotationKeys = append(annotationKeys, k)
	}
	if err := config.ValidateMCPMetaPathCollisions(annotationKeys, serverInjectionMetaPath(ms.Injection)); err != nil {
		return nil, fmt.Errorf("%w: meta_annotations %s", ErrOAuthBinding, err.Error())
	}
	if ms.OAuthProvider == "" {
		return nil, nil
	}
	return resolveProviderBinding(ms, mode, ms.OAuthProvider, providers)
}

// serverInjectionMetaPath returns the connection's credential-injection `_meta`
// key path, or "" when it declares no injection or injects somewhere other than
// `_meta` (a header / `Authorization: Basic` value writes no `_meta` node, so it
// cannot collide with an annotation path).
func serverInjectionMetaPath(inj *config.MCPCredentialInjectionConfig) string {
	if inj == nil || strings.TrimSpace(inj.Form) != config.MCPInjectionFormMeta {
		return ""
	}
	return strings.TrimSpace(inj.MetaKey)
}

// resolveToolOAuthBindings resolves a connection's per-entry `oauth_provider`
// overrides (MCP-side name → provider name) against the registry, re-enforcing
// EVERY binding rule per entry exactly like the connection-level binding
// (unknown name / stdio transport / static-Authorization conflict /
// downstream-host allow-list). The overrides apply to every identity-stamped
// RPC that addresses by the entry's key — CallTool by tool name, ReadResource /
// SubscribeResource by resource URI, GetPrompt by prompt name (the cross-surface
// key-collision guard runs at discovery, ErrAmbiguousOAuthBinding). An empty map
// returns nil (no overrides). An empty key or empty provider name fails loud.
func resolveToolOAuthBindings(ms config.MCPServerConfig, mode MCPTransportMode, providers OAuthProviderResolver) (map[string]auth.OAuthProvider, error) {
	if len(ms.ToolOAuthProviders) == 0 {
		return nil, nil
	}
	out := make(map[string]auth.OAuthProvider, len(ms.ToolOAuthProviders))
	for toolName, providerName := range ms.ToolOAuthProviders {
		if strings.TrimSpace(toolName) == "" {
			return nil, fmt.Errorf("%w: tool_oauth_providers key (tool name) must not be empty", ErrOAuthBinding)
		}
		if strings.TrimSpace(providerName) == "" {
			return nil, fmt.Errorf("%w: tool_oauth_providers[%q] provider name must not be empty", ErrOAuthBinding, toolName)
		}
		prov, err := resolveProviderBinding(ms, mode, providerName, providers)
		if err != nil {
			return nil, fmt.Errorf("tool_oauth_providers[%q]: %w", toolName, err)
		}
		out[toolName] = prov
	}
	return out, nil
}

// resolveInjectionBinding resolves a connection's non-secret per-user
// credential-INJECTION mapping (receiver-style server) into a resolved
// CredentialInjection, re-enforcing every rule config validation enforces so the
// runtime-add path (which never passes through `harbor validate`) is held to the
// same bar: mutual exclusivity with the bearer/oauth mode + a static
// Authorization header (one auth mode per connection), an http(s) transport, the
// broker's downstream-sink allow-list (via resolveProviderBinding), and a
// redaction-covered target key. Returns nil when the connection declares no
// injection.
func resolveInjectionBinding(ms config.MCPServerConfig, mode MCPTransportMode, providers OAuthProviderResolver) (*CredentialInjection, error) {
	if ms.Injection == nil {
		return nil, nil
	}
	inj := ms.Injection
	if ms.OAuthProvider != "" || len(ms.ToolOAuthProviders) > 0 {
		return nil, fmt.Errorf("%w: injection is mutually exclusive with oauth_provider/tool_oauth_providers (one auth mode per connection)", ErrOAuthBinding)
	}
	if strings.TrimSpace(inj.Provider) == "" {
		return nil, fmt.Errorf("%w: injection.provider must name a declared oauth provider", ErrOAuthBinding)
	}
	// resolveProviderBinding enforces stdio/url + static-Authorization conflict +
	// unknown-name + the downstream-sink allow-list — the same gate the bearer
	// binding uses (the pulled credential leaves to the connection host).
	prov, err := resolveProviderBinding(ms, mode, inj.Provider, providers)
	if err != nil {
		return nil, err
	}
	ci := &CredentialInjection{Provider: prov}
	switch inj.Form {
	case config.MCPInjectionFormHeader:
		if strings.TrimSpace(inj.Header) == "" {
			return nil, fmt.Errorf("%w: injection form=header requires a header", ErrOAuthBinding)
		}
		if strings.EqualFold(inj.Header, "authorization") {
			return nil, fmt.Errorf("%w: injection form=header must not target the Authorization header (use form=basic)", ErrOAuthBinding)
		}
		if !config.IsReceiverInjectionCredentialKey(inj.Header) {
			return nil, fmt.Errorf("%w: injection header %q is not a redaction-covered credential key (name it with a credential segment such as -api-key / -token / -secret)", ErrOAuthBinding, inj.Header)
		}
		ci.Form = InjectionFormHeader
		ci.Header = inj.Header
	case config.MCPInjectionFormBasic:
		ci.Form = InjectionFormBasic
		ci.BasicUsername = inj.BasicUsername
	case config.MCPInjectionFormMeta:
		if strings.TrimSpace(inj.MetaKey) == "" {
			return nil, fmt.Errorf("%w: injection form=meta requires a meta_key path", ErrOAuthBinding)
		}
		segs := strings.Split(inj.MetaKey, ".")
		for _, seg := range segs {
			if strings.TrimSpace(seg) == "" {
				return nil, fmt.Errorf("%w: injection meta_key has an empty segment", ErrOAuthBinding)
			}
			if isReservedMetaKey(seg) {
				return nil, fmt.Errorf("%w: injection meta_key segment %q is reserved (triple/agent_id/traceparent/tracestate and io.modelcontextprotocol/-prefixed keys are runtime-stamped)", ErrOAuthBinding, seg)
			}
		}
		if !config.IsReceiverInjectionCredentialKey(segs[len(segs)-1]) {
			return nil, fmt.Errorf("%w: injection meta_key leaf is not a redaction-covered credential key (name the leaf with a credential segment such as api_key / token / secret)", ErrOAuthBinding)
		}
		ci.Form = InjectionFormMeta
		ci.MetaKey = segs
	case "":
		return nil, fmt.Errorf("%w: injection.form must be set (header/basic/meta)", ErrOAuthBinding)
	default:
		return nil, fmt.Errorf("%w: injection.form %q is invalid (header/basic/meta)", ErrOAuthBinding, inj.Form)
	}
	return ci, nil
}

// resolveProviderBinding resolves one provider name against the registry and
// re-enforces the binding rules the connection-level and per-tool bindings
// share. Returns the resolved provider or a loud error.
func resolveProviderBinding(ms config.MCPServerConfig, mode MCPTransportMode, providerName string, providers OAuthProviderResolver) (auth.OAuthProvider, error) {
	// The binding needs an HTTP request to inject into. An explicit stdio
	// transport is rejected, and so is ANY connection without a URL — an
	// auto transport with only a command auto-selects stdio at connect,
	// which would silently skip injection while the operator believes
	// per-identity auth is on (silent degradation, forbidden).
	if mode == TransportStdio || ms.URL == "" {
		return nil, fmt.Errorf("%w: oauth_provider set on a connection without an http(s) url (stdio — explicit or auto-selected from a command-only config — carries no HTTP request to inject Authorization into)", ErrOAuthBinding)
	}
	for k := range ms.Headers {
		if strings.EqualFold(k, "authorization") {
			return nil, fmt.Errorf("%w: static Authorization header conflicts with oauth_provider (one auth mode per connection)", ErrOAuthBinding)
		}
	}
	prov, ok := providers.Get(providerName)
	if !ok {
		return nil, fmt.Errorf("%w: unknown provider %q (registered: %s)", ErrOAuthBinding, providerName, strings.Join(providers.Names(), ","))
	}
	// Downstream-sink allow-list (the credential-plane invariant):
	// the provider's boot-declared allow-list is the ONLY authority for
	// where its credential may be injected. An empty allow-list on a
	// bearer-injecting provider is refused fail-closed (a provider that can
	// inject a bearer must declare where); a connection host absent from
	// the list is refused — never a silent unauthenticated dial. Host
	// comparison uses the ONE normaliser (config.NormalizeDownstreamHost),
	// shared with config-time validation.
	allowed := prov.AllowedDownstreamHosts()
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: provider %q declares no allowed_downstream_hosts — a bearer-injecting provider must declare its downstream sinks (fail-closed; the credential-plane invariant)", ErrOAuthBinding, providerName)
	}
	connHost := config.NormalizeDownstreamHost(ms.URL)
	if connHost == "" || !hostAllowed(allowed, connHost) {
		return nil, fmt.Errorf("%w: connection host %q is not in provider %q's allowed_downstream_hosts — the credential may only be injected into a boot-declared downstream sink", ErrOAuthBinding, connHost, providerName)
	}
	return prov, nil
}

// resolveArtifactEgress validates a connection's egress-substitution
// declaration and compiles its per-tool artifact-parameter mapping.
//
// It re-enforces at the attach door every rule the boot validator
// enforces, because the runtime-add path never passes through `harbor
// validate` and the attach is the one gate both paths share:
//
//   - a mapping REQUIRES the operator's byte-eligibility declaration.
//     The flag IS the containment boundary for the feature, so a mapping
//     without it is refused rather than silently ignored;
//   - neither field is accepted on a connection without an http(s) url —
//     explicit stdio, or an auto transport with only a command, which
//     auto-selects stdio at connect and would otherwise leave an
//     operator believing egress was on while it silently never fired.
//     Base64-encoded artifact bytes belong in an HTTP body, not a stdio
//     frame;
//   - the mapping's SHAPE goes through the single shared authority in
//     internal/config, the same way the `_meta` annotation rules do.
//
// The half this cannot do is the schema check — whether each mapped
// parameter is declared, and declared string-typed, by the server
// itself. That needs the live discovered tool set and runs at Discover.
//
// Returns the empty mapping for a connection that declares none.
func resolveArtifactEgress(ms config.MCPServerConfig, mode MCPTransportMode) (artifactegress.Mapping, error) {
	if len(ms.ArtifactParams) == 0 && !ms.ArtifactByteEligible {
		return artifactegress.Mapping{}, nil
	}
	stdioOrNoURL := mode == TransportStdio || ms.URL == ""
	if stdioOrNoURL {
		return artifactegress.Mapping{}, fmt.Errorf("%w: artifact egress is declared on a connection without an http(s) url (stdio — explicit, or auto-selected from a command-only config); base64-encoded artifact bytes belong in an HTTP body, not a stdio frame", ErrArtifactEgressNotEligible)
	}
	if len(ms.ArtifactParams) > 0 && !ms.ArtifactByteEligible {
		return artifactegress.Mapping{}, fmt.Errorf("%w: artifact_params is set without artifact_byte_eligible on the same connection — the eligibility declaration IS the containment boundary for egress substitution, so a mapping without it is refused rather than silently ignored", ErrArtifactEgressNotEligible)
	}
	if err := config.ValidateMCPArtifactParams(ms.ArtifactParams); err != nil {
		return artifactegress.Mapping{}, fmt.Errorf("%w: artifact_params %s", ErrArtifactEgressNotEligible, err.Error())
	}
	mapping, err := artifactegress.CompileMapping(ms.ArtifactParams)
	if err != nil {
		return artifactegress.Mapping{}, fmt.Errorf("%w: %w", ErrArtifactEgressNotEligible, err)
	}
	return mapping, nil
}

// hostAllowed reports whether the normalised connection host is present in
// the provider's allow-list, normalising each list entry with the same
// (single) normaliser config-time validation uses.
func hostAllowed(allowList []string, normConnHost string) bool {
	for _, h := range allowList {
		if config.NormalizeDownstreamHost(h) == normConnHost {
			return true
		}
	}
	return false
}

// cloneHeaderMap returns a defensive copy of m so the Provider's
// Headers map cannot be mutated by callers that retain the
// MCPServerConfig.
func cloneHeaderMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
