package types_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestSessionStatus_IsValid(t *testing.T) {
	t.Parallel()
	want := map[types.SessionStatus]bool{
		types.SessionStatusRunning:   true,
		types.SessionStatusPaused:    true,
		types.SessionStatusCompleted: true,
		types.SessionStatusFailed:    true,
		"":                           false,
		"RUNNING":                    false,
		"closed":                     false,
		"running ":                   false, //nolint:gocritic // trailing space is the deliberate test input — whitespace must not match a valid status.
	}
	for s, expected := range want {
		if got := types.IsValidSessionStatus(s); got != expected {
			t.Errorf("IsValidSessionStatus(%q) = %v, want %v", s, got, expected)
		}
	}
}

func TestSessionSort_IsValid(t *testing.T) {
	t.Parallel()
	want := map[types.SessionSort]bool{
		types.SessionSortStartedDesc:      true,
		types.SessionSortStartedAsc:       true,
		types.SessionSortLastActivityDesc: true,
		types.SessionSortCostDesc:         true,
		"":                                false,
		"started":                         false,
		"cost":                            false,
	}
	for s, expected := range want {
		if got := types.IsValidSessionSort(s); got != expected {
			t.Errorf("IsValidSessionSort(%q) = %v, want %v", s, got, expected)
		}
	}
}

func TestSessionProjection_IsValid(t *testing.T) {
	t.Parallel()
	want := map[types.SessionProjection]bool{
		types.SessionProjectionFull:      true,
		types.SessionProjectionLifecycle: true,
		"":                               false,
		"FULL":                           false,
		"complete":                       false,
		"lifecycle ":                     false, //nolint:gocritic // trailing space is the deliberate test input — whitespace must not match a valid projection.
	}
	for p, expected := range want {
		if got := types.IsValidSessionProjection(p); got != expected {
			t.Errorf("IsValidSessionProjection(%q) = %v, want %v", p, got, expected)
		}
	}
}

func TestCounterStatus_ConstantsAndValidity(t *testing.T) {
	t.Parallel()
	// The four canonical states are distinct and valid.
	if !types.IsValidCounterStatus(types.CounterStatusCurrent) ||
		!types.IsValidCounterStatus(types.CounterStatusPartial) ||
		!types.IsValidCounterStatus(types.CounterStatusNotRequested) ||
		!types.IsValidCounterStatus(types.CounterStatusUnavailable) {
		t.Fatal("the four canonical counter-status values must all be valid")
	}
	// The zero value and junk are not valid — a row without a marker is
	// never mistaken for a named availability state.
	for s, expected := range map[types.CounterStatus]bool{
		"":         false,
		"zero":     false,
		"CURRENT":  false,
		"not_yet":  false,
		"current ": false, //nolint:gocritic // trailing space is the deliberate test input — whitespace must not match a valid status.
	} {
		if got := types.IsValidCounterStatus(s); got != expected {
			t.Errorf("IsValidCounterStatus(%q) = %v, want %v", s, got, expected)
		}
	}
}

func TestSessionsListRequest_MarshalRoundTrip(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	hasIntervention := true
	costAbove := int64(500)
	in := types.SessionsListRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		Filter: types.SessionFilter{
			Statuses:        []types.SessionStatus{types.SessionStatusFailed},
			AgentIDs:        []string{"agent-a"},
			UserIDs:         []string{"u1"},
			TenantIDs:       []string{"t1"},
			StartedWindow:   types.Window{From: &from, To: &to},
			HasIntervention: &hasIntervention,
			CostAboveCents:  &costAbove,
			Query:           "web_search",
		},
		Sort:   types.SessionSortCostDesc,
		Cursor: "v1:abc",
		Limit:  25,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsListRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Identity != in.Identity {
		t.Errorf("Identity round-trip: got %+v, want %+v", out.Identity, in.Identity)
	}
	if out.Sort != in.Sort || out.Cursor != in.Cursor || out.Limit != in.Limit {
		t.Errorf("scalar round-trip mismatch: got %+v", out)
	}
	if out.Filter.HasIntervention == nil || *out.Filter.HasIntervention != true {
		t.Errorf("HasIntervention round-trip lost the pointer value")
	}
	if out.Filter.CostAboveCents == nil || *out.Filter.CostAboveCents != 500 {
		t.Errorf("CostAboveCents round-trip lost the pointer value")
	}
	if out.Filter.StartedWindow.From == nil || !out.Filter.StartedWindow.From.Equal(from) {
		t.Errorf("StartedWindow.From round-trip mismatch")
	}
	if out.Filter.Query != "web_search" {
		t.Errorf("Query round-trip: got %q", out.Filter.Query)
	}
}

