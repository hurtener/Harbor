package output_test

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/llm/output"
)

// TestParseRespondWith_Happy — a well-formed
// `{"name":"respond_with","arguments":{...}}` envelope unwraps to its
// nested `arguments` payload, byte-for-byte.
func TestParseRespondWith_Happy(t *testing.T) {
	content := `{"name":"respond_with","arguments":{"sentiment":"positive","confidence":0.9}}`
	args, ok := output.ParseRespondWith(content)
	if !ok {
		t.Fatalf("ParseRespondWith(%q) ok=false, want true", content)
	}
	var got struct {
		Sentiment  string  `json:"sentiment"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(args, &got); err != nil {
		t.Fatalf("unmarshal unwrapped args: %v", err)
	}
	if got.Sentiment != "positive" || got.Confidence != 0.9 {
		t.Errorf("unwrapped args = %+v, want {positive 0.9}", got)
	}
}

// TestParseRespondWith_EmptyObjectArguments — `"arguments":{}` is a
// legitimate value (a schema with no required properties), not a
// malformed envelope: it still unwraps.
func TestParseRespondWith_EmptyObjectArguments(t *testing.T) {
	args, ok := output.ParseRespondWith(`{"name":"respond_with","arguments":{}}`)
	if !ok {
		t.Fatal("ParseRespondWith with empty-object arguments ok=false, want true")
	}
	if string(args) != "{}" {
		t.Errorf("args = %s, want {}", args)
	}
}

// TestParseRespondWith_Malformed — content that superficially looks
// like the envelope but is broken (truncated JSON, `arguments` present
// but empty, `name` wrong) reports ok=false — never an error, and never
// a partial/garbage unwrap.
func TestParseRespondWith_Malformed(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "truncated_json", content: `{"name":"respond_with","argum`},
		{name: "missing_arguments_field", content: `{"name":"respond_with"}`},
		{name: "wrong_name", content: `{"name":"some_other_tool","arguments":{"a":1}}`},
		{name: "empty_string", content: ``},
		{name: "json_array_not_object", content: `[1,2,3]`},
		{name: "json_scalar", content: `42`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := output.ParseRespondWith(tc.content)
			if ok {
				t.Errorf("ParseRespondWith(%q) ok=true, want false (args=%s)", tc.content, args)
			}
			if args != nil {
				t.Errorf("ParseRespondWith(%q) args=%s, want nil on ok=false", tc.content, args)
			}
		})
	}
}

// TestParseRespondWith_NonEnvelope — ordinary schema-shaped content (no
// envelope wrapper at all — the Native/Prompted profile shape) is not
// mistaken for the envelope: ok=false, so the caller's fallback (use the
// content verbatim) fires.
func TestParseRespondWith_NonEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "direct_schema_shape", content: `{"sentiment":"positive","confidence":0.9}`},
		{name: "plain_prose", content: `just some prose, not JSON at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := output.ParseRespondWith(tc.content); ok {
				t.Errorf("ParseRespondWith(%q) ok=true, want false (not an envelope)", tc.content)
			}
		})
	}
}
