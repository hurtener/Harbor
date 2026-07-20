package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrInvalidConfig — operator-side misconfiguration: missing URL
	// for HTTP-flavoured transports, missing Command for stdio,
	// unknown transport mode, etc.
	ErrInvalidConfig = errors.New("mcp: invalid config")
	// ErrTransportFailed — every candidate transport failed at
	// Connect time. The wrapped causes preserve the per-transport
	// failure messages.
	ErrTransportFailed = errors.New("mcp: transport failed")
	// ErrNotConnected — Discover / Invoke / SubscribeResource called
	// before Connect (or after Close).
	ErrNotConnected = errors.New("mcp: provider not connected")
	// ErrMCPToolError — the server returned a CallToolResult with
	// IsError == true. The wrapped message carries the rendered
	// text body.
	ErrMCPToolError = errors.New("mcp: server returned tool error")
	// ErrSchemaInvalid — the server-advertised InputSchema failed to
	// compile; the descriptor is rejected at Discover time so the
	// catalog never holds a Tool whose Validate is broken.
	ErrSchemaInvalid = errors.New("mcp: invalid tool input schema")
	// ErrIdentityMissing — the per-invocation ctx had no identity
	// triple. AGENTS.md §6 rule 9: identity is mandatory; the MCP
	// driver fails closed rather than dispatching to a remote server
	// with an empty `_meta` block. Mirrors the HTTP and A2A drivers.
	ErrIdentityMissing = errors.New("mcp: identity missing from ctx")
	// ErrEmptyBearer — a bound OAuth provider returned a nil error AND
	// an empty access token. Defence-in-depth on the fail-closed
	// surface: the call aborts rather than proceeding unauthenticated
	// on a connection the operator declared per-identity-authorized.
	ErrEmptyBearer = errors.New("mcp: oauth provider returned an empty bearer token")
	// ErrAmbiguousOAuthBinding — a per-tool `oauth_provider` binding key
	// addresses more than ONE MCP surface on this server at once (e.g. both a
	// tool AND a prompt of the same name, or a resource whose URI equals a tool
	// name). The per-tool binding map is a single per-entry namespace keyed by
	// MCP-side name; when a key would bind more than one surface the operator's
	// intent is ambiguous, so the binding is rejected LOUD at discovery rather
	// than silently resolved by an undocumented precedence.
	ErrAmbiguousOAuthBinding = errors.New("mcp: ambiguous oauth_provider binding (key addresses more than one MCP surface)")
)

// resourceTypeSeparator — used in the synthetic tool names for
// MCP resources / prompts: `<sourceID>__resource.<uri>`. The "__"
// is a documented marker that operators avoid in their tool names
// (collision-free; see plan §"Risks / open questions").
const (
	resourceTypeSeparator = "__"
	resourceNamePrefix    = "resource."
	promptNamePrefix      = "prompt."
)

// implementationName / implementationVersion identify Harbor as the
// MCP client to the remote server in the initialize handshake.
// These are operator-stable so multi-server logs can attribute
// requests back to Harbor unambiguously.
const (
	implementationName    = "harbor-runtime"
	implementationVersion = "v0"
)

