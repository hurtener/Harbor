package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// cmd_dev_mcp_attacher.go — the production concrete that drives the real MCP
// attach lifecycle for the admin-driven runtime add
// (`agent_config.add_mcp_connection`). It is the §4.4 boundary glue: it
// imports the concrete MCP driver (allowed in cmd/harbor) and satisfies the
// driver-agnostic agentcfgprotocol.ConnectionAttacher interface the
// agent-config service depends on. The driver remains unaware of the registry
// and the agent-config service — this glue orchestrates.
//
// The harbortest/devstack helper carries a mirror of this concrete so
// integration tests exercise the same real attach path.

// ErrRuntimeAddOwnerMissing is returned by MCPConnectionAttacher.Attach when a
// runtime-added connection reaches the attach path without a full (tenant,
// agent) owner. It is a fail-closed guard: a runtime-added entry with no owner
// tag would be an unreconcilable orphan, so the attach is rejected loudly
// rather than registering an untagged runtime-add.
var ErrRuntimeAddOwnerMissing = errors.New("serve: runtime-added mcp connection is missing its (tenant, agent) owner")

// ErrReattachStdioNotAllowed — a DECLARED stdio descriptor's argv[0] is absent
// from the CURRENT boot stdio allowlist, so the run-start re-attach refuses it.
// The revision was written while the command was allowlisted; boot policy has
// since tightened (or the allowlist is empty, the fail-closed default, which
// refuses every stdio re-attach). Fail-closed: the process is never spawned.
// Callers compare with errors.Is.
var ErrReattachStdioNotAllowed = errors.New("serve: declared stdio mcp connection command is not in the current boot allowlist — not re-attached (fail-closed; boot policy is re-applied at run start, not the policy in force when the revision was written)")

// ErrReattachInjectionDisabled — a DECLARED descriptor carries a per-user
// credential-INJECTION mapping but the fail-closed `tools.allow_wire_injection`
// opt-in is OFF, so the run-start re-attach refuses to rebuild it. The dev opt-in
// is a KILL-SWITCH, the exact twin of the provider side's
// ErrWireDescriptorDisabled: a mapping persisted while the opt-in was on is not
// rebuilt after a restart with it off. Callers compare with errors.Is.
var ErrReattachInjectionDisabled = errors.New("serve: declared mcp connection carries a credential-injection mapping and tools.allow_wire_injection is off — not re-attached (the dev opt-in is a kill-switch: a mapping persisted while it was on is not rebuilt after a restart with it off)")

// Re-attach retry-window defaults. The delays are per-(owner, name) and
// process-local; they exist because the reconcile is synchronous at run start,
// so a dead declared server would otherwise be re-dialled on every single run.
const (
	// defaultReattachTimeout bounds ONE re-attach's dial + handshake + discovery.
	defaultReattachTimeout = 10 * time.Second
	// reattachBackoffBase is the first retry delay after a retryable failure.
	reattachBackoffBase = 30 * time.Second
	// reattachBackoffMax caps the exponential growth, so a long-down third party
	// is still retried periodically rather than abandoned.
	reattachBackoffMax = 15 * time.Minute
)

// MCPAttacherOption configures the attacher at CONSTRUCTION only. There is no
// setter: the attacher is a compiled artifact shared across concurrent runs, so
// its policy inputs are immutable after construction (the concurrent-reuse
// contract). The variadic shape keeps the existing constructor call sites
// unchanged — a caller that threads no options gets the fail-closed defaults
// (empty stdio allowlist, injection opt-in off).
type MCPAttacherOption func(*MCPConnectionAttacher)

