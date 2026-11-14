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

// sessionAppSpy is a SessionAppChecker over a fixed set of session Apps.
type sessionAppSpy struct {
	apps  map[[3]string]bool
	calls int
}

func (s *sessionAppSpy) SessionHasApp(_ context.Context, id identity.Identity, agentID, serverID, resourceURI string) (bool, error) {
	s.calls++
	if id != readReachIdentity {
		return false, nil
	}
	return s.apps[[3]string{agentID, serverID, resourceURI}], nil
}

// TestAppsSurface_ReadReachAppCallNeedsRenderAdmissionAndSessionApp pins the
// D-510 App callback rule for a read-reach bearer: a render-admission-backed
// call to an App a turn of the caller's own session recorded for that agent
// is invoked; the same call without the session App, without the render
// admission, or with the proof unwired is refused before invocation.
func TestAppsSurface_ReadReachAppCallNeedsRenderAdmissionAndSessionApp(t *testing.T) {
	const server, uri = "srv", "ui://app/main.html"
	build := func(apps protocol.SessionAppChecker) (*protocol.AppsSurface, *stubInvoker) {
		inv := &stubInvoker{res: protocol.MCPAppToolResultRow{Tool: "srv_cb"}}
		s, err := protocol.NewAppsSurface(protocol.AppsDeps{
			Resource:                 &stubResourceReader{content: protocol.MCPResourceContent{ResourceURI: uri, MIMEType: "text/html", Inline: []byte("<html>x</html>")}},
			Invoker:                  inv,
			ToolContext:              &stubToolContextReader{},
			AgentResolver:            &stubAppsAgentResolver{},
			AgentReach:               auth.NewAgentReachAuthorizer(),
			RenderAdmissionAuthority: newFakeAdmissionAuthority(t),
			RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
			SessionApps:              apps,
		})
		if err != nil {
			t.Fatalf("NewAppsSurface: %v", err)
		}
		return s, inv
	}
	ctx := auth.WithAgentReadSessionApps(readReachCtx(t), true)
	mintAdmission := func(s *protocol.AppsSurface) string {
		resp, err := s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
			Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: uri, RequestRenderAdmission: true,
		})
		if err != nil {
			t.Fatalf("read-reach admission read: %v", err)
		}
		adm := resp.(*types.ReadMCPResourceResponse).RenderAdmission
		if adm == nil || adm.Token == "" {
			t.Fatal("no render admission minted")
		}
		return adm.Token
	}
	call := func(s *protocol.AppsSurface, token string) error {
		_, err := s.Dispatch(ctx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
			Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: uri, RenderAdmission: token, Tool: "srv_cb",
		})
		return err
	}

	proof := &sessionAppSpy{apps: map[[3]string]bool{{"agent-read", server, uri}: true}}
	s, inv := build(proof)
	if err := call(s, mintAdmission(s)); err != nil {
		t.Fatalf("own-session App callback: %v", err)
	}
	if inv.admittedCalls != 1 || inv.gotAgent != "agent-read" {
		t.Fatalf("admitted invocations = %d agent %q", inv.admittedCalls, inv.gotAgent)
	}
	if auth.AuthorizeAdmittedAgentRead(inv.admittedCtx, "agent-read") != nil {
		t.Fatal("invocation context lacks the read admission the source authorizer needs")
	}

	other := &sessionAppSpy{apps: map[[3]string]bool{{"agent-read", server, "ui://app/other.html"}: true}}
	s, inv = build(other)
	assertCode(t, call(s, mintAdmission(s)), protoerrors.CodeScopeMismatch)
	if inv.admittedCalls != 0 {
		t.Fatal("App outside the session was invoked")
	}

	s, inv = build(proof)
	_, err := s.Dispatch(ctx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: server, Tool: "srv_cb",
	})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
	if inv.gotTool != "" {
		t.Fatal("call without render admission was invoked")
	}

	// Without the signed agent_read_session_apps opt-in (a history-only
	// bearer, e.g. for an agent removed from the person's reach) the same
	// call is refused even with the session proof.
	s, inv = build(proof)
	token := mintAdmission(s)
	_, err = s.Dispatch(readReachCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: uri, RenderAdmission: token, Tool: "srv_cb",
	})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
	if inv.admittedCalls != 0 {
		t.Fatal("history-only read bearer invoked an App tool")
	}

	s, inv = build(nil)
	assertCode(t, call(s, mintAdmission(s)), protoerrors.CodeScopeMismatch)
	if inv.admittedCalls != 0 {
		t.Fatal("unwired session proof admitted a read-reach callback")
	}
}

type recordingSessionApps struct {
	sessionAppSpy
	recorded [][3]string
}

func (r *recordingSessionApps) RecordSessionApp(ctx context.Context, agentID, serverID, resourceURI string) error {
	if _, ok := identity.From(ctx); !ok {
		return protocol.ErrAccessorNotFound
	}
	r.recorded = append(r.recorded, [3]string{agentID, serverID, resourceURI})
	if r.apps == nil {
		r.apps = map[[3]string]bool{}
	}
	r.apps[[3]string{agentID, serverID, resourceURI}] = true
	return nil
}

