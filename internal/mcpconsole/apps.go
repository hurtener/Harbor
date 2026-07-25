package mcpconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// AppsAccessor is the runtime-side adapter that satisfies BOTH the
// protocol.MCPResourceReader and protocol.AppToolInvoker seams the
// AppsSurface reads through — the MCP Apps host's two methods.
//
// It bridges the driver-free `protocol` package to the runtime concretes
// it must not import directly (CLAUDE.md §13): the MCP driver registry
// (for `ui://` resource reads) and the tool catalog (for the app-tool-
// call proxy's re-entry into the approval / OAuth / identity invocation
// path). The heavy-content safety net (RFC §6.5) lives here too:
// content at or above the configured threshold is routed through the
// ArtifactStore by reference, with a loud `mcp.resource_offloaded`
// bypass event — never inlined past the threshold, never silently
// truncated.
//
// A `ui://` MCP App document is the one carve-out: it is a Console-render
// payload, not LLM context, so it rides inline up to the dedicated, much
// larger App-document cap instead of the LLM-context heavy threshold —
// see appDocumentInlineCap. Above that cap it still offloads (the same
// loud bypass), so a pathologically large App is never inlined unbounded.
//
// Concurrent reuse: AppsAccessor is an immutable adapter — the wrapped
// registry / catalog / store / bus are themselves concurrent-safe
// compiled artifacts, and the adapter adds no mutable state. Per-call
// identity rides ctx.
type AppsAccessor struct {
	reg      *mcp.Registry
	cat      tools.ToolCatalog
	store    artifacts.ArtifactStore
	bus      events.EventBus
	toolCtx  *ToolContextStore
	agentCfg agentcfg.Registry // optional — nil ⇒ the app-call exposure gate is inert
	agentID  string            // the agent slot the gate reads current desired-state from
	// sessionOverlay is the session-safe narrow-only disable set the gate
	// UNIONS into the admin exposure (parity with the run-start planner-view
	// projection, which unions it too). Optional — nil ⇒ only the admin
	// exposure gates. Keyed by the ctx (tenant, user, session) triple.
	sessionOverlay sessionoverlay.Store
	threshold      int
	clock          func() time.Time
}

// AppsDeps bundles the runtime collaborators the AppsAccessor bridges.
// Registry, Catalog, Store, and Bus are mandatory — a nil fails closed
// at construction (CLAUDE.md §5). Threshold ≤ 0 falls back to the shared
// heavy-output default; Clock defaults to time.Now.
type AppsDeps struct {
	// Registry resolves `ui://` resource reads against the live MCP
	// providers. Mandatory.
	Registry *mcp.Registry
	// Catalog resolves the app-tool-call proxy target — the SAME catalog
	// a planner resolves against, so the approval gate + tool-side OAuth
	// fire on the proxy path. Mandatory.
	Catalog tools.ToolCatalog
	// Store is the ArtifactStore the heavy-content branch offloads to.
	// Mandatory.
	Store artifacts.ArtifactStore
	// Bus carries the `mcp.resource_offloaded` loud bypass event.
	// Mandatory.
	Bus events.EventBus
	// ToolContext is the tool-context store backing `mcp.apps.tool_context`
	// — the same store the MCP providers capture through, so a discovered
	// app's context is readable here. Mandatory.
	ToolContext *ToolContextStore
	// AgentConfig is the agent-config desired-state registry the app→host
	// call gate reads CURRENT exposure from (the planner-snapshot /
	// app-call-current asymmetry): an App callback to a tool whose server is
	// currently paused — or whose tool is currently disabled — is rejected,
	// while an in-flight planner snapshot is undisturbed. Optional: a nil
	// registry (or empty AgentID) leaves the gate inert (backward
	// compatible). The transport is NEVER consulted — pause is desired-state,
	// not transport state; the live transport stays warm.
	AgentConfig agentcfg.Registry
	// AgentID is the agent slot the gate reads desired-state from. Required
	// when AgentConfig is set; ignored when it is nil.
	AgentID string
	// SessionOverlay is the session-safe narrow-only disable set the app→host
	// gate UNIONS into the admin exposure, so a tool a SESSION user disabled
	// (via agent_config.session.set_source_disables) is also rejected from an
	// App callback — parity with the run-start planner-view projection, which
	// unions the same overlay. Optional: nil leaves only the admin exposure
	// gating. Keyed by the ctx (tenant, user, session) triple.
	SessionOverlay sessionoverlay.Store
	// Threshold is the heavy-content threshold in bytes. ≤ 0 falls back
	// to artifacts' shared default.
	Threshold int
	// Clock returns the current wall-clock time. Optional — defaults to
	// time.Now.
	Clock func() time.Time
}

