package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// Phase 166 (D-300) — credential-sink hardening, end-to-end with REAL
// drivers across the tools/auth + tools/mcp + config seam. It proves the
// two shipped-code exfil paths are closed: (1) a southbound binding to an
// UNLISTED downstream host is refused at attach — the connection never
// attaches unauthenticated; (2) a redirecting broker never receives a
// re-POSTed client_secret at the redirect target (the hardened
// token-exchange client refuses the redirect). Identity propagation is
// asserted on the exchange (the subject_token decodes to the ctx triple);
// the broker fixture asserts the RFC-8693 request shape (§17.8); a
// concurrency stress runs the refusal path under -race.

const (
	p166Tenant  = "tenant-166"
	p166User    = "user-166"
	p166Session = "sess-166"
)

func p166Identity() identity.Identity {
	return identity.Identity{TenantID: p166Tenant, UserID: p166User, SessionID: p166Session}
}

func p166Bus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// p166Provider builds a REAL tokenexchange provider with the given
// downstream-host allow-list, pointed at tokenURL. deps.HTTPClient (when
// non-nil) drives the supplied-client redirect-refusal path.
func p166Provider(t *testing.T, bus events.EventBus, tokenURL string, allowedHosts []string, httpClient *http.Client) auth.OAuthProvider {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sealer, err := auth.NewAESGCMSealer(bytes.Repeat([]byte{0x01}, auth.KEKSizeBytes))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store, err := auth.NewTokenStore(st, sealer)
	if err != nil {
		t.Fatalf("token store: %v", err)
	}
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   "m365",
		CredentialSource:       credsource.Static("dummy-client-id-not-a-secret", "dummy-client-secret-not-a-secret"),
		TokenURL:               tokenURL,
		Scopes:                 []string{"Mail.Read"},
		Audience:               "https://graph.microsoft.com",
		AllowedDownstreamHosts: allowedHosts,
	}, auth.FactoryDeps{
		Store:       store,
		Bus:         bus,
		Redactor:    auditpatterns.New(),
		Coordinator: pauseresume.New(),
		HTTPClient:  httpClient,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov
}

// TestE2E_Phase166_UnlistedDownstreamHostRefusedAtAttach proves a binding to
// a downstream host absent from the provider's allow-list is refused at
// attach, with real drivers — the connection never attaches unauthenticated.
func TestE2E_Phase166_UnlistedDownstreamHostRefusedAtAttach(t *testing.T) {
	bus := p166Bus(t)
	prov := p166Provider(t, bus, "https://broker.example.test/token", []string{"listed.example.test"}, nil)
	providers := map[string]auth.OAuthProvider{"m365": prov}

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	var closers []func(context.Context) error

	err := mcpdrv.Attach(context.Background(), config.MCPServerConfig{
		Name:          "evil-conn",
		TransportMode: "streamable_http",
		URL:           "https://evil.example.test", // NOT in the allow-list
		OAuthProvider: "m365",
	}, mcpdrv.AttachDeps{
		Catalog:         cat,
		Registry:        reg,
		Bus:             bus,
		DefaultIdentity: p166Identity(),
		Closers:         &closers,
		OAuthProviders:  providers,
	})
	if err == nil {
		t.Fatal("Attach accepted a binding to an unlisted downstream host — the bearer sink is unbounded")
	}
	if !errors.Is(err, mcpdrv.ErrOAuthBinding) {
		t.Fatalf("want ErrOAuthBinding, got %v", err)
	}
	// The refusal is pre-dial: no closer was appended, so no subprocess /
	// transport was ever opened (never attached unauthenticated).
	if len(closers) != 0 {
		t.Fatalf("a transport was opened despite the refusal (%d closers) — the connection attached unauthenticated", len(closers))
	}
}

