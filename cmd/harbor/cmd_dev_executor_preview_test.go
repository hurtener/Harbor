// cmd_dev_executor_preview_test.go — pins the field-aware preview
// behaviour for heavy tool results: small top-level scalars / arrays
// are preserved verbatim and oversized nested values are replaced
// with a `[omitted: N bytes]` sentinel so the LLM sees both what's
// available and what was pruned.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFieldAwarePreview_PreservesScalarsAndPrunesNested builds a
// synthetic shape mirroring real-world metadata pathology — one huge
// nested field early in alphabetical order plus a scalar we care
// about elsewhere — and asserts the scalar is preserved verbatim
// while the big nested blob renders as a sentinel.
func TestFieldAwarePreview_PreservesScalarsAndPrunesNested(t *testing.T) {
	t.Parallel()

	bigNested := make(map[string]any, 200)
	for i := range 200 {
		bigNested[string(rune('a'+i%26))+string(rune('a'+(i/26)%26))] = strings.Repeat("https://www.youtube.com/api/timedtext?long=url&with=many&query=params ", 5)
	}
	m := map[string]any{
		"automatic_captions": bigNested, // ~70 KB
		"abr":                134.009,
		"acodec":             "opus",
		"age_limit":          0,
		"duration":           6821,
		"title":              "World Cup 2026 Funky House Music Mix",
		"view_count":         276253,
	}

	preview, ok := fieldAwarePreview(m)
	if !ok {
		t.Fatal("fieldAwarePreview returned !ok for a normal map")
	}

	// duration MUST appear verbatim as a number, not as a sentinel.
	if !strings.Contains(preview, `"duration":6821`) {
		t.Errorf("preview missing duration:6821. Preview:\n%s", preview)
	}
	// Scalar string field also preserved.
	if !strings.Contains(preview, `"title":"World Cup 2026 Funky House Music Mix"`) {
		t.Errorf("preview missing title verbatim. Preview:\n%s", preview)
	}
	// The heavy nested field is replaced with a sentinel.
	if !strings.Contains(preview, `"automatic_captions":"[omitted:`) {
		t.Errorf("automatic_captions should be pruned to a sentinel. Preview:\n%s", preview)
	}
	// Preview is well-formed JSON we can re-parse.
	var probe map[string]any
	if err := json.Unmarshal([]byte(preview), &probe); err != nil {
		t.Errorf("preview is not valid JSON: %v\n%s", err, preview)
	}
	// And the LLM-relevant fields survived the round-trip.
	if probe["duration"].(float64) != 6821 {
		t.Errorf("duration lost on round-trip: %v", probe["duration"])
	}
}

// TestFieldAwarePreview_RespectsTotalBudget asserts that even when
// every individual field passes the per-field budget, the assembled
// preview is capped at previewTotalMaxBytes. Builds 500 short scalar
// fields — each well under the per-field cap — and confirms the
// final output stays within budget.
func TestFieldAwarePreview_RespectsTotalBudget(t *testing.T) {
	t.Parallel()

	m := make(map[string]any, 500)
	for i := range 500 {
		key := "field_" + strings.Repeat("x", i%20)
		m[key] = strings.Repeat("v", 50) // each field ~70 bytes serialised
	}
	preview, ok := fieldAwarePreview(m)
	if !ok {
		t.Fatal("fieldAwarePreview returned !ok")
	}
	if len(preview) > previewTotalMaxBytes+len("...(truncated)") {
		t.Errorf("preview exceeded total budget: %d bytes (cap %d)", len(preview), previewTotalMaxBytes)
	}
	// When budget was hit, the truncation marker is present.
	if len(preview) > previewTotalMaxBytes && !strings.HasSuffix(preview, "...(truncated)") {
		t.Errorf("preview hit budget but lacks ...(truncated) marker")
	}
}

