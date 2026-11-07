package audit

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/hurtener/Harbor/internal/config"
)

// URLPlaceholder replaces sensitive URL query values in diagnostics.
const URLPlaceholder = "REDACTED"

// RedactURL returns a diagnostic copy of a URL or request target. It preserves
// query ordering, repeated parameters, and non-sensitive values. An invalid
// query is replaced in full rather than risking a partial parse leaking secrets.
// The input is never used to construct an outgoing request.
func RedactURL(raw string) string {
	start := strings.IndexByte(raw, '?')
	if start < 0 {
		return raw
	}
	query, fragment, _ := strings.Cut(raw[start+1:], "#")
	if _, err := url.ParseQuery(query); err != nil {
		return raw[:start+1] + URLPlaceholder
	}
	parts := strings.Split(query, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			return raw[:start+1] + URLPlaceholder
		}
		if sensitiveQueryKey(decoded) {
			parts[i] = key + "=" + URLPlaceholder
		}
	}
	result := raw[:start+1] + strings.Join(parts, "&")
	if strings.Contains(raw[start+1:], "#") {
		result += "#" + fragment
	}
	return result
}

func sensitiveQueryKey(key string) bool {
	if config.IsReceiverInjectionCredentialKey(key) {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(key))
	segments := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '[' || r == ']'
	})
	if len(segments) == 0 {
		return false
	}
	switch segments[len(segments)-1] {
	case "code", "sig", "signature", "key", "token", "secret", "credential", "credentials", "auth", "authorization", "assertion", "ticket", "password", "passwd":
		return true
	}
	// Common camel-case names have no separator to expose their credential leaf.
	for _, suffix := range []string{"token", "apikey", "accesskey", "privatekey", "clientsecret", "signature", "credential"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// Quotes are valid query content, so they cannot terminate a URL match. Also
// consume quoted values containing whitespace (or an unmatched quote through
// the end of the line) rather than exposing the rest of a malformed credential.
var diagnosticURLPattern = regexp.MustCompile(`(?m)(?:[A-Za-z][A-Za-z0-9+.-]*://|/|\?)(?:='[^'\r\n]*(?:'|$)|="[^"\r\n]*(?:"|$)|[^\s])+`)

// RedactURLText sanitizes URLs embedded in messages and error strings, including
// relative request targets. It leaves the surrounding diagnostic text intact.
func RedactURLText(text string) string {
	if !strings.ContainsRune(text, '?') {
		return text
	}
	return diagnosticURLPattern.ReplaceAllStringFunc(text, func(candidate string) string {
		// Keep surrounding diagnostic punctuation such as the closing `":` in
		// net/http errors. Removing punctuation before redaction cannot expose
		// credential bytes; it only preserves these literal delimiters.
		body := strings.TrimRight(candidate, "\"':,)")
		return RedactURL(body) + candidate[len(body):]
	})
}

// RedactURLValue sanitizes URL text in structured diagnostic values. Values with
// no URL changes retain their original type; changed containers use the audit
// walker's copied map/slice representation. Unsupported depth fails closed.
func RedactURLValue(payload any) (any, error) {
	changed := false
	clean, err := walkReplaceStrings(payload, func(text string) string {
		redacted := RedactURLText(text)
		changed = changed || redacted != text
		return redacted
	}, 0)
	if err != nil {
		return nil, err
	}
	if !changed {
		return payload, nil
	}
	return clean, nil
}

type urlQueryRule struct{}

func (*urlQueryRule) Name() string { return "url_query" }

func (*urlQueryRule) Apply(_ context.Context, payload any) (any, error) {
	return walkReplaceStrings(payload, RedactURLText, 0)
}
