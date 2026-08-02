package tokenexchange_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

// TestTokenExchange_AudienceFromCeilingNotProviderName pins the D-300
// audience ceiling: when a boot `Audience` is declared it is the authority
// for the exchanged token's audience — NOT the provider name, and NOT the
// extra.audience legacy override. The broker-side form is the ground truth.
func TestTokenExchange_AudienceFromCeilingNotProviderName(t *testing.T) {
	broker := newFakeBroker(t)
	deps, _, _ := mkDeps(t)
	const ceiling = "https://ceiling.example/api"
	cfg := auth.ProviderConfig{
		Name:             tProviderName, // the caller-chosen name — must NOT become the audience
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": "https://legacy.example/should-be-overridden"},
		Audience:         ceiling,
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	if _, err := prov.Token(mkCtx(t, aliceID()), ""); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := broker.form().Get("audience"); got != ceiling {
		t.Fatalf("broker audience = %q, want the boot ceiling %q (not the provider name / legacy extra)", got, ceiling)
	}
}

// TestTokenExchange_ScopesIntersectedAgainstCeiling pins the D-300 scope
// ceiling: a requested scope outside the ceiling is dropped, never sent.
func TestTokenExchange_ScopesIntersectedAgainstCeiling(t *testing.T) {
	broker := newFakeBroker(t)
	deps, _, _ := mkDeps(t)
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read", "Files.ReadWrite", "Calendars.Read"},
		ScopeCeiling:     []string{"Mail.Read", "Calendars.Read"}, // Files.ReadWrite is out-of-ceiling
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	if _, err := prov.Token(mkCtx(t, aliceID()), ""); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got := broker.form().Get("scope")
	if got != "Mail.Read Calendars.Read" {
		t.Fatalf("broker scope = %q, want the intersection %q (Files.ReadWrite dropped)", got, "Mail.Read Calendars.Read")
	}
}

// TestTokenExchange_HTTPClient_RefusesPrivateDial proves the production
// hardened client (nil deps.HTTPClient) refuses a private-range / link-local
// dial at dial time — the credential-bearing POST is never sent to an
// RFC1918 destination (the DNS-rebinding backstop). Loopback is a
// deliberate carve-out (a boot-declared localhost broker is legitimate), so
// this asserts on a non-loopback private address.
func TestTokenExchange_HTTPClient_RefusesPrivateDial(t *testing.T) {
	deps, _, _ := mkDeps(t)
	deps.HTTPClient = nil // force the production, fully-hardened client (dial control)
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		// An RFC1918 token endpoint: the dial control refuses it post-DNS.
		TokenURL: "http://10.255.255.1:9/oauth2/token",
		Extra:    map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	_, err = prov.Token(mkCtx(t, aliceID()), "")
	if err == nil {
		t.Fatal("Token succeeded against an RFC1918 token endpoint — the hardened client must refuse the private dial")
	}
	if !errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("want ErrPrivateDialRefused, got %v", err)
	}
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("want the failure wrapped as ErrExchangeFailed, got %v", err)
	}
}

// TestTokenExchange_HTTPClient_AllowsLoopback pins the loopback carve-out:
// the production hardened client (nil deps.HTTPClient) reaches a
// boot-declared broker on loopback — the legitimate localhost-sidecar
// deployment (and the fixture-broker path the integration tests use).
func TestTokenExchange_HTTPClient_AllowsLoopback(t *testing.T) {
	broker := newFakeBroker(t) // httptest binds 127.0.0.1
	deps, _, _ := mkDeps(t)
	deps.HTTPClient = nil // force the production, fully-hardened client
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	if _, err := prov.Token(mkCtx(t, aliceID()), ""); err != nil {
		t.Fatalf("Token against a loopback broker must succeed (localhost-sidecar carve-out): %v", err)
	}
}

// TestTokenExchange_HTTPClient_RefusesRedirect proves the client refuses a
// redirect from the token endpoint — the client_secret form body is never
// replayed to the redirect target (Go replays the body on 307/308). Uses a
// supplied client (the shallow-copy + redirect-refusal path).
func TestTokenExchange_HTTPClient_RefusesRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true // must NEVER fire — the redirect must be refused first
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(broker.Close)

	deps, _, _ := mkDeps(t) // provider-owned hardened client → redirect-refusal path
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         broker.URL + "/oauth2/token",
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	_, err = prov.Token(mkCtx(t, aliceID()), "")
	if err == nil {
		t.Fatal("Token followed the token-endpoint redirect — the client_secret would be replayed")
	}
	if !errors.Is(err, tokenexchange.ErrTokenEndpointRedirect) {
		t.Fatalf("want ErrTokenEndpointRedirect, got %v", err)
	}
	if redirected {
		t.Fatal("the redirect target received a request — the credential form was replayed to it")
	}
}

// TestTokenExchange_SuppliedClientNotMutated proves the caller-supplied
// client is shallow-copied, not mutated in place: the caller's own instance
// keeps its (nil) CheckRedirect while the provider's copy refuses redirects.
func TestTokenExchange_SuppliedClientNotMutated(t *testing.T) {
	broker := newFakeBroker(t)
	deps, _, _ := mkDeps(t)
	supplied := &http.Client{Timeout: 5 * time.Second}
	deps.HTTPClient = supplied
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
	if _, err := tokenexchange.New(cfg, deps); err != nil {
		t.Fatalf("New: %v", err)
	}
	if supplied.CheckRedirect != nil {
		t.Fatal("caller's supplied client was mutated in place (CheckRedirect set) — it must be shallow-copied")
	}
}
