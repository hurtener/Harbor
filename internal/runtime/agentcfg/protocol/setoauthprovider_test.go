package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// setoauthprovider_test.go — unit coverage for
// `agent_config.set_oauth_provider` / `remove_oauth_provider`: the zero-URL
// descriptor validation, the boot-declared collision, install-unavailable, the
// revision write + owner-tagged install, the uninstall symmetry, and the
// fail-closed audit (revert on emit failure).

// fakeInstaller records install/uninstall calls; installErr (when set) makes
// InstallProvider fail loud so a test can assert the revision is rolled back.
type fakeInstaller struct {
	mu         sync.Mutex
	installed  map[string]struct{} // name -> present
	installErr error
	installs   int
	uninstalls int
	// lastUninstallOwner records the (tenant, agentID) the handler passed to the
	// most recent UninstallProvider call, so a test can assert the caller-side
	// owner is threaded to the store boundary (AC-5).
	lastUninstallTenant  string
	lastUninstallAgentID string
}

func newFakeInstaller() *fakeInstaller {
	return &fakeInstaller{installed: map[string]struct{}{}}
}

func (f *fakeInstaller) InstallProvider(_ context.Context, _, _ string, desc agentcfg.OAuthProviderDescriptor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	if f.installErr != nil {
		return f.installErr
	}
	f.installed[desc.Name] = struct{}{}
	return nil
}

func (f *fakeInstaller) PrepareProvider(_ context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) (agentcfgprotocol.PreparedOAuthProvider, error) {
	return &testPreparedOAuthProvider{
		publish:  func(ctx context.Context) error { return f.InstallProvider(ctx, tenant, agentID, desc) },
		rollback: func(ctx context.Context) error { return f.UninstallProvider(ctx, tenant, agentID, desc.Name) },
	}, nil
}

type testPreparedOAuthProvider struct {
	publish   func(context.Context) error
	rollback  func(context.Context) error
	published bool
}

func (*testPreparedOAuthProvider) Binding() toolauth.OAuthProvider { return nil }
func (p *testPreparedOAuthProvider) Publish(ctx context.Context) error {
	if p.published {
		return nil
	}
	if err := p.publish(ctx); err != nil {
		return err
	}
	p.published = true
	return nil
}
func (*testPreparedOAuthProvider) Commit(context.Context) {}
func (p *testPreparedOAuthProvider) Rollback(ctx context.Context) error {
	if !p.published {
		return nil
	}
	p.published = false
	return p.rollback(ctx)
}
func (p *testPreparedOAuthProvider) Close(ctx context.Context) error { return p.Rollback(ctx) }

func (f *fakeInstaller) UninstallProvider(_ context.Context, tenant, agentID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstalls++
	f.lastUninstallTenant = tenant
	f.lastUninstallAgentID = agentID
	delete(f.installed, name)
	return nil
}

func (f *fakeInstaller) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.installed[name]
	return ok
}

func svcWithInstaller(t *testing.T, inst agentcfgprotocol.ProviderInstaller, boot []string, bus interface {
	opt() agentcfgprotocol.Option
}) *agentcfgprotocol.Service {
	t.Helper()
	opts := []agentcfgprotocol.Option{
		agentcfgprotocol.WithBootDeclaredOAuthProviders(boot),
	}
	if inst != nil {
		opts = append(opts, agentcfgprotocol.WithProviderInstaller(inst))
	}
	if bus != nil {
		opts = append(opts, bus.opt())
	}
	s, err := agentcfgprotocol.NewService(newRegistry(t), opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func okProvider(name string) prototypes.AgentConfigOAuthProviderDescriptor {
	return prototypes.AgentConfigOAuthProviderDescriptor{
		Name: name, Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "m365-broker", Scopes: []string{"mail.read"},
	}
}

func TestSetOAuthProvider_Unavailable(t *testing.T) {
	s := svcWithInstaller(t, nil, nil, nil)
	_, err := s.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	})
	if !errors.Is(err, agentcfgprotocol.ErrProviderInstallUnavailable) {
		t.Fatalf("want ErrProviderInstallUnavailable, got %v", err)
	}
}

func TestSetOAuthProvider_ValidationRejects(t *testing.T) {
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, nil, nil)
	cases := map[string]prototypes.AgentConfigOAuthProviderDescriptor{
		"empty name":             {Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "b"},
		"bad driver":             {Name: "m", Driver: "oauth2", CredentialSource: "remote", CredentialBroker: "b"},
		"empty cred source":      {Name: "m", Driver: "tokenexchange", CredentialSource: "", CredentialBroker: "b"},
		"non-remote cred src":    {Name: "m", Driver: "tokenexchange", CredentialSource: "env", CredentialBroker: "b"},
		"empty credential brokr": {Name: "m", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: ""},
	}
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
				Identity: scope(), AgentID: testAgentID, Provider: desc,
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidProvider) {
				t.Fatalf("want ErrInvalidProvider, got %v", err)
			}
		})
	}
	if inst.installs != 0 {
		t.Fatalf("no install should have been attempted on a validation reject, got %d", inst.installs)
	}
}