// Config is the operator-supplied configuration for one MCP
// attachment. Operator-facing fields map 1:1 to the
// `config.MCPServerConfig` yaml shape; the runtime entry point
// (cmd/harbor wiring, future phase) is responsible for the
// projection.
type Config struct {
	// Name is the unique source ID prefix. Empty rejects with
	// ErrInvalidConfig.
	Name string
	// TransportMode selects the wire transport. Empty defaults to
	// TransportAuto.
	TransportMode MCPTransportMode
	// URL is the endpoint for SSE / streamable-HTTP transports.
	// Required for those modes.
	URL string
	// Command is the argv-form subprocess command for the stdio
	// transport. [0] is the binary; [1:] are args. Required for
	// stdio. NEVER shell-form — the driver enforces this in
	// transport_stdio.go.
	Command []string
	// Headers are operator-supplied HTTP headers added to every
	// SSE / streamable-HTTP request (auth tokens, custom auth).
	// "URL connections require explicit headers for auth (no
	// implicit env passthrough)" — a settled security rule.
	Headers map[string]string
	// KeepAlive is the ping interval for the MCP session; zero
	// disables. The SDK's KeepAlive runs the underlying ping/pong.
	KeepAlive time.Duration
	// Logger is the per-provider slog logger. nil → a discard
	// logger; runtime never panics on absent Logger.
	Logger *slog.Logger
	// Bus is the event bus used to publish `mcp.resource_updated`
	// notifications. Required.
	Bus events.EventBus
	// ToolContext captures the tool context (input + lowered result) behind
	// a declared MCP App so the host can deliver it to the rendered app
	// (the MCP Apps "Data Delivery" lifecycle). Optional — a nil capturer
	// means tool-context delivery is not wired (the host's tool-context
	// read then returns not-found). When set, the driver calls it from the
	// tool-invocation path whenever a result declares a `ui://` app; a
	// Capture error is logged loudly but never fails the tool call.
	ToolContext ToolContextCapturer
	// DefaultPolicy is the ToolPolicy applied to descriptors built
	// from this provider. Zero-valued → tools.DefaultPolicy().
	DefaultPolicy tools.ToolPolicy
	// ToolPolicies are per-tool ToolPolicy overrides keyed by the
	// MCP server-side tool name (NOT the `<source>_<tool>` Harbor
	// name). When a discovered tool's name is present here, its
	// descriptor uses the override instead of DefaultPolicy; a tool
	// absent from the map falls back to DefaultPolicy.
	// Per-tool overrides apply to TOOLS only — MCP resources and
	// prompts always run under DefaultPolicy (the per-server default).
	//
	// Concurrent reuse: the map is read-only after New — it is
	// never mutated per-run. buildToolDescriptor only reads it, and
	// the resolved ToolPolicy is copied by value into each descriptor
	// at Discover time, so concurrent invocations of different tools
	// never share or race this map.
	ToolPolicies map[string]tools.ToolPolicy
	// DefaultIdentity is the fallback identity stamped on
	// transport-side events (notifications that arrive without an
	// inflight call). Required so the bus's ValidateEvent does not
	// reject the event when the SDK-supplied ctx carries no triple.
	//
	// the role narrows. For events the
	// SDK delivers WITH a populated ctx (per-call notifications
	// originating from an inflight tool / resource subscription),
	// the driver prefers `identity.From(ctx)` over this cached
	// default — `pushIdentity(ctx, cfg)` is the single helper that
	// implements the preference. The DefaultIdentity remains the
	// fallback for genuine transport-level events (a server-pushed
	// `notifications/resources/updated` arriving outside any
	// inflight call) where the ctx has no triple to read.
	DefaultIdentity identity.Identity

	// HostDisplayModes lists the MCP App (`io.modelcontextprotocol/ui`)
	// display modes this host can render. When non-empty, the provider
	// advertises the UI extension during the MCP initialize handshake with
	// these modes (filtered against the closed valid-mode set), so a server
	// can tailor the app references it returns to what the host actually
	// renders. Empty leaves the SDK's default capability advertisement
	// untouched (no UI extension) — the behaviour for an embedder that does
	// not opt in. The boot loader sources this from the deployment-level
	// `tools.mcp_app_host.display_modes` config (defaulting to inline), but
	// a programmatic embedder may set it directly. Read once at construction;
	// immutable thereafter.
	HostDisplayModes []string

	// OAuthProvider, when non-nil, binds this connection to a per-identity
	// OAuth credential source: every identity-stamped per-call RPC resolves a
	// fresh bearer via Token(ctx, source) and injects Authorization on THAT
	// request only. Nil leaves the connection on its static Headers (the
	// unbound default). Resolved once at construction from the operator's
	// non-secret provider NAME (config `oauth_provider`) against the declared
	// provider registry — the name selects an acquisition strategy; the
	// secret stays on the provider. Immutable after construction: no per-call
	// transport state mutates (the token rides the call's ctx). A connect-time
	// request (initialize / discovery) predates any per-call identity and so
	// carries only the static Headers — a documented limitation.
	OAuthProvider auth.OAuthProvider

	// MetaAnnotations is a static, non-secret set of operator-declared
	// key/values merged verbatim into the `_meta` map on every
	// identity-stamped per-call RPC, so a deployment can carry its own
	// attribution vocabulary to a shared server. Reserved keys (the triple
	// keys, `agent_id`, `traceparent`, `tracestate`, and any
	// `io.modelcontextprotocol/`-prefixed key) are rejected at config /
	// attach validation and can never shadow the triple or agent provenance
	// (those are stamped last). Read once at construction; immutable
	// thereafter.
	MetaAnnotations map[string]string

	// OnAuthChallenge, when non-nil, is invoked whenever an MCP HTTP call to
	// this server answers `401` with a `WWW-Authenticate` Bearer challenge —
	// the MCP authorization spec's OAuth step-up. The callback records the
	// advertised OAuth requirement on the connection's registry state so an
	// operator can inspect it. Capture is pure observation: it never
	// retries, never attaches credentials, and never alters the call's error
	// semantics. Optional; nil disables challenge capture (stdio connections
	// never set it — the challenge is an HTTP-auth construct). Read once at
	// construction; immutable thereafter.
	OnAuthChallenge func(AuthChallenge)

	// OnScopeShortfall, when non-nil, is invoked whenever an MCP HTTP call
	// answers `403` with a `WWW-Authenticate` marking
	// `error="insufficient_scope"` (RFC 6750 §3.1) — a downstream step-up
	// scope shortfall. The callback records the parsed shortfall on the
	// connection's registry state so an operator can inspect the last
	// observed required-vs-granted gap. Best-effort observability: it never
	// retries, never widens a binding, and never alters the call's error
	// semantics (the per-call error enrichment rides a request-scoped ctx
	// slot instead). Optional; nil disables the registry-side record (stdio
	// connections never set it). Read once at construction; immutable
	// thereafter.
	OnScopeShortfall func(ScopeShortfall)

	// ToolOAuthProviders are per-entry OAuth-provider overrides keyed by the
	// MCP-side name (mirroring ToolPolicies' shape). An entry named here binds
	// THAT provider for every identity-stamped RPC that addresses by the entry's
	// key — a CallTool by tool name, a ReadResource / SubscribeResource by
	// resource URI, and a GetPrompt by prompt name; an unlisted key falls back
	// to OAuthProvider (the connection-level binding). Each entry was resolved +
	// validated against the same binding rules as OAuthProvider at attach time
	// (unknown name / stdio transport / static-Authorization conflict /
	// downstream-host allow-list). The map is a single per-entry namespace; a
	// key that would address more than one surface at once (a tool AND a prompt
	// of the same name) is rejected loud at discovery (ErrAmbiguousOAuthBinding),
	// never silently resolved by precedence. Read-only after New; the resolved
	// provider is read per-call from the call's own addressing key, so concurrent
	// invocations of different tools / resources / prompts never share or race
	// this map (the concurrent-reuse contract).
	ToolOAuthProviders map[string]auth.OAuthProvider
}

// pushIdentity returns the identity to stamp on a server-pushed
// event. Prefers `identity.From(ctx)` so per-call subscriptions
// (subscriptions registered under a real (tenant, user, session)
// triple) keep the inflight caller's identity on the resulting
// event — multi-tenant operators get correct provenance instead of
// a cached single-triple stamp. Falls back to the configured
// `DefaultIdentity` for transport-side events (notifications
// arriving without an inflight call); `Config.validate()` ensures
// that fallback is fully populated so the bus never sees an empty
// triple.
//
// (Item 1). Mirrors the §6 isolation rule: per-
// call identity beats a cached default whenever both are present.
func pushIdentity(ctx context.Context, cfg Config) identity.Identity {
	if ctx != nil {
		if id, ok := identity.From(ctx); ok {
			if id.TenantID != "" && id.UserID != "" && id.SessionID != "" {
				return id
			}
		}
	}
	return cfg.DefaultIdentity
}

// validate checks Config invariants. Used by New + tests.
func (c Config) validate() error {
	if c.Name == "" {
		return fmt.Errorf("%w: Name is empty", ErrInvalidConfig)
	}
	if c.Bus == nil {
		return fmt.Errorf("%w: Bus is required (used by mcp.resource_updated)", ErrInvalidConfig)
	}
	if c.TransportMode != "" && !isValidMode(c.TransportMode) {
		return fmt.Errorf("%w: unknown TransportMode %q", ErrInvalidConfig, c.TransportMode)
	}
	mode := c.TransportMode
	if mode == "" {
		mode = TransportAuto
	}
	switch mode {
	case TransportSSE, TransportStreamableHTTP:
		if c.URL == "" {
			return fmt.Errorf("%w: %s transport requires URL", ErrInvalidConfig, mode)
		}
	case TransportStdio:
		if len(c.Command) == 0 {
			return fmt.Errorf("%w: stdio transport requires Command (argv form)", ErrInvalidConfig)
		}
		if c.Command[0] == "" {
			return fmt.Errorf("%w: stdio Command[0] (binary path) is empty", ErrInvalidConfig)
		}
	case TransportAuto:
		if c.URL == "" && len(c.Command) == 0 {
			return fmt.Errorf("%w: auto mode requires URL or Command", ErrInvalidConfig)
		}
	}
	// Identity for server-pushed events: a fully-populated default
	// is mandatory because the event bus rejects empty-triple events.
	if c.DefaultIdentity.TenantID == "" || c.DefaultIdentity.UserID == "" || c.DefaultIdentity.SessionID == "" {
		return fmt.Errorf("%w: DefaultIdentity must be fully populated (tenant/user/session)", ErrInvalidConfig)
	}
	return nil
}

