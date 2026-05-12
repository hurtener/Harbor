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
		name     string
		mutate   func(*config.Config)
		wantPath string
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
			func(c *config.Config) { c.Memory.Strategy = "not-a-strategy" },
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
		{
			"negative memory recovery backlog max",
			func(c *config.Config) { c.Memory.RecoveryBacklogMax = -1 },
			"memory.recovery_backlog_max",
		},
		// Phase 29 tools validation.
		{
			"a2a peer empty url",
			func(c *config.Config) {
				c.Tools.A2APeers = []config.A2APeerConfig{{URL: "", TrustTier: 3, LatencyTierMS: 10}}
			},
			"tools.a2a_peers[0].url",
		},
		{
			"a2a peer trust tier zero",
			func(c *config.Config) {
				c.Tools.A2APeers = []config.A2APeerConfig{{URL: "https://a", TrustTier: 0, LatencyTierMS: 10}}
			},
			"tools.a2a_peers[0].trust_tier",
		},
		{
			"a2a peer trust tier above range",
			func(c *config.Config) {
				c.Tools.A2APeers = []config.A2APeerConfig{{URL: "https://a", TrustTier: 6, LatencyTierMS: 10}}
			},
			"tools.a2a_peers[0].trust_tier",
		},
		{
			"a2a peer negative latency",
			func(c *config.Config) {
				c.Tools.A2APeers = []config.A2APeerConfig{{URL: "https://a", TrustTier: 3, LatencyTierMS: -1}}
			},
			"tools.a2a_peers[0].latency_tier_ms",
		},
		// Phase 33a custom-provider validation.
		{
			"custom provider empty name",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{BaseURL: "https://e", APIKeyEnvVar: "E", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].name",
		},
		{
			"custom provider empty base url",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", APIKeyEnvVar: "E", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].base_url",
		},
		{
			"custom provider empty env var",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].api_key_env_var",
		},
		{
			"custom provider env var with env. prefix",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", APIKeyEnvVar: "env.E", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].api_key_env_var",
		},
		{
			"custom provider empty models",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", APIKeyEnvVar: "E"},
				}
			},
			"llm.custom_providers[0].models",
		},
		{
			"custom provider duplicate name",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", APIKeyEnvVar: "E", Models: []string{"m"}},
					{Name: "x", BaseURL: "https://e2", APIKeyEnvVar: "E2", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[1].name",
		},
		{
			"custom provider name collides with native",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "openai"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "openai", BaseURL: "https://e", APIKeyEnvVar: "E", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].name",
		},
		{
			"custom provider unknown base_provider_type",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "x"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", APIKeyEnvVar: "E",
						BaseProviderType: "anthropic-compat", Models: []string{"m"}},
				}
			},
			"llm.custom_providers[0].base_provider_type",
		},
		{
			"llm.provider matches neither native nor custom",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.Provider = "ghost-provider"
				c.LLM.CustomProviders = []config.LLMCustomProviderConfig{
					{Name: "x", BaseURL: "https://e", APIKeyEnvVar: "E", Models: []string{"m"}},
				}
			},
			"llm.provider",
		},
		{
			"network defaults negative timeout",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.NetworkDefaults = config.LLMNetworkDefaults{Timeout: -1}
			},
			"llm.network_defaults.timeout",
		},
		{
			"network defaults negative max_retries",
			func(c *config.Config) {
				c.LLM.Driver = "bifrost"
				c.LLM.NetworkDefaults = config.LLMNetworkDefaults{MaxRetries: -1}
			},
			"llm.network_defaults.max_retries",
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

// Phase 33a — when llm.provider names a custom-provider entry, the
// legacy llm.api_key / llm.timeout / llm.base_url fields are NOT
// required because the custom entry supplies them.
func TestValidate_CustomProviderPrimary_LegacyFieldsOptional(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.LLM.Driver = "bifrost"
	cfg.LLM.Provider = "nim"
	cfg.LLM.APIKey = ""
	cfg.LLM.BaseURL = ""
	cfg.LLM.Timeout = 0
	cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{
		{
			Name:         "nim",
			BaseURL:      "https://integrate.api.nvidia.com",
			APIKeyEnvVar: "NVIDIA_API_KEY",
			Models:       []string{"google/gemma-4-31b-it"},
			Timeout:      180 * 1e9, // 180s in time.Duration
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected custom-primary config: %v", err)
	}
}

// Phase 33a — declared custom provider that isn't the primary still
// validates. Native primary continues to require legacy fields.
func TestValidate_NativeAndCustomCoexist(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.LLM.Driver = "bifrost"
	cfg.LLM.Provider = "openrouter"
	cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{
		{
			Name:         "in-house-llm",
			BaseURL:      "http://localhost:8000",
			APIKeyEnvVar: "INHOUSE_KEY",
			Models:       []string{"llama"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected mixed native + custom config: %v", err)
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

func TestValidate_AcceptsEmptyTools(t *testing.T) {
	// Tools section is optional; absent / empty is accepted.
	cfg := mustLoadValid(t)
	cfg.Tools = config.ToolsConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected empty ToolsConfig: %v", err)
	}
	cfg.Tools = config.ToolsConfig{HTTPManifests: []string{
		"/etc/harbor/tools/weather.yaml",
		"/etc/harbor/tools/github.yaml",
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected populated ToolsConfig: %v", err)
	}
}

func TestValidate_RejectsEmptyToolsManifestPath(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Tools = config.ToolsConfig{HTTPManifests: []string{
		"/etc/harbor/tools/weather.yaml",
		"   ",
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted blank manifest path")
	}
	if !strings.Contains(err.Error(), "tools.http_manifests[1]") {
		t.Errorf("err missing path: %v", err)
	}
}

func TestLiveReloadable_EmptyInPhase02(t *testing.T) {
	cfg := mustLoadValid(t)
	got := cfg.LiveReloadable()
	if len(got) != 0 {
		t.Errorf("LiveReloadable = %v, want empty in Phase 02", got)
	}
}

func TestValidateTools_MCPServers(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantOK  bool
		wantSub string
	}{
		{
			name:   "empty mcp_servers passes",
			mutate: func(c *config.Config) { c.Tools.MCPServers = nil },
			wantOK: true,
		},
		{
			name: "valid sse server",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "github", TransportMode: "sse", URL: "https://example.com/sse"},
				}
			},
			wantOK: true,
		},
		{
			name: "valid streamable_http server",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "filesystem", TransportMode: "streamable_http", URL: "https://example.com/mcp"},
				}
			},
			wantOK: true,
		},
		{
			name: "valid stdio server",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "local", TransportMode: "stdio", Command: []string{"/usr/local/bin/mcp-server"}},
				}
			},
			wantOK: true,
		},
		{
			name: "valid auto with URL",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "auto", TransportMode: "auto", URL: "https://example.com/mcp"},
				}
			},
			wantOK: true,
		},
		{
			name: "empty mode defaults to auto",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "auto", URL: "https://example.com/mcp"},
				}
			},
			wantOK: true,
		},
		{
			name: "missing name rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{{TransportMode: "sse", URL: "https://x"}}
			},
			wantSub: "tools.mcp_servers[0].name",
		},
		{
			name: "duplicate name rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "dup", TransportMode: "sse", URL: "https://x"},
					{Name: "dup", TransportMode: "sse", URL: "https://y"},
				}
			},
			wantSub: "duplicate name",
		},
		{
			name: "unknown mode rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "websocket", URL: "https://x"},
				}
			},
			wantSub: "tools.mcp_servers[0].transport_mode",
		},
		{
			name: "sse without url rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "sse"},
				}
			},
			wantSub: "tools.mcp_servers[0].url",
		},
		{
			name: "stdio without command rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "stdio"},
				}
			},
			wantSub: "tools.mcp_servers[0].command",
		},
		{
			name: "stdio with empty binary path rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "stdio", Command: []string{""}},
				}
			},
			wantSub: "tools.mcp_servers[0].command[0]",
		},
		{
			name: "auto with neither url nor command rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "auto"},
				}
			},
			wantSub: "auto mode requires url or command",
		},
		{
			name: "negative keep_alive rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "x", TransportMode: "sse", URL: "https://x", KeepAlive: -1},
				}
			},
			wantSub: "tools.mcp_servers[0].keep_alive",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected ok, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation failure, got nil")
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("expected substring %q in error, got: %v", tc.wantSub, err)
			}
		})
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
