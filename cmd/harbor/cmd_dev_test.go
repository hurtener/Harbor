// cmd/harbor/cmd_dev_test.go — unit tests for the Phase 64 `harbor
// dev` subcommand's reachable helpers. The end-to-end wire-side boot
// is exercised by `test/integration/phase64_harbor_dev_test.go`; this
// file pins the pre-boot logic (the validateLLMProvider fail-loud, the
// HARBOR_BIND port parser, the dev signer + token mint flow, the
// boot-error → CLIError mapping).

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// TestValidateLLMProvider_NoMockEscape_Bifrost_RejectsEmptyProvider —
// constraint #2 fail-loud: driver=bifrost without a provider/model/
// api_key surfaces ErrLLMRequired naming the missing field.
func TestValidateLLMProvider_NoMockEscape_Bifrost_RejectsEmptyProvider(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "bifrost"}}
	err := validateLLMProvider(cfg, false)
	if !errors.Is(err, ErrLLMRequired) {
		t.Fatalf("validateLLMProvider() = %v; want errors.Is(err, ErrLLMRequired)", err)
	}
	if !contains(err.Error(), "llm.provider") {
		t.Errorf("error message %q missing 'llm.provider' named-field hint", err.Error())
	}
}

// TestValidateLLMProvider_NoMockEscape_Bifrost_AcceptsFullSpec —
// constraint #2 happy path: a full bifrost spec passes validation.
func TestValidateLLMProvider_NoMockEscape_Bifrost_AcceptsFullSpec(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{
		Driver:   "bifrost",
		Provider: "openrouter",
		Model:    "anthropic/claude-sonnet-4",
		APIKey:   "env.OPENROUTER_API_KEY",
	}}
	if err := validateLLMProvider(cfg, false); err != nil {
		t.Errorf("validateLLMProvider() = %v; want nil", err)
	}
}

// TestValidateLLMProvider_NoMockEscape_MockDriver_FailsLoud —
// constraint #2: driver=mock without HARBOR_DEV_ALLOW_MOCK=1 fails
// loud. This is the §13 "test stubs as production defaults" gate.
func TestValidateLLMProvider_NoMockEscape_MockDriver_FailsLoud(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "mock"}}
	err := validateLLMProvider(cfg, false)
	if !errors.Is(err, ErrLLMRequired) {
		t.Fatalf("validateLLMProvider() = %v; want ErrLLMRequired", err)
	}
	if !contains(err.Error(), EnvDevAllowMock) {
		t.Errorf("error message %q should mention the escape-hatch env var %q", err.Error(), EnvDevAllowMock)
	}
}

// TestValidateLLMProvider_MockEscape_ShortCircuits — when
// allowMock=true (HARBOR_DEV_ALLOW_MOCK=1), the function returns nil
// regardless of the driver knobs. The dev cmd's runtime path
// overrides driver to "mock" downstream.
func TestValidateLLMProvider_MockEscape_ShortCircuits(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "bifrost"}} // missing provider/model/api_key — but allowMock bypasses.
	if err := validateLLMProvider(cfg, true); err != nil {
		t.Errorf("validateLLMProvider(allowMock=true) = %v; want nil", err)
	}
}

// TestParsePortFromBind_Valid — HARBOR_BIND=host:port parses cleanly.
func TestParsePortFromBind_Valid(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:18080": 18080,
		"localhost:8080":  8080,
		// IPv6 bracketed form — uses LastIndex(':') so the trailing
		// port parses out cleanly.
		"[::1]:9090": 9090,
	}
	for bind, want := range cases {
		got, ok := parsePortFromBind(bind)
		if !ok {
			t.Errorf("parsePortFromBind(%q) ok=false; want true", bind)
			continue
		}
		if got != want {
			t.Errorf("parsePortFromBind(%q) = %d, want %d", bind, got, want)
		}
	}
}

