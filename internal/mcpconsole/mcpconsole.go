// Package mcpconsole wires the Phase 73k (D-119) MCP-Connections
// Protocol surface to its runtime-side dependencies — the Phase 28 MCP
// driver registry and the Phase 30 tool-side OAuth provider.
//
// # Why a separate package
//
// The `internal/protocol` package owns the MCPSurface dispatcher and
// the MCPAccessor / MCPOAuthAccessor interfaces, but it MUST NOT import
// the `mcp` driver or the `tools/auth` package (CLAUDE.md §13 — the
// Protocol package stays driver-free; a Protocol type that re-exported
// a driver type would be the reject-on-sight smell). The adapters that
// bridge the two live here, in a wiring package both `cmd/harbor` and
// the Phase 73k integration test import. The MCPSurface depends ONLY on
// the interfaces; this package is the single concrete that satisfies
// them.
//
// # Concurrent reuse (D-025)
//
// RegistryAccessor and OAuthAccessor are thin, immutable adapters — the
// wrapped Registry / Provider are themselves D-025-safe compiled
// artifacts, and the adapters add no mutable state.
package mcpconsole

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// RegistryAccessor adapts a *mcp.Registry to the protocol.MCPAccessor
// interface. It is the runtime-side read/control seam the MCPSurface
// calls for the nine `mcp.servers.*` read methods plus the raw-HTML
// trust toggle.
type RegistryAccessor struct {
	reg *mcp.Registry
}

// NewRegistryAccessor wraps a *mcp.Registry as a protocol.MCPAccessor.
// A nil registry is rejected — fail closed (CLAUDE.md §5).
func NewRegistryAccessor(reg *mcp.Registry) (*RegistryAccessor, error) {
	if reg == nil {
		return nil, errors.New("mcpconsole: NewRegistryAccessor requires a non-nil *mcp.Registry")
	}
	return &RegistryAccessor{reg: reg}, nil
}

// compile-time assertion: RegistryAccessor satisfies protocol.MCPAccessor.
var _ protocol.MCPAccessor = (*RegistryAccessor)(nil)

// ListServers implements protocol.MCPAccessor.
func (a *RegistryAccessor) ListServers(ctx context.Context, f protocol.MCPListFilter) ([]protocol.MCPServerRow, string, error) {
	filter := mcp.ListFilter{
		Transport:      f.Transport,
		HasOAuth:       f.HasOAuth,
		HasRecentError: f.HasRecentError,
		NamePrefix:     f.NamePrefix,
		PageToken:      f.PageToken,
		PageSize:       f.PageSize,
	}
	for _, st := range f.State {
		filter.State = append(filter.State, mcp.ServerState(st))
	}
	views, cur, err := a.reg.ListServers(ctx, filter)
	if err != nil {
		return nil, "", err
	}
	rows := make([]protocol.MCPServerRow, 0, len(views))
	for _, v := range views {
		rows = append(rows, serverRow(v))
	}
	next := ""
	if cur != nil {
		next = cur.NextPageToken
	}
	return rows, next, nil
}

// GetServer implements protocol.MCPAccessor.
func (a *RegistryAccessor) GetServer(ctx context.Context, name string) (protocol.MCPServerRow, error) {
	v, err := a.reg.GetServer(ctx, name)
	if err != nil {
		return protocol.MCPServerRow{}, err
	}
	return serverRow(*v), nil
}

// ListResources implements protocol.MCPAccessor.
func (a *RegistryAccessor) ListResources(ctx context.Context, name string) ([]protocol.MCPResourceRow, error) {
	views, err := a.reg.ListResources(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.MCPResourceRow, 0, len(views))
	for _, v := range views {
		out = append(out, protocol.MCPResourceRow{
			URI: v.URI, MimeType: v.MimeType, SizeBytes: v.SizeBytes, Name: v.Name, Title: v.Title,
		})
	}
	return out, nil
}

