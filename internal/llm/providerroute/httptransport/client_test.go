package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestSafeDialContextRejectsPrivateAndMixedDestinations(t *testing.T) {
	tests := []struct {
		name string
		ips  []net.IP
	}{
		{"loopback-https", []net.IP{net.ParseIP("127.0.0.1")}},
		{"private-10", []net.IP{net.ParseIP("10.0.0.1")}},
		{"metadata", []net.IP{net.ParseIP("169.254.169.254")}},
		{"ipv4-zero", []net.IP{net.ParseIP("0.1.2.3")}},
		{"ipv4-reserved-high", []net.IP{net.ParseIP("240.1.2.3")}},
		{"ipv4-special-use", []net.IP{net.ParseIP("192.0.0.42")}},
		{"localhost-private-dns", []net.IP{net.ParseIP("10.0.0.1")}},
		{"mixed-public-private", []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.1")}},
		{"ipv6-loopback", []net.IP{net.ParseIP("::1")}},
		{"ipv6-unspecified", []net.IP{net.ParseIP("::")}},
		{"ipv6-private", []net.IP{net.ParseIP("fc00::1")}},
		{"ipv6-link-local", []net.IP{net.ParseIP("fe80::1")}},
		{"ipv6-multicast", []net.IP{net.ParseIP("ff02::1")}},
		{"ipv6-reserved-low", []net.IP{net.ParseIP("::1:2")}},
		{"ipv6-discard", []net.IP{net.ParseIP("100::1")}},
		{"ipv6-documentation", []net.IP{net.ParseIP("2001:db8::1")}},
		{"ipv6-benchmarking", []net.IP{net.ParseIP("2001:2::1")}},
		{"ipv6-orchid", []net.IP{net.ParseIP("2001:20::1")}},
		{"ipv6-translation", []net.IP{net.ParseIP("64:ff9b::1")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dials := 0
			dial := safeDialContext("https", "route.example.test", func(context.Context, string) ([]net.IP, error) { return test.ips, nil }, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, nil
			})
			if _, err := dial(context.Background(), "tcp", "route.example.test:443"); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("dial error = %v, want ErrInvalidConfig", err)
			}
			if dials != 0 {
				t.Fatalf("dialer calls = %d, want 0", dials)
			}
		})
	}
}

func TestSafeDialContextAllowsPublicAndLoopbackHTTPOnly(t *testing.T) {
	tests := []struct {
		name, scheme string
		ips          []net.IP
	}{
		{"public-https", "https", []net.IP{net.ParseIP("93.184.216.34")}},
		{"loopback-http", "http", []net.IP{net.ParseIP("127.0.0.1")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dials := 0
			dial := safeDialContext(test.scheme, "route.example.test", func(context.Context, string) ([]net.IP, error) { return test.ips, nil }, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, nil
			})
			if _, err := dial(context.Background(), "tcp", "route.example.test:443"); err != nil {
				t.Fatalf("dial error = %v", err)
			}
			if dials != 1 {
				t.Fatalf("dialer calls = %d, want 1", dials)
			}
		})
	}
}

func TestSafeDialContextUsesFreshLookupAndValidatedLiteral(t *testing.T) {
	lookups := 0
	var addresses []string
	dial := safeDialContext("https", "route.example.test", func(context.Context, string) ([]net.IP, error) {
		lookups++
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		addresses = append(addresses, address)
		return nil, nil
	})
	for range 2 {
		if _, err := dial(context.Background(), "tcp", "route.example.test:443"); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 2 || len(addresses) != 2 || addresses[0] != "93.184.216.34:443" || addresses[1] != addresses[0] {
		t.Fatalf("lookups=%d addresses=%v", lookups, addresses)
	}
}

func TestNewWithNetworkOwnsTransportEgressHooks(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, nil }
	proxyTransport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if _, err := newWithNetwork(Config{ResolverURL: "https://resolver.example.test", AuthToken: "fixture-token"}, lookup, dial); err != nil {
		t.Fatalf("default transport rejected: %v", err)
	}
	if _, err := newWithNetwork(Config{ResolverURL: "https://resolver.example.test", AuthToken: "fixture-token", HTTPClient: &http.Client{Transport: proxyTransport}}, lookup, dial); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("proxy transport error = %v, want ErrInvalidConfig", err)
	}
	custom := &http.Transport{
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return nil, nil },
		DialTLS:        func(string, string) (net.Conn, error) { return nil, nil },
	}
	if _, err := newWithNetwork(Config{ResolverURL: "https://resolver.example.test", AuthToken: "fixture-token", HTTPClient: &http.Client{Transport: custom}}, lookup, dial); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("custom TLS transport error = %v, want ErrInvalidConfig", err)
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(Config{ResolverURL: server.URL, AuthToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.httpClient.Do(req); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("redirect error = %v", err)
	}
}

