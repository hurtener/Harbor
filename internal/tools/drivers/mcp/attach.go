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

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
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

// Attach wires one configured MCP server (config.MCPServerConfig) into
// the supplied catalog + registry. See the file doc for the full
// lifecycle. Every error is wrapped with the failing step so the boot
// path's `mcp[<name>]: ...` prefix pins the offending server.
func Attach(ctx context.Context, ms config.MCPServerConfig, deps AttachDeps) error {
	if deps.Catalog == nil {
		return fmt.Errorf("mcp attach: Catalog is required")
	}
	if deps.Registry == nil {
		return fmt.Errorf("mcp attach: Registry is required")
	}
	if deps.Closers == nil {
		return fmt.Errorf("mcp attach: Closers chain is required (the Provider's subprocess must drain on teardown)")
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
		return fmt.Errorf("mcp server %q: %w", ms.Name, policyErr)
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
	oauthProvider, bindErr := resolveOAuthBinding(ms, mode, resolver)
	if bindErr != nil {
		return fmt.Errorf("mcp server %q: %w", ms.Name, bindErr)
	}
	provider, err := New(Config{
		Name:             ms.Name,
		TransportMode:    mode,
		URL:              ms.URL,
		Command:          append([]string(nil), ms.Command...),
		Headers:          cloneHeaderMap(ms.Headers),
		KeepAlive:        ms.KeepAlive,
		Logger:           deps.Logger,
		Bus:              deps.Bus,
		DefaultPolicy:    defaultPolicy,
		ToolPolicies:     toolPolicies,
		DefaultIdentity:  deps.DefaultIdentity,
		HostDisplayModes: append([]string(nil), deps.HostDisplayModes...),
		ToolContext:      deps.ToolContext,
		OAuthProvider:    oauthProvider,
		MetaAnnotations:  cloneHeaderMap(ms.MetaAnnotations),
		// Record any `WWW-Authenticate` OAuth step-up challenge on the
		// registry state so an operator can inspect the advertised requirement
		// Best-effort observability — never alters the call.
		OnAuthChallenge: func(ch AuthChallenge) {
			deps.Registry.RecordAuthChallenge(ms.Name, ch)
		},
	})
	if err != nil {
		return fmt.Errorf("mcp.New: %w", err)
	}
	if connectErr := provider.Connect(ctx); connectErr != nil {
		_ = provider.Close(ctx)
		return fmt.Errorf("provider.Connect: %w", connectErr)
	}
	// Append closer NOW (after a successful Connect) so a Discover
	// failure still drains the live subprocess.
	*deps.Closers = append(*deps.Closers, provider.Close)

	descriptors, discoverErr := provider.Discover(ctx)
	if discoverErr != nil {
		return fmt.Errorf("provider.Discover: %w", discoverErr)
	}
	for _, d := range descriptors {
		if regErr := deps.Catalog.Register(d); regErr != nil {
			return fmt.Errorf("catalog.Register(%q): %w", d.Tool.Name, regErr)
		}
	}

	// Surface the live Provider on the MCP Registry so observability
	// surfaces can project it without re-spawning. URLOrCommand is
	// best-effort cosmetic — Console operators read it to identify the
	// server.
	urlOrCommand := ms.URL
	if urlOrCommand == "" {
		urlOrCommand = strings.Join(ms.Command, " ")
	}
	if regErr := deps.Registry.Register(ServerRegistration{
		Provider:     provider,
		Transport:    string(mode),
		URLOrCommand: urlOrCommand,
		InitialState: ServerStateOnline,
		// Surface the configured per-server policy on the registry so the
		// Console's mcp.servers.list / mcp.servers.policy read the policy
		// the operator actually set, not tools.DefaultPolicy() (an
		// audit fix). Per-tool overrides are not part of the registry
		// projection; the per-server default is the headline.
		Policy: defaultPolicy,
		// The explicit cross-origin allowance list for OAuth-requirement
		// discovery fetches. Empty leaves the authorization-server hop
		// needs-allowance (partial discovery), never a network hole.
		OAuthDiscoveryAllowedOrigins: append([]string(nil), ms.OAuthDiscoveryAllowedOrigins...),
		// The reconcile-view owner tag — zero for boot-declared servers, a
		// non-zero (tenant, agent) for a runtime-added connection.
		Owner: deps.Owner,
	}); regErr != nil {
		return fmt.Errorf("registry.Register: %w", regErr)
	}
	// (P1+P2): seed the registry's per-server stats from the
	// boot-time discovery so mcp.servers.list reports the actual
	// tool_count + a real last_discovery_at instead of zero values on a
	// just-booted Runtime.
	if recErr := deps.Registry.RecordDiscovery(ms.Name, descriptors); recErr != nil {
		return fmt.Errorf("registry.RecordDiscovery: %w", recErr)
	}
	if deps.Logger != nil {
		deps.Logger.Info("mcp: server attached",
			slog.String("name", ms.Name),
			slog.String("transport", string(mode)),
			slog.Int("tools_registered", len(descriptors)),
		)
	}
	return nil
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

// ErrOAuthBinding — a connection's `oauth_provider` binding is invalid: it
// names an unregistered provider, sits on a stdio transport, or conflicts
// with a static `Authorization` header. Callers compare with errors.Is.
var ErrOAuthBinding = errors.New("mcp: invalid oauth_provider binding")

// resolveOAuthBinding resolves a connection's non-secret `oauth_provider`
// name against the declared registry and re-enforces the binding rules
// (mirroring config-time validation for the runtime-add path). Returns the
// resolved provider (nil when the connection binds none) or a loud error.
func resolveOAuthBinding(ms config.MCPServerConfig, mode MCPTransportMode, providers OAuthProviderResolver) (auth.OAuthProvider, error) {
	// Reserved / spec-prefixed annotation keys are re-checked here so a
	// runtime-added connection cannot smuggle an identity-shadowing key.
	for k := range ms.MetaAnnotations {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%w: meta_annotations key must not be empty", ErrOAuthBinding)
		}
		if isReservedMetaKey(k) {
			return nil, fmt.Errorf("%w: meta_annotations key %q is reserved (triple/agent_id/traceparent/tracestate and io.modelcontextprotocol/-prefixed keys are runtime-stamped)", ErrOAuthBinding, k)
		}
	}
	if ms.OAuthProvider == "" {
		return nil, nil
	}
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
	prov, ok := providers.Get(ms.OAuthProvider)
	if !ok {
		return nil, fmt.Errorf("%w: unknown provider %q (registered: %s)", ErrOAuthBinding, ms.OAuthProvider, strings.Join(providers.Names(), ","))
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
		return nil, fmt.Errorf("%w: provider %q declares no allowed_downstream_hosts — a bearer-injecting provider must declare its downstream sinks (fail-closed; the credential-plane invariant)", ErrOAuthBinding, ms.OAuthProvider)
	}
	connHost := config.NormalizeDownstreamHost(ms.URL)
	if connHost == "" || !hostAllowed(allowed, connHost) {
		return nil, fmt.Errorf("%w: connection host %q is not in provider %q's allowed_downstream_hosts — the credential may only be injected into a boot-declared downstream sink", ErrOAuthBinding, connHost, ms.OAuthProvider)
	}
	return prov, nil
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
