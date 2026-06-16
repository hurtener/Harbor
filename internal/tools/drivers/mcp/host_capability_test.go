package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestHostCapabilities_AdvertisesConfiguredModes asserts the host capability
// builder emits the UI extension carrying the configured display modes, and
// preserves the SDK's roots advertisement (the regression guard — setting any
// capability overrides the SDK default, so RootsV2 must be replicated).
func TestHostCapabilities_AdvertisesConfiguredModes(t *testing.T) {
	caps := hostCapabilities([]string{"inline", "pip"})
	if caps == nil {
		t.Fatal("hostCapabilities returned nil for a non-empty configured mode set")
	}
	ext, ok := caps.Extensions[uiExtensionKey].(map[string]any)
	if !ok {
		t.Fatalf("UI extension %q absent or wrong shape: %#v", uiExtensionKey, caps.Extensions)
	}
	got, ok := ext["displayModes"].([]string)
	if !ok {
		t.Fatalf("displayModes absent or wrong type: %#v", ext["displayModes"])
	}
	if want := []string{"inline", "pip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("displayModes = %v, want %v", got, want)
	}
	// Roots-preserved regression guard: opting into the UI extension must NOT
	// drop the roots capability the runtime advertises today.
	if caps.RootsV2 == nil || !caps.RootsV2.ListChanged {
		t.Fatalf("roots capability dropped: RootsV2 = %#v (want ListChanged=true)", caps.RootsV2)
	}
}

// TestHostCapabilities_NoRenderableModesLeavesSDKDefault asserts that with no
// configured (or no VALID) modes the builder returns nil, leaving the SDK's
// default capability advertisement in place — the behaviour for an embedder
// that does not opt in.
func TestHostCapabilities_NoRenderableModesLeavesSDKDefault(t *testing.T) {
	for name, in := range map[string][]string{
		"nil":         nil,
		"empty":       {},
		"invalidOnly": {"bogus", "hologram"},
	} {
		t.Run(name, func(t *testing.T) {
			if caps := hostCapabilities(in); caps != nil {
				t.Fatalf("hostCapabilities(%v) = %#v, want nil", in, caps)
			}
		})
	}
}

// TestFilterHostDisplayModes_FiltersDedupesPreservesOrder pins the host-side
// filter as the mirror of the server-side negotiation: only valid modes
// survive, duplicates collapse, advertised order is preserved.
func TestFilterHostDisplayModes_FiltersDedupesPreservesOrder(t *testing.T) {
	got := filterHostDisplayModes([]string{"pip", "bogus", "inline", "pip", "fullscreen"})
	want := []string{"pip", "inline", "fullscreen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterHostDisplayModes = %v, want %v", got, want)
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

// uiDisplayModes extracts the advertised UI-extension display modes from a
// server-captured client InitializeParams, or nil when the extension is
// absent. The server receives the modes as []any after JSON round-trip.
func uiDisplayModes(t *testing.T, caps *mcpsdk.ClientCapabilities) []string {
	t.Helper()
	if caps == nil {
		return nil
	}
	ext, ok := caps.Extensions[uiExtensionKey].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := ext["displayModes"]
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

// TestHostCapabilityAdvertisement_TwoProviders_EchoUIExtensionAndPreserveRoots
// is the cross-subsystem integration test: two MCP providers built from ONE
// resolved host-display-mode config value each advertise the UI extension to
// their server during the real SDK initialize handshake, the servers echo
// back the configured modes, AND every provider still advertises roots
// (the regression guard). Identity still propagates on a real tool call, and
// the failure mode — a provider that opts out — advertises roots with NO UI
// extension. Real drivers on the seam (in-mem bus, real SDK transports).
func TestHostCapabilityAdvertisement_TwoProviders_EchoUIExtensionAndPreserveRoots(t *testing.T) {
	// ONE config value resolved once, threaded into BOTH providers — mirrors
	// the boot loader sourcing tools.mcp_app_host.display_modes once.
	cfg := config.ToolsConfig{MCPAppHost: &config.MCPAppHostConfig{DisplayModes: []string{"inline", "pip"}}}
	hostModes := cfg.MCPAppHostDisplayModes()

	for _, name := range []string{"alpha", "beta"} {
		p, m := newHostProvider(t, name, hostModes)
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

		got := uiDisplayModes(t, caps)
		if want := []string{"inline", "pip"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: advertised displayModes = %v, want %v", name, got, want)
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

	// Failure mode / opt-out: a provider with NO host modes advertises roots
	// and NO UI extension — the backward-compatible default path.
	p, m := newHostProvider(t, "optout", nil)
	serverSession, cleanup := pairProviderCapturingServer(t, m, p)
	t.Cleanup(func() {
		_ = p.Close(context.Background())
		cleanup()
	})
	caps := serverSession.InitializeParams().Capabilities
	if caps == nil {
		t.Fatal("optout: server captured no client capabilities")
	}
	if got := uiDisplayModes(t, caps); got != nil {
		t.Fatalf("optout: advertised UI extension %v despite no host modes", got)
	}
	if !rootsListChanged(caps) {
		t.Fatalf("optout: roots capability dropped on the default (no-opt-in) path (caps=%#v)", caps)
	}
}
