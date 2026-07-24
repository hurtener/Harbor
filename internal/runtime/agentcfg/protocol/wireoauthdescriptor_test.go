package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// wireoauthdescriptor_test.go — the DEV-GATED wire-carried OAuth-provider
// descriptor (HA-32 / D-340). The load-bearing assertions:
//
//   - With the opt-in OFF (the default, all of production) a descriptor carrying
//     ANY credential-sink field (token_url / audience / remote) is REJECTED with
//     ErrWireDescriptorNotAllowed — for BOTH set_oauth_provider and the
//     add_mcp_connection inline binding — so the D-303 zero-URL posture is
//     unchanged. The name-only descriptor is unaffected.
//   - With the opt-in ON the provider installs, and its allowed_downstream_hosts
//     is DERIVED from the connection URL (never a wire field).
//   - A failed attach rolls back a newly-installed inline wire provider (no orphan).
//
// MUTATION-VERIFICATION: removing the `!s.allowWireOAuthDescriptor` guard in
// gateAndValidateOAuthProviderDescriptor makes TestWireOAuthDescriptor_OptInOff_*
// FAIL (a sink field is then accepted with the opt-in off) — confirming the guard
// is the thing under test, not a tautology.

// capturingInstaller records install/uninstall calls AND the last descriptor
// installed, so a test can assert the DERIVED allowed_downstream_hosts.
type capturingInstaller struct {
	mu          sync.Mutex
	installed   map[string]agentcfg.OAuthProviderDescriptor
	uninstalled []string
	installErr  error
}

func newCapturingInstaller() *capturingInstaller {
	return &capturingInstaller{installed: map[string]agentcfg.OAuthProviderDescriptor{}}
}

func (c *capturingInstaller) InstallProvider(_ context.Context, _, _ string, desc agentcfg.OAuthProviderDescriptor) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.installErr != nil {
		return c.installErr
	}
	c.installed[desc.Name] = desc
	return nil
}

func (c *capturingInstaller) UninstallProvider(_ context.Context, _, _, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uninstalled = append(c.uninstalled, name)
	delete(c.installed, name)
	return nil
}

func (c *capturingInstaller) get(name string) (agentcfg.OAuthProviderDescriptor, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.installed[name]
	return d, ok
}

func (c *capturingInstaller) uninstalledNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.uninstalled...)
}

// wireHarness wires a Service with a capturing installer + fake attacher + a real
// registry, and the wire-descriptor opt-in set to `allow`.
type wireHarness struct {
	svc      *agentcfgprotocol.Service
	inst     *capturingInstaller
	attacher *fakeAttacher
}

func newWireHarness(t *testing.T, allow bool, attachResult error) *wireHarness {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	coord := pauseresume.New(pauseresume.WithBus(bus))
	inst := newCapturingInstaller()
	attacher := &fakeAttacher{result: attachResult}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithProviderInstaller(inst),
		agentcfgprotocol.WithAllowWireOAuthDescriptor(allow),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	return &wireHarness{svc: s, inst: inst, attacher: attacher}
}

func wireProviderDesc(name string) prototypes.AgentConfigOAuthProviderDescriptor {
	return prototypes.AgentConfigOAuthProviderDescriptor{
		Name:             name,
		Driver:           "tokenexchange",
		CredentialSource: "remote",
		CredentialBroker: "m365-broker",
		TokenURL:         "https://broker.example.com/oauth2/token",
		Audience:         "https://graph.microsoft.com",
		Scopes:           []string{"mail.read"},
	}
}

// --- Gate OFF: every wire field is rejected (the D-303 posture) ---

func TestWireOAuthDescriptor_OptInOff_SetOAuthProvider_RejectsEachSinkField(t *testing.T) {
	h := newWireHarness(t, false, nil)
	cases := map[string]prototypes.AgentConfigOAuthProviderDescriptor{
		"token_url": {Name: "wp", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker", TokenURL: "https://broker/oauth2/token"},
		"audience":  {Name: "wp", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker", Audience: "https://graph"},
	}
	for field, desc := range cases {
		t.Run(field, func(t *testing.T) {
			_, err := h.svc.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
				Identity: scope(), AgentID: testAgentID, Provider: desc,
			})
			if !errors.Is(err, agentcfgprotocol.ErrWireDescriptorNotAllowed) {
				t.Fatalf("field %q: want ErrWireDescriptorNotAllowed, got %v", field, err)
			}
		})
	}
	if _, ok := h.inst.get("wp"); ok {
		t.Fatalf("no provider should be installed when the opt-in is off")
	}
}

func TestWireOAuthDescriptor_OptInOff_NameOnlyStillWorks(t *testing.T) {
	h := newWireHarness(t, false, nil)
	_, err := h.svc.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID,
		Provider: prototypes.AgentConfigOAuthProviderDescriptor{
			Name: "nameonly", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker",
		},
	})
	if err != nil {
		t.Fatalf("name-only install must succeed with the opt-in off: %v", err)
	}
	if _, ok := h.inst.get("nameonly"); !ok {
		t.Fatalf("name-only provider should have installed")
	}
}

