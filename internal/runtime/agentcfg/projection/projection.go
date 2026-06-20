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
	"github.com/hurtener/Harbor/internal/planner"
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

// ActivePromptLayers resolves the agent's active-config layered system
// prompt at run start. It returns the base + user layer text and whether the
// active revision carries a prompt-layer section (ok). A nil registry, an
// empty agentID, an agent with no active revision, or an active revision with
// no prompt-layer section returns ("", "", false, nil) — the
// backward-compatible "no durable prompt layers" path (the run keeps its
// configured base). A registry read error is returned so the caller fails the
// run loudly (CLAUDE.md §13): no silent fall-through on a read failure.
//
// An unset layer within a present section returns the empty string for that
// layer (the caller treats empty as "inherit the configured default" for the
// base, and "no user layer" for the user layer).
//
// The active revision is read ONCE per run; the returned values are plain
// strings, so concurrent / in-flight runs keep their own snapshot (the
// concurrent-reuse contract). Only the identity triple is used (the registry
// is identity-scoped, never keyed by run).
func ActivePromptLayers(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (base string, user string, ok bool, err error) {
	if reg == nil || agentID == "" {
		return "", "", false, nil
	}
	rev, found, rerr := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID)
	if rerr != nil {
		return "", "", false, rerr
	}
	if !found || rev.Payload.PromptLayers == nil {
		return "", "", false, nil
	}
	base, _ = rev.Payload.BasePrompt()
	user, _ = rev.Payload.UserPrompt()
	return base, user, true, nil
}

// ApplyPromptLayers overlays the agent's active-config durable prompt layers
// onto the run's resolved per-run override bundle at run start. It is the
// ONE shared seam both run-loop drivers (the production dev driver and the
// harbortest devstack twin) call after resolving the LLM-parameter overrides,
// so the two binaries cannot drift (CLAUDE.md §17.6).
//
// A non-empty base layer is set as ov.BasePromptLayer (overriding the run's
// configured base at the prompt builder); a non-empty user layer is set as
// ov.UserPromptLayer (composed below the base in the lower-trust position).
// An empty layer is treated as unset (the base inherits the configured
// default; no user layer is added) — so a run with no durable prompt layers
// is unchanged. When ov is nil but a layer is present, a fresh bundle is
// allocated. A registry read error is returned so the caller fails the run
// loudly. The returned bundle is fresh-per-run (no shared mutable state).
func ApplyPromptLayers(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error) {
	base, user, ok, err := ActivePromptLayers(ctx, reg, agentID, id)
	if err != nil {
		return nil, err
	}
	if !ok || (base == "" && user == "") {
		return ov, nil
	}
	if ov == nil {
		ov = &planner.LLMOverrides{}
	}
	if base != "" {
		b := base
		ov.BasePromptLayer = &b
	}
	if user != "" {
		u := user
		ov.UserPromptLayer = &u
	}
	return ov, nil
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
