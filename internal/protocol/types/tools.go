package types

import "time"

// Phase 73f (Wave 13 / D-116) — the Console Tools-page wire types.
//
// These structs are the single source of truth (D-002) for the seven
// `tools.*` Protocol methods the Console Tools page consumes:
//
//   - tools.list           — ToolListRequest  → ToolListResponse
//   - tools.get            — ToolGetRequest   → Tool
//   - tools.describe       — ToolDescribeRequest → ToolManifest
//   - tools.metrics        — ToolMetricsRequest → ToolMetrics
//   - tools.content_stats  — ToolContentStatsRequest → ToolContentStats
//   - tools.set_approval_policy — ToolSetApprovalPolicyRequest → ToolSetApprovalPolicyResponse
//   - tools.revoke_oauth   — ToolRevokeOAuthRequest → ToolRevokeOAuthResponse
//
// The wire vocabulary is the Protocol's own (RFC §5.1 / CLAUDE.md §13
// single-source rule): the Console never reads a runtime-internal Go
// type. `tools.ToolDescriptor`, `tools/auth.BindingScope`,
// `tools/approval.ApprovalPolicy` are runtime concepts; the projector
// in `internal/tools/protocol` maps them onto these flat wire shapes.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): a request whose embedded IdentityScope is incomplete fails
// closed at the wire edge with CodeIdentityRequired. The two admin
// methods additionally require the verified `auth.ScopeAdmin` claim
// (D-079; there is NO `tools.admin` scope) — a missing claim fails
// closed with CodeIdentityScopeRequired (HTTP 403).

// Tools-page pagination bounds for `tools.list`. Mirrors the
// pause.list / search.* contract so a future Console-side pagination
// component is shared, not re-implemented per page. A request above
// MaxToolListPageSize gets a 400 (CodeInvalidRequest) — never a silent
// clamp.
const (
	// DefaultToolListPageSize is the page size applied when a
	// ToolListRequest omits PageSize (or passes a non-positive value).
	DefaultToolListPageSize = 50
	// MaxToolListPageSize bounds the page size a client may request.
	MaxToolListPageSize = 200
)

// ToolTransport is the wire enum for a tool's transport. It is the
// Protocol projection of the runtime-internal `tools.TransportKind`.
type ToolTransport string

// Canonical tool transports — the closed set the catalog supports.
const (
	ToolTransportInProc ToolTransport = "in-proc"
	ToolTransportHTTP   ToolTransport = "HTTP"
	ToolTransportMCP    ToolTransport = "MCP"
	ToolTransportA2A    ToolTransport = "A2A"
	ToolTransportFlow   ToolTransport = "flow"
)

// IsValidToolTransport reports whether t is one of the canonical
// tool transports.
func IsValidToolTransport(t ToolTransport) bool {
	switch t {
	case ToolTransportInProc, ToolTransportHTTP, ToolTransportMCP,
		ToolTransportA2A, ToolTransportFlow:
		return true
	}
	return false
}

// ToolOAuthStatus is the wire enum for a tool's OAuth binding status.
type ToolOAuthStatus string

// Canonical OAuth statuses surfaced in the catalog table.
const (
	// ToolOAuthBound — the tool requires OAuth and a live binding exists.
	ToolOAuthBound ToolOAuthStatus = "Bound"
	// ToolOAuthRequired — the tool requires OAuth but no binding exists.
	ToolOAuthRequired ToolOAuthStatus = "Required"
	// ToolOAuthExpired — the tool requires OAuth and the binding lapsed.
	ToolOAuthExpired ToolOAuthStatus = "Expired"
	// ToolOAuthNotApplicable — the tool does not require OAuth.
	ToolOAuthNotApplicable ToolOAuthStatus = "n/a"
)

// IsValidToolOAuthStatus reports whether s is one of the canonical
// OAuth statuses.
func IsValidToolOAuthStatus(s ToolOAuthStatus) bool {
	switch s {
	case ToolOAuthBound, ToolOAuthRequired, ToolOAuthExpired, ToolOAuthNotApplicable:
		return true
	}
	return false
}

// ToolApprovalPolicy is the wire enum for a tool's approval gate.
type ToolApprovalPolicy string

