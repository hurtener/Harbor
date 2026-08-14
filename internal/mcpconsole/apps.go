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
// larger App-document cap instead of the ordinary inline-payload bound —
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
	agentID  string            // legacy/default slot; effective ctx selection wins
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
// at construction (CLAUDE.md §5). Threshold ≤ 0 falls back to the
// Console inline-payload bound; Clock defaults to time.Now.
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
	// AgentID is the legacy/default slot the gate reads only when a direct
	// embedder did not seat EffectiveAgentConfig on ctx. Production Protocol
	// dispatch always seats its reach-admitted effective selection first.
	// Required when AgentConfig is set; ignored when it is nil.
	AgentID string
	// SessionOverlay is the session-safe narrow-only disable set the app→host
	// gate UNIONS into the admin exposure, so a tool a SESSION user disabled
	// (via agent_config.session.set_source_disables) is also rejected from an
	// App callback — parity with the run-start planner-view projection, which
	// unions the same overlay. Optional: nil leaves only the admin exposure
	// gating. Keyed by the ctx (tenant, user, session) triple.
	SessionOverlay sessionoverlay.Store
	// Threshold is the inline-payload bound in bytes. ≤ 0 falls back to
	// the Console inline-payload bound. The wiring passes that pinned
	// bound rather than the operator's LLM-context heavy-output
	// threshold: these payloads are browser-rendered, never prompt bytes.
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
// the operator-legible "paused by a system administrator" overlay.
//
// It wraps protocol.ErrAccessorScopeDenied so the wire edge classifies it by
// errors.Is. The edge previously matched this message's TEXT, which a
// southbound server's error could contribute to; see the Protocol sentinel's
// godoc for why a refusal must not be mintable from foreign wording.
var ErrAppToolExposureDenied = fmt.Errorf(
	"%w: mcpconsole: tool unavailable — paused or disabled by agent configuration",
	protocol.ErrAccessorScopeDenied)

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

// defaultHeavyThreshold is the inline-payload bound applied when the
// caller supplies no threshold. Single-sourced on the CONSOLE-FACING
// inline bound, not on the LLM-context heavy-output default: every
// payload this package shapes — an MCP resource, an App tool result, a
// captured tool context — is read by a browser and never enters a
// model's context window, so it is priced in HTTP bytes rather than
// tokens.
const defaultHeavyThreshold = config.DefaultConsoleInlinePayloadBytes

// appDocumentInlineCap is the inline byte ceiling for a `ui://` MCP App
// document fetched through ReadResource. It is DELIBERATELY larger than
// the Console inline-payload bound (defaultHeavyThreshold) because the
// two ceilings guard different things:
//
//   - The inline-payload bound keeps an ordinary reply small enough that
//     a browser page carrying many of them stays cheap; the LLM-context
//     heavy-output threshold (RFC §6.5) separately keeps bulky bytes OUT
//     of a model's context window.
//   - An MCP App document NEVER enters the LLM context. The tool result
//     carries only the tiny `_meta.ui.resourceUri` reference string; the
//     `ui://` HTML itself is fetched ONLY by the Console (via
//     mcp.servers.read_resource) and rendered in a sandboxed iframe. It
//     is a render payload, not a context payload.
//
// Gating an App document on the ordinary inline bound offloads ordinary
// real-world apps (a studio App's HTML routinely runs 80–100 KiB) to the
// ArtifactStore by reference — and a by-reference App can only be fetched
// via a presigned URL, which every non-S3 artifact driver fails loud on,
// so the App never renders on the in-memory / filesystem / SQLite /
// Postgres stores. The dedicated, larger cap lets the common case ride
// inline on EVERY driver. Above the cap the read still offloads (the loud
// `mcp.resource_offloaded` bypass) so a pathologically large App is never
// inlined unbounded and never silently truncated.
const appDocumentInlineCap = 2 * 1024 * 1024 // 2 MiB

// compile-time assertions: AppsAccessor satisfies the three Apps seams
// plus the distinct admission-aware invoker seam (HA-56).
var (
	_ protocol.MCPResourceReader       = (*AppsAccessor)(nil)
	_ protocol.AppToolInvoker          = (*AppsAccessor)(nil)
	_ protocol.AppToolContextReader    = (*AppsAccessor)(nil)
	_ protocol.AppToolAdmissionInvoker = (*AppsAccessor)(nil)
)

