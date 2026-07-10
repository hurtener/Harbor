// serve_config_test.go — Options.Config semantics: the caller-supplied
// pre-loaded config path added for the external-serving facade.
package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// loadTestCfg loads + validates the test yaml at path.
func loadTestCfg(t *testing.T, path string) (*config.Config, error) {
	t.Helper()
	return config.Load(context.Background(), path)
}

// TestBoot_Config_WinsOverConfigPath — when both Options.Config and
// Options.ConfigPath are set, the pre-loaded Config wins (ConfigPath is
// not read). Pinned by giving ConfigPath a path that would fail loudly
// if loaded.
func TestBoot_Config_WinsOverConfigPath(t *testing.T) {
	cfgPath := writeTestCfg(t)
	cfg, err := loadTestCfg(t, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A distinctive marker on the pre-loaded config proves it is the one
	// the boot consumed.
	cfg.Telemetry.ServiceName = "harbor-config-wins"

	opts := baseOptions(t)
	opts.Config = cfg
	opts.ConfigPath = "/nonexistent/should-never-be-read/harbor.yaml"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot with Config set must not read ConfigPath; got %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	if h.Cfg.Telemetry.ServiceName != "harbor-config-wins" {
		t.Errorf("booted config ServiceName = %q, want the pre-loaded config's marker (Config must win over ConfigPath)",
			h.Cfg.Telemetry.ServiceName)
	}
}

// TestBoot_Config_Invalid_FailsValidateLoud — a hand-built (or mutated)
// config passed via Options.Config is re-run through the full-binary
// Validate; an invalid one fails Boot loud, never a listener that
// misbehaves at first request.
func TestBoot_Config_Invalid_FailsValidateLoud(t *testing.T) {
	cfgPath := writeTestCfg(t)
	cfg, err := loadTestCfg(t, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Strip the identity JWKS source — the full-binary profile rejects it.
	cfg.Identity.JWKSURL = ""
	cfg.Identity.JWKSFile = ""

	opts := baseOptions(t)
	opts.Config = cfg
	opts.ConfigPath = ""

	h, bErr := Boot(context.Background(), opts)
	if bErr == nil {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
		t.Fatal("Boot succeeded with an invalid hand-mutated config — Options.Config must be re-validated")
	}
	if !strings.Contains(bErr.Error(), "identity") {
		t.Errorf("validation error should name the identity field, got %v", bErr)
	}
}
