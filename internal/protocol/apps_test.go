package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
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

func newAppsSurface(t *testing.T, r protocol.MCPResourceReader, inv protocol.AppToolInvoker) *protocol.AppsSurface {
	t.Helper()
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: r, Invoker: inv})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	return s
}

func TestAppsSurface_NewRejectsNilDeps(t *testing.T) {
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{Invoker: &stubInvoker{}}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("nil Resource: err = %v, want ErrAppsMisconfigured", err)
	}
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: &stubResourceReader{}}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("nil Invoker: err = %v, want ErrAppsMisconfigured", err)
	}
}

func TestAppsSurface_ReadResource_InlineProjection(t *testing.T) {
	rr := &stubResourceReader{content: protocol.MCPResourceContent{
		ResourceURI: "ui://app/main.html", MIMEType: "text/html", Inline: []byte("<html>x</html>"),
	}}
	s := newAppsSurface(t, rr, &stubInvoker{})
	resp, err := s.Dispatch(context.Background(), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
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
	resp, err := s.Dispatch(context.Background(), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
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
	_, err := s.Dispatch(context.Background(), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: types.IdentityScope{Tenant: "t-1"}, // user + session missing
		ServerID: "srv-a", ResourceURI: "ui://x",
	})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

func TestAppsSurface_ReadResource_RequiresServerAndURI(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(context.Background(), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
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
	resp, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
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

func TestAppsSurface_CallTool_FailsClosedOnMissingIdentity(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: types.IdentityScope{}, Tool: "srv-a_x",
	})
	assertCode(t, err, protoerrors.CodeIdentityRequired)
}

func TestAppsSurface_Dispatch_RejectsNonAppsMethod(t *testing.T) {
	s := newAppsSurface(t, &stubResourceReader{}, &stubInvoker{})
	_, err := s.Dispatch(context.Background(), methods.MethodStart, &types.ReadMCPResourceRequest{Identity: validScope()})
	assertCode(t, err, protoerrors.CodeUnknownMethod)
}
