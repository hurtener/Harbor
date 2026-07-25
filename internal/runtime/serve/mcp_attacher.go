package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
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

	mu      sync.Mutex
	closers []func(context.Context) error
}

// Compile-time assertions that the production concrete satisfies BOTH seams the
// agent-config service binds it to. The mux binds the applier through an
// UNCHECKED type assertion (`in.MCPAttacher.(agentcfgprotocol.DiscoveryOriginApplier)`),
// which degrades silently to "not wired" on a signature drift — the write would
// keep answering 200 with applied_live=false forever. These pin the shapes at
// compile time so the drift is a build failure instead.
var (
	_ agentcfgprotocol.ConnectionAttacher     = (*MCPConnectionAttacher)(nil)
	_ agentcfgprotocol.DiscoveryOriginApplier = (*MCPConnectionAttacher)(nil)
)

// NewMCPConnectionAttacher builds the production attacher. catalog,
// registry, and bus are mandatory (mcpdrv.Attach validates them too).
// oauthProviders may be nil when no provider is declared; oauthProviderSet, when
// non-nil, is the runtime set (boot seed + installs) resolution consults first.
// toolContext is the MCP Apps tool-context capturer a runtime-added server's
// providers capture through — pass the runtime's store so a runtime-added
// server behaves like a boot-config one; nil is legitimate only where no store
// exists (the driver then stamps no tool-call id).
func NewMCPConnectionAttacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, bus events.EventBus, logger *slog.Logger, defaultIdentity identity.Identity, oauthProviders map[string]toolauth.OAuthProvider, oauthProviderSet toolauth.ProviderSet, toolContext mcpdrv.ToolContextCapturer) *MCPConnectionAttacher {
	return &MCPConnectionAttacher{
		catalog:          catalog,
		registry:         registry,
		bus:              bus,
		logger:           logger,
		defaultIdentity:  defaultIdentity,
		oauthProviders:   oauthProviders,
		oauthProviderSet: oauthProviderSet,
		toolContext:      toolContext,
	}
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
	// Stamp the (tenant, agent) reconcile-view owner tag from the verified
	// caller identity + the target agent. Fail CLOSED when either component is
	// missing: a runtime-added connection with no owner would be an
	// unreconcilable orphan (a run-start reconcile could never scope to it,
	// leaving it attached forever or, worse, detachable only by a
	// process-global sweep). Never a silent untagged attach (CLAUDE.md §13).
	owner := toolauth.Owner{Tenant: req.Identity.TenantID, Agent: req.AgentID}
	if owner.IsZero() || owner.Tenant == "" || owner.Agent == "" {
		return fmt.Errorf("%w: runtime-added connection %q requires a (tenant, agent) owner (tenant=%q agent=%q)",
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
	}

	// Serialise adds: the per-add closer slice is merged into the master
	// chain under the lock, and serialising the whole attach keeps two
	// concurrent adds of the same name from racing in the catalog / registry.
	// Adds are infrequent admin actions, so the coarse lock is acceptable.
	a.mu.Lock()
	defer a.mu.Unlock()

	var local []func(context.Context) error
	err := mcpdrv.Attach(ctx, ms, mcpdrv.AttachDeps{
		Catalog:          a.catalog,
		Registry:         a.registry,
		Bus:              a.bus,
		Logger:           a.logger,
		DefaultIdentity:  a.defaultIdentity,
		Closers:          &local,
		OAuthProviders:   a.oauthProviders,
		OAuthProviderSet: a.oauthProviderSet,
		// Capture app tool contexts on this connection exactly as the
		// boot-config attach path does, so a `ui://` app declared by a tool on
		// a runtime-added server renders with its real data instead of an
		// empty shell.
		ToolContext: a.toolContext,
		Owner:       owner,
	})
	// Merge whatever closers Attach appended (a successful Connect appends the
	// provider's Close even if a later step failed — drain it on teardown).
	a.closers = append(a.closers, local...)
	if err != nil {
		// A binding/config error (unknown provider, missing or mismatched
		// downstream allow-list, stdio/no-url, static-Authorization conflict)
		// is a fail-loud operator misconfiguration, never a transient
		// auth-required. Its message carries "oauth_provider", which the string
		// heuristic below would otherwise match on "oauth" and misclassify as
		// auth-required — silently parking the connection and hiding the real
		// diagnostic. Check the typed sentinel first so the operator sees the
		// actual cause (e.g. "unknown provider" / "host not in allow-list").
		if errors.Is(err, mcpdrv.ErrOAuthBinding) {
			return err
		}
		if looksLikeAuthRequired(err) {
			return fmt.Errorf("%w: %w", agentcfgprotocol.ErrAuthRequired, err)
		}
		return err
	}
	return nil
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

// looksLikeAuthRequired reports whether an attach error indicates the MCP
// server requires authorization, so the service routes it onto the unified
// pause/resume primitive. The MCP driver does not yet surface a TYPED auth
// error, so this is a documented best-effort string heuristic over the
// transport's HTTP-status / OAuth markers; it is conservative (a false
// negative surfaces as a loud `failed`, never a silent drop, and a false
// positive only parks an operator-cancellable connection — no security
// bypass either way). When the driver gains a typed auth sentinel this
// heuristic is replaced by errors.Is — tracked as a follow-up alongside the
// MCP-SDK typed-error work.
func looksLikeAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// The numeric 401 is matched on a word boundary so it does not trip on
	// an unrelated number (a latency, a byte count, a port). The remaining
	// markers are auth-specific phrases.
	if status401Pattern.MatchString(msg) {
		return true
	}
	for _, marker := range []string{"unauthorized", "www-authenticate", "oauth", "invalid_token", "authentication required", "authorization required"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// status401Pattern matches an HTTP 401 status code on a word boundary so a
// bare "401" inside an unrelated number does not false-positive.
var status401Pattern = regexp.MustCompile(`\b401\b`)
