package a2a

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// validatePeerURL returns the parsed URL when peerURL is acceptable for
// the A2A wire driver, an error otherwise.
//
// AGENTS.md §7 (security rule 1-equivalent for outbound calls):
//   - HTTPS is required by default.
//   - HTTP is allowed when the host is loopback (127.0.0.1, ::1,
//     "localhost") OR when allowInsecureLoopback is true. The flag is
//     INTENTIONALLY name-checked against loopback only — operators can
//     not flip a flag to talk to an arbitrary HTTP peer; they would
//     have to use a real HTTPS URL.
//
// The error wraps ErrInvalidPeerURL or ErrInsecureScheme so callers can
// errors.Is on either.
func validatePeerURL(peerURL string, allowInsecureLoopback bool) (*url.URL, error) {
	if peerURL == "" {
		return nil, fmt.Errorf("%w: empty URL", ErrInvalidPeerURL)
	}
	u, err := url.Parse(peerURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrInvalidPeerURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%w: missing scheme/host: %q", ErrInvalidPeerURL, peerURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		// Always accepted.
		return u, nil
	case "http":
		if isLoopbackHost(u.Host) {
			return u, nil
		}
		if allowInsecureLoopback {
			// Flag is reserved for loopback-shaped peers (e.g. test
			// servers binding 0.0.0.0 on a docker bridge); a non-
			// loopback HTTP host is still rejected so an operator
			// cannot accidentally allow plaintext to a public peer.
			return nil, fmt.Errorf("%w: AllowInsecureLoopback set but host %q is not loopback", ErrInsecureScheme, u.Host)
		}
		return nil, fmt.Errorf("%w: HTTP scheme requires loopback host (got %q) or AllowInsecureLoopback flag", ErrInsecureScheme, u.Host)
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrInsecureScheme, u.Scheme)
	}
}

// isLoopbackHost reports whether host (with optional :port) refers to
// a loopback address. Recognises "localhost", "127.0.0.1", "::1",
// "[::1]", and any 127.0.0.0/8 address.
func isLoopbackHost(host string) bool {
	// Strip port if present.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
