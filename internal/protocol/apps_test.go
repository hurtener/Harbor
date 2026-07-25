package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// stubResourceReader / stubInvoker are deterministic Apps seams.
type stubResourceReader struct {
	content protocol.MCPResourceContent
	err     error
	gotID   string
	gotURI  string
}

func (s *stubResourceReader) ReadResource(_ context.Context, serverID, uri string) (protocol.MCPResourceContent, error) {
	s.gotID, s.gotURI = serverID, uri
	if s.err != nil {
		return protocol.MCPResourceContent{}, s.err
	}
	return s.content, nil
}

type stubInvoker struct {
	res     protocol.MCPAppToolResultRow
	err     error
	gotTool string
}

func (s *stubInvoker) CallTool(_ context.Context, tool string, _ json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	s.gotTool = tool
	if s.err != nil {
		return protocol.MCPAppToolResultRow{}, s.err
	}
	return s.res, nil
}

// stubToolContextReader is a deterministic AppToolContextReader seam.
type stubToolContextReader struct {
	row         protocol.AppToolContextRow
	err         error
	gotServerID string
	gotCallID   string
}

func (s *stubToolContextReader) ToolContext(_ context.Context, serverID, toolCallID string) (protocol.AppToolContextRow, error) {
	s.gotServerID, s.gotCallID = serverID, toolCallID
	if s.err != nil {
		return protocol.AppToolContextRow{}, s.err
	}
	return s.row, nil
}

func newAppsSurface(t *testing.T, r protocol.MCPResourceReader, inv protocol.AppToolInvoker) *protocol.AppsSurface {
	t.Helper()
	return newAppsSurfaceTC(t, r, inv, &stubToolContextReader{})
}

func newAppsSurfaceTC(t *testing.T, r protocol.MCPResourceReader, inv protocol.AppToolInvoker, tc protocol.AppToolContextReader) *protocol.AppsSurface {
	t.Helper()
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: r, Invoker: inv, ToolContext: tc})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	return s
}

func TestAppsSurface_NewRejectsNilDeps(t *testing.T) {
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{}}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("nil Resource: err = %v, want ErrAppsMisconfigured", err)
	}
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: &stubResourceReader{}, ToolContext: &stubToolContextReader{}}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("nil Invoker: err = %v, want ErrAppsMisconfigured", err)
	}
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: &stubResourceReader{}, Invoker: &stubInvoker{}}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("nil ToolContext: err = %v, want ErrAppsMisconfigured", err)
	}
}

func TestAppsSurface_ReadResource_InlineProjection(t *testing.T) {
	rr := &stubResourceReader{content: protocol.MCPResourceContent{
		ResourceURI: "ui://app/main.html", MIMEType: "text/html", Inline: []byte("<html>x</html>"),
	}}
	s := newAppsSurface(t, rr, &stubInvoker{})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), ServerID: "srv-a", ResourceURI: "ui://app/main.html",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.Content != "<html>x</html>" || out.ArtifactRef != nil {
		t.Errorf("inline projection wrong: %+v", out)
	}
	if rr.gotID != "srv-a" || rr.gotURI != "ui://app/main.html" {
		t.Errorf("accessor args wrong: id=%q uri=%q", rr.gotID, rr.gotURI)
	}
}

func TestAppsSurface_ReadResource_ArtifactProjection(t *testing.T) {
	rr := &stubResourceReader{content: protocol.MCPResourceContent{
		ResourceURI: "ui://app/main.html",
		Artifact:    &protocol.MCPResourceArtifactRow{ID: "mcp-apps_abc", SizeBytes: 9001, SHA256: "deadbeef"},
	}}
	s := newAppsSurface(t, rr, &stubInvoker{})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), ServerID: "srv-a", ResourceURI: "ui://app/main.html",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.Content != "" || out.ArtifactRef == nil || out.ArtifactRef.ID != "mcp-apps_abc" {
		t.Errorf("artifact projection wrong: %+v", out)
	}
}

func TestAppsSurface_ReadResource_FailsClosedOnMissingIdentity(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: types.IdentityScope{Tenant: "t-1"}, // user + session missing
		ServerID: "srv-a", ResourceURI: "ui://x",
	})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

