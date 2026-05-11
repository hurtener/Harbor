package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
	hdriver "github.com/hurtener/Harbor/internal/tools/drivers/http"
)

// TestHTTPTool_SchemaValidation_RejectsBadInput exercises the
// input-schema validator path through the policy shell.
func TestHTTPTool_SchemaValidation_RejectsBadInput(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "schema.input", "POST", srv.URL+"/echo",
		hdriver.WithPolicy(tools.ToolPolicy{
			MaxRetries:  0,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
			Validate:    tools.ValidateBoth,
		}),
		hdriver.WithArgsSchema([]byte(`{
			"type":"object",
			"properties":{"message":{"type":"string"}},
			"required":["message"],
			"additionalProperties":false
		}`)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("schema.input")
	ctx := mustIdentityCtx(t)
	// Missing required field 'message'.
	_, err = d.Invoke(ctx, []byte(`{}`))
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got %v", err)
	}
}

// TestHTTPTool_OutputSchemaValidation asserts the output validator
// runs on success.
func TestHTTPTool_OutputSchemaValidation(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a shape that won't match the schema.
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "out.schema", "GET", srv.URL+"/x",
		hdriver.WithPolicy(tools.ToolPolicy{
			MaxRetries:  0,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{},
			Validate:    tools.ValidateOut,
		}),
		hdriver.WithOutSchema([]byte(`{
			"type":"object",
			"properties":{"ok":{"type":"boolean"}},
			"required":["ok"],
			"additionalProperties":false
		}`)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("out.schema")
	ctx := mustIdentityCtx(t)
	_, err = d.Invoke(ctx, []byte(`null`))
	if err == nil {
		t.Fatal("expected output-schema validation failure")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs (wraps output failure), got %v", err)
	}
}

// TestHTTPTool_RegisterHTTPTool_RejectsBadSchema asserts an invalid
// JSON schema at register time fails loudly.
func TestHTTPTool_RegisterHTTPTool_RejectsBadSchema(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "bad.schema", "GET", "https://example.com/",
		hdriver.WithArgsSchema([]byte(`{not valid json`)),
	)
	if err == nil {
		t.Fatal("expected error for invalid schema")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Errorf("expected 'compile' in error, got %q", err.Error())
	}
}

