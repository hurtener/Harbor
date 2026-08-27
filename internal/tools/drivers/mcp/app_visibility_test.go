package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestAppVisibilityOnly pins the `_meta.ui.visibility: ["app"]`
// classification rule (HA-56): the array must contain `app` AND contain no
// model-facing entry for the tool to be APP-ONLY. `["app"]` is a callback;
// `["app","tool"]` / `["app","all"]` are visible to both; an absent /
// empty / malformed visibility keeps the pre-existing ordinary default. An
// unknown future value is treated as non-model-facing (conservative: the
// callback stays OUT of planner context when in doubt).
func TestAppVisibilityOnly(t *testing.T) {
	cases := []struct {
		name string
		meta mcpsdk.Meta
		want bool
	}{
		{"canonical app-only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"app"}}}, true},
		{"[]string app-only", mcpsdk.Meta{"ui": map[string]any{"visibility": []string{"app"}}}, true},
		{"app plus unknown future value stays app-only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"app", "widget"}}}, true},
		{"app plus resourceUri stays app-only", mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://x/y.html", "visibility": []any{"app"}}}, true},
		{"app and tool", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"app", "tool"}}}, false},
		{"app and model", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"model", "app"}}}, false},
		{"app and planner", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"planner", "app"}}}, false},
		{"app and all", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"all", "app"}}}, false},
		{"tool only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"tool"}}}, false},
		{"model only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"model"}}}, false},
		{"no visibility keeps ordinary default", mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://x/y.html"}}, false},
		{"empty visibility", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{}}}, false},
		{"visibility malformed as string", mcpsdk.Meta{"ui": map[string]any{"visibility": "app"}}, false},
		{"visibility entries wrong type", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{42}}}, false},
		{"ui not an object", mcpsdk.Meta{"ui": "ui://nope"}, false},
		{"no ui slot", mcpsdk.Meta{"other": "x"}, false},
		{"nil meta", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appVisibilityOnly(tc.meta); got != tc.want {
				t.Errorf("appVisibilityOnly(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

// TestAppVisibilityContainsApp pins the broader dispatch classification:
// every visibility list containing `app` is App-visible, including a mixed
// model/App declaration. The narrower AppOnly classification remains false
// for those mixed declarations so the ordinary planner/model projection
// still retains them.
func TestAppVisibilityContainsApp(t *testing.T) {
	cases := []struct {
		name string
		meta mcpsdk.Meta
		want bool
	}{
		{"canonical app-only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"app"}}}, true},
		{"model and app", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"model", "app"}}}, true},
		{"tool and app", mcpsdk.Meta{"ui": map[string]any{"visibility": []string{"app", "tool"}}}, true},
		{"model only", mcpsdk.Meta{"ui": map[string]any{"visibility": []any{"model"}}}, false},
		{"no visibility", mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://x/y.html"}}, false},
		{"visibility malformed as string", mcpsdk.Meta{"ui": map[string]any{"visibility": "app"}}, false},
		{"ui not an object", mcpsdk.Meta{"ui": "ui://nope"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appVisibilityContainsApp(tc.meta); got != tc.want {
				t.Errorf("appVisibilityContainsApp(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

// appCatalogFixtureServer builds the HA-56 acceptance fixture server: one
// ordinary tool, one `_meta.ui.visibility: ["app"]` callback, and one tool
// visible to BOTH app and model — all from the same server, so one
// discovered snapshot must partition into the two deliberately different
// views. The provider-authored visibility rides the tool DEFINITION's
// `_meta.ui` slot (the spec-conformant placement, alongside resourceUri).
func appCatalogFixtureServer() *mcpsdk.Server {
	return appCatalogFixtureServerNamed("callback", "ui://app/callback.html")
}

// appCatalogFixtureServerNamed is the parameterised fixture builder: the
// replacement attach test needs a second generation whose app-only
// callback has a DIFFERENT name, so a stale first-generation callback
// surviving the replace is observable over the wire.
func appCatalogFixtureServerNamed(callbackName, callbackURI string) *mcpsdk.Server {
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-app-catalog-fixture", Version: "v0"},
		&mcpsdk.ServerOptions{},
	)
	emptySchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
	// plain: no `_meta.ui` at all — the ordinary default.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{Name: "plain", Description: "An ordinary model-facing tool.", InputSchema: emptySchema},
		func(context.Context, *mcpsdk.CallToolRequest, any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}}}, nil, nil
		},
	)
	// callback: app-only — a callback for the rendered App, not a model op.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        callbackName,
			Description: "An app-only callback for the server's rendered App.",
			Meta: mcpsdk.Meta{"ui": map[string]any{
				"resourceUri": callbackURI,
				"visibility":  []any{"app"},
			}},
			InputSchema: emptySchema,
		},
		func(context.Context, *mcpsdk.CallToolRequest, any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"cb":true}`}}}, nil, nil
		},
	)
	// both: visible to the model AND the App.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "both",
			Description: "A tool visible to both the model and the App.",
			Meta: mcpsdk.Meta{"ui": map[string]any{
				"resourceUri": "ui://app/both.html",
				"visibility":  []any{"app", "tool"},
			}},
			InputSchema: emptySchema,
		},
		func(context.Context, *mcpsdk.CallToolRequest, any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"both":true}`}}}, nil, nil
		},
	)
	return srv
}

// newAppCatalogProvider connects the fixture server to a real Provider over
// the SDK's in-memory transport pair — the same wire the HTTP / stdio
// transports share for discovery — so the classification below is proven
// against the SDK's own tools/list round-trip, not a hand-shaped blob.
func newAppCatalogProvider(t *testing.T, name string, srv *mcpsdk.Server) *Provider {
	t.Helper()
	p, err := New(Config{
		Name:            name,
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             newTestBus(t),
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

// TestProvider_Discover_StampsAppVisibilityFromVisibility proves the
// provider preserves the provider-authored `_meta.ui.visibility`
// classifications on the discovered Tool, so the attach path can partition
// ONE snapshot into the ordinary planner/model projection and the
// per-server App dispatch catalog. `plain` stays ordinary, `callback` is
// AppOnly and AppVisible, and `both` is model-visible plus AppVisible.
func TestProvider_Discover_StampsAppVisibilityFromVisibility(t *testing.T) {
	p := newAppCatalogProvider(t, "fixture", appCatalogFixtureServer())
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byName := make(map[string]tools.Tool, len(descs))
	for _, d := range descs {
		byName[d.Tool.Name] = d.Tool
	}
	cases := []struct {
		name       string
		appOnly    bool
		appVisible bool
	}{
		{"fixture_plain", false, false},
		{"fixture_callback", true, true},
		{"fixture_both", false, true},
	}
	for _, tc := range cases {
		tool, ok := byName[tc.name]
		if !ok {
			t.Fatalf("discovery missing tool %q (got %v)", tc.name, keysOf(byName))
			continue
		}
		if tool.AppOnly != tc.appOnly {
			t.Errorf("%s AppOnly = %v, want %v", tc.name, tool.AppOnly, tc.appOnly)
		}
		if tool.AppVisible != tc.appVisible {
			t.Errorf("%s AppVisible = %v, want %v", tc.name, tool.AppVisible, tc.appVisible)
		}
		if tool.Source != "fixture" || tool.Transport != tools.TransportMCP {
			t.Errorf("%s Source/Transport = %q/%q, want fixture/mcp", tc.name, tool.Source, tool.Transport)
		}
	}
}

// keysOf returns the sorted names of a descriptor-by-name map for a
// readable failure message.
func keysOf(m map[string]tools.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