// WithReattachGates threads the CURRENT boot policy the run-start re-attach leg
// re-applies: the stdio command allowlist and the effective per-user
// credential-injection opt-in. Both default to their fail-closed values, so a
// caller that does not thread them refuses every stdio re-attach and every
// injection-carrying descriptor rather than widening either gate silently.
func WithReattachGates(stdioAllowlist []string, allowWireInjection bool) MCPAttacherOption {
	return func(a *MCPConnectionAttacher) {
		set := make(map[string]struct{}, len(stdioAllowlist))
		for _, c := range stdioAllowlist {
			if c != "" {
				set[c] = struct{}{}
			}
		}
		a.stdioAllowlist = set
		a.allowWireInjection = allowWireInjection
	}
}

// WithReattachTimeout overrides the per-connection re-attach bound. Zero or
// negative leaves the default. Tests use it to prove the bound fires; operators
// do not configure it (the value is a runtime constant, not a knob).
func WithReattachTimeout(d time.Duration) MCPAttacherOption {
	return func(a *MCPConnectionAttacher) {
		if d > 0 {
			a.reattachTimeout = d
		}
	}
}

// WithReattachClock overrides the clock the re-attach backoff reads, so the
// backoff is testable without sleeping (CLAUDE.md §11). Nil leaves time.Now.
func WithReattachClock(now func() time.Time) MCPAttacherOption {
	return func(a *MCPConnectionAttacher) {
		if now != nil {
			a.now = now
		}
	}
}

// MCPConnectionAttacher implements agentcfgprotocol.ConnectionAttacher by
// reusing the boot-time mcpdrv.Attach lifecycle (dial → initialize →
// discover → register) against the LIVE catalog + registry + bus. It owns a
// mutex-guarded closer chain so a runtime-added server's transport drains on
// stack teardown.
//
// Concurrent reuse: the collaborators (catalog / registry / bus / logger /
// defaultIdentity) are set once at construction; the only mutable state is
// the closers slice, guarded by mu (documented internally-synchronised).
type MCPConnectionAttacher struct {
	catalog         tools.ToolCatalog
	registry        *mcpdrv.Registry
	bus             events.EventBus
	logger          *slog.Logger
	defaultIdentity identity.Identity
	// oauthProviders is the declared OAuth-provider registry a runtime-added
	// connection's `oauth_provider` binding resolves against (mcpdrv.Attach
	// does the resolution + fail-loud). Set once at construction. Superseded by
	// oauthProviderSet when non-nil (kept for the fallback / callback path).
	oauthProviders map[string]toolauth.OAuthProvider
	// oauthProviderSet is the runtime provider SET (boot seed + owner-tagged
	// Protocol-installed providers) a runtime-added connection's binding
	// resolves against, so an installed provider is bindable. Takes precedence
	// over oauthProviders. Set once at construction.
	oauthProviderSet toolauth.ProviderSet
	// toolContext is the MCP Apps tool-context capturer a runtime-added
	// server's providers persist an app-declaring tool call's input + lowered
	// result through — the SAME store the boot-config attach path wires, so a
	// server attached at runtime is not a second-class citizen. Without it, a
	// tool on a runtime-added server that declares a `ui://` app would capture
	// nothing, and a host could not fetch the context that makes the rendered
	// app show real data. Nil is legitimate (an embedder with no store); the
	// driver then stamps no tool-call id at all rather than promising a
	// context that does not exist. Set once at construction.
	toolContext mcpdrv.ToolContextCapturer
	// artifactEgressMaxBytes bounds ONE substituted artifact value on one
	// outbound call for connections this attacher wires — the deployment's
	// `tools.mcp_artifact_egress_max_bytes`. Zero leaves the driver on
	// config.DefaultMCPArtifactEgressMaxBytes, so an embedder that does
	// not set it still gets a real ceiling rather than an unbounded one.
	// Set once at construction.
	artifactEgressMaxBytes int
	// stdioAllowlist is the CURRENT boot stdio-command allowlist the run-start
	// re-attach re-applies (the same fail-closed RCE gate the admin add door
	// applies before it dials). Nil / empty refuses every stdio re-attach. Set
	// once at construction; a nil allowlist is the fail-closed default, so a
	// caller that forgets to thread it cannot accidentally widen the gate.
	stdioAllowlist map[string]struct{}
	// allowWireInjection is the EFFECTIVE per-user credential-injection opt-in
	// (`tools.allow_wire_injection`, config flag OR the boot env captured at
	// process start). It is a KILL-SWITCH on the re-attach path: a mapping
	// persisted while it was on is not rebuilt after a restart with it off. Set
	// once at construction; false is the fail-closed default.
	allowWireInjection bool
	// reattachTimeout bounds ONE re-attach's dial + handshake + discovery. It is
	// load-bearing, not hygiene: the MCP provider's Connect carries no internal
	// timeout (it inherits the caller's ctx) and the reconcile is synchronous at
	// run start, so an unresponsive declared server would otherwise add its full
	// stall to every run's start. Set once at construction.
	reattachTimeout time.Duration
	// now is the clock the re-attach backoff reads. Injectable so the backoff
	// tests are deterministic rather than sleeping. Set once at construction.
	now func() time.Time

	mu      sync.Mutex
	closers []func(context.Context) error
	// reattachBackoff is the per-(owner, name) retry window the run-start attach
	// leg holds so a permanently-unreachable or permanently-refused declared
	// server is not re-dialled — and not re-reported — at every run start.
	// INTERNALLY SYNCHRONISED: guarded by mu, the same lock that serialises the
	// whole attach. It is deliberately process-local and NOT persisted: a
	// crash-looping deployment re-dials once per boot, which is bounded by the
	// boot rate rather than the run rate, and reconcile-attempt scheduling
	// metadata does not belong in the state store.
	reattachBackoff map[reattachKey]*reattachState
}

