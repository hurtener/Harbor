package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// prStatus returns the protected_resource step status (or nil).
func prStatus(req OAuthRequirement) *DiscoveryStepStatus {
	for i := range req.Status {
		if req.Status[i].Step == StepProtectedResource {
			return &req.Status[i]
		}
	}
	return nil
}

// asStatus returns the first authorization_server step status (or nil).
func asStatus(req OAuthRequirement) *DiscoveryStepStatus {
	for i := range req.Status {
		if req.Status[i].Step == StepAuthorizationServer {
			return &req.Status[i]
		}
	}
	return nil
}

// The pinned-relaxation POSITIVE: on the PRODUCTION construction path (NO
// WithPrivateNetworkAccessForTest) the same-origin RFC 9728 protected-resource
// hop to the operator-declared ServerURL COMPLETES when that server resolves to
// a private/loopback address — pinned to pinnedIPs + pinnedPort. Proven for all
// THREE self-hosted postures (the anti-rubber-stamp guard). These flip the two
// former bug-asserting refusals (hostname→loopback, IP-literal→loopback), whose
// negative role now lives in the rebind / different-port / cross-origin tests.
func TestDiscoverer_SameOriginPrivateResourceHop_CompletesOnProductionPath(t *testing.T) {
	// A same-origin PR fixture that advertises a (public-looking) AS so the walk
	// proceeds past the PR hop — the AS hop's own gate is not under test here.
	prBody := []byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`)

	t.Run("loopback_hostname", func(t *testing.T) {
		srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
		defer srv.Close()
		if net.ParseIP("localhost") != nil {
			t.Fatal("precondition: `localhost` must be a hostname, not an IP literal")
		}
		hostnameURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
		d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second)) // strict, production path
		req := d.Discover(context.Background(), DiscoveryInput{
			ServerURL:           hostnameURL,
			ResourceMetadataURL: hostnameURL + prWellKnown,
			Source:              "probe",
		})
		if st := prStatus(req); st == nil || !st.OK {
			t.Fatalf("same-origin loopback-hostname PR hop should complete on the production path; statuses=%+v", req.Status)
		}
	})

	t.Run("ip_literal", func(t *testing.T) {
		srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
		defer srv.Close()
		d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second)) // strict, production path
		req := d.Discover(context.Background(), DiscoveryInput{
			ServerURL:           srv.URL, // http://127.0.0.1:PORT — canonical compose/localhost form
			ResourceMetadataURL: srv.URL + prWellKnown,
			Source:              "probe",
		})
		if st := prStatus(req); st == nil || !st.OK {
			t.Fatalf("same-origin IP-literal PR hop should complete on the production path; statuses=%+v", req.Status)
		}
	})

	t.Run("plain_http_nonloopback_service_name", func(t *testing.T) {
		// The compose/k8s posture: a plain-HTTP NON-loopback service name that
		// resolves to the loopback fixture — proves the WARN-1 https-off-loopback
		// extension AND the dial pin compose on a real walk. A trailing-dot FQDN
		// suppresses search-domain expansion so the fake resolver sees exactly one
		// A query per lookup.
		srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
		defer srv.Close()
		port := portOf(t, srv.URL)
		svcURL := "http://mcp.svc.:" + port // non-loopback name, plain HTTP
		res := newFakeResolver(t, "127.0.0.1")
		d := NewDiscoverer(WithDiscoveryTimeout(2*time.Second), WithResolverForTest(res))
		req := d.Discover(context.Background(), DiscoveryInput{
			ServerURL:           svcURL,
			ResourceMetadataURL: svcURL + prWellKnown,
			Source:              "probe",
		})
		if st := prStatus(req); st == nil || !st.OK {
			t.Fatalf("same-origin plain-HTTP non-loopback PR hop should complete on the production path; statuses=%+v", req.Status)
		}
	})
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

// ---------------------------------------------------------------------------
// Same-origin dial-pin (HA-19) — the security-sensitive relaxation tests.
// ---------------------------------------------------------------------------

// portOf extracts the port from a URL (test helper).
func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Port()
}

// fakeDNS is a minimal in-test DNS server: it answers every A query with the
// next IPv4 in aSeq (repeating the last once exhausted) and every AAAA query
// with NODATA. It maps a service name to a chosen address on the PRODUCTION
// dial path without touching the OS resolver — and, with a two-element aSeq,
// models a DNS rebind (the name resolves to IP #1 at pin time and IP #2 at dial
// time). It relaxes NO SSRF guardrail; the dial pin still governs every dial.
type fakeDNS struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	aSeq  []string
	aUsed int
}

func newFakeResolver(t *testing.T, aSeq ...string) *net.Resolver {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	f := &fakeDNS{conn: pc, aSeq: aSeq}
	go f.serve()
	t.Cleanup(func() { _ = pc.Close() })
	dnsAddr := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", dnsAddr)
		},
	}
}

func (f *fakeDNS) serve() {
	buf := make([]byte, 512)
	for {
		n, addr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return // conn closed by t.Cleanup
		}
		if resp := f.respond(buf[:n]); resp != nil {
			_, _ = f.conn.WriteToUDP(resp, addr)
		}
	}
}

func (f *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	// Walk the QNAME labels to find the QTYPE.
	off := 12
	for off < len(q) {
		l := int(q[off])
		if l == 0 {
			off++
			break
		}
		off += l + 1
	}
	if off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	questionEnd := off + 4 // QTYPE(2) + QCLASS(2)

	resp := make([]byte, 0, 64)
	resp = append(resp, q[0:2]...)  // ID (echo)
	resp = append(resp, 0x81, 0x80) // flags: QR, RD, RA, RCODE 0
	resp = append(resp, 0x00, 0x01) // QDCOUNT 1
	ancountIdx := len(resp)
	resp = append(resp, 0x00, 0x00)           // ANCOUNT (patched below)
	resp = append(resp, 0x00, 0x00)           // NSCOUNT
	resp = append(resp, 0x00, 0x00)           // ARCOUNT
	resp = append(resp, q[12:questionEnd]...) // echo question

	var ancount uint16
	if qtype == 1 { // A
		f.mu.Lock()
		var ipStr string
		if len(f.aSeq) > 0 {
			i := f.aUsed
			if i >= len(f.aSeq) {
				i = len(f.aSeq) - 1
			}
			ipStr = f.aSeq[i]
			f.aUsed++
		}
		f.mu.Unlock()
		if ip := net.ParseIP(ipStr).To4(); ip != nil {
			resp = append(resp, 0xC0, 0x0C)             // NAME pointer → offset 12
			resp = append(resp, 0x00, 0x01)             // TYPE A
			resp = append(resp, 0x00, 0x01)             // CLASS IN
			resp = append(resp, 0x00, 0x00, 0x00, 0x1E) // TTL 30
			resp = append(resp, 0x00, 0x04)             // RDLENGTH 4
			resp = append(resp, ip...)                  // RDATA
			ancount = 1
		}
	}
	binary.BigEndian.PutUint16(resp[ancountIdx:], ancount)
	return resp
}

// ip parses an IPv4 test literal.
func ip(s string) net.IP { return net.ParseIP(s) }

// The dial-gate decision table — the CORE security logic, proven exhaustively
// without DNS. It is the one function ControlContext calls post-resolution.
func TestEvalDialGate_Table(t *testing.T) {
	pinPR := &dialPin{step: StepProtectedResource, sameOrigin: true, pinnedIPs: []net.IP{ip("127.0.0.1"), ip("10.1.2.3")}, pinnedPort: "8080"}
	cases := []struct {
		name         string
		host, port   string
		allowPrivate bool
		pin          *dialPin
		wantPermit   bool
	}{
		{"public IP always permitted", "93.184.216.34", "443", false, pinPR, true},
		{"public IP permitted even with nil pin", "8.8.8.8", "443", false, nil, true},
		{"same-origin PR private in-set + matching port permitted", "127.0.0.1", "8080", false, pinPR, true},
		{"same-origin PR private second-in-set IP permitted", "10.1.2.3", "8080", false, pinPR, true},
		{"same-origin PR private in-set but WRONG port refused", "127.0.0.1", "22", false, pinPR, false},
		{"same-origin PR private OUT-of-set refused", "10.9.9.9", "8080", false, pinPR, false},
		{"cross-origin private refused", "127.0.0.1", "8080", false, &dialPin{step: StepProtectedResource, sameOrigin: false, pinnedIPs: []net.IP{ip("127.0.0.1")}, pinnedPort: "8080"}, false},
		{"AS-step private refused even when IP in-set + port match", "127.0.0.1", "8080", false, &dialPin{step: StepAuthorizationServer, sameOrigin: true, pinnedIPs: []net.IP{ip("127.0.0.1")}, pinnedPort: "8080"}, false},
		{"missing pin fails closed on private", "127.0.0.1", "8080", false, nil, false},
		{"allowPrivate short-circuits (test-only)", "127.0.0.1", "22", true, nil, true},
		{"non-IP resolved host refused fail-closed", "not-an-ip", "8080", false, pinPR, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := evalDialGate(c.host, c.port, c.allowPrivate, c.pin)
			if c.wantPermit && err != nil {
				t.Fatalf("want permit, got refusal: %v", err)
			}
			if !c.wantPermit && err == nil {
				t.Fatalf("want refusal, got permit")
			}
		})
	}
}

// The dial IP:port step-gate: an authorization-server hop whose target IP is IN
// pinnedIPs (attacker sets authorization_servers[] to the operator's OWN origin)
// is STILL refused — the relaxation is gated by step == StepProtectedResource,
// not merely by origin-difference.
func TestDiscoverer_AuthServerHopToPinnedIP_StillRefused(t *testing.T) {
	pin := &dialPin{step: StepAuthorizationServer, sameOrigin: true, pinnedIPs: []net.IP{ip("127.0.0.1")}, pinnedPort: "8080"}
	if err := evalDialGate("127.0.0.1", "8080", false, pin); err == nil {
		t.Fatal("AS-step dial to the pinned IP:port must be refused (step != protected_resource)")
	}
}

// Intentional widening (recorded, not a leak): the string-layer https extension
// is same-origin-only and the dial pin governs only PRIVATE dials (a public IP
// passes evalDialGate unconditionally), so a same-origin plain-HTTP protected-
// resource hop to a PUBLIC operator-declared server now COMPLETES where it was
// previously ReasonNotHTTPS. Proven at BOTH gate layers (string + dial) — the
// two gates a walk composes — without network egress: safe because discovery
// carries zero credentials, the response is report-only (D-297), and any
// advertised AS still halts at needs_allowance.
func TestDiscoverer_SameOriginPublicPlainHTTP_Completes(t *testing.T) {
	d := NewDiscoverer()
	const serverOrigin = "http://public.example.com" // public, plain-HTTP, same-origin

	// String layer: no ReasonNotHTTPS for the same-origin protected-resource hop.
	if reason, _, ok := d.validateHop(serverOrigin+prWellKnown, StepProtectedResource, serverOrigin, map[string]bool{}); !ok {
		t.Fatalf("same-origin public plain-HTTP PR hop should pass the string layer, got %q", reason)
	}
	// Dial layer: a public resolved IP passes unconditionally (the pin governs
	// only private dials) — even for the protected-resource pin, and even nil.
	prPin := &dialPin{step: StepProtectedResource, sameOrigin: true, pinnedIPs: []net.IP{ip("93.184.216.34")}, pinnedPort: "80"}
	if err := evalDialGate("93.184.216.34", "80", false, prPin); err != nil {
		t.Fatalf("public IP dial for the same-origin PR hop should be permitted, got %v", err)
	}
	if err := evalDialGate("93.184.216.34", "80", false, nil); err != nil {
		t.Fatalf("public IP dial should be permitted even with no pin, got %v", err)
	}
}

// The WARN-1 https extension is PR-step-only at the STRING layer: a same-origin
// protected-resource plain-HTTP hop to the pinned target passes NotHTTPS...
func TestDiscoverer_ValidateHop_SameOriginPlainHTTPProtectedResource_Passes(t *testing.T) {
	d := NewDiscoverer()
	const serverOrigin = "http://mcp.svc:8080"
	reason, _, ok := d.validateHop(serverOrigin+prWellKnown, StepProtectedResource, serverOrigin, map[string]bool{})
	if !ok {
		t.Fatalf("same-origin plain-HTTP PR hop should pass the string layer, got %q", reason)
	}
}

// ...while a NON-pinned / cross-origin plain-HTTP target STILL returns NotHTTPS.
func TestDiscoverer_NonPinnedPlainHTTP_StillNotHTTPS(t *testing.T) {
	d := NewDiscoverer()
	const serverOrigin = "http://mcp.svc:8080"
	allowed := map[string]bool{"http://other.svc:8080": true}
	// Cross-origin plain-HTTP protected-resource target → NotHTTPS (the https
	// extension is same-origin-only).
	reason, _, ok := d.validateHop("http://other.svc:8080"+prWellKnown, StepProtectedResource, serverOrigin, allowed)
	if ok || reason != ReasonNotHTTPS {
		t.Fatalf("cross-origin plain-HTTP PR hop: want %q refusal, got ok=%v reason=%q", ReasonNotHTTPS, ok, reason)
	}
}

// The STRING-layer step-gate on the https extension: a same-origin-by-string
// authorization-server hop over plain-HTTP (attacker issuer = the operator's own
// origin, with an allowance) is STILL refused NotHTTPS — the https relaxation is
// gated on step == StepProtectedResource, not sameOrigin alone.
func TestDiscoverer_SameOriginStringASHopPlainHTTP_StillNotHTTPS(t *testing.T) {
	d := NewDiscoverer()
	const serverOrigin = "http://mcp.svc:8080"
	allowed := map[string]bool{serverOrigin: true} // AS origin == server origin, allowed
	reason, _, ok := d.validateHop(serverOrigin+asWellKnown, StepAuthorizationServer, serverOrigin, allowed)
	if ok || reason != ReasonNotHTTPS {
		t.Fatalf("same-origin-string plain-HTTP AS hop: want %q refusal, got ok=%v reason=%q", ReasonNotHTTPS, ok, reason)
	}
}

// The DNS-rebinding crux: a same-origin-by-string metadata URL whose host
// resolves to a DIFFERENT private IP at dial time than at pin time is STILL
// refused. The fake resolver returns 127.0.0.1 for the walk-start pin lookup and
// 10.99.99.99 for the dial lookup (a rebind). Under an origin-string-only pin
// (no resolved-IP membership check) this would WRONGLY PERMIT; the resolved-IP
// set membership is what refuses it.
func TestDiscoverer_SameOriginRebindToDifferentPrivateIP_StillRefused(t *testing.T) {
	srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{
		prWellKnown: []byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`),
	}})
	defer srv.Close()
	port := portOf(t, srv.URL)
	svcURL := "http://mcp.svc.:" + port
	// The DISCRIMINATOR: the pin lookup resolves to a dead private IP
	// (10.99.99.99), the DIAL lookup rebinds to 127.0.0.1 — the live fixture. A
	// resolved-IP pin refuses (127.0.0.1 ∉ {10.99.99.99}). An origin-string-only
	// pin would PERMIT the rebind, connect to the live fixture, and the PR hop
	// would WRONGLY complete — so this ordering is what makes the test fail the
	// wrong implementation (§17.8), not merely fail a dead connection.
	res := newFakeResolver(t, "10.99.99.99", "127.0.0.1")
	d := NewDiscoverer(WithDiscoveryTimeout(2*time.Second), WithResolverForTest(res))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           svcURL,
		ResourceMetadataURL: svcURL + prWellKnown, // same origin string
		Source:              "probe",
	})
	st := prStatus(req)
	if st == nil || st.OK {
		t.Fatalf("rebind to a different private IP must be refused; statuses=%+v", req.Status)
	}
}

