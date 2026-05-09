package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

func TestCanonicalRules_ListsAllNamedSecrets(t *testing.T) {
	got := audit.CanonicalRules()
	want := map[string]bool{
		"api_key":         false,
		"password":        false,
		"secret":          false,
		"token":           false,
		"cookie":          false,
		"authorization":   false,
		"bearer":          false,
		"bearer_in_value": false,
		"multimodal":      false,
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
		"description":   "this is a normal description, not a secret",
		"username":      "alice",
		"tenant_id":     "t-1",
		"user_password": "yes-this-key-fragment-token-bearer", // partial match must not trigger
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
	// user_password is NOT exactly "password" so it should pass through.
	if m["user_password"] != "yes-this-key-fragment-token-bearer" {
		t.Errorf("user_password unexpectedly modified: %v", m["user_password"])
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
	for i := 0; i < audit.MaxDepth+10; i++ {
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
