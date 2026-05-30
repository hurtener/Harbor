package planner

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// TestResolveProvenance_Table covers the canonical else-chain (Phase
// 107f — D-176): canonical `source` key → tool name → flow name →
// producer → "unknown".
func TestResolveProvenance_Table(t *testing.T) {
	cases := []struct {
		name string
		src  map[string]any
		want string
	}{
		{"canonical_source_user_upload", map[string]any{"source": "user_upload"}, "user_upload"},
		{"canonical_source_tool", map[string]any{"source": "tool", "tool": "web_search"}, "tool"},
		{"tool_name_fallback", map[string]any{"tool": "web_search", "producer": "dev-tool-executor"}, "tool: web_search"},
		{"flow_name_fallback", map[string]any{"flow": "billing_flow"}, "flow: billing_flow"},
		{"producer_fallback", map[string]any{"producer": "flows.runs.describe"}, "flows.runs.describe"},
		{"empty_source_string_falls_through", map[string]any{"source": "", "tool": "calc"}, "tool: calc"},
		{"nil_map", nil, "unknown"},
		{"empty_map", map[string]any{}, "unknown"},
		{"unrelated_keys", map[string]any{"run_id": "r1"}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveProvenance(tc.src); got != tc.want {
				t.Errorf("ResolveProvenance(%v) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestBuildArtifactManifest_OrdersNewestFirst asserts the shared builder
// orders by created_at descending with a stable ID tiebreaker, and maps
// each ref's metadata + provenance onto an entry.
func TestBuildArtifactManifest_OrdersNewestFirst(t *testing.T) {
	now := time.Now().UTC()
	refs := []artifacts.ArtifactRef{
		{ID: "default_old", MimeType: "text/plain", SizeBytes: 1, Filename: "old.txt",
			Source: map[string]any{"source": "user_upload", "created_at": now.Add(-2 * time.Hour)}},
		{ID: "default_new", MimeType: "application/json", SizeBytes: 2, Filename: "new.json",
			Source: map[string]any{"tool": "web_search", "created_at": now}},
		{ID: "default_mid", MimeType: "text/csv", SizeBytes: 3, Filename: "mid.csv",
			Source: map[string]any{"flow": "billing", "created_at": now.Add(-1 * time.Hour)}},
	}
	got := BuildArtifactManifest(refs)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	wantOrder := []string{"default_new", "default_mid", "default_old"}
	for i, w := range wantOrder {
		if got[i].Ref != w {
			t.Errorf("entry[%d].Ref = %q, want %q", i, got[i].Ref, w)
		}
	}
	// Provenance resolution flows through.
	if got[0].Provenance != "tool: web_search" {
		t.Errorf("newest provenance = %q, want 'tool: web_search'", got[0].Provenance)
	}
	if got[1].Provenance != "flow: billing" {
		t.Errorf("mid provenance = %q, want 'flow: billing'", got[1].Provenance)
	}
	if got[2].Provenance != "user_upload" {
		t.Errorf("old provenance = %q, want 'user_upload'", got[2].Provenance)
	}
	// Metadata projection.
	if got[0].MIME != "application/json" || got[0].SizeBytes != 2 || got[0].Filename != "new.json" {
		t.Errorf("metadata projection wrong: %+v", got[0])
	}
}

// TestBuildArtifactManifest_StableTiebreak asserts that refs with no /
// equal created_at fall back to a deterministic ID-ascending order, so
// the map-iteration non-determinism of List never leaks into the prompt.
func TestBuildArtifactManifest_StableTiebreak(t *testing.T) {
	refs := []artifacts.ArtifactRef{
		{ID: "default_ccc"},
		{ID: "default_aaa"},
		{ID: "default_bbb"},
	}
	got := BuildArtifactManifest(refs)
	want := []string{"default_aaa", "default_bbb", "default_ccc"}
	for i, w := range want {
		if got[i].Ref != w {
			t.Errorf("entry[%d].Ref = %q, want %q", i, got[i].Ref, w)
		}
	}
}

// TestBuildArtifactManifest_Empty returns nil for nil / empty input so
// the planner omits the block entirely.
func TestBuildArtifactManifest_Empty(t *testing.T) {
	if got := BuildArtifactManifest(nil); got != nil {
		t.Errorf("BuildArtifactManifest(nil) = %v, want nil", got)
	}
	if got := BuildArtifactManifest([]artifacts.ArtifactRef{}); got != nil {
		t.Errorf("BuildArtifactManifest([]) = %v, want nil", got)
	}
}
