package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerToolGet(rc RegistryContext) error {
	grantedScopes := append([]string(nil), rc.GrantedScopes...)
	return inproc.RegisterFunc[ToolGetArgs, ToolGetOut](
		rc.Catalog, "tool_get",
		func(ctx context.Context, args ToolGetArgs) (ToolGetOut, error) {
			return toolGetWithScopes(ctx, rc.Catalog, args, grantedScopes)
		},
		tools.WithDescription("Fetch the full description + args schema for a named tool available to this run."),
		tools.WithSideEffect(tools.SideEffectPure),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "discovery"),
	)
}

type ToolGetArgs struct {
	Name string `json:"name"`
}

type ToolGetOut struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ArgsSchema  string `json:"args_schema,omitempty"`
	// Server is the providing server's human-readable label, when known.
	Server string `json:"server,omitempty"`
	Found  bool   `json:"found"`
	Error  string `json:"error,omitempty"`
}

// toolGetWithScopes describes only a tool the calling run's view admits. The
// name may be a declared name or any unambiguous exact reference
// tool_search resolves (see runDiscovery.exactMatches); the result names the
// tool by its callable declared name. A
// name outside the view — another principal's tool, an excluded or
// non-allowlisted tool — is indistinguishable from an unknown name.
func toolGetWithScopes(ctx context.Context, _ tools.ToolCatalog, args ToolGetArgs, grantedScopes []string) (ToolGetOut, error) {
	disc, err := discoveryForRun(ctx, grantedScopes)
	if err != nil {
		return ToolGetOut{}, err
	}
	t, ok := disc.resolveExact(args.Name)
	if !ok {
		return ToolGetOut{Name: args.Name, Found: false, Error: fmt.Sprintf("tool %q not found", args.Name)}, nil
	}
	// Echo the callable declared name, never the physical catalog key.
	declared, _ := disc.projection.DeclaredName(t.Name)
	return ToolGetOut{
		Name:        declared,
		Description: t.Description,
		ArgsSchema:  string(t.ArgsSchema),
		Server:      t.SourceTitle,
		Found:       true,
	}, nil
}
