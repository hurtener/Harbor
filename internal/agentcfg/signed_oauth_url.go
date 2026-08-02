package agentcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// ErrInvalidOAuthMCPURL marks a URL that cannot be used as a signed OAuth MCP
// capability endpoint. The registration surface accepts only this canonical
// representation, so callers never sign one spelling and dial another.
var ErrInvalidOAuthMCPURL = errors.New("agentcfg: invalid signed oauth mcp url")

// CanonicalOAuthMCPURL returns the one byte representation used for signed
// OAuth MCP capability matching, pair fingerprints, and downstream bearer
// sink enforcement. It deliberately does not use url.URL.String: its escaping
// choices are not the signed-capability wire contract.
//
// The result is absolute HTTPS with an explicit canonical port; sink is the
// corresponding HTTPS origin. Query order, duplicate keys, and a terminal
// empty query are preserved because they can be part of an MCP endpoint's
// routing semantics.
func CanonicalOAuthMCPURL(raw string) (canonicalURL, sink string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: parse: %w", ErrInvalidOAuthMCPURL, err)
	}
	if u.Scheme != "https" || u.Opaque != "" || u.Host == "" {
		return "", "", fmt.Errorf("%w: absolute https URL required", ErrInvalidOAuthMCPURL)
	}
	if u.User != nil || u.Fragment != "" {
		return "", "", fmt.Errorf("%w: userinfo and fragment are forbidden", ErrInvalidOAuthMCPURL)
	}

	host, err := canonicalOAuthMCPHost(u.Hostname())
	if err != nil {
		return "", "", err
	}
	port, err := canonicalOAuthMCPPort(u.Port())
	if err != nil {
		return "", "", err
	}
	hostPort := net.JoinHostPort(host, port)

	path, err := canonicalOAuthMCPPath(u.EscapedPath())
	if err != nil {
		return "", "", err
	}
	query, err := canonicalOAuthMCPEscapes(u.RawQuery)
	if err != nil {
		return "", "", err
	}

	sink = "https://" + hostPort
	canonicalURL = sink + path
	if u.ForceQuery || u.RawQuery != "" {
		canonicalURL += "?" + query
	}
	return canonicalURL, sink, nil
}

// OAuthMCPURLDigest returns the fixed SHA-256 digest used in signed authority
// claims. It accepts only the canonical URL returned by CanonicalOAuthMCPURL;
// callers must not digest unnormalised request text.
func OAuthMCPURLDigest(canonicalURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL))
	return hex.EncodeToString(sum[:])
}

func canonicalOAuthMCPHost(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if host == "" || strings.Contains(host, "%") {
		return "", fmt.Errorf("%w: host is empty or contains an IP zone", ErrInvalidOAuthMCPURL)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return "", fmt.Errorf("%w: host is not valid IDNA2008", ErrInvalidOAuthMCPURL)
	}
	return strings.ToLower(ascii), nil
}

func canonicalOAuthMCPPort(raw string) (string, error) {
	if raw == "" {
		return "443", nil
	}
	if len(raw) > 1 && raw[0] == '0' {
		return "", fmt.Errorf("%w: explicit port has a leading zero", ErrInvalidOAuthMCPURL)
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return "", fmt.Errorf("%w: invalid port", ErrInvalidOAuthMCPURL)
	}
	return strconv.FormatUint(port, 10), nil
}

func canonicalOAuthMCPPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	normalized, err := canonicalOAuthMCPPercent(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("%w: path must be absolute", ErrInvalidOAuthMCPURL)
	}
	return removeOAuthMCPDotSegments(normalized), nil
}

func canonicalOAuthMCPEscapes(raw string) (string, error) {
	return canonicalOAuthMCPPercent(raw)
}

func canonicalOAuthMCPPercent(raw string) (string, error) {
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			out.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", fmt.Errorf("%w: incomplete percent escape", ErrInvalidOAuthMCPURL)
		}
		v, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("%w: invalid percent escape", ErrInvalidOAuthMCPURL)
		}
		b := byte(v)
		if oauthMCPUnreserved(b) {
			out.WriteByte(b)
		} else {
			const hex = "0123456789ABCDEF"
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&0x0f])
		}
		i += 2
	}
	return out.String(), nil
}

func oauthMCPUnreserved(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~'
}

func removeOAuthMCPDotSegments(path string) string {
	segments := strings.Split(path, "/")
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case ".":
			continue
		case "..":
			if len(out) > 1 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, segment)
		}
	}
	result := strings.Join(out, "/")
	if result == "" {
		return "/"
	}
	if !strings.HasPrefix(result, "/") {
		return "/" + result
	}
	return result
}