// ErrAppsMisconfigured — NewAppsAccessor was called with a missing
// mandatory dependency. Fails closed.
var ErrAppsMisconfigured = errors.New("mcpconsole: AppsAccessor missing a mandatory dependency")

// ErrAppToolExposureDenied — an App app→host tool call targeted a tool
// whose MCP server is currently paused, or a tool that is currently
// disabled, in the agent's active config. It is an authorization rejection
// (mapped to CodeScopeMismatch at the wire edge), the functional basis for
// the operator-legible "paused by a system administrator" overlay. Its
// message carries the stable "paused or disabled by agent configuration"
// marker the Protocol edge classifies on (the `protocol` package does not
// import this package, mirroring the existing not-found / identity-missing
// markers).
var ErrAppToolExposureDenied = errors.New("mcpconsole: tool unavailable — paused or disabled by agent configuration")

// NewAppsAccessor builds the MCP Apps host adapter. Registry, Catalog,
// Store, and Bus are mandatory; a nil fails loud.
func NewAppsAccessor(deps AppsDeps) (*AppsAccessor, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: Registry is nil", ErrAppsMisconfigured)
	}
	if deps.Catalog == nil {
		return nil, fmt.Errorf("%w: Catalog is nil", ErrAppsMisconfigured)
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: Store is nil", ErrAppsMisconfigured)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Bus is nil", ErrAppsMisconfigured)
	}
	if deps.ToolContext == nil {
		return nil, fmt.Errorf("%w: ToolContext is nil", ErrAppsMisconfigured)
	}
	threshold := deps.Threshold
	if threshold <= 0 {
		threshold = defaultHeavyThreshold
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &AppsAccessor{
		reg:            deps.Registry,
		cat:            deps.Catalog,
		store:          deps.Store,
		bus:            deps.Bus,
		toolCtx:        deps.ToolContext,
		agentCfg:       deps.AgentConfig,
		agentID:        deps.AgentID,
		sessionOverlay: deps.SessionOverlay,
		threshold:      threshold,
		clock:          clock,
	}, nil
}

// defaultHeavyThreshold is the heavy-output floor applied when the
// operator-configured threshold is unset. Single-sourced on the
// canonical config default so the unconfigured fallback can never drift
// from the runtime-wide heavy-output boundary.
const defaultHeavyThreshold = config.DefaultHeavyOutputThresholdBytes

// appDocumentInlineCap is the inline byte ceiling for a `ui://` MCP App
// document fetched through ReadResource. It is DELIBERATELY larger than
// the heavy-output threshold (defaultHeavyThreshold, 32 KiB) because the
// two ceilings guard different things:
//
//   - The heavy-output threshold (RFC §6.5) keeps bulky bytes OUT of the
//     LLM context window — a large tool result inlined into the next
//     prompt balloons the context and risks a context-window overflow.
//   - An MCP App document NEVER enters the LLM context. The tool result
//     carries only the tiny `_meta.ui.resourceUri` reference string; the
//     `ui://` HTML itself is fetched ONLY by the Console (via
//     mcp.servers.read_resource) and rendered in a sandboxed iframe. It
//     is a render payload, not a context payload.
//
// Gating an App document on the LLM-context threshold offloads ordinary
// real-world apps (a studio App's HTML routinely runs 80–100 KiB) to the
// ArtifactStore by reference — and a by-reference App can only be fetched
// via a presigned URL, which every non-S3 artifact driver fails loud on,
// so the App never renders on the in-memory / filesystem / SQLite /
// Postgres stores. The dedicated, larger cap lets the common case ride
// inline on EVERY driver. Above the cap the read still offloads (the loud
// `mcp.resource_offloaded` bypass) so a pathologically large App is never
// inlined unbounded and never silently truncated.
const appDocumentInlineCap = 2 * 1024 * 1024 // 2 MiB

