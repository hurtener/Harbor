package mcp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mockServer wires an in-process mcp.Server with a small surface
// suitable for the driver's tests: an `echo` tool, a `flaky` tool
// (configurable failure-count), an `add` tool (typed in/out), a
// single resource `mem://hello`, and a `greet` prompt. It also
// exposes hooks to assert per-call identity propagation.
//
// The server is paired with a Provider via the SDK's
// `NewInMemoryTransports` factory — both ends share one Go process,
// no kernel-level transport involved. This is the recommended
// in-process test seam (mcp_example_test.go).
type mockServer struct {
	server          *mcpsdk.Server
	flakyTarget     atomic.Int64 // when > 0, the first N flaky calls return IsError
	flakyAttempts   atomic.Int64
	alwaysFailCount atomic.Int64 // per-tool attempt counters for the always-erroring siblings
	alwaysFail2     atomic.Int64
	identityCapture sync.Map // map[string]map[string]any — keyed by tool name
}

// newMockServer constructs the test MCP server with the expected
// surface. The server is ready but not yet connected; pair it with
// `pairProvider` to spin up a `Provider` bound via in-memory
// transports.
func newMockServer() *mockServer {
	m := &mockServer{}
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-test-server", Version: "v0"},
		&mcpsdk.ServerOptions{
			SubscribeHandler: func(ctx context.Context, req *mcpsdk.SubscribeRequest) error {
				return nil
			},
			UnsubscribeHandler: func(ctx context.Context, req *mcpsdk.UnsubscribeRequest) error {
				return nil
			},
		},
	)
	m.server = srv

	// echo: returns the `text` argument back. Captures the caller's
	// _meta map for identity-propagation assertions.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "echo",
			Description: "Echo the text argument back.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string"},
				},
				"required":             []string{"text"},
				"additionalProperties": false,
			},
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			m.captureMeta("echo", req.Params.Meta)
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}},
			}, nil, nil
		},
	)

	// flaky: returns IsError until flakyTarget calls have failed,
	// then returns text. Used to exercise the ToolPolicy retry
	// shell over the MCP wire.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "flaky",
			Description: "Returns a transient error for the first N calls.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			m.captureMeta("flaky", req.Params.Meta)
			n := m.flakyAttempts.Add(1)
			target := m.flakyTarget.Load()
			if n <= target {
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("transient: attempt %d", n)}},
					IsError: true,
				}, nil, nil
			}
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("ok: attempt %d", n)}},
			}, nil, nil
		},
	)

	// always_fail / always_fail2: every call returns a transient-class
	// IsError, incrementing a per-tool attempt counter. Used by the
	// Phase 26b per-tool-policy tests to assert exact attempt counts: a
	// tool with a per-tool override of max_attempts:1 makes exactly ONE
	// attempt; a sibling on the server default makes the default count.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "always_fail",
			Description: "Always returns a transient error.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			n := m.alwaysFailCount.Add(1)
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("transient: attempt %d", n)}},
				IsError: true,
			}, nil, nil
		},
	)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "always_fail2",
			Description: "Always returns a transient error (sibling).",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			n := m.alwaysFail2.Add(1)
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("transient: attempt %d", n)}},
				IsError: true,
			}, nil, nil
		},
	)

	// add: typed-in/typed-out tool — Harbor side reads the result as
	// a TextContent that decodes back to JSON. Identity is captured.
	type addIn struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name: "add",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "integer"},
					"b": map[string]any{"type": "integer"},
				},
				"required":             []string{"a", "b"},
				"additionalProperties": false,
			},
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in addIn) (*mcpsdk.CallToolResult, any, error) {
			m.captureMeta("add", req.Params.Meta)
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("%d", in.A+in.B)}},
			}, nil, nil
		},
	)

	// One resource — the driver maps to a `read_resource`-style tool.
	srv.AddResource(
		&mcpsdk.Resource{
			URI:         "mem://hello",
			Name:        "hello",
			Description: "Static hello blob.",
			MIMEType:    "text/plain",
		},
		func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			m.captureMeta("read_resource", req.Params.Meta)
			return &mcpsdk.ReadResourceResult{
				Contents: []*mcpsdk.ResourceContents{
					{
						URI:      req.Params.URI,
						MIMEType: "text/plain",
						Text:     "hello world",
					},
				},
			}, nil
		},
	)

	// One prompt — the driver maps to a `get_prompt`-style tool.
	srv.AddPrompt(
		&mcpsdk.Prompt{
			Name: "greet",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "who", Required: true},
			},
		},
		func(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
			m.captureMeta("get_prompt", req.Params.Meta)
			who := "world"
			if req.Params.Arguments != nil {
				if v, ok := req.Params.Arguments["who"]; ok && v != "" {
					who = v
				}
			}
			return &mcpsdk.GetPromptResult{
				Messages: []*mcpsdk.PromptMessage{
					{Role: "assistant", Content: &mcpsdk.TextContent{Text: "Hello, " + who}},
				},
			}, nil
		},
	)

	return m
}

func (m *mockServer) captureMeta(tool string, meta mcpsdk.Meta) {
	out := map[string]any{}
	for k, v := range meta {
		out[k] = v
	}
	m.identityCapture.Store(tool, out)
}

// metaFor returns the captured _meta map for the named tool's most
// recent invocation. Empty when the tool was never called.
func (m *mockServer) metaFor(tool string) map[string]any {
	v, ok := m.identityCapture.Load(tool)
	if !ok {
		return nil
	}
	return v.(map[string]any)
}

// setFlakyTarget sets the number of leading invocations that should
// fail before flaky succeeds.
func (m *mockServer) setFlakyTarget(n int64) {
	m.flakyTarget.Store(n)
	m.flakyAttempts.Store(0)
}

// pairProvider connects p to m via in-memory transports. Returns a
// cleanup that closes both sides. Both Provider.Connect and the
// server's session run on goroutines from the SDK.
func pairProvider(t *testing.T, m *mockServer, p *Provider) func() {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()

	// Server side first (so it's ready to receive initialize).
	serverSession, err := m.server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}

	// Bind the client to the same in-memory transport pair: we have
	// to bypass selectTransport (which would re-build a streamable /
	// SSE / stdio transport from Config) by calling client.Connect
	// directly.
	clientSession, err := p.client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	p.mu.Lock()
	p.session = clientSession
	p.selectedMode = MCPTransportMode("inmemory")
	p.mu.Unlock()

	// Stash the server side so the test can reach it for
	// server-pushed notifications.
	recordServerForTest(p, m.server)

	return func() {
		forgetServerForTest(p)
		_ = clientSession.Close()
		_ = serverSession.Wait()
	}
}
