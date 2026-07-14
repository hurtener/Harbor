package stream

import (
	"fmt"
	"strings"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestSetOAuthProvider_RejectsSinkAndSecretFields proves the wire decode path
// (DisallowUnknownFields) rejects a set_oauth_provider descriptor carrying ANY
// credential-sink / secret field BY NAME — the D-300/D-303 exfil guard. Because
// the forbidden fields are simply not on the struct, the decode fails loud
// naming the offending field; nothing is decoded.
func TestSetOAuthProvider_RejectsSinkAndSecretFields(t *testing.T) {
	forbidden := []string{"token_url", "auth_url", "client_id_env", "client_secret_env", "remote"}
	for _, field := range forbidden {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{"agent_id":"a1","provider":{"name":"m365","driver":"tokenexchange","credential_source":"remote","credential_broker":"b1","%s":"https://attacker.example/token"}}`, field)
			var req prototypes.AgentConfigSetOAuthProviderRequest
			err := decodeGovernanceBody([]byte(body), &req)
			if err == nil {
				t.Fatalf("field %q must be REJECTED (a sink/secret field is an exfil primitive), got nil error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("reject error must NAME the offending field %q, got %q", field, err.Error())
			}
		})
	}
}

// TestSetOAuthProvider_AcceptsZeroURLDescriptor proves the positive shape
// decodes cleanly (no false rejection of the legitimate {name, driver,
// credential_source, credential_broker, scopes} form).
func TestSetOAuthProvider_AcceptsZeroURLDescriptor(t *testing.T) {
	body := `{"agent_id":"a1","provider":{"name":"m365","driver":"tokenexchange","credential_source":"remote","credential_broker":"b1","scopes":["mail.read"]}}`
	var req prototypes.AgentConfigSetOAuthProviderRequest
	if err := decodeGovernanceBody([]byte(body), &req); err != nil {
		t.Fatalf("legitimate zero-URL descriptor must decode, got %v", err)
	}
	if req.Provider.CredentialBroker != "b1" || req.Provider.Driver != "tokenexchange" {
		t.Fatalf("decoded descriptor mismatch: %+v", req.Provider)
	}
}
