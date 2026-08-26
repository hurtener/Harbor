package providerroute

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestSelectionContractCarriesOperationButNeverCredential(t *testing.T) {
	req := llm.ProviderRouteRequest{
		TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call",
		RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast",
	}
	requestBody, err := MarshalSelectionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	operation, decoded, err := UnmarshalOperationRequest(requestBody)
	if err != nil || operation != OperationSelect || decoded != req {
		t.Fatalf("selection request operation=%q decoded=%+v err=%v", operation, decoded, err)
	}
	selected := llm.SelectedProviderRoute{
		Provider: "openai", Model: "selected-model", KeyName: "route key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	responseBody, err := MarshalSelectionResponse(req, selected)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(responseBody, []byte(`"credential":`)) {
		t.Fatalf("selection response contains credential field: %s", responseBody)
	}
	if _, err := ParseSelectionResponse(req, append(responseBody[:len(responseBody)-1], []byte(`,"credential":"canary"}`)...)); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("credential-bearing selection error = %v, want ErrProviderRouteInvalid", err)
	}
}