// TestAppsSurface_NestedAppJoinsTheSessionLedger pins D-510 nested Apps: an
// App returned by an admitted callback is recorded for the same session and
// effective agent before the reply leaves, so its own buttons then pass.
func TestAppsSurface_NestedAppJoinsTheSessionLedger(t *testing.T) {
	const server, uri, nested = "srv", "ui://app/main.html", "ui://app/detail.html"
	ledger := &recordingSessionApps{sessionAppSpy: sessionAppSpy{apps: map[[3]string]bool{{"agent-read", server, uri}: true}}}
	inv := &stubInvoker{res: protocol.MCPAppToolResultRow{Tool: "srv_open", App: &protocol.MCPAppRefRow{ServerID: server, ResourceURI: nested}}}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{content: protocol.MCPResourceContent{ResourceURI: uri, MIMEType: "text/html", Inline: []byte("<html>x</html>")}},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               auth.NewAgentReachAuthorizer(),
		RenderAdmissionAuthority: newFakeAdmissionAuthority(t),
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
		SessionApps:              ledger,
		SessionAppRecorder:       ledger,
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	ctx := auth.WithAgentReadSessionApps(readReachCtx(t), true)
	admit := func(resource string) string {
		resp, err := s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
			Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: resource, RequestRenderAdmission: true,
		})
		if err != nil {
			t.Fatalf("read %s: %v", resource, err)
		}
		return resp.(*types.ReadMCPResourceResponse).RenderAdmission.Token
	}
	call := func(resource, token string) error {
		_, err := s.Dispatch(ctx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
			Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: resource, RenderAdmission: token, Tool: "srv_open",
		})
		return err
	}
	if err := call(uri, admit(uri)); err != nil {
		t.Fatalf("outer App callback: %v", err)
	}
	if len(ledger.recorded) != 1 || ledger.recorded[0] != [3]string{"agent-read", server, nested} {
		t.Fatalf("nested App ledger writes = %v", ledger.recorded)
	}
	if err := call(nested, admit(nested)); err != nil {
		t.Fatalf("nested App callback: %v", err)
	}
}

// TestAppsSurface_ParticipantUsesOnlyTheSharedSessionsApps pins D-510 for
// session participants: a participant reads and calls an App only when the
// shared session's ledger recorded it, and a call needs a render admission.
func TestAppsSurface_ParticipantUsesOnlyTheSharedSessionsApps(t *testing.T) {
	const server, uri = "srv", "ui://app/main.html"
	ledger := &sessionAppSpy{apps: map[[3]string]bool{{"agent-read", server, uri}: true}}
	inv := &stubInvoker{res: protocol.MCPAppToolResultRow{Tool: "srv_cb"}}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{content: protocol.MCPResourceContent{ResourceURI: uri, MIMEType: "text/html", Inline: []byte("<html>x</html>")}},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               auth.NewAgentReachAuthorizer(),
		RenderAdmissionAuthority: newFakeAdmissionAuthority(t),
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
		SessionApps:              ledger,
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	// The partition is the owner's (t-1/u-1/s-1); the participant is u-guest.
	ctx, err := identity.WithParticipant(auth.WithAgentReadSessionApps(readReachCtx(t), true), identity.Participant{UserID: "u-guest", Speaker: "Guest"})
	if err != nil {
		t.Fatal(err)
	}
	read := func(resource string) (*types.ReadMCPResourceResponse, error) {
		resp, err := s.Dispatch(ctx, methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
			Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: resource, RequestRenderAdmission: true,
		})
		if err != nil {
			return nil, err
		}
		return resp.(*types.ReadMCPResourceResponse), nil
	}
	if _, err := read("ui://app/other.html"); err == nil {
		t.Fatal("participant read an App the shared session never rendered")
	}
	out, err := read(uri)
	if err != nil {
		t.Fatalf("participant read of the session's App: %v", err)
	}
	if _, err := s.Dispatch(ctx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: server, ResourceURI: uri, RenderAdmission: out.RenderAdmission.Token, Tool: "srv_cb",
	}); err != nil {
		t.Fatalf("participant App callback: %v", err)
	}
	// Even with agent_reach, a participant call needs a render admission and
	// the session proof.
	selecting, err := identity.WithParticipant(auth.WithAgentReach(verifiedCtx(t), []string{"agent-read"}), identity.Participant{UserID: "u-guest"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Dispatch(selecting, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), AgentID: "agent-read", ServerID: server, Tool: "srv_cb",
	})
	assertCode(t, err, protoerrors.CodeScopeMismatch)
	if inv.admittedCalls != 1 {
		t.Fatalf("participant admitted calls = %d, want 1", inv.admittedCalls)
	}
}
