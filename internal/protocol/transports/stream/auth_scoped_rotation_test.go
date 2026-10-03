package stream_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

func TestAuthHandler_RestrictedAdminCannotRotateIntoBroadToken(t *testing.T) {
	handler := newAuthHandler(t)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exercise the real AuthHandler reconstruction of Verified: only
		// identity/scopes are copied into that value, so the transport-
		// independent RotateSurface must retain the context restriction.
		ctx := auth.WithMethodReach(r.Context(), []methods.Method{methods.MethodAuthRotateToken})
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	code, body := doAuthRequest(t, wrapped, "rotate_token", "{}", &authHandlerID, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusForbidden || !strings.Contains(string(body), "scope_mismatch") || strings.Contains(string(body), "new_token") {
		t.Fatalf("restricted admin rotation = %d: %s", code, body)
	}
}
