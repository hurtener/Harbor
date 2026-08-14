package protocol

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
)

// AppsSurface is the transport-agnostic Harbor Protocol MCP Apps
// handler. It owns the three MCP Apps host methods — the
// `mcp.servers.read_resource` `ui://` UI-document fetch and the
// `mcp.apps.call_tool` app-tool-call proxy, and the identity-scoped
// `mcp.apps.tool_context` read — that back the Console's
// sandboxed-iframe MCP App renderer.
//
// AppsSurface is a sibling of the MCPSurface (the twelve Console
// `mcp.servers.*` methods), not an extension: it reaches the runtime's
// MCP driver registry (for resource reads) and the tool catalog (for the
// proxy's re-entry into the approval / OAuth / identity tool-safety
// path).
//
// # The proxy is a re-entry, not a new path
//
// `mcp.apps.call_tool` does NOT open a fresh tool-dispatch path. It
// resolves the named tool from the SAME catalog a planner-initiated call
// resolves and invokes the SAME wrapped descriptor — so the approval
// gate (the unified pause primitive) and tool-side OAuth fire exactly as
// they do for a planner call. An app call to a gated tool parks on the
// pause primitive before any side effect; there is no bypass.
//
// # The Console never reads internal Runtime objects (CLAUDE.md §8/§13)
//
// The MCP Apps data flows as canonical Protocol wire types
// (internal/protocol/types) — never as a re-export of the `mcp` driver
// or `tools` Go structs. The MCPResourceReader / AppToolInvoker seams
// are narrow adapters the Runtime wires at boot; the `protocol` package
// never imports the `mcp` driver or the tool catalog. The only tools
// dependency is the context capability used by every execution path to
// carry an already reach-admitted effective-agent selection.
//
// AppsSurface is immutable after construction and safe for concurrent
// use by N goroutines — Dispatch reads request-specific data from ctx +
// the request argument, never from the surface struct.
type AppsSurface struct {
	resource    MCPResourceReader
	invoker     AppToolInvoker
	toolContext AppToolContextReader
	agents      AgentResolver
	reach       auth.AgentReachAuthorizer
	// admissionAuthority is the OPTIONAL sealed render-admission seam
	// (HA-56). When nil, the opt-in `request_render_admission` mint
	// fails loud with CodeRuntimeError and a render-admission-backed
	// app-tool-call fails loud — the ordinary resource read and the
	// legacy binding path are untouched.
	admissionAuthority RenderAdmissionAuthority
	// descriptorGeneration reads the CURRENT registry descriptor
	// generation fingerprint for a (server, resource) tuple — the
	// provider/catalog generation every mint and every verify binds.
	// Mandatory alongside admissionAuthority; a nil or empty fingerprint
	// is a typed refusal (generation is never optional).
	descriptorGeneration DescriptorGenerationReader
}

// RenderAdmissionAuthority is the narrow, stateless, sealed
// render-admission seam the AppsSurface calls into (HA-56). The Runtime
// wires the production implementation — the sealed authority in
// internal/mcpconsole/admission over the shared auth.Sealer — and tests
// wire a deterministic fake. The seam performs NO viewer authorization
// and NO resource lookup: the AppsSurface runs the full verified
// identity / reach / retirement / erasure / current-exposure / exact
// server+resource checks BEFORE any mint, and the authority seals only
// the already-authorized render tuple. The sealed token never exposes
// its claims, key material, or a provider-local live binding.
type RenderAdmissionAuthority interface {
	// Mint seals a fresh render admission for the exact tuple. Two mints
	// for the identical tuple produce distinct tokens (a fresh claim
	// nonce per call).
	Mint(ctx context.Context, rt admission.RenderTuple) (admission.Token, error)
	// Verify opens token, strictly validates its claims, and requires
	// the exact expected tuple — identity, agent, server, resource URI,
	// and the CURRENT descriptor fingerprint. Outcomes are typed via
	// errors.Is against admission.ErrTokenMissing / ErrTokenUnavailable /
	// ErrTokenInvalid / ErrTokenExpired / ErrTokenMismatch.
	Verify(ctx context.Context, expected admission.RenderTuple, token string) (admission.Claims, error)
}

