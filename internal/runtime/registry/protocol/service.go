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
	"github.com/hurtener/Harbor/internal/runtime/registry"
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
	// ErrControlScopeRequired — a fleet-control verb (Pause / Drain /
	// Restart / ForceStop / Deregister) was called without the elevated
	// `auth.ScopeAdmin` control claim. Fails closed.
	ErrControlScopeRequired = errors.New("registry/protocol: fleet-control requires the elevated control-scope claim")
	// ErrControlUnsupported — a fleet-control verb was called on a Service
	// built without a Controller (WithController). The dev/server wiring
	// always injects one; this guards a misconfiguration rather than
	// nil-panicking (CLAUDE.md §5).
	ErrControlUnsupported = errors.New("registry/protocol: fleet-control not wired on this Service")
	// ErrScopeMismatch — an admin-widened `agents.list` fleet enumeration
	// (a non-empty Filter.TenantIDs) was issued without the verified
	// `auth.ScopeAdmin` claim. Fails closed — never a silent narrowing to
	// own scope (CLAUDE.md §6 rule 5 / §13).
	ErrScopeMismatch = errors.New("registry/protocol: fleet enumeration requires the admin scope claim")
)

// Projector is the read seam the Service depends on. The V1 production
// implementation is RegistryProjector. Every method takes the verified
// identity triple so the implementation scopes its reads by the
// (tenant, user, session) tuple — NEVER by agent_id.
type Projector interface {
	// ListAgents returns every agent visible to id, in agent_id order.
	// The Service applies the facet filter + pagination on top.
	ListAgents(ctx context.Context, id identity.Identity) ([]prototypes.Agent, error)
	// ListTenantAgents returns every agent across ALL sessions of the
	// named tenants — the admin-widened FLEET read. It is invoked by
	// Service.List ONLY after the admin-scope gate passes; each row carries
	// full per-(tenant, user, session) identity attribution. This lands the
	// aggregating fleet projector behind the unchanged Projector interface.
	ListTenantAgents(ctx context.Context, tenantIDs []string) ([]prototypes.Agent, error)
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

// Controller is the mutate seam the five fleet-control verbs depend on.
// The V1 production implementation is the
// in-process `*registry.Registry`, whose control methods self-enforce
// `registry.HasControlScope(ctx)` and emit the `agent.*` events. Every
// method takes the verified identity (folded into ctx by the handler)
// plus the control-scope claim (attached by the Service before the call)
// agent_id is the target handle, never an isolation key.
type Controller interface {
	Pause(ctx context.Context, agentID, reason string) error
	Drain(ctx context.Context, agentID, reason string) error
	Restart(ctx context.Context, agentID, reason string) error
	ForceStop(ctx context.Context, agentID, reason string) error
	Deregister(ctx context.Context, agentID string) error
}

// Service implements the thirteen `agents.*` Protocol methods (the eight
// read projections + the five fleet-control verbs). It is a concurrency-safe
// compiled artifact — immutable after NewService.
type Service struct {
	projector  Projector
	controller Controller
	bus        events.EventBus // optional — nil ⇒ admin audit emit is logged only
	redactor   audit.Redactor  // optional — defence-in-depth before the emit
	logger     *slog.Logger
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

// WithBus wires the canonical events.EventBus the Service publishes the
// `audit.admin_scope_used` event onto when an admin-widened `agents.list`
// fleet enumeration succeeds. A nil bus is treated as "WithBus not
// supplied" — the fleet path still works, but the audit observation is
// logged at Info instead of published (the admin action is NEVER fully
// silent — CLAUDE.md §13).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the Service runs the
// `audit.admin_scope_used` payload through before publishing. A nil
// redactor is treated as "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(s *Service) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithController injects the fleet-control mutate seam
// Without it, the five control verbs fail closed with
// ErrControlUnsupported. The production wiring passes the in-process
// `*registry.Registry`.
func WithController(c Controller) Option {
	return func(s *Service) {
		if c != nil {
			s.controller = c
		}
	}
}

// NewService builds the Agents Protocol service over a Projector. The
// projector is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5). The returned *Service is immutable after
// construction and safe for concurrent use by N goroutines.
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
// agent_id never enters this validation.
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

// List implements the `agents.list` method. It validates identity,
// resolves the agent set from the Projector — the caller's own
// identity-scoped set, or (when the request names tenants for fleet
// widening AND the verified admin claim is present) the admin-widened set
// across the named tenants — applies the facet filter + free-text search
// + pagination, computes the filtered-view aggregates, and emits an
// `audit.admin_scope_used` event on a successful fleet enumeration.
//
// adminScoped is the verified-JWT scope decision the wire handler computes
// from `auth.HasScope(ctx, auth.ScopeAdmin)`. It is consulted ONLY when
// the request names tenants for fleet widening (Filter.TenantIDs); a
// non-widened request never requires it and stays byte-compatible with
// the identity-scoped read.
func (s *Service) List(ctx context.Context, req prototypes.AgentListRequest, adminScoped bool) (prototypes.AgentListResponse, error) {
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

	// Fleet widening. A non-empty Filter.TenantIDs is the admin-widened
	// FLEET-enumeration selector: enumerate every agent across all sessions
	// of the named tenants. Widening requires the verified admin claim — a
	// widened request without it fails LOUD with ErrScopeMismatch, never a
	// silent narrowing to own scope (CLAUDE.md §6 rule 5 / §13).
	widenTenants := dedupeNonEmpty(req.Filter.TenantIDs)
	widened := len(widenTenants) > 0
	if widened && !adminScoped {
		return prototypes.AgentListResponse{}, fmt.Errorf(
			"%w: agents.list named %d tenant(s) for fleet enumeration", ErrScopeMismatch, len(widenTenants))
	}

	var all []prototypes.Agent
	if widened {
		// adminScoped is guaranteed true here.
		all, err = s.projector.ListTenantAgents(ctx, widenTenants)
	} else {
		all, err = s.projector.ListAgents(ctx, id)
	}
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

	if widened {
		s.emitAdminAudit(ctx, id, "agents.list", len(widenTenants))
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

// dedupeNonEmpty returns the input slice with empty strings dropped and
// duplicates collapsed, preserving first-seen order. It normalises the
// admin-widened Filter.TenantIDs selector so the gate + audit count the
// distinct named tenants.
func dedupeNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
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

// Pause implements the `agents.pause` fleet-control verb.
func (s *Service) Pause(ctx context.Context, req prototypes.AgentControlRequest, controlScoped bool) (prototypes.AgentControlResponse, error) {
	return s.doControl(ctx, req, controlScoped, "pause",
		func(c context.Context, id string) error { return s.controller.Pause(c, id, req.Reason) })
}

// Drain implements the `agents.drain` fleet-control verb.
func (s *Service) Drain(ctx context.Context, req prototypes.AgentControlRequest, controlScoped bool) (prototypes.AgentControlResponse, error) {
	return s.doControl(ctx, req, controlScoped, "drain",
		func(c context.Context, id string) error { return s.controller.Drain(c, id, req.Reason) })
}

// Restart implements the `agents.restart` fleet-control verb.
func (s *Service) Restart(ctx context.Context, req prototypes.AgentControlRequest, controlScoped bool) (prototypes.AgentControlResponse, error) {
	return s.doControl(ctx, req, controlScoped, "restart",
		func(c context.Context, id string) error { return s.controller.Restart(c, id, req.Reason) })
}

// ForceStop implements the `agents.force_stop` fleet-control verb.
func (s *Service) ForceStop(ctx context.Context, req prototypes.AgentControlRequest, controlScoped bool) (prototypes.AgentControlResponse, error) {
	return s.doControl(ctx, req, controlScoped, "force_stop",
		func(c context.Context, id string) error { return s.controller.ForceStop(c, id, req.Reason) })
}

// Deregister implements the `agents.deregister` fleet-control verb.
// Irreversible; the reason is not carried to the registry
// (registry.Deregister takes no reason).
func (s *Service) Deregister(ctx context.Context, req prototypes.AgentControlRequest, controlScoped bool) (prototypes.AgentControlResponse, error) {
	return s.doControl(ctx, req, controlScoped, "deregister",
		func(c context.Context, id string) error { return s.controller.Deregister(c, id) })
}

// doControl is the shared body of the five fleet-control verbs: validate
// identity, reject an empty id, fail closed without the control claim
// (BEFORE touching the registry), attach the registry control-scope
// claim, invoke the registry, then report the agent's ACTUAL post-control
// status.
//
// No fabrication (CLAUDE.md §13): the returned Status is re-read from the
// registry via the projector, NOT the command's intended effect. This
// matters because the V1 registry has no "paused" Health state — `pause`
// and `restart` emit their `agent.*` event (the Console's observation
// surface) but leave the observable status `active`; only `drain`
// (→drained), `force_stop` (→force_stopped) and `deregister` (→gone)
// transition the stored status. Reporting the real status keeps the
// control response consistent with what `agents.list` / `agents.get`
// show. `deregister` removes the agent, so its terminal status is
// `deregistered` (no re-read — the agent is gone).
func (s *Service) doControl(
	ctx context.Context,
	req prototypes.AgentControlRequest,
	controlScoped bool,
	command string,
	invoke func(ctx context.Context, agentID string) error,
) (prototypes.AgentControlResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.AgentControlResponse{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.AgentControlResponse{}, fmt.Errorf("%w: agent id is empty", ErrInvalidRequest)
	}
	// Fleet control is a more-elevated privilege tier than observation.
	// Fail closed without the claim — before touching the
	// registry.
	if !controlScoped {
		return prototypes.AgentControlResponse{}, fmt.Errorf("%w: command=%q", ErrControlScopeRequired, command)
	}
	if s.controller == nil {
		return prototypes.AgentControlResponse{}, ErrControlUnsupported
	}
	// Attach the registry's control-scope claim its own HasControlScope
	// gate requires. The identity triple is already on ctx (folded by the
	// handler); the registry scopes by it, never by agent_id.
	if err := invoke(registry.WithControlScope(ctx), req.ID); err != nil {
		return prototypes.AgentControlResponse{}, mapControlErr(err)
	}
	resp := prototypes.AgentControlResponse{AgentID: req.ID, Command: command}
	if command == "deregister" {
		// The agent is gone — re-reading would 404. Its terminal status.
		resp.Status = prototypes.AgentStatusDeregistered
		return resp, nil
	}
	// Re-read the ACTUAL post-control status (no fabrication). A racing
	// deregister between the control and the re-read leaves the agent
	// gone — report the honest terminal state. But ONLY the not-found
	// case collapses to `deregistered`: any OTHER re-read error (a
	// transient StateStore failure, an unmarshal error) must surface
	// loudly rather than masquerade as a successful deregister
	// (CLAUDE.md §13 — no silent degradation, no fabricated terminal
	// status). The control command itself already succeeded.
	got, gerr := s.projector.GetAgent(ctx, id, req.ID)
	if gerr != nil {
		if errors.Is(gerr, ErrAgentNotFound) {
			resp.Status = prototypes.AgentStatusDeregistered
			return resp, nil
		}
		return prototypes.AgentControlResponse{}, mapProjectorErr(gerr)
	}
	resp.Status = got.Agent.Status
	return resp, nil
}

// mapControlErr maps a registry control error onto the Service sentinel
// set so the wire handler can branch on a stable error.
func mapControlErr(err error) error {
	switch {
	case errors.Is(err, registry.ErrAgentNotFound):
		return ErrAgentNotFound
	case errors.Is(err, registry.ErrControlScopeRequired):
		return ErrControlScopeRequired
	default:
		return fmt.Errorf("registry/protocol: control: %w", err)
	}
}

// mapProjectorErr maps a Projector error onto the Service sentinel set
// so the wire handler can branch on a stable error.
func mapProjectorErr(err error) error {
	if errors.Is(err, ErrAgentNotFound) {
		return err
	}
	return fmt.Errorf("registry/protocol: projector: %w", err)
}
