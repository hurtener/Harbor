package mcp

import (
	"context"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
)

func appIdentity(t *testing.T, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: session})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestProvider_AppBinding_IsOpaqueAndResourceScoped(t *testing.T) {
	ctx := appIdentity(t, "s")
	const resourceURI = "ui://app/main.html"
	p := &Provider{}
	token := p.mintAppBinding(ctx, resourceURI, "srv_render")
	if token == "" || token == "srv_render" {
		t.Fatal("binding must be a non-empty opaque capability")
	}
	// The capability is resource-bound, not render-tool-bound: another callback
	// on the same provider may use it when it presents the same rendered resource.
	if !p.ValidateAppBinding(ctx, token, resourceURI) {
		t.Fatal("runtime-issued binding was rejected for a same-resource callback")
	}
	if p.ValidateAppBinding(ctx, token, "ui://other/main.html") {
		t.Fatal("binding crossed resource authority")
	}
	if p.ValidateAppBinding(appIdentity(t, "other"), token, resourceURI) {
		t.Fatal("binding crossed identity scope")
	}
	if p.ValidateAppBinding(ctx, token+"x", resourceURI) {
		t.Fatal("opaque binding forgery was accepted")
	}

	otherProvider := &Provider{}
	if p.ValidateAppBinding(ctx, otherProvider.mintAppBinding(ctx, resourceURI, "srv_render"), resourceURI) {
		t.Fatal("binding crossed provider scope")
	}
}

func TestProvider_AppBinding_EvictsExpiresAndCloses(t *testing.T) {
	ctx := appIdentity(t, "s")
	now := time.Unix(100, 0)
	p := &Provider{clock: func() time.Time { return now }}
	const resourceURI = "ui://app/main.html"

	first := p.mintAppBinding(ctx, resourceURI, "srv_callback")
	for range appBindingLimit {
		p.mintAppBinding(ctx, resourceURI, "srv_callback")
	}
	if p.ValidateAppBinding(ctx, first, resourceURI) {
		t.Fatal("oldest binding survived bounded eviction")
	}

	current := p.mintAppBinding(ctx, resourceURI, "srv_callback")
	now = now.Add(appBindingTTL)
	if p.ValidateAppBinding(ctx, current, resourceURI) {
		t.Fatal("expired binding was accepted")
	}

	live := p.mintAppBinding(ctx, resourceURI, "srv_callback")
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p.ValidateAppBinding(ctx, live, resourceURI) {
		t.Fatal("binding survived provider Close")
	}
}

// TestParseAppRef_RecognisesUIScheme covers the host-derived AppAttachment
// resource URI: a ui:// definition is promoted while ordinary URLs are not.
func TestParseAppRef_RecognisesUIScheme(t *testing.T) {
	meta := mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://weather/view.html", "preferredFrame": "fullscreen"}}
	ref := parseAppRef(meta)
	if ref == nil || ref.ResourceURI != "ui://weather/view.html" || ref.PreferredDisplayMode != "fullscreen" {
		t.Fatalf("parseAppRef = %+v, want host resource and display hint", ref)
	}
}

func TestParseAppRef_RejectsNonUIScheme(t *testing.T) {
	for _, uri := range []string{"file:///etc/passwd", "https://example.com/x.html", ""} {
		if ref := parseAppRef(mcpsdk.Meta{"ui": map[string]any{"resourceUri": uri}}); ref != nil {
			t.Errorf("parseAppRef(%q) = %+v, want nil", uri, ref)
		}
	}
}

func TestLowerCallToolResult_SurfacesAppRef(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}}, Meta: mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://app/main.html"}}}
	value, err := lowerCallToolResult(res)
	if err != nil {
		t.Fatalf("lowerCallToolResult: %v", err)
	}
	if value.AppRef == nil || value.AppRef.ResourceURI != "ui://app/main.html" {
		t.Fatalf("AppRef = %+v, want host resource URI", value.AppRef)
	}
}
