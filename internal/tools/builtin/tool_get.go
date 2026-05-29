package builtin

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func registerToolGet(cat tools.ToolCatalog) error {
	return inproc.RegisterFunc[ToolGetArgs, ToolGetOut](
		cat, "tool_get",
		func(ctx context.Context, args ToolGetArgs) (ToolGetOut, error) {
			return toolGet(ctx, cat, args)
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
	if _, err := requireIdentity(ctx); err != nil {
		return ToolGetOut{}, err
	}
	d, ok := cat.Resolve(args.Name)
	if !ok {
		return ToolGetOut{Name: args.Name, Found: false, Error: fmt.Sprintf("tool %q not found", args.Name)}, nil
	}
	return ToolGetOut{
		Name:        d.Tool.Name,
		Description: d.Tool.Description,
		ArgsSchema:  string(d.Tool.ArgsSchema),
		Found:       true,
	}, nil
}