// DescriptorGenerationReader returns the CURRENT registry descriptor
// generation fingerprint for a (server, resource) tuple — the
// provider/catalog generation the render admission binds as its
// provider generation. The value is MANDATORY for every mint and every
// verify: a nil reader, an error, or an empty fingerprint is a typed
// refusal (a render admission never binds an empty generation, and an
// otherwise-current App with an unverifiable generation never collapses
// to a generic not-found).
type DescriptorGenerationReader interface {
	// DescriptorGeneration returns the current descriptor generation
	// fingerprint for the exact server + resource. An error means the
	// registry could not resolve a CURRENT generation for the tuple.
	DescriptorGeneration(ctx context.Context, serverID, resourceURI string) (string, error)
}

// MCPResourceArtifactRow is the runtime-side projection of a by-reference
// heavy-content stub. The accessor returns it when fetched content meets
// or exceeds the heavy-content threshold (RFC §6.5); the surface
// maps it onto the wire `types.MCPResourceArtifactRef`.
type MCPResourceArtifactRow struct {
	ID        string
	MimeType  string
	SizeBytes int64
	Filename  string
	SHA256    string
}

// MCPResourceContent is the runtime-side result of a resource read.
// EXACTLY ONE of Inline / Artifact is populated: Inline for content
// below the heavy-content threshold, Artifact for content at or above it
// (the accessor emits the loud bypass event when it offloads). The
// accessor owns the threshold decision — the surface only projects.
type MCPResourceContent struct {
	ResourceURI string
	MIMEType    string
	Inline      []byte
	Artifact    *MCPResourceArtifactRow
}

// MCPResourceReader is the narrow contract the AppsSurface calls into for
// `mcp.servers.read_resource`. The Runtime's MCP registry (wrapped with
// the artifact store + bus for the heavy-content branch) satisfies
// it; the `protocol` package never imports the `mcp` driver.
//
// ReadResource is identity-mandatory: the implementation reads the triple
// from ctx and fails closed on a missing one.
type MCPResourceReader interface {
	ReadResource(ctx context.Context, serverID, resourceURI string) (MCPResourceContent, error)
}

// MCPAppRefRow is the runtime-side projection of an MCP App reference
// parsed from a tool result's `_meta.ui.resourceUri` slot. It is nil for
// a non-app tool result.
type MCPAppRefRow struct {
	ServerID       string
	Binding        string
	ToolCallID     string
	ResourceURI    string
	DisplayMode    string
	RawHTMLTrusted bool
}

// AppToolContextPayloadRow is the runtime-side projection of one half
// (input or result) of a captured tool context. EXACTLY ONE of Inline /
// Artifact is populated — Inline for content captured below the
// heavy-content threshold, Artifact for content at or above it (offloaded
// by reference at capture time). The accessor owns the threshold decision;
// the surface only projects.
type AppToolContextPayloadRow struct {
	Inline   json.RawMessage
	Artifact *MCPResourceArtifactRow
}

// AppToolContextRow is the runtime-side result of a tool-context read. It
// carries the captured input + lowered result that produced a rendered MCP
// App, each inline-or-by-reference (the heavy-content discipline matches
// MCPResourceContent).
type AppToolContextRow struct {
	Tool    string
	Input   AppToolContextPayloadRow
	Result  AppToolContextPayloadRow
	IsError bool
}