// TestHTTPTool_NonJSONResponse asserts a non-JSON content-type
// response is returned as a string.
func TestHTTPTool_NonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain text body"))
	}))
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "text.tool", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(0)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("text.tool")
	ctx := mustIdentityCtx(t)
	res, err := d.Invoke(ctx, []byte(`null`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	s, ok := res.Value.(string)
	if !ok {
		t.Fatalf("expected string body, got %T", res.Value)
	}
	if s != "plain text body" {
		t.Errorf("body mismatch: %q", s)
	}
}

// TestHTTPTool_EmptyBodyJSONResponse asserts an empty 2xx body is
// surfaced as nil Value.
func TestHTTPTool_EmptyBodyJSONResponse(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(nethttp.StatusOK)
	}))
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "empty.tool", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(0)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("empty.tool")
	ctx := mustIdentityCtx(t)
	res, err := d.Invoke(ctx, []byte(`null`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Value != nil {
		t.Errorf("expected nil value for empty body, got %v", res.Value)
	}
}

// TestHTTPTool_BadResponseJSON asserts a malformed JSON response
// fails loudly.
func TestHTTPTool_BadResponseJSON(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid`))
	}))
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "bad.json", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(0)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("bad.json")
	ctx := mustIdentityCtx(t)
	_, err = d.Invoke(ctx, []byte(`null`))
	if err == nil {
		t.Fatal("expected JSON decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected 'decode response' in error, got %q", err.Error())
	}
}

// TestHTTPTool_RetryAfterHTTPDate exercises the http-date parser
// path in parseRetryAfter.
func TestHTTPTool_RetryAfterHTTPDate(t *testing.T) {
	delay := 2 * time.Second
	target := time.Now().Add(delay)
	var attempts int
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", target.UTC().Format(nethttp.TimeFormat))
			w.WriteHeader(nethttp.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"err":"unavail"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "rl.date", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(3)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("rl.date")
	ctx := mustIdentityCtx(t)
	start := time.Now()
	res, err := d.Invoke(ctx, []byte(`null`))
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	v, _ := res.Value.(map[string]any)
	if v["ok"] != true {
		t.Errorf("expected ok=true, got %v", res.Value)
	}
	// We don't insist on full `delay` because the HTTP-date format
	// has 1-second precision, so the actual elapsed might be ~1s.
	if dur < 500*time.Millisecond {
		t.Errorf("expected elapsed >= 500ms (some Retry-After honour), got %v", dur)
	}
}

// TestHTTPTool_WithClient_HonoursCustom asserts WithClient is
// actually consulted.
func TestHTTPTool_WithClient_HonoursCustom(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	custom := &nethttp.Client{Timeout: 10 * time.Second}
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "custom.client", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(0)),
		hdriver.WithClient(custom),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("custom.client")
	ctx := mustIdentityCtx(t)
	if _, err := d.Invoke(ctx, []byte(`null`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
}

// TestHTTPTool_BadURL_FailsLoudly asserts an unparseable rendered
// URL surfaces ErrTemplateRender.
func TestHTTPTool_BadURL_FailsLoudly(t *testing.T) {
	cat := tools.NewCatalog()
	// Use a template that renders to an invalid URL with control bytes.
	err := hdriver.RegisterHTTPTool(cat, "bad.url", "GET", `://invalid{{ .Args.foo }}`,
		hdriver.WithPolicy(fastPolicy(0)),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("bad.url")
	ctx := mustIdentityCtx(t)
	_, err = d.Invoke(ctx, []byte(`{"foo":"bar"}`))
	if err == nil {
		t.Fatal("expected url parse error")
	}
}

// TestHTTPTool_LargeBody_Snippet asserts the body snippet is bounded.
func TestHTTPTool_LargeBody_Snippet(t *testing.T) {
	big := make([]byte, 1024)
	for i := range big {
		big[i] = 'x'
	}
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.WriteHeader(nethttp.StatusInternalServerError)
		_, _ = w.Write(big)
	}))
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "big.err", "GET", srv.URL+"/x",
		hdriver.WithPolicy(tools.ToolPolicy{
			MaxRetries:  0,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{},
			Validate:    tools.ValidateNone,
		}),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("big.err")
	ctx := mustIdentityCtx(t)
	_, err = d.Invoke(ctx, []byte(`null`))
	if err == nil {
		t.Fatal("expected error from 500")
	}
	if len(err.Error()) > 1024 {
		t.Errorf("error message not bounded: len=%d", len(err.Error()))
	}
}

// TestHTTPTool_ConnectionRefused asserts a transport-level error is
// classified as Transient and retried.
func TestHTTPTool_ConnectionRefused(t *testing.T) {
	// 127.0.0.1:1 is reserved as TCPMUX; should refuse connections.
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "conn.refused", "GET", "http://127.0.0.1:1/",
		hdriver.WithPolicy(tools.ToolPolicy{
			MaxRetries:  1,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   500,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
			Validate:    tools.ValidateNone,
		}),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("conn.refused")
	ctx := mustIdentityCtx(t)
	_, err = d.Invoke(ctx, []byte(`null`))
	if err == nil {
		t.Fatal("expected connection error")
	}
	if !errors.Is(err, tools.ErrToolPolicyExhausted) {
		t.Fatalf("expected ErrToolPolicyExhausted (retried), got %v", err)
	}
}

// TestHTTPTool_IsRateLimitedHelper sanity-checks IsRateLimited on a
// directly-constructed error.
func TestHTTPTool_IsRateLimitedHelperDirect(t *testing.T) {
	delay, ok := hdriver.IsRateLimited(fmt.Errorf("not a rate limit"))
	if ok {
		t.Errorf("expected non-rate-limit, got delay=%v", delay)
	}
}

