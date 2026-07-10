package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Production driver aggregator — the same blank import a served
	// binary adds. It resolves the real stores/bus the boot opens.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// The mock LLM driver is a registered driver the CONFIG selects; the
	// facade never seats it. Blank-imported here so a boot with a
	// driver: mock config resolves in the test binary.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/server"
	"github.com/hurtener/Harbor/sdk/tools"
)

// baseYAML is a valid full-binary config with a mock LLM driver and a
// jwks_file identity source. Callers substitute JWKS_FILE.
const baseYAML = `
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
  service_name: harbor-sdkserver-test
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

// writeRSAJWKS emits a minimal RSA JWK Set to a temp file, encoded
// independently of the parser under test. Returns the path.
func writeRSAJWKS(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	set := map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "sdkserver-test",
		"n": b64(pub.N.Bytes()),
		"e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	p := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return p
}

// validConfig returns a validated *config.Config with a live jwks_file.
func validConfig(t *testing.T) *config.Config {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	jwks := writeRSAJWKS(t, &priv.PublicKey)
	yaml := strings.Replace(baseYAML, "JWKS_FILE", jwks, 1)
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yaml))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	return cfg
}

// TestOpen_ProductionBoot_SucceedsAndCloses proves the facade boots a
// real server from a validated config with a jwks_file identity source,
// with a compiled-tool registrar wired, and closes clean.
func TestOpen_ProductionBoot_SucceedsAndCloses(t *testing.T) {
	cfg := validConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registered := false
	h, err := server.Open(ctx, cfg, server.Options{
		RegisterCatalog: func(cat tools.ToolCatalog) error {
			registered = true
			_ = cat // the registrar rides the pre-policy seam; wrap-firing is proven at the assemble layer
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !registered {
		t.Error("RegisterCatalog was not invoked through the facade")
	}
	if got := h.BindAddr(); got == "" {
		t.Error("BindAddr empty before Serve")
	}
	cc, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	if err := h.Close(cc); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpen_MissingJWKS_FailsLoudNamingField pins the production posture:
// a config whose identity block carries no JWKS source fails Open loud,
// naming the field — never at the first request.
func TestOpen_MissingJWKS_FailsLoudNamingField(t *testing.T) {
	cfg := validConfig(t)
	cfg.Identity.JWKSFile = ""
	cfg.Identity.JWKSURL = ""
	_, err := server.Open(context.Background(), cfg, server.Options{})
	if err == nil {
		t.Fatal("Open succeeded with no JWKS source; want a loud named-field error")
	}
	if !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), "jwks") {
		t.Fatalf("error must name the identity/jwks field, got %v", err)
	}
}

// TestOpen_InvalidConfig_FailsLoudAtOpen proves validation is not
// bypassable: an invalid programmatic config fails at Open, not at the
// first request.
func TestOpen_InvalidConfig_FailsLoudAtOpen(t *testing.T) {
	cfg := validConfig(t)
	// A negative shutdown grace is a structural server-block violation.
	cfg.Server.ShutdownGracePeriod = -1 * time.Second
	_, err := server.Open(context.Background(), cfg, server.Options{})
	if err == nil {
		t.Fatal("Open succeeded with an invalid config; want a loud validation error at Open")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected a config validation error, got %v", err)
	}
}

// TestOpen_NoConfigNoPath_FailsLoud — neither a config nor a path is a
// loud error (never a server from nothing).
func TestOpen_NoConfigNoPath_FailsLoud(t *testing.T) {
	_, err := server.Open(context.Background(), nil, server.Options{})
	if err == nil {
		t.Fatal("Open(nil, {}) succeeded; want ErrConfigRequired")
	}
	if !errors.Is(err, server.ErrConfigRequired) && !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected ErrConfigRequired, got %v", err)
	}
}

// TestOpen_FromConfigFile_LoadsAndBoots exercises the load-from-path
// convenience: a nil config + Options.ConfigPath loads, validates, and
// boots.
func TestOpen_FromConfigFile_LoadsAndBoots(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	jwks := writeRSAJWKS(t, &priv.PublicKey)
	yaml := strings.Replace(baseYAML, "JWKS_FILE", jwks, 1)
	yamlPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := server.Open(ctx, nil, server.Options{ConfigPath: yamlPath})
	if err != nil {
		t.Fatalf("Open from path: %v", err)
	}
	cc, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	if err := h.Close(cc); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestFacade_NoDevSurfaces_NoInjectionSeams is the D-205 no-behavior
// guard for this facade: the sdk/server source must not reference a
// dev-signer, a mock knob, or any of the serve band's caller-side
// injection seams — a production-serving facade cannot seat them.
func TestFacade_NoDevSurfaces_NoInjectionSeams(t *testing.T) {
	// Concrete seam identifiers (not prose): none of the serve band's
	// caller-side injection seams, dev-signer factory, or mock knobs may
	// be REFERENCED in the facade's surface file. doc.go is exempt — it
	// legitimately DESCRIBES what the facade omits.
	forbidden := []string{
		"devSigner", "AuthValidatorFactory",
		"BuildAuthSurface", "BuildLLMSnapshot", "ExtraRoutes", "PostBoot",
		"allowMock", "AllowMock", "HARBOR_DEV_ALLOW_MOCK",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "doc.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s references forbidden dev/injection identifier %q — the facade is production-only", name, bad)
			}
		}
	}
}