// Provider implements tools.ToolProvider against a remote MCP
// server. Safe for N concurrent goroutines after Connect;
// per-call state lives on the call's ctx, never on the Provider.
//
// Concurrent reuse contract:
//   - `session` is set once by Connect under mu; subsequent reads
//     are guarded by mu.RLock so Invoke / Discover / Close races
//     are safe.
//   - `closed` flips to true on Close; subsequent Invoke / Discover
//     return ErrNotConnected.
//   - The resource-update goroutine reads `session` once at Connect
//     time and exits when the session closes.
type Provider struct {
	cfg    Config
	logger *slog.Logger
	source tools.ToolSourceID
	client *mcpsdk.Client

	mu      sync.RWMutex
	session *mcpsdk.ClientSession
	closed  bool

	// selectedMode is the actual transport mode chosen by
	// selectTransport — useful for tests and observability.
	selectedMode MCPTransportMode
}

// New constructs a Provider. The Provider is NOT connected; the
// caller MUST call Connect before Discover / Invoke /
// SubscribeResource.
func New(cfg Config) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.TransportMode == "" {
		cfg.TransportMode = TransportAuto
	}
	p := &Provider{
		cfg:    cfg,
		logger: cfg.Logger,
		source: tools.ToolSourceID(cfg.Name),
	}
	// The client is constructed eagerly so subscriptions /
	// notification handlers attached at New time survive the
	// re-Connect cycle a ToolPolicy retry might trigger.
	clientOpts := &mcpsdk.ClientOptions{
		Logger:                 cfg.Logger,
		KeepAlive:              cfg.KeepAlive,
		ResourceUpdatedHandler: p.onResourceUpdated,
	}
	// Advertise the MCP App (`io.modelcontextprotocol/ui`) host capability
	// during the initialize handshake. Harbor always hosts apps via the
	// Console, so the capability is advertised unconditionally — a
	// conformant server gates its `ui://` tool registration on the spec
	// `mimeTypes` field this carries.
	clientOpts.Capabilities = hostCapabilities()
	p.client = mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: implementationName, Version: implementationVersion},
		clientOpts,
	)
	return p, nil
}

// hostCapabilities builds the client capabilities advertised during the MCP
// initialize handshake. The host can render MCP App
// (`io.modelcontextprotocol/ui`) documents, so it advertises the UI extension
// carrying the spec `mimeTypes` field — the field a conformant ext-apps
// server reads (its `getUiCapability(caps).mimeTypes` gate) to decide whether
// to register its `ui://` tools. The value is the single canonical MCP App
// media type, ResourceMIMEType, mirroring the ext-apps SDK constant.
//
// Display modes are NOT a capability field (the spec carries the host's
// renderable modes in the `ui/initialize` host-context `availableDisplayModes`,
// surfaced to the Console via runtime.info). This builder therefore advertises
// only `mimeTypes`.
//
// Setting any client capability overrides the SDK's default advertisement
// (`{"roots":{"listChanged":true}}`), and the SDK ignores the deprecated
// `Roots` field in favour of `RootsV2`. So this MUST replicate the current
// roots advertisement explicitly — otherwise opting into the UI extension
// would silently drop the roots capability the runtime advertises today.
// Sampling and elicitation remain inferred from their handlers (unset here).
func hostCapabilities() *mcpsdk.ClientCapabilities {
	caps := &mcpsdk.ClientCapabilities{
		RootsV2: &mcpsdk.RootCapabilities{ListChanged: true},
	}
	// McpUiClientCapabilities is `{ "mimeTypes": [...] }` — a conformant
	// server checks the array includes ResourceMIMEType before registering
	// its app tools.
	settings := map[string]any{"mimeTypes": []string{ResourceMIMEType}}
	caps.AddExtension(uiExtensionKey, settings)
	return caps
}

// filterHostDisplayModes returns the host-advertised display modes that are in
// the closed valid-mode set, with duplicates removed and advertised order
// preserved, so the host reports only modes it can actually render. It is the
// projection DisplayModes() returns for the Registry / Console column.
func filterHostDisplayModes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, mode := range in {
		if _, valid := validDisplayModes[mode]; !valid {
			continue
		}
		if _, dup := seen[mode]; dup {
			continue
		}
		seen[mode] = struct{}{}
		out = append(out, mode)
	}
	return out
}

// SourceID returns the source ID under which this provider's
// descriptors are stamped. Implements tools.ToolProvider.
func (p *Provider) SourceID() tools.ToolSourceID {
	return p.source
}

// SelectedMode reports the transport mode that succeeded at
// Connect time. Empty before Connect.
func (p *Provider) SelectedMode() MCPTransportMode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.selectedMode
}

// Connect establishes the MCP session. Calling Connect twice
// without an interleaving Close returns the existing session (Connect
// is idempotent on the second call only when the first succeeded).
//
// Auto-mode fallback: when TransportMode is TransportAuto and the
// URL is set, the Provider tries streamable-HTTP first. On a
// non-cancellation failure of `client.Connect` (which covers both
// transport-Connect and the MCP initialize handshake), it retries
// with SSE.
func (p *Provider) Connect(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrNotConnected
	}
	if p.session != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	transport, mode, err := selectTransport(p.cfg)
	if err != nil {
		return err
	}
	session, firstErr := p.client.Connect(ctx, transport, nil)
	if firstErr == nil {
		p.mu.Lock()
		p.session = session
		p.selectedMode = mode
		p.mu.Unlock()
		return nil
	}

	// Auto-mode fallback: a URL+streamable-HTTP first try that
	// failed, when the operator did not pin TransportMode, retries
	// with SSE. Explicit modes do NOT fall back.
	autoFallback := (p.cfg.TransportMode == "" || p.cfg.TransportMode == TransportAuto) &&
		mode == TransportStreamableHTTP &&
		p.cfg.URL != "" &&
		classifyConnectError(firstErr)
	if !autoFallback {
		return fmt.Errorf("%w: %w", ErrTransportFailed, firstErr)
	}

	sseTransport := newSSETransport(p.cfg)
	session, sseErr := p.client.Connect(ctx, sseTransport, nil)
	if sseErr != nil {
		return fmt.Errorf("%w: streamable-http failed (%w); sse failed (%w)",
			ErrTransportFailed, firstErr, sseErr)
	}
	p.logger.Info("mcp: auto-fallback streamable-http -> sse",
		slog.String("source", string(p.source)),
		slog.String("primary_error", firstErr.Error()),
	)
	p.mu.Lock()
	p.session = session
	p.selectedMode = TransportSSE
	p.mu.Unlock()
	return nil
}

// session returns the active session under read-lock, or
// ErrNotConnected.
func (p *Provider) sessionForRead() (*mcpsdk.ClientSession, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, ErrNotConnected
	}
	if p.session == nil {
		return nil, ErrNotConnected
	}
	return p.session, nil
}

