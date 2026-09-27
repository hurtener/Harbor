package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestRuntimeInfoEffectiveHookRequiresAdminAndExactAgentReach(t *testing.T) {
	surface := newPostureFixture(t, func(d *protocol.PostureDeps) {
		d.AgentResolver = fixtureAgentResolver{}
		d.AgentReach = auth.NewAgentReachAuthorizer()
		d.EffectiveRunCompletion = func(_ context.Context, agent string, id identity.Identity) (types.EffectiveRunCompletionHook, error) {
			if id.TenantID != "tenant-a" || agent != "agent-a" {
				t.Fatal("wrong effective projection identity")
			}
			return types.EffectiveRunCompletionHook{AgentID: agent, State: "active", Tool: "memory_ingest_run", TimeoutMS: 5000}, nil
		}
	})
	req := validRequest()
	req.EffectiveAgentID = "agent-a"
	if _, err := surface.Dispatch(context.Background(), methods.MethodRuntimeInfo, req); err == nil {
		t.Fatal("unauthenticated effective hook request accepted")
	}
	admin := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	if _, err := surface.Dispatch(admin, methods.MethodRuntimeInfo, req); err == nil {
		t.Fatal("admin without agent reach accepted")
	}
	allowed := auth.WithAgentReach(admin, []string{"agent-a"})
	out, err := surface.Dispatch(allowed, methods.MethodRuntimeInfo, req)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := out.(*types.RuntimeInfo)
	if !ok || info.EffectiveRunCompletion == nil || info.EffectiveRunCompletion.AgentID != "agent-a" || info.EffectiveRunCompletion.Tool != "memory_ingest_run" {
		t.Fatalf("wrong effective hook projection: %#v", out)
	}
	req.EffectiveAgentID = "agent-b"
	if _, err := surface.Dispatch(allowed, methods.MethodRuntimeInfo, req); err == nil {
		t.Fatal("cross-agent posture accepted")
	}
	ordinary := validRequest()
	out, err = surface.Dispatch(context.Background(), methods.MethodRuntimeInfo, ordinary)
	if err != nil || out.(*types.RuntimeInfo).EffectiveRunCompletion != nil {
		t.Fatal("ordinary runtime info inherited agent-specific data", err)
	}
}
