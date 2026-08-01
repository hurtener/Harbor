package config

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeMCPHTTPURL validates and canonicalizes an MCP HTTP transport URL.
// Query parameters are retained because they can be part of a legitimate
// endpoint identity. Userinfo and fragments are refused: credentials belong in
// Harbor's explicit credential bindings, and fragments are never sent on HTTP.
func NormalizeMCPHTTPURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("url must be absolute and include a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not include userinfo; use an explicit credential binding")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("url must not include a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u.String(), nil
}