// Discover returns one ToolDescriptor per remote tool, plus one per
// resource (rendered as a `__resource.<uri>` tool) and one per
// prompt (`__prompt.<name>`). All descriptors carry Transport =
// TransportMCP and Source = p.source.
func (p *Provider) Discover(ctx context.Context) ([]tools.ToolDescriptor, error) {
	session, err := p.sessionForRead()
	if err != nil {
		return nil, err
	}

	// Tools.
	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: list tools: %w", ErrTransportFailed, err)
	}

	// Capacity is seeded from the tool count; resources/prompts append
	// further and may grow the slice. The three per-surface key sets are
	// collected alongside so the per-entry OAuth-binding namespace can be
	// checked for a cross-surface collision once discovery is complete.
	out := make([]tools.ToolDescriptor, 0, len(toolsRes.Tools))
	toolNames := make(map[string]struct{}, len(toolsRes.Tools))
	resourceURIs := make(map[string]struct{})
	promptNames := make(map[string]struct{})
	for _, t := range toolsRes.Tools {
		if t == nil {
			continue
		}
		desc, err := p.buildToolDescriptor(t)
		if err != nil {
			// A schema-compile failure on one tool MUST NOT poison the
			// whole catalog — log and skip. Other tools may still be
			// usable.
			p.logger.Warn("mcp: skipping tool with invalid schema",
				slog.String("source", string(p.source)),
				slog.String("tool", t.Name),
				slog.String("error", err.Error()),
			)
			continue
		}
		toolNames[t.Name] = struct{}{}
		out = append(out, desc)
	}

	// Resources.
	resRes, err := session.ListResources(ctx, nil)
	if err == nil && resRes != nil {
		for _, r := range resRes.Resources {
			if r == nil || r.URI == "" {
				continue
			}
			resourceURIs[r.URI] = struct{}{}
			out = append(out, p.buildResourceDescriptor(r))
		}
	}
	// ListResources returning method-not-found is benign — the
	// server simply doesn't expose resources.

	// Prompts.
	prRes, err := session.ListPrompts(ctx, nil)
	if err == nil && prRes != nil {
		for _, pr := range prRes.Prompts {
			if pr == nil || pr.Name == "" {
				continue
			}
			promptNames[pr.Name] = struct{}{}
			out = append(out, p.buildPromptDescriptor(pr))
		}
	}

	// A per-entry OAuth binding key must address exactly ONE surface. A key
	// matching a tool AND a prompt (or a resource URI equal to a tool name)
	// is ambiguous — fail the discovery loud so the boot / runtime-add attach
	// fails rather than silently binding the credential to a surface the
	// operator did not intend.
	if err := p.checkOAuthBindingAmbiguity(toolNames, resourceURIs, promptNames); err != nil {
		return nil, err
	}

	return out, nil
}

// checkOAuthBindingAmbiguity fails loud when a per-entry `tool_oauth_providers`
// key addresses more than one discovered MCP surface at once (a tool, a resource
// URI, and a prompt name share the single per-entry namespace). It is the
// discovery-time half of AC-3: a cross-surface key collision is rejected with
// ErrAmbiguousOAuthBinding, never resolved by an undocumented precedence. An
// unbound connection (no per-entry map) is a no-op.
func (p *Provider) checkOAuthBindingAmbiguity(toolNames, resourceURIs, promptNames map[string]struct{}) error {
	if len(p.cfg.ToolOAuthProviders) == 0 {
		return nil
	}
	for key := range p.cfg.ToolOAuthProviders {
		kinds := make([]string, 0, 3)
		if _, ok := toolNames[key]; ok {
			kinds = append(kinds, "tool")
		}
		if _, ok := resourceURIs[key]; ok {
			kinds = append(kinds, "resource")
		}
		if _, ok := promptNames[key]; ok {
			kinds = append(kinds, "prompt")
		}
		if len(kinds) > 1 {
			return fmt.Errorf("%w: key %q addresses %s on server %q — rename one surface or bind them to distinct keys",
				ErrAmbiguousOAuthBinding, key, strings.Join(kinds, "+"), p.source)
		}
	}
	return nil
}

// buildToolDescriptor maps an MCP Tool into a Harbor ToolDescriptor.
// The InputSchema is compiled to a `tools.Validate`; on compile
// failure, ErrSchemaInvalid is returned (the caller skips this
// tool — see Discover).
//
// The Invoke closure captures `p.session` lazily (via
// `sessionForRead`) so a ToolPolicy-driven retry that follows a
// reconnect transparently uses the new session.
func (p *Provider) buildToolDescriptor(t *mcpsdk.Tool) (tools.ToolDescriptor, error) {
	schemaBytes, err := marshalSchema(t.InputSchema)
	if err != nil {
		return tools.ToolDescriptor{}, fmt.Errorf("%w: %w", ErrSchemaInvalid, err)
	}
	outSchemaBytes, _ := marshalSchema(t.OutputSchema) //nolint:errcheck // OutputSchema is optional; a marshal failure yields no out-schema

	// step 10/11 audit: provider-native tool-calling
	// requires the wire-side function name to match
	// `^[a-zA-Z0-9_-]{1,128}$` (OpenAI spec; OpenRouter→Bedrock
	// enforces strictly). The original separator was `.` which fails
	// validation — `youtube.get_metadata` → OpenRouter 400. Single
	// underscore is spec-safe AND matches the operator-facing naming
	// users already expect (see media-helper-agent harbor.yaml's
	// `planning_hints.preferred_tools: [youtube_get_metadata, ...]`).
	// The double-underscore `__` separator stays reserved for
	// resources / prompts (`__resource.` / `__prompt.` markers).
	// per-tool policy override. A tool named in
	// p.cfg.ToolPolicies (keyed by the server-side MCP tool name)
	// uses that policy; otherwise the per-server DefaultPolicy
	// applies. The lookup happens BEFORE the Invoke closure captures
	// tool.Policy below, so the configured budget governs every
	// attempt. The map is read-only after New.
	policy := p.cfg.DefaultPolicy
	if override, ok := p.cfg.ToolPolicies[t.Name]; ok {
		policy = override
	}
	// Loading defaults every injected tool-form descriptor to LoadingAlways —
	// the driver DEFAULT, not a dead end: the boot config's
	// `tools.entries[].loading_mode` and, at runtime, the agent-config
	// tool-exposure loading-mode overrides both sit ABOVE this default in
	// the pinned precedence order (see agentcfg.ToolExposure godoc). Form
	// stays zero-valued (ToolFormTool) — only the resource/prompt wrappers
	// below stamp a non-default Form.
	tool := tools.Tool{
		Name:        fmt.Sprintf("%s_%s", string(p.source), t.Name),
		Description: t.Description,
		ArgsSchema:  schemaBytes,
		OutSchema:   outSchemaBytes,
		SideEffects: deriveSideEffect(t.Annotations),
		Source:      p.source,
		Transport:   tools.TransportMCP,
		Policy:      policy,
		Loading:     tools.LoadingAlways,
	}

	mcpName := t.Name
	// MCP App binding: the canonical `io.modelcontextprotocol/ui` dialect
	// carries the `ui://` UI resource on the tool DEFINITION's
	// `_meta.ui.resourceUri` slot. Capture it here, at discovery — this is
	// the spec-conformant source of the app reference. A real ext-apps
	// server (e.g. one that advertises a `ui://…/index.html` per tool)
	// returns an EMPTY result `_meta`, so reading the binding from the
	// result alone never fires discovery. The binding is immutable after
	// discovery and captured by value into the Invoke closure — the
	// concurrent-reuse contract: no shared mutable per-run state on the
	// Provider.
	toolApp := parseAppRef(t.Meta)
	invoke := func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(
			ctx,
			args,
			func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return p.callTool(ctx, mcpName, args, toolApp)
			},
			nil, // server-side schema validates on the wire; client-side compiled
			nil, // output validator is optional.
			tool.Policy,
		)
	}
	return tools.ToolDescriptor{
		Tool:     tool,
		Invoke:   invoke,
		Validate: nil, // schemas live on the server side; the wire does the validation.
	}, nil
}

