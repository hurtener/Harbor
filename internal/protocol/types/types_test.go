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
	// Phase 72b: when the impersonation triplet is absent, the three
	// fields MUST be omitted from the wire — `omitempty` on
	// *IdentityScope means a nil pointer disappears from the JSON.
	for _, k := range []string{"actor", "requester", "impersonating"} {
		if _, present := generic[k]; present {
			t.Errorf("phase 72b: empty impersonation field %q should be omitted from the wire form", k)
		}
	}
}

// TestIdentityScope_Impersonation_JSONRoundTrip — Phase 72b: an
// IdentityScope with the admin-impersonation triplet round-trips
// byte-identical through json.Marshal / json.Unmarshal so a third-party
// Console implementing `harbor console` from scratch sees the exact
// same wire shape Harbor's own code does.
func TestIdentityScope_Impersonation_JSONRoundTrip(t *testing.T) {
	in := types.IdentityScope{
		Tenant:  "tenant-acme",
		User:    "user-target",
		Session: "sess-target",
		Actor: &types.IdentityScope{
			Tenant:  "tenant-acme",
			User:    "admin-alice",
			Session: "sess-admin",
		},
		Requester: &types.IdentityScope{
			Tenant:  "tenant-acme",
			User:    "admin-alice",
			Session: "sess-admin",
		},
		Impersonating: &types.IdentityScope{
			Tenant:  "tenant-acme",
			User:    "user-target",
			Session: "sess-target",
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("Unmarshal generic: %v", err)
	}
	// The three impersonation keys MUST be present on the wire when
	// non-nil — the wire-shape is part of the Protocol contract per
	// Brief 11 §PG-5 (the verbatim triplet).
	for _, k := range []string{"actor", "requester", "impersonating"} {
		if _, present := generic[k]; !present {
			t.Errorf("phase 72b: impersonation field %q missing from the wire form", k)
		}
	}
	var out types.IdentityScope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal typed: %v", err)
	}
	if out.Actor == nil || out.Requester == nil || out.Impersonating == nil {
		t.Fatalf("phase 72b: one of the triplet pointers is nil after round-trip: actor=%v requester=%v impersonating=%v",
			out.Actor, out.Requester, out.Impersonating)
	}
	if *out.Actor != *in.Actor {
		t.Errorf("actor round-trip mismatch: got %+v, want %+v", *out.Actor, *in.Actor)
	}
	if *out.Requester != *in.Requester {
		t.Errorf("requester round-trip mismatch: got %+v, want %+v", *out.Requester, *in.Requester)
	}
	if *out.Impersonating != *in.Impersonating {
		t.Errorf("impersonating round-trip mismatch: got %+v, want %+v", *out.Impersonating, *in.Impersonating)
	}
	// IsImpersonating reports true exactly when Impersonating is
	// non-nil — the downstream gate keys off this predicate.
	if !out.IsImpersonating() {
		t.Error("phase 72b: IsImpersonating() returned false on a fully-populated impersonation triplet")
	}
}

// TestIdentityScope_NoImpersonation_IsImpersonatingFalse — Phase 72b:
// the predicate returns false exactly when Impersonating is nil; the
// downstream gate's "no impersonation = existing behaviour" branch
// keys off this.
func TestIdentityScope_NoImpersonation_IsImpersonatingFalse(t *testing.T) {
	s := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	if s.IsImpersonating() {
		t.Error("phase 72b: IsImpersonating() returned true on a non-impersonation scope")
	}
}

// TestStartRequest_Impersonation_JSONRoundTrip — Phase 72b: the
// extension flows through types.StartRequest, the wire request for
// the `start` Protocol method (Brief 11 §PG-5 lands the triplet on
// `start` first; `redirect` / `user_message` reuse the same shape via
// types.ControlRequest).
func TestStartRequest_Impersonation_JSONRoundTrip(t *testing.T) {
	in := types.StartRequest{
		Identity: types.IdentityScope{
			Tenant:  "tenant-acme",
			User:    "user-target",
			Session: "sess-target",
			Actor: &types.IdentityScope{
				Tenant: "tenant-acme", User: "admin-alice", Session: "sess-admin",
			},
			Requester: &types.IdentityScope{
				Tenant: "tenant-acme", User: "admin-alice", Session: "sess-admin",
			},
			Impersonating: &types.IdentityScope{
				Tenant: "tenant-acme", User: "user-target", Session: "sess-target",
			},
		},
		Query: "run-as-target",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.StartRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !out.Identity.IsImpersonating() {
		t.Fatal("phase 72b: round-trip dropped the impersonation triplet")
	}
	if out.Identity.Impersonating.User != "user-target" {
		t.Errorf("Impersonating.User round-trip: got %q want %q", out.Identity.Impersonating.User, "user-target")
	}
	if out.Identity.Actor.User != "admin-alice" {
		t.Errorf("Actor.User round-trip: got %q want %q", out.Identity.Actor.User, "admin-alice")
	}
	if out.Query != in.Query {
		t.Errorf("Query round-trip: got %q want %q", out.Query, in.Query)
	}
}