// Intra-host PORT SSRF: a same-origin redirect to a DIFFERENT port on the pinned
// IP (302 → {pinnedIP}:6379) is STILL refused (the port is part of the pin). The
// dial to :6379 is refused by ControlContext BEFORE any connection is attempted.
func TestDiscoverer_SameOriginRedirectToDifferentPort_StillRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		http.Redirect(w, r, "http://"+net.JoinHostPort(host, "6379")+prWellKnown, http.StatusFound)
	}))
	defer srv.Close()
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second)) // strict, production path
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL, // http://127.0.0.1:PORT
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
	})
	st := prStatus(req)
	if st == nil || st.OK {
		t.Fatalf("same-origin redirect to a different port must be refused; statuses=%+v", req.Status)
	}
}

// A redirect that leaves the pinned target (a DIFFERENT private IP) is refused
// AT THE REDIRECT'S DIAL by ControlContext pin-membership — not by a validateHop
// re-entry. DisableKeepAlives guarantees the redirect dial is a fresh connection
// that re-enters the gate.
func TestDiscoverer_RedirectOffPinnedTarget_StillRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(r.Host)
		http.Redirect(w, r, "http://"+net.JoinHostPort("10.99.99.99", port)+prWellKnown, http.StatusFound)
	}))
	defer srv.Close()
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
	})
	st := prStatus(req)
	if st == nil || st.OK {
		t.Fatalf("redirect off the pinned target (different IP) must be refused; statuses=%+v", req.Status)
	}
}

