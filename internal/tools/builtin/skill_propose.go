package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/skills/generator"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// registerSkillPropose installs the `skill_propose` built-in as a
// thin delegation to the Phase-41 generator (`generator.Propose`) —
// its first production registration (Phase 111d / D-201).
//
// D-054 semantics ride unchanged through this carrier: draft
// validation, Origin=Generated + OriginRef stamping, the conflict
// policy (PackImport-protected; Generated→Generated content-hash-
// gated LWW), the MANDATORY redacted `skill.proposed` audit emit
// with persist rollback on emit failure.
//
// Registration requires Bus AND Redactor on the RegistryContext —
// both are wiring-shaped deps (a generator without the audit emit
// path is a §13 violation), so a nil value fails the boot loudly.
// The tool itself registers ONLY when the operator lists
// `skill_propose` in `tools.built_in`; it appears in no recommended
// default set (persistence-capable skill authoring is a deliberate
// operator opt-in).
func registerSkillPropose(rc RegistryContext) error {
	if err := requireSkillDeps("skill_propose", rc); err != nil {
		return err
	}
	if rc.Redactor == nil {
		return fmt.Errorf("skill_propose: RegistryContext.Redactor is required (audit.Redactor) — the generator's mandatory skill.proposed audit emit redacts through it")
	}
	return inproc.RegisterFunc[generator.ProposeArgs, generator.SkillReceipt](
		rc.Catalog, generator.ToolNameSkillPropose,
		func(ctx context.Context, args generator.ProposeArgs) (generator.SkillReceipt, error) {
			if rc.SkillStore == nil {
				return generator.SkillReceipt{}, fmt.Errorf("skill_propose: backing SkillStore is nil — `skill_propose` was registered without skills.SkillStore deps (operator misconfiguration)")
			}
			return generator.Propose(ctx, rc.SkillStore, generator.Deps{
				Bus:      rc.Bus,
				Redactor: rc.Redactor,
			}, args)
		},
		tools.WithDescription("Validate an LLM-drafted skill and, when persist=true, persist it with provenance stamping (Origin=Generated), the settled conflict policy (pack-protected; Generated→Generated content-hash-gated last-write-wins), and a mandatory redacted skill.proposed audit event. Returns a receipt with the result branch (validated/persisted/idempotent/rejected)."),
		tools.WithSideEffect(tools.SideEffectWrite),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "skills", "generate"),
		tools.WithBus(rc.Bus),
		tools.WithSource(tools.ToolSourceID("skills/generator")),
	)
}
