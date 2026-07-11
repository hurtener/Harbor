package mcp

import (
	"net/http"
	"strings"
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
	// Raw is the verbatim header value (provenance / debugging).
	Raw string
	// CapturedAt is the wall-clock instant the challenge was observed.
	CapturedAt time.Time
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
		}
	}
	return ch, true
}

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
	clock       func() time.Time
}

// RoundTrip implements http.RoundTripper.
func (c *challengeCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if hv := resp.Header.Get("WWW-Authenticate"); hv != "" {
			now := time.Now
			if c.clock != nil {
				now = c.clock
			}
			if ch, ok := parseWWWAuthenticate(hv, now()); ok && c.onChallenge != nil {
				c.onChallenge(ch)
			}
		}
	}
	return resp, err
}