// ListPrompts implements protocol.MCPAccessor.
func (a *RegistryAccessor) ListPrompts(ctx context.Context, name string) ([]protocol.MCPPromptRow, error) {
	views, err := a.reg.ListPrompts(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.MCPPromptRow, 0, len(views))
	for _, v := range views {
		args := make([]protocol.MCPPromptArgRow, 0, len(v.Arguments))
		for _, ag := range v.Arguments {
			args = append(args, protocol.MCPPromptArgRow{
				Name: ag.Name, Description: ag.Description, Required: ag.Required,
			})
		}
		out = append(out, protocol.MCPPromptRow{
			Name: v.Name, Description: v.Description, Arguments: args,
		})
	}
	return out, nil
}

// RefreshDiscovery implements protocol.MCPAccessor.
func (a *RegistryAccessor) RefreshDiscovery(ctx context.Context, name string) (protocol.MCPDiscoveryRow, error) {
	r, err := a.reg.RefreshDiscovery(ctx, name)
	if err != nil {
		return protocol.MCPDiscoveryRow{}, err
	}
	return protocol.MCPDiscoveryRow{
		DiscoveryID: r.DiscoveryID, ToolCount: r.ToolCount,
		ResourceCount: r.ResourceCount, PromptCount: r.PromptCount,
	}, nil
}

// Probe implements protocol.MCPAccessor.
func (a *RegistryAccessor) Probe(ctx context.Context, name string) (protocol.MCPProbeRow, error) {
	r, err := a.reg.Probe(ctx, name)
	if err != nil {
		return protocol.MCPProbeRow{}, err
	}
	return protocol.MCPProbeRow{OK: r.OK, LatencyMs: r.LatencyMs, Error: r.Error}, nil
}

// Health implements protocol.MCPAccessor.
func (a *RegistryAccessor) Health(ctx context.Context, name string) (protocol.MCPHealthRow, error) {
	snap, err := a.reg.Health(ctx, name, 0)
	if err != nil {
		return protocol.MCPHealthRow{}, err
	}
	buckets := make([]protocol.MCPHealthBucketRow, 0, len(snap.HandshakeLatencyBuckets))
	for _, b := range snap.HandshakeLatencyBuckets {
		buckets = append(buckets, protocol.MCPHealthBucketRow{StartMs: b.StartMs, LatencyMs: b.LatencyMs})
	}
	reconnects := make([]protocol.MCPReconnectRow, 0, len(snap.ReconnectHistory))
	for _, rc := range snap.ReconnectHistory {
		reconnects = append(reconnects, protocol.MCPReconnectRow{OccurredAt: rc.OccurredAt, Reason: rc.Reason})
	}
	return protocol.MCPHealthRow{
		HandshakeLatencyBuckets: buckets,
		ReconnectHistory:        reconnects,
		TransportErrorRate:      snap.TransportErrorRate,
	}, nil
}

// SetRawHTMLTrust implements protocol.MCPAccessor.
func (a *RegistryAccessor) SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (bool, error) {
	return a.reg.SetRawHTMLTrust(ctx, name, trusted)
}

// serverRow maps a mcp.ServerView onto the protocol.MCPServerRow shape.
func serverRow(v mcp.ServerView) protocol.MCPServerRow {
	return protocol.MCPServerRow{
		Name:              v.Name,
		Transport:         v.Transport,
		URLOrCommand:      v.URLOrCommand,
		State:             string(v.State),
		LastDiscoveryAt:   v.LastDiscoveryAt,
		ToolCount:         v.ToolCount,
		ResourceCount:     v.ResourceCount,
		PromptCount:       v.PromptCount,
		RecentLatencyMs:   v.RecentLatencyMs,
		ErrorRatePerMin:   v.ErrorRatePerMin,
		OAuthBindingCount: v.OAuthBindingCount,
		RawHTMLTrusted:    v.RawHTMLTrusted,
		DisplayModes:      v.DisplayModes,
		ContentShapes:     v.ContentShapes,
		PolicyTimeoutMs:   int64(v.Policy.TimeoutMS),
		PolicyMaxRetries:  v.Policy.MaxRetries,
	}
}

