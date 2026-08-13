package mcp

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProvider_AppBinding_IsOpaqueAndScoped(t *testing.T) {
	p := &Provider{}
	ctx := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	token := p.mintAppBinding(ctx, "ui://app/main.html", "srv_callback")
	if token == "" || token == "srv-a" {
		t.Fatal("binding must be a non-empty opaque capability")
	}
	if !p.ValidateAppBinding(ctx, token, "srv_callback") {
		t.Fatal("runtime-issued binding was rejected")
	}
	if p.ValidateAppBinding(ctx, token+"x", "srv_callback") {
		t.Fatal("forged binding was accepted")
	}
	other := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "other"})
	if p.ValidateAppBinding(other, token, "srv_callback") {
		t.Fatal("binding crossed identity scope")
	}
}

// TestParseAppRef_RecognisesUIScheme covers the criterion that a tool
// result carrying a `_meta.ui.resourceUri` slot with a `ui://`-scheme URI
// is parsed into an AppRef.
func TestParseAppRef_RecognisesUIScheme(t *testing.T) {
	meta := mcpsdk.Meta{
		"ui": map[string]any{
			"resourceUri":    "ui://weather/view.html",
			"preferredFrame": "fullscreen",
		},
	}
	ref := parseAppRef(meta)
	if ref == nil {
		t.Fatal("parseAppRef returned nil for a ui:// resource — want an AppRef")
	}
	if ref.ResourceURI != "ui://weather/view.html" {
		t.Errorf("ResourceURI = %q, want ui://weather/view.html", ref.ResourceURI)
	}
	if ref.PreferredDisplayMode != "fullscreen" {
		t.Errorf("PreferredDisplayMode = %q, want fullscreen", ref.PreferredDisplayMode)
	}
}

// TestParseAppRef_RejectsNonUIScheme covers the criterion that an
// ordinary file:// / https:// resource is NOT treated as an app — the
// `ui://` recognition is distinct.
func TestParseAppRef_RejectsNonUIScheme(t *testing.T) {
	cases := []struct {
		name string
		meta mcpsdk.Meta
	}{
		{"file scheme", mcpsdk.Meta{"ui": map[string]any{"resourceUri": "file:///etc/passwd"}}},
		{"https scheme", mcpsdk.Meta{"ui": map[string]any{"resourceUri": "https://example.com/x.html"}}},
		{"empty uri", mcpsdk.Meta{"ui": map[string]any{"resourceUri": ""}}},
		{"missing resourceUri", mcpsdk.Meta{"ui": map[string]any{"other": "x"}}},
		{"ui not an object", mcpsdk.Meta{"ui": "ui://nope"}},
		{"no ui slot", mcpsdk.Meta{"other": "x"}},
		{"nil meta", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ref := parseAppRef(tc.meta); ref != nil {
				t.Errorf("parseAppRef = %+v, want nil (a non-ui:// resource must not be promoted to an app)", ref)
			}
		})
	}
}

// TestLowerCallToolResult_SurfacesAppRef proves the parse is wired into
// the tool-result lowering — the AppRef reaches MCPToolValue.
func TestLowerCallToolResult_SurfacesAppRef(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}},
	}
	res.Meta = mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://app/main.html"}}
	value, err := lowerCallToolResult(res)
	if err != nil {
		t.Fatalf("lowerCallToolResult: %v", err)
	}
	if value.AppRef == nil || value.AppRef.ResourceURI != "ui://app/main.html" {
		t.Fatalf("AppRef = %+v, want ResourceURI ui://app/main.html", value.AppRef)
	}
	// A result with NO _meta.ui is not an app.
	plain, _ := lowerCallToolResult(&mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hi"}},
	})
	if plain.AppRef != nil {
		t.Errorf("plain result AppRef = %+v, want nil", plain.AppRef)
	}
}