// TestBuildPreview_UnwrapsSingleKeyResultWrapper asserts that the
// `{"result": {<actual metadata>}}` shape MCP tools (and many Go
// structs) emit is unwrapped one level before the field-aware
// preview runs. Without this unwrap the outer single-key wrapper
// would prune `result` itself as oversized and the model would see
// `{"result":"[omitted: N bytes]"}` — useless. The pinned input
// shape here mirrors what `youtube_get_metadata` actually returns
// through the MCP driver (the live YouTube test surface).
func TestBuildPreview_UnwrapsSingleKeyResultWrapper(t *testing.T) {
	t.Parallel()

	inner := map[string]any{
		"duration":           6821,
		"title":              "World Cup 2026 Funky House Music Mix",
		"view_count":         276253,
		"automatic_captions": strings.Repeat("x", 50_000), // heavy
	}
	raw := map[string]any{
		"result": inner,
	}
	encoded, _ := json.Marshal(raw)

	prev := buildPreview(raw, encoded)
	if !strings.Contains(prev, `"duration":6821`) {
		t.Errorf("unwrapped preview missing duration. Preview:\n%s", prev)
	}
	if strings.Contains(prev, `"result":"[omitted`) {
		t.Errorf("unwrap did not fire — result still pruned. Preview:\n%s", prev)
	}
}

// TestBuildPreview_FromTypedStructEncodedJSON asserts that when raw
// is a typed Go struct (so a `map[string]any` type assertion fails),
// the preview path still kicks in via re-unmarshalling the encoded
// bytes. This is the case for the MCP driver which returns its own
// typed result value.
func TestBuildPreview_FromTypedStructEncodedJSON(t *testing.T) {
	t.Parallel()

	type meta struct {
		Result struct {
			Title    string `json:"title"`
			Duration int    `json:"duration"`
			Heavy    string `json:"heavy"`
		} `json:"result"`
	}
	var m meta
	m.Result.Title = "World Cup 2026"
	m.Result.Duration = 6821
	m.Result.Heavy = strings.Repeat("x", 50_000)
	encoded, _ := json.Marshal(m)

	prev := buildPreview(m, encoded)
	if !strings.Contains(prev, `"duration":6821`) {
		t.Errorf("struct-shaped raw lost duration. Preview:\n%s", prev)
	}
}

// TestBuildPreview_NonObjectFallsBackToByteTrunc asserts a top-level
// array or scalar (not a JSON object) flows through byte-truncation
// since there's no field structure to be aware of. This is the
// fallback path; pre-fix behaviour for non-objects is preserved.
func TestBuildPreview_NonObjectFallsBackToByteTrunc(t *testing.T) {
	t.Parallel()

	// Top-level array.
	arr := make([]any, 0, 1000)
	for range 1000 {
		arr = append(arr, "item-"+strings.Repeat("x", 50))
	}
	encoded, _ := json.Marshal(arr)
	prev := buildPreview(arr, encoded)
	if len(prev) > previewTotalMaxBytes+len("...(truncated)") {
		t.Errorf("array fallback exceeded budget: %d", len(prev))
	}
	if !strings.HasSuffix(prev, "...(truncated)") {
		t.Errorf("array fallback missing truncation marker")
	}
}

// TestHeavyTruncationSummary_EndToEnd asserts the full
// heavyTruncationSummary shape mirrors what the planner's prompt
// builder expects: `tool`, `size_bytes`, `truncated: true`, `preview`,
// `artifact_ref`. The preview itself MUST carry the now-pinned
// `duration` value, proving the YouTube-loop case is closed.
func TestHeavyTruncationSummary_EndToEnd(t *testing.T) {
	t.Parallel()

	bigNested := strings.Repeat("x", 50_000) // 50 KB single nested string
	raw := map[string]any{
		"automatic_captions": bigNested,
		"duration":           6821,
		"title":              "World Cup 2026",
	}
	encoded, _ := json.Marshal(raw)
	got := heavyTruncationSummary("youtube_get_metadata", raw, encoded, len(encoded), "ref_xyz")

	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("heavyTruncationSummary returned non-map: %T", got)
	}
	for _, key := range []string{"tool", "size_bytes", "truncated", "preview", "artifact_ref"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q in summary", key)
		}
	}
	if m["truncated"] != true {
		t.Errorf("truncated should be true")
	}
	preview, _ := m["preview"].(string)
	if !strings.Contains(preview, `"duration":6821`) {
		t.Errorf("E2E preview missing duration:6821 — the YouTube-loop regression gate failed. Preview:\n%s", preview)
	}
}
