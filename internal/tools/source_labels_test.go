package tools

import "testing"

func TestReadableToolNameAndSourceLabel(t *testing.T) {
	t.Parallel()
	user := Tool{Name: "crm~u-0123456789abcdef0123456789abcdef_add_leads", Source: "crm~u-0123456789abcdef0123456789abcdef", Transport: TransportMCP}
	agent := Tool{Name: "docs~a-00112233445566778899aabbccddeeff_read", Source: "docs~a-00112233445566778899aabbccddeeff", SourceTitle: "Team/<Docs>\n", Transport: TransportMCP}
	local := Tool{Name: "unit_convert"}
	unprefixed := Tool{Name: "notes.find", Source: "notes", Transport: TransportHTTP}
	for _, tc := range []struct {
		tool            Tool
		readable, label string
	}{
		{user, "add_leads", "crm"},
		{agent, "read", "Team Docs"},
		{local, "unit_convert", "built-in"},
		{unprefixed, "notes.find", "notes"},
	} {
		if got := ReadableToolName(tc.tool); got != tc.readable {
			t.Errorf("ReadableToolName(%q) = %q, want %q", tc.tool.Name, got, tc.readable)
		}
		if got := SourceLabel(tc.tool); got != tc.label {
			t.Errorf("SourceLabel(%q) = %q, want %q", tc.tool.Name, got, tc.label)
		}
	}
	for query, want := range map[string]bool{
		"crm/add_leads":            true,
		"crm (personal)/add_leads": true,
		"crm (crm)/add_leads":      true,
		"crm (agent)/add_leads":    false,
		"crm/add_lead":             false,
		"add_leads":                false,
		"other/add_leads":          false,
	} {
		if got := MatchesQualifiedToolName(user, query); got != want {
			t.Errorf("MatchesQualifiedToolName(%q) = %t, want %t", query, got, want)
		}
	}
	if !MatchesQualifiedToolName(agent, "Team Docs (agent)/read") {
		t.Error("tier-qualified agent label did not match")
	}
}
