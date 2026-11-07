package assemble

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcpCatalogChangeHint adapts MCP registry structural changes into the
// best-effort tools.catalog.changed hint. Only owner-tagged registrations
// resolve to an agent; boot-declared servers (zero owner) have no
// (tenant, agent) and are skipped at Debug rather than published with an
// invalid identity. A user-owned registration reports scope "user".
func mcpCatalogChangeHint(bus events.EventBus, logger *slog.Logger) mcpdrv.RegistryChangeObserver {
	return func(ctx context.Context, _ string, owner toolauth.Owner, reason mcpdrv.RegistryChangeReason) {
		scope := agentcfg.CatalogChangeScopeAgent
		if owner.User != "" {
			scope = agentcfg.CatalogChangeScopeUser
		}
		hintReason := agentcfg.CatalogChangeReasonConnection
		if reason == mcpdrv.RegistryChangeDiscovery {
			hintReason = agentcfg.CatalogChangeReasonDiscovery
		}
		writer, _ := identity.QuadrupleFrom(ctx)
		ev, ok := agentcfg.CatalogChangedEvent(writer, owner.Tenant, owner.Agent, scope, hintReason, time.Now())
		if !ok {
			logger.DebugContext(ctx, "assemble: tools.catalog.changed skipped: registration has no agent owner",
				slog.String("reason", hintReason))
			return
		}
		if err := events.PublishAsync(context.WithoutCancel(ctx), bus, ev); err != nil {
			logger.DebugContext(ctx, "assemble: tools.catalog.changed not admitted",
				slog.String("reason", hintReason), slog.String("error", err.Error()))
		}
	}
}
