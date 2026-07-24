package stream

import (
	"fmt"
	"strings"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestSetOAuthProvider_RejectsUnknownSinkFields proves the wire decode path
// (DisallowUnknownFields) STILL rejects a set_oauth_provider descriptor carrying a
// credential-sink / secret field that is NOT on the struct — BY NAME. After
// D-340 the deliberate wire fields (token_url / audience) are known fields (gated
// at validation, see the agentcfg gate tests), but any credential-SOURCE URL /
// env-var name — `remote`, `auth_token_env`, `client_*_env`, `auth_url` — and a
// raw `allowed_downstream_hosts` list have no field to land in, so the decode
// fails loud naming it. This is the structural proof the self-contained wire
// credential-pull exfil surface cannot exist.
func TestSetOAuthProvider_RejectsUnknownSinkFields(t *testing.T) {
	forbidden := []string{"remote", "auth_token_env", "auth_url", "client_id_env", "client_secret_env", "allowed_downstream_hosts"}
	for _, field := range forbidden {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{"agent_id":"a1","provider":{"name":"m365","driver":"tokenexchange","credential_source":"remote","credential_broker":"b1","%s":"https://attacker.example/token"}}`, field)
			var req prototypes.AgentConfigSetOAuthProviderRequest
			err := decodeGovernanceBody([]byte(body), &req)
			if err == nil {
				t.Fatalf("field %q must be REJECTED at decode (not on the struct), got nil error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("reject error must NAME the offending field %q, got %q", field, err.Error())
			}
		})
	}
}

// TestSetOAuthProvider_WireFieldsDecodeButGateAtValidation proves the D-340
// subtlety: token_url / audience are now KNOWN fields, so DisallowUnknownFields no
// longer rejects them at DECODE — the fail-closed rejection moved to the
// validation gate (ErrWireDescriptorNotAllowed, pinned in the agentcfg gate tests
// + phase-199 smoke). A decode test that expected a decode-time reject here would
// silently stop guarding the exfil path. The wire descriptor still NAMES a
// boot-declared credential_broker (no credential-source URL on the wire).
func TestSetOAuthProvider_WireFieldsDecodeButGateAtValidation(t *testing.T) {
	body := `{"agent_id":"a1","provider":{"name":"m365","driver":"tokenexchange","credential_source":"remote","credential_broker":"m365-broker","token_url":"https://broker/token","audience":"https://graph"}}`
	var req prototypes.AgentConfigSetOAuthProviderRequest
	if err := decodeGovernanceBody([]byte(body), &req); err != nil {
		t.Fatalf("D-340 wire fields must DECODE (the gate rejects them at validation, not decode), got %v", err)
	}
	if req.Provider.TokenURL == "" || req.Provider.CredentialBroker == "" {
		t.Fatalf("wire fields must be decoded onto the struct, got %+v", req.Provider)
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