// compile-time assertions: AppsAccessor satisfies the three Apps seams.
var (
	_ protocol.MCPResourceReader    = (*AppsAccessor)(nil)
	_ protocol.AppToolInvoker       = (*AppsAccessor)(nil)
	_ protocol.AppToolContextReader = (*AppsAccessor)(nil)
)

// ReadResource implements protocol.MCPResourceReader. It fetches the
// resource bytes from the MCP registry under the ctx identity triple,
// then applies the inline-or-offload decision against an effective cap:
// a `ui://` MCP App document — a Console-render payload that never enters
// the LLM context — rides inline up to the dedicated App-document cap
// (appDocumentInlineCap); any other resource keeps the LLM-context heavy
// threshold. Content at or above the effective cap is offloaded to the
// ArtifactStore by reference (with a loud `mcp.resource_offloaded`
// event); below it, the content rides inline.
func (a *AppsAccessor) ReadResource(ctx context.Context, serverID, resourceURI string) (protocol.MCPResourceContent, error) {
	content, mime, err := a.reg.ReadResource(ctx, serverID, resourceURI)
	if err != nil {
		// Translate the driver's not-found sentinel into the Protocol's so the
		// wire edge classifies by errors.Is (see markNotFound).
		return protocol.MCPResourceContent{}, markNotFound(err)
	}
	out := protocol.MCPResourceContent{ResourceURI: resourceURI, MIMEType: mime}
	// A `ui://` App document is fetched only by the Console and rendered in
	// a sandboxed iframe — it never reaches the LLM, so the LLM-context
	// heavy threshold does not apply. It rides inline up to the larger
	// App-document cap so real-world apps render on every artifact driver,
	// not just S3. An ordinary resource keeps the heavy-output threshold.
	inlineCap := a.threshold
	if mcp.IsUIResourceURI(resourceURI) {
		inlineCap = appDocumentInlineCap
	}
	if len(content) < inlineCap {
		out.Inline = content
		return out, nil
	}
	row, offErr := a.offload(ctx, content, mime, resourceURI, resourceURI)
	if offErr != nil {
		return protocol.MCPResourceContent{}, offErr
	}
	out.Artifact = row
	return out, nil
}

// CallTool implements protocol.AppToolInvoker. It resolves the named tool
// from the SAME catalog a planner resolves against and invokes the SAME
// wrapped descriptor — so the approval gate (the unified pause primitive)
// + tool-side OAuth fire exactly as on a planner call. The result is
// heavy-content-disciplined (the context-window safety net); a `ui://`
// MCP App the invoked tool declared (on its tool-definition `_meta.ui`,
// reconciled by the driver onto the result's app reference) is projected
// onto the App field.
func (a *AppsAccessor) CallTool(ctx context.Context, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	desc, ok := a.cat.Resolve(tool)
	if !ok {
		// Wrap the Protocol's not-found SENTINEL alongside the catalog's, so
		// the wire edge classifies this as CodeNotFound on an assertion THIS
		// accessor makes ("I resolved it and it is absent") rather than on the
		// wording of an error chain a southbound server can contribute to. The
		// App renders a not-found as a permanent verdict, so it must not be
		// reachable by a transient transport failure that happens to phrase
		// itself the same way.
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: %w: %q",
			protocol.ErrAccessorNotFound, tools.ErrToolNotFound, tool)
	}
	if desc.Invoke == nil {
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("mcpconsole: tool %q registered without an Invoke function", tool)
	}
	// The app→host current-state gate (the planner-snapshot / app-call-
	// current asymmetry): an App callback is a NEW invocation fired after
	// the run, so it is gated against the CURRENT agent-config desired
	// state — NOT the run's stale snapshot, and NOT the transport's
	// online/warm state (a paused server stays connected). Once an admin
	// pauses the tool's server (or disables the tool), the App's callbacks
	// are rejected here, before any side effect, while any still-running
	// planner snapshot is undisturbed.
	if err := a.gateToolExposure(ctx, tool, desc.Tool.Source); err != nil {
		return protocol.MCPAppToolResultRow{}, err
	}
	// Invoke the wrapped descriptor. For a gated tool this parks on the
	// unified pause primitive inside the approval wrapper before any side
	// effect — the proxy is a re-entry, not a bypass.
	res, err := desc.Invoke(ctx, args)
	out := protocol.MCPAppToolResultRow{Tool: tool}
	if err != nil {
		// A server-side tool error (IsError) is surfaced as a result with
		// IsError=true, NOT a Protocol error — the app renders it. Every
		// other error (approval reject, OAuth-required, transport failure,
		// missing identity) propagates so the gate's verdict is honoured.
		if errors.Is(err, mcp.ErrMCPToolError) {
			out.IsError = true
		} else {
			return protocol.MCPAppToolResultRow{}, err
		}
	}
	out.App = appRefFromValue(res.Value, string(desc.Tool.Source))
	encoded, encErr := json.Marshal(res.Value)
	if encErr != nil {
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("mcpconsole: encode tool %q result: %w", tool, encErr)
	}
	if len(encoded) < a.threshold {
		out.Inline = json.RawMessage(encoded)
		return out, nil
	}
	row, offErr := a.offload(ctx, encoded, "application/json", tool, fmt.Sprintf("app-tool-result-%s.json", tool))
	if offErr != nil {
		return protocol.MCPAppToolResultRow{}, offErr
	}
	out.Artifact = row
	return out, nil
}

