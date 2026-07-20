package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
)

// TestE2E_InferenceBrokerPull_SeamAndFailure wires the FULL inference-plane
// broker-pull seam under REAL drivers (patterns redactor + inmem bus + a
// spec-shaped fixture broker): InferenceKeySource → LiveKey (the exact holder
// bifrost's Account reads on the hot path) → the runtime-scoped audit event on
// the bus. It asserts (a) a connect pull seeds the LiveKey and fires the
// runtime-principal-keyed `llm.provider_credential_fetched` event (identity
// propagation for the runtime service token, NOT a session triple), and (b) a
// failure mode: a broker that goes unreachable on refresh past the TTL raises
// ErrProviderKeyUnavailable and serves NO stale key. Runs under -race.
func TestE2E_InferenceBrokerPull_SeamAndFailure(t *testing.T) {
	const (
		dummyToken = "dummy-runtime-service-token-not-a-secret"
		dummyKey   = "dummy-provider-key-not-a-secret"
		authEnv    = "HARBOR_E2E_INFERENCE_BROKER_TOKEN"
		principal  = "e2e-runtime-principal"
		broker     = "e2e-openai-broker"
	)
	t.Setenv(authEnv, dummyToken)

	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 2 * time.Second, DropWindow: 100 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Subscribe under the reserved runtime-principal scope (NOT a real session)
	// — the exact identity the runtime-scoped pull event is published under.
	runtimeID := identity.Identity{
		TenantID:  credsource.RuntimePrincipalTenant,
		UserID:    credsource.RuntimePrincipalUser,
		SessionID: principal,
	}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: runtimeID.TenantID, User: runtimeID.UserID, Session: runtimeID.SessionID,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	srv := inferencebrokertest.New(t, dummyToken, dummyKey)

	// The shared LiveKey is the exact holder bifrost's Account reads via
	// primaryKey.Get() on the hot path — no per-call broker round-trip.
	live := llm.NewLiveKey()
	clockT := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return clockT }

	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: "openai", BrokerName: broker, RuntimePrincipal: principal,
		CredentialURL: srv.URL(), AuthTokenEnv: authEnv, CacheTTL: 5 * time.Minute,
		Live: live, Bus: bus, Redactor: red, Clock: clock,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}

	// (a) Connect seeds the LiveKey + fires the runtime-scoped audit event.
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := live.Get(); got != dummyKey {
		t.Fatalf("bifrost hot-path read (LiveKey) = %q, want the broker-pulled key", got)
	}
	ev := waitInferenceEvent(t, sub.Events())
	p, ok := ev.Payload.(llm.LLMProviderCredentialFetchedPayload)
	if !ok {
		t.Fatalf("event payload type %T", ev.Payload)
	}
	if p.RuntimePrincipal != principal || p.Broker != broker {
		t.Fatalf("runtime-scoped attribution wrong: %+v", p)
	}
	if p.KeyFingerprint == "" || p.KeyFingerprint == dummyKey {
		t.Fatalf("fingerprint must be a non-reversible digest, never the key: %q", p.KeyFingerprint)
	}
	if ev.Identity.TenantID != credsource.RuntimePrincipalTenant {
		t.Fatalf("event must ride the reserved runtime scope (not a real session), got %q", ev.Identity.TenantID)
	}

	// (b) Failure mode: broker unreachable on refresh past the TTL → fail loud,
	// no stale key served.
	srv.SetPosture(inferencebrokertest.PostureDown)
	clockT = clockT.Add(6 * time.Minute)
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("want ErrProviderKeyUnavailable on unreachable refresh, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("MUST NOT serve a stale key past its refresh contract; LiveKey still = %q", got)
	}
	src.Close()
}

func waitInferenceEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == llm.EventTypeProviderCredentialFetched {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the runtime-scoped pull event")
		}
	}
}
