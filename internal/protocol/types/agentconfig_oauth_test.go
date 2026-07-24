package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestSetOAuthProviderWire_FieldSetIsClosed pins the writable OAuth provider
// descriptor to a CLOSED field set. By default it is the name-only shape
// {name, driver, credential_source, credential_broker, scopes} (D-300/D-303).
// The dev-gated wire fields {token_url, audience} (D-340) — the NEW server's
// OAuth params — are also permitted BUT are enforced at validation behind the
// fail-closed `tools.allow_wire_oauth_descriptor` opt-in; with the opt-in off
// either is rejected loud, so their presence on the struct does NOT weaken the
// credential-plane invariant by default. A NEW field that is not in this closed
// set fails the test — a new sink must be proven safe (and gated) before it lands.
func TestSetOAuthProviderWire_FieldSetIsClosed(t *testing.T) {
	allowed := map[string]struct{}{
		// Name-only shape (D-303) — credential_broker required in BOTH shapes.
		"name": {}, "driver": {}, "credential_source": {}, "credential_broker": {}, "scopes": {},
		// Dev-gated wire fields (D-340): the NEW server's token endpoint + audience.
		// The downstream sink is DERIVED (no `allowed_downstream_hosts`) and the
		// runtime's OWN credential source stays boot-declared on the named broker
		// (no `remote`, no env-var names).
		"token_url": {}, "audience": {},
	}
	rt := reflect.TypeOf(AgentConfigOAuthProviderDescriptor{})
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if _, ok := allowed[name]; !ok {
			t.Fatalf("descriptor field %q (json %q) is not in the closed set %v — a new field is a potential sink and must be proven safe + gated (D-340)", rt.Field(i).Name, name, keys(allowed))
		}
	}
}

// TestSetOAuthProviderWire_NoCredentialSourceOrRawHostField asserts the fields
// that must NEVER be wire-writable — EVEN behind the D-340 opt-in — are absent:
// any credential-SOURCE URL or env-var name (the runtime's own credential custody
// stays 100% boot-declared on the named credential_broker; no `remote`, no
// `client_*_env`, no `auth_url`), and a raw `allowed_downstream_hosts` list (the
// downstream sink is DERIVED from the connected server's URL, never wire-chosen).
// This is the structural proof that a self-contained wire credential-pull — the
// exfil surface — cannot exist.
func TestSetOAuthProviderWire_NoCredentialSourceOrRawHostField(t *testing.T) {
	forbidden := map[string]struct{}{
		"remote": {}, "client_id_env": {}, "client_secret_env": {}, "auth_url": {},
		"auth_token_env": {}, "allowed_downstream_hosts": {},
	}
	rt := reflect.TypeOf(AgentConfigOAuthProviderDescriptor{})
	for i := range rt.NumField() {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if _, bad := forbidden[name]; bad {
			t.Fatalf("descriptor field %q must NEVER be wire-writable (the runtime credential source stays boot-declared on the named broker; the downstream sink is DERIVED, never a wire list) — D-340", name)
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
