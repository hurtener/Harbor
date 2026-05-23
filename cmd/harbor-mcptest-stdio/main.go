// harbor-mcptest-stdio is a minimal MCP stdio server used by the
// Phase 83g integration test. It exposes a single tool — `echo` — and
// nothing else. Built only by the integration test (via `go build`
// into a tempdir); never shipped in releases.
//
// The binary's contract is intentionally tiny: prove that Harbor's
// dev-binary MCP wiring (cmd/harbor/cmd_dev.go::bootDevStack) spawns
// a real subprocess, opens the MCP session, discovers tools, and
// registers their descriptors into the catalog. Anything richer than
// "one tool that echoes its input" would test the SDK, not Harbor's
// consumer wiring.
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoArgs struct {
	Message string `json:"message" jsonschema:"the text to echo back"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "harbor-mcptest",
		Version: "test-fixture",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "Echo the provided message verbatim.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: args.Message}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		// Log to stderr — stdout is the MCP wire transport and must
		// not carry log noise.
		log.New(os.Stderr, "harbor-mcptest-stdio: ", log.LstdFlags).
			Fatalf("server.Run: %v", err)
	}
}
