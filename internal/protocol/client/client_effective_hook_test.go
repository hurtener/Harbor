package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestRuntimeClient_EffectiveHookPosture_UsesCanonicalIdentityAndAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/runtime.info" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var req types.RuntimeInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode runtime.info: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Identity != (types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}) || req.EffectiveAgentID != "agent-a" {
			t.Errorf("runtime.info request = %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol_version":"0.1.0","effective_run_completion":{"agent_id":"agent-a","state":"off"}}`))
	}))
	defer server.Close()

	c, ok := testClient(t, server).(RuntimeClient)
	if !ok {
		t.Fatal("client lacks runtime inspection surface")
	}
	if _, err := c.RuntimeInfoForAgent(context.Background(), ""); err == nil {
		t.Fatal("empty agent selector accepted")
	}
	info, err := c.RuntimeInfoForAgent(context.Background(), "agent-a")
	if err != nil || info.EffectiveRunCompletion == nil || info.EffectiveRunCompletion.State != "off" {
		t.Fatalf("effective hook posture = %+v, %v", info.EffectiveRunCompletion, err)
	}
}
