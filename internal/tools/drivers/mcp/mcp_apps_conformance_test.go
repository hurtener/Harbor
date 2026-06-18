package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

// clientAdvertisesAppMimeType mirrors the ext-apps server-side
// `getUiCapability(caps).mimeTypes` gate: it returns true when the client's
// negotiated capabilities advertise the `io.modelcontextprotocol/ui` extension
// with a `mimeTypes` array containing the canonical MCP App media type. A
// conformant ext-apps server uses exactly this check to decide whether to
// register its `ui://` tools. The server receives the array as []any after the
// JSON wire round-trip.
func clientAdvertisesAppMimeType(caps *mcpsdk.ClientCapabilities) bool {
	if caps == nil {
		return false
	}
	ext, ok := caps.Extensions[uiExtensionKey].(map[string]any)
	if !ok {
		return false
	}
	raw, ok := ext["mimeTypes"]
	if !ok {
		return false
	}
	var modes []string
	switch v := raw.(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				modes = append(modes, s)
			}
		}
	case []string:
		modes = v
	}
	for _, m := range modes {
		if m == ResourceMIMEType {
			return true
		}
	}
	return false
}

// TestConformance_RealSDKServer_GatesUIToolOnMimeTypes is the FAIL-1
// revert-guard, substantiated against the REAL go-sdk handshake (§17.8 — the
// fixture derives from the official package's wire behaviour, not a hand blob).
//
// A spec-conformant ext-apps server registers its `ui://` tools ONLY when the
// connecting client advertises the UI capability with `mimeTypes` containing
// `text/html;profile=mcp-app` (its `getUiCapability(caps).mimeTypes` gate). This
// test models exactly that: a go-sdk server reads the capabilities Harbor's
// client actually sent during initialize, applies the gate, registers its
// `ui://`-bound tool only on a pass, and the test asserts Harbor then discovers
// it. If `hostCapabilities` were reverted to the non-spec `displayModes`
// payload, the gate would fail, the tool would never register, and Discover
// would not find it — so this fails loudly on a revert.
//
// Runs in CI (in-memory real-SDK transports, no external binary / API cost) —
// a stronger guard than an env-gated external probe. The external-binary
// HARBOR_LIVE_MCP probe (TestLive_MCPAppAvailable_RealExtAppsServer) remains
// for the end-to-end discovery path against go-study-mcp.
func TestConformance_RealSDKServer_GatesUIToolOnMimeTypes(t *testing.T) {
	const uiToolName = "studio"
	const uiResourceURI = "ui://conformance/studio/index.html"

	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-conformance-gating-server", Version: "v0"},
		&mcpsdk.ServerOptions{},
	)
	// A baseline non-UI tool so the server is valid pre-gate.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "ping",
			Description: "A non-UI tool present regardless of the UI gate.",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}}}, nil, nil
		},
	)

	bus := newTestBus(t)
	p, err := New(Config{
		Name:            "conformance",
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

	// The conformance gate: read the capabilities Harbor's client actually
	// advertised, and apply the SAME check a real ext-apps server applies.
	iparams := serverSession.InitializeParams()
	if iparams == nil || iparams.Capabilities == nil {
		t.Fatal("server captured no client capabilities")
	}
	if !clientAdvertisesAppMimeType(iparams.Capabilities) {
		t.Fatalf("FAIL-1 revert guard: Harbor's client did NOT advertise the spec UI mimeTypes capability "+
			"(getUiCapability(caps).mimeTypes must include %q) — a conformant server would not register its ui:// tools. "+
			"caps=%#v", ResourceMIMEType, iparams.Capabilities)
	}

	// Gate passed → the server registers its ui://-bound tool, exactly as a
	// conformant ext-apps server does only for a mimeTypes-advertising client.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        uiToolName,
			Description: "A ui://-bound tool registered only because the client passed the mimeTypes gate.",
			Meta:        mcpsdk.Meta{"ui": map[string]any{"resourceUri": uiResourceURI}},
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
		},
	)

	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// The ui:// tool is registered against Harbor (gate passed → tool present).
	if findByName(descs, "conformance_"+uiToolName) == nil {
		t.Fatalf("ui:// tool %q not discovered after the mimeTypes gate passed; have: %s",
			"conformance_"+uiToolName, names(descs))
	}

	// Sanity: the gate correctly REJECTS a displayModes-only advertisement (the
	// reverted shape) — proving the guard would catch a regression.
	reverted := &mcpsdk.ClientCapabilities{}
	reverted.AddExtension(uiExtensionKey, map[string]any{"displayModes": []string{"inline"}})
	if clientAdvertisesAppMimeType(reverted) {
		t.Fatal("gate incorrectly accepted a displayModes-only capability — the revert guard is inert")
	}
}
