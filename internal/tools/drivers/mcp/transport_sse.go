package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
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

// unownedRequestTimeout bounds a request this driver issues whose CONTEXT THE
// DRIVER DOES NOT OWN — see [unownedBoundingTransport] for exactly which
// requests those are and why the bound is load-bearing rather than hygiene.
const unownedRequestTimeout = 15 * time.Second

// unownedBoundingTransport applies [unownedRequestTimeout] to any request that
// carries NO deadline of its own and is not a server→client event stream. It is
// the OUTERMOST-but-one wrapper on every MCP HTTP connection.
//
// # Why it exists (a real stall, not a hypothetical)
//
// Every request the RUNTIME originates is already bounded: a tool call carries
// the dispatch policy's deadline, an attach carries the caller's. But the MCP SDK
// also issues requests on a context this driver never sees — notably the
// session-termination request its session teardown makes after a failed
// handshake. Against a server that accepts the TCP connection and then answers
// nothing, that request blocks FOREVER, so the caller's own bounded context ends
// the handshake but NOT the teardown that follows it, and the caller never
// returns. That stall reaches the admin add verb's request AND the run-start
// re-attach leg, which is synchronous at run start.
//
// # Why it is shaped this way
//
//   - It bounds only requests with NO deadline. A request the runtime bounded
//     keeps its own budget untouched, so an operator who raises a slow tool's
//     `timeout_ms` above this value is never silently pre-empted. A blanket
//     transport-level response-header timeout would have exactly that bug.
//   - It exempts the server→client event stream, identified by its
//     `Accept: text/event-stream` (the protocol-level signal, not a URL or method
//     guess). That stream is deliberately long-lived; bounding it would break
//     streaming outright.
type unownedBoundingTransport struct {
	base http.RoundTripper
}

func (t *unownedBoundingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, bounded := req.Context().Deadline(); bounded {
		return t.base.RoundTrip(req)
	}
	if strings.Contains(strings.ToLower(req.Header.Get("Accept")), "text/event-stream") {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), unownedRequestTimeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	// The response body is still streaming under ctx: cancelling now would
	// truncate it. Tie the cancel to the body's Close instead, so the bound
	// covers the whole exchange without cutting a body the caller is reading.
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnCloseBody releases a request-scoped cancel when the response body is
// closed, so the bounded context outlives the headers but not the exchange.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeOnce.Do(b.cancel)
	return err
}

// buildHTTPClient returns an *http.Client whose transport injects the
// operator's static cfg.Headers and — when the connection binds an OAuth
// provider — the per-identity bearer carried on each call's ctx. Every
// connection gets its own client, so the unowned-request liveness bound is
// installed whether or not the connection injects anything.
//
// Transport layering (load-bearing): the bearer transport is the INNERMOST
// wrapper, so it sets Authorization on the request that actually reaches the
// base RoundTripper — LAST, after the static header transport ran. A stray
// static `Authorization` header (rejected at validation, but defence in
// depth) can therefore never shadow the per-identity bearer.
func buildHTTPClient(cfg Config) *http.Client {
	// The liveness bound sits at the BOTTOM of the wrapper stack (closest to the
	// network), so it sees the request as it will actually be sent and every
	// injecting wrapper above it has already run.
	rt := http.RoundTripper(&unownedBoundingTransport{base: http.DefaultTransport})
	if cfg.OAuthProvider != nil {
		rt = &bearerInjectingTransport{base: rt}
	}
	// Per-user credential injection (receiver-style server): the HTTP forms
	// (header / Basic) ride each call's ctx into this innermost injecting
	// transport, so the injected header wins over any static header and holds NO
	// mutable state (the value rides req.Context()). The `_meta` form injects
	// into the JSON-RPC body, not a header, so it needs no transport here.
	if cfg.Injection != nil && (cfg.Injection.Form == InjectionFormHeader || cfg.Injection.Form == InjectionFormBasic) {
		rt = &injectedHeaderTransport{base: rt}
	}
	if len(cfg.Headers) > 0 {
		rt = &headerInjectingTransport{
			base:    rt,
			headers: copyHeaders(cfg.Headers),
		}
	}
	// The challenge capturer is the OUTERMOST wrapper so it observes the final
	// response (after any auth injection) and can read a `401`'s
	// `WWW-Authenticate` header (OAuth step-up) or a `403`'s
	// `insufficient_scope` shortfall. It never alters the call — see
	// challengeCapturingTransport.
	if cfg.OnAuthChallenge != nil || cfg.OnScopeShortfall != nil {
		rt = &challengeCapturingTransport{
			base:             rt,
			onChallenge:      cfg.OnAuthChallenge,
			onScopeShortfall: cfg.OnScopeShortfall,
		}
	}
	// Every connection gets its own client. It deliberately does NOT fall back to
	// http.DefaultClient any more: that client carries an unwrapped
	// http.DefaultTransport, so an otherwise-plain connection to an unresponsive
	// server would keep the stall the bound above exists to close. The allocation
	// is per-connection (attach-time), not per-call.
	client := &http.Client{Transport: rt}
	// Redirect hardening for a credential-injecting connection (the
	// credential-plane invariant). The injecting transports re-inject the
	// credential on EVERY hop from inside the RoundTripper, so Go's cross-host
	// header stripping does NOT help — an allow-listed host that answers a 3xx
	// would otherwise send the credential to an arbitrary redirect target.
	// Re-validate each redirect target host against the bound provider's
	// boot-declared AllowedDownstreamHosts; a redirect to an unlisted host is
	// refused with a typed sentinel, so the credential never reaches an off-list
	// host. This covers both the bearer path and the receiver-style injection
	// forms (including `_meta`, whose credential rides the redirected body).
	switch {
	case cfg.OAuthProvider != nil:
		if strict, ok := cfg.OAuthProvider.(interface{ RefuseRedirects() bool }); ok && strict.RefuseRedirects() {
			client.CheckRedirect = refuseEveryCredentialRedirect
		} else {
			client.CheckRedirect = redirectGuardFor(cfg.OAuthProvider.AllowedDownstreamHosts())
		}
	case cfg.Injection != nil:
		client.CheckRedirect = redirectGuardFor(cfg.Injection.Provider.AllowedDownstreamHosts())
	}
	return client
}

