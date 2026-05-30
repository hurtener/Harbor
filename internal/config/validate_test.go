package config_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		// Phase 83v (D-162) — CORS allowlist validation.
		{
			"allowed_origins entry empty",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"   "}
			},
			"server.allowed_origins[0]",
		},
		{
			"allowed_origins entry missing scheme",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"console.example.com"}
			},
			"server.allowed_origins[0]",
		},
		{
			"allowed_origins entry with path",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"https://console.example.com/foo"}
			},
			"server.allowed_origins[0]",
		},
		{
			"allowed_origins entry with query",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"https://console.example.com?x=1"}
			},
			"server.allowed_origins[0]",
		},
		{
			"allowed_origins wildcard rejected without dev flag",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"*"}
				c.Server.CORSDevAllowAny = false
			},
			"server.allowed_origins[0]",
		},
		{
			"allowed_origins ftp scheme rejected",
			func(c *config.Config) {
				c.Server.AllowedOrigins = []string{"ftp://console.example.com"}
			},
			"server.allowed_origins[0]",
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
			"negative repair_attempts",
			func(c *config.Config) { c.Governance.RepairAttempts = -1 },
			"governance.repair_attempts",
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
		// Phase 36a / 36b governance identity-tier validation.
		{
			"identity tier empty name",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"": {BudgetCeilingUSD: 1.0},
				}
			},
			"governance.identity_tiers",
		},
		{
			"identity tier negative budget ceiling",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {BudgetCeilingUSD: -1.0},
				}
			},
			"governance.identity_tiers[\"premium\"].budget_ceiling_usd",
		},
		{
			"identity tier negative max_tokens",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {MaxTokens: -1},
				}
			},
			"governance.identity_tiers[\"premium\"].max_tokens",
		},
		{
			"identity tier negative rate-limit capacity",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {RateLimit: config.GovernanceRateLimitConfig{Capacity: -1}},
				}
			},
			"governance.identity_tiers[\"premium\"].rate_limit.capacity",
		},
		{
			"identity tier negative rate-limit refill_tokens",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {RateLimit: config.GovernanceRateLimitConfig{RefillTokens: -1}},
				}
			},
			"governance.identity_tiers[\"premium\"].rate_limit.refill_tokens",
		},
		{
			"identity tier negative rate-limit refill_interval",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {RateLimit: config.GovernanceRateLimitConfig{RefillInterval: -1}},
				}
			},
			"governance.identity_tiers[\"premium\"].rate_limit.refill_interval",
		},
		{
			"identity tier refill set without capacity",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {RateLimit: config.GovernanceRateLimitConfig{
						RefillTokens:   10,
						RefillInterval: time.Second,
						Capacity:       0,
					}},
				}
			},
			"governance.identity_tiers[\"premium\"].rate_limit.capacity",
		},
		{
			"identity tier refill_tokens set without refill_interval",
			func(c *config.Config) {
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {RateLimit: config.GovernanceRateLimitConfig{
						Capacity:     100,
						RefillTokens: 10,
					}},
				}
			},
			"governance.identity_tiers[\"premium\"].rate_limit.refill_interval",
		},
		{
			"default_tier names unknown tier",
			func(c *config.Config) {
				c.Governance.DefaultTier = "phantom"
				c.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
					"premium": {BudgetCeilingUSD: 1.0},
				}
			},
			"governance.default_tier",
		},
		// Phase 34 corrections-profile enum validation.
		{
			"corrections invalid message_ordering",
			func(c *config.Config) {
				c.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
					"m": {ContextWindowTokens: 4096, Corrections: &config.LLMCorrectionsProfileConfig{
						MessageOrdering: "reverse-then-shuffle",
					}},
				}
			},
			"corrections.message_ordering",
		},
		{
			"corrections invalid schema_mode",
			func(c *config.Config) {
				c.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
					"m": {ContextWindowTokens: 4096, Corrections: &config.LLMCorrectionsProfileConfig{
						SchemaMode: "guesswork",
					}},
				}
			},
			"corrections.schema_mode",
		},
		{
			"corrections invalid reasoning_effort_routing",
			func(c *config.Config) {
				c.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
					"m": {ContextWindowTokens: 4096, Corrections: &config.LLMCorrectionsProfileConfig{
						ReasoningEffortRouting: "wild-guess",
					}},
				}
			},
			"corrections.reasoning_effort_routing",
		},
		{
			"corrections invalid response_format_shape",
			func(c *config.Config) {
				c.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
					"m": {ContextWindowTokens: 4096, Corrections: &config.LLMCorrectionsProfileConfig{
						ResponseFormatShape: "bespoke",
					}},
				}
			},
			"corrections.response_format_shape",
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

