package agentcfg

import "testing"

func TestSignedOAuthMCPPairView_DeepClonesInjection(t *testing.T) {
	original := &SignedOAuthMCPPair{
		Connection: SignedOAuthMCPConnectionDescriptor{
			Injection: &MCPCredentialInjectionDescriptor{
				Provider: "provider", Form: "header", Header: "x-bamboohr-api-key",
			},
		},
	}
	payload := ConfigPayload{SignedOAuthMCPPair: original}
	view, ok := payload.SignedOAuthMCPPairView()
	if !ok || view.Connection.Injection == nil {
		t.Fatalf("SignedOAuthMCPPairView = ok %t, view %+v", ok, view)
	}

	view.Connection.Injection.Header = "x-mutated-api-key"
	if original.Connection.Injection.Header != "x-bamboohr-api-key" {
		t.Fatalf("view mutation aliased immutable pair injection: original = %+v", original.Connection.Injection)
	}
	original.Connection.Injection.Provider = "mutated-provider"
	if view.Connection.Injection.Provider != "provider" {
		t.Fatalf("source mutation aliased defensive view injection: view = %+v", view.Connection.Injection)
	}
}

func TestEffectiveSignedOAuthMCPPairs_StrictLegacyMapUnion(t *testing.T) {
	legacy := &SignedOAuthMCPPair{ProviderName: "legacy", Connection: SignedOAuthMCPConnectionDescriptor{Name: "legacy-connection"}}
	pairs := SignedOAuthMCPPairs{"modern": {ProviderName: "modern", Connection: SignedOAuthMCPConnectionDescriptor{Name: "modern-connection"}}}
	payload := ConfigPayload{SignedOAuthMCPPair: legacy, SignedOAuthMCPPairs: &pairs}
	effective, err := payload.EffectiveSignedOAuthMCPPairs()
	if err != nil || len(effective) != 2 || effective["legacy"].Connection.Name != "legacy-connection" || effective["modern"].Connection.Name != "modern-connection" {
		t.Fatalf("effective union = %+v err=%v", effective, err)
	}
	effective["modern"].Connection.Name = "mutated"
	if (*payload.SignedOAuthMCPPairs)["modern"].Connection.Name != "modern-connection" {
		t.Fatal("effective union aliases stored pair")
	}

	duplicate := SignedOAuthMCPPairs{"legacy": {ProviderName: "legacy"}}
	if _, err := (ConfigPayload{SignedOAuthMCPPair: legacy, SignedOAuthMCPPairs: &duplicate}).EffectiveSignedOAuthMCPPairs(); err == nil {
		t.Fatal("duplicate legacy/map provider accepted")
	}
	mismatch := SignedOAuthMCPPairs{"key": {ProviderName: "other"}}
	if _, err := (ConfigPayload{SignedOAuthMCPPairs: &mismatch}).EffectiveSignedOAuthMCPPairs(); err == nil {
		t.Fatal("map key/provider mismatch accepted")
	}
}

func TestContentHash_LegacySignedOAuthMCPPairUnchangedByCollectionSupport(t *testing.T) {
	payload := ConfigPayload{SignedOAuthMCPPair: &SignedOAuthMCPPair{ProviderName: "legacy", Broker: "broker"}}
	got, err := ContentHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	const legacyV1269Hash = "e1f9409853bd2398d0dc473dfcbf27352e04ab442afc5f0f3677b859c3881aa4"
	if got != legacyV1269Hash {
		t.Fatalf("legacy content hash = %q, want v1.26.9 %q", got, legacyV1269Hash)
	}
}

func TestEffectiveSignedOAuthMCPPairs_RejectsNonCanonicalProviderValue(t *testing.T) {
	pairs := SignedOAuthMCPPairs{"provider": {ProviderName: " provider"}}
	if _, err := (ConfigPayload{SignedOAuthMCPPairs: &pairs}).EffectiveSignedOAuthMCPPairs(); err == nil {
		t.Fatal("whitespace-bearing provider_name matched a canonical map key")
	}
}

func TestContentHash_SignedOAuthMCPPairsIgnoresMapInsertionOrder(t *testing.T) {
	left := make(SignedOAuthMCPPairs)
	left["alpha"] = SignedOAuthMCPPair{ProviderName: "alpha", Broker: "broker-a"}
	left["beta"] = SignedOAuthMCPPair{ProviderName: "beta", Broker: "broker-b"}
	right := make(SignedOAuthMCPPairs)
	right["beta"] = SignedOAuthMCPPair{ProviderName: "beta", Broker: "broker-b"}
	right["alpha"] = SignedOAuthMCPPair{ProviderName: "alpha", Broker: "broker-a"}

	leftHash, err := ContentHash(ConfigPayload{SignedOAuthMCPPairs: &left})
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := ContentHash(ConfigPayload{SignedOAuthMCPPairs: &right})
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("map insertion order changed content hash: left=%q right=%q", leftHash, rightHash)
	}
}
