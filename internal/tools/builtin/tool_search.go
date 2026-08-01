package builtin

import (
	"context"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerToolSearch(rc RegistryContext) error {
	grantedScopes := append([]string(nil), rc.GrantedScopes...)
	return inproc.RegisterFunc[ToolSearchArgs, ToolSearchOut](
		rc.Catalog, "tool_search",
		func(ctx context.Context, args ToolSearchArgs) (ToolSearchOut, error) {
			return toolSearchWithScopes(ctx, rc.Catalog, args, grantedScopes)
		},
		tools.WithDescription("Search the tool catalog by capability text + optional tag filter. Returns matching tool names + descriptions."),
		tools.WithSideEffect(tools.SideEffectPure),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery"),
	)
}

type ToolSearchArgs struct {
	Query string   `json:"query"`
	Tags  []string `json:"tags,omitempty"`
	Limit int      `json:"limit,omitempty"`
}

type ToolSearchOut struct {
	Tools []ToolSearchResult `json:"tools"`
	Count int                `json:"count"`
}

type ToolSearchResult struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

func toolSearch(ctx context.Context, cat tools.ToolCatalog, args ToolSearchArgs) (ToolSearchOut, error) {
	return toolSearchWithScopes(ctx, cat, args, nil)
}

func toolSearchWithScopes(ctx context.Context, cat tools.ToolCatalog, args ToolSearchArgs, grantedScopes []string) (ToolSearchOut, error) {
	q, err := requireIdentity(ctx)
	if err != nil {
		return ToolSearchOut{}, err
	}
	if args.Limit <= 0 {
		args.Limit = 10
	} else if args.Limit > 50 {
		args.Limit = 50
	}
	results := cat.Search(ctx, args.Query, args.Tags, args.Limit)
	projection := modelToolNames(cat, q, grantedScopes)
	out := ToolSearchOut{Tools: make([]ToolSearchResult, 0, len(results))}
	for _, t := range results {
		declaredName, kept := projection.DeclaredName(t.Name)
		if !kept {
			// Search indexes can lag catalog removal and can return a
			// residual-collision loser. Neither is callable in the model's
			// declared namespace, so teaching that raw key would create a
			// second, false vocabulary.
			continue
		}
		// `tags` is a required array in the tool_search output schema, so it
		// must serialize as `[]`, never `null`. A tool with no tags — an MCP
		// tool discovered without any (its descriptor carries `tags: null`) —
		// has a nil slice, which JSON-marshals to null and fails the schema.
		// Normalize to an empty slice at the emit boundary.
		tags := t.Tags
		if tags == nil {
			tags = []string{}
		}
		out.Tools = append(out.Tools, ToolSearchResult{
			Name:        declaredName,
			Description: t.Description,
			Tags:        tags,
		})
	}
	out.Count = len(out.Tools)
	return out, nil
}
