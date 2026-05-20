package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// MCPSurface is the transport-agnostic Harbor Protocol MCP-Connections
// handler (Phase 73k / D-119). It owns the twelve `mcp.servers.*`
// methods that back the Console MCP Connections page — nine read
// methods and three admin verbs.
//
// MCPSurface is a sibling of the Phase 54 ControlSurface and the Phase
// 72f PostureSurface, not an extension: the MCP methods reach the
// runtime's MCP driver registry and tool-side OAuth provider, not the
// steering inbox.
//
// MCPSurface is built once per Runtime process via NewMCPSurface and
// shared across every Protocol request; Dispatch is safe for concurrent
// use by N goroutines (D-025). Every field is set once at construction
// and never mutated — Dispatch reads its request-specific data from
// ctx + the request argument, never from the surface struct.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// Every handler fails closed on an incomplete identity triple with
// CodeIdentityRequired. The three admin verbs (`refresh_binding` /
// `revoke_binding` / `set_raw_html_trust`) gate additionally on the
// `auth.ScopeAdmin` claim per D-079; a missing claim surfaces
// CodeScopeMismatch. The two control-plane verbs (`refresh_discovery` /
// `probe`) gate on the `auth.ScopeAdmin` claim too (D-066 — control-
// plane verbs mutate the runtime's view of upstream state) — there is
// NO new scope minted for MCP (D-079 closed-set).
//
// # The raw-HTML trust audit (brief 11 §8)
//
// A successful `mcp.servers.set_raw_html_trust` emits a
// `mcp.raw_html_trust_toggled` audit event through the wired Redactor +
// Bus. Raw HTML / SVG from an MCP server is untrusted by default; the
// audit event is the load-bearing record of the operator's explicit
// opt-in. A failed audit emit fails the call loudly (CodeRuntimeError)
// — an un-auditable trust toggle is refused, never silently applied.
//
// # The Console never reads internal Runtime objects (CLAUDE.md §8/§13)
//
// The MCP data flows as canonical Protocol wire types
// (internal/protocol/types) — never as a re-export of the `mcp` driver
// or `tools/auth` Go structs. The MCPAccessor / MCPOAuthAccessor seams
// are narrow read/control adapters the Runtime wires at boot; the
// `protocol` package never imports the `mcp` driver package (the seam
// is satisfied structurally — no import cycle).
type MCPSurface struct {
	mcp      MCPAccessor
	oauth    MCPOAuthAccessor
	redactor audit.Redactor
	bus      events.EventBus
	clock    func() time.Time
}

// MCPServerRow is the runtime-side projection of one MCP server. The
// `mcp` driver's `Registry.ServerView` satisfies the MCPAccessor
// contract by returning this flat shape; the MCPSurface translates it
// onto `types.MCPServerView`. Keeping the projection here (not importing
// the driver type) keeps the `protocol` package driver-free.
type MCPServerRow struct {
	Name              string
	Transport         string
	URLOrCommand      string
	State             string
	LastDiscoveryAt   time.Time
	ToolCount         int
	ResourceCount     int
	PromptCount       int
	RecentLatencyMs   int64
	ErrorRatePerMin   float64
	OAuthBindingCount int
	RawHTMLTrusted    bool
	DisplayModes      []string
	ContentShapes     []string
	PolicyTimeoutMs   int64
	PolicyMaxRetries  int
	PolicyConcurrency int
}

// MCPListFilter is the runtime-side filter the MCPAccessor's ListServers
// applies. It mirrors the Protocol `MCPServersListRequest` filter shape.
type MCPListFilter struct {
	State          []string
	Transport      []string
	HasOAuth       *bool
	HasRecentError *bool
	NamePrefix     string
	PageToken      string
	PageSize       int
}

// MCPResourceRow is the runtime-side projection of one advertised MCP
// resource.
type MCPResourceRow struct {
	URI       string
	MimeType  string
	SizeBytes int64
	Name      string
	Title     string
}