func TestAppsSurface_ReadResource_RequiresServerAndURI(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: validScope(), ResourceURI: "ui://x",
	})
	assertCode(t, err, protoerrors.CodeInvalidRequest)
}

func TestAppsSurface_CallTool_AppRefProjection(t *testing.T) {
	inv := &stubInvoker{res: protocol.MCPAppToolResultRow{
		Tool:   "srv-a_weather",
		Inline: json.RawMessage(`{"ok":true}`),
		App:    &protocol.MCPAppRefRow{ServerID: "srv-a", ResourceURI: "ui://weather/view.html", DisplayMode: "inline"},
	}}
	s := newAppsSurface(t, &stubResourceReader{}, inv)
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), Tool: "srv-a_weather", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out := resp.(*types.MCPAppCallToolResponse)
	if out.App == nil || out.App.ResourceURI != "ui://weather/view.html" || out.App.DisplayMode != "inline" {
		t.Errorf("app-ref projection wrong: %+v", out.App)
	}
	if out.App != nil && out.App.ServerID != "srv-a" {
		t.Errorf("app-ref server_id projection wrong: got %q, want srv-a", out.App.ServerID)
	}
	if string(out.Content) != `{"ok":true}` {
		t.Errorf("content wrong: %s", out.Content)
	}
	if inv.gotTool != "srv-a_weather" {
		t.Errorf("invoker got tool %q", inv.gotTool)
	}
}

// TestAppsSurface_CallTool_FailsClosedOnMissingIdentity — a dispatch
// whose context carries no established identity has nothing for the
// body-scope gate to reconcile against, so it is refused before the
// invoker is reached. The body's own triple never supplies the missing
// authority.
func TestAppsSurface_CallTool_FailsClosedOnMissingIdentity(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), Tool: "srv-a_x",
	})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

// TestAppsSurface_CallTool_EmptyBodyTripleBackfilled — an empty body
// triple is the caller saying "use my established identity"; the gate
// fills it in rather than refusing.
func TestAppsSurface_CallTool_EmptyBodyTripleBackfilled(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	req := &types.MCPAppCallToolRequest{Identity: types.IdentityScope{}, Tool: "srv-a_x"}
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, req); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if req.Identity != validScope() {
		t.Errorf("body triple = %+v, want the established identity %+v", req.Identity, validScope())
	}
}

func TestAppsSurface_Dispatch_RejectsNonAppsMethod(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodStart, &types.ReadMCPResourceRequest{Identity: validScope()})
	assertCode(t, err, protoerrors.CodeUnknownMethod)
}

func TestAppsSurface_ToolContext_InlineProjection(t *testing.T) {
	tc := &stubToolContextReader{row: protocol.AppToolContextRow{
		Tool:    "srv-a_weather",
		IsError: false,
		Input:   protocol.AppToolContextPayloadRow{Inline: json.RawMessage(`{"city":"NYC"}`)},
		Result:  protocol.AppToolContextPayloadRow{Inline: json.RawMessage(`{"temp":21}`)},
	}}
	s := newAppsSurfaceTC(t, &stubResourceReader{}, &stubInvoker{}, tc)
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ServerID: "srv-a", ToolCallID: "abc123",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out := resp.(*types.ToolContextResponse)
	if out.Tool != "srv-a_weather" || out.IsError {
		t.Errorf("tool-context projection wrong: %+v", out)
	}
	if string(out.Input.Content) != `{"city":"NYC"}` || out.Input.ArtifactRef != nil {
		t.Errorf("input projection wrong: %+v", out.Input)
	}
	if string(out.Result.Content) != `{"temp":21}` || out.Result.ArtifactRef != nil {
		t.Errorf("result projection wrong: %+v", out.Result)
	}
	if tc.gotServerID != "srv-a" || tc.gotCallID != "abc123" {
		t.Errorf("reader args wrong: server=%q call=%q", tc.gotServerID, tc.gotCallID)
	}
}

