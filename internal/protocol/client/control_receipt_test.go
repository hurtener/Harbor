package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestClient_ControlReceipt_BindsConnectionIdentityAndExactEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/control.receipt" {
			t.Errorf("path=%q", r.URL.Path)
		}
		var request types.ControlReceiptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Identity.Tenant != "tenant" || request.Identity.User != "user" || request.Identity.Session != "session" || request.Identity.Run != "exact-task" || request.EventID != "exact-event" {
			t.Errorf("request=%+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ControlReceiptResponse{Receipt: types.ControlReceipt{EventID: request.EventID, TaskID: request.Identity.Run, InputRevision: 7, Status: "applied"}, ProtocolVersion: types.ProtocolVersion})
	}))
	defer server.Close()
	client := testClient(t, server)
	r, err := client.ControlReceipt(t.Context(), types.ControlReceiptRequest{Identity: types.IdentityScope{Tenant: "untrusted", Run: "exact-task"}, EventID: "exact-event"})
	if err != nil || r.Receipt.InputRevision != 7 || r.Receipt.Status != "applied" {
		t.Fatalf("receipt=%+v err=%v", r, err)
	}
}

func TestClient_ControlReceipt_ExpectedRevisionRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req types.ControlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.ExpectedInputRevision == nil || *req.ExpectedInputRevision != 7 || req.EventID != "exact-event" || req.Identity.Run != "exact-task" {
			t.Errorf("request=%+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ControlResponse{Accepted: true, Method: string(methods.MethodUserMessage), ProtocolVersion: types.ProtocolVersion, Receipt: &types.ControlReceipt{EventID: req.EventID, TaskID: req.Identity.Run, InputRevision: 8, Status: "accepted"}})
	}))
	defer server.Close()
	client := testClient(t, server)
	expected := uint64(7)
	resp, err := client.Control(t.Context(), methods.MethodUserMessage, types.ControlRequest{Identity: types.IdentityScope{Run: "exact-task"}, EventID: "exact-event", ExpectedInputRevision: &expected, Payload: map[string]any{"message": "revision-bound correction"}})
	if err != nil || resp.Receipt == nil || resp.Receipt.InputRevision != 8 {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
}