// MCPPromptRow is the runtime-side projection of one advertised MCP
// prompt.
type MCPPromptRow struct {
	Name        string
	Description string
	Arguments   []MCPPromptArgRow
}

// MCPPromptArgRow is one declared prompt argument.
type MCPPromptArgRow struct {
	Name        string
	Description string
	Required    bool
}

// MCPDiscoveryRow is the runtime-side projection of a refresh-discovery
// result.
type MCPDiscoveryRow struct {
	DiscoveryID   string
	ToolCount     int
	ResourceCount int
	PromptCount   int
}

// MCPProbeRow is the runtime-side projection of a transport-probe result.
type MCPProbeRow struct {
	OK        bool
	LatencyMs int64
	Error     string
}

// MCPHealthRow is the runtime-side projection of a health snapshot.
type MCPHealthRow struct {
	HandshakeLatencyBuckets []MCPHealthBucketRow
	ReconnectHistory        []MCPReconnectRow
	TransportErrorRate      float64
}

// MCPHealthBucketRow is one handshake-latency sparkline bucket.
type MCPHealthBucketRow struct {
	StartMs   int64
	LatencyMs int64
}

// MCPReconnectRow is one reconnect-history entry.
type MCPReconnectRow struct {
	OccurredAt time.Time
	Reason     string
}

// MCPBindingRow is the runtime-side projection of one OAuth binding. It
// NEVER carries token plaintext (D-083 invariant).
type MCPBindingRow struct {
	PrincipalID  string
	BindingScope string
	Scopes       []string
	ExpiresAt    time.Time
	LastUsedAt   time.Time
}

// MCPAccessor is the narrow read/control contract the MCPSurface calls
// into for the nine `mcp.servers.*` read methods plus the raw-HTML
// trust toggle. The Runtime's `mcp.Registry` is wrapped to satisfy it;
// the `protocol` package never imports the `mcp` driver.
//
// Every method is identity-mandatory: the implementation reads the
// triple from ctx and fails closed on a missing one.
type MCPAccessor interface {
	// ListServers returns the filtered, paginated server list plus the
	// next-page token (empty when the page is the last).
	ListServers(ctx context.Context, f MCPListFilter) (rows []MCPServerRow, nextPageToken string, err error)
	// GetServer returns one server's detail row.
	GetServer(ctx context.Context, name string) (MCPServerRow, error)
	// ListResources returns a server's advertised resources.
	ListResources(ctx context.Context, name string) ([]MCPResourceRow, error)
	// ListPrompts returns a server's advertised prompts.
	ListPrompts(ctx context.Context, name string) ([]MCPPromptRow, error)
	// RefreshDiscovery re-runs a server's discovery.
	RefreshDiscovery(ctx context.Context, name string) (MCPDiscoveryRow, error)
	// Probe runs a transport round-trip against a server.
	Probe(ctx context.Context, name string) (MCPProbeRow, error)
	// Health returns a server's health snapshot.
	Health(ctx context.Context, name string) (MCPHealthRow, error)
	// SetRawHTMLTrust persists the per-server raw-HTML trust flag and
	// returns the prior value.
	SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (prev bool, err error)
}

// MCPOAuthAccessor is the narrow contract the MCPSurface calls into for
// the OAuth binding methods — `bindings.list` (read) and the admin
// verbs `refresh_binding` / `revoke_binding`. The Runtime's
// `tools/auth.Provider` is wrapped to satisfy it.
//
// The accessor NEVER returns token plaintext (D-083 invariant) — only
// binding metadata and, for an InitiateFlow, the runtime-minted
// AuthorizeURL + flow State.
type MCPOAuthAccessor interface {
	// ListBindings returns the OAuth bindings for a server (metadata
	// only — never token plaintext).
	ListBindings(ctx context.Context, server string) ([]MCPBindingRow, error)
	// InitiateBinding starts an OAuth (re)connect flow and returns the
	// AuthorizeURL + flow State the Console opens in a popup.
	InitiateBinding(ctx context.Context, server, principalID string) (authorizeURL, state string, err error)
	// RevokeBinding revokes a server's OAuth binding.
	RevokeBinding(ctx context.Context, server, principalID string) (revoked bool, err error)
}

