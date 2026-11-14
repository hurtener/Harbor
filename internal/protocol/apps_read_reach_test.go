package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
)

// readReachCtx is a verified bearer that may READ agent-read but select none.
func readReachCtx(t *testing.T) context.Context {
	t.Helper()
	return auth.WithAgentReadReach(auth.WithAgentReach(verifiedCtx(t), nil), []string{"agent-read"})
}

type admissionRecordingReader struct {
	stubResourceReader
	readAdmitted bool
}

func (r *admissionRecordingReader) ReadResource(ctx context.Context, serverID, uri string) (protocol.MCPResourceContent, error) {
	agent, _ := tools.EffectiveAgentConfigFrom(ctx)
	r.readAdmitted = auth.AuthorizeAdmittedAgentRead(ctx, agent) == nil
	return r.stubResourceReader.ReadResource(ctx, serverID, uri)
}

// TestAppsSurface_ReadReachReadsResourceButNeverCallsTools pins D-510: a
// bearer whose agent_read_reach names the agent may read its App resource
// (the read admission rides the context so the source authorizer can honour
// it), but may not call the App's tools and may not read an agent outside
// both reach sets.
func TestAppsSurface_ReadReachReadsResourceButNeverCallsTools(t *testing.T) {
	reader := &admissionRecordingReader{}
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: reader, Invoker: inv, ToolContext: &stubToolContextReader{},
		AgentResolver: &stubAppsAgentResolver{}, AgentReach: auth.NewAgentReachAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	ctx := readReachCtx(t)
	if _, err := s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: "srv-a", ResourceURI: "ui://x",
	}); err != nil {
		t.Fatalf("read-reach resource read: %v", err)
	}
	if reader.gotAgent != "agent-read" || !reader.readAdmitted {
		t.Fatalf("reader saw agent %q readAdmitted=%v, want the read-admitted agent", reader.gotAgent, reader.readAdmitted)
	}
	_, err = s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), AgentID: "agent-other", ServerID: "srv-a", ResourceURI: "ui://x",
	})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
	_, err = s.Dispatch(ctx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: "srv-a", Tool: "srv-a_cb",
	})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
	if inv.gotTool != "" {
		t.Fatalf("read-reach bearer invoked App tool %q", inv.gotTool)
	}
}

// TestAppsSurface_SelectingReachCarriesNoReadAdmission keeps the ordinary
// path unchanged: an agent_reach bearer is admitted without the read marker.
func TestAppsSurface_SelectingReachCarriesNoReadAdmission(t *testing.T) {
	reader := &admissionRecordingReader{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: reader, Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{},
		AgentResolver: &stubAppsAgentResolver{}, AgentReach: auth.NewAgentReachAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	ctx := auth.WithAgentReadReach(auth.WithAgentReach(verifiedCtx(t), []string{"agent-read"}), []string{"agent-read"})
	if _, err := s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: "srv-a", ResourceURI: "ui://x",
	}); err != nil {
		t.Fatalf("selecting read: %v", err)
	}
	if reader.readAdmitted {
		t.Fatal("selecting bearer was marked read-admitted")
	}
}

var readReachIdentity = identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}

// TestAdmitEffectiveAgentForRead_StartStillRequiresAgentReach: the read helper is not used by
// start, so agent_read_reach alone never opens a run.
func TestAdmitEffectiveAgentForRead_StartStillRequiresAgentReach(t *testing.T) {
	ctx := readReachCtx(t)
	resolver := &stubAppsAgentResolver{}
	reach := auth.NewAgentReachAuthorizer()
	if _, err := protocol.AdmitEffectiveAgent(ctx, string(methods.MethodStart), readReachIdentity, "agent-read", resolver, reach); err == nil {
		t.Fatal("agent_read_reach admitted an agent-selecting method")
	}
	admitted, id, err := protocol.AdmitEffectiveAgentForRead(ctx, string(methods.MethodToolsDescribe), readReachIdentity, "agent-read", resolver, reach)
	if err != nil || id != "agent-read" || auth.AuthorizeAdmittedAgentRead(admitted, "agent-read") != nil {
		t.Fatalf("read admission = (%q, %v)", id, err)
	}
	if auth.AuthorizeAdmittedAgentRead(admitted, "agent-other") == nil {
		t.Fatal("read admission for one agent authorized another")
	}
	if _, _, err := protocol.AdmitEffectiveAgentForRead(ctx, "m", readReachIdentity, "agent-read", resolver, nil); err == nil {
		t.Fatal("unwired reach gate admitted a read")
	}
}

func TestMCPSurface_Resources_AdmitsReadReach(t *testing.T) {
	mcp := &stubMCP{}
	s, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP: mcp, OAuth: &stubOAuth{}, Redactor: patterns.New(), Bus: newMCPBus(t),
		AgentResolver: &stubAppsAgentResolver{}, AgentReach: auth.NewAgentReachAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	ctx := readReachCtx(t)
	if _, err := s.Dispatch(ctx, methods.MethodMCPServersResources,
		&types.MCPServerResourcesRequest{Identity: validScope(), AgentID: "agent-read", Name: "github-server"}); err != nil {
		t.Fatalf("read-reach resources: %v", err)
	}
	if mcp.gotAgent != "agent-read" || mcp.resourcesCalls != 1 {
		t.Fatalf("resources path = agent %q calls %d", mcp.gotAgent, mcp.resourcesCalls)
	}
	_, err = s.Dispatch(ctx, methods.MethodMCPServersResources,
		&types.MCPServerResourcesRequest{Identity: validScope(), AgentID: "agent-other", Name: "github-server"})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
}
