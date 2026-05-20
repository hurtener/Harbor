package types

import "time"

// Phase 73k (D-119) — the MCP-Connections-page wire types.
//
// Phase 73k ships the operator control plane for Harbor's MCP southbound
// surface (Phase 28). It lands twelve Protocol methods under the
// `mcp.servers.*` namespace — nine read methods, three admin verbs:
//
//	mcp.servers.list / get / resources / prompts / refresh_discovery /
//	probe / health / bindings.list / policy   (reads)
//	mcp.servers.refresh_binding / revoke_binding /
//	set_raw_html_trust                         (admin verbs)
//
// Every type here is a flat, Protocol-owned struct — never a re-export
// of an internal Runtime Go type (RFC §5.1 reject-on-sight smell). The
// MCPSurface translates the runtime-side `mcp` driver's projection types
// onto these wire shapes; the Console never sees the MCP SDK internals.
//
// Identity is mandatory on every request (RFC §5.5). The admin verbs
// gate additionally on the `auth.ScopeAdmin` claim (D-079 closed-set —
// no new scope is minted for MCP).

// MCPServerStateView is the canonical state chip a Console renders for an
// MCP server row. The V1 set is closed.
type MCPServerStateView string

// The canonical MCP server state values.
const (
	// MCPStateOnline — the transport is connected and the last
	// discovery / probe succeeded.
	MCPStateOnline MCPServerStateView = "online"
	// MCPStateReconnecting — the transport dropped and the driver is
	// re-establishing the session.
	MCPStateReconnecting MCPServerStateView = "reconnecting"
	// MCPStateOffline — the transport is down (never connected, or
	// closed).
	MCPStateOffline MCPServerStateView = "offline"
	// MCPStateAuthPending — the server requires an OAuth binding that
	// has not been completed.
	MCPStateAuthPending MCPServerStateView = "auth_pending"
	// MCPStateError — the last discovery / probe failed with a
	// transport error.
	MCPStateError MCPServerStateView = "error"
)

// MCPServerView is the per-row payload returned by mcp.servers.list and
// (extended) by mcp.servers.get. It is a flat projection of the runtime
// MCP driver's per-server state — never a re-export of the driver type.
type MCPServerView struct {
	// Name is the unique MCP server / source id.
	Name string `json:"name"`
	// Transport is the wire transport — "stdio", "http+sse",
	// "streamable-http", or "websocket".
	Transport string `json:"transport"`
	// URLOrCommand is the transport-prefixed endpoint URL (HTTP
	// transports) or the argv-form command string (stdio).
	URLOrCommand string `json:"url_or_command"`
	// State is the canonical state chip.
	State MCPServerStateView `json:"state"`
	// LastDiscoveryAt is the wall-clock instant of the last successful
	// discovery. Zero when discovery has never run.
	LastDiscoveryAt time.Time `json:"last_discovery_at"`
	// ToolCount is the number of tools the server advertises.
	ToolCount int32 `json:"tool_count"`
	// ResourceCount is the number of resources the server advertises.
	ResourceCount int32 `json:"resource_count"`
	// PromptCount is the number of prompts the server advertises.
	PromptCount int32 `json:"prompt_count"`
	// RecentLatencyMs is the most recent observed handshake / probe
	// latency in milliseconds.
	RecentLatencyMs int64 `json:"recent_latency_ms"`
	// ErrorRatePerMin is the transport-error rate over the recent
	// window (errors per minute).
	ErrorRatePerMin float64 `json:"error_rate_per_min"`
	// OAuthBindingCount is the number of OAuth bindings configured for
	// this server.
	OAuthBindingCount int32 `json:"oauth_binding_count"`
	// RawHTMLTrusted reports whether the per-server raw-HTML opt-in flag
	// is set. Default false (default-deny — brief 11 §8).
	RawHTMLTrusted bool `json:"raw_html_trusted"`
}

