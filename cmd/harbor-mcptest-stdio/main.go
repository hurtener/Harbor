// harbor-mcptest-stdio is a minimal MCP stdio server used by the
// integration tests. It exposes:
//
//   - `echo` — echoes its `message` argument verbatim (the dispatch
//     proof tool).
//   - `fail_permanent` — always returns a namespaced `conflict`
//     IsError with a bounded actionable message + a structured
//     `current_revision` in the lowered result, the shape a planner
//     needs to choose a reread/retry once.
//   - `fail_transient` — always returns a namespaced `transient`
//     IsError; the reliability shell retries it per the configured
//     per-tool policy.
//   - `fail_oversized` — always returns a namespaced `transient`
//     IsError whose message exceeds the Harbor bound and whose lowered
//     result exceeds the heavy-content threshold, so the integration
//     test can prove the bounded projection (message truncated to
//     MCPToolErrorMessageLimit; result promoted to an artifact
//     reference).
//
// Built only by the integration tests (via `go build` into a tempdir);
// never shipped in releases.
//
// The binary's contract is intentionally tiny: prove that Harbor's
// dev-binary MCP wiring (cmd/harbor/cmd_dev.go::bootDevStack) spawns
// a real subprocess, opens the MCP session, discovers tools, and
// registers their descriptors into the catalog. Anything richer than
// "a few tools that exercise the MCP error surface" would test the
// SDK, not Harbor's consumer wiring.
package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

type echoArgs struct {
	Message string `json:"message" jsonschema:"the text to echo back"`
}

// mcpErrorResult builds an MCP CallToolResult that returns an IsError
// carrying the namespaced `harbor.error{class, message}` classification
// the driver lowers into a typed tools.MCPToolResultError. text is the
// human-readable error body; structured is the typed result content the
// planner retains (e.g. a revision for a conflict).
func mcpErrorResult(class tools.MCPToolErrorClass, message, text string, structured map[string]any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Meta: mcp.Meta{
			tools.MCPResultErrorNamespace: map[string]any{
				"error": map[string]any{
					"class":   string(class),
					"message": message,
				},
			},
		},
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: structured,
		IsError:           true,
	}
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

	mcp.AddTool(server, &mcp.Tool{
		Name:        "fail_permanent",
		Description: "Always fails with a namespaced conflict classification.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return mcpErrorResult(
			tools.MCPToolErrorConflict,
			"document changed; the stored copy now has revision rev-42",
			"conflict: current revision is rev-42",
			map[string]any{"current_revision": "rev-42"},
		), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "fail_transient",
		Description: "Always fails with a namespaced transient classification.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return mcpErrorResult(
			tools.MCPToolErrorTransient,
			"upstream hiccup; please retry",
			"transient: upstream hiccup",
			nil,
		), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "fail_oversized",
		Description: "Always fails with an oversized message and result.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		// Permanent class (invalid_argument) so the reliability shell
		// makes exactly ONE attempt — the assertion is about bounding,
		// not retries — while the message exceeds
		// MCPToolErrorMessageLimit and the lowered result exceeds the
		// heavy-output threshold.
		return mcpErrorResult(
			tools.MCPToolErrorInvalidArgument,
			strings.Repeat("m", 2000)+" MCP_MSG_TAIL_SENTINEL_77",
			strings.Repeat("z", 40000)+" MCP_RAW_TAIL_SENTINEL_88",
			nil,
		), nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		// Log to stderr — stdout is the MCP wire transport and must
		// not carry log noise.
		log.New(os.Stderr, "harbor-mcptest-stdio: ", log.LstdFlags).
			Fatalf("server.Run: %v", err)
	}
}