// AppToolContextReader is the narrow contract the AppsSurface calls into
// for `mcp.apps.tool_context`. The runtime's tool-context store (over the
// StateStore + ArtifactStore) satisfies it; the `protocol` package never
// imports the store.
//
// ToolContext is identity-mandatory: the implementation reads the triple
// from ctx and resolves the captured context scoped to it, so an unknown
// or cross-identity (serverID, toolCallID) is not found.
type AppToolContextReader interface {
	ToolContext(ctx context.Context, serverID, toolCallID string) (AppToolContextRow, error)
}

// MCPAppToolResultRow is the runtime-side result of an app-tool-call
// proxy invocation. EXACTLY ONE of Inline / Artifact is populated (the
// heavy-content discipline matches MCPResourceContent). App is non-nil
// when the tool result declared a `ui://` MCP App.
type MCPAppToolResultRow struct {
	Tool     string
	Inline   json.RawMessage
	Artifact *MCPResourceArtifactRow
	IsError  bool
	App      *MCPAppRefRow
}

// AppToolInvoker is the narrow contract the AppsSurface calls into for
// `mcp.apps.call_tool`. The Runtime's tool catalog (wrapped with the
// artifact store + bus for the heavy-content branch) satisfies it.
// The implementation MUST resolve the tool from the SAME catalog a
// planner uses (and, for an app-only callback, from the named server's
// App dispatch catalog) and invoke the SAME wrapped descriptor, so the
// approval gate + tool-side OAuth fire — there is no parallel path.
//
// serverID is the HOST-DERIVED identity of the MCP server whose rendered
// App issued the call — authoritative runtime context, never parsed from
// the tool name. It is the key the dispatch check verifies against: an
// app-only callback is resolvable ONLY through its own server's App
// dispatch catalog, and an ordinary tool called with a disagreeing
// serverID is refused before invocation. CallTool is identity-mandatory:
// the implementation reads the triple from ctx (the gate wrapper's
// `identity.From`) and fails closed on a missing one.
type AppToolInvoker interface {
	CallTool(ctx context.Context, serverID, tool string, args json.RawMessage) (MCPAppToolResultRow, error)
}

type appBindingInvoker interface {
	CallToolWithBinding(context.Context, string, string, string, string, json.RawMessage) (MCPAppToolResultRow, error)
}

// AppsDeps bundles the runtime-side seams an AppsSurface reads through.
type AppsDeps struct {
	// Resource is the `ui://` resource reader (the MCP registry adapter).
	// Mandatory.
	Resource MCPResourceReader
	// Invoker is the app-tool-call proxy (the tool-catalog adapter).
	// Mandatory.
	Invoker AppToolInvoker
	// ToolContext is the tool-context reader (the StateStore + ArtifactStore
	// adapter) backing `mcp.apps.tool_context`. Mandatory.
	ToolContext AppToolContextReader
	// AgentResolver selects the configured default for older clients and
	// verifies that an explicit agent exists under the caller's tenant.
	// Mandatory; Apps data-plane calls fail closed without it.
	AgentResolver AgentResolver
	// AgentReach is the shared signed-agent-reach gate. Nil installs the
	// canonical fail-closed implementation, matching ControlSurface.
	AgentReach auth.AgentReachAuthorizer
	// RenderAdmissionAuthority is the OPTIONAL sealed render-admission
	// seam (HA-56). When nil, the opt-in `request_render_admission`
	// mint and a render-admission-backed app-tool-call fail loud; the
	// ordinary resource read and the legacy binding path are untouched.
	RenderAdmissionAuthority RenderAdmissionAuthority
	// DescriptorGeneration is the OPTIONAL current registry descriptor
	// generation reader for (server, resource) tuples. Required
	// alongside RenderAdmissionAuthority — the render admission always
	// binds the exact current generation, never an optional/empty one.
	DescriptorGeneration DescriptorGenerationReader
}

// ErrAppsMisconfigured — NewAppsSurface was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5).
var ErrAppsMisconfigured = stderrors.New("protocol: AppsSurface missing a mandatory dependency")

