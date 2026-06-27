package integration

// oidc_serve_roundtrip_test.go — the binding production-leg E2E for the
// Protocol adopter path: an OIDC client obtains a JWT via the OAuth2
// client-credentials grant and attaches it to a real `harbor serve`.
//
// The leg this proves (the v1.8.0 Adopter-Path P0 consumer): a hermetic
// ES256/JWKS mock-OIDC issuer mints a token the REAL parser accepts,
// `harbor serve` boots pointed at the issuer's published JWKS, the worked
// example (examples/protocol-clients/oidc-client-example, SDK-free) runs
// the client-credentials grant and calls runtime.info, and the call
// returns OK. The granted scopes are decoded client-side from the JWT by
// the worked example (the runtime verifies the token signature, not the
// scope set; runtime.info echoes no scopes), so the scope assertion proves
// the IdP→client claim mapping, not a runtime-side scope return. A failure-mode leg presents a
// token signed by a key the JWKS does not publish and asserts the
// verifier rejects it (the §17.3 ≥1-failure-mode requirement).
//
// Real drivers across the seam (CLAUDE.md §17.3): the production
// JWKS-backed validator (internal/protocol/auth), the production control
// transport, and the real `bin/harbor serve` subprocess — no mock at the
// auth boundary. Identity propagates as the (tenant, user, session) JWT
// claim triple verified at the Protocol edge.

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	oidcBuildOnce  sync.Once
	oidcHarborBin  string
	oidcExampleBin string
	oidcBuildErr   error
)

// buildOIDCBinaries compiles `bin/harbor` and the worked OIDC client
// example once for this test file.
func buildOIDCBinaries(t *testing.T) (harborBin, exampleBin string) {
	t.Helper()
	oidcBuildOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
		if err != nil {
			oidcBuildErr = err
			return
		}
		binDir := filepath.Join(root, "bin")
		oidcHarborBin = filepath.Join(binDir, "harbor-oidc-roundtrip-test")
		oidcExampleBin = filepath.Join(binDir, "oidc-client-example-test")

		if err := goBuild(root, oidcHarborBin, "./cmd/harbor"); err != nil {
			oidcBuildErr = fmt.Errorf("build harbor: %w", err)
			return
		}
		if err := goBuild(root, oidcExampleBin, "./examples/protocol-clients/oidc-client-example"); err != nil {
			oidcBuildErr = fmt.Errorf("build example: %w", err)
			return
		}
	})
	if oidcBuildErr != nil {
		t.Fatalf("build binaries: %v", oidcBuildErr)
	}
	return oidcHarborBin, oidcExampleBin
}

// goBuild runs a CGo-free `go build -o out pkg` from root.
func goBuild(root, out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}

var oidcBoundRe = regexp.MustCompile(`HARBOR_(?:DEV_)?BOUND=([^\s]+)`)

// TestE2E_OIDCClient_ServeRoundtrip is the headline binding leg.
func TestE2E_OIDCClient_ServeRoundtrip(t *testing.T) {
	harborBin, exampleBin := buildOIDCBinaries(t)

	// Skip on a build that predates `harbor serve` (the 404/405/501 →
	// SKIP convention applied to a CLI surface).
	if !subcommandExists(t, harborBin, "serve") {
		t.Skip("harbor serve subcommand absent — pre-serve build")
	}

	const (
		audience = "harbor"
		clientID = "harbor-oidc-example"
		// Documented dummy fixture secret — not a real credential.
		clientSecret = "test-client-secret"
		tenant       = "tenant-acme"
		user         = "svc-oidc-client"
		session      = "sess-oidc-roundtrip"
		wantScope    = "console:fleet"
	)

	issuer := startMockOIDC(t, mockOIDCConfig{
		audience:     audience,
		clientID:     clientID,
		clientSecret: clientSecret,
		tenant:       tenant,
		user:         user,
		session:      session,
	})

	cfgPath := writeServeConfig(t, serveConfigParams{
		issuer:   issuer.issuer,
		audience: audience,
		jwksURL:  issuer.jwksURL(),
	})

	baseURL := bootServe(t, harborBin, cfgPath)

	// The worked client: obtain a token via client-credentials, then
	// runtime.info. Expected scope is requested and asserted in output.
	out, err := runExample(t, exampleBin, map[string]string{
		"OIDC_TOKEN_URL":     issuer.tokenURL(),
		"OIDC_CLIENT_ID":     clientID,
		"OIDC_CLIENT_SECRET": clientSecret,
		"OIDC_SCOPE":         wantScope,
		"HARBOR_BASE_URL":    baseURL,
	})
	if err != nil {
		t.Fatalf("oidc-client-example failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "attach OK") {
		t.Fatalf("example did not report a successful attach; output:\n%s", out)
	}
	if !strings.Contains(out, wantScope) {
		t.Fatalf("example output did not surface the granted scope %q; output:\n%s", wantScope, out)
	}
	if !strings.Contains(out, "speaks Protocol") {
		t.Fatalf("example did not print the runtime.info handshake; output:\n%s", out)
	}
}

// TestE2E_OIDCClient_RejectsUnpublishedKey is the failure-mode leg: a
// token signed by a key absent from the JWKS must be rejected by the
// production verifier, so the worked client fails non-zero at attach.
func TestE2E_OIDCClient_RejectsUnpublishedKey(t *testing.T) {
	harborBin, exampleBin := buildOIDCBinaries(t)
	if !subcommandExists(t, harborBin, "serve") {
		t.Skip("harbor serve subcommand absent — pre-serve build")
	}

	const (
		audience     = "harbor"
		clientID     = "harbor-oidc-example"
		clientSecret = "test-client-secret"
	)
	issuer := startMockOIDC(t, mockOIDCConfig{
		audience:     audience,
		clientID:     clientID,
		clientSecret: clientSecret,
		tenant:       "tenant-acme",
		user:         "svc-oidc-client",
		session:      "sess-oidc-rogue",
	})
	cfgPath := writeServeConfig(t, serveConfigParams{
		issuer:   issuer.issuer,
		audience: audience,
		jwksURL:  issuer.jwksURL(),
	})
	baseURL := bootServe(t, harborBin, cfgPath)

	out, err := runExample(t, exampleBin, map[string]string{
		// Point the client at the rogue token endpoint — same issuer,
		// but signed by an unpublished key.
		"OIDC_TOKEN_URL":     issuer.rogueTokenURL(),
		"OIDC_CLIENT_ID":     clientID,
		"OIDC_CLIENT_SECRET": clientSecret,
		"HARBOR_BASE_URL":    baseURL,
	})
	if err == nil {
		t.Fatalf("example attached with an unpublished-key token; the verifier must reject it.\noutput:\n%s", out)
	}
	if strings.Contains(out, "attach OK") {
		t.Fatalf("example reported attach OK on a token the verifier should reject;\noutput:\n%s", out)
	}
	// Pin the non-zero exit to the VERIFIER rejecting the attach, not an
	// unrelated upstream failure (token endpoint, decode, etc.): the rogue
	// token endpoint returns a token fine, so the failure must surface as a
	// 401 from runtime.info. Without this, a future regression breaking an
	// earlier step would keep this leg green while no longer exercising
	// signature rejection (§17.8: the fixture must distinguish right from wrong).
	if !strings.Contains(out, "runtime.info") || !strings.Contains(out, "HTTP 401") {
		t.Fatalf("example failed, but not via a runtime.info 401 — the leg did not exercise verifier rejection;\noutput:\n%s", out)
	}
}

// subcommandExists reports whether `bin <sub> --help` exits zero.
func subcommandExists(t *testing.T, bin, sub string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, sub, "--help").Run() == nil
}

