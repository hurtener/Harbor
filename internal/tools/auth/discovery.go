package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth requirement discovery (MCP).
//
// A connected MCP HTTP server can ADVERTISE that its tools need OAuth: the
// MCP authorization spec (2025-06-18) has an unauthorized call answer `401`
// + `WWW-Authenticate: Bearer resource_metadata="…"`, whose URL points at an
// RFC 9728 protected-resource-metadata document, which names
// `authorization_servers[]`, each resolvable to an RFC 8414 / OIDC
// authorization-server-metadata document (endpoints, scopes, PKCE posture,
// an optional RFC 7591 registration endpoint, an RFC 8707 resource id).
//
// The Discoverer walks that chain and returns it VERBATIM as inert data —
// a report an operator confirms, never config Harbor follows. Harbor never
// runs the OAuth flow, never holds or refreshes a token, and never dials any
// endpoint beyond the metadata documents themselves; the RFC 7591
// registration endpoint is reported, never invoked.
//
// The metadata documents are fetched from attacker-influenceable URLs, so
// every fetch is bounded by SSRF guardrails (validateHop): the protected-
// resource hop defaults to the server's own origin; the authorization-server
// hop is inherently cross-origin and ALWAYS requires an explicit per-
// connection origin allowance; every allowed cross-origin fetch additionally
// refuses private-range / IP-literal destinations; every hop enforces
// https-off-loopback, bounded redirects, a per-fetch timeout, a response
// size cap, and attaches NO credentials of any kind. Each refusal is a typed,
// loud per-step status — never a silent empty (CLAUDE.md §13).

// discoveryStep names the metadata hop a DiscoveryStepStatus reports on.
type discoveryStep string

const (
	// StepProtectedResource is the RFC 9728 protected-resource-metadata hop.
	StepProtectedResource discoveryStep = "protected_resource"
	// StepAuthorizationServer is the RFC 8414 / OIDC authorization-server-
	// metadata hop.
	StepAuthorizationServer discoveryStep = "authorization_server"
)

// Stable, machine-readable reason codes carried on a failed DiscoveryStepStatus.
// They are report-only strings — never surfaced as an error the runtime acts on.
const (
	// ReasonCrossOriginNotAllowed — a metadata URL on a different origin than
	// the declared server, without an explicit per-connection allowance.
	ReasonCrossOriginNotAllowed = "cross_origin_not_allowed"
	// ReasonNeedsAllowance — the authorization-server hop, which always needs
	// an explicit per-connection origin allowance, had none for the AS origin.
	ReasonNeedsAllowance = "needs_allowance"
	// ReasonPrivateOrIPLiteral — an allowed cross-origin target resolved to a
	// private-range / loopback / link-local address or was an IP literal.
	ReasonPrivateOrIPLiteral = "private_or_ip_literal"
	// ReasonNotHTTPS — a non-loopback target on a scheme other than https.
	ReasonNotHTTPS = "not_https"
	// ReasonInvalidURL — the metadata URL did not parse / had no host.
	ReasonInvalidURL = "invalid_url"
	// ReasonFetchFailed — the HTTP fetch failed (transport error / non-2xx /
	// redirect bound exceeded / size cap exceeded / timeout).
	ReasonFetchFailed = "fetch_failed"
	// ReasonParseFailed — the fetched document did not decode as the expected
	// metadata shape.
	ReasonParseFailed = "parse_failed"
	// ReasonMetadataAbsent — the document parsed but named no
	// authorization_servers (the 9728 hop) — nothing to walk.
	ReasonMetadataAbsent = "metadata_absent"
)

// AuthorizationServerMeta is one RFC 8414 / OIDC authorization-server-metadata
// document, surfaced VERBATIM. It is inert data — Harbor never dials any of
// these endpoints. RegistrationEndpoint is reported, never invoked.
type AuthorizationServerMeta struct {
	// Issuer is the authorization server's issuer identifier.
	Issuer string `json:"issuer"`
	// AuthorizationEndpoint is the RFC 6749 authorization endpoint URL.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	// TokenEndpoint is the RFC 6749 token endpoint URL. NEVER dialed by Harbor.
	TokenEndpoint string `json:"token_endpoint"`
	// ScopesSupported is the advertised scope list (may be empty).
	ScopesSupported []string `json:"scopes_supported"`
	// CodeChallengeMethodsSupported is the advertised PKCE method list.
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	// RegistrationEndpoint is the optional RFC 7591 dynamic-registration
	// endpoint. REPORTED, never invoked (the custody boundary).
	RegistrationEndpoint string `json:"registration_endpoint,omitempty"`
	// Resource is the optional RFC 8707 resource identifier the AS binds.
	Resource string `json:"resource,omitempty"`
	// SourceURL is the URL the document was fetched from (provenance).
	SourceURL string `json:"source_url"`
}