// MCPDeps bundles the runtime-side seams an MCPSurface reads through.
type MCPDeps struct {
	// MCP is the read/control accessor over the `mcp` driver registry.
	// Mandatory.
	MCP MCPAccessor
	// OAuth is the accessor over the `tools/auth` OAuth provider.
	// Mandatory.
	OAuth MCPOAuthAccessor
	// Redactor is the audit Redactor the `mcp.raw_html_trust_toggled`
	// payload runs through before the bus publish. Mandatory.
	Redactor audit.Redactor
	// Bus is the canonical event bus the `mcp.raw_html_trust_toggled`
	// audit event is published onto. Mandatory.
	Bus events.EventBus
	// Clock returns the current wall-clock time. Optional — defaults
	// to time.Now.
	Clock func() time.Time
}

// ErrMCPMisconfigured — NewMCPSurface was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5).
var ErrMCPMisconfigured = stderrors.New("protocol: MCPSurface missing a mandatory dependency")

// NewMCPSurface builds the Protocol MCP-Connections surface. The MCP /
// OAuth accessors, the Redactor, and the Bus are all mandatory; a nil
// fails loud with a wrapped ErrMCPMisconfigured.
//
// The returned MCPSurface is immutable after construction (D-025) and
// safe for concurrent use by N goroutines.
func NewMCPSurface(deps MCPDeps) (*MCPSurface, error) {
	if deps.MCP == nil {
		return nil, fmt.Errorf("%w: MCP accessor is nil", ErrMCPMisconfigured)
	}
	if deps.OAuth == nil {
		return nil, fmt.Errorf("%w: OAuth accessor is nil", ErrMCPMisconfigured)
	}
	if deps.Redactor == nil {
		return nil, fmt.Errorf("%w: Redactor is nil", ErrMCPMisconfigured)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Bus is nil", ErrMCPMisconfigured)
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &MCPSurface{
		mcp:      deps.MCP,
		oauth:    deps.OAuth,
		redactor: deps.Redactor,
		bus:      deps.Bus,
		clock:    clock,
	}, nil
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// `mcp.servers.*` method call. A Phase 60 REST handler decodes a request,
// calls Dispatch, and encodes the response — Dispatch IS the surface.
//
// method selects the handler; it MUST be one of the twelve
// `mcp.servers.*` methods (methods.IsMCPServersMethod). req MUST be the
// wire request type the method expects.
//
// The return is always a *types.<Method>Response or a *protoerrors.Error:
//
//   - CodeUnknownMethod   — method is not an MCP method.
//   - CodeInvalidRequest  — req is nil or the wrong wire type.
//   - CodeIdentityRequired — the request's identity triple is incomplete.
//   - CodeScopeMismatch   — an admin / control verb without the admin
//     scope claim.
//   - CodeNotFound        — the named MCP server does not exist.
//   - CodeRuntimeError    — an accessor or audit-emit failure.
//
// Dispatch holds no per-call state on the MCPSurface (D-025).
func (s *MCPSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsMCPServersMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol MCP method", string(method))
	}

	// Identity + admin/control-scope gate at the edge. Every MCP method
	// is identity-mandatory; the three admin verbs AND the two
	// control-plane verbs (refresh_discovery / probe) require the admin
	// scope claim (D-079 closed-set; D-066 — control-plane verbs).
	id, scoped, perr := s.gate(ctx, method, req)
	if perr != nil {
		return nil, perr
	}

	switch method {
	case methods.MethodMCPServersList:
		return s.handleList(ctx, id, req)
	case methods.MethodMCPServersGet:
		return s.handleGet(ctx, id, req)
	case methods.MethodMCPServersResources:
		return s.handleResources(ctx, id, req)
	case methods.MethodMCPServersPrompts:
		return s.handlePrompts(ctx, id, req)
	case methods.MethodMCPServersRefreshDiscovery:
		return s.handleRefreshDiscovery(ctx, id, req)
	case methods.MethodMCPServersProbe:
		return s.handleProbe(ctx, id, req)
	case methods.MethodMCPServersHealth:
		return s.handleHealth(ctx, id, req)
	case methods.MethodMCPServersBindingsList:
		return s.handleBindingsList(ctx, id, req)
	case methods.MethodMCPServersPolicy:
		return s.handlePolicy(ctx, id, req)
	case methods.MethodMCPServersRefreshBinding:
		return s.handleRefreshBinding(ctx, id, req)
	case methods.MethodMCPServersRevokeBinding:
		return s.handleRevokeBinding(ctx, id, req)
	case methods.MethodMCPServersSetRawHTMLTrust:
		return s.handleSetRawHTMLTrust(ctx, id, scoped, req)
	default:
		// Unreachable: IsMCPServersMethod already gated the set.
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no MCP handler (Protocol-surface invariant violated)", string(method))
	}
}

// gate validates identity + the admin-scope claim. It returns the
// validated caller identity and whether the admin scope is verified.
// For an admin / control-plane verb, a missing admin scope fails closed
// with CodeScopeMismatch.
func (s *MCPSurface) gate(ctx context.Context, method methods.Method, req any) (identity.Identity, bool, *protoerrors.Error) {
	scope := extractIdentityScope(req)
	if scope == nil {
		return identity.Identity{}, false, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a recognised MCP request type", string(method))
	}
	id := identity.Identity{
		TenantID:  scope.Tenant,
		UserID:    scope.User,
		SessionID: scope.Session,
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	hasAdmin := auth.HasScope(ctx, auth.ScopeAdmin)
	// Admin verbs + the two control-plane verbs require the admin scope.
	requiresAdmin := methods.IsMCPAdminMethod(method) ||
		method == methods.MethodMCPServersRefreshDiscovery ||
		method == methods.MethodMCPServersProbe
	if requiresAdmin && !hasAdmin {
		return identity.Identity{}, false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: requires the admin scope claim", string(method))
	}
	return id, hasAdmin, nil
}

// extractIdentityScope pulls the flat IdentityScope out of any of the
// twelve MCP request shapes. Returns nil for an unrecognised / nil type.
func extractIdentityScope(req any) *types.IdentityScope {
	switch v := req.(type) {
	case *types.MCPServersListRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerGetRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerResourcesRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerPromptsRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerRefreshDiscoveryRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerProbeRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerHealthRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerBindingsListRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerPolicyRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerRefreshBindingRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerRevokeBindingRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	case *types.MCPServerSetRawHTMLTrustRequest:
		if v == nil {
			return nil
		}
		return &v.Identity
	default:
		return nil
	}
}

// withIdentity injects the validated caller identity into ctx so the
// accessor's identity-mandatory gate (it reads the triple from ctx) is
// satisfied — the trust-based Phase 60 posture leaves no ctx-identity.
func withIdentity(ctx context.Context, id identity.Identity) (context.Context, *protoerrors.Error) {
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"identity scope incomplete: %v", err)
	}
	return idCtx, nil
}

