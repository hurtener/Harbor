package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPToolValue_MarshalJSON_UnwrapsJSONText asserts a text-only
// MCPToolValue whose Text is well-formed JSON renders as the parsed
// JSON value, not as a `{"Text":"<escaped JSON>"}` wrapper. This is
// the load-bearing regression gate from the Phase 107c live-test bug
// where YouTube metadata (a JSON document in a TextContent block)
// rode to Claude Haiku 4.5 as `{"Text":"{\\\"id\\\":...}"}` and the
// model looped re-emitting the same tool call because it could not
// reliably parse the doubly-encoded shape.
func TestMCPToolValue_MarshalJSON_UnwrapsJSONText(t *testing.T) {
	t.Parallel()
	v := MCPToolValue{
		Text: `{"id":"abc","title":"hello","duration_s":212}`,
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, `"Text"`) {
		t.Errorf("text-only JSON payload marshaled as Text-wrapper: %s", got)
	}
	if !strings.Contains(got, `"id"`) || !strings.Contains(got, `"duration_s"`) {
		t.Errorf("text-only JSON payload lost structure: %s", got)
	}
	// Round-trip: the LLM reads parsed JSON.
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("LLM-side reparse: %v (raw=%s)", err, got)
	}
	if probe["id"] != "abc" || probe["title"] != "hello" {
		t.Errorf("LLM-side fields lost: %v", probe)
	}
}

// TestMCPToolValue_MarshalJSON_TextNonJSONStaysQuoted — when Text is
// plain prose (e.g. an error message or a non-JSON tool result), the
// marshal emits a JSON string so the LLM sees ordinary content.
func TestMCPToolValue_MarshalJSON_TextNonJSONStaysQuoted(t *testing.T) {
	t.Parallel()
	v := MCPToolValue{Text: "video duration is 3 minutes"}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if got != `"video duration is 3 minutes"` {
		t.Errorf("non-JSON Text mis-encoded: %s", got)
	}
}

// TestMCPToolValue_MarshalJSON_EmptyText — degenerate empty MCPToolValue
// (no text, no parts, no structured content) renders as null.
func TestMCPToolValue_MarshalJSON_EmptyText(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(MCPToolValue{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(raw) != "null" {
		t.Errorf("empty value rendered as %s, want null", string(raw))
	}
}

// TestMCPToolValue_MarshalJSON_StructuredContentWins — when the MCP
// server returns typed structured content, that's the canonical LLM-
// facing projection; render it directly (no Text wrapper).
func TestMCPToolValue_MarshalJSON_StructuredContentWins(t *testing.T) {
	t.Parallel()
	v := MCPToolValue{
		StructuredContent: map[string]any{"score": 0.95, "label": "match"},
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, `"StructuredContent"`) || strings.Contains(got, `"Text"`) {
		t.Errorf("structured-content payload still carries Go-struct wrapper: %s", got)
	}
	if !strings.Contains(got, `"score"`) || !strings.Contains(got, `"label"`) {
		t.Errorf("structured-content fields lost: %s", got)
	}
}

// TestMCPToolValue_MarshalJSON_StructuredContentWinsOverText — the
// load-bearing regression gate for the live YouTube test failure.
// mcp-youtube returns BOTH a TextContent block (the metadata as a
// quoted JSON string) AND a StructuredContent field (the same
// metadata as typed JSON). The original branch logic only unwrapped
// when Text was empty; under the typical mcp-youtube response that
// falls through to the wrapper `{"Text":"<escaped JSON>","StructuredContent":{...}}`
// and the LLM cannot reliably parse it. The fix: StructuredContent
// wins whenever it's present — the duplicated Text is the brief-07-
// era fallback and is dropped on the wire under native tool-calling.
func TestMCPToolValue_MarshalJSON_StructuredContentWinsOverText(t *testing.T) {
	t.Parallel()
	structured := map[string]any{
		"id":         "dQw4w9WgXcQ",
		"title":      "Rick Astley — Never Gonna Give You Up",
		"duration_s": 212,
	}
	v := MCPToolValue{
		// Same data, quoted form (the wire shape mcp-youtube emits).
		Text:              `{"id":"dQw4w9WgXcQ","title":"Rick Astley — Never Gonna Give You Up","duration_s":212}`,
		StructuredContent: structured,
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, `"Text"`) {
		t.Errorf("payload retains Text wrapper despite StructuredContent: %s", got)
	}
	if strings.Contains(got, `"StructuredContent"`) {
		t.Errorf("payload retains StructuredContent wrapper key: %s", got)
	}
	// Round-trip: the LLM reads the typed JSON directly.
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("reparse: %v (raw=%s)", err, got)
	}
	if probe["id"] != "dQw4w9WgXcQ" || probe["title"] != "Rick Astley — Never Gonna Give You Up" {
		t.Errorf("fields lost: %v", probe)
	}
}

// TestMCPToolValue_MarshalJSON_MixedContentRetainsWrapper — when Parts
// are present (image, audio, link, embedded), the wrapper IS the
// canonical shape; the default struct render applies so downstream
// consumers can read every part. Verified by asserting the wrapper
// fields are present.
func TestMCPToolValue_MarshalJSON_MixedContentRetainsWrapper(t *testing.T) {
	t.Parallel()
	v := MCPToolValue{
		Text: "preamble",
		Parts: []ContentPart{
			{Kind: ContentKindLink, Link: &LinkRef{URI: "file://foo", Name: "foo"}},
		},
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"Text"`) || !strings.Contains(got, `"Parts"`) {
		t.Errorf("mixed-content value lost wrapper fields: %s", got)
	}
}
