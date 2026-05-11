package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStreamableTransport builds an mcpsdk.StreamableClientTransport
// from cfg. The SDK's streamable transport is bidirectional over a
// single HTTP request (newer than SSE) and ships internal reconnect
// for the standalone SSE stream with exponential backoff — there is
// no need for a Phase-28-internal reconnect state machine. Operator
// recovery for transient transport failure rides on the outer
// `ToolPolicy` retry shell (D-024).
//
// URL MUST be set; caller (selectTransport) validates this. Headers
// flow through the shared headerInjectingTransport so auth is
// uniform with SSE.
func newStreamableTransport(cfg Config) mcpsdk.Transport {
	client := buildHTTPClient(cfg)
	return &mcpsdk.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: client,
		// Negative disables retries; zero defaults to 5. We pass zero
		// so the SDK's default applies; ToolPolicy retries on top.
		MaxRetries: 0,
	}
}