// ErrAccessorNotFound — the sentinel a runtime-side accessor wraps to state,
// in its own voice, that the request's target does not exist. mapMCPError
// classifies it as CodeNotFound via errors.Is.
//
// # Why a sentinel rather than an error-text marker
//
// The MCP surface historically classified not-found by substring-matching the
// error chain's rendered text. That works only while every error on the chain
// is Harbor's own. It is not: a southbound MCP server's error text is wrapped
// verbatim into the chain, so a REMOTE party's wording decides a Harbor
// classification. A server whose transport failure happens to read "tool not
// found" would be laundered into a typed not-found, which a rendered MCP App
// reads as "this action does not exist here" — a permanent, wrong conclusion
// drawn from a transient failure.
//
// Wrapping this sentinel is an assertion by the accessor that IT resolved the
// target and IT found nothing. No upstream text can forge that.
var ErrAccessorNotFound = stderrors.New("protocol: accessor target not found")

// ErrAccessorScopeDenied — the sentinel a runtime-side accessor wraps to state
// that the caller's request was refused on an AUTHORIZATION ground rather than
// because the target is absent. mapMCPError classifies it as CodeScopeMismatch
// via errors.Is.
//
// Same reasoning as ErrAccessorNotFound, and the same hazard: this
// classification used to substring-match the chain for the exposure gate's
// message, and that chain carries a southbound server's text verbatim. A
// refusal and an absence are the two verdicts a rendered MCP App branches on
// most sharply — "you may not" versus "there is no such thing" — so neither may
// be reachable from wording Harbor does not author.
var ErrAccessorScopeDenied = stderrors.New("protocol: accessor refused the request")

// NewAppsSurface builds the Protocol MCP Apps surface. All three accessors and
// the agent resolver are mandatory; a nil fails loud with a wrapped
// ErrAppsMisconfigured. A nil reach authorizer installs the canonical
// fail-closed signed-reach gate. The render-admission authority and the
// descriptor-generation reader are OPTIONAL: supplying only one of the pair
// is treated as supplying neither (a half-wired admission seam is refused
// loud at the seam use site, never silently degraded).
//
// The returned AppsSurface is immutable after construction and safe for
// concurrent use by N goroutines.
func NewAppsSurface(deps AppsDeps) (*AppsSurface, error) {
	if deps.Resource == nil {
		return nil, fmt.Errorf("%w: Resource reader is nil", ErrAppsMisconfigured)
	}
	if deps.Invoker == nil {
		return nil, fmt.Errorf("%w: tool-call Invoker is nil", ErrAppsMisconfigured)
	}
	if deps.ToolContext == nil {
		return nil, fmt.Errorf("%w: tool-context reader is nil", ErrAppsMisconfigured)
	}
	if deps.AgentResolver == nil {
		return nil, fmt.Errorf("%w: AgentResolver is nil", ErrAppsMisconfigured)
	}
	reach := deps.AgentReach
	if reach == nil {
		reach = auth.NewAgentReachAuthorizer()
	}
	admissionAuthority := deps.RenderAdmissionAuthority
	descriptorGeneration := deps.DescriptorGeneration
	if admissionAuthority == nil || descriptorGeneration == nil {
		// A half-wired admission seam is an unwired seam: the surface
		// must never mint with only one half of the pair present.
		admissionAuthority = nil
		descriptorGeneration = nil
	}
	return &AppsSurface{
		resource: deps.Resource, invoker: deps.Invoker, toolContext: deps.ToolContext,
		agents: deps.AgentResolver, reach: reach,
		admissionAuthority:   admissionAuthority,
		descriptorGeneration: descriptorGeneration,
	}, nil
}

