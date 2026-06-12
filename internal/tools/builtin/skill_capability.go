package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
)

// runCapability computes the capability envelope the skill_* built-in
// delegations pass to the Phase-38 handlers.
//
// The envelope is SERVER-computed, never LLM-supplied: the builtin
// arg shapes deliberately omit the `capability` field so a model
// cannot widen its own allowed-tool set and surface skills the run
// is not entitled to. AllowedTools is the run's full reachable tool
// set — `tools.VisibleNames` over the catalog under the run's
// identity triple + the operator-declared granted scopes, both
// loading modes (Always + Deferred, since `tool_search` makes the
// deferred set reachable).
//
// AllowedNamespaces / AllowedTags stay EMPTY: Harbor has no runtime
// source of namespace / tag grants (no config surface declares them),
// so per the Phase-38 default-deny stance a skill whose RequiredNS /
// RequiredTags is non-empty is filtered out. A recorded decision —
// when a grants surface lands, this is the one place to thread it.
//
// A missing identity yields an empty envelope; the downstream handler
// rejects the call on its own identity gate before the envelope is
// consulted, so the empty value is never load-bearing.
func runCapability(ctx context.Context, rc RegistryContext) skilltools.CapabilityContext {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		if id, ok2 := identity.From(ctx); ok2 {
			q = identity.Quadruple{Identity: id}
		}
	}
	names := tools.VisibleNames(rc.Catalog, tools.CatalogFilter{
		TenantID:      q.TenantID,
		UserID:        q.UserID,
		SessionID:     q.SessionID,
		GrantedScopes: rc.GrantedScopes,
	})
	return skilltools.CapabilityContext{AllowedTools: names}
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
