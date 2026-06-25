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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
)

// ErrSkillBodyMissing is returned by [ActiveSkillViews] when an agent's
// active config pins an ADMIN skill-membership name whose body is absent
// from the store (e.g. the skill was hard-deleted, or a rollback landed on a
// revision referencing a since-deleted skill). Per the 92c plan this is a
// LOUD failure at run-start projection — never a silent drop (CLAUDE.md §13).
// Session-personal skill names are exempt (a safe-subset add that may
// legitimately not be in the directory view).
var ErrSkillBodyMissing = errors.New("agentcfg/projection: agent-config pins a skill whose body is absent from the store")

// loadOverlay reads the session's safe-subset overlay (the lower tier of the
// authorization matrix) for the run's REAL (tenant, user, session) triple. A
// nil store, an empty agentID, or an agent with no overlay returns the zero
// overlay (and a nil error) — the backward-compatible "no session overlay"
// path. An
// overlay read error is returned so the caller fails the run loudly
// (CLAUDE.md §13). The overlay is keyed by the real triple (NOT the synthetic
// agentcfg identity), so it is session-isolated by construction.
func loadOverlay(ctx context.Context, ov sessionoverlay.Store, agentID string, id identity.Quadruple) (sessionoverlay.Overlay, error) {
	if ov == nil || agentID == "" {
		return sessionoverlay.Overlay{}, nil
	}
	o, _, err := ov.Get(ctx, identity.Quadruple{Identity: id.Identity}, agentID)
	return o, err
}

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
func ActiveSkillViews(ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, agentID string, id identity.Quadruple, views []skills.SkillView) ([]skills.SkillView, error) {
	// Resolve the session's ephemeral personal-skill names FIRST: they are a
	// safe-subset ADD (the session's own session-scoped skills, never a
	// capability the admin restricts) that survive the admin membership
	// filter below. They never promote past the session (the overlay + the
	// SkillStore are both session-keyed).
	overlay, oerr := loadOverlay(ctx, ov, agentID, id)
	if oerr != nil {
		return nil, oerr
	}
	personal := overlay.PersonalSkills

	if reg == nil || agentID == "" {
		return views, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return nil, err
	}
	if !ok || rev.Payload.Skills == nil {
		// The admin pins no skills membership: every directory-visible skill
		// (which already includes the session's personal skills, stored under
		// the session triple) is in scope.
		return views, nil
	}
	// The admin pinned a membership. An ADMIN-pinned name whose body is ABSENT
	// from the directory view is a LOUD failure (the plan's "skill not found
	// in store", §13) — a rollback onto a since-hard-deleted skill must fail
	// the run, not quietly run without a skill the admin expects. (Session
	// PERSONAL names stay silent below — a safe-subset add that may
	// legitimately not be in the view.)
	present := make(map[string]struct{}, len(views))
	for _, v := range views {
		present[v.Name] = struct{}{}
	}
	for _, name := range rev.Payload.Skills.Names {
		if _, ok := present[name]; !ok {
			return nil, fmt.Errorf("%w: agent %q pins skill %q", ErrSkillBodyMissing, agentID, name)
		}
	}
	// Keep the admin members AND add back the session's personal skills (the
	// session-overlay ON TOP of the admin baseline). A personal name absent
	// from views is harmless — FilterSkillViewsByMembership keeps only names
	// present in views.
	allowed := append(append([]string(nil), rev.Payload.Skills.Names...), personal...)
	return FilterSkillViewsByMembership(views, allowed), nil
}