// Dispatch is the single transport-agnostic entry point for an MCP Apps
// method call. method MUST be one of the three MCP Apps methods
// (methods.IsMCPAppsMethod); req MUST be the wire request type the
// method expects.
//
// The return is always a *types.<Method>Response or a *protoerrors.Error:
//
//   - CodeUnknownMethod    — method is not an MCP Apps method.
//   - CodeInvalidRequest   — req is nil or the wrong wire type.
//   - CodeIdentityRequired — the request's identity triple is incomplete.
//   - CodeNotFound         — the named server / tool / resource is absent.
//   - CodeRuntimeError     — an accessor failure (incl. a gate rejection).
//
// Dispatch holds no per-call state on the AppsSurface.
func (s *AppsSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsMCPAppsMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol MCP Apps method", string(method))
	}
	switch method {
	case methods.MethodMCPReadResource:
		return s.handleReadResource(ctx, req)
	case methods.MethodMCPAppsCallTool:
		return s.handleCallTool(ctx, req)
	case methods.MethodMCPAppsToolContext:
		return s.handleToolContext(ctx, req)
	default:
		// Unreachable: IsMCPAppsMethod already gated the set.
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no MCP Apps handler (Protocol-surface invariant violated)", string(method))
	}
}

// handleReadResource serves mcp.servers.read_resource.
func (s *AppsSurface) handleReadResource(ctx context.Context, req any) (any, error) {
	method := methods.MethodMCPReadResource
	r, ok := req.(*types.ReadMCPResourceRequest)
	if !ok || r == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: expected *types.ReadMCPResourceRequest, got %T", string(method), req)
	}
	id, perr := gateAppsIdentity(ctx, method, &r.Identity)
	if perr != nil {
		return nil, perr
	}
	if r.ServerID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: server_id is required", string(method))
	}
	if r.ResourceURI == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: resource_uri is required", string(method))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	effectiveID, err := admitEffectiveAgent(idCtx, string(method), id, r.AgentID, s.agents, s.reach)
	if err != nil {
		return nil, err
	}
	idCtx = tools.WithEffectiveAgentConfig(idCtx, effectiveID)
	content, err := s.resource.ReadResource(idCtx, r.ServerID, r.ResourceURI)
	if err != nil {
		return nil, mapMCPError(string(method), err)
	}
	resp := &types.ReadMCPResourceResponse{
		ResourceURI:     content.ResourceURI,
		MIMEType:        content.MIMEType,
		ProtocolVersion: types.ProtocolVersion,
	}
	if resp.ResourceURI == "" {
		resp.ResourceURI = r.ResourceURI
	}
	if content.Artifact != nil {
		resp.ArtifactRef = projectArtifactRow(content.Artifact)
	} else {
		resp.Content = string(content.Inline)
	}
	// HA-56 amendment: the OPT-IN render admission. Omitted/false
	// preserves the ordinary read byte-for-byte and mints NO callback
	// authority. Only a SUCCESSFUL read carrying true may mint, and the
	// mint binds the exact CURRENT registry descriptor generation — a
	// nil reader, an error, or an empty fingerprint is a typed refusal,
	// never an admission over an empty generation.
	if r.RequestRenderAdmission {
		adm, perr := s.mintRenderAdmission(idCtx, id, effectiveID, r.ServerID, r.ResourceURI)
		if perr != nil {
			return nil, perr
		}
		if adm != nil {
			resp.RenderAdmission = adm
		}
	}
	return resp, nil
}

