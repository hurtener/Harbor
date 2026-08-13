package mcp

import (
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestLowerCallToolResult_TypedErrorPreservesLoweredResult(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		IsError:           true,
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "provider says no"}},
		StructuredContent: map[string]any{"answer": "structured"},
		Meta: mcpsdk.Meta{tools.MCPResultErrorNamespace: map[string]any{
			"error": map[string]any{"class": string(tools.MCPToolErrorConflict), "message": "already exists"},
		}},
	}
	value, err := lowerCallToolResult(res)
	if err == nil || !errors.Is(err, tools.ErrMCPToolError) {
		t.Fatalf("lower error = %v, want ErrMCPToolError", err)
	}
	if value.Text != "provider says no" {
		t.Fatalf("lowered text = %q", value.Text)
	}
	if value.StructuredContent.(map[string]any)["answer"] != "structured" {
		t.Fatalf("structured content = %#v", value.StructuredContent)
	}
	var typed *tools.MCPToolResultError
	if !errors.As(err, &typed) || typed.Class != tools.MCPToolErrorConflict {
		t.Fatalf("typed error = %#v", typed)
	}
}

func TestLowerCallToolResult_LegacyAndMalformedAreTransient(t *testing.T) {
	for _, res := range []*mcpsdk.CallToolResult{
		{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "transient"}}},
		{IsError: true, Meta: mcpsdk.Meta{tools.MCPResultErrorNamespace: "malformed"}},
	} {
		_, err := lowerCallToolResult(res)
		var typed *tools.MCPToolResultError
		if !errors.As(err, &typed) || typed.Class != tools.MCPToolErrorTransient {
			t.Fatalf("error = %#v, want transient typed error", err)
		}
	}
}
