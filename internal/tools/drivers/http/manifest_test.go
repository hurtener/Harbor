package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	hdriver "github.com/hurtener/Harbor/internal/tools/drivers/http"
)

// TestLoadManifest_ValidFixture loads the valid manifest fixture
// (with HARBOR_TEST_ECHO_KEY env var set) and asserts each tool
// registers successfully.
func TestLoadManifest_ValidFixture(t *testing.T) {
	t.Setenv("HARBOR_TEST_ECHO_KEY", "secret-echo-key")

	m, err := hdriver.LoadManifest(filepath.Join("testdata", "manifest_valid.yaml"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(m.Tools))
	}
	if got := m.Auth["echo_key"].Secret; got != "secret-echo-key" {
		t.Errorf("expected expanded secret 'secret-echo-key', got %q", got)
	}

	cat := tools.NewCatalog()
	if err := hdriver.RegisterManifest(cat, m); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	d, ok := cat.Resolve("echo.lookup")
	if !ok {
		t.Fatal("echo.lookup not registered")
	}
	if d.Tool.Transport != tools.TransportHTTP {
		t.Errorf("expected Transport=http, got %q", d.Tool.Transport)
	}
	if !strings.HasPrefix(string(d.Tool.Source), "manifest:") {
		t.Errorf("expected manifest: source prefix, got %q", d.Tool.Source)
	}
	if len(d.Tool.ArgsSchema) == 0 {
		t.Error("expected non-empty ArgsSchema")
	}
}

// TestLoadManifest_RejectsSecretLeak asserts that a manifest whose
// URL template references .Auth fails at load time.
func TestLoadManifest_RejectsSecretLeak(t *testing.T) {
	t.Setenv("HARBOR_TEST_WEATHER_KEY", "value-that-should-never-be-used")
	_, err := hdriver.LoadManifest(filepath.Join("testdata", "manifest_secret_leak.yaml"))
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid wrapping secret leak, got %v", err)
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("expected error to mention secret, got %q", err.Error())
	}
}

// TestLoadManifest_RejectsLiteralSecret asserts that a manifest with
// an inline literal secret fails at load time.
func TestLoadManifest_RejectsLiteralSecret(t *testing.T) {
	_, err := hdriver.LoadManifest(filepath.Join("testdata", "manifest_literal_secret.yaml"))
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "${ENV_VAR}") &&
		!strings.Contains(err.Error(), "literal") {
		t.Errorf("expected error to flag literal secret, got %q", err.Error())
	}
}

// TestLoadManifest_RejectsMissingEnvVar asserts that a manifest
// referencing an unset env var fails at load time.
func TestLoadManifest_RejectsMissingEnvVar(t *testing.T) {
	// Ensure the env var is unset.
	t.Setenv("HARBOR_TEST_ECHO_KEY", "")

	_, err := hdriver.LoadManifest(filepath.Join("testdata", "manifest_valid.yaml"))
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for missing env, got %v", err)
	}
}

// TestLoadManifest_RejectsBadMethod runs against an inline-loaded
// manifest with an OPTIONS method.
func TestLoadManifest_RejectsBadMethod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad_method.yaml")
	body := []byte(`
auth:
  k:
    kind: bearer
    secret: ${BAD_METHOD_KEY}
tools:
  - name: bad.method
    method: OPTIONS
    url_template: 'https://example.com/'
    auth_ref: k
`)
	if err := write(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("BAD_METHOD_KEY", "v")
	_, err := hdriver.LoadManifest(path)
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for bad method, got %v", err)
	}
}

// TestLoadManifest_RejectsDuplicateTool asserts duplicate tool names
// inside one manifest fail loudly.
func TestLoadManifest_RejectsDuplicateTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.yaml")
	body := []byte(`
tools:
  - name: dup
    method: GET
    url_template: 'https://example.com/a'
  - name: dup
    method: GET
    url_template: 'https://example.com/b'
`)
	if err := write(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := hdriver.LoadManifest(path)
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for duplicate name, got %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected error to mention duplicate, got %q", err.Error())
	}
}

// TestLoadManifest_RejectsEmpty asserts an empty file fails loudly.
func TestLoadManifest_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := write(path, []byte("tools: []\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := hdriver.LoadManifest(path)
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for empty manifest, got %v", err)
	}
}

// TestLoadManifest_RejectsUnknownAuthRef asserts a tool referencing
// a nonexistent auth_ref fails loudly.
func TestLoadManifest_RejectsUnknownAuthRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad_ref.yaml")
	body := []byte(`
tools:
  - name: bad.ref
    method: GET
    url_template: 'https://example.com/'
    auth_ref: nonexistent
`)
	if err := write(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := hdriver.LoadManifest(path)
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for unknown auth_ref, got %v", err)
	}
}

// TestLoadManifest_RejectsInvalidJSONSchema asserts an args_schema
// that isn't valid JSON fails loudly.
func TestLoadManifest_RejectsInvalidJSONSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad_schema.yaml")
	body := []byte(`
tools:
  - name: bad.schema
    method: GET
    url_template: 'https://example.com/'
    args_schema: 'not-json{'
`)
	if err := write(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := hdriver.LoadManifest(path)
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for invalid schema, got %v", err)
	}
}

// TestHTTPTool_InlineEqualsManifest asserts the inline + manifest
// registration paths converge: same Tool struct shape; same Invoke
// result for the same input.
func TestHTTPTool_InlineEqualsManifest(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echoed":` + string(body) + `}`))
	}))
	defer srv.Close()

	// Register inline.
	catInline := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(catInline, "echo", "POST", srv.URL+"/echo",
		hdriver.WithPolicy(fastPolicy(0)),
	)
	if err != nil {
		t.Fatalf("register inline: %v", err)
	}

	// Register via manifest.
	t.Setenv("INLINE_EQ_KEY", "ignored")
	dir := t.TempDir()
	path := filepath.Join(dir, "echo.yaml")
	body := []byte(`
tools:
  - name: echo
    method: POST
    url_template: '` + srv.URL + `/echo'
`)
	if err := write(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := hdriver.LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	catManifest := tools.NewCatalog()
	if err := hdriver.RegisterManifest(catManifest, m); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}

	// Override policy on the manifest descriptor — we can't pass
	// fastPolicy via YAML easily, so just give the test a tight
	// deadline; the manifest's zero-policy means defaults (3 retries)
	// which we DON'T trigger because the server returns 200.

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	args := []byte(`{"hello":"world"}`)

	dInline, _ := catInline.Resolve("echo")
	rInline, err := dInline.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke inline: %v", err)
	}
	dMani, _ := catManifest.Resolve("echo")
	rMani, err := dMani.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke manifest: %v", err)
	}

	// The Value shapes should be byte-identical when marshalled.
	bInline, _ := json.Marshal(rInline.Value)
	bMani, _ := json.Marshal(rMani.Value)
	if string(bInline) != string(bMani) {
		t.Errorf("inline vs manifest result mismatch:\n  inline:   %s\n  manifest: %s",
			bInline, bMani)
	}
}

// write is a tiny test helper to drop a file at path.
func write(path string, content []byte) error {
	return os.WriteFile(path, content, 0o600)
}
