package types

import "encoding/json"

// MCP Apps wire types — the canonical contract for the MCP Apps host
// surface (the official `io.modelcontextprotocol/ui` extension). An MCP
// tool declares an interactive HTML UI via a `ui://` resource referenced
// from a tool result's `_meta.ui.resourceUri`; the Console renders that
// UI in a sandboxed iframe. These flat wire shapes are the only thing
// that crosses to the Console — runtime Go structs never leak (RFC §5.1
// / CLAUDE.md §13 single-source rule).
//
// Three methods consume these types:
//
//   - mcp.servers.read_resource — ReadMCPResourceRequest →
//     ReadMCPResourceResponse. Fetches a `ui://` resource's HTML scoped
//     to the request identity triple, heavy-content-aware: content at or
//     above the heavy threshold rides by reference, never inline.
//   - mcp.apps.call_tool — MCPAppCallToolRequest →
//     MCPAppCallToolResponse. An app-initiated tool invocation proxied
//     back through the SAME identity + approval-gate + tool-side-OAuth
//     path a planner-initiated call uses — never a parallel path.
//   - mcp.apps.tool_context — ToolContextRequest → ToolContextResponse.
//     Reads the identity-scoped captured input/result that produced an app;
//     it performs no credential or agent-config selection.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): an incomplete IdentityScope fails closed at the wire edge
// with CodeIdentityRequired.

// MCPAppRef is the host-side projection of an MCP App reference parsed
// from a tool result's `_meta.ui.resourceUri` slot. It is empty (nil on
// the response) for an ordinary, non-app tool result. The DisplayMode
// is the negotiated value the host reconciles from the server's
// advertised capability set; RawHTMLTrusted is the per-server raw-HTML
// trust posture (default-deny).
type MCPAppRef struct {
	// AgentID is the effective agent configuration that produced this app.
	// It is runtime-authored discovery authority, not an isolation principal:
	// the Console echoes it on resource reads and app tool calls, and the
	// Runtime re-runs signed reach + tenant-local resolution before use.
	// Empty is the backward-compatible pre-v1.26.11/default-agent shape.
	AgentID string `json:"agent_id,omitempty"`
	// ServerID is the MCP server (source id) hosting the app's UI document
	// and tools. The Console pairs it with ResourceURI to fetch the
	// document via mcp.servers.read_resource — without it the renderer
	// cannot resolve which server to read from. Empty for a non-app result.
	ServerID string `json:"server_id,omitempty"`
	// ToolCallID is the stable per-invocation id of the tool call that
	// declared the app. The Console pairs it with ServerID to fetch the
	// tool context (input + lowered result) via mcp.apps.tool_context.
	// Empty for a non-app result, and empty when no tool context was
	// captured for the invocation — a non-empty value promises a fetchable
	// record, so a client may treat a miss as an expired one.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ResourceURI is the `ui://`-scheme URI of the app's UI document.
	// The Console fetches the document via mcp.servers.read_resource.
	ResourceURI string `json:"resource_uri"`
	// DisplayMode is the negotiated MCP-Apps display mode for this
	// result (one of inline / fullscreen / pip), or empty when the
	// server stated no preference and none was negotiated.
	DisplayMode string `json:"display_mode,omitempty"`
	// RawHTMLTrusted reports the per-server raw-HTML trust flag — the
	// default-deny posture means the host sandboxes the iframe unless an
	// operator explicitly opted in (mcp.servers.set_raw_html_trust).
	RawHTMLTrusted bool `json:"raw_html_trusted"`
	// Binding is an opaque host/render-issued callback capability. Apps cannot
	// author it; the trusted host echoes it on callback requests.
	Binding string `json:"binding,omitempty"`
}

// MCPResourceArtifactRef is the by-reference shape the MCP Apps surface
// returns when fetched content (a `ui://` HTML document, or an app-tool-
// call result) meets or exceeds the configured heavy-content threshold
// (RFC §6.5). It mirrors a subset of `internal/artifacts.Ref`
// but is a flat wire type — the Console fetches the bytes via the
// artifacts surface against this stub. It is the same flat shape
// MemoryArtifactRef uses, kept distinct so a future divergence does not
// whipsaw either surface.
type MCPResourceArtifactRef struct {
	// ID is the content-addressed identifier (`{namespace}_{sha256[:12]}`).
	ID string `json:"id"`
	// MimeType is the IANA media type, when known.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the length of the referenced bytes.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Filename is metadata only (never used for path construction).
	Filename string `json:"filename,omitempty"`
	// SHA256 is the full hex digest of the referenced bytes.
	SHA256 string `json:"sha256,omitempty"`
}

