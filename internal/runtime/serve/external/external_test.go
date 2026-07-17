package external_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/serve/external"
	"github.com/hurtener/Harbor/internal/tools"
)

const externalYAML = `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 2s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: JWKS_FILE
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-external-test
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
  subscriber_buffer_size: 128
  idle_timeout: 30s
  drop_window: 1s
  replay_buffer_size: 256
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

func writeJWKS(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	set := map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "external-test",
		"n": b64(priv.N.Bytes()),
		"e": b64(big.NewInt(int64(priv.E)).Bytes()),
	}}}
	raw, _ := json.Marshal(set)
	p := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return p
}

func externalConfig(t *testing.T) *config.Config {
	t.Helper()
	yaml := strings.Replace(externalYAML, "JWKS_FILE", writeJWKS(t), 1)
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yaml))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	return cfg
}

func TestOpen_ConfigRequired(t *testing.T) {
	_, err := external.Open(context.Background(), nil, "", nil)
	if !errors.Is(err, external.ErrConfigRequired) {
		t.Fatalf("want ErrConfigRequired, got %v", err)
	}
}

func TestOpen_Success_RegistrarRuns(t *testing.T) {
	cfg := externalConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var count int
	h, err := external.Open(ctx, cfg, "", func(cat tools.ToolCatalog) error {
		count++
		_ = cat
		return nil
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if count != 1 {
		t.Fatalf("registrar invoked %d times, want 1", count)
	}
	if h.BindAddr() == "" {
		t.Error("BindAddr empty before Serve")
	}
	// Concurrent BindAddr reads on the shared handle race-free.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = h.BindAddr() }()
	}
	wg.Wait()

	// Serve binds a real listener and answers /healthz; it returns when
	// ctx cancels (bounded eventually-poll for the bound port).
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = h.Serve(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	served := false
	for time.Now().Before(deadline) {
		addr := h.BindAddr()
		if addr != "" && !strings.HasSuffix(addr, ":0") {
			resp, gErr := http.Get("http://" + addr + "/healthz")
			if gErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					served = true
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !served {
		t.Error("Serve never answered /healthz within the deadline")
	}
	cancel()
	select {
	case <-serveDone:
	case <-time.After(10 * time.Second):
		t.Error("Serve did not return after ctx cancel")
	}

	cc, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	if err := h.Close(cc); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent second Close.
	if err := h.Close(cc); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpen_MissingJWKS_NamesField(t *testing.T) {
	cfg := externalConfig(t)
	cfg.Identity.JWKSFile = ""
	_, err := external.Open(context.Background(), cfg, "", nil)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("want a loud identity-field error, got %v", err)
	}
}

// TestExternal_NoDevSurfaces_NoInjectionSeams is the production-posture
// grep gate on the INTERNAL serving band (the sibling of the sdk/server
// facade gate): this package is where a dev seam would actually seat, so
// its production source must not reference the serve band's dev-only
// injection seams, a dev signer, or a mock knob. The production JWKS
// factory (serve.NewJWKSAuthValidatorFactory) is the one legitimate
// auth-factory reference.
func TestExternal_NoDevSurfaces_NoInjectionSeams(t *testing.T) {
	forbidden := []string{
		"BuildAuthSurface", "BuildLLMSnapshot", "ExtraRoutes", "PostBoot",
		"devSigner", "DevAllowMock", "allowMock", "HARBOR_DEV_ALLOW_MOCK",
		"bootstrap",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rErr := os.ReadFile(name)
		if rErr != nil {
			t.Fatalf("read %s: %v", name, rErr)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s references forbidden dev/injection identifier %q — the external serving band is production-only", name, bad)
			}
		}
	}
}

func TestOpen_FromPath_LoadsValidatesBoots(t *testing.T) {
	yaml := strings.Replace(externalYAML, "JWKS_FILE", writeJWKS(t), 1)
	yamlPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, nil, yamlPath, nil)
	if err != nil {
		t.Fatalf("Open from path: %v", err)
	}
	cc, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	if err := h.Close(cc); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpen_WaitReady_ReturnsBoundAddress is the AC1 gate: WaitReady
// returns the actual OS-assigned address after Serve binds an ephemeral
// port, with no polling or second listener lifecycle. The address
// WaitReady returns is the one a co-launched client dials.
func TestOpen_WaitReady_ReturnsBoundAddress(t *testing.T) {
	cfg := externalConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, cfg, "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = h.Close(cc)
	}()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readyCancel()
	go func() { _ = h.Serve(ctx) }()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("WaitReady returned empty or unbound address %q", addr)
	}
	// The address WaitReady returned is dialable.
	resp, gErr := http.Get("http://" + addr + "/healthz")
	if gErr != nil {
		t.Fatalf("dial WaitReady address %s: %v", addr, gErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz on %s: status %d", addr, resp.StatusCode)
	}
	cancel()
}

// TestOpen_WaitReady_ConcurrentWaiters_RaceFree proves the one-shot
// readiness is race-safe: N goroutines all calling WaitReady against one
// shared handle receive the same bound address with no data race. The
// -race detector is the gate.
func TestOpen_WaitReady_ConcurrentWaiters_RaceFree(t *testing.T) {
	cfg := externalConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, cfg, "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	const N = 100
	var wg sync.WaitGroup
	addrs := make([]string, N)
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
			defer rc()
			a, wErr := h.WaitReady(readyCtx)
			if wErr != nil {
				t.Errorf("waiter %d: %v", idx, wErr)
				return
			}
			addrs[idx] = a
		}(i)
	}
	wg.Wait()
	cancel()
	first := addrs[0]
	if first == "" {
		t.Fatal("first waiter got empty address")
	}
	for i, a := range addrs {
		if a != first {
			t.Errorf("waiter %d got %q, want %q (first waiter)", i, a, first)
		}
	}
}

// TestOpen_WaitReady_LateWaiter_ReadsBufferedValue proves a caller that
// calls WaitReady AFTER the listener has already bound receives the
// buffered readiness value immediately (no blocking, no polling).
func TestOpen_WaitReady_LateWaiter_ReadsBufferedValue(t *testing.T) {
	cfg := externalConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, cfg, "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	// Wait for the first readiness to fire.
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr1, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("first WaitReady: %v", err)
	}
	// A late waiter should get the same value immediately.
	lateCtx, lc := context.WithTimeout(context.Background(), 2*time.Second)
	defer lc()
	addr2, err := h.WaitReady(lateCtx)
	if err != nil {
		t.Fatalf("late WaitReady: %v", err)
	}
	if addr2 != addr1 {
		t.Fatalf("late waiter got %q, want %q", addr2, addr1)
	}
	cancel()
}

// TestOpen_WaitReady_BindFailure_ReturnsError proves WaitReady returns
// the bind error when the listener fails to bind (a port already in use
// by a held listener). The one-shot signal carries the error.
func TestOpen_WaitReady_BindFailure_ReturnsError(t *testing.T) {
	cfg := externalConfig(t)
	// Hold a listener on the same port to force a bind failure.
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold listen: %v", err)
	}
	defer hold.Close()
	cfg.Server.BindAddr = hold.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, cfg, "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(context.Background(), 5*time.Second)
	defer rc()
	_, wErr := h.WaitReady(readyCtx)
	if wErr == nil {
		t.Fatal("WaitReady succeeded on a bind that should have failed")
	}
	cancel()
}

// TestOpen_WaitReady_CancelledCtx_ReturnsCtxErr proves WaitReady returns
// ctx.Err() when the caller's ctx cancels before the listener binds.
func TestOpen_WaitReady_CancelledCtx_ReturnsCtxErr(t *testing.T) {
	cfg := externalConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := external.Open(ctx, cfg, "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = h.Close(cc)
	}()
	readyCtx, readyCancel := context.WithCancel(context.Background())
	readyCancel() // cancel immediately
	_, wErr := h.WaitReady(readyCtx)
	if wErr == nil {
		t.Fatal("WaitReady succeeded on an already-cancelled ctx")
	}
}