// marshalSchema converts a SDK-side InputSchema (any) into a
// json.RawMessage suitable for the Harbor Tool surface. Returns nil
// for nil input. The SDK populates the field with a map[string]any
// on the client side; marshalling round-trips fine.
func marshalSchema(s any) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	bytes, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(bytes), nil
}

// deriveSideEffect inspects MCP ToolAnnotations to pick the closest
// Harbor SideEffect class. Read-only → SideEffectRead; destructive
// → SideEffectExternal; idempotent → SideEffectRead; default
// SideEffectExternal (MCP tools cross a process boundary).
func deriveSideEffect(a *mcpsdk.ToolAnnotations) tools.SideEffect {
	if a == nil {
		return tools.SideEffectExternal
	}
	if a.ReadOnlyHint {
		return tools.SideEffectRead
	}
	if a.IdempotentHint {
		return tools.SideEffectRead
	}
	return tools.SideEffectExternal
}

// callTool dispatches a CallTool RPC and lowers the result. Used by
// the descriptor's Invoke closure under the ToolPolicy shell. toolApp is
// the tool-DEFINITION MCP App binding captured at discovery (nil for a
// non-app tool); it is reconciled with any per-result `_meta.ui` hint to
// produce the effective app reference on the result.
func (p *Provider) callTool(ctx context.Context, name string, args json.RawMessage, toolApp *AppRef) (tools.ToolResult, error) {
	session, err := p.sessionForRead()
	if err != nil {
		return tools.ToolResult{}, err
	}
	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return tools.ToolResult{}, fmt.Errorf("%w: decode args: %w", tools.ErrToolInvalidArgs, err)
		}
	}
	meta, err := buildIdentityMeta(ctx, p.cfg.MetaAnnotations)
	if err != nil {
		return tools.ToolResult{}, err
	}
	// Thread a per-call scope-shortfall slot onto ctx BEFORE dispatch: the
	// challenge-capturing transport writes a parsed `403` + insufficient_scope
	// step-up here, and this call reads it after CallTool returns to enrich the
	// SAME call's error. Per-run state on ctx, never provider-level state.
	slot := &scopeShortfallSlot{}
	ctx = withScopeShortfallSlot(ctx, slot)
	// Fail-closed per-identity bearer: on a bound connection, resolve the
	// token (per-tool override, falling back to the connection binding) and
	// thread it onto the call's ctx BEFORE dispatch. A Token() failure aborts
	// here — no wire request is issued.
	ctx, err = p.resolveBearerCtx(ctx, name)
	if err != nil {
		return tools.ToolResult{}, err
	}
	params := &mcpsdk.CallToolParams{
		Name:      name,
		Arguments: argMap,
	}
	params.Meta = meta
	res, err := session.CallTool(ctx, params)
	if err != nil {
		// When the transport observed a `403` + insufficient_scope step-up on
		// this exact call, upgrade the opaque transport error to the typed,
		// structured *tools.ErrInsufficientScope so the tool-result error path
		// carries the required-vs-granted gap and classification stops
		// retrying a shortfall retry can never fix.
		if sf, granted := slot.get(); sf != nil {
			// Enrich the connection-view record with the tool name the
			// transport-side capture could not know (the transport already
			// recorded a tool-less shortfall; last-write-wins upgrades it).
			if p.cfg.OnScopeShortfall != nil {
				named := *sf
				named.ToolName = name
				named.GrantedScopes = granted
				p.cfg.OnScopeShortfall(named)
			}
			return tools.ToolResult{}, &tools.ErrInsufficientScope{
				Source:             p.source,
				ToolName:           name,
				DownstreamResource: sf.DownstreamResource,
				RequiredScopes:     sf.RequiredScopes,
				GrantedScopes:      granted,
				WWWAuthenticate:    sf.WWWAuthenticate,
				Origin:             sf.Origin,
			}
		}
		return tools.ToolResult{}, fmt.Errorf("%w: call %q: %w", ErrTransportFailed, name, err)
	}
	value, lowerErr := lowerCallToolResult(res)
	// MCP App discovery: the effective app reference is the tool-DEFINITION
	// binding (the spec-conformant source of the `ui://` resource URI,
	// captured at discovery), reconciled with any optional per-result
	// `_meta.ui` hint (which wins for display mode only). The reconciled
	// reference feeds BOTH the discovery event below AND the app-tool-call
	// proxy projection, which reads value.AppRef. When the invoked tool has
	// a UI binding, surface a discovery event so a Protocol client can mount
	// the inline renderer for this turn. This is the planner-path projection
	// of the app reference — the proxy path reuses the same value.AppRef on
	// its response, but a planner-initiated call never enters that path.
	value.AppRef = reconcileAppRef(toolApp, value.AppRef, uiDisplayModeHint(res.Meta))
	if value.AppRef != nil {
		// Mint the stable per-invocation id (a content hash of run /
		// server / tool / args — no mutable Provider field) and
		// stamp it on the reference so BOTH the discovery event and the
		// proxy-path projection (which reads value.AppRef) correlate to the
		// captured tool context.
		runID := ""
		if quad, qok := identity.QuadrupleFrom(ctx); qok {
			runID = quad.RunID
		}
		value.AppRef.ToolCallID = ToolCallID(runID, string(p.source), name, args)
		p.captureToolContext(ctx, value.AppRef.ToolCallID, name, args, value, res.IsError)
		p.publishAppAvailable(ctx, value.AppRef, name)
	}
	if lowerErr != nil {
		return tools.ToolResult{Value: value}, lowerErr
	}
	return tools.ToolResult{Value: value}, nil
}