// handleList serves mcp.servers.list.
func (s *MCPSurface) handleList(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServersListRequest)
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	f := MCPListFilter{
		Transport:      r.Transport,
		HasOAuth:       r.HasOAuth,
		HasRecentError: r.HasRecentError,
		NamePrefix:     r.NamePrefix,
		PageToken:      r.PageToken,
		PageSize:       int(r.PageSize),
	}
	for _, st := range r.State {
		f.State = append(f.State, string(st))
	}
	rows, nextToken, err := s.mcp.ListServers(idCtx, f)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersList), err)
	}
	out := make([]types.MCPServerView, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectServerRow(row))
	}
	return &types.MCPServersListResponse{
		Servers:         out,
		NextPageToken:   nextToken,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleGet serves mcp.servers.get.
func (s *MCPSurface) handleGet(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerGetRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersGet))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.mcp.GetServer(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersGet), err)
	}
	bindings, berr := s.oauth.ListBindings(idCtx, r.Name)
	if berr != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersGet), berr)
	}
	return &types.MCPServerGetResponse{
		Server:                 projectServerRow(row),
		DisplayModesAdvertised: nonNilStrings(row.DisplayModes),
		ContentShapes:          nonNilStrings(row.ContentShapes),
		ToolPolicy:             projectPolicy(row),
		BindingsSummary:        bindingScopeCounts(bindings),
		ProtocolVersion:        types.ProtocolVersion,
	}, nil
}

