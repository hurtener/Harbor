package types_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// TestStateHistoryRequest_JSONRoundTrip pins the wire shape: the request
// carries identity + session_id + the optional before/limit window knobs.
func TestStateHistoryRequest_JSONRoundTrip(t *testing.T) {
	req := types.StateHistoryRequest{
		Identity:  types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		SessionID: "s1",
		Before:    42,
		Limit:     25,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.StateHistoryRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != req {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, req)
	}
	// before/limit are omitempty — a zero window read omits them.
	zb, _ := json.Marshal(types.StateHistoryRequest{SessionID: "s1"})
	zs := string(zb)
	for _, omitted := range []string{`"before"`, `"limit"`} {
		if contains(zs, omitted) {
			t.Errorf("zero-window request JSON = %s, want %s omitted (omitempty)", zs, omitted)
		}
	}
}

// TestStateArtifactRef_Routable pins the routable-ref contract: the flat
// ref carries an ID and SHA256 (unlike the metadata-only ArtifactRefSummary).
func TestStateArtifactRef_Routable(t *testing.T) {
	ref := types.StateArtifactRef{
		ID:        "default_abc123def456",
		MimeType:  "application/json",
		SizeBytes: 9001,
		Filename:  "tool-result.json",
		SHA256:    "deadbeef",
	}
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["id"] != "default_abc123def456" {
		t.Errorf("StateArtifactRef.id = %v, want the content-addressed id (the routable key)", got["id"])
	}
	if got["sha256"] != "deadbeef" {
		t.Errorf("StateArtifactRef.sha256 = %v, want the digest (routable-ref contract)", got["sha256"])
	}
}

// TestStateEvent_JSONShape pins the flat wire-event projection (the same
// field set the SSE wireEvent carries, plus routable artifact refs).
func TestStateEvent_JSONShape(t *testing.T) {
	se := types.StateEvent{
		Type:       "llm.completion.chunk",
		Sequence:   7,
		OccurredAt: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC),
		Tenant:     "t1",
		User:       "u1",
		Session:    "s1",
		Run:        "r1",
		Payload:    map[string]any{"Delta": "hello"},
		Artifacts:  []types.StateArtifactRef{{ID: "default_xyz"}},
	}
	b, err := json.Marshal(se)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"type", "sequence", "occurred_at", "tenant", "user", "session", "run", "payload", "artifacts"} {
		if _, ok := got[k]; !ok {
			t.Errorf("StateEvent JSON missing key %q", k)
		}
	}
}

// TestStateHistoryResponse_JSONShape pins the page envelope.
func TestStateHistoryResponse_JSONShape(t *testing.T) {
	resp := types.StateHistoryResponse{
		Events:       []types.StateEvent{{Type: "task.completed", Sequence: 3}},
		HeadSequence: 1,
		TailSequence: 3,
		NextCursor:   3,
		HasMore:      true,
		Truncated:    false,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.StateHistoryResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.HeadSequence != 1 || got.TailSequence != 3 || got.NextCursor != 3 || !got.HasMore {
		t.Errorf("response round-trip mismatch: %+v", got)
	}
}

// TestStateHistoryLimitBounds pins the documented window bounds.
func TestStateHistoryLimitBounds(t *testing.T) {
	if types.DefaultStateHistoryLimit != 50 {
		t.Errorf("DefaultStateHistoryLimit = %d, want 50", types.DefaultStateHistoryLimit)
	}
	if types.MaxStateHistoryLimit != 200 {
		t.Errorf("MaxStateHistoryLimit = %d, want 200", types.MaxStateHistoryLimit)
	}
}
