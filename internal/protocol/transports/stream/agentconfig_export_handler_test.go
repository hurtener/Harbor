package stream_test

import (
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// Export uses the same claim-free, verified-identity and signed-reach door as
// list even on a partial build whose import/export service is unwired.
func TestAgentConfigHandler_UserSkillExport_VerifiedDoor(t *testing.T) {
	const valid = `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","name":"personal"}`
	for _, tc := range []struct {
		name   string
		body   string
		id     *identity.Identity
		reach  []string
		status int
		code   protoerrors.Code
	}{
		{"unwired", valid, acID(), []string{acAgent}, http.StatusNotImplemented, protoerrors.CodeUnknownMethod},
		{"identity required", valid, nil, []string{acAgent}, http.StatusUnauthorized, protoerrors.CodeIdentityRequired},
		{"foreign body", `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"agent-x","name":"personal"}`, acID(), []string{acAgent}, http.StatusUnauthorized, protoerrors.CodeIdentityRequired},
		{"excluded reach", valid, acID(), []string{"other"}, http.StatusForbidden, protoerrors.CodeScopeMismatch},
		{"empty reach", valid, acID(), []string{}, http.StatusForbidden, protoerrors.CodeScopeMismatch},
		{"authority field", `{"agent_id":"agent-x","name":"personal","scope":"agent"}`, acID(), []string{acAgent}, http.StatusBadRequest, protoerrors.CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := sessionHandler(t)
			status, raw := acReqReach(t, h, "user/skills/export", tc.body, tc.id, []auth.Scope{}, tc.reach)
			if status != tc.status || errCode(t, raw) != tc.code {
				t.Fatalf("export = (%d,%s), want (%d,%s)", status, raw, tc.status, tc.code)
			}
		})
	}
}
