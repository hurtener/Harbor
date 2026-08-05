package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestMCPAppCallToolRequest_CarriesNoServerScope pins the confinement design
// (D-227 item 3, re-affirmed by D-351): the MCP server an App is confined to is
// HOST-DERIVED, and the app→host tool-call wire request therefore carries no
// server-scope field at all.
//
// The property this protects is not "the field is missing" for its own sake. If
// the request grew a `server_id`, the Console would have to populate it, and a
// populated field is a field some future caller can be talked into taking from
// the App — which is exactly the influence the confinement forbids. The
// namespace arrives with the App reference (the backend-minted `server_id` on
// `mcp.app_available`) and is applied host-side; the App chooses only the
// suffix. Adding a server scope here is a design change that belongs in an RFC
// PR and a new decision, not a field.
func TestMCPAppCallToolRequest_CarriesNoServerScope(t *testing.T) {
	rt := reflect.TypeOf(MCPAppCallToolRequest{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if strings.Contains(strings.ToLower(f.Name), "server") ||
			strings.Contains(strings.ToLower(tag), "server") {
			t.Fatalf("MCPAppCallToolRequest gained a server-scope field %q (json %q): "+
				"an app→host tool call's server namespace is host-derived and must never "+
				"travel on the wire where a caller could supply it", f.Name, tag)
		}
	}
	// Guard the guard: the fields the request DOES carry are identity + the
	// runtime-authored agent binding + the already-qualified tool name +
	// arguments. AgentID is resource authority, not a server namespace. A
	// rename that dodged the scan above would also drop one of these.
	want := map[string]bool{"Identity": true, "AgentID": true, "Tool": true, "Arguments": true}
	got := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		got[rt.Field(i).Name] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("MCPAppCallToolRequest field set changed: got %v, want %v", got, want)
	}
}
