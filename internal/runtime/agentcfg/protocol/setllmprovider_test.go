package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// setllmprovider_test.go — unit coverage for `agent_config.set_llm_provider`:
// the zero-URL descriptor validation, the unknown-broker reject, the
// install-unavailable degradation, the live install, and the fail-closed audit
// (uninstall on emit failure). The reflective no-sink-field test is its OWN
// test in internal/protocol/types (TestSetLLMProviderWire_HasNoSinkField).

// fakeLLMInstaller records install/uninstall calls; installErr (when set) makes
// InstallLLMProvider fail loud so a test can assert no observable state.
type fakeLLMInstaller struct {
	mu           sync.Mutex
	installed    map[string]struct{}
	installErr   error
	installs     int
	uninstalls   int
	lastProvider agentcfg.LLMProviderDescriptor
}

func newFakeLLMInstaller() *fakeLLMInstaller {
	return &fakeLLMInstaller{installed: map[string]struct{}{}}
}

func (f *fakeLLMInstaller) InstallLLMProvider(_ context.Context, _, _ string, desc agentcfg.LLMProviderDescriptor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	if f.installErr != nil {
		return f.installErr
	}
	f.installed[desc.Name] = struct{}{}
	f.lastProvider = desc
	return nil
}

func (f *fakeLLMInstaller) UninstallLLMProvider(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstalls++
	delete(f.installed, name)
	return nil
}

func (f *fakeLLMInstaller) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.installed[name]
	return ok
}

func svcWithLLMInstaller(t *testing.T, inst agentcfgprotocol.LLMProviderInstaller, brokers []string, failBus bool) *agentcfgprotocol.Service {
	t.Helper()
	opts := []agentcfgprotocol.Option{
		agentcfgprotocol.WithInferenceBrokers(brokers),
	}
	if inst != nil {
		opts = append(opts, agentcfgprotocol.WithLLMProviderInstaller(inst))
	}
	if failBus {
		opts = append(opts, agentcfgprotocol.WithBus(failingBus{}))
	}
	s, err := agentcfgprotocol.NewService(newRegistry(t), opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func okLLMProvider() prototypes.AgentConfigLLMProviderDescriptor {
	return prototypes.AgentConfigLLMProviderDescriptor{
		Name: "openai", Provider: "openai", CredentialSource: "remote", InferenceBroker: "openai-broker",
		ModelAllow: []string{"gpt-4o"},
	}
}

func TestSetLLMProvider_Unavailable(t *testing.T) {
	// A known broker but no installer wired → 501-mapped ErrLLMProviderInstallUnavailable.
	s := svcWithLLMInstaller(t, nil, []string{"openai-broker"}, false)
	_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if !errors.Is(err, agentcfgprotocol.ErrLLMProviderInstallUnavailable) {
		t.Fatalf("want ErrLLMProviderInstallUnavailable, got %v", err)
	}
}

func TestSetLLMProvider_ValidationRejects(t *testing.T) {
	inst := newFakeLLMInstaller()
	s := svcWithLLMInstaller(t, inst, []string{"openai-broker"}, false)
	cases := map[string]prototypes.AgentConfigLLMProviderDescriptor{
		"empty name":           {Provider: "openai", CredentialSource: "remote", InferenceBroker: "openai-broker"},
		"empty provider":       {Name: "p", CredentialSource: "remote", InferenceBroker: "openai-broker"},
		"empty cred source":    {Name: "p", Provider: "openai", CredentialSource: "", InferenceBroker: "openai-broker"},
		"non-remote source":    {Name: "p", Provider: "openai", CredentialSource: "env", InferenceBroker: "openai-broker"},
		"empty inference brkr": {Name: "p", Provider: "openai", CredentialSource: "remote", InferenceBroker: ""},
	}
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
				Identity: scope(), AgentID: testAgentID, Provider: desc,
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidLLMProvider) {
				t.Fatalf("want ErrInvalidLLMProvider, got %v", err)
			}
		})
	}
	if inst.installs != 0 {
		t.Fatalf("no install on a validation reject, got %d", inst.installs)
	}
}

func TestSetLLMProvider_UnknownBrokerRejected(t *testing.T) {
	inst := newFakeLLMInstaller()
	// Broker set does NOT contain the descriptor's broker.
	s := svcWithLLMInstaller(t, inst, []string{"some-other-broker"}, false)
	_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if !errors.Is(err, agentcfgprotocol.ErrLLMProviderBrokerUnknown) {
		t.Fatalf("want ErrLLMProviderBrokerUnknown, got %v", err)
	}
	if inst.installs != 0 {
		t.Fatalf("unknown broker must not attempt an install")
	}
}

func TestSetLLMProvider_EmptyBrokerSetRejectsEveryName(t *testing.T) {
	inst := newFakeLLMInstaller()
	// No brokers declared → every name unknown (fail-closed default).
	s := svcWithLLMInstaller(t, inst, nil, false)
	_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if !errors.Is(err, agentcfgprotocol.ErrLLMProviderBrokerUnknown) {
		t.Fatalf("want ErrLLMProviderBrokerUnknown when no brokers declared, got %v", err)
	}
}

func TestSetLLMProvider_SuccessInstalls(t *testing.T) {
	inst := newFakeLLMInstaller()
	s := svcWithLLMInstaller(t, inst, []string{"openai-broker"}, false)
	resp, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if resp.Name != "openai" || !resp.Installed {
		t.Fatalf("bad response: %+v", resp)
	}
	if !inst.has("openai") {
		t.Fatalf("binding not installed live")
	}
	if inst.lastProvider.InferenceBroker != "openai-broker" || inst.lastProvider.Provider != "openai" {
		t.Fatalf("installed descriptor wrong: %+v", inst.lastProvider)
	}
}

func TestSetLLMProvider_AuditFailClosed(t *testing.T) {
	inst := newFakeLLMInstaller()
	s := svcWithLLMInstaller(t, inst, []string{"openai-broker"}, true)
	_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if err == nil {
		t.Fatalf("a failing audit emit must fail the call closed")
	}
	// Compensation: the binding was uninstalled (no observable state).
	if inst.has("openai") {
		t.Fatalf("audit fail-closed must UNINSTALL the just-installed binding")
	}
	if inst.uninstalls == 0 {
		t.Fatalf("audit fail-closed must call UninstallLLMProvider")
	}
}

func TestSetLLMProvider_InstallFailureNoState(t *testing.T) {
	inst := newFakeLLMInstaller()
	inst.installErr = errors.New("broker connect failed")
	s := svcWithLLMInstaller(t, inst, []string{"openai-broker"}, false)
	_, err := s.SetLLMProvider(context.Background(), prototypes.AgentConfigSetLLMProviderRequest{
		Identity: scope(), AgentID: testAgentID, Provider: okLLMProvider(),
	})
	if err == nil {
		t.Fatalf("a live install failure must fail loud")
	}
	if inst.has("openai") {
		t.Fatalf("a failed install must leave NO binding")
	}
}
