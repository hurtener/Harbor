package bifrost

import (
	"context"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
)

// TestNewAccount_BrokeredPrimary_SourcesKeyFromBroker proves the full seam the
// bifrost Account consumes: a `credential_source: remote` primary does NOT
// resolve a local key at construction; the shared LiveKey is seeded by the
// broker-pull InferenceKeySource, and GetKeysForProvider returns the pulled key
// (the exact bytes bifrost hands the provider) — no per-call broker round-trip.
func TestNewAccount_BrokeredPrimary_SourcesKeyFromBroker(t *testing.T) {
	const (
		dummyToken = "dummy-runtime-service-token-not-a-secret"
		dummyKey   = "dummy-openai-broker-key-not-a-secret"
		authEnv    = "HARBOR_BIFROST_BROKER_TEST_TOKEN"
	)
	t.Setenv(authEnv, dummyToken)

	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	srv := inferencebrokertest.New(t, dummyToken, dummyKey)

	// The shared holder: passed as Deps.LiveKey to the Account AND to the source.
	live := llm.NewLiveKey()

	// A brokered primary: no local api_key; the driver leaves the holder for the
	// source to seed.
	cfg := llm.ConfigSnapshot{
		Provider:         string(bfschemas.OpenAI),
		CredentialSource: "remote",
		InferenceBroker:  "openai-broker",
	}
	a, err := newAccount(cfg, llm.Deps{LiveKey: live})
	if err != nil {
		t.Fatalf("newAccount (brokered): %v", err)
	}
	// Before the source connects, the holder is empty — bifrost fails closed.
	if keys, _ := a.GetKeysForProvider(context.Background(), bfschemas.OpenAI); len(keys) == 1 && keys[0].Value.Val != "" {
		t.Fatalf("a brokered primary must NOT carry a local key before connect, got %q", keys[0].Value.Val)
	}

	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: "openai", BrokerName: "openai-broker", RuntimePrincipal: "rt-1",
		CredentialURL: srv.URL(), AuthTokenEnv: authEnv,
		Live: live, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// After connect, the Account's hot-path read returns the broker-pulled key.
	keys, err := a.GetKeysForProvider(context.Background(), bfschemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].Value.Val != dummyKey {
		t.Fatalf("GetKeysForProvider must return the broker-pulled key, got %+v", keys)
	}
}
