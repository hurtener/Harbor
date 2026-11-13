package mcp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestLowerCallToolResult_TypedErrorPreservesLoweredResult(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		IsError:           true,
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "provider says no"}},
		StructuredContent: map[string]any{"answer": "structured"},
		Meta: mcpsdk.Meta{
			"ui": map[string]any{"resourceUri": "ui://app/main.html"},
			tools.MCPResultErrorNamespace: map[string]any{
				"error": map[string]any{"class": string(tools.MCPToolErrorConflict), "message": "already exists"},
			},
		},
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
	lowered, ok := typed.Result.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("typed result value = %T, want MCPToolValue", typed.Result.Value)
	}
	if lowered.Text != "provider says no" {
		t.Fatalf("typed result text = %q", lowered.Text)
	}
	structured, ok := lowered.StructuredContent.(map[string]any)
	if !ok || structured["answer"] != "structured" {
		t.Fatalf("typed result structured content = %#v", lowered.StructuredContent)
	}
	if lowered.AppRef == nil || lowered.AppRef.ResourceURI != "ui://app/main.html" {
		t.Fatalf("typed result AppRef = %+v", lowered.AppRef)
	}
}

// The structured code/doc_id/outline shape matches the read refusal emitted by
// a real document provider's MCP ErrorOutput; the SDK transports it unchanged.
func TestProvider_StructuredErrorCodesReachRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		code       string
		retryable  any
		wantClass  tools.MCPToolErrorClass
		wantCalls  int64
		recognized bool
	}{
		{"invalid_argument", nil, tools.MCPToolErrorInvalidArgument, 1, true},
		{"validation_failed", nil, tools.MCPToolErrorValidation, 1, true},
		{"forbidden_scope", nil, tools.MCPToolErrorAuthorization, 1, true},
		{"scope_blocked", nil, tools.MCPToolErrorAuthorization, 1, true},
		{"scope_mismatch", nil, tools.MCPToolErrorAuthorization, 1, true},
		{"not_found", nil, tools.MCPToolErrorNotFound, 1, true},
		{"document_too_large", nil, tools.MCPToolErrorToolDomain, 1, true},
		{"not_found", true, tools.MCPToolErrorTransient, 4, false},
		{"not_found", "false", tools.MCPToolErrorTransient, 4, false},
		{"provider_unavailable", nil, tools.MCPToolErrorTransient, 4, false},
		{"future_code", nil, tools.MCPToolErrorTransient, 4, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			p, server, cleanup := newPolicyTestProvider(t, fastPolicy(3), nil)
			defer cleanup()
			var calls atomic.Int64
			structured := map[string]any{
				"code":    tc.code,
				"doc_id":  "fixture-document",
				"outline": map[string]any{"sections": []any{map[string]any{"section_id": "fixture-section"}}},
			}
			if tc.retryable != nil {
				structured["retryable"] = tc.retryable
			}
			mcpsdk.AddTool(server.server, &mcpsdk.Tool{Name: "read_refusal", InputSchema: map[string]any{"type": "object"}},
				func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
					calls.Add(1)
					return &mcpsdk.CallToolResult{
						IsError:           true,
						Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: tc.code}},
						StructuredContent: structured,
					}, nil, nil
				})
			ctx := mustIdentity(t)
			descriptors, err := p.Discover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := findByName(descriptors, "mock_read_refusal")
			if descriptor == nil {
				t.Fatal("read_refusal was not discovered")
			}
			_, err = descriptor.Invoke(ctx, []byte(`{}`))
			var typed *tools.MCPToolResultError
			if !errors.As(err, &typed) || typed.Class != tc.wantClass || typed.Recognized != tc.recognized {
				t.Fatalf("result error = %#v, want %s (recognized %v)", err, tc.wantClass, tc.recognized)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("provider calls = %d, want %d", got, tc.wantCalls)
			}
			retained, ok := typed.Result.Value.(MCPToolValue)
			if !ok || retained.Text != tc.code {
				t.Fatalf("retained result = %#v", typed.Result.Value)
			}
			output := retained.StructuredContent.(map[string]any)
			outline := output["outline"].(map[string]any)
			section := outline["sections"].([]any)[0].(map[string]any)
			if output["code"] != tc.code || output["doc_id"] != "fixture-document" || section["section_id"] != "fixture-section" {
				t.Fatalf("provider refusal lost code/document/outline: %#v", output)
			}
			if tc.code == "document_too_large" && !strings.Contains(typed.Message, "outline/section selectors") {
				t.Fatalf("model guidance missing: %q", typed.Message)
			}
		})
	}
}

func TestLowerCallToolResult_SuccessCodeDoesNotBecomeError(t *testing.T) {
	value, err := lowerCallToolResult(&mcpsdk.CallToolResult{
		StructuredContent: map[string]any{"code": "not_found"},
	})
	if err != nil || value.StructuredContent.(map[string]any)["code"] != "not_found" {
		t.Fatalf("successful result = %#v, %v", value, err)
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

// A provider that publishes only a recognized structured code keeps its
// human-readable repair instructions in the planner-visible error.
func TestLowerCallToolResult_CodeOnlyStructuredErrorKeepsProviderText(t *testing.T) {
	const detail = "field section_id must name an existing section; available sections: intro, results"
	_, err := lowerCallToolResult(&mcpsdk.CallToolResult{
		IsError:           true,
		StructuredContent: map[string]any{"code": "invalid_argument"},
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: detail}},
	})
	var typed *tools.MCPToolResultError
	if !errors.As(err, &typed) || typed.Class != tools.MCPToolErrorInvalidArgument || !typed.Recognized {
		t.Fatalf("result error = %#v, want recognized invalid_argument", err)
	}
	if !strings.Contains(err.Error(), "invalid_argument") || !strings.Contains(err.Error(), detail) {
		t.Fatalf("planner-visible error lost the provider repair detail: %q", err.Error())
	}
}
