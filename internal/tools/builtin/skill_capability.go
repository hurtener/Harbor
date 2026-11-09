package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
)

// skillReader returns the explicitly supplied read projection, falling back
// to the historical SkillStore field for source-compatible boot wiring.
func skillReader(rc RegistryContext) skills.SkillReader {
	if rc.SkillReader != nil {
		return rc.SkillReader
	}
	return rc.SkillStore
}

// runCapability computes the capability envelope the skill_* built-in
// delegations pass to the skills-tools handlers.
//
// The envelope is SERVER-computed, never LLM-supplied: the builtin arg
// shapes deliberately omit the `capability` field so a model cannot widen
// its own allowed-tool set and surface skills the run is not entitled to.
// AllowedTools is the calling run's full reachable tool set — the discovery
// set of the run tool view seated on ctx (both loading modes, since
// `tool_search` makes the deferred set reachable), narrowed by the
// operator-declared granted scopes. It is never a process-catalog listing:
// the catalog holds every principal's tools.
//
// AllowedNamespaces / AllowedTags stay EMPTY: Harbor has no runtime
// source of namespace / tag grants (no config surface declares them),
// so per the skills-tools default-deny stance a skill whose RequiredNS /
// RequiredTags is non-empty is filtered out. A recorded decision —
// when a grants surface lands, this is the one place to thread it.
//
// A missing identity yields an empty envelope and no error: the downstream
// handler rejects the call on its own identity gate (and emits its
// rejection event) before the envelope is consulted. With identity but no
// seated run view the call fails closed with ErrRunToolViewRequired.
func runCapability(ctx context.Context, rc RegistryContext) (skilltools.CapabilityContext, error) {
	if _, err := requireIdentity(ctx); err != nil {
		return skilltools.CapabilityContext{}, nil
	}
	names, err := runAllowedTools(ctx, rc.GrantedScopes)
	if err != nil {
		return skilltools.CapabilityContext{}, err
	}
	return skilltools.CapabilityContext{AllowedTools: names}, nil
}

// runAllowedTools is the sorted reachable tool-name set of the run view
// seated on ctx, narrowed by grantedScopes. Fails closed without a view.
func runAllowedTools(ctx context.Context, grantedScopes []string) ([]string, error) {
	discovered, _, ok := tools.RunToolDiscovery(ctx)
	if !ok {
		return nil, ErrRunToolViewRequired
	}
	granted := make(map[string]struct{}, len(grantedScopes))
	for _, s := range grantedScopes {
		granted[s] = struct{}{}
	}
	permitted := make([]tools.Tool, 0, len(discovered))
	for _, t := range discovered {
		if tools.ScopesGranted(t.AuthScopes, granted) {
			permitted = append(permitted, t)
		}
	}
	return tools.SortedToolNames(permitted), nil
}

// requireSkillDeps is the shared registration-time gate for the
// skill_* built-ins: a nil Bus is a boot-path bug (the delegated
// handlers emit `skill.identity_rejected` through it), so it fails
// the registration loudly rather than panicking at invoke time.
func requireSkillDeps(name string, rc RegistryContext) error {
	if rc.Bus == nil {
		return fmt.Errorf("%s: RegistryContext.Bus is required (events.EventBus) — the delegated skills handlers emit identity rejections through it", name)
	}
	return nil
}
