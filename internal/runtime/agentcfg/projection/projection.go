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

	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
)

// ErrSkillBodyMissing is returned by [ActiveSkillViews] when an agent's
// active config pins an ADMIN skill-membership name whose body is absent
// from the store (e.g. the skill was hard-deleted, or a rollback landed on a
// revision referencing a since-deleted skill). An admin-pinned skill whose
// body is absent from the store is a LOUD failure at run-start projection —
// never a silent drop (CLAUDE.md §13).
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

// RunCompletionHookFromConfig projects the static
// `runtime.hooks.run_completion` yaml block onto a
// steering.CompletionHookSpec, or nil when no static hook is configured (an
// empty / whitespace-only tool). It is the ONE yaml half of the hook
// resolution, shared by every run-loop driver (the production dev binary,
// the devstack twin, and the embed RunOnce path) so the yaml projection
// cannot drift between binaries. AgentID is left empty — the run-start
// resolution stamps the acting agent id; timeout defaulting happens at fire
// time.
func RunCompletionHookFromConfig(rc config.RunCompletionHookConfig) *steering.CompletionHookSpec {
	if strings.TrimSpace(rc.Tool) == "" {
		return nil
	}
	return &steering.CompletionHookSpec{Tool: rc.Tool, Timeout: rc.Timeout}
}

// ActiveRunCompletionHook resolves the run-completion hook for a run at run
// start with next-run projection semantics. Resolution precedence is
// pinned here (and by a table test — CLAUDE.md §17.6): the agent-config
// `hooks` section (when it sets a non-empty run-completion tool) over the
// static yaml default over no hook. The two run-loop drivers (the production
// dev driver and the harbortest devstack twin) call this ONE helper, so the
// precedence cannot drift between binaries.
//
// yamlDefault is the operator's static `runtime.hooks.run_completion`
// projection (nil when unset). The returned spec's AgentID is stamped from
// agentID (registration metadata, never an isolation key — §6). A nil
// registry, an empty agentID, an agent with no active revision, or an active
// revision with no hooks section falls through to yamlDefault. A registry
// read error is returned so the caller fails the run loudly (CLAUDE.md §13):
// no silent fall-through to yaml on a read failure.
//
// The active revision is read ONCE per run; the returned spec is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). Only the identity triple is used (the registry is
// identity-scoped, never keyed by run).
func ActiveRunCompletionHook(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, yamlDefault *steering.CompletionHookSpec) (*steering.CompletionHookSpec, bool, error) {
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return nil, false, err
		}
		if ok {
			if rc, set := rev.Payload.RunCompletionHookView(); set && rc.Tool != "" {
				spec := &steering.CompletionHookSpec{
					Tool:    rc.Tool,
					Timeout: time.Duration(rc.TimeoutMS) * time.Millisecond,
					AgentID: agentID,
				}
				return spec, true, nil
			}
		}
	}
	// Fall through to the static yaml default (when set).
	if yamlDefault != nil && yamlDefault.Tool != "" {
		// Copy so the caller cannot mutate the shared boot-time default, and
		// stamp the acting agent id (the boot default knows no agent).
		spec := &steering.CompletionHookSpec{
			Tool:    yamlDefault.Tool,
			Timeout: yamlDefault.Timeout,
			AgentID: agentID,
		}
		return spec, true, nil
	}
	return nil, false, nil
}

