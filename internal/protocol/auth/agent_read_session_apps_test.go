package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAgentReadSessionApps(t *testing.T) {
	reach := []string{"agent-a"}
	if got, err := ParseAgentReadSessionApps(nil, nil); got || err != nil {
		t.Fatalf("absent = %v, %v", got, err)
	}
	if got, err := ParseAgentReadSessionApps(true, reach); !got || err != nil {
		t.Fatalf("true = %v, %v", got, err)
	}
	if got, err := ParseAgentReadSessionApps(false, nil); got || err != nil {
		t.Fatalf("false = %v, %v", got, err)
	}
	for _, bad := range []struct {
		raw   any
		reach []string
	}{{true, nil}, {"true", reach}, {1.0, reach}, {[]any{true}, reach}} {
		if _, err := ParseAgentReadSessionApps(bad.raw, bad.reach); !errors.Is(err, ErrAgentReadReachMalformed) {
			t.Fatalf("Parse(%v, %v) err = %v", bad.raw, bad.reach, err)
		}
	}
	if AgentReadSessionAppsAllowed(context.Background()) {
		t.Fatal("absent opt-in reads as allowed")
	}
	if !AgentReadSessionAppsAllowed(WithAgentReadSessionApps(context.Background(), true)) {
		t.Fatal("seated opt-in not visible")
	}
}

// TestParticipantRoutesIncludeSessionApps pins D-510: a shared-session
// participant may reach the App routes (each handler then requires the App to
// be on the shared session's ledger), and still nothing partition-wide.
func TestParticipantRoutesIncludeSessionApps(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/control/mcp.servers.read_resource":        true,
		"/v1/control/mcp.apps.call_tool":               true,
		"/v1/control/mcp.apps.tool_context":            true,
		"/v1/control/mcp.servers.resources":            false,
		"/v1/tools/list":                               false,
		"/v1/agent_config/user/reconcile_live_profile": false,
		"/v1/sessions/list":                            false,
	} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		if got := participantRouteAllowed(r); got != want {
			t.Fatalf("participant route %s = %v, want %v", path, got, want)
		}
	}
}
