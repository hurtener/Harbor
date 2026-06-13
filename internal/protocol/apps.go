package protocol

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// AppsSurface is the transport-agnostic Harbor Protocol MCP Apps
// handler. It owns the two MCP Apps host methods — the
// `mcp.servers.read_resource` `ui://` UI-document fetch and the
// `mcp.apps.call_tool` app-tool-call proxy — that back the Console's
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
// never imports the `mcp` driver or the tool catalog.
//
// AppsSurface is immutable after construction and safe for concurrent
// use by N goroutines — Dispatch reads request-specific data from ctx +
// the request argument, never from the surface struct.
type AppsSurface struct {
	resource MCPResourceReader
	invoker  AppToolInvoker
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
	ResourceURI    string
	DisplayMode    string
	RawHTMLTrusted bool
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
// planner uses and invoke the SAME wrapped descriptor, so the approval
// gate + tool-side OAuth fire — there is no parallel path.
//
// CallTool is identity-mandatory: the implementation reads the triple
// from ctx (the gate wrapper's `identity.From`) and fails closed on a
// missing one.
type AppToolInvoker interface {
	CallTool(ctx context.Context, tool string, args json.RawMessage) (MCPAppToolResultRow, error)
}

// AppsDeps bundles the runtime-side seams an AppsSurface reads through.
type AppsDeps struct {
	// Resource is the `ui://` resource reader (the MCP registry adapter).
	// Mandatory.
	Resource MCPResourceReader
	// Invoker is the app-tool-call proxy (the tool-catalog adapter).
	// Mandatory.
	Invoker AppToolInvoker
}

// ErrAppsMisconfigured — NewAppsSurface was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5).
var ErrAppsMisconfigured = stderrors.New("protocol: AppsSurface missing a mandatory dependency")

// NewAppsSurface builds the Protocol MCP Apps surface. Both the resource
// reader and the tool-call invoker are mandatory; a nil fails loud with a
// wrapped ErrAppsMisconfigured.
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
	return &AppsSurface{resource: deps.Resource, invoker: deps.Invoker}, nil
}

// Dispatch is the single transport-agnostic entry point for an MCP Apps
// method call. method MUST be one of the two MCP Apps methods
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
	id, perr := gateAppsIdentity(method, r.Identity)
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
	return resp, nil
}

// handleCallTool serves mcp.apps.call_tool — the app-tool-call proxy.
func (s *AppsSurface) handleCallTool(ctx context.Context, req any) (any, error) {
	method := methods.MethodMCPAppsCallTool
	r, ok := req.(*types.MCPAppCallToolRequest)
	if !ok || r == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: expected *types.MCPAppCallToolRequest, got %T", string(method), req)
	}
	id, perr := gateAppsIdentity(method, r.Identity)
	if perr != nil {
		return nil, perr
	}
	if r.Tool == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: tool is required", string(method))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	// The invoker re-enters the EXISTING approval / OAuth / identity
	// tool-invocation path — a gated tool parks on the unified pause
	// primitive here, never bypassed.
	res, err := s.invoker.CallTool(idCtx, r.Tool, r.Arguments)
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
			ResourceURI:    res.App.ResourceURI,
			DisplayMode:    res.App.DisplayMode,
			RawHTMLTrusted: res.App.RawHTMLTrusted,
		}
	}
	return resp, nil
}

// gateAppsIdentity validates the request's identity triple at the edge.
// Every MCP Apps method is identity-mandatory; an incomplete triple
// fails closed with CodeIdentityRequired (CLAUDE.md §6 rule 9). There is
// no admin-scope gate — the proxy's tool-safety gates fire inside the
// invocation path, and a resource read is a plain identity-scoped read.
func gateAppsIdentity(method methods.Method, scope types.IdentityScope) (identity.Identity, *protoerrors.Error) {
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
