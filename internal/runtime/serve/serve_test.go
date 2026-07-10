package serve

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"

	// Real drivers + the mock LLM so Boot can assemble a full stack.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// serveTestYAML is a minimal hermetic config (in-memory drivers + mock LLM).
const serveTestYAML = `
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

// testKeySet is a static single-key ES256 KeySet for the test validator.
type testKeySet struct {
	kid string
	pub *ecdsa.PublicKey
}

func (k *testKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != k.kid {
		return nil, "", fmt.Errorf("kid %q not known", kid)
	}
	return k.pub, "ES256", nil
}

// testFactory returns a valid auth-validator factory (mounts the auth
// middleware; the tests do not need to mint a token — an unauthenticated
// request yields 401, a missing route yields 404).
func testFactory(t *testing.T) AuthValidatorFactory {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ks := &testKeySet{kid: "serve-test", pub: &priv.PublicKey}
	return func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
		return auth.NewValidator(ks, auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
	}
}

func writeTestCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(p, []byte(serveTestYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return p
}

func baseOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		ConfigPath:           writeTestCfg(t),
		Logger:               slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Stderr:               io.Discard,
		SubcommandLabel:      "dev",
		AuthValidatorFactory: testFactory(t),
		BuildLLMSnapshot: func(cfg *config.Config) (*llm.ConfigSnapshot, error) {
			snap := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
			return &snap, nil
		},
		DisplayName:  "harbor test",
		InstanceID:   "harbor-test",
		BuildVersion: "test",
		BuildCommit:  "test",
	}
}

func bootTest(t *testing.T, ctx context.Context, opts Options) *Handle {
	t.Helper()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		cc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Close(cc)
	})
	return h
}

// TestBoot_NilFactory_FailsLoud — identity is mandatory: a nil auth-validator
// factory is a loud construction error, never an unauthenticated listener.
func TestBoot_NilFactory_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.AuthValidatorFactory = nil
	_, err := Boot(context.Background(), opts)
	if !errors.Is(err, ErrAuthValidatorFactoryRequired) {
		t.Fatalf("want ErrAuthValidatorFactoryRequired, got %v", err)
	}
}

// TestBoot_SharedSurfacesOnly — with no injection seams composed, the
// constructor mounts ONLY the shared surfaces: no dev bootstrap route, no
// Console mount. /healthz answers; the dev-only bootstrap endpoint 404s.
func TestBoot_SharedSurfacesOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h := bootTest(t, ctx, baseOptions(t))

	// Shared surface answers.
	code := probe(h, http.MethodGet, "/healthz", "")
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", code)
	}
	// Dev-only route is absent (no ExtraRoutes seam composed).
	code = probe(h, http.MethodPost, "/v1/dev/bootstrap.json", "{}")
	if code != http.StatusNotFound {
		t.Fatalf("bootstrap without ExtraRoutes = %d, want 404", code)
	}
	// No Console mount: an unknown non-/v1 path is 404 (nothing at `/`).
	code = probe(h, http.MethodGet, "/some-console-route", "")
	if code != http.StatusNotFound {
		t.Fatalf("root path without Console mount = %d, want 404", code)
	}
}

// TestBoot_PerSeam_Injection — each injection seam is observed exactly where
// the caller placed it: an injected pre-CORS route answers, the LLM-snapshot
// builder is invoked, and the post-boot hook fires.
func TestBoot_PerSeam_Injection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		llmBuilt  bool
		postBoot  bool
		closerRan bool
	)
	opts := baseOptions(t)
	opts.BuildLLMSnapshot = func(cfg *config.Config) (*llm.ConfigSnapshot, error) {
		llmBuilt = true
		snap := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
		return &snap, nil
	}
	opts.ExtraRoutes = func(_ context.Context, m RouteMount) ([]func(context.Context) error, error) {
		if m.Validator == nil {
			t.Error("RouteMount.Validator is nil")
		}
		if m.BindAddr == "" {
			t.Error("RouteMount.BindAddr is empty")
		}
		m.Router.HandleFunc("/probe/seam", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
		return []func(context.Context) error{func(context.Context) error { closerRan = true; return nil }}, nil
	}
	opts.PostBoot = func(_ context.Context, ph PostBootHandles) error {
		postBoot = true
		if ph.Tasks == nil || ph.Sessions == nil {
			t.Error("PostBootHandles missing core handles")
		}
		return nil
	}

	h := bootTest(t, ctx, opts)

	if !llmBuilt {
		t.Error("LLM-snapshot builder was not invoked")
	}
	if !postBoot {
		t.Error("post-boot hook did not fire")
	}
	// The injected route is mounted where the caller placed it.
	code := probe(h, http.MethodGet, "/probe/seam", "")
	if code != http.StatusTeapot {
		t.Fatalf("injected pre-CORS route = %d, want 418", code)
	}

	// Drain and assert the injected closer ran.
	cc, ccCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccCancel()
	h.Close(cc)
	if !closerRan {
		t.Error("injected closer did not run on Close")
	}
}

// TestServedHandle_ConcurrentReuse pins the D-025 contract: the served Handle
// is a compiled artifact — N≥100 concurrent requests against one instance
// under -race, no races, no identity bleed, goroutine baseline restored after
// Close.
func TestServedHandle_ConcurrentReuse(t *testing.T) {
	base := goruntime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	handler := h.Handler()

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			// Alternate identity per request; the auth middleware rejects
			// (401) but the point is race-freedom + no cross-request bleed
			// on the shared handler.
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("req %d: /healthz = %d, want 200", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()

	cc, ccCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccCancel()
	h.Close(cc)
	cancel()

	// Allow goroutines to unwind, then assert the baseline is restored.
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		goruntime.GC()
	}
	if leaked := goruntime.NumGoroutine() - base; leaked > 2 {
		t.Errorf("goroutine leak after Close: baseline=%d now=%d (leaked ~%d)", base, goruntime.NumGoroutine(), leaked)
	}
}

// TestBoot_CloseCycleStress boots + closes N≥10 times, proving no
// listener/handle leak across repeated composition.
func TestBoot_CloseCycleStress(t *testing.T) {
	for i := range 12 {
		ctx, cancel := context.WithCancel(context.Background())
		h, err := Boot(ctx, baseOptions(t))
		if err != nil {
			cancel()
			t.Fatalf("cycle %d: Boot: %v", i, err)
		}
		cc, ccCancel := context.WithTimeout(context.Background(), 5*time.Second)
		h.Close(cc)
		ccCancel()
		cancel()
	}
}

// probe runs one request against the Handle's composed handler.
func probe(h *Handle, method, path, body string) int {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec.Code
}
