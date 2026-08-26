package providerroute_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/sdk/llm/providerroute"
)

func TestPublicContractRoundTrip(t *testing.T) {
	req := providerroute.Request{
		TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call",
		RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "fast",
	}
	body, err := providerroute.MarshalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := providerroute.UnmarshalRequest(body)
	if err != nil || parsed != req {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	response := providerroute.Response{
		Provider: "openai", Model: "model", KeyName: "route key", RouteID: "route", RouteGeneration: 1,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 2,
		CredentialAssetGeneration: 3, ModelSelector: "fast", Credential: "fixture", ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}
	raw, err := providerroute.MarshalResponse(req, response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := providerroute.ParseResponse(req, raw)
	if err != nil || got.Credential != response.Credential {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestPublicHandlerServesSelectionAndResolutionWithoutInternalImports(t *testing.T) {
	endpointValue, endpointDigest, err := providerroute.NormalizeEndpoint("https://gateway.example.test/v1/")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &providerroute.EndpointBinding{Kind: providerroute.EndpointOpenAICompatible, Value: endpointValue, Digest: endpointDigest}
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		operation, req, err := providerroute.UnmarshalOperationRequest(body)
		if err != nil {
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		var response []byte
		switch operation {
		case providerroute.OperationSelect:
			response, err = providerroute.MarshalSelectionResponse(req, providerroute.SelectedResponse{
				Provider: "openai", Model: "selected-model", KeyName: "route key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
				ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
				CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
				Endpoint: endpoint, ExpiresAt: time.Now().Add(time.Minute),
			})
		case providerroute.OperationResolve:
			response, err = providerroute.MarshalResponse(req, providerroute.Response{
				Provider: "openai", Model: "selected-model", KeyName: "route key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
				ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
				CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
				Endpoint: endpoint, Credential: "attempt-only-key", ExpiresAt: time.Now().Add(time.Minute),
			})
		default:
			http.Error(w, "operation", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "encode", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(response)
	}))
	defer handler.Close()

	req := providerroute.Request{
		TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call",
		RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "fast",
	}
	selectionBody, err := providerroute.MarshalSelectionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	selectionRaw := postPublicRouteContract(t, handler.URL, selectionBody)
	if bytes.Contains(selectionRaw, []byte(`"credential":`)) {
		t.Fatalf("public selection response leaked credential field: %s", selectionRaw)
	}
	selected, err := providerroute.ParseSelectionResponse(req, selectionRaw)
	if err != nil || selected.Model != "selected-model" || selected.Endpoint == nil || selected.Endpoint.Digest != endpointDigest {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}

	resolveBody, err := providerroute.MarshalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := providerroute.ParseResponse(req, postPublicRouteContract(t, handler.URL, resolveBody))
	if err != nil || resolved.Credential != "attempt-only-key" || resolved.Model != selected.Model || resolved.Endpoint == nil || resolved.Endpoint.Digest != endpointDigest {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
}

func postPublicRouteContract(t *testing.T, url string, body []byte) []byte {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	return raw
}
