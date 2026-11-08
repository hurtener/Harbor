package llm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestPromptCacheScope_StableForSameInputsAndSeparatesEachField(t *testing.T) {
	t.Parallel()
	base := llm.PromptCacheScopeInput{TenantID: "tenant-a", AgentID: "agent-1", ToolsDigest: "tools", SystemDigest: "spine"}
	scope := llm.PromptCacheScope(base)
	if scope != llm.PromptCacheScope(base) {
		t.Fatal("scope is not deterministic")
	}
	variants := []llm.PromptCacheScopeInput{
		{TenantID: "tenant-b", AgentID: "agent-1", ToolsDigest: "tools", SystemDigest: "spine"},
		{TenantID: "tenant-a", AgentID: "agent-2", ToolsDigest: "tools", SystemDigest: "spine"},
		{TenantID: "tenant-a", AgentID: "agent-1", ToolsDigest: "tools2", SystemDigest: "spine"},
		{TenantID: "tenant-a", AgentID: "agent-1", ToolsDigest: "tools", SystemDigest: "spine2"},
		// Length-prefixing: shifting bytes between fields must not collide.
		{TenantID: "tenant-aa", AgentID: "gent-1", ToolsDigest: "tools", SystemDigest: "spine"},
	}
	for i, v := range variants {
		if llm.PromptCacheScope(v) == scope {
			t.Errorf("variant %d shares the base scope", i)
		}
	}
	for _, raw := range []string{"tenant-a", "agent-1"} {
		if strings.Contains(scope, raw) {
			t.Errorf("scope carries raw identifier %q", raw)
		}
	}
}

func TestPromptCacheKey_BindsProviderAndModel(t *testing.T) {
	t.Parallel()
	if llm.PromptCacheKey("", "openai", "m") != "" {
		t.Error("an empty scope must yield no key")
	}
	k := llm.PromptCacheKey("scope", "openai", "gpt")
	if !strings.HasPrefix(k, "hpc1-") || len(k) != len("hpc1-")+40 {
		t.Errorf("key %q has an unexpected shape", k)
	}
	if k != llm.PromptCacheKey("scope", "openai", "gpt") {
		t.Error("key is not deterministic")
	}
	for _, other := range []string{
		llm.PromptCacheKey("scope2", "openai", "gpt"),
		llm.PromptCacheKey("scope", "openrouter", "gpt"),
		llm.PromptCacheKey("scope", "openai", "gpt-mini"),
	} {
		if other == k {
			t.Error("distinct scope/provider/model share a key")
		}
	}
}

func TestStripSchemaAnnotations_RemovesKeywordsAtEverySubschema(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "$comment": "generated",
  "type": "object",
  "properties": {
    "$comment": {"type": "string", "$comment": "a property literally named $comment survives"},
    "items": {"type": "array", "items": {"$schema": "x", "type": "string", "enum": ["$comment"]}},
    "pick": {"anyOf": [{"$comment": "a", "type": "string"}, {"type": "integer"}]},
    "note": {"type": "string", "default": {"$comment": "data, not a keyword"}, "description": "<b>&</b>"}
  },
  "additionalProperties": {"$comment": "x", "type": "string"},
  "$defs": {"d": {"$schema": "x", "type": "string"}},
  "x-ext": {"$comment": "unknown extension keyword is data"}
}`)
	out := llm.StripSchemaAnnotations(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stripped schema is not JSON: %v\n%s", err, out)
	}
	want := `{"type":"object","properties":{"$comment":{"type":"string"},"items":{"type":"array","items":{"type":"string","enum":["$comment"]}},"pick":{"anyOf":[{"type":"string"},{"type":"integer"}]},"note":{"type":"string","default":{"$comment":"data, not a keyword"},"description":"<b>&</b>"}},"additionalProperties":{"type":"string"},"$defs":{"d":{"type":"string"}},"x-ext":{"$comment":"unknown extension keyword is data"}}`
	if string(out) != want {
		t.Errorf("stripped schema (key order preserved):\n got %s\nwant %s", out, want)
	}
}

func TestStripSchemaAnnotations_ReturnsInputUnchangedWhenNothingToStrip(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		`{"type":"object","properties":{"q":{"type":"string"}}}`,
		`{ "type" : "object" }`,      // whitespace is not rewritten
		`{"$schema": "x", "type": }`, // malformed stays malformed for the caller to reject
		`{"$schema":"x"} trailing`,
	} {
		if got := llm.StripSchemaAnnotations(json.RawMessage(in)); string(got) != in {
			t.Errorf("input %q rewritten to %q", in, got)
		}
	}
}
