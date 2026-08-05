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
