package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// loadFixture reads a committed spec-derived fixture (§17.8).
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "oauthdiscovery", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return b
}

// recordingHandler serves a body at a path and records every request it saw
// (path + headers) so tests can assert zero-non-metadata fetches + no creds.
type recordingHandler struct {
	mu       sync.Mutex
	requests []*http.Request
	routes   map[string][]byte
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.Clone(context.Background()))
	body, ok := h.routes[r.URL.Path]
	h.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (h *recordingHandler) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.requests))
	for _, r := range h.requests {
		out = append(out, r.URL.Path)
	}
	return out
}

const (
	prWellKnown = "/.well-known/oauth-protected-resource"
	asWellKnown = "/.well-known/oauth-authorization-server"
)

// newChainFixture wires a protected-resource server and an authorization-server
// server, rewriting the RFC 9728 doc's authorization_servers to point at the
// AS fixture (loopback). Returns both handlers + the DiscoveryInput.
func newChainFixture(t *testing.T) (*recordingHandler, *recordingHandler, func(), DiscoveryInput) {
	t.Helper()
	asHandler := &recordingHandler{routes: map[string][]byte{
		asWellKnown: loadFixture(t, "rfc8414_authorization_server.json"),
	}}
	asServer := httptest.NewServer(asHandler)

	// Rewrite the committed 9728 doc so its authorization_servers points at the
	// loopback AS fixture (the field NAMES stay the real spec names).
	var pr map[string]any
	if err := json.Unmarshal(loadFixture(t, "rfc9728_protected_resource.json"), &pr); err != nil {
		t.Fatalf("unmarshal 9728: %v", err)
	}
	pr["authorization_servers"] = []string{asServer.URL}
	prBody, _ := json.Marshal(pr)

	prHandler := &recordingHandler{routes: map[string][]byte{prWellKnown: prBody}}
	prServer := httptest.NewServer(prHandler)

	cleanup := func() {
		prServer.Close()
		asServer.Close()
	}
	in := DiscoveryInput{
		ServerURL:           prServer.URL,
		ResourceMetadataURL: prServer.URL + prWellKnown,
		Source:              "probe",
		AllowedOrigins:      []string{asServer.URL},
	}
	return prHandler, asHandler, cleanup, in
}

func TestDiscoverer_Discover_FullChain_Succeeds(t *testing.T) {
	prH, asH, cleanup, in := newChainFixture(t)
	defer cleanup()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), in)

	if req.Source != "probe" {
		t.Fatalf("source = %q, want probe", req.Source)
	}
	if len(req.AuthorizationServers) != 1 {
		t.Fatalf("authorization_servers = %d, want 1; statuses=%+v", len(req.AuthorizationServers), req.Status)
	}
	as := req.AuthorizationServers[0]
	if as.Issuer != "https://server.example.com" {
		t.Errorf("issuer = %q", as.Issuer)
	}
	if as.AuthorizationEndpoint != "https://server.example.com/authorize" {
		t.Errorf("authorization_endpoint = %q", as.AuthorizationEndpoint)
	}
	if as.TokenEndpoint != "https://server.example.com/token" {
		t.Errorf("token_endpoint = %q", as.TokenEndpoint)
	}
	if as.RegistrationEndpoint != "https://server.example.com/register" {
		t.Errorf("registration_endpoint = %q (reported, never invoked)", as.RegistrationEndpoint)
	}
	if len(as.ScopesSupported) == 0 {
		t.Errorf("scopes_supported empty, want the RFC 8414 example scopes")
	}
	// Every hop OK.
	for _, st := range req.Status {
		if !st.OK {
			t.Errorf("hop %s target %s not OK: %s/%s", st.Step, st.Target, st.Reason, st.Detail)
		}
	}
	// Zero non-metadata fetches: only the well-known paths were hit; the token
	// endpoint / register endpoint were NEVER dialed (D-297 custody boundary).
	for _, p := range append(prH.paths(), asH.paths()...) {
		if p != prWellKnown && p != asWellKnown {
			t.Errorf("unexpected non-metadata fetch to %q — discovery must never dial beyond the metadata chain", p)
		}
	}
	// No credentials on any discovery fetch.
	assertNoCredentials(t, prH)
	assertNoCredentials(t, asH)
}

