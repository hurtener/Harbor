package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunWithPolicy_MCPResultClassificationPermanentDoesNotRetry(t *testing.T) {
	attempts := 0
	want := ToolResult{Value: map[string]any{
		"text":       "bounded failure",
		"structured": map[string]any{"answer": "preserved"},
		"app":        map[string]any{"resource": "ui://bounded"},
	}}
	result, err := RunWithPolicy(context.Background(), json.RawMessage(`{}`), func(context.Context, json.RawMessage) (ToolResult, error) {
		attempts++
		mcpErr := NewMCPToolResultError(MCPToolErrorInvalidArgument, "bad input")
		var typed *MCPToolResultError
		if !errors.As(mcpErr, &typed) {
			t.Fatalf("errors.As(mcpErr): %v", mcpErr)
		}
		typed.Result = want
		return typed.Result, typed
	}, nil, nil, ToolPolicy{MaxRetries: 4})
	if err == nil || attempts != 1 {
		t.Fatalf("err=%v attempts=%d, want one permanent attempt", err, attempts)
	}
	if got, ok := result.Value.(map[string]any); !ok || got["text"] != "bounded failure" || got["structured"].(map[string]any)["answer"] != "preserved" || got["app"].(map[string]any)["resource"] != "ui://bounded" {
		t.Fatalf("result=%#v, want original bounded MCP result", result)
	}
	var typed *MCPToolResultError
	if !errors.As(err, &typed) || typed.Class != MCPToolErrorInvalidArgument {
		t.Fatalf("err=%v, want typed permanent MCP error", err)
	}
}

func TestRunWithPolicy_MCPResultClassificationTransientRecovers(t *testing.T) {
	attempts := 0
	result, err := RunWithPolicy(context.Background(), json.RawMessage(`{}`), func(context.Context, json.RawMessage) (ToolResult, error) {
		attempts++
		if attempts == 1 {
			return ToolResult{Value: "stale"}, NewMCPToolResultError(MCPToolErrorProviderUnavailable, "retry")
		}
		return ToolResult{Value: map[string]any{"text": "ok", "structured": map[string]any{"answer": "success"}}}, nil
	}, nil, nil, ToolPolicy{MaxRetries: 1, BackoffBase: 0})
	if err != nil || attempts != 2 {
		t.Fatalf("result=%#v err=%v attempts=%d, want recovery on second attempt", result, err, attempts)
	}
	got := result.Value.(map[string]any)
	if got["text"] != "ok" || got["structured"].(map[string]any)["answer"] != "success" {
		t.Fatalf("result=%#v, want only successful attempt result", result)
	}
}

func TestRunWithPolicy_MCPResultClassificationExhaustedPreservesFinalResultAndChains(t *testing.T) {
	attempts := 0
	final := ToolResult{Value: map[string]any{"structured": map[string]any{"answer": "last"}}}
	result, err := RunWithPolicy(context.Background(), json.RawMessage(`{}`), func(context.Context, json.RawMessage) (ToolResult, error) {
		attempts++
		mcpErr := NewMCPToolResultError(MCPToolErrorProviderUnavailable, "provider down")
		var typed *MCPToolResultError
		if !errors.As(mcpErr, &typed) {
			t.Fatal("MCP error lost its type")
		}
		if attempts == 2 {
			typed.Result = final
			return final, typed
		}
		return ToolResult{Value: map[string]any{"structured": map[string]any{"answer": "stale"}}}, typed
	}, nil, nil, ToolPolicy{MaxRetries: 1, BackoffBase: 0})
	if attempts != 2 || result.Value.(map[string]any)["structured"].(map[string]any)["answer"] != "last" {
		t.Fatalf("result=%#v attempts=%d, want final attempt result", result, attempts)
	}
	if !errors.Is(err, ErrToolPolicyExhausted) || !errors.Is(err, ErrMCPToolError) {
		t.Fatalf("err=%v, want exhaustion and MCP error chains", err)
	}
	var typed *MCPToolResultError
	if !errors.As(err, &typed) || typed.Class != MCPToolErrorProviderUnavailable {
		t.Fatalf("err=%v, want final typed MCP error", err)
	}
}
