package types_test

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestProtocolVersion_Pinned(t *testing.T) {
	// The version is pinned; a bump is an RFC change. This test is the
	// trip-wire — a casual edit to version.go fails CI and forces the
	// RFC conversation.
	if types.ProtocolVersion != "0.1.0" {
		t.Fatalf("ProtocolVersion = %q, want %q — bumping the Protocol version is an RFC change (RFC §5.3)", types.ProtocolVersion, "0.1.0")
	}
}

func TestStartRequest_JSONRoundTrip(t *testing.T) {
	in := types.StartRequest{
		Identity: types.IdentityScope{
			Tenant:  "tenant-a",
			User:    "user-1",
			Session: "session-x",
		},
		Query:          "summarise the Q3 report",
		Description:    "quarterly summary run",
		Priority:       5,
		IdempotencyKey: "idem-key-001",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.StartRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestStartResponse_JSONRoundTrip(t *testing.T) {
	in := types.StartResponse{
		TaskID:          "01HXXXXXXXXXXXXXXXXXXXXXXX",
		Reused:          true,
		ProtocolVersion: types.ProtocolVersion,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.StartResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestControlRequest_JSONRoundTrip(t *testing.T) {
	in := types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant:  "tenant-a",
			User:    "user-1",
			Session: "session-x",
			Run:     "run-42",
			Scope:   "owner_user",
		},
		Payload: map[string]any{
			"goal": "switch to the executive-summary template",
		},
		EventID: "01HYYYYYYYYYYYYYYYYYYYYYYY",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.ControlRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Identity != in.Identity {
		t.Fatalf("identity round-trip mismatch:\n got %+v\nwant %+v", out.Identity, in.Identity)
	}
	if out.EventID != in.EventID {
		t.Fatalf("event id round-trip mismatch: got %q want %q", out.EventID, in.EventID)
	}
	if got, ok := out.Payload["goal"].(string); !ok || got != "switch to the executive-summary template" {
		t.Fatalf("payload round-trip mismatch: got %v", out.Payload)
	}
}

func TestControlResponse_JSONRoundTrip(t *testing.T) {
	in := types.ControlResponse{
		Accepted:        true,
		Method:          string(methods.MethodPause),
		ProtocolVersion: types.ProtocolVersion,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.ControlResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestIdentityScope_OmitemptyOptionalFields(t *testing.T) {
	// A `start` request carries no Run / Scope — those fields are
	// omitempty so the wire form stays minimal.
	in := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := generic["run"]; present {
		t.Error("empty Run should be omitted from the wire form")
	}
	if _, present := generic["scope"]; present {
		t.Error("empty Scope should be omitted from the wire form")
	}
	for _, k := range []string{"tenant", "user", "session"} {
		if _, present := generic[k]; !present {
			t.Errorf("mandatory field %q missing from the wire form", k)
		}
	}
}