// TestE2E_Phase166_RedirectingBrokerNeverRePostsClientSecret proves the
// hardened token-exchange client refuses a broker redirect, so the
// client_secret form is never replayed to the redirect target. The broker
// fixture asserts the RFC-8693 request shape and identity propagation
// (§17.8 + §17.3).
func TestE2E_Phase166_RedirectingBrokerNeverRePostsClientSecret(t *testing.T) {
	var targetCredReqs atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "" {
			targetCredReqs.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	var brokerHits atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		brokerHits.Add(1)
		// RFC-8693 request-shape assertions (§17.8) — a driver wired to the
		// wrong field fails HERE.
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
			http.Error(w, "grant_type", http.StatusBadRequest)
			return
		}
		if r.Form.Get("subject_token_type") != "urn:harbor:oauth:token-type:identity-triple" {
			http.Error(w, "subject_token_type", http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") == "" || r.Form.Get("client_secret") == "" {
			http.Error(w, "client auth", http.StatusUnauthorized)
			return
		}
		// Identity propagation: the subject_token decodes to the ctx triple.
		if sub := decodeP166Subject(t, r.Form.Get("subject_token")); sub.TenantID != p166Tenant || sub.UserID != p166User || sub.SessionID != p166Session {
			http.Error(w, "subject triple mismatch", http.StatusBadRequest)
			return
		}
		// The broker (the legitimate sink) answers with a redirect — the
		// exfil vector this phase closes. The client must refuse it.
		http.Redirect(w, r, target.URL+"/exfil", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(broker.Close)

	bus := p166Bus(t)
	prov := p166Provider(t, bus, broker.URL+"/token",
		[]string{"graph.microsoft.com"}, &http.Client{Timeout: 5 * time.Second})

	ctx, err := identity.With(context.Background(), p166Identity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = prov.Token(ctx, "")
	if err == nil {
		t.Fatal("Token followed the broker redirect — the client_secret would be replayed to the redirect target")
	}
	if !errors.Is(err, tokenexchange.ErrTokenEndpointRedirect) {
		t.Fatalf("want ErrTokenEndpointRedirect, got %v", err)
	}
	if got := targetCredReqs.Load(); got != 0 {
		t.Fatalf("the redirect target received %d credential-bearing requests — the client_secret was exfiltrated", got)
	}
	if got := brokerHits.Load(); got != 1 {
		t.Fatalf("broker hit %d times, want exactly 1 (the legitimate sink)", got)
	}
}

// TestE2E_Phase166_ConcurrentAttachRefusal_Race exercises the allow-list
// refusal seam under N>=100 concurrent attaches against ONE shared provider
// (D-025 / §17.3 concurrency stress). Every attach is refused; no race.
func TestE2E_Phase166_ConcurrentAttachRefusal_Race(t *testing.T) {
	bus := p166Bus(t)
	prov := p166Provider(t, bus, "https://broker.example.test/token", []string{"listed.example.test"}, nil)
	providers := map[string]auth.OAuthProvider{"m365": prov}

	const n = 120
	var wg sync.WaitGroup
	var refused atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cat := tools.NewCatalog()
			reg := mcpdrv.NewRegistry()
			var closers []func(context.Context) error
			err := mcpdrv.Attach(context.Background(), config.MCPServerConfig{
				Name:          "conn",
				TransportMode: "streamable_http",
				URL:           "https://evil.example.test",
				OAuthProvider: "m365",
			}, mcpdrv.AttachDeps{
				Catalog: cat, Registry: reg, Bus: bus,
				DefaultIdentity: p166Identity(), Closers: &closers, OAuthProviders: providers,
			})
			if errors.Is(err, mcpdrv.ErrOAuthBinding) && len(closers) == 0 {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := refused.Load(); got != n {
		t.Fatalf("refused %d/%d concurrent attaches; every unlisted-host binding must be refused", got, n)
	}
}

type p166Subject struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
}

func decodeP166Subject(t *testing.T, encoded string) p166Subject {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode subject_token: %v", err)
	}
	var s p166Subject
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal subject_token: %v", err)
	}
	return s
}
