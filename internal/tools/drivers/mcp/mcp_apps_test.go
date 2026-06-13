package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// newAppsProvider builds an in-memory MCP server that advertises the
// `io.modelcontextprotocol/ui` extension (inline/fullscreen/pip) and
// hosts a `ui://` resource, paired with a connected Provider. Returns the
// Provider and a cleanup.
func newAppsProvider(t *testing.T, uiBody string) *Provider {
	t.Helper()
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-apps-test-server", Version: "v0"},
		&mcpsdk.ServerOptions{
			Capabilities: &mcpsdk.ServerCapabilities{
				Extensions: map[string]any{
					uiExtensionKey: map[string]any{
						"displayModes": []any{"inline", "fullscreen", "pip"},
					},
				},
			},
		},
	)
	srv.AddResource(
		&mcpsdk.Resource{URI: "ui://app/main.html", Name: "main", MIMEType: "text/html"},
		func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{
				Contents: []*mcpsdk.ResourceContents{
					{URI: req.Params.URI, MIMEType: "text/html", Text: uiBody},
				},
			}, nil
		},
	)

	bus := newTestBus(t)
	p, err := New(Config{
		Name:            "apps-server",
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		DefaultPolicy:   tools.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	clientSession, err := p.client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	p.mu.Lock()
	p.session = clientSession
	p.selectedMode = MCPTransportMode("inmemory")
	p.mu.Unlock()
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
		_ = p.Close(context.Background())
	})
	return p
}

func appsIdentityCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), defaultIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestProvider_ReadResource_FetchesUIDocument is the real-wire e2e: the
// driver reads a `ui://` resource from a live MCP server under the
// identity triple.
func TestProvider_ReadResource_FetchesUIDocument(t *testing.T) {
	p := newAppsProvider(t, "<html><body>hello app</body></html>")
	ctx, cancel := context.WithTimeout(appsIdentityCtx(t), 5*time.Second)
	defer cancel()
	content, mime, err := p.ReadResource(ctx, "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if string(content) != "<html><body>hello app</body></html>" {
		t.Errorf("content = %q", string(content))
	}
	if mime != "text/html" {
		t.Errorf("mime = %q, want text/html", mime)
	}
}

// TestProvider_ReadResource_FailsClosedOnMissingIdentity proves the
// identity-mandatory fail-closed posture.
func TestProvider_ReadResource_FailsClosedOnMissingIdentity(t *testing.T) {
	p := newAppsProvider(t, "x")
	_, _, err := p.ReadResource(context.Background(), "ui://app/main.html")
	if !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("ReadResource without identity: err = %v, want ErrIdentityMissing", err)
	}
}

// TestProvider_DisplayModes_NegotiatesFromCapability proves DisplayModes
// reflects the live server's advertised UI capability.
func TestProvider_DisplayModes_NegotiatesFromCapability(t *testing.T) {
	p := newAppsProvider(t, "x")
	got := p.DisplayModes()
	want := []string{"inline", "fullscreen", "pip"}
	if len(got) != len(want) {
		t.Fatalf("DisplayModes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DisplayModes = %v, want %v", got, want)
		}
	}
}

// TestRegistry_ReadResource_EndToEnd exercises the registry leg: identity
// is mandatory, an unknown server fails, and a real read returns content.
func TestRegistry_ReadResource_EndToEnd(t *testing.T) {
	p := newAppsProvider(t, "ui-doc")
	r := NewRegistry()
	if err := r.Register(ServerRegistration{
		Provider:     p,
		Transport:    "inmemory",
		InitialState: ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Display modes negotiated at Register time from the live capability.
	v, err := r.GetServer(appsIdentityCtx(t), "apps-server")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if len(v.DisplayModes) != 3 {
		t.Errorf("negotiated DisplayModes = %v, want 3", v.DisplayModes)
	}

	// Missing identity fails closed.
	if _, _, err := r.ReadResource(context.Background(), "apps-server", "ui://app/main.html"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("ReadResource no identity: err = %v, want ErrRegistryIdentityMissing", err)
	}
	// Unknown server.
	if _, _, err := r.ReadResource(appsIdentityCtx(t), "nope", "ui://x"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("ReadResource unknown server: err = %v, want ErrServerNotFound", err)
	}
	// Happy path.
	content, _, err := r.ReadResource(appsIdentityCtx(t), "apps-server", "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if string(content) != "ui-doc" {
		t.Errorf("content = %q, want ui-doc", string(content))
	}
}