// ErrRedirectToUnlistedHost is the typed sentinel the MCP bearer client
// refuses a redirect with when the redirect target host is not in the bound
// provider's downstream-sink allow-list. Callers compare with errors.Is.
var ErrRedirectToUnlistedHost = errors.New("mcp: redirect target host is not in the bound provider's allowed_downstream_hosts")

func refuseEveryCredentialRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w: signed credential binding refuses redirect to %q", ErrRedirectToUnlistedHost, req.URL.String())
}

// redirectGuardFor builds an http.Client CheckRedirect that refuses any
// redirect whose target host is not in allowList (normalised via the ONE
// shared normaliser). An empty allow-list refuses every redirect — a
// bearer-injecting connection with no declared sink must never follow a
// redirect. A bounded redirect budget also caps chains.
func redirectGuardFor(allowList []string) func(*http.Request, []*http.Request) error {
	const maxRedirects = 5
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: stopped after %d redirects", ErrRedirectToUnlistedHost, maxRedirects)
		}
		target := config.NormalizeDownstreamHost(req.URL.String())
		for _, h := range allowList {
			if config.NormalizeDownstreamHost(h) == target {
				return nil
			}
		}
		return fmt.Errorf("%w: %q", ErrRedirectToUnlistedHost, target)
	}
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
// whose ctx carries no bearer passes through untouched (the unbound-call path).
type bearerInjectingTransport struct {
	base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (b *bearerInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := bearerFrom(req.Context())
	if tok == "" {
		// An unbound call carries no bearer; pass through so static Headers
		// remain the only auth. Pair-owned initialize/discovery paths resolve
		// before reaching this transport.
		return b.base.RoundTrip(req)
	}
	// Clone so we never mutate the caller's request (the SDK may reuse it
	// across retries). Preserve the ctx so a nested transport still reads it.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(clone)
}

// injectedHeaderCtxKey is the unexported key under which per-call injected
// headers (the header / Basic receiver-style forms) are carried on a request's
// ctx for injectedHeaderTransport to read.
type injectedHeaderCtxKey struct{}

// withInjectedHeaders returns a child ctx carrying headers to Set on the
// outbound request. An empty map is a no-op.
func withInjectedHeaders(ctx context.Context, headers map[string]string) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	return context.WithValue(ctx, injectedHeaderCtxKey{}, headers)
}

// injectedHeadersFrom returns the per-call injected headers carried on ctx, or nil.
func injectedHeadersFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	h, ok := ctx.Value(injectedHeaderCtxKey{}).(map[string]string)
	if !ok {
		return nil
	}
	return h
}

// injectedHeaderTransport is the context-aware RoundTripper that Sets the
// per-call injected headers (the receiver-style header / Basic forms) carried on
// the request's ctx (see withInjectedHeaders). It holds NO mutable state — the
// values ride req.Context(), so one shared transport serves N concurrent
// identities with no credential bleed (the concurrent-reuse contract). A request
// whose ctx carries no injected headers passes through untouched (connect-time
// and unbound paths).
type injectedHeaderTransport struct {
	base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *injectedHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	headers := injectedHeadersFrom(req.Context())
	if len(headers) == 0 {
		return t.base.RoundTrip(req)
	}
	// Clone so we never mutate the caller's request (the SDK may reuse it across
	// retries). Preserve the ctx so a nested transport still reads it.
	clone := req.Clone(req.Context())
	for k, v := range headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
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
