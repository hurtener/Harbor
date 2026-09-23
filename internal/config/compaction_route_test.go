package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestMemoryCompactionRoute_LoadExactSelector(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromBytes(t.Context(), []byte(string(fixture)+`
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
`))
	if err != nil {
		t.Fatal(err)
	}
	want := config.MemorySummarizerProviderRoute{RouteID: "maintenance-route", RouteGeneration: 9007199254740993127,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
	if cfg.Memory.BudgetTokens != 64000 || cfg.Memory.Summarizer.ProviderRoute == nil || *cfg.Memory.Summarizer.ProviderRoute != want {
		t.Fatalf("YAML budget/selector changed: %+v", cfg.Memory)
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
