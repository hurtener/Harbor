// Package protocol implements the seven `tools.*` Protocol methods the
// Console Tools page (Phase 73f / D-116) consumes:
//
//   - tools.list           — paginated, faceted catalog projection.
//   - tools.get            — single catalog-row projection.
//   - tools.describe       — full manifest projection.
//   - tools.metrics        — per-tool error-rate gauges + status pill.
//   - tools.content_stats  — per-tool result-size histogram + DisplayMode.
//   - tools.set_approval_policy — ADMIN: mutate a tool's approval policy.
//   - tools.revoke_oauth   — ADMIN: revoke a tool's OAuth bindings.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the `Projector` interface, not on a concrete
// tool catalog. The V1 production implementation is `CatalogProjector`
// (catalog_projector.go) — a thin read-only projection over a
// `tools.ToolCatalog` plus optional metric / admin backends. A future
// remote-catalog projector slots in behind the same interface without
// reshaping the Service.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's `IdentityScope`. An incomplete
// triple fails closed with `ErrIdentityRequired` — there is no
// identity-downgrading knob. The Service NEVER reads identity from a
// package-level global; the triple flows in via the request.
//
// # Admin gating (D-079)
//
// `tools.set_approval_policy` and `tools.revoke_oauth` MUTATE runtime
// tool state and require the verified `auth.ScopeAdmin` claim. The
// Service receives an `adminScoped bool` the wire handler computes from
// the verified JWT scope set; a false value on an admin method fails
// closed with `ErrAdminScopeRequired`. There is NO `tools.admin` scope
// — the closed two-scope set (`admin` + `console:fleet`) is the only
// admit surface, and the Tools admin methods gate on `ScopeAdmin`.
//
// # Concurrent reuse (D-025)
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Projector
// reference + an optional bus + logger; every method's per-call state
// lives in the call's arguments and locals, never on the Service.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Sentinel errors the Service returns. The wire handler maps each onto
// a canonical Protocol Code + HTTP status; in-process callers compare
// with errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. RFC §5.5 / CLAUDE.md §6 rule 9 — fails closed.
	ErrIdentityRequired = errors.New("tools/protocol: identity scope incomplete")
	// ErrAdminScopeRequired — an admin method (`tools.set_approval_policy`
	// / `tools.revoke_oauth`) was called without the verified
	// `auth.ScopeAdmin` claim (D-079).
	ErrAdminScopeRequired = errors.New("tools/protocol: admin scope claim required")
	// ErrToolNotFound — the requested tool ID is not registered in the
	// catalog visible to the caller's identity scope.
	ErrToolNotFound = errors.New("tools/protocol: tool not found")
	// ErrInvalidRequest — the request was structurally invalid (an
	// empty tool ID, an out-of-range page size, an unknown enum value).
	ErrInvalidRequest = errors.New("tools/protocol: invalid request")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("tools/protocol: NewService missing a mandatory dependency")
	// ErrAdminUnsupported — an admin method was called but the
	// Projector does not implement the corresponding admin backend.
	// Fails loud rather than silently no-op'ing (CLAUDE.md §13).
	ErrAdminUnsupported = errors.New("tools/protocol: admin backend not configured")
)

// Projector is the read + mutate seam the Service depends on. The V1
// production implementation is CatalogProjector. Every method takes the
// verified identity triple so the implementation scopes its reads —
// the Service never trusts a Projector to apply identity itself for the
// list path, but passes the triple so the implementation CAN scope a
// per-tenant view.
type Projector interface {
	// ListTools returns every catalog row visible to id, sorted by Name.
	// The Service applies the facet filter + pagination on top; the
	// Projector returns the full identity-scoped set.
	ListTools(ctx context.Context, id identity.Identity) ([]prototypes.Tool, error)
	// GetTool returns the catalog row for toolID, or ErrToolNotFound.
	GetTool(ctx context.Context, id identity.Identity, toolID string) (prototypes.Tool, error)
	// DescribeTool returns the full manifest for toolID, or
	// ErrToolNotFound.
	DescribeTool(ctx context.Context, id identity.Identity, toolID string) (prototypes.ToolManifest, error)
	// ToolMetrics returns per-tool error-rate gauges for toolID over the
	// resolved window, or ErrToolNotFound.
	ToolMetrics(ctx context.Context, id identity.Identity, toolID string, window prototypes.ToolMetricsWindow) (prototypes.ToolMetrics, error)
	// ToolContentStats returns the per-tool result-size histogram for
	// toolID, or ErrToolNotFound.
	ToolContentStats(ctx context.Context, id identity.Identity, toolID string) (prototypes.ToolContentStats, error)
}

// ApprovalPolicySetter is the optional admin backend the
// `tools.set_approval_policy` method requires. A Projector that does
// not implement it makes the method fail loud with ErrAdminUnsupported.
type ApprovalPolicySetter interface {
	// SetApprovalPolicy mutates toolID's approval policy. Returns
	// ErrToolNotFound when toolID is unknown.
	SetApprovalPolicy(ctx context.Context, id identity.Identity, toolID string, policy prototypes.ToolApprovalPolicy) error
}