func TestSessionsListResponse_TruncatedRoundTrip(t *testing.T) {
	t.Parallel()
	in := types.SessionsListResponse{
		Rows: []types.SessionRow{{
			SessionID:      "s1",
			Status:         types.SessionStatusRunning,
			UserID:         "u1",
			TenantID:       "t1",
			StartedAt:      time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC),
			LastActivityAt: time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC),
			Duration:       30 * time.Minute,
			TasksCount:     3,
			EventsCount:    42,
			TotalCostCents: 120,
			TotalTokens:    9000,
			Identity:       types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		}},
		NextCursor: "v1:def",
		Truncated:  true,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !out.Truncated {
		t.Error("Truncated round-trip lost — D-026 fail-loudly signal must survive the wire")
	}
	if out.NextCursor != "v1:def" {
		t.Errorf("NextCursor round-trip: got %q", out.NextCursor)
	}
	if len(out.Rows) != 1 || out.Rows[0].Duration != 30*time.Minute {
		t.Errorf("SessionRow round-trip mismatch: %+v", out.Rows)
	}
	// D-065: no Priority — the row carries no priority surface. The
	// struct simply has no such field; this assertion documents the
	// carve-out for the reader.
	if out.Rows[0].SessionID != "s1" {
		t.Errorf("SessionID round-trip: got %q", out.Rows[0].SessionID)
	}
}

// TestSessionRow_AgentAbsence_Representable pins the D-309/D-311
// representable-absence contract for the two agent fields: a nil AgentID /
// AgentName is OMITTED on the wire (distinguishable from a present
// empty-string agent id), and a non-nil binding round-trips as a value.
func TestSessionRow_AgentAbsence_Representable(t *testing.T) {
	t.Parallel()
	// Nil agent binding — the V1 default. The keys must be ABSENT, not "".
	row := types.SessionRow{SessionID: "s1", Status: types.SessionStatusRunning}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if s := string(raw); strings.Contains(s, "agent_id") || strings.Contains(s, "agent_name") {
		t.Fatalf("nil agent binding must be OMITTED on the wire (representable absence, D-311); got %s", s)
	}
	if strings.Contains(string(raw), "counters_partial") {
		t.Fatalf("counters_partial=false must be omitted on the wire; got %s", raw)
	}
	// A non-nil binding round-trips as a value.
	agent := "agent-a"
	name := "Agent A"
	row2 := types.SessionRow{SessionID: "s2", AgentID: &agent, AgentName: &name, CountersPartial: true}
	raw2, err := json.Marshal(row2)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionRow
	if err := json.Unmarshal(raw2, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.AgentID == nil || *out.AgentID != "agent-a" {
		t.Errorf("AgentID round-trip: got %v, want agent-a", out.AgentID)
	}
	if out.AgentName == nil || *out.AgentName != "Agent A" {
		t.Errorf("AgentName round-trip: got %v, want Agent A", out.AgentName)
	}
	if !out.CountersPartial {
		t.Error("CountersPartial round-trip lost — the honest lower-bound marker must survive the wire (D-309 WARN-1)")
	}
}

// TestSessionsListRequest_Projection_OmittedStaysOffWireAndRoundTrips pins
// the default-compatible contract of the additive projection selector: a
// client that never sends the field must be byte-identical to the original
// wire shape (no `projection` key), and an explicit lifecycle projection
// must round-trip.
func TestSessionsListRequest_Projection_OmittedStaysOffWireAndRoundTrips(t *testing.T) {
	t.Parallel()
	req := types.SessionsListRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "projection") {
		t.Fatalf("an omitted projection must stay OFF the wire (default-compatible); got %s", raw)
	}
	req.Projection = types.SessionProjectionLifecycle
	raw2, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsListRequest
	if err := json.Unmarshal(raw2, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Projection != types.SessionProjectionLifecycle {
		t.Errorf("Projection round-trip: got %q, want %q", out.Projection, types.SessionProjectionLifecycle)
	}
}

// TestSessionsInspectRequest_ProjectionRoundTrips pins the same
// default-compatible selector on `sessions.inspect`.
func TestSessionsInspectRequest_ProjectionRoundTrips(t *testing.T) {
	t.Parallel()
	req := types.SessionsInspectRequest{
		Identity:  types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		SessionID: "s1",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "projection") {
		t.Fatalf("an omitted projection must stay OFF the wire; got %s", raw)
	}
	req.Projection = types.SessionProjectionFull
	raw2, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsInspectRequest
	if err := json.Unmarshal(raw2, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Projection != types.SessionProjectionFull {
		t.Errorf("Projection round-trip: got %q, want %q", out.Projection, types.SessionProjectionFull)
	}
}

// TestSessionRow_CounterStatus_RepresentableAndRoundTrips pins the explicit
// counter-availability marker: an empty marker is OMITTED on the wire (a
// row constructed without one must not fabricate an availability state),
// and every canonical marker round-trips as a value.
func TestSessionRow_CounterStatus_RepresentableAndRoundTrips(t *testing.T) {
	t.Parallel()
	row := types.SessionRow{SessionID: "s1", Status: types.SessionStatusRunning}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "counter_status") {
		t.Fatalf("an empty counter_status must be omitted on the wire; got %s", raw)
	}
	for _, cs := range []types.CounterStatus{
		types.CounterStatusCurrent,
		types.CounterStatusPartial,
		types.CounterStatusNotRequested,
		types.CounterStatusUnavailable,
	} {
		row2 := types.SessionRow{SessionID: "s1", CounterStatus: cs}
		raw2, err := json.Marshal(row2)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", cs, err)
		}
		var out types.SessionRow
		if err := json.Unmarshal(raw2, &out); err != nil {
			t.Fatalf("Unmarshal(%q): %v", cs, err)
		}
		if out.CounterStatus != cs {
			t.Errorf("CounterStatus round-trip: got %q, want %q", out.CounterStatus, cs)
		}
	}
}

