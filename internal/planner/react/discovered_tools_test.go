package react

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// findReservedDecl returns the reserved-control declaration with the
// given name, failing the test if absent.
func findReservedDecl(t *testing.T, name string) string {
	t.Helper()
	for _, d := range reservedPlannerControlDeclarations() {
		if d.Name == name {
			return d.Description
		}
	}
	t.Fatalf("reserved declaration %q not found", name)
	return ""
}

// TestReservedDecl_SpawnTask_TeachesBatchingContract — the `_spawn_task`
// description teaches the AC-21′ batching contract (it may co-occur with
// catalog tools and other spawns in one response) and no longer implies
// the pre-fix standalone framing that invited the parallelism the old
// guard rejected. This closes the prompt-vs-validator disagreement.
func TestReservedDecl_SpawnTask_TeachesBatchingContract(t *testing.T) {
	t.Parallel()
	desc := findReservedDecl(t, SpawnTaskToolName)

	// Regression guard: the pre-fix phrase that framed spawn as running
	// alone must not reappear.
	if strings.Contains(desc, "Use to launch parallel work") {
		t.Errorf("_spawn_task description still carries the pre-fix 'Use to launch parallel work' phrasing:\n%s", desc)
	}

	// It must teach co-occurrence with other calls.
	lower := strings.ToLower(desc)
	mentionsCoOccur := strings.Contains(lower, "alongside") ||
		strings.Contains(lower, "co-occur") ||
		strings.Contains(lower, "same response")
	if !mentionsCoOccur {
		t.Errorf("_spawn_task description does not teach that it may accompany other calls:\n%s", desc)
	}
}

// TestReservedDecl_AwaitTask_StaysStandalone — the `_await_task`
// description still instructs the model to send it alone (never batched),
// matching the AC-21′ standalone guard the projector keeps for it.
func TestReservedDecl_AwaitTask_StaysStandalone(t *testing.T) {
	t.Parallel()
	desc := findReservedDecl(t, AwaitTaskToolName)
	lower := strings.ToLower(desc)
	if !strings.Contains(lower, "alone") {
		t.Errorf("_await_task description does not instruct it be sent alone:\n%s", desc)
	}
	if !strings.Contains(lower, "task_id") {
		t.Errorf("_await_task description no longer mentions passing a task_id:\n%s", desc)
	}
}

// TestExtractDiscoveredNames_ObservationShapes covers the observation
// shapes a tool_search result can arrive in: typed map, JSON bytes, JSON
// RawMessage, string, a struct (best-effort re-encode), nil, and
// unknown. This lifts the previously-uncovered extraction helpers the
// discovered-tools projection depends on.
func TestExtractDiscoveredNames_ObservationShapes(t *testing.T) {
	t.Parallel()
	want := []string{"youtube_download", "pdf_export"}
	rawJSON := `{"tools":[{"name":"youtube_download"},{"name":"pdf_export"}]}`

	mapShape := map[string]any{"tools": []any{
		map[string]any{"name": "youtube_download"},
		map[string]any{"name": "pdf_export"},
	}}
	mapSliceShape := map[string]any{"tools": []map[string]any{
		{"name": "youtube_download"},
		{"name": "pdf_export"},
	}}
	type toolEntry struct {
		Name string `json:"name"`
	}
	structShape := struct {
		Tools []toolEntry `json:"tools"`
	}{Tools: []toolEntry{{Name: "youtube_download"}, {Name: "pdf_export"}}}

	cases := []struct {
		name string
		obs  any
		want []string
	}{
		{name: "map_any", obs: mapShape, want: want},
		{name: "map_slice", obs: mapSliceShape, want: want},
		{name: "raw_message", obs: json.RawMessage(rawJSON), want: want},
		{name: "bytes", obs: []byte(rawJSON), want: want},
		{name: "string", obs: rawJSON, want: want},
		{name: "struct_reencode", obs: structShape, want: want},
		{name: "nil", obs: nil, want: nil},
		{name: "unknown_int", obs: 42, want: nil},
		{name: "empty_bytes", obs: []byte(""), want: nil},
		{name: "malformed_json", obs: json.RawMessage(`{`), want: nil},
		{name: "no_tools_key", obs: map[string]any{"other": 1}, want: nil},
		{name: "tools_wrong_type", obs: map[string]any{"tools": "nope"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractDiscoveredNames(tc.obs)
			if !equalStrings(got, tc.want) {
				t.Errorf("extractDiscoveredNames(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDeriveDiscoveredFromTrajectory_DedupesAcrossSteps — names are
// pulled only from tool_search steps and deduplicated across the
// trajectory; a nil/empty trajectory yields nil.
func TestDeriveDiscoveredFromTrajectory_DedupesAcrossSteps(t *testing.T) {
	t.Parallel()
	if got := deriveDiscoveredFromTrajectory(nil); got != nil {
		t.Errorf("nil trajectory = %v, want nil", got)
	}
	tr := &planner.Trajectory{Steps: []planner.Step{
		{
			Action:         planner.CallTool{Tool: toolSearchToolName},
			LLMObservation: map[string]any{"tools": []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}},
		},
		{
			// Non-tool_search step is ignored.
			Action:         planner.CallTool{Tool: "search"},
			LLMObservation: map[string]any{"tools": []any{map[string]any{"name": "c"}}},
		},
		{
			// Duplicate "a" is deduped; falls back to Observation when
			// LLMObservation is nil.
			Action:      planner.CallTool{Tool: toolSearchToolName},
			Observation: map[string]any{"tools": []any{map[string]any{"name": "a"}, map[string]any{"name": "d"}}},
		},
	}}
	got := deriveDiscoveredFromTrajectory(tr)
	if !equalStrings(got, []string{"a", "b", "d"}) {
		t.Errorf("deriveDiscoveredFromTrajectory = %v, want [a b d]", got)
	}
}

// TestMergeDiscovered_UnionPreservesOrder — the deduplicated union keeps
// existing entries in place, then appends new derived entries; empty
// strings are dropped; both-nil yields nil.
func TestMergeDiscovered_UnionPreservesOrder(t *testing.T) {
	t.Parallel()
	if got := mergeDiscovered(nil, nil); got != nil {
		t.Errorf("both-nil = %v, want nil", got)
	}
	got := mergeDiscovered([]string{"a", "", "b"}, []string{"b", "c", ""})
	if !equalStrings(got, []string{"a", "b", "c"}) {
		t.Errorf("mergeDiscovered = %v, want [a b c]", got)
	}
}

// equalStrings compares two string slices for exact equality.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
