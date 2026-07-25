package serve

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
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

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"

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

// testSigner pairs an ephemeral ES256 keypair with a validator factory and a
// token-mint helper, so tests can drive AUTHENTICATED requests under
// arbitrary identity triples through the composed handler.
type testSigner struct {
	priv *ecdsa.PrivateKey
	kid  string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &testSigner{priv: priv, kid: "serve-test"}
}

// factory returns the auth-validator factory backed by this signer's key.
func (s *testSigner) factory() AuthValidatorFactory {
	ks := &testKeySet{kid: s.kid, pub: &s.priv.PublicKey}
	return func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
		return auth.NewValidator(ks, auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
	}
}

// sign mints a Bearer JWT for the identity triple + scopes.
func (s *testSigner) sign(t *testing.T, id identity.Identity, scopes []string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub":     id.UserID,
		"exp":     now.Add(time.Hour).Unix(),
		"nbf":     now.Add(-time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	})
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signed
}

// testFactory returns a valid auth-validator factory (mounts the auth
// middleware; callers that need to MINT tokens use newTestSigner instead).
func testFactory(t *testing.T) AuthValidatorFactory {
	t.Helper()
	return newTestSigner(t).factory()
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

// TestBoot_NilValidatorFromFactory_FailsLoud — identity is mandatory: a
// factory that hands back a nil Validator with a nil error is a loud
// construction error too. BuildMux reads a nil Validator as the test-kit
// WithoutValidator() opt-out, so Boot refuses it at construction.
// Identity is mandatory.
func TestBoot_NilValidatorFromFactory_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.AuthValidatorFactory = func(_ context.Context, _ *config.Config, _ audit.Redactor, _ events.EventBus, _ *slog.Logger) (auth.Validator, error) {
		return nil, nil
	}
	_, err := Boot(context.Background(), opts)
	if !errors.Is(err, ErrAuthValidatorRequired) {
		t.Fatalf("want ErrAuthValidatorRequired, got %v", err)
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

// identityEchoIssuer is the auth.TokenIssuer the D-025 test wires behind the
// auth.rotate_token surface: the "token" it mints encodes the VERIFIED
// identity triple the surface handed it, so each response provably reflects
// the caller's own identity — a mismatched echo (or the surface's
// body-vs-JWT identity check firing 401) is a cross-request identity bleed.
type identityEchoIssuer struct{}

func (identityEchoIssuer) IssueToken(_ context.Context, id identity.Identity, _ []auth.Scope, now time.Time) (string, time.Time, error) {
	return "echo|" + id.TenantID + "|" + id.UserID + "|" + id.SessionID, now.Add(time.Hour), nil
}

// TestServedHandle_ConcurrentReuse pins the D-025 contract: the served Handle
// is a compiled artifact — N≥100 concurrent AUTHENTICATED requests against
// one instance under -race, each under its own (tenant,user,session) triple,
// asserting (a) no data races, (b) no identity bleed: every response echoes
// ONLY its caller's triple through the full auth-middleware→surface path,
// (c) no cancellation cross-talk: cancelling some requests' contexts leaves
// the others completing normally, (d) goroutine baseline restored after Close.
func TestServedHandle_ConcurrentReuse(t *testing.T) {
	base := goruntime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signer := newTestSigner(t)
	opts := baseOptions(t)
	opts.AuthValidatorFactory = signer.factory()
	// The identity-echoing authenticated surface: auth.rotate_token verifies
	// the JWT, checks the body identity against the VERIFIED identity, and
	// mints through the echo issuer — so the response's new_token carries the
	// triple the server-side saw for THIS request.
	opts.BuildAuthSurface = func(red audit.Redactor, bus events.EventBus, _ *slog.Logger) (*auth.RotateSurface, error) {
		return auth.NewRotateSurface(identityEchoIssuer{}, red, auth.WithRotateBus(bus))
	}

	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	handler := h.Handler()

	// N distinct identities, one pre-minted token each (admin scope — the
	// rotate surface requires it).
	const n = 150
	tokens := make([]string, n)
	ids := make([]identity.Identity, n)
	for i := range n {
		ids[i] = identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%03d", i),
			UserID:    fmt.Sprintf("user-%03d", i),
			SessionID: fmt.Sprintf("sess-%03d", i),
		}
		tokens[i] = signer.sign(t, ids[i], []string{"admin"})
	}

	// Every 5th request runs under an already-cancelled context — the
	// cancellation-cross-talk leg: those requests may fail any way they like,
	// but the OTHER requests must still complete with their own echo.
	cancelled := func(i int) bool { return i%5 == 0 }

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q}}`,
				ids[i].TenantID, ids[i].UserID, ids[i].SessionID)
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/rotate_token", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tokens[i])
			if cancelled(i) {
				reqCtx, reqCancel := context.WithCancel(context.Background())
				reqCancel() // cancelled before dispatch — must not disturb siblings
				req = req.WithContext(reqCtx)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if cancelled(i) {
				return // its own outcome is unspecified; the point is isolation
			}
			if rec.Code != http.StatusOK {
				t.Errorf("req %d: rotate_token = %d (an ErrRotateIdentityMismatch 401 here IS an identity bleed), body=%s", i, rec.Code, rec.Body.String())
				return
			}
			var resp struct {
				NewToken string `json:"new_token"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Errorf("req %d: decode: %v", i, err)
				return
			}
			want := "echo|" + ids[i].TenantID + "|" + ids[i].UserID + "|" + ids[i].SessionID
			if resp.NewToken != want {
				t.Errorf("req %d: identity bleed — echoed %q, want %q", i, resp.NewToken, want)
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
