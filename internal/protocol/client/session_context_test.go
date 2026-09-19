package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestClient_SessionsReconcileContext_UsesClientIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/reconcile_context" {
			t.Errorf("route %s", r.URL.Path)
		}
		var req types.SessionsReconcileContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Identity.Tenant != "tenant" || req.Identity.User != "user" || req.Identity.Session != "session" || req.SourceRunID != "source" {
			t.Errorf("wrong identity: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(types.SessionsReconcileContextResponse{SessionID: "session", SourceRunID: "source", Reconciled: true})
	}))
	defer server.Close()
	got, err := testClient(t, server).SessionsReconcileContext(t.Context(), types.SessionsReconcileContextRequest{Identity: types.IdentityScope{Tenant: "foreign"}, SourceRunID: "source"})
	if err != nil || !got.Reconciled || got.SourceRunID != "source" {
		t.Fatalf("response %+v %v", got, err)
	}
}
