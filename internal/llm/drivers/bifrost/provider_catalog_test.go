package bifrost

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/provider"
)

func TestStaticProviderDescriptorsSeparateCustomEndpointAndManualFallback(t *testing.T) {
	descriptors := StaticProviderDescriptors([]llm.CustomProviderSpec{{
		Name: "internal-gateway", BaseURL: "https://gateway.example.invalid/v1", APIKeyEnvVar: "LLM_KEY", Models: []string{"model"},
	}})
	var native, custom *provider.ProviderDescriptor
	for i := range descriptors {
		switch descriptors[i].ID {
		case "openai":
			native = &descriptors[i]
		case "internal-gateway":
			custom = &descriptors[i]
		}
	}
	if native == nil || native.CustomEndpoint != provider.SupportManual {
		t.Fatalf("native endpoint claim is too strong: %+v", native)
	}
	if custom == nil || custom.CustomEndpoint != provider.SupportSupported || custom.Discovery.State != provider.SupportSupported || !custom.Discovery.RuntimeOrigin {
		t.Fatalf("custom endpoint/runtime discovery facts missing: %+v", custom)
	}
	for _, field := range custom.CredentialFields {
		if field.Name == "api_key" && (!field.Secret || !field.Required) {
			t.Fatalf("custom credential field is not secret/required: %+v", field)
		}
	}
}

func TestMapBifrostModelDoesNotCarryProviderExtra(t *testing.T) {
	contextLength := 1000
	model := mapBifrostModel(bfschemas.Model{
		ID: "model", ContextLength: &contextLength,
		ProviderExtra:       []byte(`{"secret":"must-not-cross-adapter"}`),
		SupportedParameters: []string{"tools"},
	})
	if model.ID != "model" || model.ContextLength == nil || *model.ContextLength != 1000 {
		t.Fatalf("model facts were not mapped: %+v", model)
	}
	if model.PricingKnown {
		t.Fatal("pricing should remain unknown when omitted")
	}
}

func TestClassifyBifrostErrorUsesStableClassification(t *testing.T) {
	status := 403
	secret := "sk-secret-must-not-be-returned"
	typeName := "provider_error"
	err := classifyBifrostError(&bfschemas.BifrostError{
		Type: &typeName, StatusCode: &status,
		Error: &bfschemas.ErrorField{Message: secret},
	})
	if err == nil || err.Error() != "provider_credential_rejected" {
		t.Fatalf("unexpected classification: %v", err)
	}
	if err.Error() == secret {
		t.Fatal("provider error message leaked")
	}
}

func TestProviderCatalogCustomEndpointDiscoveryUsesBifrost(t *testing.T) {
	const envName = "HARBOR_TEST_PROVIDER_CATALOG_KEY"
	t.Setenv(envName, "catalog-test-key")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" {
			t.Errorf("model discovery path = %q, want /v1/models", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("model discovery did not carry bearer authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"remote-model","object":"model","owned_by":"test"}]}`))
	}))
	defer server.Close()

	catalog, err := NewProviderCatalog(llm.ConfigSnapshot{
		Provider: "internal-gateway",
		CustomProviders: []llm.CustomProviderSpec{{
			Name:         "internal-gateway",
			BaseURL:      server.URL,
			APIKeyEnvVar: envName,
			Models:       []string{"remote-model"},
			Timeout:      time.Second,
		}},
	})
	if err != nil {
		t.Fatalf("NewProviderCatalog: %v", err)
	}
	defer func() { _ = catalog.Close(context.Background()) }()

	result, err := catalog.Discover(context.Background(), provider.DiscoveryRequest{
		ProviderID: "internal-gateway", PageSize: 10, MaxPages: 1,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if requests != 1 {
		t.Fatalf("model discovery requests = %d, want 1", requests)
	}
	if result.Outcome.State != provider.SupportSupported || result.Outcome.Code != "model_discovery_complete" {
		t.Fatalf("unexpected discovery outcome: %+v", result.Outcome)
	}
	if len(result.Models) != 1 || result.Models[0].ID != "internal-gateway/remote-model" || result.Models[0].Source != provider.ModelSourceDiscovered {
		t.Fatalf("unexpected discovered models: %+v", result.Models)
	}
}

func TestProviderCatalogConcurrentReuse(t *testing.T) {
	const envName = "HARBOR_TEST_PROVIDER_CATALOG_REUSE_KEY"
	t.Setenv(envName, "catalog-reuse-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("model discovery path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"reuse-model","object":"model"}]}`))
	}))
	defer server.Close()

	catalog, err := NewProviderCatalog(llm.ConfigSnapshot{
		Provider: "reuse-gateway",
		CustomProviders: []llm.CustomProviderSpec{{
			Name: "reuse-gateway", BaseURL: server.URL, APIKeyEnvVar: envName,
			Models: []string{"reuse-model"}, Timeout: time.Second,
		}},
	})
	if err != nil {
		t.Fatalf("NewProviderCatalog: %v", err)
	}
	defer func() { _ = catalog.Close(context.Background()) }()

	const invocations = 100
	var wg sync.WaitGroup
	errs := make(chan error, invocations)
	for range invocations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if descriptors := catalog.Descriptors(context.Background()); len(descriptors) != len(bfschemas.StandardProviders)+1 {
				errs <- fmt.Errorf("descriptor count = %d", len(descriptors))
				return
			}
			result, discoverErr := catalog.Discover(context.Background(), provider.DiscoveryRequest{
				ProviderID: "reuse-gateway", PageSize: 10, MaxPages: 1,
			})
			if discoverErr != nil {
				errs <- discoverErr
				return
			}
			if result.Outcome.State != provider.SupportSupported || len(result.Models) != 1 {
				errs <- fmt.Errorf("unexpected reuse result: %+v", result.Outcome)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
