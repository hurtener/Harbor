package builtin

import (
	"context"
	"fmt"

	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// SkillSearchArgs is the LLM-facing input shape for the `skill_search`
// built-in. Phase 111d (D-201): the shape deliberately OMITS the rich
// handler's `capability` field — the envelope is server-computed from
// the run's visible-tool set (see skill_capability.go) so a model
// cannot self-grant a wider skill view. The pre-111d `tags` filter is
// dropped; tag-scoped enumeration is `skill_list`'s job.
type SkillSearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// registerSkillSearch installs the `skill_search` built-in as a thin
// delegation to the Phase-38 handler (`skilltools.SearchHandler`):
// capability filter + tool-name redaction run on every production
// call. The pre-111d package-local query/projection body is deleted —
// `internal/skills/tools` is the single implementation home.
func registerSkillSearch(rc RegistryContext) error {
	if err := requireSkillDeps("skill_search", rc); err != nil {
		return err
	}
	return inproc.RegisterFunc[SkillSearchArgs, skilltools.SearchResult](
		rc.Catalog, skilltools.ToolNameSkillSearch,
		func(ctx context.Context, args SkillSearchArgs) (skilltools.SearchResult, error) {
			if rc.SkillStore == nil {
				return skilltools.SearchResult{}, fmt.Errorf("skill_search: backing SkillStore is nil — `skill_search` was registered without skills.SkillStore deps (operator misconfiguration)")
			}
			return skilltools.SearchHandler(ctx, rc.SkillStore, rc.Bus, skilltools.SearchArgs{
				Query:      args.Query,
				Limit:      args.Limit,
				Capability: runCapability(ctx, rc),
			})
		},
		tools.WithDescription("Search the skill catalog by capability text. Returns ranked, capability-filtered, redacted skill candidates with name, title, trigger, and score."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery", "skills"),
		tools.WithBus(rc.Bus),
		tools.WithSource(tools.ToolSourceID("skills/tools")),
	)
}
