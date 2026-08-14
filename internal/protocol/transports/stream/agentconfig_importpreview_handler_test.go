package stream_test

import (
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// agentconfig_importpreview_handler_test.go — the two-phase verified-caller
// skill-package import routes (HA-61) and the read-only composition
// preview route (HA-66) on the shared agent-config wire handler. Both
// families are CLAIM-FREE (identity-mandatory, no admin / user scope), so
// a no-scope caller reaches the service seam; with the services unwired
// the routes answer 501 (the partial-build convention), and the typed
// error mappings are pinned below.

func TestAgentConfigHandler_ImportPreviewRoutes_UnwiredAnswer501(t *testing.T) {
	h := sessionHandler(t) // fixture wires NO import / preview services
	cases := []struct {
		route string
		body  string
	}{
		{"user/skills/import_validate", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","artifact_id":"art-1"}`},
		{"user/skills/import_commit", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","proposal_token":"tok","name":"n","reviewed_package_hash":"h","expected_content_hash":"e"}`},
		{"composition/preview", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			// CLAIM-FREE: a no-scope caller passes the route gate and
			// reaches the (unwired) service seam → 501, never 403.
			code, body := acReq(t, h, tc.route, tc.body, acID(), []auth.Scope{})
			if code != http.StatusNotImplemented {
				t.Fatalf("unwired %s status = %d, want 501; body=%s", tc.route, code, body)
			}
			if got := errCode(t, body); got != protoerrors.CodeUnknownMethod {
				t.Fatalf("unwired %s code = %q, want %q", tc.route, got, protoerrors.CodeUnknownMethod)
			}
		})
	}
}

func TestAgentConfigHandler_ImportPreviewRoutes_ForeignBodyRejected(t *testing.T) {
	h := sessionHandler(t)
	cases := []struct {
		route string
		body  string
	}{
		{"user/skills/import_validate", `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"agent-x","artifact_id":"art-1"}`},
		{"composition/preview", `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"agent-x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			code, body := acReq(t, h, tc.route, tc.body, acID(), []auth.Scope{})
			if code != http.StatusUnauthorized {
				t.Fatalf("foreign body %s status = %d, want 401; body=%s", tc.route, code, body)
			}
			if got := errCode(t, body); got != protoerrors.CodeIdentityRequired {
				t.Fatalf("foreign body %s code = %q, want %q", tc.route, got, protoerrors.CodeIdentityRequired)
			}
		})
	}
}

// The typed import / preview error mappings + the preserved
// ErrBootPackOwned mapping live in internal_test.go (package stream),
// beside the existing classifyAgentConfigError pin tests.
