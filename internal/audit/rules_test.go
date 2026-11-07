package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

func TestCanonicalRules_ListsAllNamedSecrets(t *testing.T) {
	got := audit.CanonicalRules()
	want := map[string]bool{
		"api_key":              false,
		"password":             false,
		"secret":               false,
		"token":                false,
		"cookie":               false,
		"authorization":        false,
		"bearer":               false,
		"injection_credential": false,
		"bearer_in_value":      false,
		"basic_in_value":       false,
		"multimodal":           false,
	}
	for _, r := range got {
		if _, ok := want[r.Name()]; ok {
			want[r.Name()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("rule %q missing from CanonicalRules()", name)
		}
	}
}

func TestKeyRule_RedactsCanonicalKeys(t *testing.T) {
	driver := patterns.New()
	cases := []struct {
		key      string
		input    string
		expected string
	}{
		{"api_key", "sk-real", "***"},
		{"apikey", "sk-real", "***"},
		{"api-key", "sk-real", "***"},
		{"x-api-key", "sk-real", "***"},
		{"password", "hunter2", "***"},
		{"Password", "hunter2", "***"},
		{"client_secret", "abc", "***"},
		{"private_key", "----BEGIN----", "***"},
		{"signing_key", "shhhh", "***"},
		{"access_token", "jwt", "***"},
		{"refresh_token", "rt", "***"},
		{"id_token", "id", "***"},
		{"cookie", "session=abc", "***"},
		{"set-cookie", "session=abc; Path=/", "***"},
		{"Authorization", "Bearer xxx", "***"},
		{"bearer", "xxx", "***"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			in := map[string]any{tc.key: tc.input}
			out, err := driver.Redact(context.Background(), in)
			if err != nil {
				t.Fatalf("Redact: %v", err)
			}
			m, ok := out.(map[string]any)
			if !ok {
				t.Fatalf("Redact returned %T, want map[string]any", out)
			}
			if got := m[tc.key]; got != tc.expected {
				t.Errorf("redacted[%q] = %v, want %q", tc.key, got, tc.expected)
			}
		})
	}
}

func TestKeyRule_DoesNotOverMatchPlainWords(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"description": "this is a normal description, not a secret",
		"username":    "alice",
		"tenant_id":   "t-1",
		// The injection_credential rule matches only the TRAILING key segment,
		// so a legitimate observability field whose key merely CONTAINS a token
		// word must pass through unredacted (token_type / token_url are RFC 8693
		// identifiers, not secrets).
		"token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":  "https://broker.example/token",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["description"] != "this is a normal description, not a secret" {
		t.Errorf("description was modified: %v", m["description"])
	}
	if m["username"] != "alice" {
		t.Errorf("username was modified: %v", m["username"])
	}
	if m["token_type"] != "urn:ietf:params:oauth:token-type:jwt" {
		t.Errorf("token_type unexpectedly modified: %v", m["token_type"])
	}
	if m["token_url"] != "https://broker.example/token" {
		t.Errorf("token_url unexpectedly modified: %v", m["token_url"])
	}
}

// TestInjectionCredentialRule_RedactsReceiverInjectionForms proves every
// receiver-style credential-injection form is held to the same `***` bar as the
// Bearer path: an Authorization: Basic value (both as a header-map value and an
// inline string), an arbitrary vendor header key, and a nested `_meta`
// credential leaf.
func TestInjectionCredentialRule_RedactsReceiverInjectionForms(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"headers": map[string]any{
			"Authorization":    "Basic dXNlcjpzM2NyZXQ=", // base64(user:s3cret)
			"x-vendor-api-key": "sk-live-vendor-DO-NOT-LEAK",
			"content-type":     "application/json",
		},
		"_meta": map[string]any{
			"vendor": map[string]any{
				"api_key": "meta-cred-DO-NOT-LEAK",
			},
		},
		"log_line": "sent Authorization: Basic dXNlcjpzM2NyZXQ= downstream",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	headers := m["headers"].(map[string]any)
	if headers["Authorization"] != "***" {
		t.Errorf("Authorization: Basic not redacted: %v", headers["Authorization"])
	}
	if headers["x-vendor-api-key"] != "***" {
		t.Errorf("vendor api-key header not redacted: %v", headers["x-vendor-api-key"])
	}
	if headers["content-type"] != "application/json" {
		t.Errorf("content-type over-redacted: %v", headers["content-type"])
	}
	meta := m["_meta"].(map[string]any)
	vendor := meta["vendor"].(map[string]any)
	if vendor["api_key"] != "***" {
		t.Errorf("_meta credential leaf not redacted: %v", vendor["api_key"])
	}
	logLine := m["log_line"].(string)
	if strings.Contains(logLine, "dXNlcjpzM2NyZXQ=") {
		t.Errorf("inline Basic credential leaked: %q", logLine)
	}
	if !strings.Contains(logLine, "Basic ***") {
		t.Errorf("inline Basic redaction marker missing: %q", logLine)
	}
}

