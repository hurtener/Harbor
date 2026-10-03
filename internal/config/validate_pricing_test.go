package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm/pricing"
)

func TestMonetaryPricing_ConfigRejectsIncompleteCatalog(t *testing.T) {
	cfg, err := config.Load(t.Context(), "testdata/valid_minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLM.PricingManifests = []pricing.Manifest{{ID: "fixture", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{{Provider: "openai", Model: "fixture-v1", ModelVersion: "fixture-v1", ImmutableModelVersion: true, IncludesAllCharges: true}}}}
	if err = cfg.Validate(); err == nil || !strings.Contains(err.Error(), "llm.pricing_manifests") {
		t.Fatal("incomplete catalog was accepted", err)
	}
}
