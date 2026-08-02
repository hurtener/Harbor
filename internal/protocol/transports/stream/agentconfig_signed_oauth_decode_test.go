package stream_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestAgentConfigHandler_RegisterOAuthMCPCapabilityRejectsForbiddenFieldsWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		value     string
		container string
	}{
		{name: "token URL", field: "token_url", value: `"https://evil.test/token"`},
		{name: "credential URL", field: "credential_url", value: `"https://evil.test/credential"`},
		{name: "auth token env", field: "auth_token_env", value: `"SECRET_ENV"`},
		{name: "environment", field: "env", value: `{"SECRET":"value"}`},
		{name: "secret", field: "secret", value: `"value"`},
		{name: "allowed downstream hosts", field: "allowed_downstream_hosts", value: `["evil.test"]`},
		{name: "downstream hosts", field: "downstream_hosts", value: `["evil.test"]`},
		{name: "hosts", field: "hosts", value: `["evil.test"]`},
		{name: "headers", field: "headers", value: `{"Authorization":"Bearer value"}`, container: "connection"},
		{name: "command", field: "command", value: `["sh"]`, container: "connection"},
		{name: "credential source", field: "credential_source", value: `"remote"`},
		{name: "OAuth provider", field: "oauth_provider", value: `"other"`, container: "connection"},
		{name: "injection", field: "injection", value: `{"target":"header"}`, container: "connection"},
		{name: "discovery origins", field: "oauth_discovery_allowed_origins", value: `["https://evil.test"]`, container: "connection"},
		{name: "meta annotations", field: "meta_annotations", value: `{"readOnlyHint":true}`, container: "connection"},
		{name: "client secret", field: "client_secret", value: `"value"`},
		{name: "client ID env", field: "client_id_env", value: `"CLIENT_ID"`},
		{name: "client secret env", field: "client_secret_env", value: `"CLIENT_SECRET"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := sessionHandler(t)
			extra := fmt.Sprintf(`,%q:%s`, tc.field, tc.value)
			connectionExtra := ""
			if tc.container == "connection" {
				connectionExtra, extra = extra, ""
			}
			body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},` +
				`"agent_id":"agent-x","provider_name":"provider","broker":"broker",` +
				`"audience":"audience","scopes":["read"],` +
				`"connection":{"name":"server","url":"https://mcp.example.test/mcp"` + connectionExtra + `},` +
				`"authority_envelope":"signed"` + extra + `}`
			code, resp := acReq(t, h, "register_oauth_mcp_capability", body, acID(), []auth.Scope{auth.ScopeAdmin})
			if code != http.StatusBadRequest {
				t.Fatalf("forbidden field %q status = %d, want 400; body=%s", tc.field, code, resp)
			}
			if !strings.Contains(string(resp), tc.field) {
				t.Fatalf("forbidden field refusal does not identify %q: %s", tc.field, resp)
			}

			getBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`
			getCode, getResp := acReq(t, h, "get", getBody, acID(), []auth.Scope{auth.ScopeAdmin})
			if getCode != http.StatusOK {
				t.Fatalf("get after refusal status = %d; body=%s", getCode, getResp)
			}
			var got prototypes.AgentConfigGetResponse
			if err := json.Unmarshal(getResp, &got); err != nil {
				t.Fatalf("decode get response: %v", err)
			}
			if got.Set || got.Revision != nil {
				t.Fatalf("forbidden field %q caused an agent-config side effect: %+v", tc.field, got)
			}
		})
	}
}
