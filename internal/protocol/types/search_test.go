package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestSearchIndex_IsValid(t *testing.T) {
	t.Parallel()
	want := map[types.SearchIndex]bool{
		types.SearchIndexSessions:  true,
		types.SearchIndexTasks:     true,
		types.SearchIndexEvents:    true,
		types.SearchIndexArtifacts: true,
		"":                         false,
		"tools":                    false,
		"agents":                   false,
		"flows":                    false,
		"sessions ":                false, //nolint:gocritic // trailing space is the deliberate test input — whitespace must not match a valid index.
		"SESSIONS":                 false,
	}
	for idx, expected := range want {
		got := types.IsValidSearchIndex(idx)
		if got != expected {
			t.Errorf("IsValidSearchIndex(%q) = %v, want %v", idx, got, expected)
		}
	}
}

func TestSearchRequest_PageBounds_Defaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       types.SearchRequest
		wantPage int
		wantSize int
	}{
		{"empty", types.SearchRequest{}, 1, types.DefaultSearchPageSize},
		{"explicit", types.SearchRequest{Page: 3, PageSize: 50}, 3, 50},
		{"negative-page", types.SearchRequest{Page: -1, PageSize: 50}, 1, 50},
		{"zero-size", types.SearchRequest{Page: 2, PageSize: 0}, 2, types.DefaultSearchPageSize},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, s := tc.in.PageBounds()
			if p != tc.wantPage || s != tc.wantSize {
				t.Fatalf("PageBounds(%+v) = (%d, %d), want (%d, %d)", tc.in, p, s, tc.wantPage, tc.wantSize)
			}
		})
	}
}

func TestSearchRequest_JSON_RoundTrip(t *testing.T) {
	t.Parallel()
	src := types.SearchRequest{
		Query: "hello",
		Indexes: []types.SearchIndex{
			types.SearchIndexSessions, types.SearchIndexTasks,
		},
		Filter: types.SearchFilter{
			TenantIDs: []string{"t1", "t2"},
			Since:     time.Unix(1700000000, 0).UTC(),
		},
		Facets: []types.SearchFacet{
			{Key: "tasks.status", Value: "running"},
		},
		Page:     2,
		PageSize: 50,
	}
	body, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.SearchRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Query != src.Query {
		t.Errorf("Query: got %q, want %q", got.Query, src.Query)
	}
	if len(got.Indexes) != 2 || got.Indexes[0] != types.SearchIndexSessions {
		t.Errorf("Indexes round-trip mismatch: got %v", got.Indexes)
	}
	if len(got.Filter.TenantIDs) != 2 {
		t.Errorf("Filter.TenantIDs: got %v", got.Filter.TenantIDs)
	}
	if got.Page != src.Page || got.PageSize != src.PageSize {
		t.Errorf("pagination round-trip: got (%d,%d), want (%d,%d)", got.Page, got.PageSize, src.Page, src.PageSize)
	}
	if len(got.Facets) != 1 || got.Facets[0].Key != "tasks.status" {
		t.Errorf("Facets round-trip mismatch: got %v", got.Facets)
	}
}

func TestSearchResponse_JSON_RoundTrip(t *testing.T) {
	t.Parallel()
	src := types.SearchResponse{
		Rows: []types.SearchResultRow{
			{
				Index:      types.SearchIndexEvents,
				ID:         "sess-1:42",
				TenantID:   "t1",
				UserID:     "u1",
				SessionID:  "sess-1",
				RunID:      "run-7",
				OccurredAt: time.Unix(1700000000, 0).UTC(),
				Preview:    "tool.failed: deploy",
				Facets:     map[string]string{"events.type": "tool.failed"},
			},
			{
				Index:     types.SearchIndexArtifacts,
				ID:        "default_abc123def456",
				TenantID:  "t1",
				UserID:    "u1",
				SessionID: "sess-1",
				Ref: &types.SearchArtifactRef{
					ID:        "default_abc123def456",
					MimeType:  "application/pdf",
					SizeBytes: 1024 * 1024,
					Filename:  "report.pdf",
					SHA256:    "abc123def4560000",
				},
			},
		},
		Page:            2,
		PageSize:        50,
		PageCount:       3,
		TotalCount:      120,
		HasMore:         true,
		ProtocolVersion: "0.1.0",
	}
	body, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.SearchResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("Rows length: got %d, want 2", len(got.Rows))
	}
	if got.Rows[0].Index != types.SearchIndexEvents {
		t.Errorf("Rows[0].Index: got %q", got.Rows[0].Index)
	}
	if got.Rows[1].Ref == nil || got.Rows[1].Ref.ID == "" {
		t.Errorf("Rows[1].Ref must be populated, got %+v", got.Rows[1].Ref)
	}
	if !got.HasMore {
		t.Error("HasMore round-trip failed")
	}
	if got.TotalCount != 120 {
		t.Errorf("TotalCount: got %d, want 120", got.TotalCount)
	}
	if got.ProtocolVersion != "0.1.0" {
		t.Errorf("ProtocolVersion: got %q", got.ProtocolVersion)
	}
}

func TestSearchResultRow_HeavyPayload_PreviewOrRef(t *testing.T) {
	t.Parallel()
	// The contract: when Ref is populated, Preview is empty; the wire
	// shape must not carry both for the same row (D-026 enforcement
	// runs at the searcher layer; this test pins the row shape only).
	row := types.SearchResultRow{
		Index:     types.SearchIndexArtifacts,
		ID:        "default_xyz",
		TenantID:  "t1",
		UserID:    "u1",
		SessionID: "sess-1",
		Ref:       &types.SearchArtifactRef{ID: "default_xyz", SizeBytes: 100000},
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.SearchResultRow
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Preview != "" {
		t.Errorf("Preview should be empty when Ref is set, got %q", got.Preview)
	}
	if got.Ref == nil || got.Ref.SizeBytes != 100000 {
		t.Errorf("Ref round-trip failed, got %+v", got.Ref)
	}
}