// publishAppAvailable emits the MCP App discovery event for an invoked
// tool that declared a `ui://` app on its definition. The event is scoped to the inflight
// caller's identity quadruple (its RunID correlates the discovery to the
// turn). The emit is best-effort observability — the tool result is the
// source of truth — so a missing identity or a publish failure logs and
// returns rather than failing the call.
func (p *Provider) publishAppAvailable(ctx context.Context, ref *AppRef, toolName string) {
	if p.cfg.Bus == nil || ref == nil {
		return
	}
	id, ok := identity.From(ctx)
	if !ok {
		// Identity is mandatory for the call itself (buildIdentityMeta
		// rejects a missing triple earlier); a result here without one is
		// not reachable, but the bus rejects empty-triple events, so guard.
		return
	}
	q := identity.Quadruple{Identity: id}
	if quad, qok := identity.QuadrupleFrom(ctx); qok {
		q.RunID = quad.RunID
	}
	ev := events.Event{
		Type:       EventTypeMCPAppAvailable,
		Identity:   q,
		OccurredAt: time.Now(),
		Payload: AppAvailablePayload{
			Identity:    q,
			ServerID:    p.source,
			ToolCallID:  ref.ToolCallID,
			ToolName:    toolName,
			ResourceURI: ref.ResourceURI,
			DisplayMode: ref.PreferredDisplayMode,
			// Default-deny: the per-server trust posture is reconciled by
			// the client via mcp.servers.get.
			RawHTMLTrusted: false,
			OccurredAt:     time.Now(),
		},
	}
	if err := p.cfg.Bus.Publish(ctx, ev); err != nil {
		p.logger.Warn("mcp: publish app_available failed",
			slog.String("source", string(p.source)),
			slog.String("resource_uri", ref.ResourceURI),
			slog.String("error", err.Error()),
		)
	}
}

// captureToolContext persists the tool context (input + lowered result)
// behind a declared app so a Protocol client can fetch it via
// mcp.apps.tool_context. The capture is optional (a nil capturer means
// the feature is not wired) and best-effort relative to the tool call: a
// missing identity or a Capture error is logged loudly and returns —
// never silently swallowed (CLAUDE.md §13) — but it does not fail the tool
// call, because the tool result is the source of truth for the planner.
func (p *Provider) captureToolContext(ctx context.Context, toolCallID, toolName string, args json.RawMessage, value MCPToolValue, isError bool) {
	if p.cfg.ToolContext == nil {
		return
	}
	if _, ok := identity.From(ctx); !ok {
		p.logger.Warn("mcp: app tool-context capture skipped: no identity on ctx",
			slog.String("source", string(p.source)),
			slog.String("tool", toolName),
		)
		return
	}
	resultJSON, err := json.Marshal(value)
	if err != nil {
		p.logger.Warn("mcp: app tool-context capture failed: encode result",
			slog.String("source", string(p.source)),
			slog.String("tool", toolName),
			slog.String("error", err.Error()),
		)
		return
	}
	in := CapturedToolContext{
		ServerID:   p.source,
		ToolCallID: toolCallID,
		Tool:       toolName,
		Input:      args,
		Result:     json.RawMessage(resultJSON),
		IsError:    isError,
	}
	if capErr := p.cfg.ToolContext.Capture(ctx, in); capErr != nil {
		p.logger.Warn("mcp: app tool-context capture failed",
			slog.String("source", string(p.source)),
			slog.String("tool", toolName),
			slog.String("tool_call_id", toolCallID),
			slog.String("error", capErr.Error()),
		)
	}
}

// buildResourceDescriptor wraps an MCP Resource as a one-shot
// `read_resource`-style tool. Invocation accepts no arguments (the
// resource URI is captured in the closure). Identity scopes the
// `_meta` field on the read call.
func (p *Provider) buildResourceDescriptor(r *mcpsdk.Resource) tools.ToolDescriptor {
	toolName := fmt.Sprintf("%s%s%s%s", string(p.source), resourceTypeSeparator, resourceNamePrefix, r.URI)
	tool := tools.Tool{
		Name:        toolName,
		Description: chooseString(r.Description, fmt.Sprintf("Read MCP resource %s", r.URI)),
		ArgsSchema:  emptyArgsSchema(),
		Source:      p.source,
		Transport:   tools.TransportMCP,
		Policy:      p.cfg.DefaultPolicy,
		SideEffects: tools.SideEffectRead,
		Loading:     tools.LoadingDeferred,
		Form:        tools.ToolFormResource,
	}
	uri := r.URI
	invoke := func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(
			ctx, args,
			func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				session, err := p.sessionForRead()
				if err != nil {
					return tools.ToolResult{}, err
				}
				params := &mcpsdk.ReadResourceParams{URI: uri}
				meta, mErr := buildIdentityMeta(ctx, p.cfg.MetaAnnotations)
				if mErr != nil {
					return tools.ToolResult{}, mErr
				}
				// Per-entry binding: the resource URI is the addressing key, so a
				// per-tool `oauth_provider` override declared for this resource
				// resolves its bearer; an unbound resource falls back to the
				// connection-level binding.
				ctx, mErr = p.resolveBearerCtx(ctx, uri)
				if mErr != nil {
					return tools.ToolResult{}, mErr
				}
				params.Meta = meta
				res, err := session.ReadResource(ctx, params)
				if err != nil {
					return tools.ToolResult{}, fmt.Errorf("%w: read %q: %w", ErrTransportFailed, uri, err)
				}
				return tools.ToolResult{Value: lowerReadResourceResult(res)}, nil
			},
			nil, nil, tool.Policy,
		)
	}
	return tools.ToolDescriptor{Tool: tool, Invoke: invoke}
}

// buildPromptDescriptor wraps an MCP Prompt as a one-shot
// `get_prompt`-style tool. Arguments are forwarded to the server
// as a map[string]string.
func (p *Provider) buildPromptDescriptor(pr *mcpsdk.Prompt) tools.ToolDescriptor {
	toolName := fmt.Sprintf("%s%s%s%s", string(p.source), resourceTypeSeparator, promptNamePrefix, pr.Name)
	tool := tools.Tool{
		Name:        toolName,
		Description: chooseString(pr.Description, fmt.Sprintf("Get MCP prompt %s", pr.Name)),
		ArgsSchema:  promptArgsSchema(pr),
		Source:      p.source,
		Transport:   tools.TransportMCP,
		Policy:      p.cfg.DefaultPolicy,
		SideEffects: tools.SideEffectRead,
		Loading:     tools.LoadingDeferred,
		Form:        tools.ToolFormPrompt,
	}
	name := pr.Name
	invoke := func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(
			ctx, args,
			func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
				session, err := p.sessionForRead()
				if err != nil {
					return tools.ToolResult{}, err
				}
				var argMap map[string]string
				if len(args) > 0 {
					if err := json.Unmarshal(args, &argMap); err != nil {
						return tools.ToolResult{}, fmt.Errorf("%w: decode prompt args: %w", tools.ErrToolInvalidArgs, err)
					}
				}
				params := &mcpsdk.GetPromptParams{Name: name, Arguments: argMap}
				meta, mErr := buildIdentityMeta(ctx, p.cfg.MetaAnnotations)
				if mErr != nil {
					return tools.ToolResult{}, mErr
				}
				// Per-entry binding: the prompt name is the addressing key, so a
				// per-tool `oauth_provider` override declared for this prompt
				// resolves its bearer; an unbound prompt falls back to the
				// connection-level binding.
				ctx, mErr = p.resolveBearerCtx(ctx, name)
				if mErr != nil {
					return tools.ToolResult{}, mErr
				}
				params.Meta = meta
				res, err := session.GetPrompt(ctx, params)
				if err != nil {
					return tools.ToolResult{}, fmt.Errorf("%w: get prompt %q: %w", ErrTransportFailed, name, err)
				}
				return tools.ToolResult{Value: lowerGetPromptResult(res)}, nil
			},
			nil, nil, tool.Policy,
		)
	}
	return tools.ToolDescriptor{Tool: tool, Invoke: invoke}
}