func TestAppsSurface_ToolContext_ArtifactProjection(t *testing.T) {
	tc := &stubToolContextReader{row: protocol.AppToolContextRow{
		Tool:   "srv-a_weather",
		Input:  protocol.AppToolContextPayloadRow{Inline: json.RawMessage("null")},
		Result: protocol.AppToolContextPayloadRow{Artifact: &protocol.MCPResourceArtifactRow{ID: "mcp-apps_xyz", SizeBytes: 99000}},
	}}
	s := newAppsSurfaceTC(t, &stubResourceReader{}, &stubInvoker{}, tc)
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ServerID: "srv-a", ToolCallID: "abc123",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out := resp.(*types.ToolContextResponse)
	if out.Result.ArtifactRef == nil || out.Result.ArtifactRef.ID != "mcp-apps_xyz" || out.Result.Content != nil {
		t.Errorf("result by-reference projection wrong: %+v", out.Result)
	}
}

// TestAppsSurface_ToolContext_FailsClosedOnMissingIdentity — see
// TestAppsSurface_CallTool_FailsClosedOnMissingIdentity.
func TestAppsSurface_ToolContext_FailsClosedOnMissingIdentity(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ServerID: "srv-a", ToolCallID: "abc",
	})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

func TestAppsSurface_ToolContext_RequiresServerAndCallID(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ToolCallID: "abc",
	}); true {
		assertCode(t, err, protoerrors.CodeInvalidRequest)
	}
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ServerID: "srv-a",
	}); true {
		assertCode(t, err, protoerrors.CodeInvalidRequest)
	}
}

func TestAppsSurface_ToolContext_UnknownIDMapsToNotFound(t *testing.T) {
	// The accessor states the verdict via the sentinel; the edge no longer
	// classifies on the message text (mcpconsole.ToolContext applies this
	// wrap in production).
	tc := &stubToolContextReader{err: fmt.Errorf("%w: mcpconsole: tool context not found (server %q, call %q): state: record not found",
		protocol.ErrAccessorNotFound, "srv-a", "nope")}
	s := newAppsSurfaceTC(t, &stubResourceReader{}, &stubInvoker{}, tc)
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsToolContext, &types.ToolContextRequest{
		Identity: validScope(), ServerID: "srv-a", ToolCallID: "nope",
	})
	assertCode(t, err, protoerrors.CodeNotFound)
}

// An app-initiated call naming a tool that does not resolve inside the calling
// app's own server namespace must surface as CodeNotFound — the outcome an app
// can act on — not as the undifferentiated CodeRuntimeError ("MCP read failed")
// it cannot tell apart from a broken southbound transport. The catalog's
// `tools: tool not found` wrap is the marker the Protocol edge classifies on.
func TestAppsSurface_CallTool_UnresolvableToolMapsToNotFound(t *testing.T) {
	inv := &stubInvoker{err: fmt.Errorf("%w: tools: tool not found: %q",
		protocol.ErrAccessorNotFound, "srv-a_nope")}
	s := newAppsSurface(t, &stubResourceReader{}, inv)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), Tool: "srv-a_nope",
	})
	assertCode(t, err, protoerrors.CodeNotFound)
}

func TestAppsSurface_CallTool_TransportFailureStaysRuntimeError(t *testing.T) {
	inv := &stubInvoker{err: errors.New("mcpconsole: stdio transport reset by peer")}
	s := newAppsSurface(t, &stubResourceReader{}, inv)
	_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: validScope(), Tool: "srv-a_echo",
	})
	assertCode(t, err, protoerrors.CodeRuntimeError)
}

// THE LAUNDERING GUARD. A southbound MCP server's error text is wrapped
// verbatim into the chain, so a REMOTE party gets to phrase part of what the
// Protocol edge reads. Classification must not be reachable from that text: a
// transport failure whose message happens to contain the words a not-found
// would use stays CodeRuntimeError. A rendered App treats not-found as a
// PERMANENT verdict ("this action does not exist here"), so laundering a
// transient failure into one is a wrong answer the App cannot recover from.
func TestAppsSurface_CallTool_RemoteErrorTextCannotLaunderIntoNotFound(t *testing.T) {
	for _, msg := range []string{
		`mcp: call "srv-a_echo": upstream said: tool not found in cache, retrying`,
		`mcp: transport failed: server not found upstream (503)`,
	} {
		inv := &stubInvoker{err: errors.New(msg)}
		s := newAppsSurface(t, &stubResourceReader{}, inv)
		_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
			Identity: validScope(), Tool: "srv-a_echo",
		})
		assertCode(t, err, protoerrors.CodeRuntimeError)
	}
}
