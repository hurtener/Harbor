package mcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OAuth requirement discovery — challenge capture (southbound MCP HTTP edge).
//
// The MCP authorization spec (2025-06-18) has an unauthorized HTTP call answer
// `401` + `WWW-Authenticate: Bearer resource_metadata="…"`. This file captures
// that challenge at the shared HTTP-transport choke point (buildHTTPClient) so
// the connection's registry state records the advertised OAuth requirement.
//
// Capture is pure observation: it NEVER retries, NEVER attaches credentials,
// and NEVER alters the call's error semantics — the caller still sees the
// dial/call failure it would see today. The discovered metadata CHAIN is
// walked later, on demand (an operator probe), by the report-only walker in
// internal/tools/auth; nothing here follows any discovered URL.

// AuthChallenge is a parsed `WWW-Authenticate` Bearer challenge captured off a
// `401` response from an MCP HTTP server. It is inert data recorded on the
// connection's registry state.
type AuthChallenge struct {
	// Scheme is the challenge auth scheme (e.g. "Bearer").
	Scheme string
	// ResourceMetadataURL is the RFC 9728 `resource_metadata` pointer, when
	// the challenge carried one.
	ResourceMetadataURL string
	// Realm is the optional `realm` challenge parameter.
	Realm string
	// Error is the optional RFC 6750 §3.1 `error` challenge parameter (e.g.
	// "insufficient_scope", "invalid_token"). Empty when the challenge
	// carried none.
	Error string
	// Scope is the optional RFC 6750 §3.1 `scope` challenge parameter — the
	// space-delimited scopes the downstream requires. Empty when absent.
	Scope string
	// Raw is the verbatim header value (provenance / debugging).
	Raw string
	// CapturedAt is the wall-clock instant the challenge was observed.
	CapturedAt time.Time
}

// ScopeShortfall is a parsed downstream insufficient-scope step-up captured
// off a `403` whose `WWW-Authenticate` carried `error="insufficient_scope"`
// (RFC 6750 §3.1). It is inert, server-supplied data — the connection view's
// LastScopeShortfall record and the per-call error enrichment both read it.
type ScopeShortfall struct {
	// RequiredScopes is the parsed `scope` challenge parameter.
	RequiredScopes []string
	// GrantedScopes is the binding's most-recently-granted scope set
	// (populated by the reader that resolved the token, not the transport).
	GrantedScopes []string
	// DownstreamResource is the host the challenge came from.
	DownstreamResource string
	// Origin is the scheme://host[:port] the challenge was observed on.
	Origin string
	// WWWAuthenticate is the verbatim challenge header value.
	WWWAuthenticate string
	// ToolName is the server-side tool name the call targeted (populated by
	// the reader that owns the tool name, not the transport).
	ToolName string
	// CapturedAt is the wall-clock instant the shortfall was observed.
	CapturedAt time.Time
}

// scopeShortfallSlot is the per-call capture slot threaded on a request's
// ctx: the challenge-capturing transport writes a parsed 403 shortfall here,
// and callTool reads it AFTER the RPC returns to enrich the SAME call's
// error. Per-run state on ctx, never provider-level mutable state — one
// shared transport serves N concurrent calls with no bleed (the
// concurrent-reuse contract). The mutex guards the single write/read.
type scopeShortfallSlot struct {
	mu        sync.Mutex
	shortfall *ScopeShortfall
	granted   []string
}

func (s *scopeShortfallSlot) set(sf ScopeShortfall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	captured := sf
	s.shortfall = &captured
}

func (s *scopeShortfallSlot) setGranted(scopes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.granted = append([]string(nil), scopes...)
}

func (s *scopeShortfallSlot) get() (*ScopeShortfall, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shortfall, append([]string(nil), s.granted...)
}

// scopeShortfallCtxKey is the unexported key the per-call scope-shortfall
// slot is carried under on a request's ctx.
type scopeShortfallCtxKey struct{}

// withScopeShortfallSlot returns a child ctx carrying slot so the
// challenge-capturing transport can write a parsed 403 shortfall for callTool
// to read after the call returns.
func withScopeShortfallSlot(ctx context.Context, slot *scopeShortfallSlot) context.Context {
	return context.WithValue(ctx, scopeShortfallCtxKey{}, slot)
}

// scopeShortfallSlotFrom returns the per-call slot carried on ctx, or nil.
func scopeShortfallSlotFrom(ctx context.Context) *scopeShortfallSlot {
	if ctx == nil {
		return nil
	}
	slot, _ := ctx.Value(scopeShortfallCtxKey{}).(*scopeShortfallSlot)
	return slot
}

