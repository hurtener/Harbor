package auth

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
)

// cryptoRandRead is a tiny alias so randHex's caller doesn't need a
// direct crypto/rand import.
func cryptoRandRead(b []byte) (int, error) { return cryptorand.Read(b) }

// toolsToolSource is a tiny alias keeping `tools.ToolSourceID` reachable
// from this file even when imports are reorganised by goimports.
func toolsToolSource(s string) tools.ToolSourceID { return tools.ToolSourceID(s) }

// fakeAuthServer is a minimal in-test OAuth authorization server. It
// implements:
//   - GET /.well-known/oauth-authorization-server   (metadata discovery)
//   - POST /register                                (RFC 7591 dynamic registration)
//   - GET  /authorize                               (records the call; redirects back)
//   - POST /token                                   (PKCE-aware token exchange)
//
// It records every received state / verifier so tests can assert
// PKCE round-trip.
type fakeAuthServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	// codes maps issued auth codes to (state, code_challenge).
	codes map[string]struct {
		state         string
		codeChallenge string
	}
	// nextClientID is the ClientID returned by /register; defaulted
	// per test.
	nextClientID string
	// tokenIssuer mints a fresh access/refresh token pair.
	tokenIssuer func() (access, refresh string)
	// callCount tracks the number of /token POSTs.
	tokenCalls int
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{
		nextClientID: "dyn-client-id",
		tokenIssuer: func() (string, string) {
			return "access-token-" + randHex(8), "refresh-token-" + randHex(8)
		},
		codes: make(map[string]struct {
			state         string
			codeChallenge string
		}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleDiscovery)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuthServer) BaseURL() string { return f.srv.URL }

func (f *fakeAuthServer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	body := map[string]string{
		"authorization_endpoint": f.srv.URL + "/authorize",
		"token_endpoint":         f.srv.URL + "/token",
		"registration_endpoint":  f.srv.URL + "/register",
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	cid := f.nextClientID
	f.mu.Unlock()
	body := map[string]any{
		"client_id": cid,
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	state := r.URL.Query().Get("state")
	chall := r.URL.Query().Get("code_challenge")
	if state == "" || chall == "" {
		http.Error(w, "missing state/challenge", http.StatusBadRequest)
		return
	}
	code := "code-" + randHex(8)
	f.mu.Lock()
	f.codes[code] = struct {
		state         string
		codeChallenge string
	}{state: state, codeChallenge: chall}
	f.mu.Unlock()
	// Echo back a JSON envelope the test can parse (we do not run
	// real browser redirects here — the test calls CompleteFlow
	// directly with (state, code)).
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "state": state})
}

func (f *fakeAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.tokenCalls++
	f.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse", http.StatusBadRequest)
		return
	}
	grant := r.PostForm.Get("grant_type")
	switch grant {
	case "authorization_code":
		f.handleAuthCodeGrant(w, r)
	case "refresh_token":
		f.handleRefreshGrant(w, r)
	default:
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)
	}
}

func (f *fakeAuthServer) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		http.Error(w, "missing code/verifier", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	rec, ok := f.codes[code]
	if ok {
		delete(f.codes, code)
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "code unknown", http.StatusBadRequest)
		return
	}
	// PKCE verification: SHA256(verifier) must equal stored
	// code_challenge.
	expectedChallenge := pkceChallengeS256(verifier)
	if expectedChallenge != rec.codeChallenge {
		http.Error(w, "pkce verification failed", http.StatusBadRequest)
		return
	}
	access, refresh := f.tokenIssuer()
	body := map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         r.PostForm.Get("scope"),
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAuthServer) handleRefreshGrant(w http.ResponseWriter, r *http.Request) {
	rt := r.PostForm.Get("refresh_token")
	if rt == "" {
		http.Error(w, "missing refresh_token", http.StatusBadRequest)
		return
	}
	access, refresh := f.tokenIssuer()
	body := map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    3600,
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAuthServer) TokenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// IssuedCodeForState returns the most recent (code, ok) for the given
// state, simulating "user visited the authorize URL in their browser
// and the redirect handler captured the code." The test then calls
// CompleteFlow(state, code) directly.
func (f *fakeAuthServer) IssuedCodeForState(state string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for code, rec := range f.codes {
		if rec.state == state {
			return code, true
		}
	}
	return "", false
}

