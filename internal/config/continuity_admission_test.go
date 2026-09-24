package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestContinuityAdmission_ModelProfileValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*config.LLMModelProfileConfig)
		field  string
	}{
		{"zero context", func(p *config.LLMModelProfileConfig) { p.ContextWindowTokens = 0 }, "context_window_tokens"},
		{"negative context", func(p *config.LLMModelProfileConfig) { p.ContextWindowTokens = -1 }, "context_window_tokens"},
		{"zero output", func(p *config.LLMModelProfileConfig) { v := 0; p.DefaultMaxTokens = &v }, "default_max_tokens"},
		{"negative output", func(p *config.LLMModelProfileConfig) { v := -1; p.DefaultMaxTokens = &v }, "default_max_tokens"},
		{"input cost", func(p *config.LLMModelProfileConfig) { p.CostOverrides.InputPer1M = -1 }, "cost_overrides"},
		{"output cost", func(p *config.LLMModelProfileConfig) { p.CostOverrides.OutputPer1M = -1 }, "cost_overrides"},
		{"reasoning cost", func(p *config.LLMModelProfileConfig) { p.CostOverrides.ReasoningPer1M = -1 }, "cost_overrides"},
		{"schema mode", func(p *config.LLMModelProfileConfig) { p.JSONSchemaMode = "unsupported" }, "json_schema_mode"},
		{"negative retries", func(p *config.LLMModelProfileConfig) { p.MaxRetries = -1 }, "max_retries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultsForCore()
			maxTokens := 128000
			profile := config.LLMModelProfileConfig{ContextWindowTokens: 1000000, DefaultMaxTokens: &maxTokens, CostOverrides: &config.LLMCostOverridesConfig{}}
			cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{cfg.LLM.Model: profile}
			if err := cfg.ValidateCore(); err != nil {
				t.Fatalf("valid large-capacity profile refused: %v", err)
			}
			tc.mutate(&profile)
			cfg.LLM.ModelProfiles[cfg.LLM.Model] = profile
			err := cfg.ValidateCore()
			if err == nil || !strings.Contains(err.Error(), "llm.model_profiles") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error=%v, want model profile field %s", err, tc.field)
			}
		})
	}
}

func TestContinuityAdmission_ProviderWaitAndQueueConfiguration(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Config){
		"timeout": func(c *config.Config) { c.LLM.NetworkDefaults.Timeout = -1; c.LLM.CustomProviders[0].Timeout = -1 },
		"max_retries": func(c *config.Config) {
			c.LLM.NetworkDefaults.MaxRetries = -1
			c.LLM.CustomProviders[0].MaxRetries = -1
		},
		"retry_backoff_initial": func(c *config.Config) {
			c.LLM.NetworkDefaults.RetryBackoffInitial = -1
			c.LLM.CustomProviders[0].RetryBackoffInitial = -1
		},
		"retry_backoff_max": func(c *config.Config) {
			c.LLM.NetworkDefaults.RetryBackoffMax = -1
			c.LLM.CustomProviders[0].RetryBackoffMax = -1
		},
		"concurrency": func(c *config.Config) {
			c.LLM.NetworkDefaults.Concurrency = -1
			c.LLM.CustomProviders[0].Concurrency = -1
		},
		"buffer_size": func(c *config.Config) {
			c.LLM.NetworkDefaults.BufferSize = -1
			c.LLM.CustomProviders[0].BufferSize = -1
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultsForCore()
			cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{{Name: "fixture-provider", BaseURL: "https://model.example.test/v1", APIKeyEnvVar: "FAKE_TEST_KEY", Models: []string{"model"}}}
			if err := cfg.ValidateCore(); err != nil {
				t.Fatal(err)
			}
			mutate(cfg)
			if err := cfg.ValidateCore(); err == nil || !strings.Contains(err.Error(), "llm.custom_providers[0]."+name) {
				t.Fatalf("invalid provider override: %v", err)
			}
			cfg.LLM.CustomProviders = nil
			if err := cfg.ValidateCore(); err == nil || !strings.Contains(err.Error(), "llm.network_defaults."+name) {
				t.Fatalf("invalid network default: %v", err)
			}
		})
	}
}

func TestContinuityAdmission_EventLifecycleValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		field  string
	}{
		{"missing driver", func(c *config.Config) { c.Events.Driver = "" }, "driver"},
		{"unknown driver", func(c *config.Config) { c.Events.Driver = "unknown" }, "driver"},
		{"no subscribers", func(c *config.Config) { c.Events.MaxSubscribersPerSession = 0 }, "max_subscribers_per_session"},
		{"no subscriber buffer", func(c *config.Config) { c.Events.SubscriberBufferSize = 0 }, "subscriber_buffer_size"},
		{"no idle timeout", func(c *config.Config) { c.Events.IdleTimeout = 0 }, "idle_timeout"},
		{"no drop window", func(c *config.Config) { c.Events.DropWindow = 0 }, "drop_window"},
		{"negative replay", func(c *config.Config) { c.Events.ReplayBufferSize = -1 }, "replay_buffer_size"},
		{"unknown state", func(c *config.Config) { c.Events.StateDriver = "unknown" }, "state_driver"},
		{"missing sqlite dsn", func(c *config.Config) { c.Events.StateDriver = "sqlite"; c.Events.StateDSN = "" }, "state_dsn"},
		{"missing postgres dsn", func(c *config.Config) { c.Events.StateDriver = "postgres"; c.Events.StateDSN = "" }, "state_dsn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultsForCore()
			cfg.Events.Driver = "durable"
			if err := cfg.ValidateCore(); err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err := cfg.ValidateCore()
			if err == nil || !strings.Contains(err.Error(), "events."+tc.field) {
				t.Fatalf("error=%v, want events.%s", err, tc.field)
			}
		})
	}
	cfg := defaultsForCore()
	cfg.Events.Driver, cfg.Events.StateDriver, cfg.Events.ReplayBufferSize = "durable", "inmem", 0
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("explicit disabled replay refused: %v", err)
	}
}