// Canonical approval policies — the closed set `tools.set_approval_policy`
// accepts and the catalog table renders.
const (
	// ToolApprovalAuto — the tool runs without a HITL gate.
	ToolApprovalAuto ToolApprovalPolicy = "auto"
	// ToolApprovalGated — every invocation pauses for HITL approval.
	ToolApprovalGated ToolApprovalPolicy = "gated"
	// ToolApprovalDenied — the tool is administratively denied.
	ToolApprovalDenied ToolApprovalPolicy = "denied"
)

// IsValidToolApprovalPolicy reports whether p is one of the three
// canonical approval policies. `tools.set_approval_policy` rejects any
// other value with CodeInvalidRequest.
func IsValidToolApprovalPolicy(p ToolApprovalPolicy) bool {
	switch p {
	case ToolApprovalAuto, ToolApprovalGated, ToolApprovalDenied:
		return true
	}
	return false
}

// ToolStatus is the wire enum for a tool's health pill.
type ToolStatus string

// Canonical tool health statuses surfaced by `tools.metrics`.
const (
	ToolStatusHealthy  ToolStatus = "Healthy"
	ToolStatusDegraded ToolStatus = "Degraded"
	ToolStatusOffline  ToolStatus = "Offline"
)

// IsValidToolStatus reports whether s is one of the canonical health
// statuses.
func IsValidToolStatus(s ToolStatus) bool {
	switch s {
	case ToolStatusHealthy, ToolStatusDegraded, ToolStatusOffline:
		return true
	}
	return false
}

// ToolMetricsWindow is the wire enum for the `tools.metrics` selectable
// observation window.
type ToolMetricsWindow string

// Canonical metrics windows — the 1h / 24h / 7d toggle in the mockup.
const (
	ToolWindow1h  ToolMetricsWindow = "1h"
	ToolWindow24h ToolMetricsWindow = "24h"
	ToolWindow7d  ToolMetricsWindow = "7d"
)

// IsValidToolMetricsWindow reports whether w is one of the three
// canonical windows.
func IsValidToolMetricsWindow(w ToolMetricsWindow) bool {
	switch w {
	case ToolWindow1h, ToolWindow24h, ToolWindow7d:
		return true
	}
	return false
}

// Tool is the catalog-row projection of a registered tool descriptor.
// It is the row shape the Console Tools-page catalog table renders and
// the payload `tools.get` returns. Flat, low-cardinality strings — the
// Console branches on the enum fields.
type Tool struct {
	// ID is the stable catalog key (the tool's registered name).
	ID string `json:"id"`
	// Name is the planner-facing display name (equal to ID in V1).
	Name string `json:"name"`
	// Version is the tool descriptor version string ("" when unset).
	Version string `json:"version"`
	// Description is the planner-facing summary.
	Description string `json:"description"`
	// Scope is the tool's visibility scope: "tenant" | "agent" | "session".
	Scope string `json:"scope"`
	// Transport discriminates the tool's source.
	Transport ToolTransport `json:"transport"`
	// OAuthStatus is the tool's OAuth binding status.
	OAuthStatus ToolOAuthStatus `json:"oauth_status"`
	// ApprovalPolicy is the tool's HITL approval gate.
	ApprovalPolicy ToolApprovalPolicy `json:"approval_policy"`
	// ReliabilityTier is the operator-facing reliability label
	// (derived from the tool's side-effect class / cost hint).
	ReliabilityTier string `json:"reliability_tier"`
	// Owner is the configured owner / source identifier ("" when unset).
	Owner string `json:"owner"`
	// LastUsedAt is the timestamp of the most recent invocation; the
	// zero value means "never used".
	LastUsedAt time.Time `json:"last_used_at"`
}

// ToolFilter is the server-enforced facet filter on `tools.list`. An
// empty facet slice matches every value on that axis. The filter is
// applied AFTER the identity-scope predicate — it never widens
// visibility.
type ToolFilter struct {
	// Scopes restricts to tools whose Scope is in this set.
	Scopes []string `json:"scopes,omitempty"`
	// Transports restricts to tools whose Transport is in this set.
	Transports []ToolTransport `json:"transports,omitempty"`
	// OAuthStatuses restricts to tools whose OAuthStatus is in this set.
	OAuthStatuses []ToolOAuthStatus `json:"oauth_statuses,omitempty"`
	// ApprovalPolicies restricts to tools whose ApprovalPolicy is in this set.
	ApprovalPolicies []ToolApprovalPolicy `json:"approval_policies,omitempty"`
	// ReliabilityTiers restricts to tools whose ReliabilityTier is in this set.
	ReliabilityTiers []string `json:"reliability_tiers,omitempty"`
	// Search is a free-text substring filter over tool name + version.
	Search string `json:"search,omitempty"`
}

