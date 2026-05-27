package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerSkillSearch(cat tools.ToolCatalog, store skills.SkillStore) error {
	return inproc.RegisterFunc[SkillSearchArgs, SkillSearchOut](
		cat, "skill_search",
		func(ctx context.Context, args SkillSearchArgs) (SkillSearchOut, error) {
			return skillSearch(ctx, store, args)
		},
		tools.WithDescription("Search the skill catalog by capability text. Returns matching skill names + titles + descriptions."),
		tools.WithSideEffect(tools.SideEffectPure),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery"),
	)
}

type SkillSearchArgs struct {
	Query string   `json:"query"`
	Tags  []string `json:"tags,omitempty"`
	Limit int      `json:"limit,omitempty"`
}

type SkillSearchOut struct {
	Skills []SkillSearchResult `json:"skills"`
	Count  int                 `json:"count"`
}

type SkillSearchResult struct {
	Name        string  `json:"name"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Score       float64 `json:"score,omitempty"`
}

func skillSearch(ctx context.Context, store skills.SkillStore, args SkillSearchArgs) (SkillSearchOut, error) {
	if store == nil {
		return SkillSearchOut{}, fmt.Errorf("skill_search: backing SkillStore is nil — `skill_search` was registered without skills.SkillStore deps (operator misconfiguration)")
	}
	if args.Limit <= 0 {
		args.Limit = 10
	} else if args.Limit > 50 {
		args.Limit = 50
	}
	id, err := requireIdentity(ctx)
	if err != nil {
		return SkillSearchOut{}, err
	}
	results, err := store.Search(ctx, id, args.Query, args.Limit)
	if err != nil {
		return SkillSearchOut{}, err
	}
	// Phase 107c / D-167 — tag filter applied client-side (intersection
	// semantics; SkillStore.Search does not accept tags today). Empty
	// tags slice passes every row through unchanged.
	out := SkillSearchOut{Skills: make([]SkillSearchResult, 0, len(results)), Count: 0}
	for _, r := range results {
		if !skillHasAllTags(r.Skill.Tags, args.Tags) {
			continue
		}
		out.Skills = append(out.Skills, SkillSearchResult{
			Name:        r.Skill.Name,
			Title:       r.Skill.Title,
			Description: r.Skill.Description,
			Score:       r.Score,
		})
	}
	out.Count = len(out.Skills)
	return out, nil
}

func skillHasAllTags(skillTags, filterTags []string) bool {
	if len(filterTags) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(skillTags))
	for _, t := range skillTags {
		set[t] = struct{}{}
	}
	for _, f := range filterTags {
		if _, ok := set[f]; !ok {
			return false
		}
	}
	return true
}
