package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestSetOAuthProviderWire_FieldSetIsClosed pins the writable OAuth provider
// descriptor to a CLOSED field set. By default it is the zero-URL name-only shape
// {name, driver, credential_source, credential_broker, scopes} (D-300/D-303).
// The dev-gated wire fields {token_url, audience, remote} (D-340) are also
// permitted BUT are enforced at validation behind the fail-closed
// `tools.allow_wire_oauth_descriptor` opt-in — with the opt-in off any of them is
// rejected loud, so their presence on the struct does NOT weaken the
// credential-plane invariant by default. A NEW field that is not in this closed
// set fails the test — a new sink must be proven safe (and gated) before it lands.
func TestSetOAuthProviderWire_FieldSetIsClosed(t *testing.T) {
	allowed := map[string]struct{}{
		// Zero-URL name-only shape (D-303).
		"name": {}, "driver": {}, "credential_source": {}, "credential_broker": {}, "scopes": {},
		// Dev-gated wire fields (D-340) — accepted only behind the opt-in; the
		// downstream sink is DERIVED, never wire, so no `allowed_downstream_hosts`.
		"token_url": {}, "audience": {}, "remote": {},
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

// TestSetOAuthProviderWire_NoBrokerSecretOrRawHostField asserts the fields that
// must NEVER be wire-writable — EVEN behind the D-340 opt-in — are absent: a
// broker client-secret env name (the runtime broker credential stays boot-declared,
// no secret ever rides the wire) and a raw `allowed_downstream_hosts` list (the
// downstream sink is DERIVED from the connected server's URL, never wire-chosen).
func TestSetOAuthProviderWire_NoBrokerSecretOrRawHostField(t *testing.T) {
	forbidden := map[string]struct{}{
		"client_id_env": {}, "client_secret_env": {}, "auth_url": {}, "allowed_downstream_hosts": {},
	}
	rt := reflect.TypeOf(AgentConfigOAuthProviderDescriptor{})
	for i := range rt.NumField() {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if _, bad := forbidden[name]; bad {
			t.Fatalf("descriptor field %q must NEVER be wire-writable (the broker secret stays boot-declared; the downstream sink is DERIVED, never a wire list) — D-340", name)
		}
	}
	// The wire Remote block carries only a URL + an env-var NAME (no secret): a
	// literal client_secret / client_id field would be a secret on the wire.
	rr := reflect.TypeOf(AgentConfigOAuthRemoteDescriptor{})
	for i := range rr.NumField() {
		name := strings.Split(rr.Field(i).Tag.Get("json"), ",")[0]
		low := strings.ToLower(name)
		if strings.Contains(low, "secret") || low == "client_id" || low == "client_secret" {
			t.Fatalf("remote descriptor field %q would put a secret on the wire — only a url + an env-var NAME are permitted (D-340)", name)
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