func TestSetOAuthProvider_BootCollisionRefused(t *testing.T) {
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, []string{"github"}, nil)
	_, err := s.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("github"),
	})
	if !errors.Is(err, agentcfgprotocol.ErrBootDeclaredProvider) {
		t.Fatalf("want ErrBootDeclaredProvider, got %v", err)
	}
	if inst.installs != 0 {
		t.Fatalf("boot collision must not attempt an install")
	}
}

func TestSetOAuthProvider_SuccessInstallsAndRecords(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, nil, nil)
	resp, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if resp.Name != "m365" || resp.Revision.RevisionID == "" {
		t.Fatalf("bad response: %+v", resp)
	}
	if !inst.has("m365") {
		t.Fatalf("provider not installed live")
	}
	// The revision records the zero-URL descriptor.
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || got.Revision == nil || got.Revision.Payload.OAuthProviders == nil {
		t.Fatalf("get after set: %+v err=%v", got, err)
	}
	provs := got.Revision.Payload.OAuthProviders.Providers
	if len(provs) != 1 || provs[0].Name != "m365" || provs[0].CredentialBroker != "m365-broker" {
		t.Fatalf("recorded providers: %+v", provs)
	}
}

func TestSetOAuthProvider_InstallFailureLeavesNoRevision(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	inst.installErr = agentcfgprotocol.ErrProviderBrokerUnknown
	s := svcWithInstaller(t, inst, nil, nil)
	// Seed a prior revision so we can prove the failed install rolled back to it.
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"keep"}}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	})
	if !errors.Is(err, agentcfgprotocol.ErrProviderBrokerUnknown) {
		t.Fatalf("want broker-unknown, got %v", err)
	}
	got, _ := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if got.Revision == nil || got.Revision.Payload.OAuthProviders != nil {
		t.Fatalf("a failed install must leave NO oauth-providers section, got %+v", got.Revision.Payload.OAuthProviders)
	}
}

// busOpt adapts a failing bus into a Service option for svcWithInstaller.
type busOpt struct{}

func (busOpt) opt() agentcfgprotocol.Option { return agentcfgprotocol.WithBus(failingBus{}) }

func TestSetOAuthProvider_AuditFailClosed(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, nil, busOpt{})
	_, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	})
	if err == nil {
		t.Fatalf("a failing audit emit must fail the call closed")
	}
	// Compensation: the provider was uninstalled and the revision rolled back.
	if inst.has("m365") {
		t.Fatalf("audit fail-closed must UNINSTALL the just-installed provider (no observable state)")
	}
	got, _ := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if got.Set && got.Revision != nil && got.Revision.Payload.OAuthProviders != nil {
		t.Fatalf("audit fail-closed must leave no installed-provider section")
	}
}

func TestRemoveOAuthProvider_NotFoundAndBoot(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, []string{"github"}, nil)
	// Unknown name.
	_, err := s.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Name: "nope",
	})
	if !errors.Is(err, agentcfgprotocol.ErrProviderNotFound) {
		t.Fatalf("want ErrProviderNotFound, got %v", err)
	}
	// Boot-declared name.
	_, err = s.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Name: "github",
	})
	if !errors.Is(err, agentcfgprotocol.ErrBootDeclaredProvider) {
		t.Fatalf("want ErrBootDeclaredProvider, got %v", err)
	}
}

func TestRemoveOAuthProvider_UninstallsAndDrops(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, nil, nil)
	if _, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	resp, err := s.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Name: "m365",
	})
	if err != nil || !resp.Uninstalled || resp.Name != "m365" {
		t.Fatalf("remove: %+v err=%v", resp, err)
	}
	if inst.has("m365") {
		t.Fatalf("remove must uninstall the provider live")
	}
	got, _ := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if got.Revision != nil && got.Revision.Payload.OAuthProviders != nil {
		t.Fatalf("remove must drop the descriptor from the revision")
	}
}

// TestRemoveOAuthProvider_PassesResolvedOwner is the AC-5 gate: the handler
// threads the caller's resolved owner (the active agent-config revision owner —
// the request tenant + agentID) to UninstallProvider, so the provider set's
// store-boundary owner check has the owner it needs to refuse a cross-owner
// drop. The caller-side owner resolution stays; the store check is additive.
func TestRemoveOAuthProvider_PassesResolvedOwner(t *testing.T) {
	ctx := context.Background()
	inst := newFakeInstaller()
	s := svcWithInstaller(t, inst, nil, nil)
	if _, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okProvider("m365"),
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := s.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
		Identity: scope(), AgentID: testAgentID, Name: "m365",
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// scope() → tenant "t"; testAgentID → "agent-x": the exact owner the store's
	// cross-owner check compares against.
	if inst.lastUninstallTenant != "t" || inst.lastUninstallAgentID != testAgentID {
		t.Fatalf("handler must pass resolved owner (tenant, agentID) to UninstallProvider, got (%q, %q)",
			inst.lastUninstallTenant, inst.lastUninstallAgentID)
	}
}