// reattachKey identifies one reconciling owner's connection for the backoff
// table. The (tenant, agent) owner is part of the key so two owners' identically
// named connections back off independently — a shared runtime must not let one
// tenant's dead server suppress another's retry.
type reattachKey struct {
	tenant string
	agent  string
	name   string
}

// reattachState is one (owner, name)'s retry window.
type reattachState struct {
	// fingerprint identifies the descriptor the window was opened for. A changed
	// descriptor (an operator edit) resets the window immediately, so a fix is
	// picked up at the very next run start rather than after the backoff.
	fingerprint string
	// failures counts consecutive failures, driving the exponential delay.
	failures int
	// nextAttempt is the earliest instant a retry may dial. Zero means "retry
	// now"; terminal (below) overrides it.
	nextAttempt time.Time
	// terminal marks a class that cannot heal by re-dialling (a boot-policy
	// refusal, a cross-owner name collision, an ambiguous id). Such a class is
	// reported ONCE per attempted descriptor and then suppressed until the
	// descriptor changes or the process restarts — never re-emitted on every run
	// start, and never silently dropped (the suppressed count rides the next
	// emitted event).
	terminal bool
	// suppressed counts attempts skipped since the last EMITTED event, so the
	// suppression is reported rather than invisible.
	suppressed int
}

// WithArtifactEgressMaxBytes sets the egress-substitution ceiling the
// attacher carries into every connection it wires — the operator's
// `tools.mcp_artifact_egress_max_bytes`, resolved through
// config.ToolsConfig.ResolvedMCPArtifactEgressMaxBytes so the
// documented default applies when the key is unset.
//
// A non-positive value is ignored rather than stored, so an
// unconfigured or mis-wired caller lands on the driver's own default
// ceiling instead of an unbounded one.
func WithArtifactEgressMaxBytes(n int) MCPAttacherOption {
	return func(a *MCPConnectionAttacher) {
		if n > 0 {
			a.artifactEgressMaxBytes = n
		}
	}
}