func assertNoCredentials(t *testing.T, h *recordingHandler) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.requests {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("discovery fetch to %q carried an Authorization header", r.URL.Path)
		}
		if r.Header.Get("Cookie") != "" {
			t.Errorf("discovery fetch to %q carried a Cookie header", r.URL.Path)
		}
	}
}

// The §17.8 right-field / wrong-field discriminator: a parser wired to the
// wrong field name must FAIL the correct fixture. We prove the walker reads
// the real `authorization_servers` field by mutating it and asserting the
// chain no longer resolves an AS.
func TestDiscoverer_Discover_WrongFieldMutation_FailsDiscriminator(t *testing.T) {
	// Correct field: the walk resolves the AS list.
	var correct protectedResourceMetadata
	if err := json.Unmarshal(loadFixture(t, "rfc9728_protected_resource.json"), &correct); err != nil {
		t.Fatalf("unmarshal correct: %v", err)
	}
	if len(correct.AuthorizationServers) == 0 {
		t.Fatalf("correct fixture parsed no authorization_servers — fixture or parser is wrong")
	}
	// Wrong field: rename authorization_servers → authorization_server.
	mutated := bytes.Replace(
		loadFixture(t, "rfc9728_protected_resource.json"),
		[]byte(`"authorization_servers"`),
		[]byte(`"authorization_server"`),
		1,
	)
	var wrong protectedResourceMetadata
	if err := json.Unmarshal(mutated, &wrong); err != nil {
		t.Fatalf("unmarshal mutated: %v", err)
	}
	if len(wrong.AuthorizationServers) != 0 {
		t.Fatalf("wrong-field mutation still parsed authorization_servers — the parser is NOT pinned to the real field name")
	}
}

func TestDiscoverer_Discover_PKCEMetadata_Parsed(t *testing.T) {
	asHandler := &recordingHandler{routes: map[string][]byte{
		asWellKnown: loadFixture(t, "rfc8414_authorization_server_pkce.json"),
	}}
	asServer := httptest.NewServer(asHandler)
	defer asServer.Close()

	var pr map[string]any
	_ = json.Unmarshal(loadFixture(t, "rfc9728_protected_resource.json"), &pr)
	pr["authorization_servers"] = []string{asServer.URL}
	prBody, _ := json.Marshal(pr)
	prHandler := &recordingHandler{routes: map[string][]byte{prWellKnown: prBody}}
	prServer := httptest.NewServer(prHandler)
	defer prServer.Close()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           prServer.URL,
		ResourceMetadataURL: prServer.URL + prWellKnown,
		Source:              "probe",
		AllowedOrigins:      []string{asServer.URL},
	})
	if len(req.AuthorizationServers) != 1 {
		t.Fatalf("want 1 AS, got %d", len(req.AuthorizationServers))
	}
	pkce := req.AuthorizationServers[0].CodeChallengeMethodsSupported
	if len(pkce) != 1 || pkce[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want [S256]", pkce)
	}
}

