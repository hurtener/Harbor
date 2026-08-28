package protocol

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
)

// CatalogProjector is the V1 production Projector — a read-only
// projection over a tools.ToolCatalog plus optional metric / annotation
// backends. It maps the runtime-internal tools.Tool descriptor onto the
// flat Protocol wire shapes the Console Tools page renders.
//
// # Annotation seam
//
// The catalog's tools.Tool descriptor carries transport / side-effect /
// scopes / examples / schemas, but NOT the per-tool OAuth-binding
// status, approval policy, last-used timestamp, or invocation metrics —
// those live in sibling subsystems (tools/auth, tools/approval, the
// events stream). CatalogProjector reads them through the optional
// Annotator interface. When no Annotator is wired, the projector
// returns conservative defaults (OAuth `n/a`, approval `auto`, zero
// metrics) so a partial-build Console still renders the catalog rather
// than failing — the defaults are honest ("we don't have this data"),
// not silent degradation of a known value.
//
// # Concurrent reuse
//
// CatalogProjector is immutable after NewCatalogProjector: it holds the
// catalog + annotator references. The catalog is itself safe for concurrent reuse; the
// projector adds no mutable state. The optional admin backends mutate
// runtime state, but that mutation lives behind the catalog / annotator
// implementations, internally synchronised there.
type CatalogProjector struct {
	catalog   tools.ToolCatalog
	annotator Annotator
	view      CatalogViewResolver
	// adminMu guards the in-memory approval-policy overrides the
	// default (annotator-less) admin path records. Production wiring
	// supplies an Annotator whose SetApprovalPolicy persists (identity-
	// scoped); the in-memory map is the conservative fallback so the admin
	// method is never a silent no-op when NO persisting annotator is wired.
	// The map is keyed by the caller's identity triple + tool ID so the
	// fallback is identity-scoped too — a set in session A never bleeds into
	// session B's projection.
	adminMu   sync.RWMutex
	overrides map[string]prototypes.ToolApprovalPolicy
	// loading is the optional agent-config loading-mode resolver
	// `tools.describe` consults when the request names an agent_id. Nil ⇒
	// describe always reports the boot-effective mode (byte-compatible with
	// the surface before this projection existed).
	loading LoadingResolver
}

// CatalogViewResolver builds the effective, identity-scoped catalog view for
// one request. The production adapter delegates to the runtime's canonical
// agent-config projection, so the Tools Protocol never reads the process-wide
// catalog directly for user-owned MCP sources. A blank agent id means the
// caller did not select an agent; the resolver must preserve ordinary
// operator/boot visibility while failing closed for private sources it cannot
// associate with that request.
type CatalogViewResolver interface {
	CatalogView(context.Context, identity.Identity, string) (tools.PlannerCatalogView, error)
}

// WithCatalogViewResolver wires the per-request effective catalog view. A nil
// resolver is treated as not supplied, preserving the existing raw-catalog
// behavior for embedders that have not assembled agent-config projection.
func WithCatalogViewResolver(r CatalogViewResolver) CatalogProjectorOption {
	return func(p *CatalogProjector) {
		if r != nil {
			p.view = r
		}
	}
}

// LoadingResolver is the optional per-agent effective-loading-mode backend
// `tools.describe` consults when a request names an agent_id. The production
// implementation composes the operator baseline and the acting user's
// durable ConfigScopeUser choice. It is
// `internal/runtime/agentcfg/projection.LoadingResolverAdapter`, wired at
// the cmd/harbor + devstack boot boundary (the §4.4 seam keeps the
// agent-config registry out of this package's import graph — the interface
// is satisfied structurally). A nil resolver (WithLoadingResolver not
// supplied) makes DescribeTool always report the boot-effective mode.
type LoadingResolver interface {
	// EffectiveLoading resolves the projected effective LoadingMode for
	// tool t under agentID's operator and acting-user tool-exposure config,
	// given boot as the fallback (the boot-effective mode already reflecting
	// the driver default + boot config). Returns boot unchanged when there is
	// no active revision, no tool-exposure section, or no relevant override.
	EffectiveLoading(ctx context.Context, id identity.Identity, agentID string, t tools.Tool, boot tools.LoadingMode) (tools.LoadingMode, error)
}

