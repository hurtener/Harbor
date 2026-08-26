package serve

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestConfigureStockProviderRoute_EmptyDoesNoEnvironmentWork(t *testing.T) {
	lookups := 0
	client, err := configureStockProviderRoute(config.LLMProviderRouteConfig{}, &Options{}, func(string) (string, bool) {
		lookups++
		return "", false
	})
	if err != nil || client != nil || lookups != 0 {
		t.Fatalf("client=%v lookups=%d err=%v", client, lookups, err)
	}
}

func TestBoot_ProviderRouteRefusesPartialAndNonBifrostConfiguration(t *testing.T) {
	for name, route := range map[string]llm.ProviderRouteConfig{
		"missing resolver": {RuntimeID: "runtime"},
		"non-Bifrost":      {Resolver: admissionRouteResolver{}, RuntimeID: "runtime"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := baseOptions(t)
			opts.ProviderRoute = route
			if _, err := Boot(context.Background(), opts); !errors.Is(err, llm.ErrInvalidConfig) {
				t.Fatalf("Boot error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