// SSRF guardrail table — validateHop pure-function refusals (production strict).
func TestDiscoverer_ValidateHop_SSRFGuardrails(t *testing.T) {
	d := NewDiscoverer() // strict: no private-network bypass
	const serverOrigin = "https://mcp.example.com"
	allowed := map[string]bool{"https://as.example.net": true, "http://plain.example.net": true}

	cases := []struct {
		name       string
		target     string
		step       discoveryStep
		wantReason string
	}{
		{"9728 same-origin ok", serverOrigin + prWellKnown, StepProtectedResource, ""},
		{"9728 cross-origin refused", "https://evil.example.org" + prWellKnown, StepProtectedResource, ReasonCrossOriginNotAllowed},
		{"8414 needs allowance", "https://unlisted.example.net" + asWellKnown, StepAuthorizationServer, ReasonNeedsAllowance},
		{"8414 allowed origin passes", "https://as.example.net" + asWellKnown, StepAuthorizationServer, ""},
		{"8414 ip-literal not-allowed refused", "https://93.184.216.34" + asWellKnown, StepAuthorizationServer, ReasonNeedsAllowance}, // not in allowed → needs_allowance first
		{"https-only off loopback (allowed http origin)", "http://plain.example.net" + asWellKnown, StepAuthorizationServer, ReasonNotHTTPS},
		{"invalid url", "://nope", StepProtectedResource, ReasonInvalidURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, _, ok := d.validateHop(c.target, c.step, serverOrigin, allowed)
			if c.wantReason == "" {
				if !ok {
					t.Fatalf("want pass, got refusal %q", reason)
				}
				return
			}
			if ok {
				t.Fatalf("want refusal %q, got pass", c.wantReason)
			}
			if reason != c.wantReason {
				t.Fatalf("reason = %q, want %q", reason, c.wantReason)
			}
		})
	}
}

// An IP-literal AS origin that IS explicitly allowed is still refused for
// being an IP literal (the allowance names a public origin, never a hole).
func TestDiscoverer_ValidateHop_AllowedIPLiteral_StillRefused(t *testing.T) {
	d := NewDiscoverer()
	allowed := map[string]bool{"https://93.184.216.34": true}
	reason, _, ok := d.validateHop("https://93.184.216.34"+asWellKnown, StepAuthorizationServer, "https://mcp.example.com", allowed)
	if ok || reason != ReasonPrivateOrIPLiteral {
		t.Fatalf("want %q refusal, got ok=%v reason=%q", ReasonPrivateOrIPLiteral, ok, reason)
	}
}

// The AS hop with no allowance surfaces PARTIALLY: the 9728 half resolves, the
// AS half reports needs_allowance — never a silent empty.
func TestDiscoverer_Discover_NoAllowance_SurfacesPartial(t *testing.T) {
	_, _, cleanup, in := newChainFixture(t)
	defer cleanup()
	in.AllowedOrigins = nil // no cross-origin allowance for the AS hop

	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), in)

	if len(req.AuthorizationServers) != 0 {
		t.Fatalf("AS resolved without an allowance: %+v", req.AuthorizationServers)
	}
	var sawNeedsAllowance, sawPR bool
	for _, st := range req.Status {
		if st.Step == StepProtectedResource && st.OK {
			sawPR = true
		}
		if st.Step == StepAuthorizationServer && !st.OK && st.Reason == ReasonNeedsAllowance {
			sawNeedsAllowance = true
		}
	}
	if !sawPR {
		t.Errorf("protected-resource hop should have succeeded")
	}
	if !sawNeedsAllowance {
		t.Errorf("AS hop should report needs_allowance; statuses=%+v", req.Status)
	}
}

func TestDiscoverer_Discover_SizeCapEnforced(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 64<<10)
	prBody := []byte(`{"resource":"x","authorization_servers":["https://as.example.net"],"pad":"` + string(big) + `"}`)
	prHandler := &recordingHandler{routes: map[string][]byte{prWellKnown: prBody}}
	prServer := httptest.NewServer(prHandler)
	defer prServer.Close()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest(), WithDiscoveryMaxBodyBytes(4<<10))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           prServer.URL,
		ResourceMetadataURL: prServer.URL + prWellKnown,
		Source:              "probe",
	})
	if len(req.Status) == 0 || req.Status[0].OK {
		t.Fatalf("oversized body should fail loud; statuses=%+v", req.Status)
	}
	if req.Status[0].Reason != ReasonFetchFailed {
		t.Errorf("reason = %q, want %q", req.Status[0].Reason, ReasonFetchFailed)
	}
}

