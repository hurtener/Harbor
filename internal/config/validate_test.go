package config_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestValidate_RejectsMissingRequired(t *testing.T) {
	_, err := config.Load(context.Background(), "testdata/invalid_missing_required.yaml")
	if err == nil {
		t.Fatal("Load accepted a config missing identity.jwt_algorithms")
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), "identity.jwt_algorithms") {
		t.Errorf("err=%q missing path identity.jwt_algorithms", err.Error())
	}
	if !strings.Contains(err.Error(), "testdata/invalid_missing_required.yaml") {
		t.Errorf("err=%q missing source path", err.Error())
	}
}

func TestValidate_RejectsForbiddenJWTAlg(t *testing.T) {
	_, err := config.Load(context.Background(), "testdata/invalid_enum.yaml")
	if err == nil {
		t.Fatal("Load accepted HS256 in jwt_algorithms")
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("err=%v, want ErrConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), "HS256") {
		t.Errorf("err=%q missing offending algorithm name", err.Error())
	}
	if !strings.Contains(err.Error(), "RS256") {
		t.Errorf("err=%q does not enumerate allowed algorithms", err.Error())
	}
}

func TestValidate_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*config.Config)
		wantPath  string
	}{
		{
			"empty bind_addr",
			func(c *config.Config) { c.Server.BindAddr = "" },
			"server.bind_addr",
		},
		{
			"malformed bind_addr",
			func(c *config.Config) { c.Server.BindAddr = "no-port-here" },
			"server.bind_addr",
		},
		{
			"zero shutdown grace",
			func(c *config.Config) { c.Server.ShutdownGracePeriod = 0 },
			"server.shutdown_grace_period",
		},
		{
			"empty issuer",
			func(c *config.Config) { c.Identity.Issuer = "" },
			"identity.issuer",
		},
		{
			"empty audience",
			func(c *config.Config) { c.Identity.Audience = "" },
			"identity.audience",
		},
		{
			"missing JWKS",
			func(c *config.Config) {
				c.Identity.JWKSURL = ""
				c.Identity.JWKSFile = ""
			},
			"identity",
		},
		{
			"unknown log_format",
			func(c *config.Config) { c.Telemetry.LogFormat = "csv" },
			"telemetry.log_format",
		},
		{
			"unknown log_level",
			func(c *config.Config) { c.Telemetry.LogLevel = "trace" },
			"telemetry.log_level",
		},
		{
			"empty service_name",
			func(c *config.Config) { c.Telemetry.ServiceName = "" },
			"telemetry.service_name",
		},
		{
			"unknown state driver",
			func(c *config.Config) { c.State.Driver = "bigtable" },
			"state.driver",
		},
		{
			"sqlite without DSN",
			func(c *config.Config) {
				c.State.Driver = "sqlite"
				c.State.DSN = ""
			},
			"state.dsn",
		},
		{
			"empty llm provider",
			func(c *config.Config) { c.LLM.Provider = "" },
			"llm.provider",
		},
		{
			"empty llm model",
			func(c *config.Config) { c.LLM.Model = "" },
			"llm.model",
		},
		{
			"empty llm api_key",
			func(c *config.Config) { c.LLM.APIKey = "" },
			"llm.api_key",
		},
		{
			"zero llm timeout",
			func(c *config.Config) { c.LLM.Timeout = 0 },
			"llm.timeout",
		},
		{
			"zero default_max_tokens",
			func(c *config.Config) { c.Governance.DefaultMaxTokens = 0 },
			"governance.default_max_tokens",
		},
		{
			"negative repair_attempts",
			func(c *config.Config) { c.Governance.RepairAttempts = -1 },
			"governance.repair_attempts",
		},
		{
			"negative cost_ceiling_usd",
			func(c *config.Config) { c.Governance.CostCeilingUSD = -1 },
			"governance.cost_ceiling_usd",
		},
		{
			"negative rate_limit_tps",
			func(c *config.Config) { c.Governance.RateLimitTPS = -1 },
			"governance.rate_limit_tps",
		},
		{
			"empty artifacts driver",
			func(c *config.Config) { c.Artifacts.Driver = "" },
			"artifacts.driver",
		},
		{
			"unknown artifacts driver",
			func(c *config.Config) { c.Artifacts.Driver = "no-such-driver" },
			"artifacts.driver",
		},
		{
			"fs driver without root",
			func(c *config.Config) {
				c.Artifacts.Driver = "fs"
				c.Artifacts.FSRoot = ""
			},
			"artifacts.fs_root",
		},
		{
			"negative heavy output threshold",
			func(c *config.Config) { c.Artifacts.HeavyOutputThresholdBytes = -1 },
			"artifacts.heavy_output_threshold_bytes",
		},
		{
			"empty memory driver",
			func(c *config.Config) { c.Memory.Driver = "" },
			"memory.driver",
		},
		{
			"unknown memory driver",
			func(c *config.Config) { c.Memory.Driver = "redis" },
			"memory.driver",
		},
		{
			"unsupported memory strategy",
			func(c *config.Config) { c.Memory.Strategy = "rolling_summary" },
			"memory.strategy",
		},
		{
			"negative memory budget",
			func(c *config.Config) { c.Memory.BudgetTokens = -1 },
			"memory.budget_tokens",
		},
		{
			"sqlite memory driver requires DSN",
			func(c *config.Config) {
				c.Memory.Driver = "sqlite"
				c.Memory.DSN = ""
			},
			"memory.dsn",
		},
		{
			"postgres memory driver requires DSN",
			func(c *config.Config) {
				c.Memory.Driver = "postgres"
				c.Memory.DSN = ""
			},
			"memory.dsn",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid config (mutation: %s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("err=%q missing path %q", err.Error(), tc.wantPath)
			}
		})
	}
}

func TestValidate_HappyPath(t *testing.T) {
	cfg := mustLoadValid(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on the canonical fixture failed: %v", err)
	}
}

func TestValidate_AcceptsSQLiteMemoryDriverWithDSN(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Memory.Driver = "sqlite"
	cfg.Memory.DSN = "/var/lib/harbor/memory.sqlite"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected sqlite memory driver: %v", err)
	}
}

func TestValidate_AcceptsPostgresMemoryDriverWithDSN(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Memory.Driver = "postgres"
	cfg.Memory.DSN = "postgres://harbor:secret@localhost:5432/harbor?sslmode=disable"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected postgres memory driver: %v", err)
	}
}

func TestValidate_AcceptsAllAllowedAlgorithms(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Identity.JWTAlgorithms = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected canonical RS/ES algorithms: %v", err)
	}
}

func TestLiveReloadable_EmptyInPhase02(t *testing.T) {
	cfg := mustLoadValid(t)
	got := cfg.LiveReloadable()
	if len(got) != 0 {
		t.Errorf("LiveReloadable = %v, want empty in Phase 02", got)
	}
}

func TestIsValidationError(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Server.BindAddr = ""
	err := cfg.Validate()
	wrapped := errors.Join(config.ErrConfigInvalid, err)
	if !config.IsValidationError(wrapped) {
		t.Errorf("IsValidationError did not flag a wrapped validation error")
	}
	if config.IsValidationError(nil) {
		t.Errorf("IsValidationError returned true for nil")
	}
	if config.IsValidationError(errors.New("unrelated")) {
		t.Errorf("IsValidationError returned true for an unrelated error")
	}
}

// mustLoadValid loads the canonical valid fixture and returns a
// mutable copy callers can break in subtests.
func mustLoadValid(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load(valid_minimal): %v", err)
	}
	return cfg
}
