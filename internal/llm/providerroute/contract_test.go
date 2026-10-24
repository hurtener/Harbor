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
		EffectiveAgentID: "agent", TaskID: "task", LogicalCallID: "call",
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
	if !bytes.Contains(requestBody, []byte(`"image_input_supported":true`)) {
		t.Fatalf("selection request omitted image-input negotiation bit: %s", requestBody)
	}
	resolveBody, err := MarshalRequest(req)
	if err != nil || !bytes.Contains(resolveBody, []byte(`"image_input_supported":true`)) {
		t.Fatalf("resolve request omitted image-input negotiation bit: %s (%v)", resolveBody, err)
	}
	if bytes.Contains(requestBody, []byte(`"runtime_id"`)) || bytes.Contains(resolveBody, []byte(`"runtime_id"`)) {
		t.Fatal("request contains removed runtime ID")
	}
	// Resolver-first rollout: an old runtime's removed field is ignored.
	oldBody := append([]byte(`{"runtime_id":"obsolete",`), requestBody[1:]...)
	if _, decoded, err := UnmarshalOperationRequest(oldBody); err != nil || decoded != req {
		t.Fatalf("old runtime request rejected: %v", err)
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
	if bytes.Contains(responseBody, []byte(`"runtime_id"`)) {
		t.Fatal("selection response changed the runtime identity contract")
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
		EffectiveAgentID: "agent", TaskID: "task", LogicalCallID: "call",
		RouteID: "route", RouteGeneration: 2, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 3, CredentialAssetGeneration: 4, ModelSelector: "fast",
		ModelProfileSupported: true,
		Purpose:               llm.ProviderRoutePurposeRun,
	}
	profile := &llm.ProviderModelProfile{
		ContextWindowTokens:   8192,
		MaxOutputTokens:       2048,
		ReasoningEffortLevels: []llm.ReasoningEffort{},
		ImageInput:            boolPtr(false),
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
	if !bytes.Contains(body, []byte(`"image_input":false`)) {
		t.Fatalf("selection response omitted explicit nonvision capability: %s", body)
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
	if decoded.ModelProfile.ImageInput == nil || *decoded.ModelProfile.ImageInput {
		t.Fatalf("decoded image capability = %#v, want explicit false", decoded.ModelProfile.ImageInput)
	}
	legacyRequest := req
	legacyRequest.ModelProfileSupported = false
	legacyBodyRequest, err := MarshalSelectionRequest(legacyRequest)
	if err != nil || bytes.Contains(legacyBodyRequest, []byte(`"image_input_supported"`)) {
		t.Fatalf("legacy request negotiated image input: %s (%v)", legacyBodyRequest, err)
	}
	if _, err := ParseSelectionResponse(legacyRequest, body); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("descriptor on an unsupported request error = %v, want ErrProviderRouteInvalid", err)
	}
	if _, err := MarshalSelectionResponse(legacyRequest, selected); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("selection marshal on an unsupported request error = %v, want ErrProviderRouteInvalid", err)
	}
	resolved := llm.ResolvedProviderRoute{
		Provider: selected.Provider, Model: selected.Model, KeyName: selected.KeyName,
		RouteID: selected.RouteID, RouteGeneration: selected.RouteGeneration,
		ProviderConnectionID: selected.ProviderConnectionID, ProviderConnectionGeneration: selected.ProviderConnectionGeneration,
		CredentialAssetGeneration: selected.CredentialAssetGeneration, ModelSelector: selected.ModelSelector,
		ExpiresAt: selected.ExpiresAt, Credential: "credential", ModelProfile: selected.ModelProfile,
	}
	if _, err := MarshalResponse(legacyRequest, resolved); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("resolve marshal on an unsupported request error = %v, want ErrProviderRouteInvalid", err)
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

func boolPtr(v bool) *bool { return &v }

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
			ContextWindowTokens: 100, MaxOutputTokens: 100,
		},
	}
	if _, err := MarshalSelectionResponse(req, selected); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("invalid profile marshal error = %v, want ErrProviderRouteInvalid", err)
	}
}

func TestRouteContractRejectsUnprovenDefaultReasoning(t *testing.T) {
	if err := llm.ValidateProviderModelProfile(llm.ProviderModelProfile{
		ContextWindowTokens: 8192,
		MaxOutputTokens:     1024,
		ReasoningEffort:     llm.ReasoningLow,
	}); !errors.Is(err, llm.ErrProviderRouteInvalid) {
		t.Fatalf("unknown-support default validation error = %v, want ErrProviderRouteInvalid", err)
	}
	for _, levels := range [][]llm.ReasoningEffort{nil, {}} {
		if err := llm.ValidateProviderModelProfile(llm.ProviderModelProfile{
			ContextWindowTokens:   8192,
			MaxOutputTokens:       1024,
			ReasoningEffort:       llm.ReasoningOff,
			ReasoningEffortLevels: levels,
		}); err != nil {
			t.Fatalf("off default with levels=%#v rejected: %v", levels, err)
		}
	}
}
