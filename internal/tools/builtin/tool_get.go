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
		tools.WithDescription("Fetch the full description + args schema for a named tool."),
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
	Found       bool   `json:"found"`
	Error       string `json:"error,omitempty"`
}

func toolGet(ctx context.Context, cat tools.ToolCatalog, args ToolGetArgs) (ToolGetOut, error) {
	return toolGetWithScopes(ctx, cat, args, nil)
}

func toolGetWithScopes(ctx context.Context, cat tools.ToolCatalog, args ToolGetArgs, grantedScopes []string) (ToolGetOut, error) {
	q, err := requireIdentity(ctx)
	if err != nil {
		return ToolGetOut{}, err
	}
	catalogName, ok := modelToolNames(cat, q, grantedScopes).ResolveDeclared(args.Name)
	if !ok {
		return ToolGetOut{Name: args.Name, Found: false, Error: fmt.Sprintf("tool %q not found", args.Name)}, nil
	}
	d, ok := cat.Resolve(catalogName)
	if !ok {
		return ToolGetOut{Name: args.Name, Found: false, Error: fmt.Sprintf("tool %q not found", args.Name)}, nil
	}
	return ToolGetOut{
		Name:        args.Name,
		Description: d.Tool.Description,
		ArgsSchema:  string(d.Tool.ArgsSchema),
		Found:       true,
	}, nil
}