// mintRenderAdmission mints the bounded render admission for a
// successful opt-in `ui://` read. It runs AFTER the full verified
// identity / reach / retirement / erasure / current-exposure / exact
// server+resource checks (the accessor read above already passed), and
// binds the exact CURRENT descriptor generation as the provider/catalog
// generation. Returns a nil *types.RenderAdmission (no error) when the
// admission could not be minted but the read itself succeeded — the
// response then carries no admission at all, which is the honest closed
// outcome. Returns a typed Protocol error when the surface is
// misconfigured (the seam is unwired) or the generation read failed.
func (s *AppsSurface) mintRenderAdmission(ctx context.Context, id identity.Identity, agentID, serverID, resourceURI string) (*types.RenderAdmission, *protoerrors.Error) {
	method := methods.MethodMCPReadResource
	if s.admissionAuthority == nil || s.descriptorGeneration == nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: render-admission authority is not wired on this runtime", string(method))
	}
	generation, err := s.descriptorGeneration.DescriptorGeneration(ctx, serverID, resourceURI)
	if err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: current descriptor generation could not be resolved: %v", string(method), err)
	}
	if generation == "" {
		// The tuple has no current generation — an admission cannot
		// bind an empty generation. The closed availability answer is
		// "unavailable": the caller must re-read, and no admission is
		// minted.
		return &types.RenderAdmission{Availability: types.RenderAdmissionUnavailable}, nil
	}
	tok, err := s.admissionAuthority.Mint(ctx, admission.RenderTuple{
		Identity:              id,
		AgentID:               agentID,
		ServerID:              serverID,
		ResourceURI:           resourceURI,
		DescriptorFingerprint: generation,
	})
	if err != nil {
		return nil, mapRenderAdmissionError(string(method), err)
	}
	return &types.RenderAdmission{
		Token:        tok.Value,
		IssuedAt:     tok.IssuedAt.UTC().Format(admissionTokenTimeLayout),
		ExpiresAt:    tok.ExpiresAt.UTC().Format(admissionTokenTimeLayout),
		Availability: types.RenderAdmissionAvailable,
	}, nil
}

// admissionTokenTimeLayout is the RFC 3339 UTC layout the render
// admission expiry metadata uses on the wire.
const admissionTokenTimeLayout = "2006-01-02T15:04:05Z07:00"

// handleCallTool serves mcp.apps.call_tool — the app-tool-call proxy.
func (s *AppsSurface) handleCallTool(ctx context.Context, req any) (any, error) {
	method := methods.MethodMCPAppsCallTool
	r, ok := req.(*types.MCPAppCallToolRequest)
	if !ok || r == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: expected *types.MCPAppCallToolRequest, got %T", string(method), req)
	}
	id, perr := gateAppsIdentity(ctx, method, &r.Identity)
	if perr != nil {
		return nil, perr
	}
	if r.Tool == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: tool is required", string(method))
	}
	// HA-56: the fresh render admission and the legacy binding are
	// DISTINCT authorities. A request that supplies BOTH is refused as
	// ambiguous — the Runtime never guesses which the App meant.
	if r.RenderAdmission != "" && r.Binding != "" {
		return nil, protoerrors.Newf(protoerrors.CodeRenderAuthorityAmbiguous,
			"method %q: request supplies both `render_admission` and the legacy `binding` — these are distinct authorities; supply exactly one", string(method))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	effectiveID, err := admitEffectiveAgent(idCtx, string(method), id, r.AgentID, s.agents, s.reach)
	if err != nil {
		return nil, err
	}
	idCtx = tools.WithEffectiveAgentConfig(idCtx, effectiveID)
	// The invoker re-enters the EXISTING approval / OAuth / identity
	// tool-invocation path — a gated tool parks on the unified pause
	// primitive here, never bypassed. The host-derived server identity
	// rides the call: the invoker verifies it against the resolved tool's
	// source (an app-only callback resolves ONLY through that server's App
	// dispatch catalog; a disagreeing serverID on an ordinary tool is
	// refused before invocation).
	//
	// HA-56: a fresh render admission is verified against the CURRENT
	// render tuple (identity / agent / server / resource URI / current
	// descriptor generation) BEFORE invocation; the verified admission
	// token then rides the SAME wrapped invocation the legacy binding
	// rides (same-server ResolveAppTool + existing invocation), so an
	// unavailable / invalid / expired / mismatched admission fails with
	// its exact typed code — never a collapse into ambiguous not-found.
	if r.RenderAdmission != "" {
		if r.ResourceURI == "" {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: resource_uri is required for a render-admission-backed call", string(method))
		}
		if perr := s.verifyRenderAdmission(idCtx, id, effectiveID, r.ServerID, r.ResourceURI, r.RenderAdmission); perr != nil {
			return nil, perr
		}
	}
	var res MCPAppToolResultRow
	if bound, ok := s.invoker.(appBindingInvoker); ok {
		authority := r.Binding
		if r.RenderAdmission != "" {
			authority = r.RenderAdmission
		}
		res, err = bound.CallToolWithBinding(idCtx, r.ServerID, authority, r.ResourceURI, r.Tool, r.Arguments)
	} else {
		res, err = s.invoker.CallTool(idCtx, r.ServerID, r.Tool, r.Arguments)
	}
	if err != nil {
		return nil, mapMCPError(string(method), err)
	}
	resp := &types.MCPAppCallToolResponse{
		Tool:            res.Tool,
		IsError:         res.IsError,
		ProtocolVersion: types.ProtocolVersion,
	}
	if resp.Tool == "" {
		resp.Tool = r.Tool
	}
	if res.Artifact != nil {
		resp.ArtifactRef = projectArtifactRow(res.Artifact)
	} else {
		resp.Content = res.Inline
	}
	if res.App != nil {
		resp.App = &types.MCPAppRef{
			AgentID:        effectiveID,
			ServerID:       res.App.ServerID,
			ToolCallID:     res.App.ToolCallID,
			ResourceURI:    res.App.ResourceURI,
			DisplayMode:    res.App.DisplayMode,
			RawHTMLTrusted: res.App.RawHTMLTrusted,
			Binding:        res.App.Binding,
		}
	}
	return resp, nil
}

