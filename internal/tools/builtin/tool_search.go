package builtin

import (
	"context"
	"strings"

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
		tools.WithDescription("Search the tools available to this run by capability text (tool name, description, or the providing server's name) + optional tag filter. Returns matching tool names, descriptions, and server labels."),
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
	// Server is the providing server's human-readable label, when the
	// provider supplies one. Informational only: Name is what to call.
	Server string `json:"server,omitempty"`
}

const (
	defaultToolSearchLimit = 10
	maxToolSearchLimit     = 50
)

// toolSearchWithScopes answers strictly from the calling run's tool view.
// The ranked index is process-wide, so it is queried with the run's
// visibility predicate (applied before the limit); results are then
// materialized from the run view itself, never from index rows. When the
// index yields nothing visible (no index, FTS miss, or substring-only
// match), a bounded in-memory substring pass over the run's own discovery
// set answers instead.
func toolSearchWithScopes(ctx context.Context, cat tools.ToolCatalog, args ToolSearchArgs, grantedScopes []string) (ToolSearchOut, error) {
	disc, err := discoveryForRun(ctx, grantedScopes)
	if err != nil {
		return ToolSearchOut{}, err
	}
	if args.Limit <= 0 {
		args.Limit = defaultToolSearchLimit
	} else if args.Limit > maxToolSearchLimit {
		args.Limit = maxToolSearchLimit
	}
	query := strings.TrimSpace(args.Query)

	var names []string
	if query != "" && cat != nil {
		hits := cat.Search(ctx, tools.ToolSearchRequest{
			Query: query,
			Tags:  args.Tags,
			Limit: args.Limit,
			Visible: func(name string) bool {
				_, ok := disc.visible[name]
				return ok
			},
		})
		for _, hit := range hits {
			names = append(names, hit.Name)
		}
	}
	if len(names) == 0 {
		names = disc.fallbackMatches(query, args.Tags, args.Limit)
	}

	out := ToolSearchOut{Tools: make([]ToolSearchResult, 0, len(names))}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if len(out.Tools) >= args.Limit {
			break
		}
		t, ok := disc.visible[name]
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		declaredName, kept := disc.projection.DeclaredName(name)
		if !kept {
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
			Server:      t.SourceTitle,
		})
	}
	out.Count = len(out.Tools)
	return out, nil
}

// fallbackMatches is the in-memory pass over the run's own discovery set
// (already bounded by the run's view). A tool matches when it carries every
// requested tag and every query token appears, case-insensitively, in its
// declared or physical name, description, tags, or server metadata. An empty
// query lists the run's tools in namespace order.
func (d runDiscovery) fallbackMatches(query string, tags []string, limit int) []string {
	tokens := strings.Fields(strings.ToLower(query))
	out := make([]string, 0, limit)
	for _, name := range d.order {
		if len(out) >= limit {
			break
		}
		t := d.visible[name]
		if !hasAllTags(t.Tags, tags) {
			continue
		}
		if len(tokens) > 0 {
			declared, _ := d.projection.DeclaredName(name)
			hay := strings.ToLower(strings.Join([]string{
				declared, t.Name, t.Description, strings.Join(t.Tags, " "), t.SourceTitle, t.SourceDescription,
			}, "\n"))
			matched := true
			for _, tok := range tokens {
				if !strings.Contains(hay, tok) {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

func hasAllTags(toolTags, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(toolTags))
	for _, t := range toolTags {
		set[strings.ToLower(t)] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[strings.ToLower(w)]; !ok {
			return false
		}
	}
	return true
}