// TestNewDevCmd_RegistersTUICoLaunchFlag — the one-command local-dev path
// (`harbor dev --tui`) must expose a boolean --tui flag that defaults off.
func TestNewDevCmd_RegistersTUICoLaunchFlag(t *testing.T) {
	cmd := newDevCmd()
	f := cmd.Flags().Lookup(flagDevTUI)
	if f == nil {
		t.Fatalf("dev command is missing the --%s flag", flagDevTUI)
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("--%s must be a bool flag, got %q", flagDevTUI, f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Fatalf("--%s must default to false, got %q", flagDevTUI, f.DefValue)
	}
}

// TestParsePortFromBind_Malformed — invalid bind strings return
// (0, false) so the caller keeps the supplied --port.
func TestParsePortFromBind_Malformed(t *testing.T) {
	cases := []string{
		"",
		"hostname",             // no colon
		"127.0.0.1:",           // trailing colon
		"127.0.0.1:notanumber", // non-numeric port
		"127.0.0.1:0",          // port 0 rejected (sentinel)
	}
	for _, bind := range cases {
		if _, ok := parsePortFromBind(bind); ok {
			t.Errorf("parsePortFromBind(%q) ok=true; want false", bind)
		}
	}
}

// TestNewDevSigner_GeneratesDistinctKeysAcrossCalls — each
// newDevSigner() mints a fresh keypair. Two consecutive calls produce
// keypairs that do NOT cross-validate, so a leaked token from one
// dev session cannot be replayed against a later session.
func TestNewDevSigner_GeneratesDistinctKeysAcrossCalls(t *testing.T) {
	a, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	b, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	// The X coordinates of the two public keys MUST differ — the
	// generator is sourced from crypto/rand, so a collision is
	// vanishingly unlikely (lottery-ticket math).
	if a.priv.X.Cmp(b.priv.X) == 0 {
		t.Error("two newDevSigner() calls produced the same public-key X — generator looks deterministic")
	}
}

// TestSignDevToken_ProducesParseableJWT — the minted token round-trips
// through the JWT parser: header has kid=harbor-dev, alg=ES256,
// claims have the supplied identity triple + scopes.
func TestSignDevToken_ProducesParseableJWT(t *testing.T) {
	s, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	tok, err := s.SignDevToken(now, "t1", "u1", "s1", []string{"admin"})
	if err != nil {
		t.Fatalf("SignDevToken: %v", err)
	}
	if tok == "" {
		t.Fatal("SignDevToken returned empty token")
	}
	// JWT structure: three '.'-separated base64 segments.
	if countDots(tok) != 2 {
		t.Errorf("token does not look like a JWT (3 segments): %q", tok)
	}
}

// TestSignDevToken_IncompleteIdentity_FailsLoud — constraint: identity
// triple is mandatory; missing component fails closed.
func TestSignDevToken_IncompleteIdentity_FailsLoud(t *testing.T) {
	s, _ := newDevSigner()
	now := time.Now()
	cases := [][3]string{
		{"", "u", "s"},
		{"t", "", "s"},
		{"t", "u", ""},
	}
	for _, c := range cases {
		_, err := s.SignDevToken(now, c[0], c[1], c[2], nil)
		if err == nil {
			t.Errorf("SignDevToken(%q, %q, %q) returned nil err; want non-nil", c[0], c[1], c[2])
		}
	}
}

// TestBootErrorToCLIError_MapsKnownSentinels — the mapping from
// boot-time errors onto CLIError codes is stable. New error classes
// added to the mapping must extend this table.
func TestBootErrorToCLIError_MapsKnownSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"llm_required", ErrLLMRequired, CodeBootLLMRequired},
		{"config_not_found", config.ErrConfigNotFound, CodeBootConfigInvalid},
		{"config_invalid", config.ErrConfigInvalid, CodeBootConfigInvalid},
		{"unknown", errors.New("anything else"), CodeBootInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := bootErrorToCLIError("dev", tc.err)
			if cli.Code != tc.want {
				t.Errorf("Code = %q, want %q (input: %v)", cli.Code, tc.want, tc.err)
			}
		})
	}
}

// contains is the stdlib-free substring helper used by the
// fail-loud message assertions above.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// countDots is the JWT-shape assertion helper.
func countDots(s string) int {
	n := 0
	for _, c := range s {
		if c == '.' {
			n++
		}
	}
	return n
}

// bootDevStackBusWiredYAML is the minimal config TestBootDevStack_*
// fixtures consume. Driver knobs match `examples/dev.yaml` shape
// (validated by config.Load) but everything is in-memory so the
// test stays hermetic.
const bootDevStackBusWiredYAML = `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms:
    - ES256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-test
state:
  driver: inmem
llm:
  driver: mock
  timeout: 30s
  context_window_reserve: 0.05
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
`

// bootDevForTest boots the dev-only serve composition (ephemeral signer +
// drafts/bootstrap routes + mock LLM) against the minimal bus-wired YAML and
// returns the promoted serve.Handle. Cleanup drains the stack.
func bootDevForTest(t *testing.T, ctx context.Context, serveConsole bool) *serve.Handle {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	comp, err := newDevComposition(devCompositionOptions{allowMock: true, serveConsole: serveConsole})
	if err != nil {
		t.Fatalf("newDevComposition: %v", err)
	}
	handle, err := serve.Boot(ctx, comp.serveOptions(cfgPath, 0, "", "dev", logger, io.Discard))
	if err != nil {
		t.Fatalf("serve.Boot (dev composition, serveConsole=%v): %v", serveConsole, err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		handle.Close(closeCtx)
	})
	return handle
}