// ToolListRequest is the `tools.list` request body.
type ToolListRequest struct {
	// Identity is the (tenant, user, session) scope the catalog is
	// projected for. Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// Filter is the optional facet filter; the zero value lists every
	// visible tool.
	Filter ToolFilter `json:"filter"`
	// Page is the 1-based page index; a non-positive value means page 1.
	Page int `json:"page,omitempty"`
	// PageSize is the rows-per-page; a non-positive value applies
	// DefaultToolListPageSize. A value above MaxToolListPageSize is a
	// 400 (CodeInvalidRequest) — never a silent clamp.
	PageSize int `json:"page_size,omitempty"`
}

// ToolAggregates carries the four catalog counters the Tools-page
// right-rail overview card renders, computed over the FILTERED view.
type ToolAggregates struct {
	// Total is the count of tools matching the filter.
	Total int64 `json:"total"`
	// Active is the count of tools with a non-zero LastUsedAt.
	Active int64 `json:"active"`
	// PendingApproval is the count of tools whose ApprovalPolicy is
	// "gated".
	PendingApproval int64 `json:"pending_approval"`
	// AwaitingOAuth is the count of tools whose OAuthStatus is
	// "Required" or "Expired".
	AwaitingOAuth int64 `json:"awaiting_oauth"`
}

// ToolListResponse is the `tools.list` reply: a paginated slice of
// catalog rows plus the filtered-view aggregates.
type ToolListResponse struct {
	// Tools is the page of catalog rows, sorted by Name.
	Tools []Tool `json:"tools"`
	// Page is the 1-based page index this response covers.
	Page int `json:"page"`
	// PageSize is the rows-per-page applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages for the filtered view.
	PageCount int `json:"page_count"`
	// TotalRows is the total row count for the filtered view.
	TotalRows int64 `json:"total_rows"`
	// Aggregates carries the four catalog counters for the filtered view.
	Aggregates ToolAggregates `json:"aggregates"`
}

// ToolGetRequest is the `tools.get` request body.
type ToolGetRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool to project.
	ID string `json:"id"`
}

// ToolDescribeRequest is the `tools.describe` request body.
type ToolDescribeRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool to describe.
	ID string `json:"id"`
}

// ToolManifest is the full descriptor projection `tools.describe`
// returns — the read-only manifest the Tools-page Manifest / Inputs /
// Outputs tabs render.
type ToolManifest struct {
	// Tool is the catalog-row projection (same shape as `tools.get`).
	Tool Tool `json:"tool"`
	// SideEffect is the declared side-effect class
	// ("pure" | "read" | "write" | "external" | "stateful").
	SideEffect string `json:"side_effect"`
	// ArgsSchema is the raw JSON-Schema (object) for the tool's
	// argument shape, as a JSON string ("" when the tool declares none).
	ArgsSchema string `json:"args_schema"`
	// OutSchema is the raw JSON-Schema (object) for the tool's result
	// shape, as a JSON string ("" when the tool declares none).
	OutSchema string `json:"out_schema"`
	// Examples carries the canonical argument-shape examples, each a
	// JSON string.
	Examples []string `json:"examples"`
	// AuthScopes lists the scopes a planner step's identity must carry
	// for the tool to be visible.
	AuthScopes []string `json:"auth_scopes"`
	// OAuthBindingScope is the tool's OAuth binding scope per D-083
	// ("user" | "agent" | "" when the tool requires no OAuth).
	OAuthBindingScope string `json:"oauth_binding_scope"`
	// RetryAttempts is the reliability shell's configured retry budget
	// (D-024).
	RetryAttempts int `json:"retry_attempts"`
	// TimeoutMS is the reliability shell's per-attempt timeout in
	// milliseconds (0 when the policy applies its default).
	TimeoutMS int64 `json:"timeout_ms"`
	// LoadingMode is the prompt-time loading mode ("always" | "deferred").
	LoadingMode string `json:"loading_mode"`
	// DisplayModes maps a MIME type to its negotiated MCP-Apps
	// DisplayMode (D-062); empty for non-MCP tools.
	DisplayModes map[string]string `json:"display_modes"`
}

