package config_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

const validMinimalFixture = "testdata/valid_minimal.yaml"

func TestLoad_ValidMinimal(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load returned nil config")
	}
	if cfg.Server.BindAddr != "0.0.0.0:9000" {
		t.Errorf("Server.BindAddr = %q, want 0.0.0.0:9000", cfg.Server.BindAddr)
	}
	if cfg.Server.ShutdownGracePeriod != 15*time.Second {
		t.Errorf("Server.ShutdownGracePeriod = %v, want 15s", cfg.Server.ShutdownGracePeriod)
	}
	if cfg.LLM.APIKey != "sk-test-fixture" {
		t.Errorf("LLM.APIKey not preserved through Load")
	}
	if got := cfg.Identity.JWTAlgorithms; len(got) != 2 || got[0] != "RS256" || got[1] != "ES256" {
		t.Errorf("Identity.JWTAlgorithms = %v, want [RS256 ES256]", got)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load(context.Background(), "testdata/does_not_exist.yaml")
	if err == nil {
		t.Fatal("Load returned nil for missing file")
	}
	if !errors.Is(err, config.ErrConfigNotFound) {
		t.Fatalf("err=%v, want errors.Is ErrConfigNotFound", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err=%v, want errors.Is fs.ErrNotExist", err)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("server: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(context.Background(), bad)
	if err == nil {
		t.Fatal("Load returned nil for malformed YAML")
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want errors.Is ErrConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q missing source path %q", err.Error(), bad)
	}
}

func TestLoad_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := config.Load(ctx, validMinimalFixture)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestLoadFromBytes_ValidMinimal(t *testing.T) {
	data, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromBytes(context.Background(), data)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.LLM.Model != "anthropic/claude-sonnet-4" {
		t.Errorf("LLM.Model = %q, want anthropic/claude-sonnet-4", cfg.LLM.Model)
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	// The minimal fixture omits telemetry.service_name's default; our
	// loader should still produce a valid config because the fixture
	// supplies it. To prove defaults work we strip a default-able key.
	yamlNoServer := []byte(`identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: harbor-test
state:
  driver: inmem
llm:
  provider: openrouter
  model: m
  api_key: k
  timeout: 30s
governance:
  repair_attempts: 1
`)
	cfg, err := config.LoadFromBytes(context.Background(), yamlNoServer)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Server.BindAddr != "127.0.0.1:8080" {
		t.Errorf("Server.BindAddr = %q, want default 127.0.0.1:8080", cfg.Server.BindAddr)
	}
	if cfg.Server.ShutdownGracePeriod != 30*time.Second {
		t.Errorf("Server.ShutdownGracePeriod = %v, want default 30s", cfg.Server.ShutdownGracePeriod)
	}
}

func TestEnvOverride_StringField(t *testing.T) {
	t.Setenv("HARBOR_LLM_MODEL", "override-model")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Model != "override-model" {
		t.Errorf("LLM.Model = %q, want override-model", cfg.LLM.Model)
	}
}

func TestEnvOverride_DurationField(t *testing.T) {
	t.Setenv("HARBOR_LLM_TIMEOUT", "5m")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Timeout != 5*time.Minute {
		t.Errorf("LLM.Timeout = %v, want 5m", cfg.LLM.Timeout)
	}
}

func TestEnvOverride_IntField(t *testing.T) {
	t.Setenv("HARBOR_GOVERNANCE_REPAIR_ATTEMPTS", "5")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Governance.RepairAttempts != 5 {
		t.Errorf("RepairAttempts = %d, want 5", cfg.Governance.RepairAttempts)
	}
}

func TestEnvOverride_FloatField(t *testing.T) {
	t.Setenv("HARBOR_LLM_CONTEXT_WINDOW_RESERVE", "0.1")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.ContextWindowReserve != 0.1 {
		t.Errorf("ContextWindowReserve = %v, want 0.1", cfg.LLM.ContextWindowReserve)
	}
}

func TestEnvOverride_SliceField(t *testing.T) {
	t.Setenv("HARBOR_IDENTITY_JWT_ALGORITHMS", "RS512,ES384")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"RS512", "ES384"}
	if len(cfg.Identity.JWTAlgorithms) != 2 ||
		cfg.Identity.JWTAlgorithms[0] != want[0] ||
		cfg.Identity.JWTAlgorithms[1] != want[1] {
		t.Errorf("JWTAlgorithms = %v, want %v", cfg.Identity.JWTAlgorithms, want)
	}
}

func TestEnvOverride_PrecedenceOverYAML(t *testing.T) {
	// YAML says 0.0.0.0:9000; env says :7000. Env wins.
	t.Setenv("HARBOR_SERVER_BIND_ADDR", "127.0.0.1:7000")
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BindAddr != "127.0.0.1:7000" {
		t.Errorf("env override did not win: BindAddr=%q", cfg.Server.BindAddr)
	}
}

func TestEnvOverride_InvalidValueIsLoudFailure(t *testing.T) {
	t.Setenv("HARBOR_GOVERNANCE_REPAIR_ATTEMPTS", "not-an-int")
	_, err := config.Load(context.Background(), validMinimalFixture)
	if err == nil {
		t.Fatal("Load returned nil for invalid env override")
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), "repair_attempts") {
		t.Errorf("err=%q missing field name", err.Error())
	}
}

func TestWithOverrides_AppliesDottedPath(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := config.WithOverrides(cfg, map[string]string{
		"llm.model":                  "claude-haiku-4-5",
		"server.bind_addr":           "127.0.0.1:5555",
		"governance.repair_attempts": "5",
	})
	if err != nil {
		t.Fatalf("WithOverrides: %v", err)
	}
	if out.LLM.Model != "claude-haiku-4-5" {
		t.Errorf("LLM.Model = %q, want claude-haiku-4-5", out.LLM.Model)
	}
	if out.Server.BindAddr != "127.0.0.1:5555" {
		t.Errorf("Server.BindAddr = %q, want 127.0.0.1:5555", out.Server.BindAddr)
	}
	if out.Governance.RepairAttempts != 5 {
		t.Errorf("Governance.RepairAttempts = %d, want 5", out.Governance.RepairAttempts)
	}
}

func TestWithOverrides_UnknownKey(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = config.WithOverrides(cfg, map[string]string{
		"nonexistent.field": "anything",
	})
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid for unknown override key", err)
	}
}

