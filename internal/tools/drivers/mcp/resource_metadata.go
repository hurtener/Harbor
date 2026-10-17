package mcp

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrResourceMetadata refuses malformed or unsupported recognized App resource
// metadata. Its message deliberately contains no provider payload or endpoint.
var ErrResourceMetadata = errors.New("mcp: invalid App resource metadata")

// ResourceRead is the request-local result of a single resource read. Optional
// metadata is detached from the provider response; it grants no callback rights.
type ResourceRead struct {
	Content         []byte
	MIMEType        string
	ResourceDomains []string
	ProviderOrigin  string
	entry           *serverEntry
	generation      string
	source          string
}

// PublicHTTPSOrigin returns only a canonical public HTTPS origin. This is a
// syntactic projection, not a DNS lookup or a network-authorization decision.
// Trusted endpoint paths/queries are discarded; credentials and nonpublic hosts
// are never disclosed. CSP declarations additionally require exact origins.
func PublicHTTPSOrigin(raw string) string {
	if len(raw) > 2048 || !utf8.ValidString(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if len(host) > 253 || strings.HasSuffix(host, ".") || !strings.Contains(host, ".") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		// IP literals are intentionally omitted, including public literals: this
		// surface is for registered public DNS providers and passive asset origins.
		return ""
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".invalid", ".test", ".example", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return ""
		}
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	if last[0] >= '0' && last[0] <= '9' {
		return ""
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return ""
			}
		}
	}
	if port := u.Port(); port != "" {
		// Canonicalize numeric ports too: browsers treat :0443 as :443.
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return ""
		}
		if n != 443 {
			host = net.JoinHostPort(host, strconv.FormatUint(n, 10))
		}
	}
	return "https://" + host
}

func resourceDomains(meta map[string]any) ([]string, error) {
	ui, exists := meta["ui"]
	if !exists {
		return nil, nil
	}
	uiObject, ok := ui.(map[string]any)
	if !ok {
		return nil, ErrResourceMetadata
	}
	csp, exists := uiObject["csp"]
	if !exists {
		return nil, nil
	}
	cspObject, ok := csp.(map[string]any)
	if !ok {
		return nil, ErrResourceMetadata
	}
	raw, exists := cspObject["resourceDomains"]
	if !exists {
		return nil, nil
	}
	var values []string
	switch v := raw.(type) {
	case []string:
		values = v
	case []any:
		if len(v) > 32 {
			return nil, ErrResourceMetadata
		}
		values = make([]string, len(v))
		for i, value := range v {
			var ok bool
			values[i], ok = value.(string)
			if !ok {
				return nil, ErrResourceMetadata
			}
		}
	default:
		return nil, ErrResourceMetadata
	}
	if len(values) > 32 {
		return nil, ErrResourceMetadata
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	size := len(`{"ui":{"csp":{"resourceDomains":[]}}}`)
	for _, value := range values {
		origin := PublicHTTPSOrigin(value)
		if origin == "" || origin != value {
			return nil, ErrResourceMetadata
		}
		b, _ := json.Marshal(value)
		size += len(b) + 1
		if size > 8192 {
			return nil, ErrResourceMetadata
		}
		if !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	return out, nil
}