// Phase 83v (D-162) — a valid CORS allowlist passes validation.
func TestValidate_AcceptsCORSAllowedOrigins(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Server.AllowedOrigins = []string{
		"https://console.example.com",
		"https://console.example.com:8443",
		"http://127.0.0.1:18790",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected valid CORS allowlist: %v", err)
	}
}

// Phase 83v (D-162) — the dev-only wildcard escape hatch passes
// validation when the operator explicitly opts in.
func TestValidate_AcceptsCORSDevAllowAny(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Server.CORSDevAllowAny = true
	cfg.Server.AllowedOrigins = []string{"*"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected dev-only wildcard with explicit opt-in: %v", err)
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

// TestValidate_ToolsGrantedScopes — Phase 83m (Item 6, D-156): the
// `tools.granted_scopes` operator-declared scope list accepts any
// non-empty string list; an empty list is valid; a blank entry is
// rejected with a path-prefixed error so the operator sees which
// index needs attention.
func TestValidate_ToolsGrantedScopes(t *testing.T) {
	t.Run("empty_list_passes", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools = config.ToolsConfig{}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("empty granted_scopes rejected: %v", err)
		}
	})
	t.Run("non_empty_strings_pass", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools = config.ToolsConfig{GrantedScopes: []string{"read:repo", "write:issues", "admin"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("operator-defined scopes rejected: %v", err)
		}
	})
	t.Run("blank_entry_rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools = config.ToolsConfig{GrantedScopes: []string{"read:repo", "   ", "admin"}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate accepted blank granted_scopes entry")
		}
		if !strings.Contains(err.Error(), "tools.granted_scopes[1]") {
			t.Errorf("err missing path index: %v", err)
		}
	})
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
		// Phase 26b — per-server policy + per-tool override validation.
		{
			name: "valid per-server policy passes",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						Policy: &config.ToolPolicyConfig{
							MaxAttempts: 2, TimeoutMS: 60000,
							RetryOn: []string{"transient", "timeout"},
						}},
				}
			},
			wantOK: true,
		},
		{
			name: "valid per-tool overrides pass",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						ToolPolicies: map[string]config.ToolPolicyConfig{
							"slow":  {MaxAttempts: 1, TimeoutMS: 60000},
							"flaky": {MaxAttempts: 5},
						}},
				}
			},
			wantOK: true,
		},
		{
			// D-175 per-field fall-through: max_attempts omitted (0) is
			// allowed and inherits the default attempt count; only a
			// `policy:` setting timeout_ms alone is exactly the documented
			// "set only timeout_ms" case. (Wave-audit WARN-2 fix.)
			name: "policy max_attempts omitted (only timeout_ms) accepted",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						Policy: &config.ToolPolicyConfig{TimeoutMS: 1000}},
				}
			},
			wantOK: true,
		},
		{
			name: "policy max_attempts negative rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						Policy: &config.ToolPolicyConfig{MaxAttempts: -1, TimeoutMS: 1000}},
				}
			},
			wantSub: "tools.mcp_servers[0].policy.max_attempts",
		},
		{
			name: "policy negative timeout rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						Policy: &config.ToolPolicyConfig{MaxAttempts: 2, TimeoutMS: -1}},
				}
			},
			wantSub: "tools.mcp_servers[0].policy.timeout_ms",
		},
		{
			name: "policy unknown retry_on rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						Policy: &config.ToolPolicyConfig{
							MaxAttempts: 2, RetryOn: []string{"bogus"}}},
				}
			},
			wantSub: "policy.retry_on[0]",
		},
		{
			name: "tool_policies empty key rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						ToolPolicies: map[string]config.ToolPolicyConfig{
							"  ": {MaxAttempts: 2}}},
				}
			},
			wantSub: "tools.mcp_servers[0].tool_policies",
		},
		{
			name: "tool_policies invalid override rejected",
			mutate: func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{
					{Name: "p", TransportMode: "sse", URL: "https://x",
						ToolPolicies: map[string]config.ToolPolicyConfig{
							"slow": {MaxAttempts: -1}}},
				}
			},
			wantSub: "max_attempts",
		},
	}
	for _, tc := range cases {

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

// TestValidateTools_Entries exercises the per-tool catalog-wiring
// declaration validator (Phase 64a / D-090). Covers: unknown policy,
// invalid binding scope, duplicate name, empty middleware block, the
// tagged-policy require_tags check, and the happy paths.
func TestValidateTools_Entries(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantOK  bool
		wantSub string
	}{
		{
			name:   "empty entries passes",
			mutate: func(c *config.Config) { c.Tools.Entries = nil },
			wantOK: true,
		},
		{
			name: "approval deny-all entry passes",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "delete_doc", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
				}
			},
			wantOK: true,
		},
		{
			name: "approval approve-all entry passes",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "read_doc", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
				}
			},
			wantOK: true,
		},
		{
			name: "approval tagged entry passes",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "write_prod", Approval: &config.ToolApprovalConfig{
						Policy:      "tagged",
						RequireTags: []string{"sensitive"},
					}},
				}
			},
			wantOK: true,
		},
		{
			name: "oauth entry passes",
			mutate: func(c *config.Config) {
				// D-095: an entry's `oauth.provider` MUST resolve to a
				// `tools.oauth_providers[].name` declared in the same
				// config — declare the provider so the cross-check
				// passes.
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{
					{
						Name: "github", Driver: "oauth2",
						ClientIDEnv: "GITHUB_OAUTH_CLIENT_ID", ClientSecretEnv: "GITHUB_OAUTH_CLIENT_SECRET",
					},
				}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "github_read", OAuth: &config.ToolOAuthConfig{
						Provider: "github", BindingScope: "user",
					}},
				}
			},
			wantOK: true,
		},
		{
			name: "approval AND oauth on the same entry passes",
			mutate: func(c *config.Config) {
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{
					{
						Name: "github", Driver: "oauth2",
						ClientIDEnv: "GITHUB_OAUTH_CLIENT_ID", ClientSecretEnv: "GITHUB_OAUTH_CLIENT_SECRET",
					},
				}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
				c.Tools.Entries = []config.ToolEntryConfig{
					{
						Name:     "github_write",
						Approval: &config.ToolApprovalConfig{Policy: "deny-all"},
						OAuth:    &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"},
					},
				}
			},
			wantOK: true,
		},
		{
			name: "empty name rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
				}
			},
			wantSub: "tools.entries[0].name",
		},
		{
			name: "duplicate name rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "x", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
					{Name: "x", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
				}
			},
			wantSub: "duplicate entry",
		},
		{
			name: "no middleware (empty entry) rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{{Name: "x"}}
			},
			wantSub: "at least one of",
		},
		{
			name: "unknown approval policy rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "x", Approval: &config.ToolApprovalConfig{Policy: "bogus"}},
				}
			},
			wantSub: "tools.entries[0].approval.policy",
		},
		{
			name: "tagged policy without require_tags rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "x", Approval: &config.ToolApprovalConfig{Policy: "tagged"}},
				}
			},
			wantSub: "tools.entries[0].approval.require_tags",
		},
		{
			name: "empty oauth provider rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "x", OAuth: &config.ToolOAuthConfig{BindingScope: "user"}},
				}
			},
			wantSub: "tools.entries[0].oauth.provider",
		},
		{
			name: "invalid oauth binding_scope rejected",
			mutate: func(c *config.Config) {
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "x", OAuth: &config.ToolOAuthConfig{
						Provider: "p", BindingScope: "bogus",
					}},
				}
			},
			wantSub: "tools.entries[0].oauth.binding_scope",
		},
	}
	for _, tc := range cases {

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

// TestValidateTools_OAuthProviders exercises the D-095 operator-config
// validator: per-provider invariants + the cross-validation that every
// `tools.entries[].oauth.provider` reference resolves.
func TestValidateTools_OAuthProviders(t *testing.T) {
	validProvider := config.ToolOAuthProviderConfig{
		Name: "github", Driver: "oauth2",
		ClientIDEnv: "GITHUB_OAUTH_CLIENT_ID", ClientSecretEnv: "GITHUB_OAUTH_CLIENT_SECRET",
	}
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantOK  bool
		wantSub string
	}{
		{
			name:   "empty providers passes",
			mutate: func(c *config.Config) { c.Tools.OAuthProviders = nil; c.Tools.OAuthTokenKEKEnv = "" },
			wantOK: true,
		},
		{
			name: "valid single provider passes",
			mutate: func(c *config.Config) {
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{validProvider}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantOK: true,
		},
		{
			name: "missing KEK env when providers declared rejected",
			mutate: func(c *config.Config) {
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{validProvider}
				c.Tools.OAuthTokenKEKEnv = ""
			},
			wantSub: "tools.oauth_token_kek_env",
		},
		{
			name: "empty provider name rejected",
			mutate: func(c *config.Config) {
				p := validProvider
				p.Name = ""
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{p}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "tools.oauth_providers[0].name",
		},
		{
			name: "duplicate provider name rejected",
			mutate: func(c *config.Config) {
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{validProvider, validProvider}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "duplicate provider name",
		},
		{
			name: "empty driver rejected",
			mutate: func(c *config.Config) {
				p := validProvider
				p.Driver = ""
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{p}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "tools.oauth_providers[0].driver",
		},
		{
			name: "unknown driver rejected",
			mutate: func(c *config.Config) {
				p := validProvider
				p.Driver = "no-such-driver"
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{p}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "no-such-driver",
		},
		{
			name: "empty client_id_env rejected",
			mutate: func(c *config.Config) {
				p := validProvider
				p.ClientIDEnv = ""
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{p}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "tools.oauth_providers[0].client_id_env",
		},
		{
			name: "empty client_secret_env rejected",
			mutate: func(c *config.Config) {
				p := validProvider
				p.ClientSecretEnv = ""
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{p}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
			},
			wantSub: "tools.oauth_providers[0].client_secret_env",
		},
		{
			name: "entry referencing unknown provider rejected",
			mutate: func(c *config.Config) {
				// Provider list declares "github" but the entry
				// references "google" — the cross-validation surfaces a
				// clear error naming both entry index and unknown
				// provider name.
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{validProvider}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "google_read", OAuth: &config.ToolOAuthConfig{
						Provider: "google", BindingScope: "user",
					}},
				}
			},
			wantSub: `unknown OAuth provider "google"`,
		},
		{
			name: "entry referencing declared provider passes",
			mutate: func(c *config.Config) {
				c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{validProvider}
				c.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
				c.Tools.Entries = []config.ToolEntryConfig{
					{Name: "github_read", OAuth: &config.ToolOAuthConfig{
						Provider: "github", BindingScope: "user",
					}},
				}
			},
			wantOK: true,
		},
	}
	for _, tc := range cases {

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

// TestValidate_Planner_AcceptsReact exercises D-103 — the V1 default
// planner driver. A drift (the validator dropping `react`) would break
// every operator config that omits the planner block (the loader's
// `defaults()` populates `Driver: "react"`).
func TestValidate_Planner_AcceptsReact(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.driver=react): %v", err)
	}
}

// TestValidate_Planner_AcceptsEmptyDefault pins the validator's empty
// → "react" default. A config that omits the planner block (zero-value
// `config.PlannerConfig{}`) MUST validate so the loader's default
// landing path stays consistent with operator yaml that just leaves
// the block off.
func TestValidate_Planner_AcceptsEmptyDefault(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.driver=\"\") rejected empty-default: %v", err)
	}
}

// TestValidate_Planner_RejectsUnknownDriver pins the §13 fail-loud
// rejection of a typoed driver name. The validator catches this
// pre-boot so `harbor validate` flags the typo.
func TestValidate_Planner_RejectsUnknownDriver(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "no-such-driver-zzz"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate(planner.driver=no-such-driver-zzz) returned nil, want error")
	}
	// The error message MUST list the allowed values so the operator
	// sees the fix.
	if !strings.Contains(err.Error(), "react") {
		t.Fatalf("Validate err = %q, want it to list allowed drivers", err.Error())
	}
}

// TestValidate_Planner_RejectsNegativeMaxSteps pins the loud rejection
// of nonsensical MaxSteps values.
func TestValidate_Planner_RejectsNegativeMaxSteps(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react", MaxSteps: -1}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate(planner.max_steps=-1) returned nil, want error")
	}
}