func TestWithOverrides_NilConfig(t *testing.T) {
	_, err := config.WithOverrides(nil, map[string]string{"x": "y"})
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid for nil *Config", err)
	}
}

func TestWithOverrides_RevalidatesAfterChange(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Empty bind addr fails validation.
	_, err = config.WithOverrides(cfg, map[string]string{
		"server.bind_addr": "",
	})
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid after invalid override", err)
	}
}

// TestMemoryConfig_DefaultsApplied confirms the memory section's
// default values land when the YAML omits the block. Phase 24
// adds `recovery_backlog_max: 16` to the defaults alongside the
// Phase 23 `driver: inmem` + `strategy: none` + `budget_tokens: 0`.
func TestMemoryConfig_DefaultsApplied(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.Driver != "inmem" {
		t.Errorf("Memory.Driver=%q, want %q", cfg.Memory.Driver, "inmem")
	}
	if cfg.Memory.Strategy != "none" {
		t.Errorf("Memory.Strategy=%q, want %q", cfg.Memory.Strategy, "none")
	}
	if cfg.Memory.BudgetTokens != 0 {
		t.Errorf("Memory.BudgetTokens=%d, want 0", cfg.Memory.BudgetTokens)
	}
	if cfg.Memory.RecoveryBacklogMax != 16 {
		t.Errorf("Memory.RecoveryBacklogMax=%d, want 16", cfg.Memory.RecoveryBacklogMax)
	}
}

