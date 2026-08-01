package builtin

import (
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// modelToolNames returns the declared-name projection for the caller's
// catalog. Both loading modes participate because tool_search is the path by
// which deferred tools become model-visible. Construction order is the
// catalog's deterministic List order, matching the ReAct catalog view.
func modelToolNames(cat tools.ToolCatalog, q identity.Quadruple, grantedScopes []string) tools.ModelToolNameProjection {
	base := tools.CatalogFilter{
		TenantID:      q.TenantID,
		UserID:        q.UserID,
		SessionID:     q.SessionID,
		GrantedScopes: append([]string(nil), grantedScopes...),
	}
	always := base
	always.LoadingModes = []tools.LoadingMode{tools.LoadingAlways}
	deferred := base
	deferred.LoadingModes = []tools.LoadingMode{tools.LoadingDeferred}
	visible := append(cat.List(always), cat.List(deferred)...)
	names := make([]string, 0, len(visible))
	for _, tool := range visible {
		names = append(names, tool.Name)
	}
	return tools.NewModelToolNameProjection(names, tools.ReservedModelToolNames())
}
