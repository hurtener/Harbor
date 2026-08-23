package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeLister struct {
	fn    func(context.Context, string, int, string) (ModelPage, error)
	calls atomic.Int32
}

func (f *fakeLister) ListModels(ctx context.Context, providerID string, pageSize int, pageToken string) (ModelPage, error) {
	f.calls.Add(1)
	return f.fn(ctx, providerID, pageSize, pageToken)
}

func supportedDescriptor(id string) ProviderDescriptor {
	return ProviderDescriptor{
		ID: id, Kind: "native", CredentialModes: []CredentialMode{CredentialAPIKey},
		CredentialFields: []CredentialField{{Name: "api_key", Kind: FieldSecret, Required: true, Secret: true}},
		CustomEndpoint:   SupportManual,
		Validation:       OperationSupport{State: SupportSupported, RuntimeOrigin: true, Bounded: true},
		Discovery:        OperationSupport{State: SupportSupported, RuntimeOrigin: true, Bounded: true},
	}
}

func TestCatalogManualModelsAreNotReportedAsDiscovered(t *testing.T) {
	catalog, err := NewCatalog(nil, []ProviderDescriptor{{
		ID: "gateway", Kind: "custom", Discovery: OperationSupport{State: SupportManual, Bounded: true},
		Validation: OperationSupport{State: SupportSupported, RuntimeOrigin: true, Bounded: true},
	}}, []string{"gateway"}, map[string][]string{"gateway": {"model-b", "model-a"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportManual || result.Outcome.Code != "model_catalog_manual" {
		t.Fatalf("unexpected outcome: %+v", result.Outcome)
	}
	if len(result.Models) != 2 || result.Models[0].Source != ModelSourceManual || result.Models[0].ID != "model-a" {
		t.Fatalf("manual models were not stable/manual: %+v", result.Models)
	}
}

func TestCatalogUsesManualFallbackWhenRuntimeDiscoveryIsUnavailable(t *testing.T) {
	lister := &fakeLister{fn: func(context.Context, string, int, string) (ModelPage, error) {
		return ModelPage{}, NewProviderError("model_discovery_unsupported", 0, true)
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{{
		ID: "gateway", Kind: "custom", Discovery: OperationSupport{State: SupportSupported, RuntimeOrigin: true, Bounded: true},
	}}, []string{"gateway"}, map[string][]string{"gateway": {"manual-model"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportManual || result.Outcome.Code != "model_catalog_manual_fallback" || !result.Outcome.RuntimeOrigin || !result.Outcome.Partial {
		t.Fatalf("unexpected manual fallback outcome: %+v", result.Outcome)
	}
	if len(result.Models) != 1 || result.Models[0].Source != ModelSourceManual {
		t.Fatalf("manual fallback was not labeled manual: %+v", result.Models)
	}
}

func TestCatalogUnsupportedProviderIsExplicit(t *testing.T) {
	catalog, err := NewCatalog(nil, []ProviderDescriptor{{
		ID: "legacy", Discovery: OperationSupport{State: SupportUnsupported},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportUnsupported || result.Outcome.Code != "model_discovery_unsupported" {
		t.Fatalf("unexpected outcome: %+v", result.Outcome)
	}
}

func TestCatalogPartialAndStaleResultsAreNotComplete(t *testing.T) {
	lister := &fakeLister{fn: func(_ context.Context, _ string, _ int, token string) (ModelPage, error) {
		if token == "" {
			return ModelPage{Models: []RawModel{{ID: "a"}}, NextPageToken: "next", KeyFailures: 1}, nil
		}
		return ModelPage{Models: []RawModel{{ID: "b"}}, Stale: true}, nil
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "openai", PageSize: 1, MaxPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportStale || !result.Outcome.Stale || !result.Outcome.Partial {
		t.Fatalf("expected stale+partial result: %+v", result.Outcome)
	}
	if result.ModelCount != 2 || lister.calls.Load() != 2 {
		t.Fatalf("unexpected bounded page result: calls=%d result=%+v", lister.calls.Load(), result)
	}
}

func TestCatalogMalformedProviderReplyFailsClosedAndRedacts(t *testing.T) {
	secret := "sk-provider-secret-not-for-output"
	lister := &fakeLister{fn: func(context.Context, string, int, string) (ModelPage, error) {
		return ModelPage{Models: []RawModel{{ID: secret}, {ID: secret}}}, nil
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportMalformed || len(result.Models) != 0 {
		t.Fatalf("malformed reply was not fail-closed: %+v", result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("provider response data leaked through result: %s", encoded)
	}
}

func TestCatalogRejectsUnboundedProviderPage(t *testing.T) {
	lister := &fakeLister{fn: func(context.Context, string, int, string) (ModelPage, error) {
		return ModelPage{Models: []RawModel{{ID: "a"}, {ID: "b"}}}, nil
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "openai", PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != SupportMalformed || result.Outcome.Code != "provider_reply_malformed" {
		t.Fatalf("unbounded page was accepted: %+v", result.Outcome)
	}
}

func TestCatalogValidationUsesStableRedactedErrors(t *testing.T) {
	secret := "https://user:password@example.invalid/v1"
	lister := &fakeLister{fn: func(context.Context, string, int, string) (ModelPage, error) {
		return ModelPage{}, NewProviderError("provider_unavailable", 401, false)
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := catalog.Validate(context.Background(), ValidationRequest{ProviderID: "openai"})
	if result.Outcome.Code != "provider_credential_rejected" || result.Outcome.Message == secret {
		t.Fatalf("unexpected sanitized validation: %+v", result)
	}
	if strings.Contains(result.Outcome.Message, "password") {
		t.Fatalf("credential leaked: %+v", result.Outcome)
	}
}

func TestProviderErrorSanitizesArbitraryCode(t *testing.T) {
	secret := "https://user:password@example.invalid/v1"
	if got := NewProviderError(secret, 0, false).Error(); strings.Contains(got, "password") || got != "provider_unavailable" {
		t.Fatalf("provider error code leaked or was not normalized: %q", got)
	}
}

func TestNormalizeModelsRejectsInvalidLimits(t *testing.T) {
	bad := -1
	if _, err := NormalizeModels([]RawModel{{ID: "model", ContextLength: &bad}}); err == nil {
		t.Fatal("expected invalid limit refusal")
	}
	good := 128000
	models, err := NormalizeModels([]RawModel{{
		ID: "model", ContextLength: &good, InputModalities: []string{"text", "image"},
		SupportedParameters: []string{"tools", "reasoning_effort"}, PricingKnown: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := models[0].Capabilities
	if capabilities.Vision != SupportSupported || capabilities.Tools != SupportSupported || capabilities.Reasoning.State != SupportSupported || capabilities.Pricing.State != SupportSupported {
		t.Fatalf("normalized capabilities lost known facts: %+v", capabilities)
	}
	if capabilities.MaxOutputTokens.State != SupportUnknown {
		t.Fatalf("omitted limits should remain unknown: %+v", capabilities)
	}
}

func TestCatalogConcurrentReuse(t *testing.T) {
	lister := &fakeLister{fn: func(_ context.Context, _ string, _ int, _ string) (ModelPage, error) {
		return ModelPage{Models: []RawModel{{ID: "model", SupportedParameters: []string{"tools"}}}}, nil
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := catalog.Discover(context.Background(), DiscoveryRequest{ProviderID: "openai"})
			if err != nil || result.Outcome.State != SupportSupported || len(result.Models) != 1 {
				t.Errorf("concurrent discovery failed: err=%v result=%+v", err, result)
			}
		}()
	}
	wg.Wait()
}

func TestCatalogContextCancellationDoesNotCallSource(t *testing.T) {
	called := atomic.Bool{}
	lister := &fakeLister{fn: func(context.Context, string, int, string) (ModelPage, error) {
		called.Store(true)
		return ModelPage{}, errors.New("must not be called")
	}}
	catalog, err := NewCatalog(lister, []ProviderDescriptor{supportedDescriptor("openai")}, []string{"openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := catalog.Validate(ctx, ValidationRequest{ProviderID: "openai"})
	if result.Outcome.Code != "provider_unavailable" || !strings.Contains(result.Outcome.Message, "cancelled") || called.Load() {
		t.Fatalf("cancellation was not bounded: %+v called=%v", result, called.Load())
	}
}
