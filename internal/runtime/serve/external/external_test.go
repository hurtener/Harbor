package external_test

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
