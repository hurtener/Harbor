// Phase 83v (D-162) — cross-origin CORS preflight + actual request
// flow end-to-end. The test boots an httptest server fronting the
// `devstack.Assemble` handler with a populated CORS allowlist (via the
// loaded config) and asserts:
//
//  1. A cross-origin preflight from an allow-listed origin returns 204
//     with the per-origin echo + credentials header set.
//  2. The actual cross-origin request returns the allow-origin header
//     so the browser surfaces the response to the page.
//  3. A cross-origin request from a NON-allowlisted origin returns the
//     body but omits the CORS headers — the browser then blocks per
//     the standard contract.
//  4. The dev-only `cors_dev_allow_any` flag accepts any origin.
//
// Real drivers throughout (config validator + devstack Assemble +
// transports.NewMux + cors.Wrap), per CLAUDE.md §17.3.

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

// writeCORSDevConfig produces a dev YAML matching `writeDevConfig` but
// with the operator-declared CORS allowlist set. Returns the file path.
func writeCORSDevConfig(t *testing.T, allowedOrigins []string, devAllowAny bool) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")

	yaml := devSmokeYAML
	// Inject the CORS fields into the existing `server:` block. The
	// block in devSmokeYAML is small + stable; the replacement is
	// surgical.
	var inject strings.Builder
	for _, o := range allowedOrigins {
		inject.WriteString("  - ")
		inject.WriteString(o)
		inject.WriteString("\n")
	}
	corsBlock := "  allowed_origins:\n" + inject.String()
	if devAllowAny {
		corsBlock += "  cors_dev_allow_any: true\n"
	}
	if len(allowedOrigins) == 0 && devAllowAny {
		corsBlock = "  cors_dev_allow_any: true\n"
	}
	if len(allowedOrigins) == 0 && !devAllowAny {
		corsBlock = ""
	}
	yaml = strings.Replace(yaml,
		"  bind_addr: 127.0.0.1:0\n",
		"  bind_addr: 127.0.0.1:0\n"+corsBlock,
		1)

	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write CORS dev config: %v", err)
	}
	return p
}

// assembleCORSStack mirrors buildPhase64TestStack but lets the caller
// pin the CORS allowlist. Returns the http.Handler + cleanup.
func assembleCORSStack(t *testing.T, allowedOrigins []string, devAllowAny bool) (http.Handler, func()) {
	t.Helper()
	cfgPath := writeCORSDevConfig(t, allowedOrigins, devAllowAny)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	llmSnap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &llmSnap,
	})
	if stack.Handler == nil {
		stack.Close()
		t.Fatal("expected stack.Handler to be non-nil")
	}
	return stack.Handler, stack.Close
}

// TestE2E_Phase83v_CORS_AllowlistMatch — a preflight from an allow-listed
// origin returns 204 with the per-origin echo header set; the actual
// request inherits the allow-origin header. Two httptest servers (the
// Runtime + a simulated Console origin) are spun up so we can prove the
// browser-side contract holds.
func TestE2E_Phase83v_CORS_AllowlistMatch(t *testing.T) {
	const allowedOrigin = "http://127.0.0.1:18790"

	handler, closeStack := assembleCORSStack(t, []string{allowedOrigin}, false)
	defer closeStack()

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Preflight: simulate the browser's OPTIONS request with Origin +
	// Access-Control-Request-Method set.
	preReq, err := http.NewRequest(http.MethodOptions, srv.URL+"/v1/control/sessions.list", nil)
	if err != nil {
		t.Fatalf("preflight req: %v", err)
	}
	preReq.Header.Set("Origin", allowedOrigin)
	preReq.Header.Set("Access-Control-Request-Method", "POST")
	preReq.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")

	preResp, err := http.DefaultClient.Do(preReq)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer preResp.Body.Close()

	if preResp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status: got %d, want 204", preResp.StatusCode)
	}
	if got := preResp.Header.Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("preflight allow-origin: got %q, want %q", got, allowedOrigin)
	}
	if got := preResp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("preflight allow-credentials: got %q, want true", got)
	}
	if got := preResp.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight allow-methods missing")
	}
	if got := preResp.Header.Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("preflight allow-headers missing")
	}
	if got := preResp.Header.Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("preflight emitted '*' — incompatible with credentialed responses")
	}

	// Actual request: GET /healthz with Origin set. The response must
	// carry allow-origin so the browser surfaces the body to the page.
	getReq, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("get req: %v", err)
	}
	getReq.Header.Set("Origin", allowedOrigin)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status: got %d, want 200", getResp.StatusCode)
	}
	if got := getResp.Header.Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("actual-request allow-origin: got %q, want %q", got, allowedOrigin)
	}
	if got := getResp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("actual-request allow-credentials: got %q, want true", got)
	}
	// The Vary header MUST list Origin so shared caches behave.
	vary := getResp.Header.Get("Vary")
	if !strings.Contains(vary, "Origin") {
		t.Errorf("Vary header missing Origin: got %q", vary)
	}
}

// TestE2E_Phase83v_CORS_DefaultDeny — empty allowlist + no dev flag
// keeps the pre-83v posture: cross-origin requests reach the handler
// but NO CORS headers are emitted (browser blocks the response).
func TestE2E_Phase83v_CORS_DefaultDeny(t *testing.T) {
	handler, closeStack := assembleCORSStack(t, nil, false)
	defer closeStack()

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Origin", "https://console.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("default-deny leaked allow-origin: %q", got)
	}
}

// TestE2E_Phase83v_CORS_DeniedOrigin_NoHeaders — operator declared an
// allowlist, but the request's origin is NOT on it: the handler still
// runs, but the middleware omits the allow-* headers so the browser
// blocks. (The server NEVER returns 4xx for a CORS-disallowed origin —
// the contract is browser-side enforcement only.)
func TestE2E_Phase83v_CORS_DeniedOrigin_NoHeaders(t *testing.T) {
	handler, closeStack := assembleCORSStack(t,
		[]string{"https://console.example.com"}, false)
	defer closeStack()

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Preflight from an attacker origin.
	preReq, err := http.NewRequest(http.MethodOptions, srv.URL+"/v1/control/sessions.list", nil)
	if err != nil {
		t.Fatalf("preflight req: %v", err)
	}
	preReq.Header.Set("Origin", "https://attacker.example.org")
	preReq.Header.Set("Access-Control-Request-Method", "POST")
	preResp, err := http.DefaultClient.Do(preReq)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer preResp.Body.Close()
	if preResp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status: got %d, want 204", preResp.StatusCode)
	}
	if got := preResp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied preflight leaked allow-origin: %q", got)
	}
}

// TestE2E_Phase83v_CORS_DevAllowAny — operator opted into the dev-only
// any-origin escape hatch. Any origin gets allow-* headers; never `*`.
func TestE2E_Phase83v_CORS_DevAllowAny(t *testing.T) {
	handler, closeStack := assembleCORSStack(t, nil, true)
	defer closeStack()

	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, origin := range []string{
		"http://127.0.0.1:5173", // Vite dev
		"https://random.example.com",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
		if err != nil {
			t.Fatalf("req: %v", err)
		}
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("DevAllowAny: allow-origin echo: got %q, want %q", got, origin)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got == "*" {
			t.Errorf("DevAllowAny: leaked '*' for origin %q", origin)
		}
	}
}
