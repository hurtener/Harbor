package stream_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
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
		{name: "injection nested unknown target", field: "target", value: `"header"`, container: "injection"},
		{name: "discovery origins", field: "oauth_discovery_allowed_origins", value: `["https://evil.test"]`, container: "connection"},
		{name: "meta annotations", field: "meta_annotations", value: `{"readOnlyHint":true}`, container: "connection"},
		{name: "client secret", field: "client_secret", value: `"value"`},
		{name: "client ID env", field: "client_id_env", value: `"CLIENT_ID"`},
		{name: "client secret env", field: "client_secret_env", value: `"CLIENT_SECRET"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSessionHandlerFixture(t)
			lifecycleIdentity, lifecycleKind, err := agentcfg.LifecycleSlot("t1", "agent-x")
			if err != nil {
				t.Fatal(err)
			}
			beforeLifecycle, err := fixture.state.Load(t.Context(), lifecycleIdentity, lifecycleKind)
			if err != nil {
				t.Fatalf("load boot lifecycle baseline: %v", err)
			}
			q := identity.Quadruple{Identity: *acID()}
			beforeHistory, err := fixture.registry.ListRevisions(t.Context(), q, "agent-x", agentcfg.ConfigScopeAgent, 0)
			if err != nil {
				t.Fatalf("load revision baseline: %v", err)
			}
			beforeMutations := fixture.mutations.count.Load()
			extra := fmt.Sprintf(`,%q:%s`, tc.field, tc.value)
			connectionExtra := ""
			switch tc.container {
			case "connection":
				connectionExtra, extra = extra, ""
			case "injection":
				connectionExtra, extra = `,"injection":{`+extra[1:]+`}`, ""
			}
			body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},` +
				`"agent_id":"agent-x","provider_name":"provider","broker":"broker",` +
				`"audience":"audience","scopes":["read"],` +
				`"connection":{"name":"server","url":"https://mcp.example.test/mcp"` + connectionExtra + `},` +
				`"authority_envelope":"signed"` + extra + `}`
			code, resp := acReq(t, fixture.handler, "register_oauth_mcp_capability", body, acID(), []auth.Scope{auth.ScopeAdmin})
			if code != http.StatusBadRequest {
				t.Fatalf("forbidden field %q status = %d, want 400; body=%s", tc.field, code, resp)
			}
			if !strings.Contains(string(resp), tc.field) {
				t.Fatalf("forbidden field refusal does not identify %q: %s", tc.field, resp)
			}
			if after := fixture.mutations.count.Load(); after != beforeMutations {
				t.Fatalf("forbidden field %q reached a StateStore mutation: before=%d after=%d", tc.field, beforeMutations, after)
			}

			afterLifecycle, err := fixture.state.Load(t.Context(), lifecycleIdentity, lifecycleKind)
			if err != nil {
				t.Fatalf("load lifecycle after refusal: %v", err)
			}
			if beforeLifecycle.ID != afterLifecycle.ID || beforeLifecycle.Version != afterLifecycle.Version ||
				!beforeLifecycle.UpdatedAt.Equal(afterLifecycle.UpdatedAt) || !bytes.Equal(beforeLifecycle.Bytes, afterLifecycle.Bytes) {
				t.Fatalf("forbidden field %q changed the lifecycle generation: before=%+v after=%+v", tc.field, beforeLifecycle, afterLifecycle)
			}
			afterHistory, err := fixture.registry.ListRevisions(t.Context(), q, "agent-x", agentcfg.ConfigScopeAgent, 0)
			if err != nil {
				t.Fatalf("load revisions after refusal: %v", err)
			}
			if !reflect.DeepEqual(beforeHistory, afterHistory) {
				t.Fatalf("forbidden field %q changed immutable config history: before=%+v after=%+v", tc.field, beforeHistory, afterHistory)
			}

			// The shared handler fixture intentionally starts with the default
			// agent's boot lifecycle materialized. GET is allowed to observe that
			// pre-existing empty revision; it is not evidence that the rejected
			// registration mutated config.
			getBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`
			getCode, getResp := acReq(t, fixture.handler, "get", getBody, acID(), []auth.Scope{auth.ScopeAdmin})
			if getCode != http.StatusOK {
				t.Fatalf("get after refusal status = %d; body=%s", getCode, getResp)
			}
			var got prototypes.AgentConfigGetResponse
			if err := json.Unmarshal(getResp, &got); err != nil {
				t.Fatalf("decode get response: %v", err)
			}
			if !got.Set || got.Revision == nil || len(beforeHistory) != 1 || got.Revision.RevisionID != beforeHistory[0].RevisionID {
				t.Fatalf("GET did not return the unchanged boot lifecycle after forbidden field %q: %+v baseline=%+v", tc.field, got, beforeHistory)
			}
		})
	}
}