// TestConfig_ConcurrentRead_ReuseContract is the D-025 concurrent-reuse
// test for *Config. It reads multiple field paths from a shared
// instance under N goroutines and asserts no data race + no goroutine
// leak. A future change that introduces a write path through *Config
// will fail under -race here.
func TestConfig_ConcurrentRead_ReuseContract(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const goroutines = 256
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var mismatches atomic.Int64
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			// Touch a fanned-out set of fields. The point is to
			// detect any concealed mutation under -race.
			if cfg.Server.BindAddr != "0.0.0.0:9000" {
				mismatches.Add(1)
			}
			if cfg.LLM.Model != "anthropic/claude-sonnet-4" {
				mismatches.Add(1)
			}
			if cfg.Identity.Issuer != "https://issuer.example.com" {
				mismatches.Add(1)
			}
			if cfg.Telemetry.LogFormat != "json" {
				mismatches.Add(1)
			}
			if cfg.LLM.APIKey != "sk-test-fixture" {
				mismatches.Add(1)
			}
			if got := cfg.LiveReloadable(); len(got) != 0 {
				mismatches.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d concurrent reads observed unexpected values", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// httpManifestFixtureYAML returns the valid_minimal fixture's bytes
// with a `tools.http_manifests` block appended, one entry per path
// given (in order).
func httpManifestFixtureYAML(t *testing.T, entries ...string) []byte {
	t.Helper()
	base, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var b strings.Builder
	b.Write(base)
	b.WriteString("\ntools:\n  http_manifests:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "    - %q\n", e)
	}
	return []byte(b.String())
}

// TestLoad_HTTPManifests_ResolvesRelativeAgainstConfigDir — the
// HTTP-manifest boot loader (CLAUDE.md §7 rule 5): a relative entry
// resolves against the loaded config file's directory, not the
// process CWD.
func TestLoad_HTTPManifests_ResolvesRelativeAgainstConfigDir(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "tools"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(tmpDir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, httpManifestFixtureYAML(t, "tools/weather.yaml"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(tmpDir, "tools", "weather.yaml")
	if got := cfg.Tools.HTTPManifests[0]; got != want {
		t.Errorf("HTTPManifests[0] = %q, want %q", got, want)
	}
}

// TestLoad_HTTPManifests_AbsoluteAcceptedAfterClean — an absolute
// entry is Clean-ed and accepted unconditionally (the documented
// `/etc/harbor/tools/*.yaml` operator deployment shape).
func TestLoad_HTTPManifests_AbsoluteAcceptedAfterClean(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, httpManifestFixtureYAML(t, "/etc/harbor/tools//weather.yaml"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "/etc/harbor/tools/weather.yaml"
	if got := cfg.Tools.HTTPManifests[0]; got != want {
		t.Errorf("HTTPManifests[0] = %q, want %q", got, want)
	}
}

// TestLoad_HTTPManifests_PathResolutionTable mirrors
// `internal/skills/importer/path_safety.go`'s rejection-case shape
// (lexical Clean+prefix escape checks) against the loader's own
// (replicated, not imported — CLAUDE.md §7 rule 5 + the "config must
// not depend on skills" constraint) implementation.
func TestLoad_HTTPManifests_PathResolutionTable(t *testing.T) {
	cases := []struct {
		name       string
		entry      string
		wantErr    bool
		wantSuffix string // relative to tmpDir; only checked when !wantErr
	}{
		{
			name:       "relative under config dir",
			entry:      "tools/weather.yaml",
			wantSuffix: filepath.Join("tools", "weather.yaml"),
		},
		{
			name:       "relative with harmless dotdot staying inside root",
			entry:      filepath.Join("tools", "..", "weather.yaml"),
			wantSuffix: "weather.yaml",
		},
		{
			name:    "relative escape via leading dotdot",
			entry:   "../escape.yaml",
			wantErr: true,
		},
		{
			name:    "relative escape via nested dotdot",
			entry:   filepath.Join("tools", "..", "..", "escape.yaml"),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfgPath := filepath.Join(tmpDir, "harbor.yaml")
			if err := os.WriteFile(cfgPath, httpManifestFixtureYAML(t, tc.entry), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(context.Background(), cfgPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load accepted escaping entry %q", tc.entry)
				}
				if !strings.Contains(err.Error(), "tools.http_manifests[0]") {
					t.Errorf("err missing field path: %v", err)
				}
				if !errors.Is(err, config.ErrConfigInvalid) {
					t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q): %v", tc.entry, err)
			}
			want := filepath.Join(tmpDir, tc.wantSuffix)
			if got := cfg.Tools.HTTPManifests[0]; got != want {
				t.Errorf("resolved = %q, want %q", got, want)
			}
		})
	}
}

// TestLoadFromBytes_HTTPManifests_RelativeEntryPassesThroughUnresolved
// — LoadFromBytes has no config-file directory, so a relative entry
// is left as-is (matching a hand-built *Config an embedder constructs
// in Go); only the structural Validate checks apply.
func TestLoadFromBytes_HTTPManifests_RelativeEntryPassesThroughUnresolved(t *testing.T) {
	data := httpManifestFixtureYAML(t, "tools/weather.yaml")
	cfg, err := config.LoadFromBytes(context.Background(), data)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if got := cfg.Tools.HTTPManifests[0]; got != "tools/weather.yaml" {
		t.Errorf("HTTPManifests[0] = %q, want unresolved %q", got, "tools/weather.yaml")
	}
}

// TestLoadFromBytesAt_HTTPManifests_ResolvesAgainstGivenPathDir proves
// LoadFromBytesAt resolves a relative entry against filepath.Dir(path)
// exactly like Load, without re-reading the file — the seam `harbor
// validate` uses so it exercises the same §7 rule 5 path-safety check
// as boot instead of silently skipping it (Phase 149 / D-279; a bare
// LoadFromBytes has no path and cannot resolve).
func TestLoadFromBytesAt_HTTPManifests_ResolvesAgainstGivenPathDir(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "tools"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(tmpDir, "harbor.yaml")
	data := httpManifestFixtureYAML(t, "tools/weather.yaml")

	cfg, err := config.LoadFromBytesAt(context.Background(), data, cfgPath)
	if err != nil {
		t.Fatalf("LoadFromBytesAt: %v", err)
	}
	want := filepath.Join(tmpDir, "tools", "weather.yaml")
	if got := cfg.Tools.HTTPManifests[0]; got != want {
		t.Errorf("HTTPManifests[0] = %q, want %q", got, want)
	}
}

// TestLoadFromBytesAt_HTTPManifests_RejectsEscape mirrors the Load
// escape-rejection test, proving LoadFromBytesAt enforces the same
// §7 rule 5 boundary rather than silently accepting it because the
// caller supplied the bytes itself.
func TestLoadFromBytesAt_HTTPManifests_RejectsEscape(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "harbor.yaml")
	data := httpManifestFixtureYAML(t, "../escape.yaml")

	_, err := config.LoadFromBytesAt(context.Background(), data, cfgPath)
	if err == nil {
		t.Fatal("LoadFromBytesAt accepted an escaping relative entry")
	}
	if !strings.Contains(err.Error(), "tools.http_manifests[0]") {
		t.Errorf("err missing field path: %v", err)
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
	}
}
