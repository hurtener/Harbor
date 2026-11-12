package projection

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// ActiveRunLimits freezes the agent's per-run limits for one run. An absent
// agent-config section inherits the runtime default; a present section is
// authoritative, and a field it omits means no limit for that dimension.
// The value is copied, so later revisions cannot change an in-flight run.
func ActiveRunLimits(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, runtimeDefault planner.RunLimits) (planner.RunLimits, error) {
	if reg == nil || agentID == "" {
		return runtimeDefault, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return planner.RunLimits{}, err
	}
	if !ok || rev.Payload.RunLimits == nil {
		return runtimeDefault, nil
	}
	var out planner.RunLimits
	if s := rev.Payload.RunLimits.MaxDurationSeconds; s != nil && *s > 0 {
		out.MaxDuration = time.Duration(*s) * time.Second
	}
	if t := rev.Payload.RunLimits.MaxTokens; t != nil && *t > 0 {
		out.MaxTokens = *t
	}
	return out, out.Validate()
}
