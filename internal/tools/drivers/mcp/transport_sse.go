package mcp

import (
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newSSETransport builds an mcpsdk.SSEClientTransport from cfg.
// Headers are applied via a wrapping http.RoundTripper so the SDK's
// internal `http.Get(c.Endpoint)` carries operator-supplied auth.
//
// URL MUST be set; caller (selectTransport) validates this.
//
// "URL connections require explicit headers for auth (no implicit
// env passthrough)" — brief 03 §4. Headers come from Config, not
// from the process environment. The driver does not inject any
// HARBOR_*-style env vars into the request.
func newSSETransport(cfg Config) mcpsdk.Transport {
	client := buildHTTPClient(cfg)
	return &mcpsdk.SSEClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: client,
	}
}

// buildHTTPClient returns an *http.Client whose transport adds
// cfg.Headers to every request. When Headers is empty, the default
// http.Client is returned (no allocation).
func buildHTTPClient(cfg Config) *http.Client {
	if len(cfg.Headers) == 0 {
		return http.DefaultClient
	}
	base := http.DefaultTransport
	return &http.Client{
		Transport: &headerInjectingTransport{
			base:    base,
			headers: copyHeaders(cfg.Headers),
		},
	}
}

// headerInjectingTransport wraps an http.RoundTripper to add static
// headers to every outbound request. Used to surface operator-
// supplied MCP server auth headers (bearer tokens, API keys) on SSE
// + streamable-HTTP requests.
type headerInjectingTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

// RoundTrip implements http.RoundTripper.
func (h *headerInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request so we don't mutate the caller's headers.
	// The SDK may reuse the request structure across retries; mutating
	// it would silently leak headers between transports.
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, v)
	}
	return h.base.RoundTrip(clone)
}

// copyHeaders returns a defensive copy of m so a later mutation of
// Config.Headers (post-Connect) doesn't whipsaw in-flight requests.
func copyHeaders(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