// ReadResource fetches a single resource's content from the remote MCP
// server under the request identity triple. It is the runtime-side leg
// of the `mcp.servers.read_resource` Protocol method: the Console reads
// an MCP App's `ui://` UI document through it (the URI is validated to
// the `ui://` scheme by the caller; ReadResource itself is scheme-
// agnostic so the same path serves any resource read).
//
// The returned bytes are the first resource-content item's payload —
// Text rendered as UTF-8 bytes, or the raw Blob when the server sent a
// binary resource. The MIME type is the content item's declared type.
// Identity is mandatory: a ctx without a full (tenant, user, session)
// triple fails closed with ErrIdentityMissing (the `_meta` builder
// rejects it) — never a read with an empty `_meta` block.
//
// Concurrent reuse: ReadResource holds no per-call state on the
// Provider; identity + the URI ride the call.
func (p *Provider) ReadResource(ctx context.Context, uri string) (content []byte, mimeType string, err error) {
	session, sErr := p.sessionForRead()
	if sErr != nil {
		return nil, "", sErr
	}
	meta, mErr := buildIdentityMeta(ctx, p.cfg.MetaAnnotations)
	if mErr != nil {
		return nil, "", mErr
	}
	// Per-entry binding: the resource URI is the addressing key so a per-tool
	// override declared for this resource resolves its bearer (connection-level
	// fallback when unbound).
	ctx, mErr = p.resolveBearerCtx(ctx, uri)
	if mErr != nil {
		return nil, "", mErr
	}
	params := &mcpsdk.ReadResourceParams{URI: uri}
	params.Meta = meta
	res, rErr := session.ReadResource(ctx, params)
	if rErr != nil {
		return nil, "", fmt.Errorf("%w: read %q: %w", ErrTransportFailed, uri, rErr)
	}
	if res == nil || len(res.Contents) == 0 {
		return nil, "", fmt.Errorf("%w: resource %q returned no content", ErrTransportFailed, uri)
	}
	rc := res.Contents[0]
	if rc == nil {
		return nil, "", fmt.Errorf("%w: resource %q returned a nil content item", ErrTransportFailed, uri)
	}
	if len(rc.Blob) > 0 {
		return append([]byte(nil), rc.Blob...), rc.MIMEType, nil
	}
	return []byte(rc.Text), rc.MIMEType, nil
}

// DisplayModes returns the MCP App display modes THIS HOST can render —
// the deployment's configured host modes (`Config.HostDisplayModes`,
// sourced from `tools.mcp_app_host.display_modes`), filtered against the
// closed valid-mode set with duplicates removed and order preserved. It is
// NOT a server read: display modes are not a spec capability field — they
// ride the `ui/initialize` host-context `availableDisplayModes` the host
// dictates — so the Registry / Console column reports what the host
// renders, not a value scraped off the server's capabilities.
//
// Concurrent reuse: the configured modes are immutable after construction;
// DisplayModes reads no per-call or session state.
func (p *Provider) DisplayModes() []string {
	return filterHostDisplayModes(p.cfg.HostDisplayModes)
}

// uiExtensionKey is the canonical capability key the MCP Apps
// (ext-apps) extension is advertised under during the MCP initialize
// handshake.
const uiExtensionKey = "io.modelcontextprotocol/ui"

// ResourceMIMEType is the single canonical media type an MCP App
// (`io.modelcontextprotocol/ui`) UI document carries. It mirrors the
// ext-apps SDK's `RESOURCE_MIME_TYPE` constant; the host advertises it in
// the `mimeTypes` UI capability so a conformant server gates its `ui://`
// tool registration on it.
const ResourceMIMEType = "text/html;profile=mcp-app"

// validDisplayModes is the closed set of MCP Apps display modes
// (ext-apps `McpUiDisplayMode`). Negotiation filters a server's
// advertised modes against this set so an unknown value never reaches
// the projection.
var validDisplayModes = map[string]struct{}{
	"inline":     {},
	"fullscreen": {},
	"pip":        {},
}

// SubscribeResource registers a server-side resource subscription.
// Updates received via the SDK's ResourceUpdatedHandler are
// published as `mcp.resource_updated` on the configured event bus
// (see Provider.onResourceUpdated).
func (p *Provider) SubscribeResource(ctx context.Context, uri string) error {
	session, err := p.sessionForRead()
	if err != nil {
		return err
	}
	params := &mcpsdk.SubscribeParams{URI: uri}
	meta, mErr := buildIdentityMeta(ctx, p.cfg.MetaAnnotations)
	if mErr != nil {
		return mErr
	}
	// Per-entry binding: the resource URI is the addressing key so a per-tool
	// override declared for this resource resolves its bearer (connection-level
	// fallback when unbound).
	ctx, mErr = p.resolveBearerCtx(ctx, uri)
	if mErr != nil {
		return mErr
	}
	params.Meta = meta
	if err := session.Subscribe(ctx, params); err != nil {
		return fmt.Errorf("%w: subscribe %q: %w", ErrTransportFailed, uri, err)
	}
	return nil
}

// onResourceUpdated is the SDK handler for incoming
// notifications/resources/updated. It publishes a typed
// `mcp.resource_updated` event on the configured bus.
//
// Concurrency: the SDK invokes the handler on its own goroutine;
// the bus.Publish + event construction is allocation-only, so no
// shared mutable state is touched.
//
// The published event's Identity prefers `identity.From(ctx)` when
// the SDK-supplied ctx carries a populated (tenant, user, session)
// triple — per-call subscriptions stamp the inflight caller's
// identity on the resulting event so multi-tenant operators see
// correct provenance instead of a cached single-triple stamp.
// Falls back to `Config.DefaultIdentity` for genuine transport-
// side events that arrive without an inflight call. A cleanup pass
// (Item 1) narrows the role of the cached default to that
// fallback path; the prior shape stamped every push with the
// cached default regardless of the ctx's contents.
func (p *Provider) onResourceUpdated(ctx context.Context, req *mcpsdk.ResourceUpdatedNotificationRequest) {
	if req == nil || req.Params == nil {
		return
	}
	if p.cfg.Bus == nil {
		// Defensive — Config.validate() rejects nil Bus, but if
		// someone bypassed New the early return prevents a nil
		// dereference.
		return
	}
	q := identity.Quadruple{Identity: pushIdentity(ctx, p.cfg)}
	ev := events.Event{
		Type:       EventTypeMCPResourceUpdated,
		Identity:   q,
		OccurredAt: time.Now(),
		Payload: ResourceUpdatedPayload{
			Identity:   q,
			Source:     p.source,
			URI:        req.Params.URI,
			OccurredAt: time.Now(),
		},
	}
	if err := p.cfg.Bus.Publish(ctx, ev); err != nil {
		p.logger.Warn("mcp: publish resource_updated failed",
			slog.String("source", string(p.source)),
			slog.String("uri", req.Params.URI),
			slog.String("error", err.Error()),
		)
	}
}

