// Phase 154 — OAuth provider credential source: env or coordinator-served
// pull (RFC §6.4 + §3.3; D-285).
//
// The §17.8 fixture-server E2E: it wires the REAL artifacts across the
// phase surface — no mocks at any seam (CLAUDE.md §17.3 #1):
//
//   - the committed reference COORDINATOR fixture (credsourcetest) that
//     serves the runtime's broker CLIENT credential over the documented
//     authenticated-GET contract;
//   - a real RFC-8693 BROKER fixture (§17.8 spec-derived) that asserts the
//     coordinator-served client credential AND the verified identity
//     triple on the broker side, so a mis-wire fails HERE;
//   - the real `remote` credential source + the real `tokenexchange`
//     OAuth provider consuming it through the seam;
//   - a real events.EventBus (inmem) + real audit.Redactor (patterns) +
//     real pauseresume.Coordinator + a real state.StateStore-backed
//     TokenStore.
//
// The legs:
//
//  1. Zero-env boot → first-need pull → exchange succeeds. NO broker
//     client env is exported (defense-in-depth: the secret never enters
//     the runtime env); the credential is pulled from the coordinator at
//     the first Token() and the RFC-8693 exchange succeeds. Asserts the
//     coordinator was hit exactly once (cached thereafter) and the broker
//     saw the coordinator-served client_id + the verified triple.
//  2. Rotation — the coordinator swaps the client credential; after both
//     caches expire (shared controllable clock) the next Token()
//     re-pulls the rotated credential WITHOUT a restart, and the broker
//     observes the new client_id.
//  3. Failure — the coordinator goes down; Token() fails loud with
//     auth.ErrExchangeFailed wrapping credsource.ErrCredentialSourceUnavailable,
//     a tool.provider_credential_fetch_failed event is emitted, and there
//     is NO fallback to env / unauthenticated operation.
//
// Runs under -race. No time.Sleep as a sync primitive.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource/credsourcetest"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

// Documented dummy fixtures — never real values (§7 rule 2).
const (
	p154Provider     = "m365-broker"
	p154Audience     = "https://graph.microsoft.com"
	p154ServiceToken = "dummy-runtime-service-token-not-a-secret"
	p154AuthTokenEnv = "HARBOR_P154_COORDINATOR_TOKEN"
	p154ClientV1     = "dummy-broker-client-v1-not-a-secret"
	p154SecretV1     = "dummy-broker-secret-v1-not-a-secret"
	p154ClientV2     = "dummy-broker-client-v2-not-a-secret"
	p154SecretV2     = "dummy-broker-secret-v2-not-a-secret"

	p154Tenant  = "tenant-P154"
	p154User    = "user-P154"
	p154Session = "session-P154"
)

// p154Broker is the RFC-8693 broker fixture (§17.8). It records the
// client_id + subject triple it observed, so a credential-rotation or a
// cross-tenant bleed is assertable broker-side.
type p154Broker struct {
	server *httptest.Server

	mu         sync.Mutex
	calls      int
	lastClient string
	lastTenant string
	lastUser   string
	down       bool
}

func newP154Broker(t *testing.T) *p154Broker {
	t.Helper()
	b := &p154Broker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", b.handle)
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *p154Broker) tokenURL() string { return b.server.URL + "/oauth2/token" }

func (b *p154Broker) snapshot() (calls int, client, tenant, user string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.lastClient, b.lastTenant, b.lastUser
}