func TestSessionsInspectResponse_AdditiveFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	in := types.SessionsInspectResponse{
		Row: types.SessionRow{SessionID: "s1", Status: types.SessionStatusCompleted},
		RecentInterventions: []types.InterventionSummary{{
			Type:       "hitl_pause",
			Reason:     "operator intervention",
			Outcome:    "resolved",
			OccurredAt: time.Date(2026, 5, 19, 9, 15, 0, 0, time.UTC),
		}},
		RecentArtifacts: []types.ArtifactRefSummary{{
			Filename:  "report.md",
			MIME:      "text/markdown",
			SizeBytes: 2048,
			CreatedAt: time.Date(2026, 5, 19, 9, 20, 0, 0, time.UTC),
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsInspectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out.RecentInterventions) != 1 || out.RecentInterventions[0].Outcome != "resolved" {
		t.Errorf("RecentInterventions round-trip mismatch: %+v", out.RecentInterventions)
	}
	if len(out.RecentArtifacts) != 1 || out.RecentArtifacts[0].MIME != "text/markdown" {
		t.Errorf("RecentArtifacts round-trip mismatch: %+v", out.RecentArtifacts)
	}
}

func TestSessionsListRequest_ZeroValueIdentityIsIncomplete(t *testing.T) {
	t.Parallel()
	// A zero-value request carries an empty IdentityScope — identity is
	// mandatory; the protocol service rejects it. This test pins the
	// wire-decode observability of the incomplete triple.
	var req types.SessionsListRequest
	if req.Identity.Tenant != "" || req.Identity.User != "" || req.Identity.Session != "" {
		t.Fatal("zero-value SessionsListRequest must carry an empty identity triple")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.SessionsListRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Identity.Tenant != "" {
		t.Error("incomplete identity must survive the round-trip as incomplete (no silent default)")
	}
}

func TestSessionsDeleteRequestResponse_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	req := types.SessionsDeleteRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal request: %v", err)
	}
	var gotReq types.SessionsDeleteRequest
	if err := json.Unmarshal(raw, &gotReq); err != nil {
		t.Fatalf("Unmarshal request: %v", err)
	}
	if gotReq.Identity.Session != "s1" || gotReq.Identity.Tenant != "t1" || gotReq.Identity.User != "u1" {
		t.Errorf("request round-trip mismatch: %+v", gotReq)
	}

	resp := types.SessionsDeleteResponse{
		SessionID:           "s1",
		Deleted:             true,
		StateRecordsDeleted: 4,
		ArtifactsDeleted:    2,
		MemoryPurged:        true,
	}
	rawResp, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal response: %v", err)
	}
	// Confirm the snake_case wire keys are stable (a third-party client
	// branches on them).
	for _, want := range []string{
		`"session_id"`, `"deleted"`, `"state_records_deleted"`,
		`"artifacts_deleted"`, `"memory_purged"`,
	} {
		if !contains(string(rawResp), want) {
			t.Errorf("response JSON missing key %s: %s", want, rawResp)
		}
	}
	var gotResp types.SessionsDeleteResponse
	if err := json.Unmarshal(rawResp, &gotResp); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if gotResp != resp {
		t.Errorf("response round-trip mismatch: got %+v want %+v", gotResp, resp)
	}
}