// ReadResource implements protocol.MCPResourceReader. It fetches the
// resource bytes from the MCP registry under the ctx identity triple,
// then applies the inline-or-offload decision against an effective cap:
// a `ui://` MCP App document — a Console-render payload that never enters
// the LLM context — rides inline up to the dedicated App-document cap
// (appDocumentInlineCap); any other resource keeps the Console
// inline-payload bound. Content at or above the effective cap is offloaded to the
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
	// a sandboxed iframe, so the ordinary inline-payload bound does not
	// apply. It rides inline up to the larger App-document cap so
	// real-world apps render on every artifact driver, not just S3. An
	// ordinary resource keeps the Console inline-payload bound.
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
// and invokes the SAME wrapped descriptor the planner resolves — so the
// approval gate (the unified pause primitive) + tool-side OAuth fire
// exactly as on a planner call. The result is
// heavy-content-disciplined (the context-window safety net); a `ui://`
// MCP App the invoked tool declared (on its tool-definition `_meta.ui`,
// reconciled by the driver onto the result's app reference) is projected
// onto the App field.
//
// # Two deliberately different resolution views
//
// The tool is resolved against the ordinary planner/model catalog FIRST
// (the pre-existing path — ordinary tools, including ones that declare a
// `ui://` app for a rendered App to re-invoke). When the name does NOT
// resolve there, the only remaining authority is the serverID-named
// server's App dispatch catalog — the app-only callbacks the provider
// declared with `_meta.ui.visibility: ["app"]` and the attach path kept
// OUT of the ordinary catalog. That catalog is keyed by the host-derived
// server identity, not by any string prefix or remembered global name:
//
//   - an app-only callback is NOT resolvable without its own serverID
//     (missing server identity → typed not-found before invocation);
//   - a serverID that does not hold the name answers not-found, so one
//     server's rendered App can never invoke another server's callback;
//   - an ordinary tool called with a serverID that disagrees with its
//     actual Source is refused as a scope mismatch BEFORE invocation (the
//     App claims an identity it does not hold). An ordinary tool called
//     with NO serverID keeps the pre-catalog behavior (legacy clients).
//
// Whatever view the tool resolved through, the invocation path is the
// SAME: the identity / agent-reach / current-state exposure gate below and
// the wrapped descriptor's approval / OAuth / identity path.
func (a *AppsAccessor) CallTool(ctx context.Context, serverID, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	return a.callTool(ctx, serverID, "", "", tool, args)
}

// CallToolWithBinding dispatches an app callback using its opaque render
// capability while preserving the ordinary invoker compatibility seam.
// It is the LEGACY live-binding path (HA-56): the binding is passed to
// the provider-local ValidateAppBinding for app-only resolution. A
// render-admission-backed call NEVER rides here — it rides the distinct
// admission-aware seam (CallToolAdmitted).
func (a *AppsAccessor) CallToolWithBinding(ctx context.Context, serverID, binding, resourceURI, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	return a.callTool(ctx, serverID, binding, resourceURI, tool, args)
}