// VisitAuthorizeURL mimics a browser hitting the authorize URL: it
// drives the GET, the server records (state, code), the test fetches
// the code through IssuedCodeForState. Helper for the round-trip
// integration tests.
func (f *fakeAuthServer) VisitAuthorizeURL(authorizeURL string) (string, string, error) {
	resp, err := http.Get(authorizeURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("authorize: status %d", resp.StatusCode)
	}
	var out struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Code, out.State, nil
}

func randHex(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	buf := make([]byte, n)
	_, _ = cryptoRandRead(buf)
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0F]
	}
	return string(out)
}

// mkRedactor returns the production patterns-driver redactor.
func mkRedactor(t *testing.T) audit.Redactor {
	t.Helper()
	return patternsAudit.New()
}

// mkBus returns the production in-mem event bus, wired through the
// production redactor.
func mkBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     32,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}
	b, err := inmem.New(cfg, red)
	if err != nil {
		t.Fatalf("inmem events.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// mkCoordinator returns a process-local Coordinator (no StateStore
// checkpoint — tests don't need restart survival).
func mkCoordinator(t *testing.T) pauseresume.Coordinator {
	t.Helper()
	return pauseresume.New()
}

// providerHarness bundles the collaborators for a Provider under
// test. Builds the fake auth server, the in-mem TokenStore + bus +
// pause coordinator + redactor, and constructs the Provider with
// configs for both binding scopes.
type providerHarness struct {
	server      *fakeAuthServer
	store       TokenStore
	bus         events.EventBus
	coordinator pauseresume.Coordinator
	redactor    audit.Redactor
	provider    *Provider
	clock       func() time.Time
	userCfg     OAuthConfig
	agentCfg    OAuthConfig
}

func newProviderHarness(t *testing.T) *providerHarness {
	t.Helper()
	server := newFakeAuthServer(t)
	store, _ := mkTokenStore(t)
	red := mkRedactor(t)
	bus := mkBus(t, red)
	coord := mkCoordinator(t)
	now := time.Now

	userCfg := OAuthConfig{
		Source:       toolsToolSource("github-user"),
		SourceName:   "GitHub",
		BindingScope: ScopeUser,
		ServerURL:    server.BaseURL(),
		RedirectURI:  "http://localhost/callback",
		Scopes:       []string{"repo", "read:user"},
	}
	agentCfg := OAuthConfig{
		Source:       toolsToolSource("outlook-shared"),
		SourceName:   "Outlook (Shared)",
		BindingScope: ScopeAgent,
		AgentID:      tDummyAgent,
		ServerURL:    server.BaseURL(),
		RedirectURI:  "http://localhost/callback",
		Scopes:       []string{"mail.read", "mail.send"},
	}
	provider, err := NewProvider([]OAuthConfig{userCfg, agentCfg}, ProviderDeps{
		Store:       store,
		Bus:         bus,
		Redactor:    red,
		Coordinator: coord,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Clock:       now,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close(context.Background()) })

	return &providerHarness{
		server:      server,
		store:       store,
		bus:         bus,
		coordinator: coord,
		redactor:    red,
		provider:    provider,
		clock:       now,
		userCfg:     userCfg,
		agentCfg:    agentCfg,
	}
}

// drainEvent subscribes for one event of the given type then cancels.
// Returns the captured payload. Used to assert tool.auth_required /
// tool.auth_completed shapes end-to-end.
func drainEvent(t *testing.T, bus events.EventBus, filter events.Filter, deadline time.Duration) events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	sub, err := bus.Subscribe(ctx, filter)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("bus channel closed before event arrived")
		}
		sub.Cancel()
		return ev
	case <-ctx.Done():
		sub.Cancel()
		t.Fatalf("timed out waiting for event")
		return events.Event{}
	}
}

// formEncode is a tiny helper because url.Values{}.Encode is a bit
// verbose to inline.
func formEncode(kv map[string]string) string {
	v := url.Values{}
	for k, val := range kv {
		v.Set(k, val)
	}
	return v.Encode()
}

// stringSliceContains is a tiny helper for slice assertions.
func stringSliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// importMarker keeps `tools` import live in this file regardless of
// which tests in the package end up referencing the symbol — go's
// dead-import rule is a per-file concern.
var _ = strings.TrimSpace