func TestBearerInValueRule_RedactsEmbeddedCredential(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"log_line": "outbound request used Bearer eyJxxx.yyy.zzz to authenticate",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["log_line"].(string)
	if strings.Contains(got, "eyJxxx.yyy.zzz") {
		t.Errorf("embedded bearer credential leaked: %q", got)
	}
	if !strings.Contains(got, "Bearer ***") {
		t.Errorf("redaction marker missing: %q", got)
	}
}

func TestMultimodalRule_RedactsDataURL(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgAAIAAAUAAeImBZsAAAAASUVORK5CYII=",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["image"].(string)
	if !strings.HasPrefix(got, "[redacted: image/png of ") {
		t.Errorf("DataURL not redacted to placeholder: %q", got)
	}
}

func TestMultimodalRule_PassesThroughArtifactRef(t *testing.T) {
	driver := patterns.New()
	ref := audit.ArtifactRef{
		Ref:       "art://store/abc",
		MIME:      "image/png",
		SizeBytes: 65536,
	}
	in := map[string]any{"image": ref}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["image"] != ref {
		t.Errorf("ArtifactRef did not pass through unchanged: %+v", m["image"])
	}
}

func TestKeyRule_PassesArtifactRefThrough(t *testing.T) {
	// A field NAMED `api_key` whose value is an ArtifactRef should
	// stay unredacted — refs carry no secret bytes themselves.
	driver := patterns.New()
	ref := audit.ArtifactRef{Ref: "art://store/key", MIME: "application/octet-stream"}
	in := map[string]any{"api_key": ref}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["api_key"] != ref {
		t.Errorf("ArtifactRef under api_key should pass through; got %+v", m["api_key"])
	}
}

func TestRedact_NilPayload(t *testing.T) {
	driver := patterns.New()
	out, err := driver.Redact(context.Background(), nil)
	if err != nil {
		t.Fatalf("Redact(nil): %v", err)
	}
	if out != nil {
		t.Errorf("Redact(nil) = %v, want nil", out)
	}
}

func TestRedact_ScalarPayload(t *testing.T) {
	driver := patterns.New()
	cases := []any{
		"a plain string",
		42,
		3.14,
		true,
	}
	for _, in := range cases {
		out, err := driver.Redact(context.Background(), in)
		if err != nil {
			t.Errorf("Redact(%v): %v", in, err)
			continue
		}
		if out != in {
			t.Errorf("Redact(%v) = %v, want unchanged", in, out)
		}
	}
}

func TestRedact_DoesNotMutateInput(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"api_key": "real-secret",
		"nested": map[string]any{
			"password": "hunter2",
		},
	}
	_, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if in["api_key"] != "real-secret" {
		t.Errorf("Redact mutated input top-level: %v", in["api_key"])
	}
	nested := in["nested"].(map[string]any)
	if nested["password"] != "hunter2" {
		t.Errorf("Redact mutated input nested: %v", nested["password"])
	}
}

func TestRedact_DepthCapTriggersError(t *testing.T) {
	// Build a payload deeper than MaxDepth via a chain of nested maps.
	deep := map[string]any{}
	cur := deep
	for range audit.MaxDepth + 10 {
		next := map[string]any{}
		cur["next"] = next
		cur = next
	}
	driver := patterns.New()
	out, err := driver.Redact(context.Background(), deep)
	if err == nil {
		t.Fatal("Redact accepted a payload deeper than MaxDepth")
	}
	if !errors.Is(err, audit.ErrRedactionDepthExceeded) {
		t.Errorf("err=%v, want errors.Is ErrRedactionDepthExceeded", err)
	}
	if out != nil {
		t.Errorf("Redact returned non-nil payload on depth error: %v", out)
	}
}

