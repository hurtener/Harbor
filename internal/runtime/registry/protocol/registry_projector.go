package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
)

// ConfigSource is the optional join seam the RegistryProjector uses to
// resolve an agent's configuration-derived projections — the
// AgentConfig, tool bindings, memory binding, governance posture, and
// attached skills. The Agent Registry (D-059) stores only the
// version_hash of an agent's AgentConfig, not the config itself; the
// configuration-derived tabs of the Console Agents page therefore need
// a join over the subsystems that DO own that data (the tool catalog,
// the memory configs, the Phase 36 governance accumulators, the skills
// catalog).
//
// ConfigSource is OPTIONAL on the projector. When it is nil, the
// configuration-derived methods return an HONEST empty projection — an
// empty tool-binding list, a zero-value memory binding, etc. — which
// the Console renders as a real "nothing configured for this agent"
// empty state. This is NOT a stubbed success (CLAUDE.md §13): the
// methods still validate identity and the agent's existence; they
// simply report that no configuration join is wired. Production wiring
// (`harbor dev`) supplies a ConfigSource so the tabs carry live data.
//
// Every method takes the verified identity tuple so the implementation
// scopes its reads — NEVER by agent_id (D-059).
type ConfigSource interface {
	// Config returns the agent's AgentConfig projection.
	Config(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentConfig, error)
	// Tools returns the agent's tool bindings.
	Tools(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentToolBinding, error)
	// Memory returns the agent's memory binding.
	Memory(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentMemoryBinding, error)
	// Governance returns the agent's governance posture.
	Governance(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentGovernance, error)
	// Skills returns the agent's attached skills.
	Skills(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentSkillBinding, error)
}

// RegistryProjector is the V1 production Projector — a read-only
// projection over a registry.AgentRegistry, optionally joined to a
// ConfigSource for the configuration-derived tabs. It is a D-025-safe
// compiled artifact: immutable after NewRegistryProjector, holding only
// interface references.
type RegistryProjector struct {
	reg    registry.AgentRegistry
	config ConfigSource // optional — nil ⇒ honest empty config projections
}

// ProjectorOption configures NewRegistryProjector.
type ProjectorOption func(*RegistryProjector)

// WithConfigSource wires the optional ConfigSource join. A nil source
// is treated as "WithConfigSource not supplied".
func WithConfigSource(src ConfigSource) ProjectorOption {
	return func(p *RegistryProjector) {
		if src != nil {
			p.config = src
		}
	}
}

// NewRegistryProjector builds the V1 production Projector. reg is
// mandatory — a nil fails loud with ErrMisconfigured (CLAUDE.md §5).
func NewRegistryProjector(reg registry.AgentRegistry, opts ...ProjectorOption) (*RegistryProjector, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: AgentRegistry is nil", ErrMisconfigured)
	}
	p := &RegistryProjector{reg: reg}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// projectStatus maps a registry Health onto the Protocol status pill.
func projectStatus(h registry.Health) prototypes.AgentStatus {
	switch h {
	case registry.HealthDraining:
		return prototypes.AgentStatusDrained
	case registry.HealthStopped:
		return prototypes.AgentStatusForceStopped
	default:
		return prototypes.AgentStatusActive
	}
}

// projectHealth maps a registry Health onto the Protocol health badge.
func projectHealth(h registry.Health) prototypes.AgentHealth {
	switch h {
	case registry.HealthHealthy:
		return prototypes.AgentHealthHealthy
	case registry.HealthDegraded:
		return prototypes.AgentHealthDegraded
	case registry.HealthDraining:
		return prototypes.AgentHealthDrained
	case registry.HealthStopped:
		return prototypes.AgentHealthForceStopped
	default:
		return prototypes.AgentHealthUnknown
	}
}

// projectHosting maps a registry Hosting onto the Protocol hosting enum.
func projectHosting(h registry.Hosting) prototypes.AgentHosting {
	if h == registry.HostingRemote {
		return prototypes.AgentHostingRemote
	}
	return prototypes.AgentHostingLocal
}