// Close shuts the session down idempotently and joins any
// in-flight SDK goroutines. Safe to call multiple times.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	session := p.session
	p.session = nil
	p.mu.Unlock()
	if session == nil {
		return nil
	}
	if err := session.Close(); err != nil {
		return fmt.Errorf("mcp: close session: %w", err)
	}
	return nil
}

// buildIdentityMeta builds the `_meta` map the MCP wire format
// uses for caller-controlled metadata. Harbor stamps the
// (tenant, user, session) triple under `tenant` / `user` /
// `session` keys so MCP servers see Harbor's isolation triple.
//
// Provenance enrichment: when the ctx carries an acting-agent
// registration id (the run loop's provenance seam), it is stamped under
// `agent_id` — PROVENANCE ONLY, never an isolation principal (a server
// MUST NOT key isolation on it). Operator-declared `annotations` are
// merged verbatim for the deployment's own attribution vocabulary.
// Reserved keys (the triple, `agent_id`, `traceparent`, `tracestate`,
// and any `io.modelcontextprotocol/`-prefixed key) are rejected at
// validation; this merge re-enforces the invariant by stamping the
// triple + agent LAST and skipping any reserved/spec-prefixed annotation
// key, so an annotation can never shadow identity even if one slipped
// past validation ("impossible by construction" defence-in-depth).
//
// AGENTS.md §6 rule 9 / forbidden practice §13: identity is
// mandatory. Missing identity returns `ErrIdentityMissing` and the
// caller MUST abort the dispatch — never proceed with an empty Meta.
// Callers are the per-invocation closures (callTool / read-resource
// / call-prompt); the server-pushed onResourceUpdated path uses
// `Config.DefaultIdentity` and bypasses this helper.
func buildIdentityMeta(ctx context.Context, annotations map[string]string) (mcpsdk.Meta, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return nil, ErrIdentityMissing
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return nil, ErrIdentityMissing
	}
	meta := make(mcpsdk.Meta, len(annotations)+4)
	// Operator annotations first, so the triple + agent stamps below win
	// unconditionally (a reserved-key annotation can never shadow identity).
	for k, v := range annotations {
		if isReservedMetaKey(k) {
			continue
		}
		meta[k] = v
	}
	meta["tenant"] = id.TenantID
	meta["user"] = id.UserID
	meta["session"] = id.SessionID
	if agentID, present := tools.InvokingAgentFrom(ctx); present {
		meta["agent_id"] = agentID
	}
	return meta, nil
}

// isReservedMetaKey reports whether k is a Harbor-reserved or spec-reserved
// `_meta` key that an operator annotation must not carry. It delegates to
// the single shared authority (config.IsReservedMCPMetaKey — the isolation
// triple, `agent_id`, the trace-context carrier keys, and the
// `io.modelcontextprotocol/` spec namespace) so the driver's merge-time
// re-check can never drift from config / attach validation.
func isReservedMetaKey(k string) bool {
	return config.IsReservedMCPMetaKey(k)
}

// resolveBearerProvider selects the OAuth provider that authenticates an RPC
// addressed by key. A non-empty key — a CallTool tool name, a ReadResource /
// SubscribeResource resource URI, or a GetPrompt prompt name — resolves the
// per-entry override map, falling back to the connection-level OAuthProvider
// when the key is unlisted; an empty key always resolves the connection-level
// OAuthProvider. The map is read-only after construction, so this per-call read
// never races.
func (p *Provider) resolveBearerProvider(key string) auth.OAuthProvider {
	if key != "" {
		if prov, ok := p.cfg.ToolOAuthProviders[key]; ok {
			return prov
		}
	}
	return p.cfg.OAuthProvider
}

// resolveBearerCtx threads a fresh per-identity bearer onto the call's ctx
// when this connection binds an OAuth provider for the addressing key, so the
// context-aware bearerInjectingTransport sets Authorization on the outbound
// request. Provider selection is per-entry: a non-empty key (a CallTool tool
// name, a ReadResource / SubscribeResource resource URI, or a GetPrompt prompt
// name) resolves the per-entry override map (falling back to the
// connection-level provider); an empty key always resolves the connection-level
// provider.
//
// It is FAIL-CLOSED: a Token() error (including a typed *auth.ErrAuthRequired
// the runtime catches to park the run on the unified pause/resume primitive)
// aborts the RPC by returning the error BEFORE any wire request — never a
// fallback to an unauthenticated call (CLAUDE.md §13). An unbound connection
// (no provider) returns the ctx unchanged.
//
// When a per-call scope-shortfall slot rides ctx, the resolved token's
// granted scopes are recorded on it so a subsequent insufficient-scope
// step-up can report the required-vs-granted gap. No Provider or transport
// field mutates: the bearer rides the returned ctx (per-call state lives on
// the ctx, never on the compiled artifact — the concurrent-reuse contract).
func (p *Provider) resolveBearerCtx(ctx context.Context, key string) (context.Context, error) {
	provider := p.resolveBearerProvider(key)
	if provider == nil {
		return ctx, nil
	}
	tok, err := provider.Token(ctx, p.source)
	if err != nil {
		// Propagate unwrapped so errors.As reaches a typed *auth.ErrAuthRequired.
		return nil, err
	}
	if tok.AccessToken == "" {
		// Defence-in-depth: a ("", nil) return from a bound provider must
		// never let the RPC proceed unauthenticated (unreachable via the
		// shipped drivers, which reject empty tokens — but this is the
		// fail-closed surface, so it fails loud here too).
		return nil, fmt.Errorf("%w (connection %q, source %q)", ErrEmptyBearer, p.cfg.Name, p.source)
	}
	if slot := scopeShortfallSlotFrom(ctx); slot != nil {
		slot.setGranted(tok.Scopes)
	}
	return withBearer(ctx, tok.AccessToken), nil
}

// chooseString returns first when non-empty, else second.
func chooseString(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// emptyArgsSchema returns the JSON-Schema for "object with no
// fields, no additionalProperties". Used for resource read
// descriptors that take no arguments.
func emptyArgsSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}

// promptArgsSchema builds a JSON-Schema object for the prompt's
// declared arguments. PromptArgument.Required → schema.required.
// Argument values are strings (per MCP spec).
func promptArgsSchema(pr *mcpsdk.Prompt) json.RawMessage {
	props := map[string]any{}
	required := make([]string, 0, len(pr.Arguments))
	for _, a := range pr.Arguments {
		if a == nil || a.Name == "" {
			continue
		}
		props[a.Name] = map[string]any{
			"type":        "string",
			"description": a.Description,
		}
		if a.Required {
			required = append(required, a.Name)
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		// schema is built from string/bool/map values only, so Marshal
		// cannot fail in practice; fall back to the empty schema rather
		// than returning a nil RawMessage.
		return emptyArgsSchema()
	}
	return b
}
