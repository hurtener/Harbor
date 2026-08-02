package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// TestResolveOAuthBinding_RefusesUnlistedDownstreamHost pins the D-300
// downstream-sink allow-list on the MCP southbound binding: a binding whose
// connection host is not in the bound provider's AllowedDownstreamHosts is
// refused (fails-without / passes-with), and an empty allow-list on a bound
// provider is refused fail-closed.
func TestResolveOAuthBinding_RefusesUnlistedDownstreamHost(t *testing.T) {
	listed := &stubOAuthProvider{token: "t", allowedHosts: []string{"graph.example.test"}}
	empty := &stubOAuthProvider{token: "t"} // no allow-list → fail-closed
	providers := map[string]auth.OAuthProvider{"listed": listed, "empty": empty}

	t.Run("unlisted host refused", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", URL: "https://evil.example.test", OAuthProvider: "listed",
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for unlisted downstream host, got %v", err)
		}
	})

	t.Run("listed host passes", func(t *testing.T) {
		got, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", URL: "https://graph.example.test", OAuthProvider: "listed",
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err != nil || got == nil {
			t.Fatalf("listed host must resolve, got (%v, %v)", got, err)
		}
	})

	t.Run("default-port equivalence", func(t *testing.T) {
		got, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", URL: "https://graph.example.test:443", OAuthProvider: "listed",
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err != nil || got == nil {
			t.Fatalf("host:443 must match the bare host (default-port equivalence), got (%v, %v)", got, err)
		}
	})

	t.Run("empty allow-list on bound provider refused fail-closed", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", URL: "https://graph.example.test", OAuthProvider: "empty",
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for empty allow-list on a bound provider, got %v", err)
		}
	})
}

// TestMCPBearerClient_RefusesRedirectToUnlistedHost pins WARN-D: the MCP
// bearer client (bearerInjectingTransport re-injects the exchanged bearer on
// every hop) must re-validate a redirect target against the bound provider's
// AllowedDownstreamHosts. An allow-listed host that 302s to an unlisted host
// is refused, so the exchanged bearer never reaches the redirect target.
func TestMCPBearerClient_RefusesRedirectToUnlistedHost(t *testing.T) {
	// The unlisted redirect target records whether it ever received a
	// request (it must not) and, if so, whether it carried the bearer.
	gotRequest := false
	gotBearer := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = true
		gotBearer = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	// The allow-listed server 302s to the unlisted target.
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/exfil", http.StatusFound)
	}))
	t.Cleanup(allowed.Close)

	allowedHost := config.NormalizeDownstreamHost(allowed.URL)
	stub := &stubOAuthProvider{token: "exchanged-bearer", allowedHosts: []string{allowedHost}}

	client := buildHTTPClient(Config{OAuthProvider: stub})
	if client.CheckRedirect == nil {
		t.Fatal("bearer client must install a CheckRedirect guard")
	}

	req, err := http.NewRequestWithContext(
		withBearer(context.Background(), "exchanged-bearer"),
		http.MethodGet, allowed.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("client followed a redirect to an unlisted host — the exchanged bearer would egress")
	}
	if !errors.Is(err, ErrRedirectToUnlistedHost) {
		// The error is wrapped in a *url.Error by net/http.
		var ue *url.Error
		if !errors.As(err, &ue) || !errors.Is(ue.Err, ErrRedirectToUnlistedHost) {
			t.Fatalf("want ErrRedirectToUnlistedHost, got %v", err)
		}
	}
	if gotRequest {
		t.Fatalf("unlisted redirect target received a request (bearer=%q) — the exchanged bearer egressed", gotBearer)
	}
}

type strictRedirectProvider struct{ *stubOAuthProvider }

func (strictRedirectProvider) RefuseRedirects() bool { return true }

func TestMCPBearerClient_SignedBindingRefusesEvenSameOriginRedirect(t *testing.T) {
	gotTarget := false
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/target", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		gotTarget = true
		w.WriteHeader(http.StatusOK)
	})
	provider := strictRedirectProvider{&stubOAuthProvider{
		token: "signed-bearer", allowedHosts: []string{config.NormalizeDownstreamHost(server.URL)},
	}}
	client := buildHTTPClient(Config{OAuthProvider: provider})
	req, err := http.NewRequestWithContext(withBearer(context.Background(), "signed-bearer"), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRedirectToUnlistedHost) {
		t.Fatalf("signed redirect was not refused: %v", err)
	}
	if gotTarget {
		t.Fatal("signed bearer followed a same-origin redirect")
	}
}