// TestValidate_Planner_AcceptsZeroMaxSteps_AsDriverDefault confirms
// MaxSteps=0 is the documented "use driver default" sentinel.
func TestValidate_Planner_AcceptsZeroMaxSteps_AsDriverDefault(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react", MaxSteps: 0}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.max_steps=0) rejected the driver-default sentinel: %v", err)
	}
}

// TestValidate_Planner_AcceptsPositiveMaxSteps pins the happy path.
func TestValidate_Planner_AcceptsPositiveMaxSteps(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react", MaxSteps: 24}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.max_steps=24): %v", err)
	}
}

// TestValidate_Planner_RejectsNegativeSpawnDepth pins the loud rejection
// of a negative absolute_max_spawn_depth (Phase 107e — D-170).
func TestValidate_Planner_RejectsNegativeSpawnDepth(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react", AbsoluteMaxSpawnDepth: -1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate(planner.absolute_max_spawn_depth=-1) returned nil, want error")
	}
}

// TestPlannerConfig_SpawnDepthCap pins the accessor: zero/unset resolves
// to the dev-runtime default of 4; a positive value is honoured verbatim.
func TestPlannerConfig_SpawnDepthCap(t *testing.T) {
	t.Parallel()
	if got := (config.PlannerConfig{}).SpawnDepthCap(); got != 4 {
		t.Errorf("unset SpawnDepthCap() = %d, want 4 (default)", got)
	}
	if got := (config.PlannerConfig{AbsoluteMaxSpawnDepth: 7}).SpawnDepthCap(); got != 7 {
		t.Errorf("SpawnDepthCap() = %d, want 7", got)
	}
}

