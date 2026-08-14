package types

import (
	"reflect"
	"testing"
)

// TestMCPAppCallToolRequest_ServerIDIsHostDerived pins the HA-56 amendment
// of the D-227 item 3 / D-351 confinement design: the app→host tool-call
// request MAY carry a `server_id`, and that field is authoritative
// HOST-DERIVED context — never a value an App supplies about itself. The
// dispatch check (mcpconsole.AppsAccessor.CallTool) verifies it against
// the resolved tool's actual source and resolves an app-only callback
// (`_meta.ui.visibility: ["app"]`) ONLY through the named server's App
// dispatch catalog, so a server_id a caller "took from the App" cannot
// select another server's callback: a mismatched or missing identity is
// refused before execution. The field is OPTIONAL (`omitempty`) — an
// absent value keeps the pre-catalog behavior for ordinary tools and
// legacy clients, and an app-only callback without its own server answers
// not-found.
func TestMCPAppCallToolRequest_ServerIDIsHostDerived(t *testing.T) {
	rt := reflect.TypeOf(MCPAppCallToolRequest{})
	f, ok := rt.FieldByName("ServerID")
	if !ok {
		t.Fatal("MCPAppCallToolRequest lost its host-derived ServerID field (HA-56): the dispatch check cannot verify an App call's server identity without it")
	}
	if tag := f.Tag.Get("json"); tag != "server_id,omitempty" {
		t.Fatalf("ServerID json tag = %q, want %q (optional, additive — an absent value keeps legacy behavior)", tag, "server_id,omitempty")
	}
	// Guard the guard: the fields the request carries are identity + the
	// runtime-authored agent binding + the host-derived server identity +
	// the already-qualified tool name + arguments. AgentID is resource
	// authority, not a server namespace; ServerID is the host-derived
	// server namespace, verified at dispatch, never trusted as supplied.
	// RenderAdmission is the DISTINCT HA-56 render-admission authority —
	// never the legacy Binding (a request supplying both is refused as
	// ambiguous).
	want := map[string]bool{"Identity": true, "AgentID": true, "ServerID": true, "Binding": true, "RenderAdmission": true, "ResourceURI": true, "Tool": true, "Arguments": true}
	got := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		got[rt.Field(i).Name] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("MCPAppCallToolRequest field set changed: got %v, want %v", got, want)
	}
}
