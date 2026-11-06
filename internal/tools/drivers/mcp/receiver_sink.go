package mcp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrReceiverSinkRefused marks an outbound receiver request whose actual URL
// does not satisfy the connection's credential destination policy.
var ErrReceiverSinkRefused = errors.New("mcp: receiver credential destination refused")

// receiverSinkPolicy is immutable after construction. It preserves explicit
// ports and the URL scheme instead of using the legacy host-only normalizer,
// which intentionally treats :80 and :443 alike for older config matching.
type receiverSinkPolicy struct {
	endpoint *url.URL
	allowed  []string
	signed   bool
}

// newReceiverSinkPolicy is shared by construction-time admission and every
// actual HTTP attempt, so neither path can weaken scheme or exact-port checks.
func newReceiverSinkPolicy(cfg Config) receiverSinkPolicy {
	endpoint, _ := url.Parse(cfg.URL)
	return receiverSinkPolicy{endpoint: endpoint, allowed: append([]string(nil), cfg.Injection.Provider.AllowedDownstreamHosts()...), signed: cfg.OwnedInjectionProvider != nil}
}

func receiverAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func receiverLoopback(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p receiverSinkPolicy) validate(u *url.URL) error {
	if u == nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return fmt.Errorf("%w: malformed destination", ErrReceiverSinkRefused)
	}
	// HTTP is retained only for an originally configured loopback HTTP endpoint.
	// Even loopback cannot turn an HTTPS connection into a plaintext one.
	if u.Scheme != "https" && !(u.Scheme == "http" && p.endpoint != nil && p.endpoint.Scheme == "http" && receiverLoopback(p.endpoint) && receiverLoopback(u)) {
		return fmt.Errorf("%w: destination requires HTTPS", ErrReceiverSinkRefused)
	}
	if p.signed && (p.endpoint == nil || u.Scheme != p.endpoint.Scheme || receiverAuthority(u) != receiverAuthority(p.endpoint)) {
		return fmt.Errorf("%w: destination differs from signed connection", ErrReceiverSinkRefused)
	}
	for _, raw := range p.allowed {
		candidate := strings.TrimSpace(raw)
		if !strings.Contains(candidate, "://") {
			scheme := "https"
			if p.endpoint != nil && p.endpoint.Scheme == "http" && receiverLoopback(p.endpoint) {
				scheme = "http"
			}
			candidate = scheme + "://" + candidate
		}
		allowed, err := url.Parse(candidate)
		if err == nil && allowed.User == nil && allowed.Scheme == u.Scheme && receiverAuthority(allowed) == receiverAuthority(u) {
			return nil
		}
	}
	return fmt.Errorf("%w: destination is outside provider allowed_downstream_hosts", ErrReceiverSinkRefused)
}

type receiverSinkTransport struct {
	base   http.RoundTripper
	policy receiverSinkPolicy
}

func (t *receiverSinkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.policy.validate(req.URL); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(req)
}