// No keep-alive step-gate bypass: a multi-hop sequence to the SAME host:port
// re-enters ControlContext on every dial (DisableKeepAlives). The PR fixture
// advertises its OWN origin as the authorization server; the PR hop completes
// (pinned), then the AS hop to the SAME loopback host:port is refused at the
// dial because the pin's step is AS — which is only observable if the gate
// re-runs on a fresh dial rather than riding the pooled PR connection.
func TestDiscoverer_SameHostMultiHop_ReValidatesEachDial(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prWellKnown {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resource":"x","authorization_servers":["` + srv.URL + `"]}`))
			return
		}
		// The AS path should NEVER be reached — the AS dial is refused first.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadFixture(t, "rfc8414_authorization_server.json"))
	}))
	defer srv.Close()
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second))
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
		AllowedOrigins:      []string{srv.URL}, // allow the same-origin AS so it reaches the dial
	})
	if st := prStatus(req); st == nil || !st.OK {
		t.Fatalf("PR hop should complete; statuses=%+v", req.Status)
	}
	if st := asStatus(req); st == nil || st.OK {
		t.Fatalf("AS hop to the same loopback host:port must be refused at the re-validated dial; statuses=%+v", req.Status)
	}
	if len(req.AuthorizationServers) != 0 {
		t.Fatalf("no AS should resolve (the AS dial was refused): %+v", req.AuthorizationServers)
	}
}

