package mcp

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/artifactcontent"
)

// materializeValue applies Harbor's transport-neutral binary-content seam to
// one lowered MCP result. The working identity is the storage scope; RunID is
// carried only as ArtifactScope.TaskID provenance. This helper is called
// before planner observation or MCP App context capture, so neither path can
// retain raw ImageContent, AudioContent, or embedded-resource bytes.
func (p *Provider) materializeValue(ctx context.Context, value MCPToolValue, producer string) (MCPToolValue, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return MCPToolValue{}, fmt.Errorf("mcp: materialize binary content: %w", identity.ErrIdentityMissing)
	}
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
	}
	if quad, ok := identity.QuadrupleFrom(ctx); ok {
		scope.TaskID = quad.RunID
	}
	projected, err := artifactcontent.Materialize(ctx, p.cfg.ArtifactStore, scope, value, producer)
	if err != nil {
		return MCPToolValue{}, fmt.Errorf("mcp: materialize binary content: %w", err)
	}
	out, ok := projected.(MCPToolValue)
	if !ok {
		return MCPToolValue{}, fmt.Errorf("mcp: materialize binary content returned %T, want MCPToolValue", projected)
	}
	return out, nil
}

func (p *Provider) contentProducer(kind, name string) string {
	return fmt.Sprintf("mcp:%s:%s:%s", p.source, kind, name)
}
