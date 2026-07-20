package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// Documented dummy fixtures — never real secrets (§7 rule 2).
const (
	instDummyToken = "dummy-runtime-service-token-not-a-secret"
	instDummyKey   = "dummy-provider-key-not-a-secret"
	instAuthEnv    = "HARBOR_SERVE_INSTALLER_TEST_TOKEN"
)

// TestLLMProviderInstaller_BootConnectPrimary_UnreachableFailsLoud is the F2
// boot-fail-loud gate: a config-declared brokered primary whose broker is
// unreachable at boot fails LOUD (BootConnectPrimary returns an error — the
// boot exits non-zero) rather than leaving the LiveKey silently empty.
func TestLLMProviderInstaller_BootConnectPrimary_UnreachableFailsLoud(t *testing.T) {
	t.Setenv(instAuthEnv, instDummyToken)
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	live := llm.NewLiveKey()
	brokers := []config.InferenceBrokerConfig{{
		Name: "down-broker", CredentialURL: "http://127.0.0.1:9/provider-key", AuthTokenEnv: instAuthEnv,
		Timeout: 500 * time.Millisecond,
	}}
	inst := NewLLMProviderInstaller(live, "openai", brokers, bus, red, "rt-boot", nil)
	if inst == nil {
		t.Fatal("installer must be non-nil with a LiveKey + bus + redactor")
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	err = inst.BootConnectPrimary(context.Background(), "down-broker")
	if !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("an unreachable brokered primary must fail boot loud, got %v", err)
	}
	if live.Get() != "" {
		t.Fatalf("a failed boot-connect must NOT seed a key, got %q", live.Get())
	}
}

// TestLLMProviderInstaller_BootConnectPrimary_ConnectsAndSeeds proves the
// happy path: a reachable broker seeds the shared LiveKey synchronously at
// boot (the bifrost Account then reads the pulled key on the hot path).
func TestLLMProviderInstaller_BootConnectPrimary_ConnectsAndSeeds(t *testing.T) {
	t.Setenv(instAuthEnv, instDummyToken)
	srv := inferencebrokertest.New(t, instDummyToken, instDummyKey)
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	live := llm.NewLiveKey()
	brokers := []config.InferenceBrokerConfig{{Name: "up-broker", CredentialURL: srv.URL(), AuthTokenEnv: instAuthEnv}}
	inst := NewLLMProviderInstaller(live, "openai", brokers, bus, red, "rt-boot", nil)
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	if err := inst.BootConnectPrimary(context.Background(), "up-broker"); err != nil {
		t.Fatalf("BootConnectPrimary: %v", err)
	}
	if live.Get() != instDummyKey {
		t.Fatalf("boot-connect must seed the LiveKey with the pulled key, got %q", live.Get())
	}
}

// TestLLMProviderInstaller_Install_UnknownBrokerAndProviderMismatch pins the
// two client-error rejections the concrete surfaces (mapped to 400 at the
// wire edge): an unknown inference_broker and a provider that is not the
// runtime's configured primary.
func TestLLMProviderInstaller_Install_UnknownBrokerAndProviderMismatch(t *testing.T) {
	t.Setenv(instAuthEnv, instDummyToken)
	srv := inferencebrokertest.New(t, instDummyToken, instDummyKey)
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	live := llm.NewLiveKey()
	brokers := []config.InferenceBrokerConfig{{Name: "ok-broker", CredentialURL: srv.URL(), AuthTokenEnv: instAuthEnv}}
	inst := NewLLMProviderInstaller(live, "openai", brokers, bus, red, "rt", nil)
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	// Unknown broker.
	err = inst.InstallLLMProvider(context.Background(), "t", "a", agentcfg.LLMProviderDescriptor{
		Name: "b", Provider: "openai", CredentialSource: "remote", InferenceBroker: "nope",
	})
	if !errors.Is(err, agentcfgprotocol.ErrLLMProviderBrokerUnknown) {
		t.Fatalf("unknown broker → ErrLLMProviderBrokerUnknown, got %v", err)
	}
	// Provider mismatch (runtime primary is "openai").
	err = inst.InstallLLMProvider(context.Background(), "t", "a", agentcfg.LLMProviderDescriptor{
		Name: "b", Provider: "anthropic", CredentialSource: "remote", InferenceBroker: "ok-broker",
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidLLMProvider) {
		t.Fatalf("provider mismatch → ErrInvalidLLMProvider, got %v", err)
	}
}

// TestLLMProviderInstaller_InstallThenUninstall_ClosesBinding proves the
// provider-SET semantics: install seeds the LiveKey (async pull lands), and
// uninstall CLOSES the binding — the live key is zeroed so a bound call fails
// loud rather than serving the old key.
func TestLLMProviderInstaller_InstallThenUninstall_ClosesBinding(t *testing.T) {
	t.Setenv(instAuthEnv, instDummyToken)
	srv := inferencebrokertest.New(t, instDummyToken, instDummyKey)
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	live := llm.NewLiveKey()
	brokers := []config.InferenceBrokerConfig{{Name: "ok-broker", CredentialURL: srv.URL(), AuthTokenEnv: instAuthEnv}}
	inst := NewLLMProviderInstaller(live, "openai", brokers, bus, red, "rt", nil)
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	if err := inst.InstallLLMProvider(context.Background(), "t", "a", agentcfg.LLMProviderDescriptor{
		Name: "rot", Provider: "openai", CredentialSource: "remote", InferenceBroker: "ok-broker",
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The async first pull seeds the key — wait (bounded) for it.
	deadline := time.After(2 * time.Second)
	for live.Get() != instDummyKey {
		select {
		case <-deadline:
			t.Fatalf("install's async pull did not seed the LiveKey; got %q", live.Get())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := inst.UninstallLLMProvider(context.Background(), "rot"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if live.Get() != "" {
		t.Fatalf("uninstall must CLOSE the binding (zero the live key), got %q", live.Get())
	}
}
