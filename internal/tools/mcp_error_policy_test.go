package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRunWithPolicy_MCPResultClassificationPermanentDoesNotRetry(t *testing.T) {
	attempts := 0
	_, err := RunWithPolicy(context.Background(), json.RawMessage(`{}`), func(context.Context, json.RawMessage) (ToolResult, error) {
		attempts++
		return ToolResult{}, NewMCPToolResultError(MCPToolErrorInvalidArgument, "bad input")
	}, nil, nil, ToolPolicy{MaxRetries: 4})
	if err == nil || attempts != 1 {
		t.Fatalf("err=%v attempts=%d, want one permanent attempt", err, attempts)
	}
}

func TestRunWithPolicy_MCPResultClassificationTransientRecovers(t *testing.T) {
	attempts := 0
	result, err := RunWithPolicy(context.Background(), json.RawMessage(`{}`), func(context.Context, json.RawMessage) (ToolResult, error) {
		attempts++
		if attempts == 1 {
			return ToolResult{}, NewMCPToolResultError(MCPToolErrorProviderUnavailable, "retry")
		}
		return ToolResult{Value: "ok"}, nil
	}, nil, nil, ToolPolicy{MaxRetries: 1, BackoffBase: 0})
	if err != nil || attempts != 2 || result.Value != "ok" {
		t.Fatalf("result=%#v err=%v attempts=%d, want recovery on second attempt", result, err, attempts)
	}
}
