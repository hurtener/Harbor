package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStreamableTransport builds an mcpsdk.StreamableClientTransport
// from cfg. The SDK's streamable transport is bidirectional over a
// single HTTP request (newer than SSE) and ships internal reconnect
// for the standalone SSE stream with exponential backoff — there is
// no need for an internal reconnect state machine. Operator
// recovery for transient transport failure rides on the outer
// `ToolPolicy` retry shell.
//
// URL MUST be set; caller (selectTransport) validates this. Headers
// flow through the shared headerInjectingTransport so auth is
// uniform with SSE.
func newStreamableTransport(cfg Config) mcpsdk.Transport {
	client := buildHTTPClient(cfg)
	return &mcpsdk.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: client,
		// A standalone SSE GET is connection-scoped and inherits the Connect
		// context. Ordinary shared/per-entry OAuth connects are bearerless; a
		// pair-private OwnOAuthProvider connect carries one short-lived preparation
		// bearer, but the SDK detaches and preserves that fixed value for a stream
		// that may outlive it. Neither context can provide a refreshable,
		// tenant/user-safe credential for one long-lived shared stream. Disable the
		// optional stream whenever the connection or any entry resolves OAuth per
		// identity. Fully unbound and static-header connections retain the default.
		DisableStandaloneSSE: cfg.OAuthProvider != nil || len(cfg.ToolOAuthProviders) > 0,
		// Negative disables retries; zero defaults to 5. We pass zero
		// so the SDK's default applies; ToolPolicy retries on top.
		MaxRetries: 0,
	}
}
