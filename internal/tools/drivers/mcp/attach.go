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
	"fmt"
	"log/slog"
	"strings"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
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