// handleResources serves mcp.servers.resources.
func (s *MCPSurface) handleResources(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerResourcesRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersResources))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	rows, err := s.mcp.ListResources(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersResources), err)
	}
	out := make([]types.MCPResourceView, 0, len(rows))
	for _, row := range rows {
		out = append(out, types.MCPResourceView{
			URI:       row.URI,
			MimeType:  row.MimeType,
			SizeBytes: row.SizeBytes,
			Name:      row.Name,
			Title:     row.Title,
		})
	}
	return &types.MCPServerResourcesResponse{
		Resources:       out,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handlePrompts serves mcp.servers.prompts.
func (s *MCPSurface) handlePrompts(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerPromptsRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersPrompts))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	rows, err := s.mcp.ListPrompts(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersPrompts), err)
	}
	out := make([]types.MCPPromptView, 0, len(rows))
	for _, row := range rows {
		args := make([]types.MCPPromptArg, 0, len(row.Arguments))
		for _, a := range row.Arguments {
			args = append(args, types.MCPPromptArg{
				Name:        a.Name,
				Description: a.Description,
				Required:    a.Required,
			})
		}
		out = append(out, types.MCPPromptView{
			Name:        row.Name,
			Description: row.Description,
			Arguments:   args,
		})
	}
	return &types.MCPServerPromptsResponse{
		Prompts:         out,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleRefreshDiscovery serves mcp.servers.refresh_discovery.
func (s *MCPSurface) handleRefreshDiscovery(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerRefreshDiscoveryRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersRefreshDiscovery))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.mcp.RefreshDiscovery(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersRefreshDiscovery), err)
	}
	return &types.MCPServerRefreshDiscoveryResponse{
		DiscoveryID:     row.DiscoveryID,
		ToolCount:       int32(row.ToolCount),
		ResourceCount:   int32(row.ResourceCount),
		PromptCount:     int32(row.PromptCount),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleProbe serves mcp.servers.probe.
func (s *MCPSurface) handleProbe(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerProbeRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersProbe))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.mcp.Probe(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersProbe), err)
	}
	return &types.MCPServerProbeResponse{
		OK:              row.OK,
		LatencyMs:       row.LatencyMs,
		Error:           row.Error,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleHealth serves mcp.servers.health.
func (s *MCPSurface) handleHealth(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerHealthRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersHealth))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.mcp.Health(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersHealth), err)
	}
	buckets := make([]types.MCPHealthBucket, 0, len(row.HandshakeLatencyBuckets))
	for _, b := range row.HandshakeLatencyBuckets {
		buckets = append(buckets, types.MCPHealthBucket{StartMs: b.StartMs, LatencyMs: b.LatencyMs})
	}
	reconnects := make([]types.MCPReconnect, 0, len(row.ReconnectHistory))
	for _, rc := range row.ReconnectHistory {
		reconnects = append(reconnects, types.MCPReconnect{OccurredAt: rc.OccurredAt, Reason: rc.Reason})
	}
	return &types.MCPServerHealthResponse{
		HandshakeLatencyBuckets: buckets,
		ReconnectHistory:        reconnects,
		TransportErrorRate:      row.TransportErrorRate,
		ProtocolVersion:         types.ProtocolVersion,
	}, nil
}