// RenderAdmission is the bounded render-admission object a successful
// OPT-IN `ui://` resource read may return. It carries ONLY the opaque
// sealed token, its expiry metadata, and the closed availability status —
// never the sealed claims, never key material, and never a
// provider-local live binding. The token is opaque by construction: no
// identity / agent / server / resource / generation component is
// recoverable from it. A client echoes the token value back on
// `mcp.apps.call_tool` (the request's distinct `render_admission` field);
// the Runtime re-verifies it against the CURRENT render tuple before
// invocation.
type RenderAdmission struct {
	// Token is the opaque sealed render-admission token (bounded
	// base64url). Empty only when Availability is "unavailable".
	Token string `json:"token,omitempty"`
	// IssuedAt is the mint instant, RFC 3339 UTC ("unavailable" mint —
	// never issued). Omitted when unavailable.
	IssuedAt string `json:"issued_at,omitempty"`
	// ExpiresAt is the admission expiry, RFC 3339 UTC. The Runtime
	// re-verifies against its own clock at call time; a past expiry is
	// refused with the typed `render_admission_expired` code.
	ExpiresAt string `json:"expires_at"`
	// Availability is the CLOSED availability status of the admission at
	// mint: "available" (the render tuple was fully verified and the
	// token is usable) or "unavailable" (the admission could not be
	// minted — the caller must re-read). Omitted/false
	// `request_render_admission` never produces this object at all.
	Availability string `json:"availability"`
}

// Render-admission availability statuses (the closed set).
const (
	// RenderAdmissionAvailable — the admission was minted and its token
	// is usable.
	RenderAdmissionAvailable = "available"
	// RenderAdmissionUnavailable — the admission could not be minted;
	// the caller must re-read the resource.
	RenderAdmissionUnavailable = "unavailable"
)

// ReadMCPResourceRequest is the `mcp.servers.read_resource` request
// body. Identity is mandatory.
type ReadMCPResourceRequest struct {
	// Identity is the (tenant, user, session) scope the read runs under.
	// Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// AgentID selects the effective agent configuration whose MCP provider
	// authority the read may consume. The Runtime resolves an omitted value to
	// its configured default, then re-authorizes signed reach before lookup.
	AgentID string `json:"agent_id,omitempty"`
	// ServerID is the MCP server (source id) hosting the resource.
	ServerID string `json:"server_id"`
	// ResourceURI is the resource to fetch — the `ui://`-scheme URI of
	// an MCP App's UI document for app fetches.
	ResourceURI string `json:"resource_uri"`
	// RequestRenderAdmission is the OPT-IN flag that asks the Runtime to
	// mint a bounded render admission for THIS successful `ui://` read.
	// Omitted or false preserves the current ordinary resource-read
	// behavior byte-for-byte and mints NO callback authority — only a
	// successful `ui://` read carrying true may return the
	// `render_admission` object, and the mint happens only AFTER the
	// full verified identity / reach / retirement / erasure / current
	// exposure / exact server+resource checks pass.
	RequestRenderAdmission bool `json:"request_render_admission,omitempty"`
}