// handleToolContext serves mcp.apps.tool_context — the tool-context read.
func (s *AppsSurface) handleToolContext(ctx context.Context, req any) (any, error) {
	method := methods.MethodMCPAppsToolContext
	r, ok := req.(*types.ToolContextRequest)
	if !ok || r == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: expected *types.ToolContextRequest, got %T", string(method), req)
	}
	id, perr := gateAppsIdentity(ctx, method, &r.Identity)
	if perr != nil {
		return nil, perr
	}
	if r.ServerID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: server_id is required", string(method))
	}
	if r.ToolCallID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: tool_call_id is required", string(method))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.toolContext.ToolContext(idCtx, r.ServerID, r.ToolCallID)
	if err != nil {
		return nil, mapMCPError(string(method), err)
	}
	resp := &types.ToolContextResponse{
		Tool:            row.Tool,
		IsError:         row.IsError,
		Input:           projectToolContextPayload(row.Input),
		Result:          projectToolContextPayload(row.Result),
		ProtocolVersion: types.ProtocolVersion,
	}
	return resp, nil
}

// projectToolContextPayload maps a runtime tool-context payload row onto
// the wire shape: inline JSON below the heavy threshold, an artifact
// reference at or above it (exactly one is set).
func projectToolContextPayload(row AppToolContextPayloadRow) types.ToolContextPayload {
	if row.Artifact != nil {
		return types.ToolContextPayload{ArtifactRef: projectArtifactRow(row.Artifact)}
	}
	return types.ToolContextPayload{Content: row.Inline}
}