// gateToolExposure rejects an app→host tool call whose MCP source server is
// in the agent's CURRENT active-revision paused set, or whose tool name is
// in the current disabled set. It reads desired-state from the agent-config
// registry — never the run snapshot, never the transport state (the live
// transport stays warm while paused). The gate is INERT when no registry /
// agent id is wired (backward compatible). Identity is mandatory: a ctx
// without a triple fails closed. A registry read error fails the call loud
// (no silent fall-through, CLAUDE.md §13).
func (a *AppsAccessor) gateToolExposure(ctx context.Context, toolName string, source tools.ToolSourceID) error {
	if a.agentCfg == nil || a.agentID == "" {
		return nil
	}
	id, ok := identity.From(ctx)
	if !ok || id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return fmt.Errorf("mcpconsole: app-call exposure gate: %w", mcp.ErrIdentityMissing)
	}
	rev, has, err := a.agentCfg.Active(ctx, identity.Quadruple{Identity: id}, a.agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return fmt.Errorf("mcpconsole: app-call exposure gate: read active config: %w", err)
	}
	// The effective exposure is the admin desired-state UNION the session
	// overlay's narrow-only disables — the SAME composition the run-start
	// planner-view projection applies. A tool/server the SESSION disabled must
	// also be rejected from an App callback, else the two views of "currently
	// exposed" diverge.
	var pausedServers, disabledTools []string
	if has && rev.Payload.ToolExposure != nil {
		pausedServers = rev.Payload.PausedServers()
		disabledTools = rev.Payload.DisabledTools()
	}
	if a.sessionOverlay != nil {
		overlay, _, oerr := a.sessionOverlay.Get(ctx, identity.Quadruple{Identity: id}, a.agentID)
		if oerr != nil {
			return fmt.Errorf("mcpconsole: app-call exposure gate: read session overlay: %w", oerr)
		}
		pausedServers = append(pausedServers, overlay.DisabledServers...)
		disabledTools = append(disabledTools, overlay.DisabledTools...)
	}
	if source != "" {
		for _, s := range pausedServers {
			if tools.ToolSourceID(s) == source {
				return fmt.Errorf("%w: tool %q is on server %q, which is paused", ErrAppToolExposureDenied, toolName, source)
			}
		}
	}
	for _, n := range disabledTools {
		if n == toolName {
			return fmt.Errorf("%w: tool %q is disabled", ErrAppToolExposureDenied, toolName)
		}
	}
	return nil
}

// ToolContext implements protocol.AppToolContextReader. It resolves the
// tool context (input + lowered result) a captured MCP App call produced,
// scoped to the ctx identity triple, projecting each half inline or by
// reference exactly as ReadResource does. An unknown or cross-identity
// (serverID, toolCallID) is not found. It delegates to the same
// ToolContextStore the MCP providers capture through.
func (a *AppsAccessor) ToolContext(ctx context.Context, serverID, toolCallID string) (protocol.AppToolContextRow, error) {
	return a.toolCtx.Load(ctx, serverID, toolCallID)
}