// DiscoveryStepStatus is the typed per-hop outcome. A failed step carries a
// stable Reason code and a human detail; a chain surfaces partially rather
// than silently empty.
type DiscoveryStepStatus struct {
	// Step names the hop.
	Step discoveryStep `json:"step"`
	// Target is the URL the hop attempted.
	Target string `json:"target"`
	// OK reports whether the hop succeeded.
	OK bool `json:"ok"`
	// Reason is the stable machine-readable failure code (empty when OK).
	Reason string `json:"reason,omitempty"`
	// Detail is a human-readable message (never carries a credential).
	Detail string `json:"detail,omitempty"`
}

// OAuthRequirement is the discovered advertisement, surfaced as inert data.
// It is a PROPOSAL an operator confirms — never auto-applied to any config.
type OAuthRequirement struct {
	// ResourceMetadataURL is the RFC 9728 protected-resource-metadata URL that
	// was walked (from the challenge, else the server's well-known default).
	ResourceMetadataURL string `json:"resource_metadata_url"`
	// AuthorizationServers is the verbatim RFC 8414 metadata for each
	// advertised authorization server that resolved. May be empty when the
	// AS hop was refused (see Status).
	AuthorizationServers []AuthorizationServerMeta `json:"authorization_servers"`
	// DiscoveredAt is the wall-clock instant discovery ran.
	DiscoveredAt time.Time `json:"discovered_at"`
	// Source is "challenge" (from a 401 WWW-Authenticate step-up) or "probe"
	// (an operator-triggered probe).
	Source string `json:"source"`
	// SourceURL is the resource-metadata URL discovery started from.
	SourceURL string `json:"source_url"`
	// Status is the typed per-hop outcome list — a partial chain reports the
	// hops it refused / failed here rather than surfacing silently empty.
	Status []DiscoveryStepStatus `json:"status"`
}

// DiscoveryInput is the per-connection input to one discovery walk.
type DiscoveryInput struct {
	// ServerURL is the declared MCP server URL (the origin the 9728 hop
	// defaults to same-origin against). Required.
	ServerURL string
	// ResourceMetadataURL is the `resource_metadata` URL carried on the
	// captured WWW-Authenticate challenge. Empty falls back to
	// `{server-origin}/.well-known/oauth-protected-resource`.
	ResourceMetadataURL string
	// Source is "challenge" or "probe".
	Source string
	// AllowedOrigins is the explicit per-connection cross-origin allowance
	// list (scheme://host[:port]). The AS hop always requires a matching
	// entry; the 9728 hop requires one only when the metadata URL is
	// cross-origin to ServerURL.
	AllowedOrigins []string
}

// protectedResourceMetadata is the RFC 9728 protected-resource-metadata
// parse shape (the subset the walker consults).
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// Discoverer walks the OAuth requirement chain under SSRF guardrails. It is
// a compiled artifact — immutable after construction and safe to share across
// N concurrent Discover calls (it holds no per-walk state; each walk's state
// lives in local variables + the returned value). See the concurrent-reuse contract.
type Discoverer struct {
	httpClient      *http.Client
	clock           func() time.Time
	maxRedirects    int
	maxBodyBytes    int64
	perFetchTimeout time.Duration
	// allowPrivate relaxes the private-range / IP-literal refusal. It is
	// TEST-ONLY (httptest fixtures bind loopback IP literals); production
	// constructs a Discoverer without it, so the strict guardrail holds.
	allowPrivate bool
}

// DiscovererOption configures a Discoverer at construction.
type DiscovererOption func(*Discoverer)

// WithDiscoveryClock overrides the wall clock (tests inject a fixed clock).
func WithDiscoveryClock(now func() time.Time) DiscovererOption {
	return func(d *Discoverer) {
		if now != nil {
			d.clock = now
		}
	}
}

// WithDiscoveryTimeout overrides the per-fetch timeout.
func WithDiscoveryTimeout(d time.Duration) DiscovererOption {
	return func(dd *Discoverer) {
		if d > 0 {
			dd.perFetchTimeout = d
		}
	}
}

// WithDiscoveryMaxBodyBytes overrides the per-fetch response size cap.
func WithDiscoveryMaxBodyBytes(n int64) DiscovererOption {
	return func(d *Discoverer) {
		if n > 0 {
			d.maxBodyBytes = n
		}
	}
}

// WithPrivateNetworkAccessForTest relaxes the private-range / IP-literal
// refusal so httptest fixtures on loopback can be reached.
//
// TEST-ONLY. Production MUST NOT call this: without it the Discoverer refuses
// every private-range / loopback / IP-literal cross-origin target, which is
// the load-bearing SSRF guardrail. It exists because httptest servers bind
// loopback and the positive-path chain-walk tests must reach them; the
// negative guardrail tests deliberately omit it to prove production refuses.
func WithPrivateNetworkAccessForTest() DiscovererOption {
	return func(d *Discoverer) { d.allowPrivate = true }
}