// gateAppsIdentity is the surface's identity gate — the one every MCP
// Apps method passes through, whichever transport (or in-process
// embedder) called Dispatch.
//
// It reconciles the request body's identity scope against the ctx
// verified identity through the shared body-identity gate under the MCP
// Apps surface's registered policy, then validates the reconciled
// triple. The policy pins all three components: this surface has no
// admin-elevation path — the proxy's tool-safety gates fire inside the
// invocation path, and a resource read is a plain identity-scoped read —
// so a body triple that disagrees with the verified one is
// unconditionally invalid.
//
// Running the reconciliation HERE rather than only in the transport is
// what makes this surface's transport-agnostic claim true: a second
// transport, or an embedder calling Dispatch directly, gets the same
// answer without re-deriving the check.
func gateAppsIdentity(ctx context.Context, method methods.Method, scope *types.IdentityScope) (identity.Identity, *protoerrors.Error) {
	if _, perr := bodyscope.Reconcile(ctx, bodyscope.ForIdentityScope(scope), bodyscope.SurfaceApps, nil); perr != nil {
		return identity.Identity{}, perr
	}
	id := identity.Identity{
		TenantID:  scope.Tenant,
		UserID:    scope.User,
		SessionID: scope.Session,
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	return id, nil
}

// projectArtifactRow maps a runtime artifact row onto the wire stub.
func projectArtifactRow(row *MCPResourceArtifactRow) *types.MCPResourceArtifactRef {
	return &types.MCPResourceArtifactRef{
		ID:        row.ID,
		MimeType:  row.MimeType,
		SizeBytes: row.SizeBytes,
		Filename:  row.Filename,
		SHA256:    row.SHA256,
	}
}

// verifyRenderAdmission verifies a render-admission-backed app-tool-call
// against the CURRENT render tuple BEFORE invocation. It binds the exact
// current registry descriptor generation (never an empty one) and maps
// every admission outcome onto its exact typed Protocol code — an
// otherwise-current App with an unavailable / invalid / expired /
// mismatched admission fails here, never as an ambiguous not-found.
func (s *AppsSurface) verifyRenderAdmission(ctx context.Context, id identity.Identity, agentID, serverID, resourceURI, token string) *protoerrors.Error {
	method := methods.MethodMCPAppsCallTool
	if s.admissionAuthority == nil || s.descriptorGeneration == nil {
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: render-admission authority is not wired on this runtime", string(method))
	}
	if token == "" {
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionMissing,
			"method %q: no render-admission token supplied", string(method))
	}
	generation, err := s.descriptorGeneration.DescriptorGeneration(ctx, serverID, resourceURI)
	if err != nil {
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: current descriptor generation could not be resolved: %v", string(method), err)
	}
	if generation == "" {
		// An otherwise-current App whose current generation cannot be
		// established is refused typed (the admission binds a generation,
		// and there is none to bind) — never a collapse into not-found.
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionUnavailable,
			"method %q: the current descriptor generation for the render tuple is not resolvable", string(method))
	}
	if _, err := s.admissionAuthority.Verify(ctx, admission.RenderTuple{
		Identity:              id,
		AgentID:               agentID,
		ServerID:              serverID,
		ResourceURI:           resourceURI,
		DescriptorFingerprint: generation,
	}, token); err != nil {
		return mapRenderAdmissionError(string(method), err)
	}
	return nil
}

// mapRenderAdmissionError maps an admission-authority error onto its
// exact typed Protocol code. The five outcome sentinels map 1:1; a
// non-outcome error (a seam failure, a ctx error) maps to
// CodeRuntimeError — never a silent success.
func mapRenderAdmissionError(method string, err error) *protoerrors.Error {
	switch {
	case stderrors.Is(err, admission.ErrTokenMissing):
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionMissing,
			"method %q: render admission is missing", method)
	case stderrors.Is(err, admission.ErrTokenUnavailable):
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionUnavailable,
			"method %q: render admission could not be opened", method)
	case stderrors.Is(err, admission.ErrTokenInvalid):
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionInvalid,
			"method %q: render admission is structurally invalid", method)
	case stderrors.Is(err, admission.ErrTokenExpired):
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionExpired,
			"method %q: render admission is expired", method)
	case stderrors.Is(err, admission.ErrTokenMismatch):
		return protoerrors.Newf(protoerrors.CodeRenderAdmissionMismatch,
			"method %q: render admission does not match the current render tuple", method)
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: render admission verification failed: %v", method, err)
	}
}