// parseWWWAuthenticate parses a `WWW-Authenticate` header value into an
// AuthChallenge, or returns (zero, false) when the value is not a Bearer
// challenge. It reads the `resource_metadata` and `realm` auth-params
// (RFC 7235 / the MCP auth-spec step-up); other params are ignored.
func parseWWWAuthenticate(headerValue string, now time.Time) (AuthChallenge, bool) {
	v := strings.TrimSpace(headerValue)
	if v == "" {
		return AuthChallenge{}, false
	}
	// scheme is the first space-delimited token; the remainder is params.
	scheme := v
	rest := ""
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		scheme = v[:i]
		rest = v[i+1:]
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return AuthChallenge{}, false
	}
	ch := AuthChallenge{Scheme: "Bearer", Raw: v, CapturedAt: now}
	for _, p := range splitAuthParams(rest) {
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch k {
		case "resource_metadata":
			ch.ResourceMetadataURL = val
		case "realm":
			ch.Realm = val
		case "error":
			ch.Error = val
		case "scope":
			ch.Scope = val
		}
	}
	return ch, true
}

// insufficientScopeError is the exact RFC 6750 §3.1 `error` value that marks a
// 403 as a scope shortfall (as opposed to an ordinary authorization-policy
// 403). Only this literal value constructs a structured shortfall.
const insufficientScopeError = "insufficient_scope"

// splitAuthParams splits a challenge parameter list on commas that are not
// inside a quoted string. `a="x,y", b=z` → [`a="x,y"`, ` b=z`].
func splitAuthParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, strings.TrimSpace(b.String()))
	}
	return out
}

// challengeCapturingTransport is the OUTERMOST RoundTripper wrapper on the
// shared MCP HTTP client. It observes every response and, on a `401` carrying
// a `WWW-Authenticate` Bearer challenge, invokes onChallenge with the parsed
// challenge. It holds NO mutable state — one shared transport serves N
// concurrent identities with no bleed (the concurrent-reuse contract). It
// never retries, never mutates the request/response, and always returns the
// underlying result untouched.
type challengeCapturingTransport struct {
	base        http.RoundTripper
	onChallenge func(AuthChallenge)
	// onScopeShortfall, when non-nil, records a parsed `403` +
	// `insufficient_scope` step-up on the connection's registry state
	// (mirroring onChallenge for the 401 path). Best-effort observability —
	// it never alters the call's error semantics.
	onScopeShortfall func(ScopeShortfall)
	clock            func() time.Time
}

// RoundTrip implements http.RoundTripper. It observes the final response and,
// on a `401` Bearer challenge, invokes onChallenge; on a `403` whose
// `WWW-Authenticate` marks `error="insufficient_scope"` (RFC 6750 §3.1), it
// records the parsed shortfall on the per-call ctx slot (for the SAME call's
// error enrichment) AND fires onScopeShortfall (for the connection view). It
// never retries, never mutates the request/response, and always returns the
// underlying result untouched.
func (c *challengeCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	now := time.Now
	if c.clock != nil {
		now = c.clock
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		if hv := resp.Header.Get("WWW-Authenticate"); hv != "" {
			if ch, ok := parseWWWAuthenticate(hv, now()); ok && c.onChallenge != nil {
				c.onChallenge(ch)
			}
		}
	case http.StatusForbidden:
		hv := resp.Header.Get("WWW-Authenticate")
		if hv == "" {
			break
		}
		ch, ok := parseWWWAuthenticate(hv, now())
		// Construct a structured shortfall ONLY when the challenge is
		// explicitly marked insufficient_scope — an unmarked 403 stays an
		// opaque error (no false-positive structured signal).
		if !ok || ch.Error != insufficientScopeError {
			break
		}
		sf := ScopeShortfall{
			RequiredScopes:     splitScopeParam(ch.Scope),
			DownstreamResource: originHost(req),
			Origin:             requestOrigin(req),
			WWWAuthenticate:    ch.Raw,
			CapturedAt:         now(),
		}
		if slot := scopeShortfallSlotFrom(req.Context()); slot != nil {
			slot.set(sf)
		}
		if c.onScopeShortfall != nil {
			c.onScopeShortfall(sf)
		}
	}
	return resp, err
}

// splitScopeParam splits an RFC 6750 space-delimited `scope` value into a
// slice; an empty value yields a nil slice.
func splitScopeParam(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// requestOrigin returns the scheme://host[:port] of the request URL, or ""
// when the URL is absent.
func requestOrigin(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	if req.URL.Scheme == "" || req.URL.Host == "" {
		return ""
	}
	return req.URL.Scheme + "://" + req.URL.Host
}

// originHost returns the host[:port] of the request URL, or "".
func originHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Host
}
