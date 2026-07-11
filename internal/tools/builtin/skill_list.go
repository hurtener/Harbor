package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// SkillListArgs is the LLM-facing input shape for the `skill_list`
// built-in (— the skills-tools third tool's first
// production registration). The `capability` field is omitted and
// server-computed (see skill_capability.go).
type SkillListArgs struct {
	Scope    skills.Scope `json:"scope,omitempty"`
	TaskType string       `json:"task_type,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Limit    int          `json:"limit,omitempty"`
	Offset   int          `json:"offset,omitempty"`
}

// registerSkillList installs the `skill_list` built-in as a thin
// delegation to the skills-tools handler (`skilltools.ListHandler`):
// paged enumeration, capability-filtered + redacted, summary-only
// (full content stays behind `skill_get`).
func registerSkillList(rc RegistryContext) error {
	if err := requireSkillDeps("skill_list", rc); err != nil {
		return err
	}
	return inproc.RegisterFunc[SkillListArgs, skilltools.ListResult](
		rc.Catalog, skilltools.ToolNameSkillList,
		func(ctx context.Context, args SkillListArgs) (skilltools.ListResult, error) {
			if rc.SkillStore == nil {
				return skilltools.ListResult{}, fmt.Errorf("skill_list: backing SkillStore is nil — `skill_list` was registered without skills.SkillStore deps (operator misconfiguration)")
			}
			return skilltools.ListHandler(ctx, rc.SkillStore, rc.Bus, skilltools.ListArgs{
				Scope:      args.Scope,
				TaskType:   args.TaskType,
				Tags:       args.Tags,
				Limit:      args.Limit,
				Offset:     args.Offset,
				Capability: runCapability(ctx, rc),
			})
		},
		tools.WithDescription("List skills in the catalog, paged and filterable by scope / task_type / tags. Capability-filtered + redacted; summary-only (call skill_get for full content)."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery", "skills"),
		tools.WithSource(tools.ToolSourceID("skills/tools")),
	)
}
