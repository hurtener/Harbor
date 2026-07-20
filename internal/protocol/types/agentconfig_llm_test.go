package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestSetLLMProviderWire_HasNoSinkField asserts the writable inference
// provider descriptor exposes NO field that is a URL or an env-var name — the
// structural form of the credential-plane invariant (D-300/D-303): no
// admin-writable field may determine where a credential is sourced. This is
// its OWN reflective test (not shared with the OAuth descriptor). The allowed
// field set is exactly {name, provider, credential_source, inference_broker,
// model_allow}; any URL/env/secret field would be a sink and must not exist
// on the struct.
func TestSetLLMProviderWire_HasNoSinkField(t *testing.T) {
	allowed := map[string]struct{}{
		"name": {}, "provider": {}, "credential_source": {}, "inference_broker": {}, "model_allow": {},
	}
	forbiddenSubstrings := []string{"url", "_env", "secret", "token_", "auth_", "client_id", "client_secret", "credential_url"}

	rt := reflect.TypeOf(AgentConfigLLMProviderDescriptor{})
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if _, ok := allowed[name]; !ok {
			t.Fatalf("descriptor field %q (json %q) is not in the allowed zero-URL set %v — a new field must be proven non-sink", rt.Field(i).Name, name, llmKeys(allowed))
		}
		low := strings.ToLower(name)
		for _, bad := range forbiddenSubstrings {
			if strings.Contains(low, bad) {
				t.Fatalf("descriptor field %q contains forbidden sink substring %q — no URL/env/secret field may exist on the writable descriptor (D-300)", name, bad)
			}
		}
	}
}

func llmKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
