package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestIsValidPauseSnapshotState(t *testing.T) {
	cases := []struct {
		state types.PauseSnapshotState
		want  bool
	}{
		{types.PauseStatePaused, true},
		{types.PauseStateResumed, true},
		{types.PauseSnapshotState("bogus"), false},
		{types.PauseSnapshotState(""), false},
		{types.PauseSnapshotState("PAUSED"), false}, // case-sensitive
	}
	for _, tc := range cases {
		if got := types.IsValidPauseSnapshotState(tc.state); got != tc.want {
			t.Errorf("IsValidPauseSnapshotState(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestPauseSnapshot_JSONRoundTrip_InlinePayload(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	in := types.PauseSnapshot{
		Token:    "tok-1",
		Reason:   "approval_required",
		State:    types.PauseStatePaused,
		Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		PausedAt: now,
		Payload:  map[string]any{"k": "v"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.PauseSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Token != in.Token || out.State != in.State {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
	if out.Payload["k"] != "v" {
		t.Errorf("Payload lost in round-trip: %+v", out.Payload)
	}
	if out.PayloadRef != nil {
		t.Errorf("PayloadRef = %+v, want nil", out.PayloadRef)
	}
}

func TestPauseSnapshot_JSONRoundTrip_PayloadRef(t *testing.T) {
	in := types.PauseSnapshot{
		Token: "tok-heavy",
		State: types.PauseStatePaused,
		PayloadRef: &types.PauseArtifactRef{
			ID:        "pause_payload_abc123",
			MimeType:  "application/json",
			SizeBytes: 4096,
			SHA256:    "deadbeef",
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.PauseSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.PayloadRef == nil {
		t.Fatal("PayloadRef lost in round-trip")
	}
	if out.PayloadRef.ID != "pause_payload_abc123" || out.PayloadRef.SizeBytes != 4096 {
		t.Errorf("PayloadRef mismatch: %+v", out.PayloadRef)
	}
	if out.Payload != nil {
		t.Errorf("Payload = %+v, want nil when PayloadRef set", out.Payload)
	}
}

func TestPauseListRequest_JSONRoundTrip(t *testing.T) {
	in := types.PauseListRequest{
		Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		Filter: types.PauseFilter{
			Status:     []string{"paused"},
			TenantIDs:  []string{"t", "t2"},
			Reasons:    []string{"approval_required"},
			SessionIDs: []string{"s"},
		},
		Page:     2,
		PageSize: 25,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.PauseListRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Page != 2 || out.PageSize != 25 {
		t.Fatalf("pagination lost: page=%d size=%d", out.Page, out.PageSize)
	}
	if len(out.Filter.TenantIDs) != 2 || out.Filter.TenantIDs[1] != "t2" {
		t.Errorf("filter TenantIDs lost: %+v", out.Filter.TenantIDs)
	}
}

func TestPauseListResponse_JSONRoundTrip(t *testing.T) {
	in := types.PauseListResponse{
		Snapshots: []types.PauseSnapshot{{Token: "tok-1", State: types.PauseStateResumed}},
		Page:      1,
		PageSize:  50,
		PageCount: 3,
		TotalRows: 120,
		Truncated: true,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.PauseListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.PageCount != 3 || out.TotalRows != 120 || !out.Truncated {
		t.Fatalf("response fields lost: %+v", out)
	}
	if len(out.Snapshots) != 1 || out.Snapshots[0].Token != "tok-1" {
		t.Errorf("snapshots lost: %+v", out.Snapshots)
	}
}

func TestPauseListPaginationBounds(t *testing.T) {
	if types.DefaultPauseListPageSize != 50 {
		t.Errorf("DefaultPauseListPageSize = %d, want 50", types.DefaultPauseListPageSize)
	}
	if types.MaxPauseListPageSize != 200 {
		t.Errorf("MaxPauseListPageSize = %d, want 200", types.MaxPauseListPageSize)
	}
}