// FuzzRedactor explores random byte inputs as JSON-decodable strings.
// The contract is: never panic; either return a redacted result or
// an error.
func FuzzRedactor(f *testing.F) {
	f.Add("hello", "api_key", "secret-value")
	f.Add("", "", "")
	f.Add(strings.Repeat("a", 1024), "password", "p")
	driver := patterns.New()
	f.Fuzz(func(t *testing.T, msg, key, val string) {
		in := map[string]any{
			"message": msg,
			key:       val,
		}
		_, err := driver.Redact(context.Background(), in)
		if err != nil && !errors.Is(err, audit.ErrRedactionFailed) &&
			!errors.Is(err, audit.ErrRedactionDepthExceeded) {
			t.Errorf("unexpected err shape: %v", err)
		}
	})
}

// TestInjectionCredentialRule_NestingCausesOverRedactionNotALeak characterises
// — and pins — the audit consequence of interpreting a `meta_annotations` key
// as a `_meta` PATH.
//
// walkRedactKeys matches on the KEY and replaces the WHOLE value; it does NOT
// recurse into a matched node. The injection rule's predicate matches on the
// LAST `-`/`_`/`.`-separated segment. So:
//
//   - FLAT (the pre-nesting shape): the literal key `token.env` has last
//     segment `env`, which is not a credential token, so it is NOT redacted.
//   - NESTED (after nesting): the node key is `token`, which IS a credential
//     token, so the ENTIRE subtree collapses to `***` — including non-secret
//     siblings under the same namespace.
//
// Redaction COVERAGE is therefore preserved (nothing that was redacted stops
// being redacted; a credential leaf under a matching node is still `***`
// because its whole parent is). The delta is OVER-redaction of non-secret
// siblings — a loss of audit usefulness, not of audit safety.
//
// This is deliberately characterised and pinned rather than "fixed": changing
// walkRedactKeys' replace-on-match semantics would alter EVERY rule's
// behaviour, which needs its own decision entry. No audit rule changes here.
func TestInjectionCredentialRule_NestingCausesOverRedactionNotALeak(t *testing.T) {
	driver := patterns.New()

	t.Run("flat key is not redacted (the pre-nesting shape)", func(t *testing.T) {
		out, err := driver.Redact(context.Background(), map[string]any{
			"_meta": map[string]any{"token.env": "prod"},
		})
		if err != nil {
			t.Fatalf("Redact: %v", err)
		}
		meta := out.(map[string]any)["_meta"].(map[string]any)
		if meta["token.env"] != "prod" {
			t.Fatalf("_meta[\"token.env\"] = %v, want the unredacted \"prod\" (last segment `env` is not a credential token)", meta["token.env"])
		}
	})

	t.Run("nested node collapses its whole subtree", func(t *testing.T) {
		out, err := driver.Redact(context.Background(), map[string]any{
			"_meta": map[string]any{
				"token": map[string]any{
					"env":     "prod",               // non-secret companion
					"api_key": "s3cret-DO-NOT-LEAK", // the credential
				},
			},
		})
		if err != nil {
			t.Fatalf("Redact: %v", err)
		}
		meta := out.(map[string]any)["_meta"].(map[string]any)
		if meta["token"] != audit.Placeholder {
			t.Fatalf("_meta.token = %#v, want the whole subtree collapsed to %q", meta["token"], audit.Placeholder)
		}
		// Coverage is preserved: no credential byte survives anywhere.
		if strings.Contains(flatten(t, out), "s3cret-DO-NOT-LEAK") {
			t.Fatal("the credential leaked through the collapsed node")
		}
		// ...and the non-secret sibling is gone too. That is the cost.
		if strings.Contains(flatten(t, out), "prod") {
			t.Fatal("test premise broken: the non-secret sibling survived, so this is not over-redaction")
		}
	})
}

// flatten renders a redacted payload for substring assertions.
func flatten(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
