package tools_test

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// mustMarshal returns the JSON marshalling of `v` or fails the test.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// unmarshalSearchResult extracts a SearchResult from a ToolResult.
// The inproc driver returns the typed struct in `Value`; we go
// through json round-trip so the test exercises the marshal-side
// of the contract.
func unmarshalSearchResult(t *testing.T, res tcat.ToolResult) skilltools.SearchResult {
	t.Helper()
	b, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	var out skilltools.SearchResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal SearchResult: %v", err)
	}
	return out
}

func unmarshalGetResult(t *testing.T, res tcat.ToolResult) skilltools.GetResult {
	t.Helper()
	b, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	var out skilltools.GetResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal GetResult: %v", err)
	}
	return out
}

func unmarshalListResult(t *testing.T, res tcat.ToolResult) skilltools.ListResult {
	t.Helper()
	b, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	var out skilltools.ListResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal ListResult: %v", err)
	}
	return out
}

// outNames returns the Name field of every skill, used for compact
// failure messages.
func outNames(in []skills.Skill) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Name
	}
	return out
}