// CallToolAdmitted implements protocol.AppToolAdmissionInvoker — the
// distinct, narrow admission-verified invocation seam (HA-56). The
// AppsSurface calls it ONLY after verifying a fresh render admission for
// the exact (identity, agent, server, resource) tuple and minting the
// unforgeable call-local proof.
//
// # The call-local proof is the authority — never method selection
//
// This exported method must not reach ResolveAppTool merely because an
// internal caller invoked it with a fully verified identity /
// effective-agent context. The accessor therefore verifies the call-local
// proof (protocol.CheckRenderAdmissionProof) against the EXACT tuple this
// call names — identity from ctx, the effective agent, serverID, the
// resourceURI, AND the exact CURRENT provider/catalog generation read from
// the registry — BEFORE any resolution, before the paused/disabled
// exposure gate, and before any invocation. A direct call with no proof,
// or a proof that does not bind the exact tuple, is refused here with
// zero callbacks. The proof can only be minted by the Protocol surface
// after it opened the sealed admission AND re-verified the current render
// tuple, so a call that did not ride the surface's verified path can
// never resolve an app-only callback.
//
// # The generation closes the TOCTOU window
//
// The proof binds the generation the surface verified. The accessor
// re-reads the CURRENT generation before resolving, so a
// refresh/replacement that landed after the surface's verification
// changes the generation and the proof no longer matches — the call is
// refused with zero callbacks. Even the window between this generation
// read and the descriptor lookup is closed: resolution goes through the
// registry's atomic ResolveAppToolAtGeneration, which compares the exact
// generation and resolves the app-only descriptor under ONE read lock, so
// a race that changes the generation mid-call fails typed and never
// returns (never executes) a newer-generation row.
//
// Unlike CallTool / CallToolWithBinding, this path resolves the named
// tool EXCLUSIVELY through its own server's App dispatch catalog — never
// the ordinary planner/model catalog, never a provider-local
// ValidateAppBinding. The sealed admission token is never handed to a
// provider-local validator. The current paused/disabled exposure gate
// re-runs, and the SAME wrapped descriptor the ordinary path invokes
// fires, so approval / OAuth / policy / redaction / retry / audit still
// work exactly as they do on a planner call.
//
// A host-derived server identity is MANDATORY: an empty server never
// falls through to ordinary/global resolution.
func (a *AppsAccessor) CallToolAdmitted(ctx context.Context, serverID, resourceURI, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	if serverID == "" {
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: %w: %q",
			protocol.ErrAccessorNotFound, tools.ErrToolNotFound, tool)
	}
	// Identity is mandatory (AGENTS.md §6 rule 9): the proof binds the
	// exact verified triple, and a call with no identity has nothing to
	// bind a proof against — fail closed before any resolution.
	id, ok := identity.From(ctx)
	if !ok {
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("mcpconsole: admission-verified app call: %w", mcp.ErrIdentityMissing)
	}
	agentID, ok := tools.EffectiveAgentConfigFrom(ctx)
	if !ok {
		// Compatibility for direct pre-v1.26.11 embedders, mirroring
		// gateToolExposure: production Protocol dispatch always seats the
		// reach-admitted effective agent.
		agentID = a.agentID
	}
	// Read the exact CURRENT generation BEFORE the proof check: the proof
	// binds the generation the surface verified, and a refresh/replacement
	// since that verification must refuse here — a stale admission never
	// resolves (and never executes) a newer-generation descriptor.
	currentGen, ok := a.reg.CurrentGeneration(serverID)
	if !ok || currentGen == "" {
		// Missing/empty current generation (absent server, detach, or a
		// server whose discovery has never established its descriptor set)
		// is a scope-level refusal — never a fallback to legacy binding or
		// ordinary resolution, never a collapse into not-found.
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: mcpconsole: render-admission call-local proof cannot be verified: server %q has no current provider/catalog generation",
			protocol.ErrAccessorScopeDenied, serverID)
	}
	if !protocol.CheckRenderAdmissionProof(ctx, id, agentID, serverID, resourceURI, currentGen) {
		// The proof is missing, binds a different tuple, or binds a
		// generation that is no longer current. A scope-level refusal
		// (ErrAccessorScopeDenied → CodeScopeMismatch at the wire edge),
		// never a not-found: the target may exist, but this call is not
		// authorized to reach it under the current generation. The refusal
		// text deliberately does not echo the generation digest — the wire
		// edge carries this message verbatim, and a digest in it would
		// leak catalog state to whoever probes the refusal.
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: mcpconsole: render-admission call-local proof is missing or does not bind the exact (identity, agent, server %q, resource %q) tuple under the current generation",
			protocol.ErrAccessorScopeDenied, serverID, resourceURI)
	}
	// The atomic compare+resolve: ONE registry read lock re-verifies the
	// exact current generation and resolves the app-only descriptor in the
	// same critical section. A refresh/replacement between the generation
	// read above and this call fails typed (ErrGenerationMismatch) — the
	// new row is never returned and never invoked.
	desc, ok, err := a.reg.ResolveAppToolAtGeneration(serverID, tool, currentGen)
	if err != nil {
		// The registry's exact-generation compare refused (absent /
		// unknown generation, or a generation change raced the call). The
		// admission is stale — a scope-level refusal, never a resolution
		// of the new row, never a fallback.
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: mcpconsole: %w", protocol.ErrAccessorScopeDenied, err)
	}
	if !ok {
		// The server is absent, or does not hold an app-only callback
		// under this name at the exact verified generation. Same typed
		// not-found — the App renders it as a permanent "there is no such
		// action on this server".
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: %w: %q (server %q)",
			protocol.ErrAccessorNotFound, tools.ErrToolNotFound, tool, serverID)
	}
	return a.invokeAppTool(ctx, tool, desc, args)
}