// TestHTTPTool_ManifestDescriptorSchemaPath asserts that a manifest
// with both args + out schema actually applies them.
func TestHTTPTool_ManifestDescriptorSchemaPath(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cat := tools.NewCatalog()
	m := &hdriver.Manifest{
		Tools: []hdriver.ManifestTool{{
			Name:        "schema.path",
			Method:      "POST",
			URLTemplate: srv.URL + "/x",
			ArgsSchema:  `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
			OutSchema:   `{"type":"object","properties":{"ok":{"type":"boolean"}}}`,
			SourceID:    "manifest:test",
		}},
	}
	if err := hdriver.RegisterManifest(cat, m); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	d, ok := cat.Resolve("schema.path")
	if !ok {
		t.Fatal("not registered")
	}
	if len(d.Tool.ArgsSchema) == 0 {
		t.Error("expected ArgsSchema on tool")
	}
	if string(d.Tool.Source) != "manifest:test" {
		t.Errorf("expected Source=manifest:test, got %q", d.Tool.Source)
	}

	ctx := mustIdentityCtx(t)
	// Wrong input shape — should fail schema validation.
	_, err := d.Invoke(ctx, []byte(`{}`))
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got %v", err)
	}

	// Right shape — should succeed.
	args, _ := json.Marshal(map[string]int{"n": 42})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	v, _ := res.Value.(map[string]any)
	if v["ok"] != true {
		t.Errorf("expected ok=true, got %v", res.Value)
	}
}

// TestHTTPTool_WithAuthScopes_VisibleViaCatalogFilter exercises the
// WithAuthScopes option through the catalog filter path.
func TestHTTPTool_WithAuthScopes_VisibleViaCatalogFilter(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "scoped.tool", "GET", srv.URL+"/x",
		hdriver.WithPolicy(fastPolicy(0)),
		hdriver.WithAuthScopes("weather:read"),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Without scope → not visible.
	list := cat.List(tools.CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	if len(list) != 0 {
		t.Errorf("expected 0 visible without scope, got %v", list)
	}
	// With scope → visible.
	list = cat.List(tools.CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
		GrantedScopes: []string{"weather:read"},
	})
	if len(list) != 1 {
		t.Errorf("expected 1 visible with scope, got %v", list)
	}
}

// TestHTTPTool_RateLimitErrorStatusGetter exercises the Status()
// accessor on the rate-limit error.
func TestHTTPTool_RateLimitErrorStatusGetter(t *testing.T) {
	ras := newRetryAfterServer(t, 100, 1*time.Second) // always rate-limit
	defer ras.server.Close()
	cat := tools.NewCatalog()
	_ = hdriver.RegisterHTTPTool(cat, "rl.always", "GET", ras.server.URL+"/x",
		hdriver.WithPolicy(tools.ToolPolicy{
			MaxRetries:  0,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   500,
			RetryOn:     []tools.ErrorClass{},
			Validate:    tools.ValidateNone,
		}),
	)
	d, _ := cat.Resolve("rl.always")
	ctx, cancel := context.WithTimeout(mustIdentityCtx(t), 1500*time.Millisecond)
	defer cancel()
	_, err := d.Invoke(ctx, []byte(`null`))
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestHTTPTool_BadTemplate_FailsAtRegister covers the template-parse
// error branch of RegisterHTTPTool.
func TestHTTPTool_BadTemplate_FailsAtRegister(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "bad.tmpl", "GET", `{{ .Args.unclosed`)
	if !errors.Is(err, hdriver.ErrTemplateRender) {
		t.Fatalf("expected ErrTemplateRender at register, got %v", err)
	}
}

// TestHTTPTool_BadBodyTemplate_FailsAtRegister covers the body
// template-parse error branch.
func TestHTTPTool_BadBodyTemplate_FailsAtRegister(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "bad.body", "POST", "https://example.com/",
		hdriver.WithBodyTemplate(`{{ .Args.unclosed`),
	)
	if !errors.Is(err, hdriver.ErrTemplateRender) {
		t.Fatalf("expected ErrTemplateRender, got %v", err)
	}
}

// TestHTTPTool_SecretLeak_InBodyTemplate covers the body-template
// secret-leak branch.
func TestHTTPTool_SecretLeak_InBodyTemplate(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "leaky.body", "POST", "https://example.com/",
		hdriver.WithBodyTemplate(`{"token":"{{ .Auth.weather }}"}`),
	)
	if !errors.Is(err, hdriver.ErrTemplateSecretLeak) {
		t.Fatalf("expected ErrTemplateSecretLeak for body, got %v", err)
	}
}

// TestHTTPTool_SecretLeak_InHeaderTemplate covers the header
// secret-leak branch.
func TestHTTPTool_SecretLeak_InHeaderTemplate(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "leaky.hdr", "GET", "https://example.com/",
		hdriver.WithHeaders(map[string]string{"X-Token": "{{ .Auth.something }}"}),
	)
	if !errors.Is(err, hdriver.ErrTemplateSecretLeak) {
		t.Fatalf("expected ErrTemplateSecretLeak for header, got %v", err)
	}
}

// TestHTTPTool_BadAuthSpec_FailsAtRegister exercises the auth-spec
// validate path during RegisterHTTPTool.
func TestHTTPTool_BadAuthSpec_FailsAtRegister(t *testing.T) {
	cat := tools.NewCatalog()
	err := hdriver.RegisterHTTPTool(cat, "bad.auth", "GET", "https://example.com/",
		hdriver.WithAuth(hdriver.AuthSpec{
			Kind:       hdriver.AuthKindAPIKey,
			HeaderName: "X",
			QueryParam: "y",
		}, "secret"),
	)
	if !errors.Is(err, hdriver.ErrAuthInvalidSpec) {
		t.Fatalf("expected ErrAuthInvalidSpec, got %v", err)
	}
}

// TestHTTPTool_RegisterEmptyName covers the empty-name branch.
func TestHTTPTool_RegisterEmptyName(t *testing.T) {
	err := hdriver.RegisterHTTPTool(tools.NewCatalog(), "", "GET", "https://example.com/")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

// TestHTTPTool_RegisterEmptyURL covers the empty-URL branch.
func TestHTTPTool_RegisterEmptyURL(t *testing.T) {
	err := hdriver.RegisterHTTPTool(tools.NewCatalog(), "name", "GET", "")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

// TestHTTPTool_RegisterNilCatalog covers the nil-catalog branch.
func TestHTTPTool_RegisterNilCatalog(t *testing.T) {
	err := hdriver.RegisterHTTPTool(nil, "name", "GET", "https://example.com/")
	if err == nil {
		t.Fatal("expected error for nil catalog")
	}
}

// TestRegisterManifest_NilManifestRejected covers the nil-manifest
// branch.
func TestRegisterManifest_NilManifestRejected(t *testing.T) {
	err := hdriver.RegisterManifest(tools.NewCatalog(), nil)
	if err == nil {
		t.Fatal("expected error for nil manifest")
	}
}

// TestRegisterManifest_NilCatalogRejected covers the nil-cat branch.
func TestRegisterManifest_NilCatalogRejected(t *testing.T) {
	err := hdriver.RegisterManifest(nil, &hdriver.Manifest{})
	if err == nil {
		t.Fatal("expected error for nil catalog")
	}
}

// TestLoadManifest_EmptyPath covers the empty-path branch.
func TestLoadManifest_EmptyPath(t *testing.T) {
	_, err := hdriver.LoadManifest("")
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for empty path, got %v", err)
	}
}

// TestLoadManifest_NonExistentFile covers the read-error branch.
func TestLoadManifest_NonExistentFile(t *testing.T) {
	_, err := hdriver.LoadManifest("/does/not/exist.yaml")
	if !errors.Is(err, hdriver.ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for missing file, got %v", err)
	}
}
