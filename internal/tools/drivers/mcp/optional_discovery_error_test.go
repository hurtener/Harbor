package mcp

import (
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestIsJSONRPCMethodNotFound_OnlyCanonicalTypedError(t *testing.T) {
	methodNotFound := &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "canonical", err: methodNotFound, want: true},
		{name: "wrapped canonical", err: fmt.Errorf("discovery: %w", methodNotFound), want: true},
		{name: "other JSON-RPC code", err: &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid params"}},
		{name: "matching text is not typed", err: errors.New("method not found")},
		{name: "transport", err: errors.New("connection reset")},
		{name: "nil"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJSONRPCMethodNotFound(tc.err); got != tc.want {
				t.Fatalf("isJSONRPCMethodNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