// A cross-origin authorization-server hop that resolves to a private address is
// STILL refused on the production path — the relaxation never applies off the
// pinned same-origin protected-resource hop. Uses the spec-derived chain fixture
// (PR + AS both on loopback) on the PRODUCTION discoverer (no test escape hatch):
// the PR hop completes (pinned same-origin), the cross-origin AS hop is refused.
func TestDiscoverer_CrossOriginPrivateHop_StillRefused(t *testing.T) {
	_, _, cleanup, in := newChainFixture(t)
	defer cleanup()
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second)) // strict production path
	req := d.Discover(context.Background(), in)
	if st := prStatus(req); st == nil || !st.OK {
		t.Fatalf("same-origin PR hop should complete; statuses=%+v", req.Status)
	}
	if st := asStatus(req); st == nil || st.OK {
		t.Fatalf("cross-origin AS hop to a private address must be refused on the production path; statuses=%+v", req.Status)
	}
	if len(req.AuthorizationServers) != 0 {
		t.Fatalf("cross-origin private AS must not resolve on the production path: %+v", req.AuthorizationServers)
	}
}

// The authorization-server hop still requires an allowance and surfaces the
// typed needs_allowance partial without one — the relaxation does not touch the
// AS gate. (Production path, same-origin PR completes, AS with no allowance.)
func TestDiscoverer_AuthServerHop_StillNeedsAllowance(t *testing.T) {
	srv := httptest.NewServer(&recordingHandler{routes: map[string][]byte{
		prWellKnown: []byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`),
	}})
	defer srv.Close()
	d := NewDiscoverer(WithDiscoveryTimeout(2 * time.Second)) // strict production path
	req := d.Discover(context.Background(), DiscoveryInput{
		ServerURL:           srv.URL,
		ResourceMetadataURL: srv.URL + prWellKnown,
		Source:              "probe",
		// no AllowedOrigins → the AS hop must report needs_allowance
	})
	if st := prStatus(req); st == nil || !st.OK {
		t.Fatalf("same-origin PR hop should complete; statuses=%+v", req.Status)
	}
	st := asStatus(req)
	if st == nil || st.OK || st.Reason != ReasonNeedsAllowance {
		t.Fatalf("AS hop should report needs_allowance; statuses=%+v", req.Status)
	}
}

// The test-only options panic outside a test binary; inside `go test`
// (testing.Testing() == true) they configure the discoverer without panicking.
// The panic branch fires only when testing.Testing() is false — unreachable
// under `go test` — so we exercise the in-test branch and the mechanism guard.
func TestWithPrivateNetworkAccessForTest_PanicsOutsideTestBinary(t *testing.T) {
	if !testing.Testing() {
		t.Fatal("precondition: testing.Testing() must be true inside a test binary")
	}
	// Inside a test binary the option applies cleanly (no panic).
	d := NewDiscoverer(WithPrivateNetworkAccessForTest())
	if !d.allowPrivate {
		t.Fatal("WithPrivateNetworkAccessForTest should set allowPrivate inside a test binary")
	}
	// WithResolverForTest is likewise test-only and applies without panic.
	res := newFakeResolver(t, "127.0.0.1")
	d2 := NewDiscoverer(WithResolverForTest(res))
	if d2.resolver != res {
		t.Fatal("WithResolverForTest should set the resolver inside a test binary")
	}
}

// Concurrent pin-isolation (D-025): N≥100 concurrent Discover calls against one
// shared Discoverer, each pinning a DIFFERENT ServerURL (a distinct loopback
// port). A pin bleeding from walk B into walk A would make A dial a port outside
// its own pin and refuse — so every PR hop completing proves no cross-walk pin
// bleed, under -race.
func TestDiscoverer_Discover_ConcurrentPinIsolation(t *testing.T) {
	const n = 100
	prBody := []byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`)
	servers := make([]*httptest.Server, n)
	for i := range n {
		servers[i] = httptest.NewServer(&recordingHandler{routes: map[string][]byte{prWellKnown: prBody}})
	}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()

	d := NewDiscoverer(WithDiscoveryTimeout(3 * time.Second)) // strict production path
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			req := d.Discover(context.Background(), DiscoveryInput{
				ServerURL:           servers[i].URL,
				ResourceMetadataURL: servers[i].URL + prWellKnown,
				Source:              "probe",
			})
			if st := prStatus(req); st == nil || !st.OK {
				t.Errorf("walk %d: PR hop should complete on its own pin; statuses=%+v", i, req.Status)
			}
		}(i)
	}
	wg.Wait()
}