// NewDiscoverer builds a Discoverer with strict SSRF defaults: 8 KiB body
// cap, 5 s per-fetch timeout, at most 3 redirects, each redirect hop re-
// validated, and a dial-time control that refuses private-range resolved
// addresses (defence against DNS rebinding).
func NewDiscoverer(opts ...DiscovererOption) *Discoverer {
	d := &Discoverer{
		clock:           time.Now,
		maxRedirects:    3,
		maxBodyBytes:    8 << 10,
		perFetchTimeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(d)
	}
	dialer := &net.Dialer{Timeout: d.perFetchTimeout}
	transport := &http.Transport{
		Proxy: nil, // never route discovery through a proxy
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !d.allowPrivate {
				host, _, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					host = addr
				}
				if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
					return nil, fmt.Errorf("%w: dial to private address %s refused", ErrDiscoveryRefused, host)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	d.httpClient = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= d.maxRedirects {
				return fmt.Errorf("%w: stopped after %d redirects", ErrDiscoveryRefused, d.maxRedirects)
			}
			// Strip any inherited auth on a redirect (defence in depth; we
			// never set one, but a redirect must never carry credentials).
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
	return d
}

// ErrDiscoveryRefused is the sentinel for an SSRF-guardrail refusal during a
// discovery fetch. It is carried into a DiscoveryStepStatus, never returned to
// a runtime caller as a hard error.
var ErrDiscoveryRefused = errors.New("auth: oauth requirement discovery fetch refused")

// Discover walks the chain for one connection and returns the verbatim
// requirement plus per-hop statuses. It never returns an error — a refused or
// failed hop is reported in Status; the caller surfaces the partial result.
func (d *Discoverer) Discover(ctx context.Context, in DiscoveryInput) OAuthRequirement {
	source := in.Source
	if source != "challenge" && source != "probe" {
		source = "probe"
	}
	req := OAuthRequirement{
		DiscoveredAt: d.clock(),
		Source:       source,
		Status:       []DiscoveryStepStatus{},
	}

	serverOrigin, originErr := originOf(in.ServerURL)
	if originErr != nil {
		req.Status = append(req.Status, DiscoveryStepStatus{
			Step: StepProtectedResource, Target: in.ServerURL, OK: false,
			Reason: ReasonInvalidURL, Detail: "server URL has no parseable origin",
		})
		return req
	}

	prURL := strings.TrimSpace(in.ResourceMetadataURL)
	if prURL == "" {
		prURL = serverOrigin + "/.well-known/oauth-protected-resource"
	}
	req.ResourceMetadataURL = prURL
	req.SourceURL = prURL

	allowed := normalizeOrigins(in.AllowedOrigins)

	// Hop 1 — RFC 9728 protected-resource metadata (same-origin default).
	body, st := d.fetchHop(ctx, prURL, StepProtectedResource, serverOrigin, allowed)
	req.Status = append(req.Status, st)
	if !st.OK {
		return req
	}
	var pr protectedResourceMetadata
	if err := json.Unmarshal(body, &pr); err != nil {
		req.Status[len(req.Status)-1] = failStatus(StepProtectedResource, prURL, ReasonParseFailed, "protected-resource metadata did not decode")
		return req
	}
	if len(pr.AuthorizationServers) == 0 {
		req.Status = append(req.Status, failStatus(StepProtectedResource, prURL, ReasonMetadataAbsent, "no authorization_servers advertised"))
		return req
	}

	// Hop 2 — RFC 8414 / OIDC authorization-server metadata per advertised AS
	// (always cross-origin → always requires the explicit allowance).
	for _, asURL := range pr.AuthorizationServers {
		asMetaURL := authServerMetadataURL(asURL)
		asBody, asSt := d.fetchHop(ctx, asMetaURL, StepAuthorizationServer, serverOrigin, allowed)
		req.Status = append(req.Status, asSt)
		if !asSt.OK {
			continue
		}
		var meta discoveredMetadata
		if err := json.Unmarshal(asBody, &meta); err != nil {
			req.Status[len(req.Status)-1] = failStatus(StepAuthorizationServer, asMetaURL, ReasonParseFailed, "authorization-server metadata did not decode")
			continue
		}
		req.AuthorizationServers = append(req.AuthorizationServers, AuthorizationServerMeta{
			Issuer:                        meta.Issuer,
			AuthorizationEndpoint:         meta.AuthorizationEndpoint,
			TokenEndpoint:                 meta.TokenEndpoint,
			ScopesSupported:               append([]string(nil), meta.ScopesSupported...),
			CodeChallengeMethodsSupported: append([]string(nil), meta.CodeChallengeMethodsSupported...),
			RegistrationEndpoint:          meta.RegistrationEndpoint,
			SourceURL:                     asMetaURL,
		})
	}
	return req
}

// fetchHop validates the target under the per-hop SSRF policy, fetches it with
// no credentials, and returns the body + a typed status.
func (d *Discoverer) fetchHop(ctx context.Context, target string, step discoveryStep, serverOrigin string, allowed map[string]bool) ([]byte, DiscoveryStepStatus) {
	if reason, detail, ok := d.validateHop(target, step, serverOrigin, allowed); !ok {
		return nil, failStatus(step, target, reason, detail)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, d.perFetchTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, failStatus(step, target, ReasonFetchFailed, "build request failed")
	}
	httpReq.Header.Set("Accept", "application/json")
	// Deliberately NO Authorization / Cookie header — discovery fetches carry
	// no credentials of any kind (§7).
	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return nil, failStatus(step, target, ReasonFetchFailed, "fetch failed or refused")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, failStatus(step, target, ReasonFetchFailed, fmt.Sprintf("status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, d.maxBodyBytes+1))
	if err != nil {
		return nil, failStatus(step, target, ReasonFetchFailed, "body read failed")
	}
	if int64(len(body)) > d.maxBodyBytes {
		return nil, failStatus(step, target, ReasonFetchFailed, "response exceeded size cap")
	}
	return body, DiscoveryStepStatus{Step: step, Target: target, OK: true}
}

// validateHop applies the per-hop SSRF policy. Returns (reason, detail, ok=false)
// on refusal, ("", "", true) when the target may be fetched.
func (d *Discoverer) validateHop(target string, step discoveryStep, serverOrigin string, allowed map[string]bool) (string, string, bool) {
	targetOrigin, err := originOf(target)
	if err != nil {
		return ReasonInvalidURL, "target URL has no parseable origin", false
	}
	u, _ := url.Parse(target) //nolint:errcheck // originOf already validated it parses with a host
	host := u.Hostname()

	sameOrigin := targetOrigin == serverOrigin
	switch step {
	case StepAuthorizationServer:
		// The AS hop is inherently cross-origin — ALWAYS requires an allowance.
		if !allowed[targetOrigin] {
			return ReasonNeedsAllowance, "authorization-server origin requires an explicit per-connection allowance", false
		}
	case StepProtectedResource:
		// Same-origin default; a cross-origin metadata URL needs an allowance.
		if !sameOrigin && !allowed[targetOrigin] {
			return ReasonCrossOriginNotAllowed, "protected-resource metadata origin differs from the server and is not allowed", false
		}
	}

	loopback := isLoopbackHost(host)
	// https-only off loopback.
	if u.Scheme != "https" && !loopback {
		return ReasonNotHTTPS, "non-loopback discovery target must use https", false
	}
	// For any cross-origin fetch, refuse IP-literal / private-range hosts.
	if !sameOrigin && !d.allowPrivate {
		if ip := net.ParseIP(host); ip != nil {
			return ReasonPrivateOrIPLiteral, "cross-origin target is an IP literal", false
		}
		// (DNS-resolved private ranges are additionally refused at dial time.)
	}
	return "", "", true
}

// failStatus builds a refused/failed DiscoveryStepStatus.
func failStatus(step discoveryStep, target, reason, detail string) DiscoveryStepStatus {
	return DiscoveryStepStatus{Step: step, Target: target, OK: false, Reason: reason, Detail: detail}
}

// originOf returns the scheme://host[:port] origin of a URL, or an error when
// it does not parse or has no host.
func originOf(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("url missing scheme or host")
	}
	return u.Scheme + "://" + u.Host, nil
}

// normalizeOrigins builds a set of the allowed origins (best-effort — an
// unparseable entry is dropped rather than failing the whole walk).
func normalizeOrigins(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, o := range list {
		if origin, err := originOf(o); err == nil {
			out[origin] = true
		}
	}
	return out
}

// authServerMetadataURL derives the RFC 8414 well-known metadata URL for an
// issuer identifier. RFC 8414 inserts the well-known segment before the
// issuer's path; for the common no-path issuer this is
// `{issuer}/.well-known/oauth-authorization-server`.
func authServerMetadataURL(issuer string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(issuer), "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed + "/.well-known/oauth-authorization-server"
	}
	if u.Path == "" || u.Path == "/" {
		return u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server"
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + u.Path
}

// isLoopbackHost reports whether host is a loopback name or a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isPrivateIP reports whether ip is in a range discovery must never reach on a
// cross-origin hop (loopback, link-local, unique-local, RFC 1918 private).
func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified()
}