// WithLoadingResolver wires the optional per-agent loading-mode resolver. A
// nil resolver is treated as "WithLoadingResolver not supplied".
func WithLoadingResolver(r LoadingResolver) CatalogProjectorOption {
	return func(p *CatalogProjector) {
		if r != nil {
			p.loading = r
		}
	}
}

// Annotator is the optional per-tool annotation backend
// CatalogProjector reads OAuth / approval / metrics / content-stats
// data through. Production wiring supplies an implementation backed by
// tools/auth + tools/approval + the events stream; tests and
// partial-builds run without one.
type Annotator interface {
	// OAuthStatus returns the tool's OAuth binding status.
	OAuthStatus(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolOAuthStatus
	// ApprovalPolicy returns the tool's configured approval policy.
	ApprovalPolicy(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolApprovalPolicy
	// LastUsedAt returns the timestamp of the tool's most recent
	// invocation in the caller's scope; the zero value means "never".
	LastUsedAt(ctx context.Context, id identity.Identity, toolID string) time.Time
	// Metrics returns per-tool error-rate gauges over the window.
	Metrics(ctx context.Context, id identity.Identity, toolID string, window prototypes.ToolMetricsWindow) prototypes.ToolMetrics
	// ContentStats returns the per-tool result-size histogram.
	ContentStats(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolContentStats
	// DisplayModes returns the negotiated MCP-Apps DisplayMode map for
	// the tool (empty for non-MCP tools).
	DisplayModes(ctx context.Context, id identity.Identity, toolID string) map[string]string
}

// CatalogProjectorOption configures NewCatalogProjector.
type CatalogProjectorOption func(*CatalogProjector)

// WithAnnotator wires the per-tool annotation backend. A nil annotator
// is treated as "WithAnnotator not supplied" — the projector returns
// conservative defaults.
func WithAnnotator(a Annotator) CatalogProjectorOption {
	return func(p *CatalogProjector) {
		if a != nil {
			p.annotator = a
		}
	}
}

// NewCatalogProjector builds the V1 production Projector over a
// tools.ToolCatalog. The catalog is mandatory — a nil fails loud with
// ErrMisconfigured. The returned *CatalogProjector is safe for concurrent reuse.
func NewCatalogProjector(catalog tools.ToolCatalog, opts ...CatalogProjectorOption) (*CatalogProjector, error) {
	if catalog == nil {
		return nil, fmt.Errorf("%w: tools.ToolCatalog is nil", ErrMisconfigured)
	}
	p := &CatalogProjector{
		catalog:   catalog,
		overrides: make(map[string]prototypes.ToolApprovalPolicy),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// AnnotationsAvailable reports whether the projector has a per-tool
// Annotator wired (OAuth / approval / last-used / metrics / content-stats /
// display-modes). False on the V1 production build (no Annotator concrete
// exists yet — that is the tools annotator follow-up). This is the SINGLE "annotator-wired"
// capability toggle: the Service loud-rejects the annotator-backed facet
// filters and marks the response-riding aggregates partial when it is false,
// and the tools annotator follow-up's un-gate is one mechanical flip (wire the Annotator →
// AnnotationsAvailable() returns true → the facets + aggregates go live).
func (p *CatalogProjector) AnnotationsAvailable() bool { return p.annotator != nil }

// transportOf maps the runtime tools.TransportKind onto the wire
// ToolTransport enum.
func transportOf(k tools.TransportKind) prototypes.ToolTransport {
	switch k {
	case tools.TransportHTTP:
		return prototypes.ToolTransportHTTP
	case tools.TransportMCP:
		return prototypes.ToolTransportMCP
	case tools.TransportA2A:
		return prototypes.ToolTransportA2A
	case tools.TransportFlow:
		return prototypes.ToolTransportFlow
	default:
		// In-process is the zero / default transport.
		return prototypes.ToolTransportInProc
	}
}

// scopeOf derives the tool's visibility scope. The runtime tools.Tool
// has no explicit scope field; V1 derives it from the transport +
// provenance shape — an MCP / A2A tool is provided by an external
// source and is agent-scoped; a tool that names a Source is
// agent-scoped; everything else is tenant-wide. The Console renders
// this as an informational facet.
func scopeOf(t tools.Tool) string {
	if t.Transport == tools.TransportMCP || t.Transport == tools.TransportA2A {
		return "agent"
	}
	if t.Source != "" {
		return "agent"
	}
	return "tenant"
}

// reliabilityTierOf derives the operator-facing reliability label from
// the tool's side-effect class. Pure / read tools are "standard";
// write / stateful tools are "guarded"; external tools are "external".
func reliabilityTierOf(t tools.Tool) string {
	switch t.SideEffects {
	case tools.SideEffectWrite, tools.SideEffectStateful:
		return "guarded"
	case tools.SideEffectExternal:
		return "external"
	default:
		return "standard"
	}
}

// projectRow maps a runtime tools.Tool onto the wire Tool row,
// resolving the annotated fields through the Annotator (or defaults).
func (p *CatalogProjector) projectRow(ctx context.Context, id identity.Identity, t tools.Tool) prototypes.Tool {
	row := prototypes.Tool{
		ID:              t.Name,
		Name:            t.Name,
		Description:     t.Description,
		Scope:           scopeOf(t),
		Transport:       transportOf(t.Transport),
		ReliabilityTier: reliabilityTierOf(t),
		Owner:           string(t.Source),
		OAuthStatus:     prototypes.ToolOAuthNotApplicable,
		ApprovalPolicy:  prototypes.ToolApprovalAuto,
	}
	if p.annotator != nil {
		row.OAuthStatus = p.annotator.OAuthStatus(ctx, id, t.Name)
		row.ApprovalPolicy = p.annotator.ApprovalPolicy(ctx, id, t.Name)
		row.LastUsedAt = p.annotator.LastUsedAt(ctx, id, t.Name)
	}
	// The in-memory admin override is the FALLBACK for a non-persisting
	// annotator: a `tools.set_approval_policy` call is still observable on
	// the next list — never a silent no-op. When a PERSISTING annotator is
	// wired (the production path), it is the identity-scoped source of truth
	// for ApprovalPolicy, so the in-memory map is neither read nor written —
	// consulting it would re-introduce the cross-session bleed its
	// non-persisting fallback is only tolerable for.
	if !p.approvalPersists() {
		p.adminMu.RLock()
		if ov, ok := p.overrides[overrideKey(id, t.Name)]; ok {
			row.ApprovalPolicy = ov
		}
		p.adminMu.RUnlock()
	}
	return row
}

// fullTripleFilter builds the catalog filter for the identity-scoped
// catalog view: both loading modes (the Console catalog shows every
// registered tool, not just the prompt-time set).
//
// V1 scope note: the filter carries no GrantedScopes, so it projects
// the tools whose AuthScopes are empty — the planner-default-visible
// set. A future phase elevates this to the admin full-discovery view
// (page-tools.md §9: `admin` scope grants "visibility into private
// agent-scoped tools"); that elevation is a deliberate post-V1 carve-
// out, not silent degradation — the V1 projector returns the honest
// default-visible set, never a partial one it pretends is complete.
func fullTripleFilter(id identity.Identity) tools.CatalogFilter {
	return tools.CatalogFilter{
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		SessionID:    id.SessionID,
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	}
}

// ListTools implements Projector.ListTools.
func (p *CatalogProjector) ListTools(ctx context.Context, id identity.Identity) ([]prototypes.Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view, err := p.catalogView(ctx, id, effectiveAgentID(ctx))
	if err != nil {
		return nil, err
	}
	descriptors := view.List()
	rows := make([]prototypes.Tool, 0, len(descriptors))
	for _, t := range descriptors {
		rows = append(rows, p.projectRow(ctx, id, t))
	}
	return rows, nil
}

// resolveDescriptor finds a tool by ID in the identity-scoped catalog.
// It uses List (not Resolve) so the identity-scope predicate is
// honoured — Resolve would leak a tool outside the caller's scope.
func (p *CatalogProjector) resolveDescriptor(ctx context.Context, id identity.Identity, toolID, agentID string) (tools.Tool, bool, error) {
	view, err := p.catalogView(ctx, id, agentID)
	if err != nil {
		return tools.Tool{}, false, err
	}
	for _, t := range view.List() {
		if t.Name == toolID {
			return t, true, nil
		}
	}
	return tools.Tool{}, false, nil
}

func (p *CatalogProjector) catalogView(ctx context.Context, id identity.Identity, agentID string) (tools.PlannerCatalogView, error) {
	if p.view != nil {
		view, err := p.view.CatalogView(ctx, id, agentID)
		if err != nil {
			return nil, err
		}
		if view == nil {
			return nil, fmt.Errorf("%w: CatalogViewResolver returned a nil view", ErrMisconfigured)
		}
		return view, nil
	}
	return tools.NewPlannerView(p.catalog, fullTripleFilter(id)), nil
}

func effectiveAgentID(ctx context.Context) string {
	agentID, _ := tools.EffectiveAgentConfigFrom(ctx)
	return agentID
}

// GetTool implements Projector.GetTool.
func (p *CatalogProjector) GetTool(ctx context.Context, id identity.Identity, toolID string) (prototypes.Tool, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.Tool{}, err
	}
	t, ok, err := p.resolveDescriptor(ctx, id, toolID, effectiveAgentID(ctx))
	if err != nil {
		return prototypes.Tool{}, err
	}
	if !ok {
		return prototypes.Tool{}, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	return p.projectRow(ctx, id, t), nil
}

// DescribeTool implements Projector.DescribeTool. agentID, when non-empty
// and a LoadingResolver is wired (WithLoadingResolver), projects the
// EFFECTIVE loading_mode through the agent's active tool-exposure config; an
// empty agentID or no wired resolver reports the boot-effective mode,
// byte-compatible with the surface before this projection existed.
func (p *CatalogProjector) DescribeTool(ctx context.Context, id identity.Identity, toolID string, agentID string) (prototypes.ToolManifest, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.ToolManifest{}, err
	}
	t, ok, err := p.resolveDescriptor(ctx, id, toolID, agentID)
	if err != nil {
		return prototypes.ToolManifest{}, err
	}
	if !ok {
		return prototypes.ToolManifest{}, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	examples := make([]string, 0, len(t.Examples))
	for _, ex := range t.Examples {
		examples = append(examples, ex.Description)
	}
	loading := string(t.Loading)
	if loading == "" {
		loading = string(tools.LoadingAlways)
	}
	if p.view == nil && p.loading != nil && agentID != "" {
		eff, err := p.loading.EffectiveLoading(ctx, id, agentID, t, tools.LoadingMode(loading))
		if err != nil {
			return prototypes.ToolManifest{}, err
		}
		loading = string(eff)
	}
	manifest := prototypes.ToolManifest{
		Tool:          p.projectRow(ctx, id, t),
		SideEffect:    string(t.SideEffects),
		ArgsSchema:    string(t.ArgsSchema),
		OutSchema:     string(t.OutSchema),
		Examples:      examples,
		AuthScopes:    append([]string(nil), t.AuthScopes...),
		RetryAttempts: t.Policy.MaxRetries,
		TimeoutMS:     int64(t.Policy.TimeoutMS),
		LoadingMode:   loading,
	}
	if p.annotator != nil {
		manifest.DisplayModes = p.annotator.DisplayModes(ctx, id, toolID)
	}
	return manifest, nil
}

// ToolMetrics implements Projector.ToolMetrics.
func (p *CatalogProjector) ToolMetrics(ctx context.Context, id identity.Identity, toolID string, window prototypes.ToolMetricsWindow) (prototypes.ToolMetrics, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.ToolMetrics{}, err
	}
	if _, ok, err := p.resolveDescriptor(ctx, id, toolID, effectiveAgentID(ctx)); err != nil {
		return prototypes.ToolMetrics{}, err
	} else if !ok {
		return prototypes.ToolMetrics{}, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	if p.annotator != nil {
		m := p.annotator.Metrics(ctx, id, toolID, window)
		m.ID = toolID
		m.Window = window
		if !prototypes.IsValidToolStatus(m.Status) {
			m.Status = prototypes.ToolStatusHealthy
		}
		return m, nil
	}
	// No annotator: a registered tool with no observed invocations is
	// Healthy by default — an honest "no failures observed", not a
	// degraded value.
	return prototypes.ToolMetrics{
		ID:     toolID,
		Window: window,
		Status: prototypes.ToolStatusHealthy,
	}, nil
}

// ToolContentStats implements Projector.ToolContentStats.
func (p *CatalogProjector) ToolContentStats(ctx context.Context, id identity.Identity, toolID string) (prototypes.ToolContentStats, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.ToolContentStats{}, err
	}
	if _, ok, err := p.resolveDescriptor(ctx, id, toolID, effectiveAgentID(ctx)); err != nil {
		return prototypes.ToolContentStats{}, err
	} else if !ok {
		return prototypes.ToolContentStats{}, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	if p.annotator != nil {
		cs := p.annotator.ContentStats(ctx, id, toolID)
		cs.ID = toolID
		return cs, nil
	}
	return prototypes.ToolContentStats{
		ID:        toolID,
		Histogram: []prototypes.ToolContentBucket{},
	}, nil
}

// SetApprovalPolicy implements ApprovalPolicySetter. When the wired
// Annotator implements ApprovalPolicySetter the call is delegated (the
// persisting path); otherwise the projector records an in-memory
// override so the change is observable on the next list — never a
// silent no-op.
func (p *CatalogProjector) SetApprovalPolicy(ctx context.Context, id identity.Identity, toolID string, policy prototypes.ToolApprovalPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok, err := p.resolveDescriptor(ctx, id, toolID, effectiveAgentID(ctx)); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	if setter, ok := p.annotator.(ApprovalPolicySetter); ok {
		// The persisting annotator is the identity-scoped source of truth;
		// delegate ONLY. Writing the in-memory override too would leak across
		// sessions (the map is a per-process fallback, the annotator is the
		// durable, isolated store).
		return setter.SetApprovalPolicy(ctx, id, toolID, policy)
	}
	p.adminMu.Lock()
	p.overrides[overrideKey(id, toolID)] = policy
	p.adminMu.Unlock()
	return nil
}

// approvalPersists reports whether the wired annotator persists approval
// policy (implements ApprovalPolicySetter). When true, the annotator is the
// identity-scoped source of truth and the in-memory override map is bypassed.
func (p *CatalogProjector) approvalPersists() bool {
	_, ok := p.annotator.(ApprovalPolicySetter)
	return ok
}

// overrideKey scopes an in-memory approval-policy override by the caller's
// identity triple + tool ID so the non-persisting fallback never bleeds
// across sessions.
func overrideKey(id identity.Identity, toolID string) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID + "\x00" + toolID
}

// RevokeOAuth implements OAuthRevoker. When the wired Annotator
// implements OAuthRevoker the call is delegated; otherwise the
// projector fails loud with ErrAdminUnsupported — there is no
// meaningful in-memory fallback for OAuth-binding revocation.
func (p *CatalogProjector) RevokeOAuth(ctx context.Context, id identity.Identity, toolID string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, ok, err := p.resolveDescriptor(ctx, id, toolID, effectiveAgentID(ctx)); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	revoker, ok := p.annotator.(OAuthRevoker)
	if !ok {
		return 0, fmt.Errorf("%w: tools.revoke_oauth needs an OAuth-aware Annotator", ErrAdminUnsupported)
	}
	return revoker.RevokeOAuth(ctx, id, toolID)
}

// compile-time interface assertions.
var (
	_ Projector            = (*CatalogProjector)(nil)
	_ ApprovalPolicySetter = (*CatalogProjector)(nil)
	_ OAuthRevoker         = (*CatalogProjector)(nil)
)
