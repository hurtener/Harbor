package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
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
	// implicit env passthrough)" — brief 03 §4.
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
	// DefaultPolicy is the ToolPolicy applied to descriptors built
	// from this provider. Zero-valued → tools.DefaultPolicy().
	DefaultPolicy tools.ToolPolicy
	// ToolPolicies are per-tool ToolPolicy overrides keyed by the
	// MCP server-side tool name (NOT the `<source>_<tool>` Harbor
	// name). When a discovered tool's name is present here, its
	// descriptor uses the override instead of DefaultPolicy; a tool
	// absent from the map falls back to DefaultPolicy (Phase 26b).
	//
	// Concurrent reuse (D-025): the map is read-only after New — it is
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
	// Phase 83m (Item 1, D-156): the role narrows. For events the
	// SDK delivers WITH a populated ctx (per-call notifications
	// originating from an inflight tool / resource subscription),
	// the driver prefers `identity.From(ctx)` over this cached
	// default — `pushIdentity(ctx, cfg)` is the single helper that
	// implements the preference. The DefaultIdentity remains the
	// fallback for genuine transport-level events (a server-pushed
	// `notifications/resources/updated` arriving outside any
	// inflight call) where the ctx has no triple to read.
	DefaultIdentity identity.Identity
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
// Phase 83m / D-156 (Item 1). Mirrors the §6 isolation rule: per-
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
// server. Safe for N concurrent goroutines after Connect (D-025);
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
	p.client = mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: implementationName, Version: implementationVersion},
		&mcpsdk.ClientOptions{
			Logger:                 cfg.Logger,
			KeepAlive:              cfg.KeepAlive,
			ResourceUpdatedHandler: p.onResourceUpdated,
		},
	)
	return p, nil
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
	// further and may grow the slice.
	out := make([]tools.ToolDescriptor, 0, len(toolsRes.Tools))
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
		out = append(out, desc)
	}

	// Resources.
	resRes, err := session.ListResources(ctx, nil)
	if err == nil && resRes != nil {
		for _, r := range resRes.Resources {
			if r == nil || r.URI == "" {
				continue
			}
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
			out = append(out, p.buildPromptDescriptor(pr))
		}
	}

	return out, nil
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

	// Phase 107c step 10/11 audit: provider-native tool-calling
	// requires the wire-side function name to match
	// `^[a-zA-Z0-9_-]{1,128}$` (OpenAI spec; OpenRouter→Bedrock
	// enforces strictly). The original separator was `.` which fails
	// validation — `youtube.get_metadata` → OpenRouter 400. Single
	// underscore is spec-safe AND matches the operator-facing naming
	// users already expect (see media-helper-agent harbor.yaml's
	// `planning_hints.preferred_tools: [youtube_get_metadata, ...]`).
	// The double-underscore `__` separator stays reserved for
	// resources / prompts (`__resource.` / `__prompt.` markers).
	// Phase 26b — per-tool policy override. A tool named in
	// p.cfg.ToolPolicies (keyed by the server-side MCP tool name)
	// uses that policy; otherwise the per-server DefaultPolicy
	// applies. The lookup happens BEFORE the Invoke closure captures
	// tool.Policy below, so the configured budget governs every
	// attempt. The map is read-only after New (D-025).
	policy := p.cfg.DefaultPolicy
	if override, ok := p.cfg.ToolPolicies[t.Name]; ok {
		policy = override
	}
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
	invoke := func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.RunWithPolicy(
			ctx,
			args,
			func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return p.callTool(ctx, mcpName, args)
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
// the descriptor's Invoke closure under the ToolPolicy shell.
func (p *Provider) callTool(ctx context.Context, name string, args json.RawMessage) (tools.ToolResult, error) {
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
	meta, err := buildIdentityMeta(ctx)
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
		return tools.ToolResult{}, fmt.Errorf("%w: call %q: %w", ErrTransportFailed, name, err)
	}
	value, lowerErr := lowerCallToolResult(res)
	if lowerErr != nil {
		return tools.ToolResult{Value: value}, lowerErr
	}
	return tools.ToolResult{Value: value}, nil
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
				meta, mErr := buildIdentityMeta(ctx)
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
				meta, mErr := buildIdentityMeta(ctx)
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
	meta, mErr := buildIdentityMeta(ctx)
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
// side events that arrive without an inflight call. Phase 83m
// (Item 1, D-156) narrows the role of the cached default to that
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
// AGENTS.md §6 rule 9 / forbidden practice §13: identity is
// mandatory. Missing identity returns `ErrIdentityMissing` and the
// caller MUST abort the dispatch — never proceed with an empty Meta.
// Callers are the per-invocation closures (callTool / read-resource
// / call-prompt); the server-pushed onResourceUpdated path uses
// `Config.DefaultIdentity` and bypasses this helper.
func buildIdentityMeta(ctx context.Context) (mcpsdk.Meta, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return nil, ErrIdentityMissing
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return nil, ErrIdentityMissing
	}
	return mcpsdk.Meta{
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
	}, nil
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