type serveConfigParams struct {
	issuer   string
	audience string
	jwksURL  string
}

// writeServeConfig writes a minimal production `harbor serve` config into
// a temp dir, pointed at the mock issuer's JWKS. The LLM provider carries
// a LITERAL dummy key (not an env. reference) so the bifrost driver
// constructs at boot without a real provider — runtime.info never invokes
// the LLM, so no real completion is ever made.
func writeServeConfig(t *testing.T, p serveConfigParams) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [ES256]
  issuer: %q
  audience: %q
  jwks_url: %q
telemetry: {log_format: text, log_level: error, service_name: harbor-oidc-smoke}
state: {driver: inmem}
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-sonnet-4
  api_key: dummy-literal-key-not-used-by-runtime-info
  timeout: 60s
  context_window_reserve: 0.05
  model_profiles:
    anthropic/claude-sonnet-4:
      context_window_tokens: 200000
      token_estimator: chars_div_4
      json_schema_mode: native
      reasoning_effort: medium
governance: {repair_attempts: 1}
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions: {idle_ttl: 24h, hard_cap: 720h, sweep_interval: 15m}
artifacts: {driver: inmem, heavy_output_threshold_bytes: 32768}
tasks: {driver: inprocess, retain_turn_timeout: 5m, continuation_hop_limit: 8}
distributed: {bus_driver: loopback, remote_driver: loopback}
memory: {driver: inmem, strategy: none}
planner: {driver: react}
tools: {http_manifests: [], mcp_servers: []}
`, p.issuer, p.audience, p.jwksURL)

	path := filepath.Join(dir, "serve.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write serve config: %v", err)
	}
	return path
}

// bootServe starts `harbor serve` on an ephemeral loopback port, waits
// for the bound-port line on stderr, polls /healthz to readiness, and
// registers teardown. It returns the base URL.
func bootServe(t *testing.T, harborBin, cfgPath string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, harborBin, "serve", "--config", cfgPath, "--bind", "127.0.0.1:0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatalf("serve stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start harbor serve: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// Read stderr until the bound-port line appears, bounded by a real-
	// time deadline (no sleep-as-synchronisation — CLAUDE.md §17.4). The
	// scanned lines are tee'd into a buffer so a boot failure surfaces.
	boundCh := make(chan string, 1)
	var logBuf strings.Builder
	var logMu sync.Mutex
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			logMu.Lock()
			logBuf.WriteString(line)
			logBuf.WriteByte('\n')
			logMu.Unlock()
			if m := oidcBoundRe.FindStringSubmatch(line); m != nil {
				select {
				case boundCh <- m[1]:
				default:
				}
			}
		}
	}()

	var bound string
	select {
	case bound = <-boundCh:
	case <-time.After(30 * time.Second):
		logMu.Lock()
		logs := logBuf.String()
		logMu.Unlock()
		t.Fatalf("harbor serve did not report a bound port within 30s; stderr:\n%s", logs)
	}

	baseURL := "http://" + bound
	// Poll /healthz until 200 or a bounded deadline.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/healthz", nil)
		resp, herr := http.DefaultClient.Do(req)
		if herr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("harbor serve /healthz never returned 200 at %s", baseURL)
	return ""
}

// runExample executes the built example with the supplied environment and
// returns its combined output. A non-nil error means a non-zero exit.
func runExample(t *testing.T, exampleBin string, env map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exampleBin)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
