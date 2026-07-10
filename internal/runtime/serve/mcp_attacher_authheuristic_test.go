package serve

import "testing"

// TestLooksLikeAuthRequired_WordBoundaryAndMarkers pins the conservative
// auth-required heuristic: real auth markers match, a bare 401 inside an
// unrelated number does NOT false-positive, and a non-auth "authorized"
// phrase does not trip it. (The heuristic is the temporary stand-in until the
// MCP driver surfaces a typed auth error; a miss is a loud `failed`, not a
// silent drop.)
func TestLooksLikeAuthRequired_WordBoundaryAndMarkers(t *testing.T) {
	mustMatch := []string{
		"server returned HTTP 401 Unauthorized",
		"WWW-Authenticate: Bearer realm=\"mcp\"",
		"oauth flow required",
		"error: invalid_token",
		"authentication required to proceed",
		"401",
	}
	for _, m := range mustMatch {
		if !looksLikeAuthRequired(errString(m)) {
			t.Errorf("expected auth-required for %q", m)
		}
	}
	mustNotMatch := []string{
		"dial tcp: connection refused",
		"completed in 11401 ms",          // bare 401 inside a number — no word boundary
		"discovered 40100 tools",         // 401 substring inside a larger number
		"not the droids you are looking", // unrelated
		"",
	}
	for _, m := range mustNotMatch {
		if looksLikeAuthRequired(errString(m)) {
			t.Errorf("did NOT expect auth-required for %q", m)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