// ActivePlannerCatalogView builds the run's planner-facing catalog view at
// run start, applying the agent's active-config tool exposure: a paused MCP
// server's tools and any individually-disabled tool are excluded from the
// view (next-turn projection — the live transport stays WARM). The exclusion
// set is the order-independent union of THREE narrow-only disable tiers — the
// admin baseline, the durable per-user disable set (which spans that user's
// sessions for the agent), and the ephemeral session overlay — so it can only
// ever grow: no tier can re-expose a tool a higher tier disabled. It always
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
	// Admin exposure (the baseline). The loading-mode override maps are
	// ADMIN-tier only — the durable per-user and ephemeral session tiers
	// below stay narrow-only disable sets with no loading field.
	var adminPaused, adminDisabled []string
	var adminToolExposure *agentcfg.ToolExposure
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return nil, err
		}
		if ok && rev.Payload.ToolExposure != nil {
			adminPaused = rev.Payload.PausedServers()
			adminDisabled = rev.Payload.DisabledTools()
			adminToolExposure = rev.Payload.ToolExposure
		}
	}

	// Loading-mode override projection: when the admin tier carries no
	// override maps, the plain PlannerView is the backward-compatible "no
	// override" path. Otherwise, the inner view is rebuilt over a
	// BROADENED filter (both loading modes) so LoadingOverrideView — not
	// filter.LoadingModes — decides prompt-time visibility against the
	// EFFECTIVE mode. The effective map is resolved ONCE per run from a
	// single catalog listing, so concurrent / in-flight runs never share
	// mutable state (the concurrent-reuse contract).
	var base tools.PlannerCatalogView = tools.NewPlannerView(cat, filter)
	if adminToolExposure != nil && (len(adminToolExposure.ServerLoadingModes) > 0 || len(adminToolExposure.ToolLoadingModes) > 0) {
		broadFilter := filter
		broadFilter.LoadingModes = []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}
		broad := tools.NewPlannerView(cat, broadFilter)
		effective := buildEffectiveLoading(broad.List(), adminToolExposure)
		base = tools.NewLoadingOverrideView(broad, effective, filter.LoadingModes)
	}

	// Durable user-scope exposure: the per-user narrow-only disable set that
	// spans the user's sessions for this agent. The run's full triple is
	// passed; the user-scope storage key (session + run zeroed, agent_id in the
	// session slot) is derived INSIDE the registry — agent_id is a per-agent
	// key here, NEVER an isolation filter, so isolation stays the run's
	// (tenant, user). A read error fails the run loudly (an incomplete triple
	// surfaces identity_required); a missing active user revision is the
	// ungated path. The set is narrow-only — there is no user enable field.
	var userPaused, userDisabled []string
	if reg != nil && agentID != "" {
		urev, uok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
		if err != nil {
			return nil, err
		}
		if uok && urev.Payload.ToolExposure != nil {
			userPaused = urev.Payload.PausedServers()
			userDisabled = urev.Payload.DisabledTools()
		}
	}

	// Session overlay (narrow-only): the session's disable set is UNIONED into
	// the exclusion set — it can only ADD to the disabled set, never remove an
	// admin or user exclusion. The exclusion set can only ever GROW, so a
	// session edit can only narrow the allowed exposure, never widen it. A
	// session "disable" of a source not in the catalog view is inert, and there
	// is no session "enable" path at all.
	overlay, oerr := loadOverlay(ctx, ov, agentID, id)
	if oerr != nil {
		return nil, oerr
	}

	// The three disable sets — admin, user, session — are UNIONED into one
	// grow-only exclusion set. unionSorted is commutative and idempotent, so
	// admin ∪ user ∪ session is order-independent: there is no precedence for
	// tool exposure, only narrowing. Neither the user nor the session tier can
	// re-widen past the admin-provisioned palette.
	paused := unionSorted(unionSorted(adminPaused, userPaused), overlay.DisabledServers)
	disabled := unionSorted(unionSorted(adminDisabled, userDisabled), overlay.DisabledTools)
	if len(paused) == 0 && len(disabled) == 0 {
		return base, nil
	}
	return tools.NewExclusionView(base, paused, disabled), nil
}

// resolveEffectiveLoading applies the top two layers of the loading-mode
// precedence order (tool_loading_modes[name] > server_loading_modes
// [source], the latter restricted to TOOL-form descriptors) to one catalog
// tool. Returns (mode, true) when an override applies; (_, false) when
// neither map names this tool — the caller keeps the boot-effective mode
// (the bottom two precedence layers, already baked into t.Loading at boot).
// This is the ONE canonical implementation shared by the run-start
// projection and the tools.describe effective-mode read surface, so the
// two seams cannot drift (CLAUDE.md §17.6).
func resolveEffectiveLoading(te *agentcfg.ToolExposure, t tools.Tool) (tools.LoadingMode, bool) {
	if te == nil {
		return "", false
	}
	if mode, ok := te.ToolLoadingModes[t.Name]; ok {
		return tools.LoadingMode(mode), true
	}
	if t.Form == tools.ToolFormTool && t.Source != "" {
		if mode, ok := te.ServerLoadingModes[string(t.Source)]; ok {
			return tools.LoadingMode(mode), true
		}
	}
	return "", false
}

// buildEffectiveLoading resolves the loading-mode override for every tool in
// list, returning a map keyed by catalog Name containing ONLY the entries an
// override actually changes (a tool with no applicable override is omitted
// — [tools.LoadingOverrideView] falls back to the tool's own boot-effective
// Loading for an absent key). A nil ToolExposure section returns nil.
func buildEffectiveLoading(list []tools.Tool, te *agentcfg.ToolExposure) map[string]tools.LoadingMode {
	if te == nil {
		return nil
	}
	out := make(map[string]tools.LoadingMode, len(list))
	for _, t := range list {
		if mode, ok := resolveEffectiveLoading(te, t); ok {
			out[t.Name] = mode
		}
	}
	return out
}

