package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestSetOAuthProviderWire_HasNoSinkField asserts the writable OAuth provider
// descriptor exposes NO field that is a URL or an env-var name — the structural
// form of the credential-plane invariant (D-300/D-303): no admin-writable field
// may determine where a credential is sent. The allowed field set is exactly
// {name, driver, credential_source, credential_broker, scopes}; any URL/env
// field (token_url, auth_url, client_id_env, client_secret_env, remote) would be
// a sink and must not exist on the struct.
func TestSetOAuthProviderWire_HasNoSinkField(t *testing.T) {
	allowed := map[string]struct{}{
		"name": {}, "driver": {}, "credential_source": {}, "credential_broker": {}, "scopes": {},
	}
	forbiddenSubstrings := []string{"url", "_env", "secret", "token_", "auth_", "remote", "client_id", "client_secret"}

	rt := reflect.TypeOf(AgentConfigOAuthProviderDescriptor{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if _, ok := allowed[name]; !ok {
			t.Fatalf("descriptor field %q (json %q) is not in the allowed zero-URL set %v — a new field must be proven non-sink", rt.Field(i).Name, name, keys(allowed))
		}
		low := strings.ToLower(name)
		for _, bad := range forbiddenSubstrings {
			if strings.Contains(low, bad) {
				t.Fatalf("descriptor field %q contains forbidden sink substring %q — no URL/env/secret field may exist on the writable descriptor (D-300)", name, bad)
			}
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
