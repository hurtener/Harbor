package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerSkillGet(cat tools.ToolCatalog, store skills.SkillStore) error {
	return inproc.RegisterFunc[SkillGetArgs, SkillGetOut](
		cat, "skill_get",
		func(ctx context.Context, args SkillGetArgs) (SkillGetOut, error) {
			return skillGet(ctx, store, args)
		},
		tools.WithDescription("Fetch the full body of a named skill playbook."),
		tools.WithSideEffect(tools.SideEffectPure),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery"),
	)
}

type SkillGetArgs struct {
	Name string `json:"name"`
}

type SkillGetOut struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Found bool   `json:"found"`
	Error string `json:"error,omitempty"`
}

func skillGet(ctx context.Context, store skills.SkillStore, args SkillGetArgs) (SkillGetOut, error) {
	if store == nil {
		return SkillGetOut{}, fmt.Errorf("skill_get: backing SkillStore is nil — `skill_get` was registered without skills.SkillStore deps (operator misconfiguration)")
	}
	id, err := requireIdentity(ctx)
	if err != nil {
		return SkillGetOut{}, err
	}
	s, err := store.Get(ctx, id, args.Name)
	if err != nil {
		return SkillGetOut{Name: args.Name, Found: false, Error: err.Error()}, nil
	}
	return SkillGetOut{
		Name:  s.Name,
		Title: s.Title,
		Body:  strings.Join(s.Steps, "\n"),
		Found: true,
	}, nil
}