// TestDevComposition_BootstrapEndpointRegistered_HarborDev — the dev
// composition mounts POST /v1/dev/bootstrap.json; a loopback peer receives a
// 200 + a connection envelope carrying the dev identity triple.
func TestDevComposition_BootstrapEndpointRegistered_HarborDev(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle := bootDevForTest(t, ctx, false /* serveConsole */)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handle.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from bootstrap on harbor dev, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		BaseURL  string                                 `json:"base_url"`
		Token    string                                 `json:"token"`
		Identity struct{ Tenant, User, Session string } `json:"identity"`
		Scopes   []string                               `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode bootstrap response: %v\nbody=%s", err, rec.Body.String())
	}
	if body.Token == "" {
		t.Error("bootstrap response token is empty")
	}
	if body.Identity.Tenant != DevTenant || body.Identity.User != DevUser || body.Identity.Session != DevSession {
		t.Errorf("identity = %+v, want (%s,%s,%s)", body.Identity, DevTenant, DevUser, DevSession)
	}
	if len(body.Scopes) == 0 {
		t.Error("bootstrap response scopes is empty")
	}
}

// TestDevComposition_BootstrapEndpointRegistered_HarborConsole — the console
// composition (serveConsole=true) mounts the bootstrap endpoint identically.
func TestDevComposition_BootstrapEndpointRegistered_HarborConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle := bootDevForTest(t, ctx, true /* serveConsole */)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handle.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from bootstrap on harbor console, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevComposition_BootstrapEndpoint_NonLoopback_Returns403 — the loopback
// gate rejects a non-loopback peer even though the route is registered; a
// spoofed X-Forwarded-For is ignored (the gate reads r.RemoteAddr directly).
func TestDevComposition_BootstrapEndpoint_NonLoopback_Returns403(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle := bootDevForTest(t, ctx, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "192.168.1.5:54321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	handle.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback peer, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevComposition_DraftEndpointMounted — the dev composition mounts the
// draft-scaffolding handler under devdraft.RoutePrefix. Without a token the
// auth middleware rejects the request (401), which still proves the route is
// mounted (a 404 would mean the dev seam did not fire).
func TestDevComposition_DraftEndpointMounted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle := bootDevForTest(t, ctx, false)

	req := httptest.NewRequest(http.MethodGet, devdraft.RoutePrefix+"/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handle.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("draft endpoint returned 404 — the dev route seam did not mount it")
	}
}

// TestServeComposition_BootstrapEndpoint_404 — the production serve posture
// composes NO dev seams (no ExtraRoutes), so the dev-only bootstrap endpoint
// is NOT mounted: a POST returns 404. This is the caller-level half of the
// posture split (the dev surfaces answer under dev, 404 under serve).
func TestServeComposition_BootstrapEndpoint_404(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// A production-shaped validator source with NO dev seams. We reuse an
	// ephemeral signer only as a KeySet provider — the point is that no
	// ExtraRoutes / BuildAuthSurface are composed, so no dev surface mounts.
	signer, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	handle, err := serve.Boot(ctx, serve.Options{
		ConfigPath:      cfgPath,
		Logger:          logger,
		Stderr:          io.Discard,
		SubcommandLabel: "serve",
		AuthValidatorFactory: func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
			return auth.NewValidator(signer.KeySet(), auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
		},
		MCPDefaultIdentity: identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession},
		DisplayName:        "harbor dev",
		InstanceID:         devInstanceID(),
		BuildVersion:       HarborVersion,
		BuildCommit:        "dev",
		BuildLLMSnapshot:   newLLMSnapshotBuilder(true),
	})
	if err != nil {
		t.Fatalf("serve.Boot (serve composition): %v", err)
	}
	t.Cleanup(func() {
		closeCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		handle.Close(closeCtx)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handle.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for the dev-only bootstrap endpoint under the serve composition, got %d", rec.Code)
	}
}

// TestServeComposition_NilFactory_FailsLoud — Boot with a nil auth-validator
// factory is a loud construction error (identity is mandatory), never an
// unauthenticated listener.
func TestServeComposition_NilFactory_FailsLoud(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	_, err := serve.Boot(context.Background(), serve.Options{
		ConfigPath: cfgPath,
		Stderr:     io.Discard,
	})
	if !errors.Is(err, serve.ErrAuthValidatorFactoryRequired) {
		t.Fatalf("expected ErrAuthValidatorFactoryRequired, got %v", err)
	}
}

// devServeOptionsForTest builds a dev-composition serve.Options (ephemeral
// signer + mock LLM) against the given config path. The captured signer is
// stable across a supervisor's reboots, so the printed token stays valid.
func devServeOptionsForTest(t *testing.T, cfgPath string, logger *slog.Logger) serve.Options {
	t.Helper()
	comp, err := newDevComposition(devCompositionOptions{allowMock: true})
	if err != nil {
		t.Fatalf("newDevComposition: %v", err)
	}
	return comp.serveOptions(cfgPath, 0, "", "dev", logger, io.Discard)
}

// writeBindAddrCfg writes the minimal bus-wired YAML with the given
// server.bind_addr substituted in.
func writeBindAddrCfg(t *testing.T, bindAddr string) string {
	t.Helper()
	yaml := strings.Replace(bootDevStackBusWiredYAML, "bind_addr: 127.0.0.1:0", "bind_addr: "+bindAddr, 1)
	cfgPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfgPath
}

// TestDevComposition_HonorsPortFlag_IgnoresConfigBindAddr is the regression
// pin for the bind-address discriminator: a dev boot against a serve-shaped
// yaml (a NON-loopback `server.bind_addr`) must resolve the caller's
// loopback 127.0.0.1:<port> — never the config address. Pre-promotion this
// was gated on the production factory being non-nil; with the factory now
// mandatory for all callers the gate is the explicit PreferConfigBindAddr
// opt-in the dev composition never sets.
func TestDevComposition_HonorsPortFlag_IgnoresConfigBindAddr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfgPath := writeBindAddrCfg(t, "0.0.0.0:8080")
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	comp, err := newDevComposition(devCompositionOptions{allowMock: true})
	if err != nil {
		t.Fatalf("newDevComposition: %v", err)
	}
	// The operator passed --port 19999; the config says 0.0.0.0:8080.
	handle, err := serve.Boot(ctx, comp.serveOptions(cfgPath, 19999, "", "dev", logger, io.Discard))
	if err != nil {
		t.Fatalf("serve.Boot (dev composition): %v", err)
	}
	t.Cleanup(func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		handle.Close(cc)
	})
	if got := handle.BindAddr(); got != "127.0.0.1:19999" {
		t.Fatalf("dev bind address = %q, want 127.0.0.1:19999 (--port honored; non-loopback config bind_addr ignored)", got)
	}
}

// TestDevComposition_LiveListenerStaysLoopback_NonLoopbackConfig proves the
// regression live: a dev boot against a non-loopback config bind_addr BINDS
// a loopback listener (never 0.0.0.0 — the dev-token stack must not be
// exposed off-box).
func TestDevComposition_LiveListenerStaysLoopback_NonLoopbackConfig(t *testing.T) {
	cfgPath := writeBindAddrCfg(t, "0.0.0.0:8080")
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	pr, pw := io.Pipe()
	comp, err := newDevComposition(devCompositionOptions{allowMock: true})
	if err != nil {
		t.Fatalf("newDevComposition: %v", err)
	}
	opts := comp.serveOptions(cfgPath, 0, "", "dev", logger, pw)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := serve.Boot(ctx, opts)
	if err != nil {
		t.Fatalf("serve.Boot: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- handle.Serve(ctx) }()

	boundCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "HARBOR_DEV_BOUND=") {
				boundCh <- strings.TrimPrefix(sc.Text(), "HARBOR_DEV_BOUND=")
				return
			}
		}
		boundCh <- ""
	}()

	var bound string
	select {
	case bound = <-boundCh:
	case <-time.After(10 * time.Second):
		t.Fatal("HARBOR_DEV_BOUND never printed")
	}
	if !strings.HasPrefix(bound, "127.0.0.1:") {
		t.Fatalf("dev listener bound %q — MUST stay loopback despite the non-loopback config bind_addr", bound)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
	_ = pw.Close()
	cc, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	handle.Close(cc)
}

// TestServeComposition_HonorsConfigBindAddr — the production serve posture
// (PreferConfigBindAddr set by cmd_serve.go) resolves the operator-configured
// `server.bind_addr` when no override is given.
func TestServeComposition_HonorsConfigBindAddr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfgPath := writeBindAddrCfg(t, "127.0.0.1:18443")
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	signer, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	handle, err := serve.Boot(ctx, serve.Options{
		ConfigPath:           cfgPath,
		Port:                 19998, // the config bind_addr must win over the port fallback
		Logger:               logger,
		Stderr:               io.Discard,
		SubcommandLabel:      "serve",
		PreferConfigBindAddr: true,
		AuthValidatorFactory: func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
			return auth.NewValidator(signer.KeySet(), auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
		},
		MCPDefaultIdentity: identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession},
		DisplayName:        "harbor serve",
		InstanceID:         serve.InstanceID("harbor-serve"),
		BuildVersion:       HarborVersion,
		BuildCommit:        "dev",
		BuildLLMSnapshot:   newLLMSnapshotBuilder(true),
	})
	if err != nil {
		t.Fatalf("serve.Boot (serve composition): %v", err)
	}
	t.Cleanup(func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		handle.Close(cc)
	})
	if got := handle.BindAddr(); got != "127.0.0.1:18443" {
		t.Fatalf("serve bind address = %q, want the config's 127.0.0.1:18443 (PreferConfigBindAddr)", got)
	}
}