// ToolMetricsRequest is the `tools.metrics` request body.
type ToolMetricsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool to report metrics for.
	ID string `json:"id"`
	// Window selects the observation window; an empty value applies
	// ToolWindow1h.
	Window ToolMetricsWindow `json:"window,omitempty"`
}

// ToolMetrics is the `tools.metrics` reply — per-tool error-rate
// gauges over the three fixed windows plus a status pill for the
// selected window.
type ToolMetrics struct {
	// ID echoes the requested tool ID.
	ID string `json:"id"`
	// Window echoes the resolved observation window.
	Window ToolMetricsWindow `json:"window"`
	// ErrorRate1h is the fraction of failed invocations over the last 1h.
	ErrorRate1h float64 `json:"error_rate_1h"`
	// ErrorRate24h is the fraction of failed invocations over the last 24h.
	ErrorRate24h float64 `json:"error_rate_24h"`
	// ErrorRate7d is the fraction of failed invocations over the last 7d.
	ErrorRate7d float64 `json:"error_rate_7d"`
	// Invocations is the total invocation count over the selected window.
	Invocations int64 `json:"invocations"`
	// Failures is the failed-invocation count over the selected window.
	Failures int64 `json:"failures"`
	// Status is the health pill for the selected window.
	Status ToolStatus `json:"status"`
}

// ToolContentStatsRequest is the `tools.content_stats` request body.
type ToolContentStatsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool to report content stats for.
	ID string `json:"id"`
}

// ToolContentBucket is one power-of-two size bucket in the per-tool
// result-size histogram.
type ToolContentBucket struct {
	// MaxBytes is the inclusive upper bound of this bucket in bytes.
	MaxBytes int64 `json:"max_bytes"`
	// Count is the number of recent results that fell in this bucket.
	Count int64 `json:"count"`
}

// ToolContentStats is the `tools.content_stats` reply — the per-tool
// distribution of recent result sizes vs the heavy-content threshold
// (RFC §6.5 / D-026) plus the negotiated DisplayMode snapshot (D-062).
type ToolContentStats struct {
	// ID echoes the requested tool ID.
	ID string `json:"id"`
	// Histogram is the result-size distribution, one bucket per
	// power-of-two byte range, ascending.
	Histogram []ToolContentBucket `json:"histogram"`
	// HeavyThresholdBytes is the configured heavy-content threshold
	// (D-026) — results at or above this size route through the
	// ArtifactStore by-reference.
	HeavyThresholdBytes int64 `json:"heavy_threshold_bytes"`
	// HeavyCount is the number of recent results at or above the
	// heavy-content threshold.
	HeavyCount int64 `json:"heavy_count"`
	// NegotiatedDisplay maps a MIME type to its negotiated MCP-Apps
	// DisplayMode (D-062); empty for non-MCP tools.
	NegotiatedDisplay map[string]string `json:"negotiated_display"`
}

// ToolSetApprovalPolicyRequest is the `tools.set_approval_policy`
// request body. ADMIN method — requires the verified `auth.ScopeAdmin`
// claim (D-079).
type ToolSetApprovalPolicyRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool to update.
	ID string `json:"id"`
	// Policy is the new approval policy; must be a valid
	// ToolApprovalPolicy or the request is rejected with
	// CodeInvalidRequest.
	Policy ToolApprovalPolicy `json:"policy"`
}

// ToolSetApprovalPolicyResponse is the `tools.set_approval_policy`
// reply.
type ToolSetApprovalPolicyResponse struct {
	// ID echoes the updated tool ID.
	ID string `json:"id"`
	// Policy echoes the applied approval policy.
	Policy ToolApprovalPolicy `json:"policy"`
}

// ToolRevokeOAuthRequest is the `tools.revoke_oauth` request body.
// ADMIN method — requires the verified `auth.ScopeAdmin` claim (D-079).
type ToolRevokeOAuthRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the catalog key of the tool whose OAuth bindings to revoke.
	ID string `json:"id"`
}

// ToolRevokeOAuthResponse is the `tools.revoke_oauth` reply.
type ToolRevokeOAuthResponse struct {
	// ID echoes the tool ID whose bindings were revoked.
	ID string `json:"id"`
	// RevokedCount is the number of OAuth bindings revoked.
	RevokedCount int64 `json:"revoked_count"`
}
