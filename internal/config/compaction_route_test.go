package config_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestMemorySummarizer_OutputBudgetConfiguration(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{0, 8192, 128000} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			data := []byte(string(fixture) + fmt.Sprintf("\nmemory:\n  budget_tokens: 64000\n  summarizer:\n    max_tokens: %d\n", value))
			cfg, err := config.LoadFromBytes(t.Context(), data)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Memory.Summarizer.MaxTokens != value || cfg.Memory.BudgetTokens != 64000 || cfg.Memory.Summarizer.ProviderRoute != nil {
				t.Fatalf("output budget changed unrelated memory policy: %+v", cfg.Memory)
			}
			t.Setenv("HARBOR_MEMORY_SUMMARIZER_MAX_TOKENS", "4096")
			cfg, err = config.LoadFromBytes(t.Context(), data)
			if err != nil || cfg.Memory.Summarizer.MaxTokens != 4096 {
				t.Fatalf("environment override not applied: cfg=%v err=%v", cfg, err)
			}
		})
	}
	cfg, err := config.Load(t.Context(), validMinimalFixture)
	if err != nil || cfg.Memory.Summarizer.MaxTokens != 0 {
		t.Fatalf("omitted output allowance changed: cfg=%v err=%v", cfg, err)
	}
	for _, value := range []string{"-1", "not-a-number", "99999999999999999999999999999"} {
		t.Run("invalid-"+value, func(t *testing.T) {
			t.Setenv("HARBOR_MEMORY_SUMMARIZER_MAX_TOKENS", value)
			if _, err := config.Load(t.Context(), validMinimalFixture); !errors.Is(err, config.ErrConfigInvalid) {
				t.Fatalf("invalid maintenance allowance accepted: %v", err)
			}
		})
	}
	if _, err := config.LoadFromBytes(t.Context(), []byte(string(fixture)+"\nmemory:\n  summarizer:\n    max_tokens: -1\n")); !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("negative YAML maintenance allowance accepted: %v", err)
	}
}

func TestMemorySummarizer_CallAllowanceConfiguration(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{0, 1, 16, 32, 1000} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			data := []byte(string(fixture) + fmt.Sprintf("\nmemory:\n  recent_turns: 100\n  summarizer:\n    max_calls: %d\n", value))
			cfg, err := config.LoadFromBytes(t.Context(), data)
			if err != nil || cfg.Memory.Summarizer.MaxCalls != value || cfg.Memory.RecentTurnsResolved() != 100 {
				t.Fatalf("YAML allowance not applied: %v", err)
			}
			t.Setenv("HARBOR_MEMORY_SUMMARIZER_MAX_CALLS", "24")
			cfg, err = config.LoadFromBytes(t.Context(), data)
			if err != nil || cfg.Memory.Summarizer.MaxCalls != 24 {
				t.Fatalf("environment allowance not applied: %v", err)
			}
		})
	}
	for _, value := range []string{"-1", "not-a-number", "99999999999999999999999999999"} {
		t.Run("invalid-"+value, func(t *testing.T) {
			t.Setenv("HARBOR_MEMORY_SUMMARIZER_MAX_CALLS", value)
			if _, err := config.Load(t.Context(), validMinimalFixture); !errors.Is(err, config.ErrConfigInvalid) {
				t.Fatalf("invalid call allowance accepted: %v", err)
			}
		})
	}
	if _, err := config.LoadFromBytes(t.Context(), []byte(string(fixture)+"\nmemory:\n  summarizer:\n    max_calls: -1\n")); !errors.Is(err, config.ErrConfigInvalid) {
		t.Fatalf("negative YAML allowance accepted: %v", err)
	}
}