// ReadMCPResourceResponse is the `mcp.servers.read_resource` reply.
// EXACTLY ONE of Content / ArtifactRef is set. Content carries the
// inline bytes when the resource is below the heavy-content threshold;
// ArtifactRef carries the by-reference stub when it meets or exceeds the
// threshold (a loud bypass event accompanies the offload — never a
// silent truncation, never an inline leak).
type ReadMCPResourceResponse struct {
	// ResourceURI echoes the fetched resource URI.
	ResourceURI string `json:"resource_uri"`
	// MIMEType is the resource's declared media type.
	MIMEType string `json:"mime_type,omitempty"`
	// Content is the inline resource bytes, populated ONLY when the
	// content is below the heavy-content threshold. Empty when
	// ArtifactRef is set.
	Content string `json:"content,omitempty"`
	// ArtifactRef is the by-reference stub, populated when the content
	// meets or exceeds the heavy-content threshold. Nil when Content is
	// set.
	ArtifactRef *MCPResourceArtifactRef `json:"artifact_ref,omitempty"`
	// RenderAdmission is the bounded render admission minted for this
	// read, present ONLY when the request carried
	// `request_render_admission: true` AND the read succeeded AND the
	// admission could be minted. Nil otherwise — an omitted/false flag
	// never mints authority, and a failed read never returns an
	// admission.
	RenderAdmission *RenderAdmission `json:"render_admission,omitempty"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPAppCallToolRequest is the `mcp.apps.call_tool` request body — an
// MCP App asking the host to invoke an MCP tool. The request carries the
// identity triple from the Protocol context; the runtime re-enters the
// EXISTING tool-invocation path (approval gate + tool-side OAuth +
// identity), so an app call to a gated tool parks on the unified pause
// primitive exactly as a planner call does. Identity is mandatory.
type MCPAppCallToolRequest struct {
	// Identity is the (tenant, user, session) scope the tool call runs
	// under. Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// AgentID selects the effective agent configuration whose current tool
	// exposure and provider authority govern this call. It is copied from the
	// runtime-authored app reference; the Runtime re-authorizes it before use.
	// Omission resolves to the configured default for older clients.
	AgentID string `json:"agent_id,omitempty"`
	// ServerID is the HOST-DERIVED identity of the MCP server whose rendered
	// App issued this call — the value the host pairs with the App's render
	// context, NEVER a value the App supplies about itself. The Runtime
	// verifies it against the resolved tool's source before invocation: an
	// app-only callback (`_meta.ui.visibility: ["app"]`) resolves ONLY
	// through its own server's App dispatch catalog, so an app-only call
	// without this field (or naming another server) is refused before
	// execution. An ordinary tool called with a server_id that disagrees
	// with its source is likewise refused; an ordinary tool called without
	// one keeps the legacy behavior (the field is optional for
	// backward-compatible clients).
	ServerID string `json:"server_id,omitempty"`
	Binding  string `json:"binding,omitempty"`
	// RenderAdmission is the DISTINCT opaque render-admission authority
	// minted by a successful opt-in `ui://` read
	// (`request_render_admission: true`) and echoed back verbatim. It is
	// NOT the legacy `binding` field: a request that supplies BOTH is
	// refused as ambiguous (`render_authority_ambiguous`) — the Runtime
	// never guesses which authority the App meant. When supplied alone,
	// the Runtime verifies it against the CURRENT render tuple
	// (identity / agent / server / resource URI / current descriptor
	// generation) before invocation; an unavailable / invalid / expired
	// / mismatched admission is refused with its exact typed code, never
	// collapsed into a generic not-found.
	RenderAdmission string `json:"render_admission,omitempty"`
	// ResourceURI is host-supplied render authority, never sandbox-authored.
	ResourceURI string `json:"resource_uri,omitempty"`
	// Tool is the catalog tool name to invoke (the Harbor-side
	// `<source>_<tool>` name).
	Tool string `json:"tool"`
	// Arguments is the raw JSON argument object the tool's schema
	// validates on the wire.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// MCPAppCallToolResponse is the `mcp.apps.call_tool` reply. EXACTLY ONE
// of Content / ArtifactRef is set (the heavy-content discipline matches
// ReadMCPResourceResponse). App is non-nil when the tool result declared
// a `ui://` MCP App via `_meta.ui.resourceUri`, empty otherwise.
type MCPAppCallToolResponse struct {
	// Tool echoes the invoked tool name.
	Tool string `json:"tool"`
	// Content is the inline tool-result JSON, populated ONLY when the
	// encoded result is below the heavy-content threshold. Empty when
	// ArtifactRef is set.
	Content json.RawMessage `json:"content,omitempty"`
	// ArtifactRef is the by-reference stub, populated when the encoded
	// result meets or exceeds the heavy-content threshold. Nil when
	// Content is set.
	ArtifactRef *MCPResourceArtifactRef `json:"artifact_ref,omitempty"`
	// IsError reports whether the MCP server returned a tool error
	// (the result's IsError flag) — the app renders it as a failure.
	IsError bool `json:"is_error"`
	// App is the MCP App reference parsed from the tool result's
	// `_meta.ui.resourceUri`, or nil for a non-app result.
	App *MCPAppRef `json:"app,omitempty"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// ToolContextRequest is the `mcp.apps.tool_context` request body — a
// rendered MCP App asking the host for the tool context (input arguments +
// lowered result) that produced it. The request carries the identity
// triple from the Protocol context; the runtime resolves the captured
// context scoped to that triple, so an app can never read a context
// captured under a different identity (a cross-identity id fails with
// CodeNotFound). Identity is mandatory.
type ToolContextRequest struct {
	// Identity is the (tenant, user, session) scope the read runs under.
	// Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// ServerID is the MCP server (source id) the tool belongs to — the
	// value carried on the `mcp.app_available` discovery event.
	ServerID string `json:"server_id"`
	// ToolCallID is the stable per-invocation id of the tool call that
	// declared the app — also carried on the discovery event.
	ToolCallID string `json:"tool_call_id"`
}

// ToolContextPayload is one half (input or result) of a tool context.
// EXACTLY ONE of Content / ArtifactRef is set: Content carries the inline
// JSON when the payload is below the heavy-content threshold; ArtifactRef
// carries the by-reference stub when it met or exceeded the threshold at
// capture time (the Console fetches the bytes via the artifacts surface).
type ToolContextPayload struct {
	// Content is the inline JSON, populated ONLY when the payload was
	// captured below the heavy-content threshold. Empty when ArtifactRef
	// is set.
	Content json.RawMessage `json:"content,omitempty"`
	// ArtifactRef is the by-reference stub, populated when the payload met
	// or exceeded the heavy-content threshold at capture. Nil when Content
	// is set.
	ArtifactRef *MCPResourceArtifactRef `json:"artifact_ref,omitempty"`
}

// ToolContextResponse is the `mcp.apps.tool_context` reply — the captured
// input + lowered result that produced the rendered app. Each of Input /
// Result is inline-or-by-reference (the heavy-content discipline matches
// ReadMCPResourceResponse): a large result rides by reference, never
// inline. An unknown or cross-identity (server_id, tool_call_id) fails
// with CodeNotFound rather than returning an empty response.
type ToolContextResponse struct {
	// Tool echoes the server-side tool name that declared the app.
	Tool string `json:"tool"`
	// Input is the tool's input arguments (inline JSON or by reference).
	Input ToolContextPayload `json:"input"`
	// Result is the tool's lowered result (inline JSON or by reference).
	Result ToolContextPayload `json:"result"`
	// IsError reports whether the tool returned a server-side error result.
	IsError bool `json:"is_error"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}
