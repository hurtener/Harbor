// agent_resolver.go — adapts the agent-config registry + the runtime's
// configured default agent id to the protocol.AgentResolver seam.
//
// The Protocol ControlSurface owns the caller-named-agent refusal on
// `start` but must not import the agent-config packages (it depends only
// on the boolean `protocol.AgentResolver` interface). This adapter lives
// at the assembly boundary — the one place allowed to know both the
// concrete registry and the Protocol surface.
package serve

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
)

// AgentResolverAdapter answers the two-check rule for a caller-named
// agent. Immutable after construction; the wrapped registry is itself
// concurrency-safe, so the adapter is too.
type AgentResolverAdapter struct {
	reg       agentcfg.Registry
	defaultID string
	ensure    agentcfg.BootLifecycleEnsurer
}

// AgentResolverOption configures the production adapter's bounded bootstrap
// seam without widening its Protocol-facing interface.
type AgentResolverOption func(*AgentResolverAdapter)

// WithBootLifecycleEnsurer installs the one shared default-agent lifecycle
// materialiser. It is reached only after ControlSurface has checked signed
// agent reach and only for defaultID.
func WithBootLifecycleEnsurer(ensure agentcfg.BootLifecycleEnsurer) AgentResolverOption {
	return func(a *AgentResolverAdapter) { a.ensure = ensure }
}

// NewAgentResolverAdapter builds the adapter from the SAME agent-config
// registry and boot agent id the run-loop driver and the `agent_config.*`
// service already hold, so the edge cannot resolve a different set of
// agents than the run loop will project.
//
// A nil registry or an empty default id is legal: the adapter then
// answers false for every id, which the ControlSurface turns into the
// standard refusal. It is never a silent accept.
func NewAgentResolverAdapter(reg agentcfg.Registry, defaultID string, opts ...AgentResolverOption) *AgentResolverAdapter {
	a := &AgentResolverAdapter{reg: reg, defaultID: defaultID}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ResolveAgent implements protocol.AgentResolver.
//
// Check (i): the id equals the runtime's configured default agent id.
// That agent is the boot-configured one every process serves through but
// never registers as a fleet entity, and nobody need ever have written a
// config revision for it — so without this arm the one id that works
// today would be refused.
//
// Check (ii): an admin-scoped config revision exists for the CALLER's
// tenant under this id. The lookup is byte-identical to the read every
// run-start projection performs, so the edge asks exactly the question
// the run is about to ask.
//
// The non-oracle property is STRUCTURAL: the config key layout puts the
// caller's own tenant in the tenant slot, so a foreign tenant's agent is
// simply not present and answers (false, nil) exactly as a never-existing
// id does. There is no branch here that distinguishes them.
//
// A store error is RETURNED, never folded into false — the surface fails
// the request loud rather than falling through to the default agent.
func (a *AgentResolverAdapter) ResolveAgent(ctx context.Context, ident identity.Identity, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	if a.defaultID != "" && agentID == a.defaultID {
		if a.ensure != nil {
			if err := a.ensure(ctx, ident, agentID); err != nil {
				return false, fmt.Errorf("ensure boot agent lifecycle: %w", err)
			}
		}
		return true, nil
	}
	if a.reg == nil {
		return false, nil
	}
	_, ok, err := a.reg.Active(ctx,
		identity.Quadruple{Identity: ident}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return false, fmt.Errorf("agent-config active revision lookup: %w", err)
	}
	return ok, nil
}

// EffectiveAgentID implements protocol.EffectiveAgentResolver. An omitted
// start target means the configured default agent and must therefore be
// authorized exactly like an explicitly named default.
func (a *AgentResolverAdapter) EffectiveAgentID(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	if a.defaultID == "" {
		return "", fmt.Errorf("configured default agent id is empty")
	}
	return a.defaultID, nil
}

// Compile-time assertion: the adapter satisfies the Protocol seam.
var _ protocol.AgentResolver = (*AgentResolverAdapter)(nil)
var _ protocol.EffectiveAgentResolver = (*AgentResolverAdapter)(nil)
