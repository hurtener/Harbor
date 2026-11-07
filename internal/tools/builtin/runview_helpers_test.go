package builtin

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// withRunView seats the plain identity-filtered planner view over cat — the
// view a run without agent-config exposure gets — as ctx's run tool view.
func withRunView(ctx context.Context, cat tools.ToolCatalog, grantedScopes ...string) context.Context {
	q, _ := identity.QuadrupleFrom(ctx)
	return tools.WithRunToolView(ctx, tools.NewPlannerView(cat, tools.CatalogFilter{
		TenantID:      q.TenantID,
		UserID:        q.UserID,
		SessionID:     q.SessionID,
		GrantedScopes: grantedScopes,
	}))
}

// ensureRunView seats the default view unless the test already chose one.
func ensureRunView(ctx context.Context, cat tools.ToolCatalog) context.Context {
	if _, ok := tools.RunToolView(ctx); ok {
		return ctx
	}
	return withRunView(ctx, cat)
}

func toolSearch(ctx context.Context, cat tools.ToolCatalog, args ToolSearchArgs) (ToolSearchOut, error) {
	return toolSearchWithScopes(ensureRunView(ctx, cat), cat, args, nil)
}

func toolGet(ctx context.Context, cat tools.ToolCatalog, args ToolGetArgs) (ToolGetOut, error) {
	return toolGetWithScopes(ensureRunView(ctx, cat), cat, args, nil)
}

func declarativeAction(ctx context.Context, cat tools.ToolCatalog, args DeclarativeActionArgs) (DeclarativeActionOut, error) {
	return declarativeActionWithScopes(ensureRunView(ctx, cat), cat, args, nil)
}
