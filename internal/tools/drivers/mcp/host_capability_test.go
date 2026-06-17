package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestHostCapabilities_AdvertisesMimeTypesNotDisplayModes asserts the host
// capability builder emits the spec UI capability — the `mimeTypes` array a
// conformant ext-apps server gates on (its `getUiCapability(caps).mimeTypes`
// check) — carrying the canonical ResourceMIMEType, and that the non-spec
// `displayModes` payload is GONE. It also preserves the SDK's roots
// advertisement (the regression guard — setting any capability overrides the
// SDK default, so RootsV2 must be replicated).
func TestHostCapabilities_AdvertisesMimeTypesNotDisplayModes(t *testing.T) {
	caps := hostCapabilities()
	if caps == nil {
		t.Fatal("hostCapabilities returned nil — the UI capability must always be advertised")
	}
	ext, ok := caps.Extensions[uiExtensionKey].(map[string]any)
	if !ok {
		t.Fatalf("UI extension %q absent or wrong shape: %#v", uiExtensionKey, caps.Extensions)
	}
	mimeTypes, ok := ext["mimeTypes"].([]string)
	if !ok {
		t.Fatalf("mimeTypes absent or wrong type: %#v", ext["mimeTypes"])
	}
	if want := []string{ResourceMIMEType}; !reflect.DeepEqual(mimeTypes, want) {
		t.Fatalf("mimeTypes = %v, want %v", mimeTypes, want)
	}
	if ResourceMIMEType != "text/html;profile=mcp-app" {
		t.Fatalf("ResourceMIMEType = %q, want the canonical ext-apps value", ResourceMIMEType)
	}
	// The non-spec displayModes capability payload must be GONE — display
	// modes ride the ui/initialize host-context, not the capability.
	if _, present := ext["displayModes"]; present {
		t.Fatalf("displayModes still present in the UI capability: %#v", ext)
	}
	// Roots-preserved regression guard: opting into the UI extension must NOT
	// drop the roots capability the runtime advertises today.
	if caps.RootsV2 == nil || !caps.RootsV2.ListChanged {
		t.Fatalf("roots capability dropped: RootsV2 = %#v (want ListChanged=true)", caps.RootsV2)
	}
}

// TestFilterHostDisplayModes_FiltersDedupesPreservesOrder pins the host-side
// filter DisplayModes() returns: only valid modes survive, duplicates
// collapse, advertised order is preserved.
func TestFilterHostDisplayModes_FiltersDedupesPreservesOrder(t *testing.T) {
	got := filterHostDisplayModes([]string{"pip", "bogus", "inline", "pip", "fullscreen"})
	want := []string{"pip", "inline", "fullscreen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterHostDisplayModes = %v, want %v", got, want)
	}
}

// TestProvider_DisplayModes_ReturnsConfiguredHostModes proves DisplayModes()
// returns the deployment's configured host modes (filtered) — NOT a value
// read off the server's capabilities (display modes are not a spec capability
// field).
func TestProvider_DisplayModes_ReturnsConfiguredHostModes(t *testing.T) {
	p, _ := newHostProvider(t, "modes", []string{"inline", "bogus", "pip", "inline"})
	got := p.DisplayModes()
	if want := []string{"inline", "pip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DisplayModes = %v, want %v", got, want)
	}

	// A provider with no configured host modes reports an empty set — never a
	// fabricated default.
	pEmpty, _ := newHostProvider(t, "nomodes", nil)
	if got := pEmpty.DisplayModes(); len(got) != 0 {
		t.Fatalf("DisplayModes (no config) = %v, want empty", got)
	}
}

// TestValidateTools_MCPAppDisplayModeAllowlistMirrors_MCPDriver pins the
// closed display-mode set the driver and config validator both know about.
// The config validator's `allowedMCPAppDisplayModes` map (in
// `internal/config/validate.go`) is intentionally duplicated to avoid a
// config → driver dependency edge; this test fails if the driver's set drifts
// from the documented mirror.
func TestValidateTools_MCPAppDisplayModeAllowlistMirrors_MCPDriver(t *testing.T) {
	want := map[string]struct{}{"inline": {}, "fullscreen": {}, "pip": {}}
	if !reflect.DeepEqual(validDisplayModes, want) {
		t.Fatalf("validDisplayModes = %v, want %v (config validator mirror must match)", validDisplayModes, want)
	}
}

// pairProviderCapturingServer connects p to m via in-memory transports and
// returns the live server session so a test can read the client's advertised
// InitializeParams — the real SDK-produced capability handshake, per §17.8
// (the fixture derives from the SDK's actual wire shape, not a hand blob).
func pairProviderCapturingServer(t *testing.T, m *mockServer, p *Provider) (*mcpsdk.ServerSession, func()) {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()

	serverSession, err := m.server.Connect(ctx, serverT, nil)
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
	recordServerForTest(p, m.server)

	return serverSession, func() {
		forgetServerForTest(p)
		_ = clientSession.Close()
		_ = serverSession.Wait()
	}
}