func (b *p154Broker) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// §17.8 broker-side wire assertions.
	if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
		http.Error(w, "grant_type", http.StatusBadRequest)
		return
	}
	if r.Form.Get("subject_token_type") != "urn:harbor:oauth:token-type:identity-triple" {
		http.Error(w, "subject_token_type", http.StatusBadRequest)
		return
	}
	var tenant, user string
	if raw, err := base64.RawURLEncoding.DecodeString(r.Form.Get("subject_token")); err == nil {
		var st struct {
			TenantID  string `json:"tenant_id"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(raw, &st)
		tenant, user = st.TenantID, st.UserID
	}

	b.mu.Lock()
	b.calls++
	b.lastClient = r.Form.Get("client_id")
	b.lastTenant = tenant
	b.lastUser = user
	down := b.down
	b.mu.Unlock()

	if down {
		http.Error(w, "broker down", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "brokered-" + tenant + "-" + user,
		"token_type":   "Bearer",
		"expires_in":   60, // short so the rotation leg can force a re-exchange
		"scope":        r.Form.Get("scope"),
	})
}

// p154Clock is a shared controllable clock so both the credential-source
// TTL and the tokenexchange token TTL expire together on Advance.
type p154Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *p154Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *p154Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func p154ID() identity.Identity {
	return identity.Identity{TenantID: p154Tenant, UserID: p154User, SessionID: p154Session}
}

func p154Ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), p154ID())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// p154Env bundles the real wiring: a coordinator fixture, an RFC-8693
// broker, a remote credential source, and a tokenexchange provider
// consuming it — all on a shared controllable clock.
type p154Env struct {
	coordinator *credsourcetest.FixtureServer
	broker      *p154Broker
	provider    auth.OAuthProvider
	bus         events.EventBus
	clock       *p154Clock
}

func newP154Env(t *testing.T) *p154Env {
	t.Helper()

	// The runtime's own service token is exported; the broker CLIENT
	// credential is NOT (it lives only in the coordinator's custody).
	t.Setenv(p154AuthTokenEnv, p154ServiceToken)

	coordinator := credsourcetest.New(t, p154ServiceToken, p154ClientV1, p154SecretV1)
	broker := newP154Broker(t)

	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	coord := pauseresume.New()

	// Real StateStore-backed TokenStore (the tokenexchange driver holds it
	// but never Puts — brokered tokens are memory-only).
	raw, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*5 + 1)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	store, err := auth.NewTokenStore(raw, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	clock := &p154Clock{t: time.Now()}

	// The `remote` credential source pulls the broker client credential
	// from the coordinator lazily at first need.
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: p154Provider,
		Remote: &credsource.RemoteConfig{
			URL:          coordinator.URL(),
			AuthTokenEnv: p154AuthTokenEnv,
			CacheTTL:     30 * time.Second, // short so rotation can expire it
		},
		Clock:    clock.Now,
		Bus:      bus,
		Redactor: red,
	})
	if err != nil {
		t.Fatalf("build remote credential source: %v", err)
	}
	if err := src.ValidateAtBoot(context.Background()); err != nil {
		t.Fatalf("ValidateAtBoot: %v", err)
	}

	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:             p154Provider,
		CredentialSource: src,
		Scopes:           []string{"Mail.Read"},
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": p154Audience, "cache_ttl_cap": "45s"},
	}, auth.FactoryDeps{
		Store:       store,
		Bus:         bus,
		Redactor:    red,
		Coordinator: coord,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Clock:       clock.Now,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	return &p154Env{coordinator: coordinator, broker: broker, provider: prov, bus: bus, clock: clock}
}

func p154Subscribe(t *testing.T, bus events.EventBus) <-chan events.Event {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: p154Tenant, User: p154User, Session: p154Session,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub.Events()
}

func p154WaitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

// TestE2E_Phase154_ZeroEnvBoot_FirstNeedPull_ExchangeSucceeds is leg 1.
func TestE2E_Phase154_ZeroEnvBoot_FirstNeedPull_ExchangeSucceeds(t *testing.T) {
	env := newP154Env(t)
	ch := p154Subscribe(t, env.bus)
	ctx := p154Ctx(t)

	tok, err := env.provider.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "brokered-"+p154Tenant+"-"+p154User {
		t.Fatalf("brokered token = %q, want identity-bound", tok.AccessToken)
	}

	// The coordinator was pulled from at first need (exactly once).
	if h := env.coordinator.Hits(); h != 1 {
		t.Fatalf("coordinator hits = %d, want 1 (pulled at first need)", h)
	}
	// The broker saw the coordinator-served client_id + the verified triple.
	calls, client, tenant, user := env.broker.snapshot()
	if calls != 1 {
		t.Fatalf("broker calls = %d, want 1", calls)
	}
	if client != p154ClientV1 {
		t.Fatalf("broker saw client_id %q, want the coordinator-served %q", client, p154ClientV1)
	}
	if tenant != p154Tenant || user != p154User {
		t.Fatalf("broker saw triple %s/%s, want %s/%s", tenant, user, p154Tenant, p154User)
	}

	// The credential fetch is observable.
	ev := p154WaitEvent(t, ch, credsource.EventTypeProviderCredentialFetched)
	p, ok := ev.Payload.(credsource.ProviderCredentialFetchedPayload)
	if !ok || p.Provider != p154Provider {
		t.Fatalf("fetched event payload = %#v", ev.Payload)
	}
	b, _ := json.Marshal(ev.Payload)
	if bytesContainsAny(b, p154ServiceToken, p154SecretV1, p154ClientV1) {
		t.Fatalf("SECRET LEAK in fetched event: %s", b)
	}

	// A second Token() within TTL re-uses caches — no new coordinator pull.
	if _, err := env.provider.Token(ctx, "any"); err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if h := env.coordinator.Hits(); h != 1 {
		t.Fatalf("coordinator hits after cache hit = %d, want 1", h)
	}
}

// TestE2E_Phase154_Rotation_RepullsWithoutRestart is leg 2.
func TestE2E_Phase154_Rotation_RepullsWithoutRestart(t *testing.T) {
	env := newP154Env(t)
	ctx := p154Ctx(t)

	if _, err := env.provider.Token(ctx, "any"); err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	_, client1, _, _ := env.broker.snapshot()
	if client1 != p154ClientV1 {
		t.Fatalf("first exchange client_id = %q, want %q", client1, p154ClientV1)
	}

	// The coordinator rotates the runtime's broker credential.
	env.coordinator.Rotate(p154ClientV2, p154SecretV2)
	// Advance past both the credential-source TTL (30s) and the brokered
	// token TTL (60s) so the next Token() re-exchanges AND re-pulls.
	env.clock.Advance(90 * time.Second)

	if _, err := env.provider.Token(ctx, "any"); err != nil {
		t.Fatalf("Token #2 (post-rotation): %v", err)
	}
	calls, client2, _, _ := env.broker.snapshot()
	if calls != 2 {
		t.Fatalf("broker calls = %d, want 2 (re-exchange after rotation)", calls)
	}
	if client2 != p154ClientV2 {
		t.Fatalf("post-rotation exchange client_id = %q, want rotated %q (picked up without restart)", client2, p154ClientV2)
	}
	if h := env.coordinator.Hits(); h != 2 {
		t.Fatalf("coordinator hits = %d, want 2 (re-pull after rotation)", h)
	}
}

// TestE2E_Phase154_CoordinatorDown_FailsLoudNoFallback is leg 3.
func TestE2E_Phase154_CoordinatorDown_FailsLoudNoFallback(t *testing.T) {
	env := newP154Env(t)
	ch := p154Subscribe(t, env.bus)
	ctx := p154Ctx(t)

	env.coordinator.SetPosture(credsourcetest.PostureDown)

	_, err := env.provider.Token(ctx, "any")
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("want wrapped ErrExchangeFailed, got %v", err)
	}
	if !errors.Is(err, credsource.ErrCredentialSourceUnavailable) {
		t.Fatalf("want wrapped ErrCredentialSourceUnavailable, got %v", err)
	}
	// NO fallback: the broker was never contacted (no unauthenticated /
	// env-fallback exchange).
	if calls, _, _, _ := env.broker.snapshot(); calls != 0 {
		t.Fatalf("broker calls = %d, want 0 (no fallback exchange)", calls)
	}
	// The failure is observable.
	ev := p154WaitEvent(t, ch, credsource.EventTypeProviderCredentialFetchFailed)
	if p, ok := ev.Payload.(credsource.ProviderCredentialFetchFailedPayload); !ok || p.Provider != p154Provider {
		t.Fatalf("fetch-failed event payload = %#v", ev.Payload)
	}
	// No secret bytes in the error.
	if containsAny(err.Error(), p154ServiceToken, p154SecretV1) {
		t.Fatalf("SECRET LEAK in error: %v", err)
	}
}

func bytesContainsAny(b []byte, subs ...string) bool { return containsAny(string(b), subs...) }

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
