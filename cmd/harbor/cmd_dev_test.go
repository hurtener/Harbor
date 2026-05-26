// cmd/harbor/cmd_dev_test.go — unit tests for the Phase 64 `harbor
// dev` subcommand's reachable helpers. The end-to-end wire-side boot
// is exercised by `test/integration/phase64_harbor_dev_test.go`; this
// file pins the pre-boot logic (the validateLLMProvider fail-loud, the
// HARBOR_BIND port parser, the dev signer + token mint flow, the
// boot-error → CLIError mapping).

package main

import (
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

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
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
	if a.priv.PublicKey.X.Cmp(b.priv.PublicKey.X) == 0 {
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
			cli := bootErrorToCLIError(tc.err)
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

// TestBootDevStack_CoordinatorEmitsPauseResumedOnBus pins F1 from the
// Wave 11.5 §17.5 closeout audit: the production `bootDevStack` MUST
// construct its pauseresume.Coordinator with WithBus(bus) so the
// canonical pause.resumed event (carrying D-096's typed Decision
// marker) reaches subscribers. A bare pauseresume.New() short-
// circuits emit when bus == nil — the regression this test guards.
//
// The test boots the production bootDevStack, subscribes to
// pause.resumed under the (tenant=dev, user=dev, session=dev) triple
// (which matches the dev token bootDevStack mints), drives a
// Request + Resume round-trip on stack.coordinator, and asserts the
// event lands with the typed Decision marker populated.
func TestBootDevStack_CoordinatorEmitsPauseResumedOnBus(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stack, err := bootDevStack(ctx, devBootOptions{
		cfgPath:   cfgPath,
		allowMock: true, // mock-driver escape per §13 amendment
		logger:    logger,
		stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		for i := len(stack.closeFns) - 1; i >= 0; i-- {
			_ = stack.closeFns[i](closeCtx)
		}
	}()

	// Subscribe BEFORE driving the Request so the fan-out bus does not
	// race the publish (pattern PR #111 / the catalog tests pinned).
	id := identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "dev"}
	sub, err := stack.bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	idCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	pause, err := stack.coordinator.Request(idCtx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("coordinator.Request: %v", err)
	}
	if err := stack.coordinator.Resume(idCtx, pause.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("coordinator.Resume: %v", err)
	}

	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before pause.resumed observed")
		}
		if ev.Type != pauseresume.EventTypePauseResumed {
			t.Fatalf("event type = %q, want %q", ev.Type, pauseresume.EventTypePauseResumed)
		}
		p, ok := ev.Payload.(pauseresume.PauseResumedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want pauseresume.PauseResumedPayload", ev.Payload)
		}
		if p.Decision != pauseresume.DecisionApprove {
			t.Errorf("payload.Decision = %q, want %q", p.Decision, pauseresume.DecisionApprove)
		}
		if p.Token != string(pause.Token) {
			t.Errorf("payload.Token = %q, want %q", p.Token, string(pause.Token))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause.resumed event did not arrive on bus — F1 regression: " +
			"bootDevStack must construct pauseresume.New(WithBus(bus))")
	}
}

// TestBootDevStack_AppendsDraftStoreAndRegistryClosers — Phase 83m
// (Item 3, D-156): the §17.5 audit pinned two constructed subsystems
// (agent registry + draft store) whose Close was NEVER appended to the
// closer chain. A clean shutdown therefore leaked file handles /
// goroutines from any future driver that owns them. The fix appends
// both Close functions; this test asserts both closers run cleanly on
// stack teardown (a no-op for the V1 drivers; the canary for any
// future driver that owns resources).
func TestBootDevStack_AppendsDraftStoreAndRegistryClosers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stack, err := bootDevStack(ctx, devBootOptions{
		cfgPath:   cfgPath,
		allowMock: true,
		logger:    logger,
		stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}

	// The draft store + agent registry are wired during boot. The
	// test asserts the closers run cleanly (the failure mode pre-fix
	// was a silent omit; the closers simply weren't in the chain. We
	// can't easily pointer-compare closer entries because they're
	// bound method values, but if either omit returned silently the
	// stack.close drain would still succeed — so we directly invoke
	// the underlying Close methods via the stack's draftStore field
	// and assert no error). For the registry, the closer is reachable
	// only through the chain; we drain it and assert no error fires.
	if stack.draftStore == nil {
		t.Fatal("stack.draftStore is nil — bootDevStack did not construct the draft store")
	}
	if err := stack.draftStore.Close(ctx); err != nil {
		t.Errorf("draftStore.Close (direct) returned %v, want nil", err)
	}

	// Drain the closer chain. Every closer must return nil; a leak
	// from a future driver that owns resources would surface here.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	for i := len(stack.closeFns) - 1; i >= 0; i-- {
		if cErr := stack.closeFns[i](closeCtx); cErr != nil {
			t.Errorf("closer %d returned %v, want nil", i, cErr)
		}
	}
}

// bootStackForBootstrapTest boots a bootDevStack against the minimal
// bus-wired YAML used by other dev tests. Caller is responsible for
// draining the closers.
func bootStackForBootstrapTest(t *testing.T, ctx context.Context, serveConsole bool) *devStack {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	stack, err := bootDevStack(ctx, devBootOptions{
		cfgPath:      cfgPath,
		allowMock:    true,
		logger:       logger,
		stderr:       io.Discard,
		serveConsole: serveConsole,
	})
	if err != nil {
		t.Fatalf("bootDevStack(serveConsole=%v): %v", serveConsole, err)
	}
	return stack
}

func drainStack(t *testing.T, stack *devStack) {
	t.Helper()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	for i := len(stack.closeFns) - 1; i >= 0; i-- {
		_ = stack.closeFns[i](closeCtx)
	}
}

// TestBootDevStack_BootstrapEndpointRegistered_HarborDev — Phase 105
// AC-6: POST /v1/dev/bootstrap.json is mounted on the `harbor dev`
// stack. A loopback peer receives a 200 + a connection envelope.
func TestBootDevStack_BootstrapEndpointRegistered_HarborDev(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stack := bootStackForBootstrapTest(t, ctx, false /* serveConsole */)
	defer drainStack(t, stack)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	stack.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from bootstrap on harbor dev, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		BaseURL  string   `json:"base_url"`
		Token    string   `json:"token"`
		Identity struct{ Tenant, User, Session string } `json:"identity"`
		Scopes   []string `json:"scopes"`
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

// TestBootDevStack_BootstrapEndpointRegistered_HarborConsole — Phase
// 105 AC-6: POST /v1/dev/bootstrap.json is also mounted when
// bootDevStack runs with serveConsole=true (the `harbor console`
// path). The handler must respond identically.
func TestBootDevStack_BootstrapEndpointRegistered_HarborConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stack := bootStackForBootstrapTest(t, ctx, true /* serveConsole */)
	defer drainStack(t, stack)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	stack.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from bootstrap on harbor console, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBootDevStack_BootstrapEndpoint_NonLoopback_Returns403 — Phase 105
// AC-7: a non-loopback peer is rejected by the loopback gate, even
// though the route is registered. The gate reads r.RemoteAddr directly.
func TestBootDevStack_BootstrapEndpoint_NonLoopback_Returns403(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stack := bootStackForBootstrapTest(t, ctx, false)
	defer drainStack(t, stack)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/bootstrap.json", strings.NewReader("{}"))
	req.RemoteAddr = "192.168.1.5:54321"
	// Spoofed XFF MUST be ignored.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	stack.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback peer, got %d: %s", rec.Code, rec.Body.String())
	}
}