// TestValidate_Planner_AcceptsExtraGuidance pins the Phase 83a
// `planner.extra_guidance` key (RFC §6.2). The validator imposes no
// rule beyond "string" — operator copy is operator copy — so an
// arbitrary non-empty value must validate.
func TestValidate_Planner_AcceptsExtraGuidance(t *testing.T) {
	t.Parallel()
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{
		Driver:        "react",
		ExtraGuidance: "Always answer in formal English; cite sources.",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.extra_guidance set): %v", err)
	}
}

// TestValidate_Planner_ReasoningReplay covers the Phase 83e (D-148)
// reasoning_replay enum: empty (→never), `never`, and `text` validate;
// any other value fails loud pre-boot with the allowlist in the error.
func TestValidate_Planner_ReasoningReplay(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "never", "text"} {
		t.Run("accepts/"+mode, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			cfg.Planner = config.PlannerConfig{Driver: "react", ReasoningReplay: mode}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(planner.reasoning_replay=%q): %v", mode, err)
			}
		})
	}
	for _, mode := range []string{"provider_native", "always", "Never", "yes"} {
		t.Run("rejects/"+mode, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			cfg.Planner = config.PlannerConfig{Driver: "react", ReasoningReplay: mode}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate(planner.reasoning_replay=%q) returned nil, want error", mode)
			}
			if !strings.Contains(err.Error(), "reasoning_replay") {
				t.Errorf("err = %q, want it to name the planner.reasoning_replay field", err.Error())
			}
		})
	}
}