// handleBindingsList serves mcp.servers.bindings.list.
func (s *MCPSurface) handleBindingsList(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerBindingsListRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersBindingsList))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	rows, err := s.oauth.ListBindings(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersBindingsList), err)
	}
	out := make([]types.MCPBindingView, 0, len(rows))
	for _, row := range rows {
		out = append(out, types.MCPBindingView{
			PrincipalID:  row.PrincipalID,
			BindingScope: row.BindingScope,
			Scopes:       nonNilStrings(row.Scopes),
			ExpiresAt:    row.ExpiresAt,
			LastUsedAt:   row.LastUsedAt,
		})
	}
	return &types.MCPServerBindingsListResponse{
		Bindings:        out,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handlePolicy serves mcp.servers.policy.
func (s *MCPSurface) handlePolicy(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerPolicyRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersPolicy))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	row, err := s.mcp.GetServer(idCtx, r.Name)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersPolicy), err)
	}
	return &types.MCPServerPolicyResponse{
		ToolPolicy:      projectPolicy(row),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleRefreshBinding serves mcp.servers.refresh_binding (admin verb).
func (s *MCPSurface) handleRefreshBinding(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerRefreshBindingRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersRefreshBinding))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	authorizeURL, state, err := s.oauth.InitiateBinding(idCtx, r.Name, r.PrincipalID)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersRefreshBinding), err)
	}
	return &types.MCPServerRefreshBindingResponse{
		AuthorizeURL:    authorizeURL,
		State:           state,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleRevokeBinding serves mcp.servers.revoke_binding (admin verb).
func (s *MCPSurface) handleRevokeBinding(ctx context.Context, id identity.Identity, req any) (any, error) {
	r := req.(*types.MCPServerRevokeBindingRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(methods.MethodMCPServersRevokeBinding))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	revoked, err := s.oauth.RevokeBinding(idCtx, r.Name, r.PrincipalID)
	if err != nil {
		return nil, mapMCPError(string(methods.MethodMCPServersRevokeBinding), err)
	}
	return &types.MCPServerRevokeBindingResponse{
		Revoked:         revoked,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleSetRawHTMLTrust serves mcp.servers.set_raw_html_trust (admin
// verb). On success it emits the `mcp.raw_html_trust_toggled` audit
// event. A failed audit emit fails the call closed — an un-auditable
// trust toggle is refused (CLAUDE.md §5, §7).
func (s *MCPSurface) handleSetRawHTMLTrust(ctx context.Context, id identity.Identity, _ bool, req any) (any, error) {
	method := methods.MethodMCPServersSetRawHTMLTrust
	r := req.(*types.MCPServerSetRawHTMLTrustRequest)
	if r.Name == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: name is required", string(method))
	}
	idCtx, perr := withIdentity(ctx, id)
	if perr != nil {
		return nil, perr
	}
	if _, err := s.mcp.SetRawHTMLTrust(idCtx, r.Name, r.Trusted); err != nil {
		return nil, mapMCPError(string(method), err)
	}
	// Emit the audit event BEFORE returning. A failed emit fails the
	// call closed — an un-auditable trust toggle is never silently
	// applied (CLAUDE.md §5 + §7 rule 6).
	if err := s.emitRawHTMLTrustToggled(ctx, id, r.Name, r.Trusted); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: trust toggle applied but audit emit failed: %v", string(method), err)
	}
	return &types.MCPServerSetRawHTMLTrustResponse{
		Name:            r.Name,
		Trusted:         r.Trusted,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// emitRawHTMLTrustToggled publishes the `mcp.raw_html_trust_toggled`
// audit event onto the wired bus. The audit-visible fields run through
// the wired Redactor BEFORE the publish (CLAUDE.md §7 rule 6 + D-020).
func (s *MCPSurface) emitRawHTMLTrustToggled(ctx context.Context, actor identity.Identity, server string, trusted bool) error {
	auditView := map[string]any{
		"actor_tenant":  actor.TenantID,
		"actor_user":    actor.UserID,
		"actor_session": actor.SessionID,
		"server_name":   server,
		"trusted":       trusted,
	}
	if _, err := s.redactor.Redact(ctx, auditView); err != nil {
		// Fail loud — never emit unredacted (CLAUDE.md §13).
		return fmt.Errorf("redactor refused raw_html_trust_toggled payload: %w", err)
	}
	q := identity.Quadruple{Identity: actor}
	ev := events.Event{
		Type:       events.EventTypeMCPRawHTMLTrustToggled,
		Identity:   q,
		OccurredAt: s.clock(),
		Payload: events.MCPRawHTMLTrustToggledPayload{
			Actor:      q,
			ServerName: server,
			Trusted:    trusted,
			OccurredAt: s.clock(),
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("publish %s: %w", ev.Type, err)
	}
	return nil
}

// projectServerRow maps a runtime MCPServerRow onto the wire view.
func projectServerRow(row MCPServerRow) types.MCPServerView {
	return types.MCPServerView{
		Name:              row.Name,
		Transport:         row.Transport,
		URLOrCommand:      row.URLOrCommand,
		State:             types.MCPServerStateView(row.State),
		LastDiscoveryAt:   row.LastDiscoveryAt,
		ToolCount:         int32(row.ToolCount),
		ResourceCount:     int32(row.ResourceCount),
		PromptCount:       int32(row.PromptCount),
		RecentLatencyMs:   row.RecentLatencyMs,
		ErrorRatePerMin:   row.ErrorRatePerMin,
		OAuthBindingCount: int32(row.OAuthBindingCount),
		RawHTMLTrusted:    row.RawHTMLTrusted,
	}
}

// projectPolicy maps a runtime MCPServerRow's policy fields onto the
// wire ToolPolicy projection.
func projectPolicy(row MCPServerRow) types.MCPToolPolicyView {
	return types.MCPToolPolicyView{
		TimeoutMs:      row.PolicyTimeoutMs,
		MaxRetries:     int32(row.PolicyMaxRetries),
		ConcurrencyCap: int32(row.PolicyConcurrency),
	}
}

// bindingScopeCounts rolls a binding list up into per-scope counts.
func bindingScopeCounts(bindings []MCPBindingRow) []types.MCPBindingScopeCount {
	counts := map[string]int32{}
	for _, b := range bindings {
		counts[b.BindingScope]++
	}
	out := make([]types.MCPBindingScopeCount, 0, len(counts))
	for scope, n := range counts {
		out = append(out, types.MCPBindingScopeCount{BindingScope: scope, Count: n})
	}
	// Deterministic order.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].BindingScope < out[i].BindingScope {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// nonNilStrings returns s, or an empty slice when s is nil — so the
// wire JSON renders `[]` not `null`.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// mapMCPError translates an accessor error onto a canonical Protocol
// error code. The mapping closes the wire surface — every error shape is
// observable as a Code (CLAUDE.md §13).
func mapMCPError(method string, err error) error {
	switch {
	case err == nil:
		return nil
	case isMCPNotFound(err):
		return protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: %v", method, err)
	case isMCPIdentityMissing(err):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: %v", method, err)
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: MCP read failed: %v", method, err)
	}
}

// isMCPNotFound / isMCPIdentityMissing classify accessor errors by their
// error-message marker. The `protocol` package does not import the `mcp`
// driver (no import cycle), so the classification is string-based — the
// accessor wraps the driver sentinel and the marker is stable.
func isMCPNotFound(err error) bool {
	return err != nil && containsMarker(err.Error(), "server not found")
}

func isMCPIdentityMissing(err error) bool {
	return err != nil && containsMarker(err.Error(), "identity missing")
}

// containsMarker reports whether s contains sub.
func containsMarker(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
