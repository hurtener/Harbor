package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// serveBootYAML is the bus-wired hermetic config for the production-boot
// test, identical to the dev fixture EXCEPT the identity section points at
// a JWKS FILE (RS256) instead of a URL — the shape `harbor serve` loads.
// Only one JWKS source is set (config rejects both — D-221 W4).
func serveBootYAML(jwksFile string) string {
	return `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: ` + jwksFile + `
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
}

// writeServeBootJWKS emits a single-key RS256 JWK Set file from pub.
func writeServeBootJWKS(t *testing.T, pub *rsa.PublicKey, kid string) string {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	set := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": e},
		},
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return path
}

// TestBootDevStack_ServeProductionBoot_GatesDevSurfacesAndVerifiesJWKS is
// the wave-end checkpoint coverage for the PRODUCTION boot wiring: it boots
// the shared bootDevStack down the serve path (authValidatorFactory set ⇒
// signer == nil) and asserts, against a RUNNING handler, that (a) a real
// JWKS-verified token is authorised through to the steering surface, and
// (b) every dev-only surface is absent. The dev/serve split lives in one
// shared bootDevStack gated on signer == nil; nothing else exercises the
// composed production boot end-to-end (the auth integration tests construct
// the validator directly, bypassing bootDevStack), so a future refactor
// could regress a gate undetected. HARBOR_DEV_SEED_FIXTURES is set to prove
// the runtime-fixture seeder stays gated on a production boot.
// TestServeCmd_HelpDocumentsTUIFlag proves the --tui flag is present
// and documented in `harbor serve --help` — the operator-facing surface
// for co-launching the native terminal client.
func TestServeCmd_HelpDocumentsTUIFlag(t *testing.T) {
	stdout, stderr, err := runRoot(t, []string{"serve", "--help"})
	if err != nil || stderr != "" {
		t.Fatalf("help err=%v stderr=%q", err, stderr)
	}
	for _, text := range []string{"--tui", "co-launch", "authenticated REST/SSE"} {
		if !strings.Contains(stdout, text) {
			t.Errorf("serve --help missing %q:\n%s", text, stdout)
		}
	}
	root := NewRootCmd()
	cmd, _, findErr := root.Find([]string{"serve"})
	if findErr != nil || cmd.Flags().Lookup("tui") == nil {
		t.Fatalf("serve --tui flag missing: %v", findErr)
	}
}

func TestBootDevStack_ServeProductionBoot_GatesDevSurfacesAndVerifiesJWKS(t *testing.T) {
	const kid = "serve-boot-test-rsa"
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	jwksPath := writeServeBootJWKS(t, &priv.PublicKey, kid)

	cfgPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(serveBootYAML(jwksPath)), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	// The seeder must NOT fire on a production boot even with the env var.
	t.Setenv(EnvDevSeedFixtures, "1")

	var stderr bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stack, err := serve.Boot(ctx, serve.Options{
		ConfigPath: cfgPath,
		Logger:     logger,
		Stderr:     &stderr,
		// The production serve posture: JWKS factory, no dev seams. allowMock
		// lets the hermetic stack boot without a real provider; the gating
		// under test is the absence of dev seams, independent of the LLM gate.
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
		BuildLLMSnapshot:     newLLMSnapshotBuilder(true),
		MCPDefaultIdentity:   identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession},
		DisplayName:          "harbor dev",
		InstanceID:           devInstanceID(),
		BuildVersion:         HarborVersion,
		BuildCommit:          "dev",
		SubcommandLabel:      "serve",
	})
	if err != nil {
		t.Fatalf("serve.Boot (serve path): %v", err)
	}
	defer func() {
		cc, ccCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccCancel()
		stack.Close(cc)
	}()

	h := stack.Handler()
	if h == nil {
		t.Fatal("booted stack has no HTTP handler")
	}

	post := func(path, body, bearer string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	now := time.Now()
	signed := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://issuer.example.com", "aud": "harbor", "sub": "user-1",
			"exp":     now.Add(time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  "tenant-acme",
			"user":    "user-1",
			"session": "sess-default",
		})
		tok.Header["kid"] = kid
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}

	// A steering control targeting the caller's OWN (tenant,user) on a run
	// with no live inbox. JWKS verification + owner_user derivation must
	// succeed (the body identity matches the token), so the surface reaches
	// the inbox Lookup and returns not_found — proving auth passed through
	// the production boot, not that auth rejected it.
	ghostBody := `{"identity":{"tenant":"tenant-acme","user":"user-1","session":"sess-default","run":"run-serve-ghost"}}`

	t.Run("jwks_verified_token_authorised", func(t *testing.T) {
		code, body := post("/v1/control/cancel", ghostBody, signed())
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (auth passed, run absent); body=%s", code, body)
		}
		var perr struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &perr)
		if perr.Code != "not_found" {
			t.Fatalf("code = %q, want not_found (a JWKS auth failure would be identity_required/auth_rejected); body=%s", perr.Code, body)
		}
	})

	t.Run("unauthenticated_rejected_401", func(t *testing.T) {
		code, _ := post("/v1/control/cancel", ghostBody, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated status = %d, want 401", code)
		}
	})

	// Dev-only surfaces must be ABSENT on the production boot (signer == nil).
	for _, dev := range []struct{ name, path, body string }{
		{"bootstrap_token_endpoint_absent", "/v1/dev/bootstrap.json", "{}"},
		{"dev_draft_surface_absent", "/v1/dev/drafts/list", "{}"},
	} {
		t.Run(dev.name, func(t *testing.T) {
			code, _ := post(dev.path, dev.body, "")
			if code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404 (dev surface must not mount on serve)", dev.path, code)
			}
		})
	}

	t.Run("seeder_did_not_fire", func(t *testing.T) {
		if strings.Contains(stderr.String(), DevSeedBanner) {
			t.Fatalf("runtime fixture seeder fired on a production boot despite signer==nil; stderr=%q", stderr.String())
		}
	})
}