func (a *AppsAccessor) callTool(ctx context.Context, serverID, binding, resourceURI, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	desc, ok := a.cat.Resolve(tool)
	if !ok {
		// Not in the ordinary planner/model catalog. The only authority
		// left is the named server's App dispatch catalog — an app-only
		// callback. Without a host-derived server identity there is
		// deliberately NO way to reach it (no string prefix or remembered
		// global name may select another server's callback).
		if serverID == "" {
			return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: %w: %q",
				protocol.ErrAccessorNotFound, tools.ErrToolNotFound, tool)
		}
		appDesc, appOK := a.reg.ResolveAppTool(serverID, tool)
		if appOK && !a.reg.ValidateAppBinding(ctx, serverID, binding, resourceURI) {
			appOK = false
		}
		if !appOK {
			// The server is absent, or does not hold an app-only callback
			// under this name. Same typed not-found — the App renders it as
			// a permanent "there is no such action on this server".
			return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: %w: %q (server %q)",
				protocol.ErrAccessorNotFound, tools.ErrToolNotFound, tool, serverID)
		}
		desc = appDesc
	} else if serverID != "" && string(desc.Tool.Source) != serverID {
		// The ordinary catalog resolved the tool, but the App claims a
		// server identity that disagrees with the tool's actual source —
		// another server's rendered App (or an App claiming a server that
		// does not own this tool at all). Refused BEFORE invocation, as a
		// typed scope-denied authorization verdict (the App's host-derived
		// identity is not the identity of the server that owns the tool).
		return protocol.MCPAppToolResultRow{}, fmt.Errorf("%w: app call to tool %q names server %q, but the tool belongs to server %q",
			protocol.ErrAccessorScopeDenied, tool, serverID, desc.Tool.Source)
	}
	return a.invokeAppTool(ctx, tool, desc, args)
}

// invokeAppTool runs the shared invocation tail every app-tool-call path
// converges on: the current paused/disabled exposure gate, the SAME
// wrapped descriptor the planner resolves (approval / OAuth / policy /
// redaction / retry / audit), and the heavy-content-disciplined result
// projection. It is the one place the proxy's wrapped invocation lives —
// no path may bypass it.
func (a *AppsAccessor) invokeAppTool(ctx context.Context, tool string, desc tools.ToolDescriptor, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
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
	if a.agentCfg == nil {
		return nil
	}
	agentID, ok := tools.EffectiveAgentConfigFrom(ctx)
	if !ok {
		// Compatibility for direct pre-v1.26.11 embedders. Production Protocol
		// dispatch always stamps the reach-admitted effective agent.
		agentID = a.agentID
	}
	if agentID == "" {
		return nil
	}
	id, ok := identity.From(ctx)
	if !ok || id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return fmt.Errorf("mcpconsole: app-call exposure gate: %w", mcp.ErrIdentityMissing)
	}
	rev, has, err := a.agentCfg.Active(ctx, identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent)
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
		overlay, _, oerr := a.sessionOverlay.Get(ctx, identity.Quadruple{Identity: id}, agentID)
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

// CurrentGeneration returns the deterministic current provider/catalog
// generation fingerprint for the named server, delegating to the MCP
// registry. The value is content-derived from the canonical CURRENT
// descriptor set (resources + app-only callbacks + ordinary catalog), so
// it changes on detach, replacement, and every successful discovery
// change, and is stable across replicas with the same canonical current
// descriptor set. Unknown/empty fails closed (false) — a render
// admission never binds an empty generation.
//
// This is the "current provider/catalog generation" a fresh
// render-admission gate binds into the sealed tuple. It is NOT itself a
// proof of erasure / session+agent exposure / paused+disabled state —
// the gate composes those checks (G4).
func (a *AppsAccessor) CurrentGeneration(serverID string) (string, bool) {
	return a.reg.CurrentGeneration(serverID)
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
		Binding:     v.AppRef.Binding,
		ToolCallID:  v.AppRef.ToolCallID,
		ResourceURI: v.AppRef.ResourceURI,
		DisplayMode: v.AppRef.PreferredDisplayMode,
	}
}
