package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Sentinel errors the Service returns. The wire handler maps each onto
// a canonical Protocol Code + HTTP status; in-process callers compare
// with errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. RFC §5.5 / CLAUDE.md §6 rule 9 — fails closed.
	ErrIdentityRequired = errors.New("registry/protocol: identity scope incomplete")
	// ErrAgentNotFound — the requested agent_id is not registered in
	// the registry visible to the caller's identity scope.
	ErrAgentNotFound = errors.New("registry/protocol: agent not found")
	// ErrInvalidRequest — the request was structurally invalid (an
	// empty agent id, an out-of-range page size).
	ErrInvalidRequest = errors.New("registry/protocol: invalid request")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("registry/protocol: NewService missing a mandatory dependency")
)

// Projector is the read seam the Service depends on. The V1 production
// implementation is RegistryProjector. Every method takes the verified
// identity triple so the implementation scopes its reads by the
// (tenant, user, session) tuple — NEVER by agent_id (D-059).
type Projector interface {
	// ListAgents returns every agent visible to id, in agent_id order.
	// The Service applies the facet filter + pagination on top.
	ListAgents(ctx context.Context, id identity.Identity) ([]prototypes.Agent, error)
	// GetAgent returns the full projection for agentID, or
	// ErrAgentNotFound.
	GetAgent(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentGetResponse, error)
	// AgentTools returns the agent's tool bindings, or ErrAgentNotFound.
	AgentTools(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentToolBinding, error)
	// AgentMemory returns the agent's memory binding, or
	// ErrAgentNotFound.
	AgentMemory(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentMemoryBinding, error)
	// AgentGovernance returns the agent's governance posture, or
	// ErrAgentNotFound.
	AgentGovernance(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentGovernance, error)
	// AgentSkills returns the agent's attached skills, or
	// ErrAgentNotFound.
	AgentSkills(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentSkillBinding, error)
	// AgentPermissions returns the agent's permission model, or
	// ErrAgentNotFound.
	AgentPermissions(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentPermissions, error)
	// Metrics returns the registry-wide rollup for id's scope.
	Metrics(ctx context.Context, id identity.Identity) (prototypes.AgentMetrics, error)
}