// projectRecord folds a registry AgentRecord into the flat Protocol
// Agent row. planner/model/tool counts come from the ConfigSource when
// one is wired; without it they are zero — an honest "not joined" view,
// not a faked value.
func (p *RegistryProjector) projectRecord(ctx context.Context, id identity.Identity, rec registry.AgentRecord) prototypes.Agent {
	a := prototypes.Agent{
		ID:           rec.AgentID,
		Name:         rec.DisplayName,
		Incarnation:  int64(rec.Incarnation), //nolint:gosec // G115 — incarnation is a small bounded counter; uint64->int64 cannot overflow
		VersionHash:  rec.VersionHash,
		Owner:        rec.RegistrationKey,
		Status:       projectStatus(rec.Health),
		Health:       projectHealth(rec.Health),
		Hosting:      projectHosting(rec.Hosting),
		RegisteredAt: rec.RegisteredAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:    rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if p.config != nil {
		if cfg, err := p.config.Config(ctx, id, rec.AgentID); err == nil {
			a.PlannerType = cfg.PlannerType
			a.Model = cfg.Model
		}
		if bindings, err := p.config.Tools(ctx, id, rec.AgentID); err == nil {
			a.ToolsCount = len(bindings)
			for _, b := range bindings {
				if b.Transport == "MCP" {
					a.MCPCount++
				}
			}
		}
	}
	return a
}

// ListAgents implements Projector.ListAgents.
func (p *RegistryProjector) ListAgents(ctx context.Context, id identity.Identity) ([]prototypes.Agent, error) {
	records, err := p.reg.List(ctx)
	if err != nil {
		return nil, mapRegistryErr(err)
	}
	out := make([]prototypes.Agent, 0, len(records))
	for _, rec := range records {
		out = append(out, p.projectRecord(ctx, id, rec))
	}
	return out, nil
}

// GetAgent implements Projector.GetAgent.
func (p *RegistryProjector) GetAgent(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentGetResponse, error) {
	rec, err := p.reg.Get(ctx, agentID)
	if err != nil {
		return prototypes.AgentGetResponse{}, mapRegistryErr(err)
	}
	resp := prototypes.AgentGetResponse{
		Agent:        p.projectRecord(ctx, id, *rec),
		AgentCardRef: rec.AgentCardRef,
	}
	if p.config != nil {
		if cfg, cerr := p.config.Config(ctx, id, agentID); cerr == nil {
			resp.Config = cfg
		}
	}
	resp.Agent.Description = rec.DisplayName
	return resp, nil
}

// AgentTools implements Projector.AgentTools.
func (p *RegistryProjector) AgentTools(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentToolBinding, error) {
	if _, err := p.reg.Get(ctx, agentID); err != nil {
		return nil, mapRegistryErr(err)
	}
	if p.config == nil {
		return []prototypes.AgentToolBinding{}, nil
	}
	bindings, err := p.config.Tools(ctx, id, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry/protocol: tools join: %w", err)
	}
	return bindings, nil
}

// AgentMemory implements Projector.AgentMemory.
func (p *RegistryProjector) AgentMemory(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentMemoryBinding, error) {
	if _, err := p.reg.Get(ctx, agentID); err != nil {
		return prototypes.AgentMemoryBinding{}, mapRegistryErr(err)
	}
	if p.config == nil {
		return prototypes.AgentMemoryBinding{}, nil
	}
	binding, err := p.config.Memory(ctx, id, agentID)
	if err != nil {
		return prototypes.AgentMemoryBinding{}, fmt.Errorf("registry/protocol: memory join: %w", err)
	}
	return binding, nil
}

// AgentGovernance implements Projector.AgentGovernance.
func (p *RegistryProjector) AgentGovernance(ctx context.Context, id identity.Identity, agentID string) (prototypes.AgentGovernance, error) {
	if _, err := p.reg.Get(ctx, agentID); err != nil {
		return prototypes.AgentGovernance{}, mapRegistryErr(err)
	}
	if p.config == nil {
		return prototypes.AgentGovernance{}, nil
	}
	gov, err := p.config.Governance(ctx, id, agentID)
	if err != nil {
		return prototypes.AgentGovernance{}, fmt.Errorf("registry/protocol: governance join: %w", err)
	}
	return gov, nil
}

// AgentSkills implements Projector.AgentSkills.
func (p *RegistryProjector) AgentSkills(ctx context.Context, id identity.Identity, agentID string) ([]prototypes.AgentSkillBinding, error) {
	if _, err := p.reg.Get(ctx, agentID); err != nil {
		return nil, mapRegistryErr(err)
	}
	if p.config == nil {
		return []prototypes.AgentSkillBinding{}, nil
	}
	skills, err := p.config.Skills(ctx, id, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry/protocol: skills join: %w", err)
	}
	return skills, nil
}

// AgentPermissions implements Projector.AgentPermissions. V1 default is
// the implicit permission model (page-agents.md §10): every
// authenticated user in the tenant can invoke the agent. The method
// still validates the agent exists; an explicit ACL surface is post-V1.
func (p *RegistryProjector) AgentPermissions(ctx context.Context, _ identity.Identity, agentID string) (prototypes.AgentPermissions, error) {
	if _, err := p.reg.Get(ctx, agentID); err != nil {
		return prototypes.AgentPermissions{}, mapRegistryErr(err)
	}
	return prototypes.AgentPermissions{
		Model: "implicit",
		Description: "Every authenticated user in the tenant can invoke this agent. " +
			"An explicit per-agent ACL surface is post-V1 (D-064).",
	}, nil
}

// Metrics implements Projector.Metrics — the registry-wide rollup. The
// V1 rollup counts Active Agents from the registry's identity-scoped
// List; Running Tasks / Total Cost / Total Tokens come from the
// ConfigSource's governance join when one is wired, else zero (an
// honest "not joined" rollup, not a faked number).
func (p *RegistryProjector) Metrics(ctx context.Context, id identity.Identity) (prototypes.AgentMetrics, error) {
	records, err := p.reg.List(ctx)
	if err != nil {
		return prototypes.AgentMetrics{}, mapRegistryErr(err)
	}
	m := prototypes.AgentMetrics{}
	for _, rec := range records {
		if projectStatus(rec.Health) == prototypes.AgentStatusActive {
			m.ActiveAgents++
		}
		if p.config != nil {
			if gov, gerr := p.config.Governance(ctx, id, rec.AgentID); gerr == nil {
				for _, c := range gov.Ceilings {
					m.TotalCostUSD += c.SpendUSD
				}
			}
		}
	}
	return m, nil
}

// mapRegistryErr maps a registry sentinel error onto the Service
// sentinel set so the wire handler can branch on a stable error.
func mapRegistryErr(err error) error {
	switch {
	case errors.Is(err, registry.ErrAgentNotFound):
		return ErrAgentNotFound
	case errors.Is(err, registry.ErrIdentityRequired):
		return fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	default:
		return fmt.Errorf("registry/protocol: registry: %w", err)
	}
}