// TestValidate_Planner_MaxToolExamplesPerTool covers the Phase 83b
// (D-144) `planner.max_tool_examples_per_tool` knob: zero (→driver
// default 3) and positive values validate; a negative value fails
// loud pre-boot with the field named in the error.
func TestValidate_Planner_MaxToolExamplesPerTool(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 3, 10} {
		t.Run("accepts", func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			cfg.Planner = config.PlannerConfig{Driver: "react", MaxToolExamplesPerTool: n}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(planner.max_tool_examples_per_tool=%d): %v", n, err)
			}
		})
	}
	cfg := mustLoadValid(t)
	cfg.Planner = config.PlannerConfig{Driver: "react", MaxToolExamplesPerTool: -1}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate(planner.max_tool_examples_per_tool=-1) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "max_tool_examples_per_tool") {
		t.Errorf("err = %q, want it to name the planner.max_tool_examples_per_tool field", err.Error())
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

// TestValidateCLI_DevHotReload exercises the Phase 65 (D-099)
// `cli.dev_hot_reload` validator: unknown policy rejected, negative
// drain timeout rejected, enabled+empty roots rejected, blank root
// rejected, defaults pass.
func TestValidateCLI_DevHotReload(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*config.Config)
		wantErr     bool
		errFragment string
	}{
		{
			name:    "defaults_from_loader_pass",
			mutate:  func(_ *config.Config) {},
			wantErr: false,
		},
		{
			name: "unknown_policy_rejected",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.Policy = "rebuild-the-universe"
			},
			wantErr:     true,
			errFragment: "cli.dev_hot_reload.policy",
		},
		{
			name: "negative_drain_timeout_rejected",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.DrainTimeout = -1 * time.Second
			},
			wantErr:     true,
			errFragment: "cli.dev_hot_reload.drain_timeout",
		},
		{
			name: "zero_drain_timeout_accepted_treated_as_default",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.DrainTimeout = 0
			},
			wantErr: false,
		},
		{
			name: "enabled_with_no_roots_rejected",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.WatchRoots = nil
			},
			wantErr:     true,
			errFragment: "cli.dev_hot_reload.watch_roots",
		},
		{
			name: "disabled_via_enabled_false_accepts_empty_roots",
			mutate: func(c *config.Config) {
				f := false
				c.CLI.DevHotReload.Enabled = &f
				c.CLI.DevHotReload.WatchRoots = nil
			},
			wantErr: false,
		},
		{
			name: "disabled_via_policy_disabled_accepts_empty_roots",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.Policy = config.DevHotReloadPolicyDisabled
				c.CLI.DevHotReload.WatchRoots = nil
			},
			wantErr: false,
		},
		{
			name: "blank_root_rejected",
			mutate: func(c *config.Config) {
				c.CLI.DevHotReload.WatchRoots = []string{".harbor/agents", "   "}
			},
			wantErr:     true,
			errFragment: "cli.dev_hot_reload.watch_roots[1]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatalf("Validate() = nil, want non-nil")
				}
				if tc.errFragment != "" && !strings.Contains(err.Error(), tc.errFragment) {
					t.Errorf("Validate() = %q, want fragment %q", err.Error(), tc.errFragment)
				}
			default:
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
			}
		})
	}
}