func TestDiscoverer_Discover_RedirectBoundEnforced(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+prWellKnown+"?n=1", http.StatusFound)
	}))
	defer srv.Close()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
	})
	if len(req.Status) == 0 || req.Status[0].OK {
		t.Fatalf("redirect loop should fail loud; statuses=%+v", req.Status)
	}
	if req.Status[0].Reason != ReasonFetchFailed {
		t.Errorf("reason = %q, want %q", req.Status[0].Reason, ReasonFetchFailed)
	}
}

func TestDiscoverer_Discover_TimeoutEnforced(t *testing.T) {
	prHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(prHandler)
	defer srv.Close()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest(), WithDiscoveryTimeout(20*time.Millisecond))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
	})
	if len(req.Status) == 0 || req.Status[0].OK {
		t.Fatalf("slow server should time out loud; statuses=%+v", req.Status)
	}
}

func TestDiscoverer_Discover_InvalidServerURL_FailsLoud(t *testing.T) {
	d := NewDiscoverer()
	req := d.Discover(context.Background(), DiscoveryInput{ServerURL: "not a url", Source: "probe"})
	if len(req.Status) != 1 || req.Status[0].Reason != ReasonInvalidURL {
		t.Fatalf("want single invalid_url status, got %+v", req.Status)
	}
}

// Concurrent-reuse contract (D-025): one Discoverer, N concurrent walks, -race.
func TestDiscoverer_Discover_ConcurrentReuse(t *testing.T) {
	prH, _, cleanup, in := newChainFixture(t)
	_ = prH
	defer cleanup()
	d := NewDiscoverer(WithPrivateNetworkAccessForTest())

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			req := d.Discover(context.Background(), in)
			if len(req.AuthorizationServers) != 1 {
				t.Errorf("concurrent walk resolved %d AS, want 1", len(req.AuthorizationServers))
			}
		}()
	}
	wg.Wait()
}

// The DNS-rebinding defence: a HOSTNAME that RESOLVES to a private/loopback
// address must be refused at dial time. This is the load-bearing case — the
// hostname (`localhost`) is not an IP literal (net.ParseIP returns nil for it),
// so a pre-resolution guard would let it through; only a post-resolution
// net.Dialer.Control check (seeing the resolved 127.0.0.1 / ::1) catches it.
//
// Fail-without / pass-with: against a pre-resolution DialContext guard this
// test FAILS (the `localhost` name skips the ParseIP check and the fetch
// succeeds); against the Control-based guard it PASSES (the resolved loopback
// IP is refused). Verified both directions during implementation.
func TestDiscoverer_Discover_DialTimeHostnameToPrivateRefusal(t *testing.T) {
	// httptest binds 127.0.0.1; we reach it via the `localhost` HOSTNAME so the
	// guard must resolve the name to a private IP to catch it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`))
	}))
	defer srv.Close()

	if net.ParseIP("localhost") != nil {
		t.Fatal("precondition: `localhost` must be a hostname, not an IP literal")
	}
	// Rewrite the IP-literal httptest URL to the `localhost` hostname form.
	hostnameURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	// Strict discoverer (no WithPrivateNetworkAccessForTest): the dial to the
	// resolved loopback address is refused by the post-resolution Control guard.
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           hostnameURL,
		ResourceMetadataURL: hostnameURL + prWellKnown,
		Source:              "probe",
	})
	if len(req.Status) == 0 || req.Status[0].OK {
		t.Fatalf("hostname-resolving-to-private dial should be refused; statuses=%+v", req.Status)
	}
	if req.Status[0].Reason != ReasonFetchFailed {
		t.Errorf("reason = %q, want %q", req.Status[0].Reason, ReasonFetchFailed)
	}
}

