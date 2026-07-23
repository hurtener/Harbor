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

// privateTokenURL is an RFC1918 endpoint nothing listens on — the dial
// guard's decision (refuse vs. permit) is observable without a real server:
// a refusal returns ErrPrivateDialRefused at Control time; a permit lets the
// dial proceed (bounded by the caller ctx below), yielding any error EXCEPT
// ErrPrivateDialRefused.
const privateTokenURL = "http://10.255.255.1:9/oauth2/token"

// exchangeAgainstPrivate builds a tokenexchange provider whose token_url is a
// private-range endpoint and drives one Token() against it under a bounded
// context, returning the resulting error. deps.HTTPClient is forced nil so
// the production, fully-hardened dial-control client is exercised (a supplied
// client would take the redirect-only path with no dial guard).
func exchangeAgainstPrivate(t *testing.T, cfg auth.ProviderConfig) error {
	t.Helper()
	deps, _, _ := mkDeps(t)
	deps.HTTPClient = nil // force the hardened dial-control client
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	// A bounded caller context caps the dial when the guard PERMITS the
	// private address (the endpoint is unreachable). A REFUSAL short-circuits
	// at Control time, well within the bound. Not a synchronization sleep —
	// the assertion is on the error identity, not on timing.
	ctx, cancel := context.WithTimeout(mkCtx(t, aliceID()), 400*time.Millisecond)
	defer cancel()
	_, err = prov.Token(ctx, "")
	return err
}

func basePrivateCfg() auth.ProviderConfig {
	return auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         privateTokenURL,
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
}

// TestTokenExchange_PrivateDial_NeitherRefuses pins the fail-closed default:
// with neither the per-provider config flag nor the boot env set, the
// credential POST to a private endpoint is refused at dial time.
func TestTokenExchange_PrivateDial_NeitherRefuses(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(false)
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	cfg := basePrivateCfg() // AllowPrivateTokenURL defaults false
	err := exchangeAgainstPrivate(t, cfg)
	if !errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("neither surface set: want ErrPrivateDialRefused, got %v", err)
	}
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("want the refusal wrapped as ErrExchangeFailed, got %v", err)
	}
}

// TestTokenExchange_PrivateDial_ConfigFlagPermits proves the per-provider
// config flag alone (boot env OFF) relaxes the guard for THIS provider.
func TestTokenExchange_PrivateDial_ConfigFlagPermits(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(false) // env surface OFF
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	cfg := basePrivateCfg()
	cfg.AllowPrivateTokenURL = true
	err := exchangeAgainstPrivate(t, cfg)
	if errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("config flag set: the private dial was still refused (%v) — the flag did not relax the guard", err)
	}
	if err == nil {
		t.Fatal("expected a dial/connect error against the unreachable private endpoint, got nil")
	}
}

// TestTokenExchange_PrivateDial_EnvCapturePermits proves the global boot-env
// capture alone (config flag OFF) relaxes the guard — the second surface of
// the same opt-in.
func TestTokenExchange_PrivateDial_EnvCapturePermits(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(true) // env surface ON
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	cfg := basePrivateCfg() // config flag OFF
	err := exchangeAgainstPrivate(t, cfg)
	if errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("boot-env captured: the private dial was still refused (%v) — the env capture did not relax the guard", err)
	}
	if err == nil {
		t.Fatal("expected a dial/connect error against the unreachable private endpoint, got nil")
	}
}

// TestTokenExchange_PrivateDial_Scoping proves the relaxation is per-provider:
// with the boot env OFF, a provider built WITH the flag permits the private
// dial while a sibling WITHOUT the flag still refuses it — the flag is scoped
// to its own provider's dialer, not global.
func TestTokenExchange_PrivateDial_Scoping(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(false) // env surface OFF
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	permissive := basePrivateCfg()
	permissive.Name = "broker-permissive"
	permissive.AllowPrivateTokenURL = true
	if err := exchangeAgainstPrivate(t, permissive); errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("flagged provider still refused the private dial: %v", err)
	}

	strict := basePrivateCfg()
	strict.Name = "broker-strict"
	// AllowPrivateTokenURL defaults false.
	if err := exchangeAgainstPrivate(t, strict); !errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("sibling provider without the flag must still refuse: want ErrPrivateDialRefused, got %v", err)
	}
}

// TestTokenExchange_PrivateDial_OptInStillRefusesUnspecified proves the
// unspecified-address block is unconditional: even with the opt-in enabled,
// a token_url resolving to 0.0.0.0 is refused (never a valid coordinator).
func TestTokenExchange_PrivateDial_OptInStillRefusesUnspecified(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(false)
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	cfg := basePrivateCfg()
	cfg.AllowPrivateTokenURL = true
	cfg.TokenURL = "http://0.0.0.0:9/oauth2/token"
	err := exchangeAgainstPrivate(t, cfg)
	if !errors.Is(err, tokenexchange.ErrPrivateDialRefused) {
		t.Fatalf("opt-in must still refuse the unspecified address: want ErrPrivateDialRefused, got %v", err)
	}
}

// TestTokenExchange_PrivateDial_OptInPreservesLoopbackAndRedirectRefusal
// proves the opt-in leaves the loopback carve-out and the redirect refusal
// intact: with the flag set and the hardened (nil-supplied) client, a
// loopback broker that attempts a redirect is still refused with
// ErrTokenEndpointRedirect (the credential form is never replayed).
func TestTokenExchange_PrivateDial_OptInPreservesLoopbackAndRedirectRefusal(t *testing.T) {
	tokenexchange.RegisterAllowPrivateExchangeCaptured(false)
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true // must NEVER fire
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(broker.Close)

	deps, _, _ := mkDeps(t)
	deps.HTTPClient = nil // hardened dial-control client (loopback allowed, redirect refused)
	cfg := auth.ProviderConfig{
		Name:                 tProviderName,
		CredentialSource:     credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:               []string{"Mail.Read"},
		TokenURL:             broker.URL + "/oauth2/token", // httptest binds loopback 127.0.0.1
		AllowPrivateTokenURL: true,
		Extra:                map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(mkCtx(t, aliceID()), 5*time.Second)
	defer cancel()
	_, err = prov.Token(ctx, "")
	if !errors.Is(err, tokenexchange.ErrTokenEndpointRedirect) {
		t.Fatalf("opt-in must not disturb the redirect refusal: want ErrTokenEndpointRedirect, got %v", err)
	}
	if redirected {
		t.Fatal("the redirect target received a request — the credential form was replayed under the opt-in")
	}
}
