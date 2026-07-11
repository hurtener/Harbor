package builtin

import (
	"context"
	"fmt"

	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// SkillGetArgs is the LLM-facing input shape for the `skill_get`
// built-in. The pre-111d single-`name` shape is
// replaced by the Phase-38 handler's multi-name + budget envelope —
// the `capability` field is omitted and server-computed (see
// skill_capability.go).
type SkillGetArgs struct {
	// Names are the skill names to fetch. Missing names are skipped
	// (a partial response beats a hard error for stale model state).
	Names []string `json:"names"`
	// MaxTokens caps the combined estimated token count of the
	// returned skills via the tiered budgeter ladder. 0 → the
	// handler default (1024).
	MaxTokens int `json:"max_tokens,omitempty"`
}

// registerSkillGet installs the `skill_get` built-in as a thin
// delegation to the Phase-38 handler (`skilltools.GetHandler`):
// capability filter + redaction + the tiered token budgeter run on
// every production call. The pre-111d package-local fetch/projection
// body is deleted — `internal/skills/tools` is the single
// implementation home.
func registerSkillGet(rc RegistryContext) error {
	if err := requireSkillDeps("skill_get", rc); err != nil {
		return err
	}
	return inproc.RegisterFunc[SkillGetArgs, skilltools.GetResult](
		rc.Catalog, skilltools.ToolNameSkillGet,
		func(ctx context.Context, args SkillGetArgs) (skilltools.GetResult, error) {
			if rc.SkillStore == nil {
				return skilltools.GetResult{}, fmt.Errorf("skill_get: backing SkillStore is nil — `skill_get` was registered without skills.SkillStore deps (operator misconfiguration)")
			}
			return skilltools.GetHandler(ctx, rc.SkillStore, rc.Bus, skilltools.GetArgs{
				Names:      args.Names,
				MaxTokens:  args.MaxTokens,
				Capability: runCapability(ctx, rc),
			})
		},
		tools.WithDescription("Fetch the full content of one or more named skill playbooks. Results are capability-filtered, redacted, and budget-fit to max_tokens via the tiered ladder (full → drop optional → cap steps to 3)."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery", "skills"),
		tools.WithSource(tools.ToolSourceID("skills/tools")),
	)
}