func testRouteRequest(tenant string, generation uint64) llm.ProviderRouteRequest {
	return llm.ProviderRouteRequest{
		TenantID: tenant, UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call-" + tenant,
		RouteID: "route", RouteGeneration: generation, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: generation, CredentialAssetGeneration: generation, ModelSelector: "fast",
		Purpose: llm.ProviderRoutePurposeRun,
	}
}

func TestClient_ConcurrentTenantsDoNotBleedCredentials(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		tenant := request["tenant_id"].(string)
		generation := uint64(request["route_generation"].(float64))
		mu.Lock()
		seen[tenant]++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "provider": "openai", "model": "model-" + tenant, "key_name": "route key",
			"route_id": "route", "route_generation": generation,
			"provider_connection_id": "connection", "provider_connection_generation": generation,
			"credential_asset_generation": generation, "model_selector": "fast", "credential": "key-" + tenant,
			"expires_at": time.Now().Add(time.Minute).UTC(),
		})
	}))
	defer server.Close()
	client, err := New(Config{ResolverURL: server.URL, AuthToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := client.ResolveProviderRoute(context.Background(), testRouteRequest(tenant, 1))
			if err != nil || got.Credential != "key-"+tenant || got.Model != "model-"+tenant {
				t.Errorf("tenant %s got %+v err=%v", tenant, got, err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if seen["tenant-a"] != 1 || seen["tenant-b"] != 1 {
		t.Fatalf("requests = %#v", seen)
	}
}

func TestClient_RefusesResolverControlledEgressAndObservesRotation(t *testing.T) {
	for _, raw := range []string{"http://resolver.example.test/route", "https://user@resolver.example.test/route?next=x", "https://resolver.example.test/route#x", "https://127.0.0.1/route", "https://10.0.0.1/route"} {
		if _, err := New(Config{ResolverURL: raw, AuthToken: "fixture-token"}); err == nil {
			t.Fatalf("unsafe resolver URL accepted: %s", raw)
		}
	}
	var generation uint64 = 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := testRouteRequest("tenant", generation)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "provider": "openai", "model": "model", "key_name": "route key",
			"route_id": req.RouteID, "route_generation": req.RouteGeneration,
			"provider_connection_id": req.ProviderConnectionID, "provider_connection_generation": req.ProviderConnectionGeneration,
			"credential_asset_generation": req.CredentialAssetGeneration, "model_selector": req.ModelSelector, "credential": "key",
			"expires_at": time.Now().Add(time.Minute).UTC(),
		})
	}))
	defer server.Close()
	client, err := New(Config{ResolverURL: server.URL, AuthToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveProviderRoute(context.Background(), testRouteRequest("tenant", generation)); err != nil {
		t.Fatal(err)
	}
	generation = 2
	if _, err := client.ResolveProviderRoute(context.Background(), testRouteRequest("tenant", generation)); err != nil {
		t.Fatal(err)
	}
}

func TestClient_SelectionIsCredentialFreeAndResolutionIsPerAttempt(t *testing.T) {
	var selections, resolutions int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		response := map[string]any{
			"version": 1, "provider": "openai", "model": "selected-model", "key_name": "route key",
			"route_id": "route", "route_generation": 1,
			"provider_connection_id": "connection", "provider_connection_generation": 1,
			"credential_asset_generation": 1, "model_selector": "fast",
			"expires_at": time.Now().Add(time.Minute).UTC(),
		}
		switch request["operation"] {
		case "select":
			selections++
		case "resolve":
			resolutions++
			response["credential"] = "attempt-only-key"
		default:
			t.Errorf("unexpected operation: %v", request["operation"])
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client, err := New(Config{ResolverURL: server.URL, AuthToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := testRouteRequest("tenant", 1)
	selected, err := client.SelectProviderRoute(context.Background(), req)
	if err != nil || selected.Model != "selected-model" {
		t.Fatalf("selection = %+v, err=%v", selected, err)
	}
	for range 2 {
		resolved, err := client.ResolveProviderRoute(context.Background(), req)
		if err != nil || resolved.Credential != "attempt-only-key" || resolved.Model != selected.Model {
			t.Fatalf("resolution = %+v, err=%v", resolved, err)
		}
	}
	if selections != 1 || resolutions != 2 {
		t.Fatalf("resolver calls select=%d resolve=%d, want 1/2", selections, resolutions)
	}
}
