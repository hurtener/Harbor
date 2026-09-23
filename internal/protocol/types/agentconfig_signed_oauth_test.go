package types

import (
	"reflect"
	"strings"
	"testing"
)

func TestRegisterOAuthMCPCapabilityWire_FieldSetsAreClosed(t *testing.T) {
	assertExactJSONFields(t, reflect.TypeOf(AgentConfigRegisterOAuthMCPCapabilityRequest{}), []string{
		"identity", "agent_id", "provider_name", "broker", "audience", "scopes", "connection",
		"expected_content_hash", "authority_envelope",
	})
	assertExactJSONFields(t, reflect.TypeOf(SignedOAuthMCPConnectionDescriptor{}), []string{
		"name", "url", "tool_allowlist", "tool_denylist", "connect_timeout_ms", "request_timeout_ms",
		"injection", "artifact_byte_eligible", "artifact_params",
	})
}

func TestRegisterOAuthMCPCapabilityWire_HasNoCredentialOrSinkConfigurationField(t *testing.T) {
	forbidden := map[string]struct{}{
		"token_url": {}, "credential_url": {}, "auth_token_env": {}, "env": {}, "secret": {},
		"allowed_downstream_hosts": {}, "downstream_hosts": {}, "hosts": {}, "headers": {},
		"command": {}, "credential_source": {}, "oauth_provider": {},
		"oauth_discovery_allowed_origins": {}, "meta_annotations": {},
	}
	for _, rt := range []reflect.Type{
		reflect.TypeOf(AgentConfigRegisterOAuthMCPCapabilityRequest{}),
		reflect.TypeOf(SignedOAuthMCPConnectionDescriptor{}),
	} {
		for i := range rt.NumField() {
			name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if _, bad := forbidden[name]; bad {
				t.Fatalf("%s exposes forbidden signed-capability input %q", rt.Name(), name)
			}
		}
	}
}

func assertExactJSONFields(t *testing.T, rt reflect.Type, expected []string) {
	t.Helper()
	if rt.NumField() != len(expected) {
		t.Fatalf("%s has %d fields, want exact closed set %v", rt.Name(), rt.NumField(), expected)
	}
	want := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		want[name] = struct{}{}
	}
	for i := range rt.NumField() {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := want[name]; !ok {
			t.Fatalf("%s field %q (json %q) is outside exact closed set %v", rt.Name(), rt.Field(i).Name, name, expected)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("%s is missing expected JSON fields %v", rt.Name(), want)
	}
}