// EffectiveLoadingMode resolves tool t's projected EFFECTIVE LoadingMode
// under agentID's ADMIN-tier active config revision — the SAME precedence
// [ActivePlannerCatalogView] applies at run start, exposed for the
// `tools.describe` read surface's optional `agent_id` path. boot is the
// boot-effective mode (already reflecting the driver default + any boot
// `tools.entries[].loading_mode`); a nil registry, an empty agentID, or an
// agent with no active revision / no tool-exposure section returns boot
// unchanged — the backward-compatible path byte-identical to `tools.describe`
// behaviour before this projection existed. A registry read error is
// returned so the caller fails the request loudly (CLAUDE.md §13).
func EffectiveLoadingMode(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, t tools.Tool, boot tools.LoadingMode) (tools.LoadingMode, error) {
	if reg == nil || agentID == "" {
		return boot, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return "", err
	}
	if !ok || rev.Payload.ToolExposure == nil {
		return boot, nil
	}
	if mode, ok := resolveEffectiveLoading(rev.Payload.ToolExposure, t); ok {
		return mode, nil
	}
	return boot, nil
}

// LoadingResolverAdapter adapts a Registry into the `internal/tools/protocol`
// package's narrow LoadingResolver seam (the `tools.describe` optional
// `agent_id` path) via [EffectiveLoadingMode], so the SAME projection
// precedence backs both the run-start prompt-time projection and the read
// surface — one shared helper, no binary-local reimplementation (CLAUDE.md
// §17.6). Satisfies `tools/protocol.LoadingResolver` structurally; this
// package does not import `tools/protocol` to avoid a needless dependency
// (Go interfaces are satisfied structurally).
type LoadingResolverAdapter struct {
	Registry agentcfg.Registry
}

// EffectiveLoading implements the `tools/protocol.LoadingResolver` seam.
func (a LoadingResolverAdapter) EffectiveLoading(ctx context.Context, id identity.Identity, agentID string, t tools.Tool, boot tools.LoadingMode) (tools.LoadingMode, error) {
	return EffectiveLoadingMode(ctx, a.Registry, agentID, identity.Quadruple{Identity: id}, t, boot)
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

	// Durable USER-scope layer (the per-user standing instruction the durable
	// user-scope config tier persists): read back the active user-scope
	// revision's user_prompt and
	// compose it BETWEEN the admin user layer and the ephemeral session
	// overlay — admin Base > admin User > USER-durable > session User. A read
	// error fails the run loudly (no silent drop of the durable layer).
	durableUser, derr := activeDurableUserPrompt(ctx, reg, agentID, id)
	if derr != nil {
		return nil, derr
	}
	user := composeUserLayer(adminUser, durableUser, overlay.UserPrompt)

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

// composeUserLayer joins the three caller-authored user layers — the admin
// user layer, the durable user-scope layer, and the ephemeral session user
// layer, IN THAT ORDER — into the single lower-trust `<user_instructions>`
// block. The order is the security boundary (admin Base, the always-present
// spine, sits above all three): a later, lower-trust layer can EXTEND the
// operator's standing instruction, never precede or weaken it. Any segment
// may be empty (whitespace-only segments are dropped); an all-empty input
// yields "".
func composeUserLayer(adminUser, durableUser, sessionUser string) string {
	segs := make([]string, 0, 3)
	for _, s := range []string{adminUser, durableUser, sessionUser} {
		if t := strings.TrimSpace(s); t != "" {
			segs = append(segs, t)
		}
	}
	return strings.Join(segs, "\n\n")
}

// activeDurableUserPrompt resolves the caller's active USER-scope durable
// config revision and returns its user_prompt — the durable user-scope prompt
// layer. It keys by the run's identity triple with agent_id as the per-agent
// key (the USER config scope), so the real (tenant, user) is the isolation
// principal and the tuple is never widened. nil registry / empty agentID / no
// active user revision / a revision with no user prompt yields "" (the
// backward-compatible "no durable user layer" path). A registry read error is
// returned so the caller fails the run loudly — never a silent drop.
func activeDurableUserPrompt(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (string, error) {
	if reg == nil || agentID == "" {
		return "", nil
	}
	rev, found, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	user, _ := rev.Payload.UserPrompt()
	return user, nil
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
