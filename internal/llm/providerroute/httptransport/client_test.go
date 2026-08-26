package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

func testRouteRequest(tenant string, generation uint64) llm.ProviderRouteRequest {
	return llm.ProviderRouteRequest{
		TenantID: tenant, UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call-" + tenant,
		RouteID: "route", RouteGeneration: generation, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: generation, CredentialAssetGeneration: generation, ModelSelector: "fast",
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
	for _, raw := range []string{"http://resolver.example.test/route", "https://user@resolver.example.test/route?next=x", "https://resolver.example.test/route#x"} {
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