// Compile-time assertions that the production concrete satisfies BOTH seams the
// agent-config service binds it to. The mux binds the applier through an
// UNCHECKED type assertion (`in.MCPAttacher.(agentcfgprotocol.DiscoveryOriginApplier)`),
// which degrades silently to "not wired" on a signature drift — the write would
// keep answering 200 with applied_live=false forever. These pin the shapes at
// compile time so the drift is a build failure instead.
var (
	_ agentcfgprotocol.ConnectionAttacher     = (*MCPConnectionAttacher)(nil)
	_ agentcfgprotocol.ConnectionPreparer     = (*MCPConnectionAttacher)(nil)
	_ agentcfgprotocol.DiscoveryOriginApplier = (*MCPConnectionAttacher)(nil)
	_ agentcfgprotocol.ConnectionDetacher     = (*MCPConnectionAttacher)(nil)
	_ projection.ConnectionReattacher         = (*MCPConnectionAttacher)(nil)
)

// NewMCPConnectionAttacher builds the production attacher. catalog,
// registry, and bus are mandatory (mcpdrv.Attach validates them too).
// oauthProviders may be nil when no provider is declared; oauthProviderSet, when
// non-nil, is the runtime set (boot seed + installs) resolution consults first.
// toolContext is the MCP Apps tool-context capturer a runtime-added server's
// providers capture through — pass the runtime's store so a runtime-added
// server behaves like a boot-config one; nil is legitimate only where no store
// exists (the driver then stamps no tool-call id).
// opts thread the run-start re-attach leg's CURRENT boot policy (the stdio
// allowlist, the credential-injection opt-in), the egress-substitution ceiling,
// and the test seams. Every option defaults fail-closed, so an omitted option
// never widens a gate.
func NewMCPConnectionAttacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, bus events.EventBus, logger *slog.Logger, defaultIdentity identity.Identity, oauthProviders map[string]toolauth.OAuthProvider, oauthProviderSet toolauth.ProviderSet, toolContext mcpdrv.ToolContextCapturer, opts ...MCPAttacherOption) *MCPConnectionAttacher {
	a := &MCPConnectionAttacher{
		catalog:          catalog,
		registry:         registry,
		bus:              bus,
		logger:           logger,
		defaultIdentity:  defaultIdentity,
		oauthProviders:   oauthProviders,
		oauthProviderSet: oauthProviderSet,
		toolContext:      toolContext,
		reattachTimeout:  defaultReattachTimeout,
		now:              time.Now,
		reattachBackoff:  make(map[reattachKey]*reattachState),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Attach drives the real MCP attach for one runtime-added connection. The
// operator-supplied auth Headers flow to the live transport ONLY (the
// agent-config service never persists them). A partial-attach failure is
// drained by mcpdrv.Attach's own closer-chain handling; this method merges
// the per-add closers into its master chain under the lock so teardown drains
// the subprocess. An auth-required condition is surfaced by wrapping
// agentcfgprotocol.ErrAuthRequired so the service parks on the unified
// pause/resume primitive.
func (a *MCPConnectionAttacher) Attach(ctx context.Context, req agentcfgprotocol.AttachRequest) error {
	prepared, err := a.PrepareConnection(ctx, req)
	if err != nil {
		return err
	}
	if err := prepared.Activate(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr := prepared.Close(cleanupCtx)
		cancel()
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// PrepareConnection dials and discovers against a private attachment. The
// shared catalog and registry remain unchanged until Activate.
func (a *MCPConnectionAttacher) PrepareConnection(ctx context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	// Stamp the (tenant, agent) reconcile-view owner tag from the verified
	// caller identity + the target agent. Fail CLOSED when either component is
	// missing: a runtime-added connection with no owner would be an
	// unreconcilable orphan (a run-start reconcile could never scope to it,
	// leaving it attached forever or, worse, detachable only by a
	// process-global sweep). Never a silent untagged attach (CLAUDE.md §13).
	owner := toolauth.Owner{Tenant: req.Identity.TenantID, Agent: req.AgentID}
	if owner.IsZero() || owner.Tenant == "" || owner.Agent == "" {
		return nil, fmt.Errorf("%w: runtime-added connection %q requires a (tenant, agent) owner (tenant=%q agent=%q)",
			ErrRuntimeAddOwnerMissing, req.Name, owner.Tenant, owner.Agent)
	}

	ms := config.MCPServerConfig{
		Name:            req.Name,
		TransportMode:   transportModeForAdd(req.Transport),
		URL:             req.URL,
		Command:         append([]string(nil), req.Command...),
		Headers:         req.Headers, // SECRET — used for the live transport, never persisted
		OAuthProvider:   req.OAuthProvider,
		MetaAnnotations: req.MetaAnnotations,
		// Close the wiring gap: carry the non-secret discovery allow-list from the
		// add request into the registry snapshot so the OAuth-requirement
		// discovery walker's allowance input is no longer inert for a
		// runtime-added connection (mcpdrv.Attach → Registry.Register reads it).
		OAuthDiscoveryAllowedOrigins: append([]string(nil), req.OAuthDiscoveryAllowedOrigins...),
		// Carry the non-secret per-user credential-INJECTION mapping (receiver-style
		// server) into the config the driver builds, so the shared injection engine
		// (mcpdrv.resolveInjectionBinding) sources + injects the acting principal's
		// credential per outbound call. NON-SECRET (a broker name + a target
		// key/form); the pulled value is resolved per-call from the ctx identity.
		Injection: injectionToConfig(req.Injection),
		// Carry the egress-substitution declaration into the config the
		// driver builds, so the boot path and the runtime-add path share ONE
		// egress engine rather than growing a second. A field on the
		// descriptor that nothing carries forward is inert — the wiring-gap
		// shape the discovery allow-list already hit on this exact path.
		// NON-SECRET (a boolean plus parameter names); mcpdrv.Attach
		// re-enforces the eligibility and transport rules, and Discover
		// checks each mapped parameter against the server's own schema.
		ArtifactByteEligible: req.ArtifactByteEligible,
		ArtifactParams:       config.MCPArtifactParams(cloneArtifactParams(req.ArtifactParams)),
	}
	if req.RequestTimeoutMS > 0 {
		ms.Policy = &config.ToolPolicyConfig{TimeoutMS: req.RequestTimeoutMS}
	}

	// Serialise adds: the per-add closer slice is merged into the master
	// chain under the lock, and serialising the whole attach keeps two
	// concurrent adds of the same name from racing in the catalog / registry.
	// Adds are infrequent admin actions, so the coarse lock is acceptable.
	a.mu.Lock()

	var local []func(context.Context) error
	prepareCtx := ctx
	var cancel context.CancelFunc
	if req.ConnectTimeoutMS > 0 {
		prepareCtx, cancel = context.WithTimeout(ctx, time.Duration(req.ConnectTimeoutMS)*time.Millisecond)
		defer cancel()
	}
	descriptorFingerprint := req.DescriptorFingerprint
	if descriptorFingerprint == "" {
		descriptorFingerprint = agentcfg.MCPConnectionFingerprint(agentcfg.MCPConnectionDescriptor{
			Name: req.Name, Transport: req.Transport, Command: append([]string(nil), req.Command...), URL: req.URL,
			OAuthProvider: req.OAuthProvider, MetaAnnotations: cloneAnnotationsForReattach(req.MetaAnnotations),
			OAuthDiscoveryAllowedOrigins: append([]string(nil), req.OAuthDiscoveryAllowedOrigins...), Injection: req.Injection.Clone(),
			ArtifactByteEligible: req.ArtifactByteEligible, ArtifactParams: cloneArtifactParams(req.ArtifactParams),
		})
	}
	prepared, err := mcpdrv.Prepare(prepareCtx, ms, mcpdrv.AttachDeps{
		Catalog:               a.catalog,
		Registry:              a.registry,
		Bus:                   a.bus,
		Logger:                a.logger,
		DefaultIdentity:       a.defaultIdentity,
		Closers:               &local,
		OAuthProviders:        a.oauthProviders,
		OAuthProviderSet:      a.oauthProviderSet,
		OAuthProviderOverride: req.OAuthProviderOverride,
		OwnOAuthProvider:      req.OwnOAuthProvider,
		ToolAllowlist:         append([]string(nil), req.ToolAllowlist...),
		ToolDenylist:          append([]string(nil), req.ToolDenylist...),
		// Capture app tool contexts on this connection exactly as the
		// boot-config attach path does, so a `ui://` app declared by a tool on
		// a runtime-added server renders with its real data instead of an
		// empty shell.
		ToolContext:           a.toolContext,
		Owner:                 owner,
		DescriptorFingerprint: descriptorFingerprint,
		// The deployment's egress ceiling. Zero leaves mcpdrv.Attach on
		// config.DefaultMCPArtifactEgressMaxBytes — a real ceiling, never an
		// unbounded one.
		ArtifactEgressMaxBytes: a.artifactEgressMaxBytes,
	})
	if err != nil {
		a.mu.Unlock()
		var providerAuth *toolauth.ErrAuthRequired
		var challengeAuth *mcpdrv.PreparationAuthRequiredError
		if errors.As(err, &providerAuth) || errors.As(err, &challengeAuth) {
			return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrAuthRequired, err)
		}
		return nil, err
	}
	return &preparedMCPConnection{owner: a, inner: prepared}, nil
}

// ConnectionMatches reports whether the exact desired descriptor is already
// live for this owner. It is the resume idempotency check after a crash between
// activation and checkpoint deletion.
func (a *MCPConnectionAttacher) ConnectionMatches(owner toolauth.Owner, name, fingerprint string) bool {
	liveOwner, liveFingerprint, ok := a.registry.RegistrationIdentity(name)
	return ok && liveOwner == owner && liveFingerprint == fingerprint
}

type preparedMCPConnection struct {
	owner   *MCPConnectionAttacher
	inner   *mcpdrv.PreparedAttachment
	release sync.Once
}

var _ agentcfgprotocol.AuthorityBoundPreparedConnection = (*preparedMCPConnection)(nil)

func (p *preparedMCPConnection) unlock() { p.release.Do(p.owner.mu.Unlock) }

func (p *preparedMCPConnection) Activate(ctx context.Context) error {
	err := p.inner.Activate(ctx)
	if err == nil {
		p.owner.closers = append(p.owner.closers, p.inner.Close)
	}
	p.unlock()
	return err
}

func (p *preparedMCPConnection) ActivateIf(ctx context.Context, prove func(context.Context) error) error {
	err := p.inner.ActivateIf(ctx, prove)
	if err == nil {
		p.owner.closers = append(p.owner.closers, p.inner.Close)
	}
	p.unlock()
	return err
}

func (p *preparedMCPConnection) ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error {
	err := p.inner.ActivateUnder(ctx, admit)
	if err == nil {
		p.owner.closers = append(p.owner.closers, p.inner.Close)
	}
	p.unlock()
	return err
}

func (p *preparedMCPConnection) Close(ctx context.Context) error {
	err := p.inner.Close(ctx)
	p.unlock()
	return err
}

// SetOAuthDiscoveryOrigins applies a runtime-added connection's
// OAuth-discovery cross-origin allow-list to the LIVE MCP registry (the live
// half of `agent_config.set_mcp_discovery_origins`), returning the prior set.
// It satisfies the agent-config service's DiscoveryOriginApplier seam by
// delegating to the process-global bare-name registry (identity-mandatory for
// authorization; the registry is never re-keyed by identity). The revoke-prune
// of any recorded requirement lives in the registry mutator.
//
// The write is OWNER-SCOPED: (tenant, agentID) is the caller's resolved owner —
// the same (tenant, agent) tag Attach stamps on the registration — and the
// registry replaces the allow-list only on a registration carrying that tag,
// so an allowance write lands on the caller's OWN connection. Both components
// are mandatory: an incomplete owner is refused loud rather than widened into
// an unscoped write (the fail-closed twin of Attach's ErrRuntimeAddOwnerMissing
// guard).
//
// The registry classifies "not mine" and "not there" identically (its
// ErrServerNotFound), which is the right answer AT the registry boundary but
// the wrong diagnostic for the operator: one degrades to a revision-only write,
// the other is an authorization refusal. So the OWNER comparison the operator
// sees is read here from the registry's owner tag — mirroring the same
// [mcpdrv.Registry.OwnerOf] comparison the same-name attach replace performs —
// and surfaced as agentcfgprotocol.ErrConnectionOwnerMismatch. The registry's
// own owner scoping remains the authoritative enforcement (a registration
// replaced between the two reads still refuses the write and degrades, never
// applies).
func (a *MCPConnectionAttacher) SetOAuthDiscoveryOrigins(ctx context.Context, tenant, agentID, name string, origins []string) (prev []string, err error) {
	if a.registry == nil {
		return nil, errors.New("serve: mcp attacher has no registry wired for discovery-origin apply")
	}
	owner := toolauth.Owner{Tenant: tenant, Agent: agentID}
	if owner.Tenant == "" || owner.Agent == "" {
		return nil, fmt.Errorf("%w: discovery-origin write for connection %q requires a (tenant, agent) owner (tenant=%q agent=%q)",
			ErrRuntimeAddOwnerMissing, name, owner.Tenant, owner.Agent)
	}
	if priorOwner, exists := a.registry.OwnerOf(name); exists && priorOwner != owner {
		return nil, fmt.Errorf("%w: %q", agentcfgprotocol.ErrConnectionOwnerMismatch, name)
	}
	prev, err = a.registry.SetOAuthDiscoveryOrigins(ctx, name, owner, origins)
	if errors.Is(err, mcpdrv.ErrServerNotFound) {
		// The connection is declared in the revision but not attached in the live
		// registry under this owner (unreachable server, or awaiting the next
		// run-start reconcile). Translate to the service-layer sentinel so the
		// setter degrades to a revision-only write (applied_live=false) instead of
		// failing loud — the reconcile applies the allowance when the server comes
		// online. Keeps this the ONLY place that imports the mcp driver's
		// not-found sentinel.
		return nil, fmt.Errorf("%w: %q", agentcfgprotocol.ErrDiscoveryTargetNotLive, name)
	}
	return prev, err
}

// DetachConnection tears a runtime-added server back down on the add door's
// COMPENSATION path: the attach succeeded, the revision write that would have
// named it then failed (an expected-revision conflict being the reachable
// case), and without this the server would stay live, registered and exposing
// tools while no revision names it — so `remove_mcp_connection` answers
// not-found and it can never be removed.
//
// It is deliberately a method on the ATTACHER rather than a second concrete:
// the compensation must tear down through exactly the registry + catalog this
// object attached through, and one object satisfying both halves is what makes
// that true by construction rather than by wiring discipline. The teardown
// itself is the shared [detachSource] the run-start reconcile also uses.
//
// The owner is stamped the same way [MCPConnectionAttacher.Attach] stamps it
// and fails CLOSED on a missing component, so a compensating detach can never
// widen into a process-global sweep that would drop a boot server or another
// owner's connection. Idempotent — an attach that never reached registration
// costs a no-op.
func (a *MCPConnectionAttacher) DetachConnection(ctx context.Context, tenant, agentID, name string) error {
	owner := toolauth.Owner{Tenant: tenant, Agent: agentID}
	if owner.Tenant == "" || owner.Agent == "" {
		return fmt.Errorf("%w: compensating detach of connection %q requires a (tenant, agent) owner (tenant=%q agent=%q)",
			ErrRuntimeAddOwnerMissing, name, owner.Tenant, owner.Agent)
	}
	return detachSource(ctx, a.catalog, a.registry, name, owner, a.logger,
		"mcp: detached runtime-added server after its revision write failed")
}

// DetachExactConnection tears down only the registration whose owner and
// complete descriptor fingerprint match the signed pair's frozen identity.
func (a *MCPConnectionAttacher) DetachExactConnection(ctx context.Context, tenant, agentID, name, descriptorFingerprint string) error {
	owner := toolauth.Owner{Tenant: tenant, Agent: agentID}
	if owner.IsZero() || descriptorFingerprint == "" {
		return fmt.Errorf("%w: exact detach requires owner and descriptor fingerprint", ErrRuntimeAddOwnerMissing)
	}
	return detachSourceExpected(ctx, a.catalog, a.registry, name, owner, descriptorFingerprint, a.logger,
		"mcp: detached exact signed capability source")
}

// BeginExactConnectionTeardown installs the process-local admission fence
// before signed desired-state removal. The receipt is sealed after the durable
// CAS or canceled when the CAS is proven not to have committed.
func (a *MCPConnectionAttacher) BeginExactConnectionTeardown(tenant, agentID, name, descriptorFingerprint string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	if a.registry == nil {
		return nil, errors.New("mcp: exact teardown fence requires registry")
	}
	return a.registry.BeginExactRemoval(name, toolauth.Owner{Tenant: tenant, Agent: agentID}, descriptorFingerprint)
}

// Close drains every runtime-added server's transport in reverse order. Wired
// into the boot closer chain so a runtime-added subprocess does not outlive
// the stack.
func (a *MCPConnectionAttacher) Close(ctx context.Context) error {
	a.mu.Lock()
	closers := a.closers
	a.closers = nil
	a.mu.Unlock()
	var firstErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// MCPAddStdioAllowlist projects the operator's stdio allowlist out of config
// for the runtime add-connection gate. A nil block yields a nil allowlist
// (fail-closed — every stdio add is rejected).
func MCPAddStdioAllowlist(cfg *config.Config) []string {
	if cfg == nil || cfg.Tools.MCPAddConnection == nil {
		return nil
	}
	return append([]string(nil), cfg.Tools.MCPAddConnection.StdioAllowlist...)
}

// injectionToConfig projects the agent-config domain injection descriptor onto
// the boot `config.MCPCredentialInjectionConfig` the MCP driver's shared
// injection engine reads (nil for nil). It is the §4.4 boundary translation for
// the runtime-add path: the wire descriptor and the boot yaml converge on ONE
// config shape, so the runtime-add and boot paths share one injection engine
// (never a parallel implementation). NON-SECRET throughout — the mapping only.
func injectionToConfig(d *agentcfg.MCPCredentialInjectionDescriptor) *config.MCPCredentialInjectionConfig {
	if d == nil {
		return nil
	}
	return &config.MCPCredentialInjectionConfig{
		Provider:      d.Provider,
		Form:          d.Form,
		Header:        d.Header,
		BasicUsername: d.BasicUsername,
		MetaKey:       d.MetaKey,
	}
}

// cloneArtifactParams returns a defensive deep copy of an
// egress-substitution artifact-parameter mapping (nil for nil / empty),
// so the attach request and the config the driver reads never share a
// backing map or slice.
func cloneArtifactParams(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for tool, params := range in {
		out[tool] = append([]string(nil), params...)
	}
	return out
}

// transportModeForAdd maps the control-plane transport onto the MCP driver's
// transport-mode string. http maps to "auto" (the driver inspects the URL and
// negotiates streamable-HTTP first, then SSE); stdio maps to "stdio".
func transportModeForAdd(t agentcfg.MCPTransport) string {
	switch t {
	case agentcfg.MCPTransportStdio:
		return string(mcpdrv.TransportStdio)
	default:
		return string(mcpdrv.TransportAuto)
	}
}
