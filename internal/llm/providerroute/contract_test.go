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
		ModelProfileSupported: true,
		Purpose:               llm.ProviderRoutePurposeRun,
	}
	requestBody, err := MarshalSelectionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(requestBody, []byte(`"model_profile_supported":true`)) {
		t.Fatalf("selection request omitted model-profile negotiation bit: %s", requestBody)
	}
	operation, decoded, err := UnmarshalOperationRequest(requestBody)
	if err != nil || operation != OperationSelect || decoded != req {
		t.Fatalf("selection request operation=%q decoded=%+v err=%v", operation, decoded, err)
	}
	if decoded.Purpose != llm.ProviderRoutePurposeRun {
		t.Fatalf("decoded purpose = %q, want run", decoded.Purpose)
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

func TestRouteContractCarriesOptionalModelProfile(t *testing.T) {
	req := llm.ProviderRouteRequest{
		TenantID: "tenant", UserID: "user", SessionID: "session", LogicalRunID: "run",
		EffectiveAgentID: "agent", RuntimeID: "runtime", TaskID: "task", LogicalCallID: "call",
		RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast",
		ModelProfileSupported: true,
		Purpose:               llm.ProviderRoutePurposeRun,
	}
	profile := &llm.ProviderModelProfile{
		ContextWindowTokens:   8192,
		MaxOutputTokens:       2048,
		ReasoningEffortLevels: []llm.ReasoningEffort{},
	}
	selected := llm.SelectedProviderRoute{
		Provider: "openai", Model: "arbitrary/model", KeyName: "route key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		ExpiresAt: time.Now().Add(time.Minute), ModelProfile: profile,
	}
	body, err := MarshalSelectionResponse(req, selected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"model_profile"`)) || !bytes.Contains(body, []byte(`"reasoning_effort_levels":[]`)) {
		t.Fatalf("selection response omitted the optional model profile or explicit empty levels: %s", body)
	}
	decoded, err := ParseSelectionResponse(req, body)
	if err != nil {
		t.Fatal(err)
	}
	if !llm.ProviderRouteResolutionMatchesSelection(llm.ResolvedProviderRoute{
		Provider: decoded.Provider, Model: decoded.Model, KeyName: decoded.KeyName,
		RouteID: decoded.RouteID, RouteGeneration: decoded.RouteGeneration,
		ProviderConnectionID: decoded.ProviderConnectionID, ProviderConnectionGeneration: decoded.ProviderConnectionGeneration,
		CredentialAssetGeneration: decoded.CredentialAssetGeneration, ModelSelector: decoded.ModelSelector,
		ModelProfile: decoded.ModelProfile,
	}, decoded) {
		t.Fatal("decoded model profile did not bind to the selected route")
	}
	if decoded.ModelProfile == nil || decoded.ModelProfile.ReasoningEffortLevels == nil || len(decoded.ModelProfile.ReasoningEffortLevels) != 0 {
		t.Fatalf("decoded reasoning levels = %#v, want an explicit empty list", decoded.ModelProfile)
	}
	legacyRequest := req
	legacyRequest.ModelProfileSupported = false
	if _, err := ParseSelectionResponse(legacyRequest, body); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("descriptor on an unsupported request error = %v, want ErrProviderRouteInvalid", err)
	}

	legacy := selected
	legacy.ModelProfile = nil
	legacyBody, err := MarshalSelectionResponse(req, legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyDecoded, err := ParseSelectionResponse(req, legacyBody)
	if err != nil || legacyDecoded.ModelProfile != nil {
		t.Fatalf("legacy response decode profile=%#v err=%v, want nil profile", legacyDecoded.ModelProfile, err)
	}
}

func TestRouteContractRejectsUnsupportedModelProfile(t *testing.T) {
	req := llm.ProviderRouteRequest{
		RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "model",
	}
	selected := llm.SelectedProviderRoute{
		Provider: "openai", Model: "model", KeyName: "key", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		ExpiresAt: time.Now().Add(time.Minute), ModelProfile: &llm.ProviderModelProfile{
			ContextWindowTokens: 100, MaxOutputTokens: 101,
		},
	}
	if _, err := MarshalSelectionResponse(req, selected); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("invalid profile marshal error = %v, want ErrProviderRouteInvalid", err)
	}
}
