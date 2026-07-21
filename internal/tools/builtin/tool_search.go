package builtin

import (
	"context"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerToolSearch(cat tools.ToolCatalog) error {
	return inproc.RegisterFunc[ToolSearchArgs, ToolSearchOut](
		cat, "tool_search",
		func(ctx context.Context, args ToolSearchArgs) (ToolSearchOut, error) {
			return toolSearch(ctx, cat, args)
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
	if _, err := requireIdentity(ctx); err != nil {
		return ToolSearchOut{}, err
	}
	if args.Limit <= 0 {
		args.Limit = 10
	} else if args.Limit > 50 {
		args.Limit = 50
	}
	results := cat.Search(ctx, args.Query, args.Tags, args.Limit)
	out := ToolSearchOut{Tools: make([]ToolSearchResult, 0, len(results)), Count: len(results)}
	for _, t := range results {
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
			Name:        t.Name,
			Description: t.Description,
			Tags:        tags,
		})
	}
	return out, nil
}
