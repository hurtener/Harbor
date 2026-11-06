package agentcfg

import "testing"

func TestSignedOAuthMCPPairFingerprint_PreparationIsImmutableAndFalsePreservesLegacy(t *testing.T) {
	connection := SignedOAuthMCPConnectionDescriptor{
		Name: "receiver", URL: "https://receiver.example.test/mcp",
		Injection: &MCPCredentialInjectionDescriptor{Provider: "provider", Form: "header", Header: "X-Receiver-Api-Key"},
	}
	binding := SignedOAuthMCPBinding{TenantID: "t", AgentID: "a", Connection: connection}
	const legacy = "3281bc810f62dd05653c39f0ae87093b2948d3fd6c3a8b626cb77275a243d267"
	if got := SignedOAuthMCPPairFingerprint(binding); got != legacy {
		t.Fatalf("false preparation changed legacy fingerprint: %s", got)
	}
	authenticated := connection
	authenticated.Injection = connection.Injection.Clone()
	authenticated.Injection.AuthenticatePreparation = true
	if sameSignedOAuthMCPConnection(connection, authenticated) {
		t.Fatal("preparation authority not compared")
	}
	binding.Connection = authenticated
	if SignedOAuthMCPPairFingerprint(binding) == legacy {
		t.Fatal("preparation authority not fingerprinted")
	}
}