func TestMemoryCompactionRoute_EnvironmentSelection(t *testing.T) {
	// Dummy route metadata, never a provider credential. Exact uint64 generations
	// must survive environment parsing just as they do YAML parsing.
	fields := map[string]string{
		"ROUTE_ID": "maintenance-route", "ROUTE_GENERATION": "9007199254740993127",
		"PROVIDER_CONNECTION_ID": "connection", "PROVIDER_CONNECTION_GENERATION": "2",
		"CREDENTIAL_ASSET_GENERATION": "3", "MODEL_SELECTOR": "compact-alias",
	}
	const prefix = "HARBOR_MEMORY_SUMMARIZER_PROVIDER_ROUTE_"
	for name, value := range fields {
		t.Setenv(prefix+name, value)
	}
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromBytes(t.Context(), []byte(string(fixture)+"\nmemory:\n  budget_tokens: 64000\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := config.MemorySummarizerProviderRoute{RouteID: "maintenance-route", RouteGeneration: 9007199254740993127,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
	if cfg.Memory.BudgetTokens != 64000 || cfg.Memory.Summarizer.ProviderRoute == nil || *cfg.Memory.Summarizer.ProviderRoute != want {
		t.Fatalf("environment selector or YAML budget changed: %+v", cfg.Memory)
	}
	for _, value := range []string{"", "0", "-1", "18446744073709551616", "not-a-number"} {
		t.Run("invalid-generation-"+value, func(t *testing.T) {
			t.Setenv(prefix+"ROUTE_GENERATION", value)
			if _, err := config.Load(t.Context(), validMinimalFixture); !errors.Is(err, config.ErrConfigInvalid) {
				t.Fatalf("invalid environment selector accepted: %v", err)
			}
		})
	}
}

func TestMemoryCompactionRoute_EnvironmentDoesNotActivateImplicitly(t *testing.T) {
	t.Setenv("HARBOR_MEMORY_BUDGET_TOKENS", "64000")
	cfg, err := config.Load(t.Context(), validMinimalFixture)
	if err != nil || cfg.Memory.Summarizer.ProviderRoute != nil {
		t.Fatalf("unselected route materialized: cfg=%v err=%v", cfg != nil, err)
	}
	for _, value := range []string{"", "partial-route"} {
		t.Run("partial-"+value, func(t *testing.T) {
			t.Setenv("HARBOR_MEMORY_SUMMARIZER_PROVIDER_ROUTE_ROUTE_ID", value)
			if _, err := config.Load(t.Context(), validMinimalFixture); !errors.Is(err, config.ErrConfigInvalid) {
				t.Fatalf("partial environment route silently ignored: %v", err)
			}
		})
	}
}

func TestMemoryCompactionRoute_LoadExactSelector(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(string(fixture) + `
memory:
  budget_tokens: 64000
  summarizer:
    provider_route:
      route_id: maintenance-route
      route_generation: 9007199254740993127
      provider_connection_id: connection
      provider_connection_generation: 2
      credential_asset_generation: 3
      model_selector: compact-alias
`)
	cfg, err := config.LoadFromBytes(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	want := config.MemorySummarizerProviderRoute{RouteID: "maintenance-route", RouteGeneration: 9007199254740993127,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
	if cfg.Memory.BudgetTokens != 64000 || cfg.Memory.Summarizer.ProviderRoute == nil || *cfg.Memory.Summarizer.ProviderRoute != want {
		t.Fatalf("YAML budget/selector changed: %+v", cfg.Memory)
	}
	t.Setenv("HARBOR_MEMORY_SUMMARIZER_PROVIDER_ROUTE_MODEL_SELECTOR", "environment-alias")
	// Reuse the same complete YAML selector and prove only the supplied leaf wins.
	updated, err := config.LoadFromBytes(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	want.ModelSelector = "environment-alias"
	if updated.Memory.Summarizer.ProviderRoute == nil || *updated.Memory.Summarizer.ProviderRoute != want {
		t.Fatalf("environment must override one YAML leaf without losing siblings: %+v", updated.Memory.Summarizer.ProviderRoute)
	}
}

func TestMemoryCompactionRoute_RejectsIncompleteOrAmbiguousConfiguration(t *testing.T) {
	for _, field := range []string{"route", "route generation", "connection", "connection generation", "credential generation", "selector", "model"} {
		t.Run(field, func(t *testing.T) {
			cfg := defaultsForCore()
			route := &config.MemorySummarizerProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
				ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
			cfg.Memory.Summarizer.ProviderRoute = route
			switch field {
			case "route":
				route.RouteID = " "
			case "route generation":
				route.RouteGeneration = 0
			case "connection":
				route.ProviderConnectionID = ""
			case "connection generation":
				route.ProviderConnectionGeneration = 0
			case "credential generation":
				route.CredentialAssetGeneration = 0
			case "selector":
				route.ModelSelector = ""
			case "model":
				cfg.Memory.Summarizer.Model = "other-model"
			}
			if err := cfg.ValidateCore(); err == nil || !strings.Contains(err.Error(), "memory.summarizer") {
				t.Fatalf("invalid configuration accepted: %v", err)
			}
		})
	}
}