// offload stores heavy content in the ArtifactStore under the ctx
// identity scope and emits the loud `mcp.resource_offloaded` bypass
// event. The event is the load-bearing record that the content was
// routed by reference, never inlined past the threshold and never
// silently truncated (RFC §6.5). A store or emit failure fails
// the call loud — there is no silent truncation fallback (CLAUDE.md §13).
func (a *AppsAccessor) offload(ctx context.Context, content []byte, mime, source, filename string) (*protocol.MCPResourceArtifactRow, error) {
	return offloadHeavy(ctx, a.store, a.bus, a.clock, content, mime, source, filename)
}

// offloadHeavy stores heavy content in the ArtifactStore under the ctx
// identity scope and emits the loud `mcp.resource_offloaded` bypass event.
// It is the shared offload path the AppsAccessor (resource reads + proxy
// results) and the ToolContextStore (captured tool contexts) route heavy
// content through — one loud-bypass implementation, never two. The event
// is the load-bearing record that the content was routed by reference,
// never inlined past the threshold and never silently truncated
// (RFC §6.5). A store or emit failure fails the call loud — there is no
// silent truncation fallback (CLAUDE.md §13).
func offloadHeavy(ctx context.Context, store artifacts.ArtifactStore, bus events.EventBus, clock func() time.Time, content []byte, mime, source, filename string) (*protocol.MCPResourceArtifactRow, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return nil, fmt.Errorf("mcpconsole: offload: %w", mcp.ErrIdentityMissing)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	ref, putErr := store.PutBytes(ctx, scope, content, artifacts.PutOpts{
		MimeType:  mime,
		Filename:  filename,
		Namespace: "mcp-apps",
		Source: map[string]any{
			"source":     "mcp",
			"producer":   "mcp-apps",
			"created_at": clock().UTC(),
		},
	})
	if putErr != nil {
		return nil, fmt.Errorf("mcpconsole: offload heavy content (%d bytes) to artifact store: %w", len(content), putErr)
	}
	q := identity.Quadruple{Identity: id}
	ev := events.Event{
		Type:       mcp.EventTypeMCPResourceOffloaded,
		Identity:   q,
		OccurredAt: clock(),
		Payload: mcp.ResourceOffloadedPayload{
			Identity:   q,
			ArtifactID: ref.ID,
			Source:     source,
			SizeBytes:  int64(len(content)),
			OccurredAt: clock(),
		},
	}
	if pubErr := bus.Publish(ctx, ev); pubErr != nil {
		return nil, fmt.Errorf("mcpconsole: publish %s: %w", ev.Type, pubErr)
	}
	return &protocol.MCPResourceArtifactRow{
		ID:        ref.ID,
		MimeType:  ref.MimeType,
		SizeBytes: ref.SizeBytes,
		Filename:  ref.Filename,
		SHA256:    ref.SHA256,
	}, nil
}

// appRefFromValue projects an MCP App reference reconciled by the driver
// onto the runtime-side row, when the invoked tool declared a `ui://` app
// (on its tool-definition `_meta.ui`, the spec-conformant placement).
// Returns nil for a non-MCP or non-app result. serverID is the source id
// of the MCP server the tool belongs to, so the Console can resolve which
// server to read the `ui://` document from. The DisplayMode is the
// optional display-mode hint (empty unless the server supplied one — the
// renderer defaults to inline); RawHTMLTrusted stays the default-deny
// posture (the Console reconciles full trust via mcp.servers.get).
func appRefFromValue(value any, serverID string) *protocol.MCPAppRefRow {
	v, ok := value.(mcp.MCPToolValue)
	if !ok || v.AppRef == nil {
		return nil
	}
	return &protocol.MCPAppRefRow{
		ServerID:    serverID,
		ToolCallID:  v.AppRef.ToolCallID,
		ResourceURI: v.AppRef.ResourceURI,
		DisplayMode: v.AppRef.PreferredDisplayMode,
	}
}