// Service implements the eight `agents.*` Protocol methods. It is a
// D-025-safe compiled artifact — immutable after NewService.
type Service struct {
	projector Projector
	logger    *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithLogger sets the slog.Logger the Service logs projector failures
// to. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the Agents Protocol service over a Projector. The
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
// identity.Identity, failing closed on an incomplete triple. The
// (tenant, user, session) tuple is the ONLY isolation principal —
// agent_id never enters this validation (D-059).
func validIdentity(scope prototypes.IdentityScope) (identity.Identity, error) {
	id := identity.Identity{
		TenantID:  scope.Tenant,
		UserID:    scope.User,
		SessionID: scope.Session,
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	return id, nil
}

// List implements the `agents.list` method. It validates identity,
// resolves the identity-scoped agent set from the Projector, applies the
// facet filter + free-text search + pagination, and computes the
// filtered-view aggregates.
func (s *Service) List(ctx context.Context, req prototypes.AgentListRequest) (prototypes.AgentListResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentListResponse{}, err
	}

	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = prototypes.DefaultAgentListPageSize
	}
	if pageSize < 0 || pageSize > prototypes.MaxAgentListPageSize {
		return prototypes.AgentListResponse{}, fmt.Errorf("%w: page_size %d outside [1,%d]",
			ErrInvalidRequest, pageSize, prototypes.MaxAgentListPageSize)
	}
	page := req.Page
	if page <= 0 {
		page = 1
	}

	all, err := s.projector.ListAgents(ctx, id)
	if err != nil {
		return prototypes.AgentListResponse{}, fmt.Errorf("registry/protocol: list: %w", err)
	}

	filtered := make([]prototypes.Agent, 0, len(all))
	for _, a := range all {
		if agentMatches(req.Filter, a) {
			filtered = append(filtered, a)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	aggregates := computeAggregates(filtered)

	totalRows := int64(len(filtered))
	pageCount := 1
	if pageSize > 0 {
		pageCount = (len(filtered) + pageSize - 1) / pageSize
		if pageCount == 0 {
			pageCount = 1
		}
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	rows := []prototypes.Agent{}
	if start < len(filtered) {
		if end > len(filtered) {
			end = len(filtered)
		}
		rows = filtered[start:end]
	}

	return prototypes.AgentListResponse{
		Agents:     rows,
		Page:       page,
		PageSize:   pageSize,
		PageCount:  pageCount,
		TotalRows:  totalRows,
		Aggregates: aggregates,
	}, nil
}

// agentMatches reports whether an agent satisfies the facet filter.
func agentMatches(f prototypes.AgentFilter, a prototypes.Agent) bool {
	if len(f.Status) > 0 {
		matched := false
		for _, st := range f.Status {
			if a.Status == st {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(f.PlannerType) > 0 {
		matched := false
		for _, pt := range f.PlannerType {
			if a.PlannerType == pt {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if q := strings.TrimSpace(strings.ToLower(f.Search)); q != "" {
		hay := strings.ToLower(a.Name + " " + a.Description)
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// computeAggregates folds the filtered view into the four counters.
func computeAggregates(agents []prototypes.Agent) prototypes.AgentAggregates {
	agg := prototypes.AgentAggregates{Total: int64(len(agents))}
	for _, a := range agents {
		switch a.Status {
		case prototypes.AgentStatusActive:
			agg.Active++
		case prototypes.AgentStatusPaused:
			agg.Paused++
		case prototypes.AgentStatusDrained:
			agg.Drained++
		}
	}
	return agg
}

// Get implements the `agents.get` method — the full projection of one
// agent.
func (s *Service) Get(ctx context.Context, req prototypes.AgentGetRequest) (prototypes.AgentGetResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentGetResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentGetResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	resp, err := s.projector.GetAgent(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentGetResponse{}, mapProjectorErr(err)
	}
	return resp, nil
}

// Tools implements the `agents.tools` method.
func (s *Service) Tools(ctx context.Context, req prototypes.AgentToolsRequest) (prototypes.AgentToolsResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentToolsResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentToolsResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	bindings, err := s.projector.AgentTools(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentToolsResponse{}, mapProjectorErr(err)
	}
	return prototypes.AgentToolsResponse{AgentID: req.ID, Bindings: bindings}, nil
}

// Memory implements the `agents.memory` method.
func (s *Service) Memory(ctx context.Context, req prototypes.AgentMemoryRequest) (prototypes.AgentMemoryResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentMemoryResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentMemoryResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	binding, err := s.projector.AgentMemory(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentMemoryResponse{}, mapProjectorErr(err)
	}
	return prototypes.AgentMemoryResponse{AgentID: req.ID, Binding: binding}, nil
}

// Governance implements the `agents.governance` method.
func (s *Service) Governance(ctx context.Context, req prototypes.AgentGovernanceRequest) (prototypes.AgentGovernanceResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentGovernanceResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentGovernanceResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	gov, err := s.projector.AgentGovernance(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentGovernanceResponse{}, mapProjectorErr(err)
	}
	return prototypes.AgentGovernanceResponse{AgentID: req.ID, Governance: gov}, nil
}

// Skills implements the `agents.skills` method.
func (s *Service) Skills(ctx context.Context, req prototypes.AgentSkillsRequest) (prototypes.AgentSkillsResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentSkillsResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentSkillsResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	skills, err := s.projector.AgentSkills(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentSkillsResponse{}, mapProjectorErr(err)
	}
	return prototypes.AgentSkillsResponse{AgentID: req.ID, Skills: skills}, nil
}

// Permissions implements the `agents.permissions` method.
func (s *Service) Permissions(ctx context.Context, req prototypes.AgentPermissionsRequest) (prototypes.AgentPermissionsResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentPermissionsResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentPermissionsResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	perms, err := s.projector.AgentPermissions(ctx, id, req.ID)
	if err != nil {
		return prototypes.AgentPermissionsResponse{}, mapProjectorErr(err)
	}
	return prototypes.AgentPermissionsResponse{AgentID: req.ID, Permissions: perms}, nil
}

// Metrics implements the `agents.metrics` method — the registry-wide
// rollup over the caller's identity scope.
func (s *Service) Metrics(ctx context.Context, req prototypes.AgentMetricsRequest) (prototypes.AgentMetricsResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentMetricsResponse{}, err
	}
	m, err := s.projector.Metrics(ctx, id)
	if err != nil {
		return prototypes.AgentMetricsResponse{}, fmt.Errorf("registry/protocol: metrics: %w", err)
	}
	return prototypes.AgentMetricsResponse{Metrics: m}, nil
}

// mapProjectorErr maps a Projector error onto the Service sentinel set
// so the wire handler can branch on a stable error.
func mapProjectorErr(err error) error {
	if errors.Is(err, ErrAgentNotFound) {
		return err
	}
	return fmt.Errorf("registry/protocol: projector: %w", err)
}