// MCPServersListRequest is the wire shape for mcp.servers.list — a paged,
// filterable list of the configured MCP southbound servers.
type MCPServersListRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// State filters to servers in any of the given states. Empty = all.
	State []MCPServerStateView `json:"state,omitempty"`
	// Transport filters to servers on any of the given transports.
	Transport []string `json:"transport,omitempty"`
	// HasOAuth, when set, filters to servers with (true) / without
	// (false) at least one OAuth binding.
	HasOAuth *bool `json:"has_oauth,omitempty"`
	// HasRecentError, when set, filters to servers with (true) /
	// without (false) a recent transport error.
	HasRecentError *bool `json:"has_recent_error,omitempty"`
	// NamePrefix filters to servers whose name has the given prefix.
	NamePrefix string `json:"name_prefix,omitempty"`
	// PageToken is the opaque cursor from a prior response. Empty = the
	// first page.
	PageToken string `json:"page_token,omitempty"`
	// PageSize is the requested maximum row count. Zero = the surface
	// default; the surface clamps to a maximum.
	PageSize int32 `json:"page_size,omitempty"`
}

// MCPServersListResponse is the mcp.servers.list reply.
type MCPServersListResponse struct {
	// Servers is the page of server rows. Always non-nil in the wire
	// JSON (an empty list is `[]`, never `null`).
	Servers []MCPServerView `json:"servers"`
	// NextPageToken is the cursor for the next page, or empty when this
	// is the last page.
	NextPageToken string `json:"next_page_token,omitempty"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerGetRequest is the wire shape for mcp.servers.get — a
// single-server detail read.
type MCPServerGetRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name to inspect. Required.
	Name string `json:"name"`
}

// MCPToolPolicyView is the read-only projection of an MCP server's
// ToolPolicy (D-024). It is read-only at V1 (policy editing is post-V1).
type MCPToolPolicyView struct {
	// TimeoutMs is the per-invocation timeout in milliseconds.
	TimeoutMs int64 `json:"timeout_ms"`
	// MaxRetries is the retry cap.
	MaxRetries int32 `json:"max_retries"`
	// ConcurrencyCap is the per-server concurrent-invocation cap. Zero
	// means unbounded.
	ConcurrencyCap int32 `json:"concurrency_cap"`
}

// MCPBindingScopeCount is one (binding-scope, count) pair in a
// per-server bindings summary.
type MCPBindingScopeCount struct {
	// BindingScope is the auth.BindingScope value ("user" / "agent").
	BindingScope string `json:"binding_scope"`
	// Count is the number of bindings at that scope.
	Count int32 `json:"count"`
}

// MCPServerGetResponse is the mcp.servers.get reply — the list-row shape
// plus per-server detail fields.
type MCPServerGetResponse struct {
	// Server is the per-server row shape.
	Server MCPServerView `json:"server"`
	// DisplayModesAdvertised lists the MCP-Apps DisplayMode values the
	// server declares (D-062). Always non-nil in the wire JSON.
	DisplayModesAdvertised []string `json:"display_modes_advertised"`
	// ContentShapes lists the canonical content shapes the server's
	// tools return ("string" / "ImageRef" / ...). Always non-nil.
	ContentShapes []string `json:"content_shapes"`
	// ToolPolicy is the read-only ToolPolicy projection.
	ToolPolicy MCPToolPolicyView `json:"tool_policy"`
	// BindingsSummary is the per-scope binding count rollup. Always
	// non-nil in the wire JSON.
	BindingsSummary []MCPBindingScopeCount `json:"bindings_summary"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPResourceView is one advertised MCP resource.
type MCPResourceView struct {
	// URI is the resource URI.
	URI string `json:"uri"`
	// MimeType is the resource MIME type, or empty when the server
	// declared none.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the declared resource size, or zero when unknown.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Name is the resource short name.
	Name string `json:"name,omitempty"`
	// Title is the resource human-readable title.
	Title string `json:"title,omitempty"`
}

// MCPServerResourcesRequest is the wire shape for mcp.servers.resources.
type MCPServerResourcesRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerResourcesResponse is the mcp.servers.resources reply.
type MCPServerResourcesResponse struct {
	// Resources is the advertised resource list. Always non-nil.
	Resources []MCPResourceView `json:"resources"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPPromptArg is one declared argument of an MCP prompt.
type MCPPromptArg struct {
	// Name is the argument name.
	Name string `json:"name"`
	// Description is the argument description.
	Description string `json:"description,omitempty"`
	// Required reports whether the argument is mandatory.
	Required bool `json:"required"`
}

// MCPPromptView is one advertised MCP prompt.
type MCPPromptView struct {
	// Name is the prompt name.
	Name string `json:"name"`
	// Description is the prompt description.
	Description string `json:"description,omitempty"`
	// Arguments is the declared argument list. Always non-nil.
	Arguments []MCPPromptArg `json:"arguments"`
}

// MCPServerPromptsRequest is the wire shape for mcp.servers.prompts.
type MCPServerPromptsRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerPromptsResponse is the mcp.servers.prompts reply.
type MCPServerPromptsResponse struct {
	// Prompts is the advertised prompt list. Always non-nil.
	Prompts []MCPPromptView `json:"prompts"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerRefreshDiscoveryRequest is the wire shape for
// mcp.servers.refresh_discovery — a control-plane verb.
type MCPServerRefreshDiscoveryRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerRefreshDiscoveryResponse is the mcp.servers.refresh_discovery
// reply — the new counts plus a discovery-id for log correlation.
type MCPServerRefreshDiscoveryResponse struct {
	// DiscoveryID is an opaque id correlating this refresh with the
	// runtime logs.
	DiscoveryID string `json:"discovery_id"`
	// ToolCount / ResourceCount / PromptCount are the post-refresh
	// advertised counts.
	ToolCount     int32 `json:"tool_count"`
	ResourceCount int32 `json:"resource_count"`
	PromptCount   int32 `json:"prompt_count"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerProbeRequest is the wire shape for mcp.servers.probe — a
// control-plane verb that runs a transport ping / tools-list round-trip.
type MCPServerProbeRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerProbeResponse is the mcp.servers.probe reply.
type MCPServerProbeResponse struct {
	// OK reports whether the probe round-trip succeeded.
	OK bool `json:"ok"`
	// LatencyMs is the probe round-trip latency in milliseconds.
	LatencyMs int64 `json:"latency_ms"`
	// Error carries the probe failure message when OK is false.
	Error string `json:"error,omitempty"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPHealthBucket is one handshake-latency sparkline bucket.
type MCPHealthBucket struct {
	// StartMs is the bucket start (Unix milliseconds).
	StartMs int64 `json:"start_ms"`
	// LatencyMs is the observed handshake latency for the bucket.
	LatencyMs int64 `json:"latency_ms"`
}

// MCPReconnect is one reconnect-history entry.
type MCPReconnect struct {
	// OccurredAt is the reconnect instant.
	OccurredAt time.Time `json:"occurred_at"`
	// Reason carries the reconnect cause.
	Reason string `json:"reason,omitempty"`
}

// MCPServerHealthRequest is the wire shape for mcp.servers.health.
type MCPServerHealthRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerHealthResponse is the mcp.servers.health reply.
type MCPServerHealthResponse struct {
	// HandshakeLatencyBuckets is the latency sparkline. Always non-nil.
	HandshakeLatencyBuckets []MCPHealthBucket `json:"handshake_latency_buckets"`
	// ReconnectHistory is the reconnect-history list. Always non-nil.
	ReconnectHistory []MCPReconnect `json:"reconnect_history"`
	// TransportErrorRate is the transport-error rate (errors / minute).
	TransportErrorRate float64 `json:"transport_error_rate"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPBindingView is one OAuth binding row. It NEVER carries token
// plaintext — only the binding metadata (D-083 invariant).
type MCPBindingView struct {
	// PrincipalID is the bound principal — a user id (ScopeUser) or an
	// agent id (ScopeAgent).
	PrincipalID string `json:"principal_id"`
	// BindingScope is the auth.BindingScope value ("user" / "agent").
	BindingScope string `json:"binding_scope"`
	// Scopes is the configured OAuth scope list. Always non-nil.
	Scopes []string `json:"scopes"`
	// ExpiresAt is the access-token expiry, or zero when no token is
	// bound yet.
	ExpiresAt time.Time `json:"expires_at"`
	// LastUsedAt is the last-use instant, or zero when never used.
	LastUsedAt time.Time `json:"last_used_at"`
}

// MCPServerBindingsListRequest is the wire shape for
// mcp.servers.bindings.list.
type MCPServerBindingsListRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerBindingsListResponse is the mcp.servers.bindings.list reply.
type MCPServerBindingsListResponse struct {
	// Bindings is the OAuth binding list. Always non-nil (may be empty).
	Bindings []MCPBindingView `json:"bindings"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerPolicyRequest is the wire shape for mcp.servers.policy.
type MCPServerPolicyRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
}

// MCPServerPolicyResponse is the mcp.servers.policy reply — the
// read-only ToolPolicy projection.
type MCPServerPolicyResponse struct {
	// ToolPolicy is the read-only ToolPolicy projection.
	ToolPolicy MCPToolPolicyView `json:"tool_policy"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerRefreshBindingRequest is the wire shape for
// mcp.servers.refresh_binding — an admin verb (auth.ScopeAdmin).
type MCPServerRefreshBindingRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
	// PrincipalID is the binding principal to (re)connect. Optional —
	// empty drives the flow for the caller's own ScopeUser binding.
	PrincipalID string `json:"principal_id,omitempty"`
}

// MCPServerRefreshBindingResponse is the mcp.servers.refresh_binding
// reply — the AuthorizeURL the Console opens in a popup, plus the
// flow state for matching the completion event.
type MCPServerRefreshBindingResponse struct {
	// AuthorizeURL is the OAuth authorization endpoint URL the Console
	// navigates the popup to. NEVER a token.
	AuthorizeURL string `json:"authorize_url"`
	// State is the opaque flow-state the Console matches against the
	// subsequent tool.auth_completed event.
	State string `json:"state"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerRevokeBindingRequest is the wire shape for
// mcp.servers.revoke_binding — an admin verb (auth.ScopeAdmin).
type MCPServerRevokeBindingRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
	// PrincipalID is the binding principal to revoke. Optional — empty
	// revokes the caller's own ScopeUser binding.
	PrincipalID string `json:"principal_id,omitempty"`
}

// MCPServerRevokeBindingResponse is the mcp.servers.revoke_binding reply.
type MCPServerRevokeBindingResponse struct {
	// Revoked reports whether a binding was revoked.
	Revoked bool `json:"revoked"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// MCPServerSetRawHTMLTrustRequest is the wire shape for
// mcp.servers.set_raw_html_trust — an admin verb (auth.ScopeAdmin).
type MCPServerSetRawHTMLTrustRequest struct {
	// Identity is the mandatory caller identity scope.
	Identity IdentityScope `json:"identity"`
	// Name is the MCP server name. Required.
	Name string `json:"name"`
	// Trusted is the new per-server raw-HTML trust flag value.
	Trusted bool `json:"trusted"`
}

// MCPServerSetRawHTMLTrustResponse is the mcp.servers.set_raw_html_trust
// reply.
type MCPServerSetRawHTMLTrustResponse struct {
	// Name is the MCP server name the flag was set on.
	Name string `json:"name"`
	// Trusted is the persisted raw-HTML trust flag value.
	Trusted bool `json:"trusted"`
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}