func TestWireOAuthDescriptor_OptInOff_AddMCPConnection_InlineRejected(t *testing.T) {
	h := newWireHarness(t, false, nil)
	conn := prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "srv", Transport: "http", URL: "https://mcp.example.com/sse",
		OAuth: descPtr(wireProviderDesc("srv-oauth")),
	}
	_, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if !errors.Is(err, agentcfgprotocol.ErrWireDescriptorNotAllowed) {
		t.Fatalf("inline wire binding must be rejected with the opt-in off, got %v", err)
	}
	if len(h.attacher.calls) != 0 {
		t.Fatalf("no attach should be attempted on a gated reject")
	}
}

// --- Gate ON: install + derive ---

func TestWireOAuthDescriptor_OptInOn_SetOAuthProvider_Installs(t *testing.T) {
	h := newWireHarness(t, true, nil)
	_, err := h.svc.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: wireProviderDesc("wp"),
	})
	if err != nil {
		t.Fatalf("wire install must succeed with the opt-in on: %v", err)
	}
	d, ok := h.inst.get("wp")
	if !ok {
		t.Fatalf("wire provider should have installed")
	}
	if d.TokenURL == "" {
		t.Fatalf("installed wire descriptor lost its token_url: %+v", d)
	}
	// The wire descriptor NAMES a boot broker for the runtime's own credential
	// custody — no credential-source URL rides the wire.
	if d.CredentialBroker == "" {
		t.Fatalf("a wire descriptor must name a boot credential_broker (credential custody stays boot-declared), got %+v", d)
	}
	// No connection bound yet ⇒ the downstream sink is DEFERRED (empty), never a
	// wire-supplied value — a binding derives it.
	if len(d.AllowedDownstreamHosts) != 0 {
		t.Fatalf("set_oauth_provider must NOT fix a downstream host (derive happens at bind), got %v", d.AllowedDownstreamHosts)
	}
}

func TestWireOAuthDescriptor_OptInOn_AddMCPConnection_DerivesDownstreamHost(t *testing.T) {
	h := newWireHarness(t, true, nil) // attach online
	conn := prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "srv", Transport: "http", URL: "https://mcp.example.com:8443/sse",
		OAuth: descPtr(wireProviderDesc("srv-oauth")),
	}
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if err != nil {
		t.Fatalf("inline wire add must succeed with the opt-in on: %v", err)
	}
	if resp.State != "online" {
		t.Fatalf("want online, got %q (%s)", resp.State, resp.Reason)
	}
	d, ok := h.inst.get("srv-oauth")
	if !ok {
		t.Fatalf("inline wire provider should have installed")
	}
	// The downstream sink is DERIVED from the connection URL (host:port), never a
	// wire field.
	want := config.NormalizeDownstreamHost("https://mcp.example.com:8443/sse")
	if len(d.AllowedDownstreamHosts) != 1 || d.AllowedDownstreamHosts[0] != want {
		t.Fatalf("allowed_downstream_hosts must be DERIVED as %q, got %v", want, d.AllowedDownstreamHosts)
	}
	// The connection is bound to the installed provider by name; the attach saw it.
	if got := h.attacher.lastCall(t).OAuthProvider; got != "srv-oauth" {
		t.Fatalf("attach must bind the installed provider name, got %q", got)
	}
	if resp.Connection.OAuthProvider != "srv-oauth" {
		t.Fatalf("recorded connection should bind the installed provider name, got %q", resp.Connection.OAuthProvider)
	}
}

func TestWireOAuthDescriptor_OptInOn_FailedAttach_RollsBackInlineProvider(t *testing.T) {
	h := newWireHarness(t, true, errors.New("dial refused")) // attach fails
	conn := prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "srv", Transport: "http", URL: "https://mcp.example.com/sse",
		OAuth: descPtr(wireProviderDesc("srv-oauth")),
	}
	resp, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if err != nil {
		t.Fatalf("a failed attach is surfaced on the response, not an error: %v", err)
	}
	if resp.State != "failed" {
		t.Fatalf("want failed, got %q", resp.State)
	}
	// The newly-installed inline provider must be rolled back (no orphan).
	found := false
	for _, n := range h.inst.uninstalledNames() {
		if n == "srv-oauth" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a failed attach must uninstall the newly-installed inline wire provider; uninstalled=%v", h.inst.uninstalledNames())
	}
}

func TestWireOAuthDescriptor_OptInOn_InlineAndNameBindingMutuallyExclusive(t *testing.T) {
	h := newWireHarness(t, true, nil)
	conn := prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "srv", Transport: "http", URL: "https://mcp.example.com/sse",
		OAuthProvider: "some-name",
		OAuth:         descPtr(wireProviderDesc("srv-oauth")),
	}
	_, err := h.svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("inline oauth + oauth_provider name must be rejected, got %v", err)
	}
}

func descPtr(v prototypes.AgentConfigOAuthProviderDescriptor) *prototypes.AgentConfigOAuthProviderDescriptor {
	return &v
}
