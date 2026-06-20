// Package projection holds the run-start agent-config projection shared by
// every run-loop driver (the production dev driver and the harbortest
// devstack twin) and exercised directly by the control-plane integration
// test. Extracting it here keeps the projection logic in ONE place: the
// drivers live in separate binaries (cmd/harbor's package main cannot be
// imported by harbortest/devstack), so an inlined copy in each would drift
// (CLAUDE.md §17.6). The integration test calls the same function, so the
// test exercises the real projection rather than a test-local copy
// (CLAUDE.md §17.4).
package projection

import (
	"context"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
)

// ActiveSkillViews applies an agent's active-config skills membership to the
// run's skill-directory views at run start. It resolves the agent's active
// revision once and, when the revision pins a skills section, keeps only the
// views whose name is in the membership set. A nil registry, an empty
// agentID, or an agent with no active revision / no skills section returns
// the views unchanged — the backward-compatible "ungated" path. A registry
// read error is returned so the caller fails the run loudly (CLAUDE.md §13):
// no silent fall-through to the unfiltered view on a read failure.
//
// The active revision is read ONCE per run; the returned slice is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). `id` carries the run's identity; only the triple is used (the
// registry is identity-scoped, never keyed by run).
func ActiveSkillViews(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, views []skills.SkillView) ([]skills.SkillView, error) {
	if reg == nil || agentID == "" {
		return views, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID)
	if err != nil {
		return nil, err
	}
	if !ok || rev.Payload.Skills == nil {
		return views, nil
	}
	return FilterSkillViewsByMembership(views, rev.Payload.Skills.Names), nil
}

// ActivePlannerCatalogView builds the run's planner-facing catalog view at
// run start, applying the agent's active-config tool exposure: a paused MCP
// server's tools and any individually-disabled tool are excluded from the
// view (next-turn projection — the live transport stays WARM). It always
// returns a usable view: a nil registry, an empty agentID, an agent with no
// active revision, or an active revision with no tool-exposure section (or an
// empty one) returns the plain [tools.NewPlannerView] over cat+filter — the
// backward-compatible "ungated" path. A registry read error is returned so
// the caller fails the run loudly (CLAUDE.md §13): no silent fall-through to
// the unfiltered view on a read failure.
//
// The active revision is read ONCE per run; the returned view is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). Only the identity triple is used (the registry is
// identity-scoped, never keyed by run).
func ActivePlannerCatalogView(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, cat tools.ToolCatalog, filter tools.CatalogFilter) (tools.PlannerCatalogView, error) {
	base := tools.NewPlannerView(cat, filter)
	if reg == nil || agentID == "" {
		return base, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID)
	if err != nil {
		return nil, err
	}
	if !ok || rev.Payload.ToolExposure == nil {
		return base, nil
	}
	paused := rev.Payload.PausedServers()
	disabled := rev.Payload.DisabledTools()
	if len(paused) == 0 && len(disabled) == 0 {
		return base, nil
	}
	return tools.NewExclusionView(base, paused, disabled), nil
}

// FilterSkillViewsByMembership keeps only the views whose Name is in the
// membership set. An empty membership keeps nothing (the rollback-to-empty
// case — an explicit empty skills section disables every skill for the next
// run). The returned slice is always freshly allocated.
func FilterSkillViewsByMembership(views []skills.SkillView, names []string) []skills.SkillView {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	out := make([]skills.SkillView, 0, len(views))
	for _, v := range views {
		if _, ok := set[v.Name]; ok {
			out = append(out, v)
		}
	}
	return out
}