// newHostProvider builds a Provider with the supplied host display modes,
// wired to a real in-mem bus.
func newHostProvider(t *testing.T, name string, hostModes []string) (*Provider, *mockServer) {
	t.Helper()
	bus := newTestBus(t)
	m := newMockServer()
	p, err := New(Config{
		Name:             name,
		URL:              "http://example.invalid",
		TransportMode:    TransportAuto,
		Bus:              bus,
		DefaultIdentity:  defaultIdentity(),
		HostDisplayModes: hostModes,
		DefaultPolicy: tools.ToolPolicy{
			TimeoutMS: 2000,
			Validate:  tools.ValidateNone,
		},
	})
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}
	return p, m
}

// rootsListChanged reports whether server-received client capabilities still
// advertise roots.listChanged. The client syncs RootsV2 → the deprecated Roots
// field before sending (RootsV2 is json:"-"), so the server-side caps carry
// the value on Roots — the only field populated after the wire round-trip.
func rootsListChanged(caps *mcpsdk.ClientCapabilities) bool {
	if caps.RootsV2 != nil {
		return caps.RootsV2.ListChanged
	}
	//nolint:staticcheck // server-received wire caps populate the deprecated Roots field; RootsV2 is json:"-".
	return caps.Roots.ListChanged
}

// uiMimeTypes extracts the advertised UI-extension `mimeTypes` from a
// server-captured client InitializeParams, or nil when the extension is
// absent. The server receives the array as []any after JSON round-trip.
func uiMimeTypes(t *testing.T, caps *mcpsdk.ClientCapabilities) []string {
	t.Helper()
	if caps == nil {
		return nil
	}
	ext, ok := caps.Extensions[uiExtensionKey].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := ext["mimeTypes"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), v...)
	default:
		return nil
	}
}

// TestHostCapabilityAdvertisement_TwoProviders_EchoMimeTypesAndPreserveRoots
// is the cross-subsystem integration test: two MCP providers each advertise
// the spec UI `mimeTypes` capability to their server during the real SDK
// initialize handshake (the field a conformant server gates on), AND every
// provider still advertises roots (the regression guard). The capability is
// advertised UNCONDITIONALLY — a provider configured with no host display
// modes still advertises `mimeTypes` (display modes are not the capability).
// Identity still propagates on a real tool call. Real drivers on the seam
// (in-mem bus, real SDK transports).
func TestHostCapabilityAdvertisement_TwoProviders_EchoMimeTypesAndPreserveRoots(t *testing.T) {
	for _, name := range []string{"alpha", "beta"} {
		p, m := newHostProvider(t, name, []string{"inline", "pip"})
		serverSession, cleanup := pairProviderCapturingServer(t, m, p)
		t.Cleanup(func() {
			_ = p.Close(context.Background())
			cleanup()
		})

		iparams := serverSession.InitializeParams()
		if iparams == nil || iparams.Capabilities == nil {
			t.Fatalf("%s: server captured no client capabilities", name)
		}
		caps := iparams.Capabilities

		got := uiMimeTypes(t, caps)
		if want := []string{ResourceMIMEType}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: advertised mimeTypes = %v, want %v", name, got, want)
		}
		// Roots preserved (the SDK syncs RootsV2 → Roots on the wire).
		if !rootsListChanged(caps) {
			t.Fatalf("%s: roots capability dropped after opting into the UI extension (caps=%#v)", name, caps)
		}

		// Identity still propagates through a real tool call on the same
		// connected provider (the capability handshake does not disturb the
		// per-call identity path).
		ictx := mustIdentity(t)
		descs, err := p.Discover(ictx)
		if err != nil {
			t.Fatalf("%s: Discover: %v", name, err)
		}
		echo := findByName(descs, name+"_echo")
		if echo == nil {
			t.Fatalf("%s: expected %s_echo, got: %s", name, name, names(descs))
		}
		args, _ := json.Marshal(map[string]any{"text": "hi"})
		if _, err := echo.Invoke(ictx, args); err != nil {
			t.Fatalf("%s: Invoke echo: %v", name, err)
		}
		if m.metaFor("echo") == nil {
			t.Fatalf("%s: echo never captured caller _meta (identity did not propagate)", name)
		}
	}

	// A provider configured with NO host display modes STILL advertises the
	// spec `mimeTypes` capability (it is unconditional — Harbor always hosts
	// apps via the Console) and preserves roots.
	p, m := newHostProvider(t, "nomodes", nil)
	serverSession, cleanup := pairProviderCapturingServer(t, m, p)
	t.Cleanup(func() {
		_ = p.Close(context.Background())
		cleanup()
	})
	caps := serverSession.InitializeParams().Capabilities
	if caps == nil {
		t.Fatal("nomodes: server captured no client capabilities")
	}
	if got := uiMimeTypes(t, caps); !reflect.DeepEqual(got, []string{ResourceMIMEType}) {
		t.Fatalf("nomodes: advertised mimeTypes = %v, want %v", got, []string{ResourceMIMEType})
	}
	if !rootsListChanged(caps) {
		t.Fatalf("nomodes: roots capability dropped (caps=%#v)", caps)
	}
}
