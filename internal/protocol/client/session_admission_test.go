package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestClient_SessionsSetAdmission_UsesExactClientIdentity(t *testing.T) {
	var got types.SessionsSetAdmissionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/set_admission" || r.Method != http.MethodPost {
			t.Errorf("wrong canonical route: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"epoch":4,"protocol_version":"0.1.0"}`))
	}))
	defer server.Close()
	out, err := testClient(t, server).SessionsSetAdmission(t.Context(), types.SessionsSetAdmissionRequest{Identity: types.IdentityScope{Tenant: "foreign"}, ExpectedEpoch: 3, Epoch: 4})
	if err != nil || out.Epoch != 4 {
		t.Fatalf("response=%+v err=%v", out, err)
	}
	if got.Identity.Tenant != "tenant" || got.Identity.User != "user" || got.Identity.Session != "session" || got.ExpectedEpoch != 3 || got.Epoch != 4 {
		t.Fatalf("request identity or epochs changed: %+v", got)
	}
}
