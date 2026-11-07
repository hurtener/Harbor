package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

func TestRedactURL_QueryCredentials(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"plain", "/v1/events?type=task.completed&session=s1", "/v1/events?type=task.completed&session=s1"},
		{"repeated", "/v1/events?access_token=first&type=a&type=b&access_token=second", "/v1/events?access_token=REDACTED&type=a&type=b&access_token=REDACTED"},
		{"encoded key", "/v1/events?%61ccess%5Ftoken=secret&resume_seq=42", "/v1/events?%61ccess%5Ftoken=REDACTED&resume_seq=42"},
		{"generic", "https://host/path?token=a&id_token=b&code=c&key=d&signature=e&sig=f", "https://host/path?token=REDACTED&id_token=REDACTED&code=REDACTED&key=REDACTED&signature=REDACTED&sig=REDACTED"},
		{"aliases", "/p?API-KEY=a&accessToken=b&clientSecret=c&X-Amz-Signature=d&credentials=e", "/p?API-KEY=REDACTED&accessToken=REDACTED&clientSecret=REDACTED&X-Amz-Signature=REDACTED&credentials=REDACTED"},
		{"malformed value", "/p?type=a&access_token=secret%ZZ&other=remaining", "/p?REDACTED"},
		{"malformed key", "/p?%ZZtoken=secret&type=a", "/p?REDACTED"},
		{"semicolon", "/p?type=a;access_token=secret", "/p?REDACTED"},
		{"metadata", "/p?token_type=bearer&token_url=https%3A%2F%2Fhost&access_token_count=3", "/p?token_type=bearer&token_url=https%3A%2F%2Fhost&access_token_count=3"},
		{"empty", "/p?token=&code", "/p?token=REDACTED&code=REDACTED"},
		{"no query", "/p", "/p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := audit.RedactURL(tc.input); got != tc.want {
				t.Fatalf("RedactURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestURLQueryRule_RedactsEmbeddedURLs(t *testing.T) {
	input := map[string]any{
		"path":   "/v1/events?access_token=url-query-sentinel&type=task.completed&session=s1&resume_seq=42",
		"error":  `Get "https://host/p?%69d_token=error-query-sentinel": connection refused`,
		"nested": []any{"/p?code=nested-query-sentinel&sig=second-query-sentinel"},
	}
	got, err := patterns.New().Redact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	out := flatten(t, got)
	for _, secret := range []string{"url-query-sentinel", "error-query-sentinel", "nested-query-sentinel", "second-query-sentinel"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked: %s", secret, out)
		}
	}
	for _, field := range []string{"REDACTED", "task.completed", "session=s1", "resume_seq=42", "connection refused"} {
		if !strings.Contains(out, field) {
			t.Errorf("diagnostic %q missing: %s", field, out)
		}
	}
	if !strings.Contains(input["path"].(string), "url-query-sentinel") {
		t.Fatal("redactor mutated the request URL")
	}
}

func TestRedactURLText_QuotedQueryValues(t *testing.T) {
	cases := []string{
		`Get "https://host/p?token='quoted-url-sentinel'": failed`,
		`Get "https://host/p?token=prefix'quoted-url-sentinel": failed`,
		`GET /p?token='quoted-url-sentinel'&type=task.completed`,
		`GET /p?token="quoted-url-sentinel"&type=task.completed`,
		`GET /p?token='quoted-url-sentinel with whitespace'&type=task.completed`,
		`GET /p?token="quoted-url-sentinel with whitespace"&type=task.completed`,
		`GET /p?token='quoted-url-sentinel unclosed with whitespace`,
		`GET /p?token="quoted-url-sentinel unclosed with whitespace`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			out := audit.RedactURLText(input)
			for _, secret := range []string{"quoted-url-sentinel", "prefix", "with whitespace"} {
				if strings.Contains(out, secret) {
					t.Fatalf("quoted credential leaked: %s", out)
				}
			}
			if !strings.Contains(out, "token=REDACTED") {
				t.Fatalf("redaction missing: %s", out)
			}
			if strings.Contains(input, "task.completed") && !strings.Contains(out, "task.completed") {
				t.Fatalf("non-sensitive query changed: %s", out)
			}
		})
	}
	if got := audit.RedactURLText(`Get "https://host/p?token=secret": failed`); got != `Get "https://host/p?token=REDACTED": failed` {
		t.Fatalf("surrounding diagnostic changed: %s", got)
	}
}

func TestURLQueryRule_PreservesJSONNumber(t *testing.T) {
	number := json.Number("9007199254740993")
	input := map[string]any{"number": number, "items": []any{number}, "url": "/p?token=number-url-sentinel"}
	got, err := patterns.New().Redact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	fields := got.(map[string]any)
	for _, value := range []any{fields["number"], fields["items"].([]any)[0]} {
		if n, ok := value.(json.Number); !ok || n != number {
			t.Fatalf("numeric evidence changed type/value: %T(%v)", value, value)
		}
	}
	if fields["url"] != "/p?token=REDACTED" {
		t.Fatalf("URL credential was not redacted: %v", fields["url"])
	}
}

func TestRedactURLValue_PreservesNumbersAndSanitizesStringers(t *testing.T) {
	number := json.Number("9007199254740993")
	got, err := audit.RedactURLValue(map[string]any{
		"number": number,
		"error":  errors.New("Get https://host/p?token=value-error-sentinel: refused"),
		"url":    &url.URL{Scheme: "https", Host: "host", RawQuery: "key=value-url-sentinel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := got.(map[string]any)["number"]
	if n, ok := value.(json.Number); !ok || n != number {
		t.Fatalf("numeric diagnostic changed type/value: %T(%v)", value, value)
	}
	out := flatten(t, got)
	if strings.Contains(out, "value-error-sentinel") || strings.Contains(out, "value-url-sentinel") || !strings.Contains(out, "REDACTED") {
		t.Fatalf("URL stringer redaction failed: %s", out)
	}
}