// ActiveLLMOverrides resolves the PER-AGENT LLM-parameter override layer from
// the agent's active config revision at run start. It returns the per-agent
// sampling defaults (model / temperature / max-tokens / reasoning-effort) the
// agent has pinned, as a [planner.LLMOverrides] carrying ONLY those four
// dimensions — the layer the run loop folds BETWEEN the session override and
// the tenant-wide baseline (precedence session › per-agent › tenant-wide
// baseline › config default).
//
// A nil registry, an empty agentID, an agent with no active revision, or an
// active revision with no LLM-params section returns (nil, nil) — the
// backward-compatible "no per-agent override" path. A registry read error is
// returned so the caller fails the run loudly (CLAUDE.md §13): no silent
// fall-through to the tenant baseline on a read failure.
//
// The active revision is read ONCE per run; the returned bundle is fresh
// (its pointers are copies), so concurrent / in-flight runs keep their own
// snapshot (the concurrent-reuse contract). `id` carries the run's identity;
// only the triple is used (the registry is identity-scoped, never keyed by
// run). This is sampling parameters only — ExtraInstructions / prompt layers
// are resolved elsewhere (the agent-config prompt-layer projection), so the
// per-agent LLM layer never carries prompt text.
func ActiveLLMOverrides(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (*planner.LLMOverrides, error) {
	if reg == nil || agentID == "" {
		return nil, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	lp, set := rev.Payload.LLMParamsView()
	if !set {
		return nil, nil
	}
	// Copy each pointer so the returned bundle never shares backing storage
	// with the (immutable) stored revision.
	out := &planner.LLMOverrides{}
	any := false
	if lp.Model != nil && *lp.Model != "" {
		v := *lp.Model
		out.Model = &v
		any = true
	}
	if lp.Temperature != nil {
		v := *lp.Temperature
		out.Temperature = &v
		any = true
	}
	if lp.MaxTokens != nil {
		v := *lp.MaxTokens
		out.MaxTokens = &v
		any = true
	}
	if lp.ReasoningEffort != nil && *lp.ReasoningEffort != "" {
		v := *lp.ReasoningEffort
		out.ReasoningEffort = &v
		any = true
	}
	if !any {
		return nil, nil
	}
	return out, nil
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
func ActivePlannerCatalogView(ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, agentID string, id identity.Quadruple, cat tools.ToolCatalog, filter tools.CatalogFilter) (tools.PlannerCatalogView, error) {
	base := tools.NewPlannerView(cat, filter)

	// Admin exposure (the baseline).
	var adminPaused, adminDisabled []string
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return nil, err
		}
		if ok && rev.Payload.ToolExposure != nil {
			adminPaused = rev.Payload.PausedServers()
			adminDisabled = rev.Payload.DisabledTools()
		}
	}

	// Session overlay (narrow-only): the session's disable set is UNIONED into
	// the admin exclusion set — it can only ADD to the disabled set, never
	// remove an admin exclusion. The exclusion set can only ever GROW, so a
	// session edit can only narrow the admin-allowed exposure, never widen it.
	// A session "disable" of a source the admin did not expose is inert (it is
	// not in the catalog view), and there is no session "enable" path at all.
	overlay, oerr := loadOverlay(ctx, ov, agentID, id)
	if oerr != nil {
		return nil, oerr
	}

	paused := unionSorted(adminPaused, overlay.DisabledServers)
	disabled := unionSorted(adminDisabled, overlay.DisabledTools)
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
	rev, found, rerr := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
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
func ApplyPromptLayers(ctx context.Context, reg agentcfg.Registry, overlayStore sessionoverlay.Store, agentID string, id identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error) {
	base, adminUser, _, err := ActivePromptLayers(ctx, reg, agentID, id)
	if err != nil {
		return nil, err
	}

	// Session user layer (the safe subset): the session writes ONLY the user
	// layer — the overlay shape carries no base field, so a session caller
	// physically cannot edit the operator base. The session layer composes
	// ABOVE the admin base in the lower-trust `<user_instructions>` position
	// (escaped by the prompt builder), appended below any admin user layer.
	overlay, oerr := loadOverlay(ctx, overlayStore, agentID, id)
	if oerr != nil {
		return nil, oerr
	}
	user := composeUserLayer(adminUser, overlay.UserPrompt)

	// The admin base is ALWAYS the spine — it is never sourced from the
	// session overlay (base-unwritable-by-session is structural).
	if base == "" && user == "" {
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

// composeUserLayer joins the durable admin user layer and the session user
// layer (admin first, then session) into the single lower-trust
// `<user_instructions>` block. Either may be empty; an empty pair yields "".
func composeUserLayer(adminUser, sessionUser string) string {
	adminUser = strings.TrimSpace(adminUser)
	sessionUser = strings.TrimSpace(sessionUser)
	switch {
	case adminUser == "":
		return sessionUser
	case sessionUser == "":
		return adminUser
	default:
		return adminUser + "\n\n" + sessionUser
	}
}

// unionSorted returns the sorted, de-duplicated union of two string sets. The
// union is the structural enforcement of NARROW-ONLY in the tool-exposure
// projection: a session disable set can only ADD to the admin exclusion set,
// never remove a member — so the resulting exclusion can only grow and the
// session can only narrow the admin-allowed exposure.
func unionSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
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