// Defence-in-depth: an IP-literal loopback dial is also refused (this path was
// already caught pre-fix by the literal check; kept as a regression guard).
func TestDiscoverer_Discover_DialTimeIPLiteralPrivateRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`))
	}))
	defer srv.Close()

	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL, // http://127.0.0.1:PORT
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
	})
	if len(req.Status) == 0 || req.Status[0].OK {
		t.Fatalf("loopback IP-literal dial should be refused; statuses=%+v", req.Status)
	}
	if req.Status[0].Reason != ReasonFetchFailed {
		t.Errorf("reason = %q, want %q", req.Status[0].Reason, ReasonFetchFailed)
	}
}

func TestDiscoverer_WithDiscoveryClock(t *testing.T) {
	fixed := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	d := NewDiscoverer(WithDiscoveryClock(func() time.Time { return fixed }))
	req := d.Discover(context.Background(), DiscoveryInput{ServerURL: "https://mcp.example.com", Source: "challenge"})
	if !req.DiscoveredAt.Equal(fixed) {
		t.Fatalf("discovered_at = %v, want %v", req.DiscoveredAt, fixed)
	}
	if req.Source != "challenge" {
		t.Errorf("source = %q, want challenge", req.Source)
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":     true,
		"10.0.0.1":      true,
		"192.168.1.1":   true,
		"169.254.0.1":   true,
		"::1":           true,
		"8.8.8.8":       false,
		"93.184.216.34": false,
	}
	for host, want := range cases {
		ip := net.ParseIP(host)
		if ip == nil {
			t.Fatalf("parse %q", host)
		}
		if got := isPrivateIP(ip); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", host, got, want)
		}
	}
}

// The aggregate walk budget: a 9728 doc packing more than maxAuthServers
// entries is truncated (reported, not silently dropped), bounding total time.
func TestDiscoverer_Discover_AuthServerCountCapped(t *testing.T) {
	asServer := httptest.NewServer(&recordingHandler{routes: map[string][]byte{
		asWellKnown: loadFixture(t, "rfc8414_authorization_server.json"),
	}})
	defer asServer.Close()

	// Advertise many more AS entries than the cap (all pointing at the one AS
	// fixture so each resolves; the cap must still truncate the walk).
	list := make([]string, 0, maxAuthServers+5)
	for range maxAuthServers + 5 {
		list = append(list, asServer.URL)
	}
	prBody, _ := json.Marshal(map[string]any{"resource": "x", "authorization_servers": list})
	prServer := httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
	defer prServer.Close()

	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           prServer.URL,
		ResourceMetadataURL: prServer.URL + prWellKnown,
		Source:              "probe",
		AllowedOrigins:      []string{asServer.URL},
	})
	if len(req.AuthorizationServers) > maxAuthServers {
		t.Fatalf("walked %d authorization servers, cap is %d", len(req.AuthorizationServers), maxAuthServers)
	}
	var sawCap bool
	for _, st := range req.Status {
		if st.Reason == ReasonTooManyAuthServers {
			sawCap = true
		}
	}
	if !sawCap {
		t.Errorf("truncation should be reported with reason %q; statuses=%+v", ReasonTooManyAuthServers, req.Status)
	}
}

func TestAuthServerMetadataURL(t *testing.T) {
	cases := map[string]string{
		"https://as.example.com":        "https://as.example.com/.well-known/oauth-authorization-server",
		"https://as.example.com/":       "https://as.example.com/.well-known/oauth-authorization-server",
		"https://as.example.com/tenant": "https://as.example.com/.well-known/oauth-authorization-server/tenant",
	}
	for in, want := range cases {
		if got := authServerMetadataURL(in); got != want {
			t.Errorf("authServerMetadataURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverer_WellKnownFallback_WhenNoChallengeURL(t *testing.T) {
	// No ResourceMetadataURL → falls back to {server}/.well-known/oauth-protected-resource.
	_, _, cleanup, in := newChainFixture(t)
	defer cleanup()
	in.ResourceMetadataURL = ""
	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	req := d.Discover(context.Background(), in)
	if !strings.HasSuffix(req.ResourceMetadataURL, prWellKnown) {
		t.Fatalf("fallback resource_metadata_url = %q", req.ResourceMetadataURL)
	}
	if len(req.AuthorizationServers) != 1 {
		t.Fatalf("fallback walk failed; statuses=%+v", req.Status)
	}
}
