package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestBuildMux_AgentPacksSurfaceIsMounted(t *testing.T) {
	deps := buildProjWiringMux(t)
	registry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{
		State: deps.in.State,
		Bus:   deps.in.Bus,
	})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })

	in := deps.in
	in.AgentConfig = registry
	in.AgentConfigID = "agent-a"
	in.AgentResolver = NewAgentResolverAdapter(registry, in.AgentConfigID)
	in.AgentReach = protocolauth.NewAgentReachAuthorizer()
	built, err := BuildMux(in)
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	ctx = protocolauth.WithScopes(ctx, []protocolauth.Scope{protocolauth.ScopeAdmin})
	ctx = protocolauth.WithAgentReach(ctx, []string{"agent-a"})
	status, body := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/inspect", id,
		`{"identity":{"tenant":"tenant-a","user":"operator","session":"session-a"},"agent_id":"agent-a"}`, ctx)
	if status != http.StatusOK {
		t.Fatalf("agent-pack inspect status = %d, body = %s", status, body)
	}
	var response prototypes.AgentConfigAgentPacksInspectResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode inspect response: %v; body=%s", err, body)
	}
	if response.AgentID != "agent-a" || len(response.CompositionHash) != 64 || len(response.BootPackSetHash) != 64 {
		t.Fatalf("inspect response = %+v", response)
	}
}