// OAuthAccessor adapts a *auth.Provider to the protocol.MCPOAuthAccessor
// interface — the runtime-side seam the MCPSurface calls for the OAuth
// binding methods (`bindings.list` read + the `refresh_binding` /
// `revoke_binding` admin verbs).
//
// # V1 binding-enumeration scope
//
// The Phase 30 `auth.Provider` keys tokens by `(BindingScope, subject,
// source)` and exposes no fleet-wide binding enumeration API. At V1 the
// adapter therefore projects the caller-visible binding state: it reports
// the configured binding scope for the source and the caller's own
// token freshness. A fleet-wide per-server binding catalog is a post-V1
// `auth.Provider` extension (page-mcp-connections.md §8 — non-admin
// operators see only their own ScopeUser binding regardless).
type OAuthAccessor struct {
	provider *auth.Provider
}

// NewOAuthAccessor wraps a *auth.Provider as a protocol.MCPOAuthAccessor.
// A nil provider is rejected — fail closed.
func NewOAuthAccessor(provider *auth.Provider) (*OAuthAccessor, error) {
	if provider == nil {
		return nil, errors.New("mcpconsole: NewOAuthAccessor requires a non-nil *auth.Provider")
	}
	return &OAuthAccessor{provider: provider}, nil
}

// compile-time assertion: OAuthAccessor satisfies protocol.MCPOAuthAccessor.
var _ protocol.MCPOAuthAccessor = (*OAuthAccessor)(nil)

// ListBindings implements protocol.MCPOAuthAccessor. It projects the
// configured binding for the server (the OAuthConfig's BindingScope +
// requested scopes) and the caller's own token freshness. NEVER returns
// token plaintext (D-083 invariant).
func (a *OAuthAccessor) ListBindings(ctx context.Context, server string) ([]protocol.MCPBindingRow, error) {
	cfg, ok := a.provider.ConfigFor(tools.ToolSourceID(server))
	if !ok {
		// No OAuth configured for this server — an empty (non-nil)
		// binding list is the correct projection.
		return []protocol.MCPBindingRow{}, nil
	}
	row := protocol.MCPBindingRow{
		BindingScope: string(cfg.BindingScope),
		Scopes:       append([]string(nil), cfg.Scopes...),
	}
	// Best-effort: surface the caller's own token freshness. A
	// missing token (ErrAuthRequired) is not an error here — it means
	// the binding is unconnected, which the page renders as such.
	if tok, err := a.provider.Token(ctx, tools.ToolSourceID(server)); err == nil {
		row.PrincipalID = tok.SubjectID()
		row.ExpiresAt = tok.ExpiresAt
		row.LastUsedAt = tok.LastRefreshedAt
		if len(tok.Scopes) > 0 {
			row.Scopes = append([]string(nil), tok.Scopes...)
		}
	}
	return []protocol.MCPBindingRow{row}, nil
}

// InitiateBinding implements protocol.MCPOAuthAccessor. It invokes
// auth.Provider.InitiateFlow and returns the AuthorizeURL + flow State
// the Console opens in a popup. The principalID argument is reserved for
// a post-V1 delegated-flow API; V1 drives the flow for the caller's own
// identity (the auth.Provider reads the subject from ctx).
func (a *OAuthAccessor) InitiateBinding(ctx context.Context, server, _ string) (string, string, error) {
	fi, err := a.provider.InitiateFlow(ctx, tools.ToolSourceID(server))
	if err != nil {
		return "", "", fmt.Errorf("mcpconsole: initiate OAuth flow for %q: %w", server, err)
	}
	return fi.AuthorizeURL, fi.State, nil
}

// RevokeBinding implements protocol.MCPOAuthAccessor. It invokes
// auth.Provider.Revoke for the server's binding.
func (a *OAuthAccessor) RevokeBinding(ctx context.Context, server, _ string) (bool, error) {
	if err := a.provider.Revoke(ctx, tools.ToolSourceID(server)); err != nil {
		return false, fmt.Errorf("mcpconsole: revoke OAuth binding for %q: %w", server, err)
	}
	return true, nil
}
