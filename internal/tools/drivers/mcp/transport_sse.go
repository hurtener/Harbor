package mcp

import (
	"context"
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
// env passthrough)" — a settled security rule. Headers come from Config, not
// from the process environment. The driver does not inject any
// HARBOR_*-style env vars into the request.
func newSSETransport(cfg Config) mcpsdk.Transport {
	client := buildHTTPClient(cfg)
	return &mcpsdk.SSEClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: client,
	}
}

// buildHTTPClient returns an *http.Client whose transport injects the
// operator's static cfg.Headers and — when the connection binds an OAuth
// provider — the per-identity bearer carried on each call's ctx. When
// neither applies the default http.Client is returned (no allocation).
//
// Transport layering (load-bearing): the bearer transport is the INNERMOST
// wrapper, so it sets Authorization on the request that actually reaches the
// base RoundTripper — LAST, after the static header transport ran. A stray
// static `Authorization` header (rejected at validation, but defence in
// depth) can therefore never shadow the per-identity bearer.
func buildHTTPClient(cfg Config) *http.Client {
	base := http.DefaultTransport
	rt := base
	if cfg.OAuthProvider != nil {
		rt = &bearerInjectingTransport{base: rt}
	}
	if len(cfg.Headers) > 0 {
		rt = &headerInjectingTransport{
			base:    rt,
			headers: copyHeaders(cfg.Headers),
		}
	}
	// The challenge capturer is the OUTERMOST wrapper so it observes the final
	// response (after any auth injection) and can read a `401`'s
	// `WWW-Authenticate` header. It never alters the call — see
	// challengeCapturingTransport.
	if cfg.OnAuthChallenge != nil {
		rt = &challengeCapturingTransport{base: rt, onChallenge: cfg.OnAuthChallenge}
	}
	if rt == base {
		return http.DefaultClient
	}
	return &http.Client{Transport: rt}
}

// bearerCtxKey is the unexported key under which the per-call OAuth bearer is
// carried on a request's ctx for bearerInjectingTransport to read.
type bearerCtxKey struct{}

// withBearer returns a child ctx carrying tok as the per-call OAuth bearer.
// An empty token is a no-op (the transport then injects nothing).
func withBearer(ctx context.Context, tok string) context.Context {
	if tok == "" {
		return ctx
	}
	return context.WithValue(ctx, bearerCtxKey{}, tok)
}

// bearerFrom returns the per-call OAuth bearer carried on ctx, or "".
func bearerFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	tok, ok := ctx.Value(bearerCtxKey{}).(string)
	if !ok {
		return ""
	}
	return tok
}

// bearerInjectingTransport is the context-aware RoundTripper that sets
// `Authorization: Bearer <tok>` on an outbound request when the request's
// ctx carries a per-call bearer (see withBearer). It holds NO mutable state —
// the token rides req.Context(), so one shared transport serves N concurrent
// identities with no token bleed (the concurrent-reuse contract). A request
// whose ctx carries no bearer passes through untouched (the unbound-call and
// connect-time paths).
type bearerInjectingTransport struct {
	base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (b *bearerInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := bearerFrom(req.Context())
	if tok == "" {
		// No per-call identity (connect-time request, or an unbound call) —
		// pass through so static Headers remain the only auth.
		return b.base.RoundTrip(req)
	}
	// Clone so we never mutate the caller's request (the SDK may reuse it
	// across retries). Preserve the ctx so a nested transport still reads it.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(clone)
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