// OAuthRevoker is the optional admin backend the `tools.revoke_oauth`
// method requires. A Projector that does not implement it makes the
// method fail loud with ErrAdminUnsupported.
type OAuthRevoker interface {
	// RevokeOAuth revokes every OAuth binding for toolID and returns the
	// count revoked. Returns ErrToolNotFound when toolID is unknown.
	RevokeOAuth(ctx context.Context, id identity.Identity, toolID string) (int64, error)
}

// Service implements the seven `tools.*` Protocol methods. It is a
// D-025-safe compiled artifact — immutable after NewService.
type Service struct {
	projector Projector
	bus       events.EventBus // optional — nil ⇒ audit emit is logged only
	redactor  audit.Redactor  // optional — nil ⇒ audit emit is logged only
	logger    *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithBus wires the canonical events.EventBus the Service publishes the
// `audit.admin_scope_used` event onto when an admin method succeeds. A
// nil bus is treated as "WithBus not supplied" — the admin path still
// works, but the audit observation is logged at Info instead of
// published. Production wiring SHOULD supply both WithBus and
// WithRedactor so the admin audit trail reaches the bus.
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the Service runs the
// `audit.admin_scope_used` payload through before publishing. A nil
// redactor is treated as "WithRedactor not supplied". The
// AdminScopeUsedPayload is a SafePayload by construction; the redactor
// is wired for defence-in-depth + parity with the Phase 72b emit site.
func WithRedactor(r audit.Redactor) Option {
	return func(s *Service) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithLogger sets the slog.Logger the Service logs admin actions and
// audit-emit failures to. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the Tools Protocol service over a Projector. The
// projector is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5). The returned *Service is immutable after
// construction (D-025) and safe for concurrent use by N goroutines.
func NewService(projector Projector, opts ...Option) (*Service, error) {
	if projector == nil {
		return nil, fmt.Errorf("%w: Projector is nil", ErrMisconfigured)
	}
	s := &Service{
		projector: projector,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// validIdentity validates the wire IdentityScope into an
// identity.Identity, failing closed on an incomplete triple.
func validIdentity(scope prototypes.IdentityScope) (identity.Identity, error) {
	id := identity.Identity{
		TenantID:  scope.Tenant,
		UserID:    scope.User,
		SessionID: scope.Session,
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return id, nil
}

// List implements the `tools.list` method. It validates identity,
// resolves the identity-scoped catalog from the Projector, applies the
// facet filter + free-text search + pagination, and computes the
// filtered-view aggregates.
func (s *Service) List(ctx context.Context, req prototypes.ToolListRequest) (prototypes.ToolListResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolListResponse{}, err
	}

	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = prototypes.DefaultToolListPageSize
	}
	if pageSize < 0 || pageSize > prototypes.MaxToolListPageSize {
		return prototypes.ToolListResponse{}, fmt.Errorf("%w: page_size %d outside [1,%d]",
			ErrInvalidRequest, pageSize, prototypes.MaxToolListPageSize)
	}
	page := req.Page
	if page <= 0 {
		page = 1
	}

	if err = validateFilter(req.Filter); err != nil {
		return prototypes.ToolListResponse{}, err
	}

	all, err := s.projector.ListTools(ctx, id)
	if err != nil {
		return prototypes.ToolListResponse{}, fmt.Errorf("tools/protocol: list: %w", err)
	}

	// Apply the facet filter + free-text search.
	filtered := make([]prototypes.Tool, 0, len(all))
	for _, t := range all {
		if filterMatches(req.Filter, t) {
			filtered = append(filtered, t)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	aggregates := computeAggregates(filtered)

	totalRows := int64(len(filtered))
	pageCount := 1
	if pageSize > 0 {
		pageCount = (len(filtered) + pageSize - 1) / pageSize
		if pageCount == 0 {
			pageCount = 1
		}
	}

	// Page slice — out-of-range pages return an empty slice, never an error.
	start := (page - 1) * pageSize
	end := start + pageSize
	rows := []prototypes.Tool{}
	if start < len(filtered) {
		if end > len(filtered) {
			end = len(filtered)
		}
		rows = filtered[start:end]
	}

	return prototypes.ToolListResponse{
		Tools:      rows,
		Page:       page,
		PageSize:   pageSize,
		PageCount:  pageCount,
		TotalRows:  totalRows,
		Aggregates: aggregates,
	}, nil
}

// Get implements the `tools.get` method — a single catalog-row
// projection.
func (s *Service) Get(ctx context.Context, req prototypes.ToolGetRequest) (prototypes.Tool, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.Tool{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.Tool{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	t, err := s.projector.GetTool(ctx, id, req.ID)
	if err != nil {
		return prototypes.Tool{}, mapProjectorErr(err)
	}
	return t, nil
}

// Describe implements the `tools.describe` method — the full manifest
// projection.
func (s *Service) Describe(ctx context.Context, req prototypes.ToolDescribeRequest) (prototypes.ToolManifest, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolManifest{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.ToolManifest{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	m, err := s.projector.DescribeTool(ctx, id, req.ID)
	if err != nil {
		return prototypes.ToolManifest{}, mapProjectorErr(err)
	}
	return m, nil
}

// Metrics implements the `tools.metrics` method — per-tool error-rate
// gauges + status pill over the resolved window.
func (s *Service) Metrics(ctx context.Context, req prototypes.ToolMetricsRequest) (prototypes.ToolMetrics, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolMetrics{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.ToolMetrics{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	window := req.Window
	if window == "" {
		window = prototypes.ToolWindow1h
	}
	if !prototypes.IsValidToolMetricsWindow(window) {
		return prototypes.ToolMetrics{}, fmt.Errorf("%w: unknown metrics window %q", ErrInvalidRequest, window)
	}
	m, err := s.projector.ToolMetrics(ctx, id, req.ID, window)
	if err != nil {
		return prototypes.ToolMetrics{}, mapProjectorErr(err)
	}
	return m, nil
}

// ContentStats implements the `tools.content_stats` method — the
// per-tool result-size histogram + negotiated DisplayMode snapshot.
func (s *Service) ContentStats(ctx context.Context, req prototypes.ToolContentStatsRequest) (prototypes.ToolContentStats, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolContentStats{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.ToolContentStats{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	cs, err := s.projector.ToolContentStats(ctx, id, req.ID)
	if err != nil {
		return prototypes.ToolContentStats{}, mapProjectorErr(err)
	}
	return cs, nil
}

// SetApprovalPolicy implements the `tools.set_approval_policy` ADMIN
// method. adminScoped is the verified-JWT scope decision the wire
// handler computes; a false value fails closed with
// ErrAdminScopeRequired (D-079). On success, emits an
// `audit.admin_scope_used` event.
func (s *Service) SetApprovalPolicy(ctx context.Context, req prototypes.ToolSetApprovalPolicyRequest, adminScoped bool) (prototypes.ToolSetApprovalPolicyResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolSetApprovalPolicyResponse{}, err
	}
	if !adminScoped {
		return prototypes.ToolSetApprovalPolicyResponse{},
			fmt.Errorf("%w: tools.set_approval_policy requires the verified `admin` scope claim", ErrAdminScopeRequired)
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.ToolSetApprovalPolicyResponse{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	if !prototypes.IsValidToolApprovalPolicy(req.Policy) {
		return prototypes.ToolSetApprovalPolicyResponse{},
			fmt.Errorf("%w: unknown approval policy %q", ErrInvalidRequest, req.Policy)
	}
	setter, ok := s.projector.(ApprovalPolicySetter)
	if !ok {
		return prototypes.ToolSetApprovalPolicyResponse{},
			fmt.Errorf("%w: tools.set_approval_policy", ErrAdminUnsupported)
	}
	if err := setter.SetApprovalPolicy(ctx, id, req.ID, req.Policy); err != nil {
		return prototypes.ToolSetApprovalPolicyResponse{}, mapProjectorErr(err)
	}
	s.emitAdminAudit(ctx, id, string(prototypes.ToolApprovalAuto), "tools.set_approval_policy", req.ID)
	return prototypes.ToolSetApprovalPolicyResponse{ID: req.ID, Policy: req.Policy}, nil
}

// RevokeOAuth implements the `tools.revoke_oauth` ADMIN method.
// adminScoped is the verified-JWT scope decision; a false value fails
// closed with ErrAdminScopeRequired (D-079). On success, emits an
// `audit.admin_scope_used` event.
func (s *Service) RevokeOAuth(ctx context.Context, req prototypes.ToolRevokeOAuthRequest, adminScoped bool) (prototypes.ToolRevokeOAuthResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.ToolRevokeOAuthResponse{}, err
	}
	if !adminScoped {
		return prototypes.ToolRevokeOAuthResponse{},
			fmt.Errorf("%w: tools.revoke_oauth requires the verified `admin` scope claim", ErrAdminScopeRequired)
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.ToolRevokeOAuthResponse{}, fmt.Errorf("%w: tool id is empty", ErrInvalidRequest)
	}
	revoker, ok := s.projector.(OAuthRevoker)
	if !ok {
		return prototypes.ToolRevokeOAuthResponse{}, fmt.Errorf("%w: tools.revoke_oauth", ErrAdminUnsupported)
	}
	count, err := revoker.RevokeOAuth(ctx, id, req.ID)
	if err != nil {
		return prototypes.ToolRevokeOAuthResponse{}, mapProjectorErr(err)
	}
	s.emitAdminAudit(ctx, id, "", "tools.revoke_oauth", req.ID)
	return prototypes.ToolRevokeOAuthResponse{ID: req.ID, RevokedCount: count}, nil
}

// mapProjectorErr maps a Projector error onto the Service sentinel set
// so the wire handler can branch on a stable error.
func mapProjectorErr(err error) error {
	if errors.Is(err, ErrToolNotFound) {
		return err
	}
	return fmt.Errorf("tools/protocol: projector: %w", err)
}
