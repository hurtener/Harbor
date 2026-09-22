package serve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestConfigureStockProviderRoute_AuthorityAndConfigurationBoundaries(t *testing.T) {
	t.Run("injected resolver conflicts with stock authority", func(t *testing.T) {
		opts := &Options{ProviderRoute: llm.ProviderRouteConfig{Resolver: admissionRouteResolver{}}}
		client, err := configureStockProviderRoute(config.LLMProviderRouteConfig{ResolverURL: "https://resolver.example"}, opts, func(string) (string, bool) {
			return "token", true
		})
		if client != nil || err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("client=%v err=%v, want stock/injected conflict", client, err)
		}
	})

	t.Run("missing token fails before client construction", func(t *testing.T) {
		opts := &Options{}
		client, err := configureStockProviderRoute(config.LLMProviderRouteConfig{
			ResolverURL: "https://resolver.example", AuthTokenEnv: "HARBOR_TEST_ROUTE_TOKEN",
		}, opts, func(name string) (string, bool) {
			if name != "HARBOR_TEST_ROUTE_TOKEN" {
				t.Fatalf("lookup name = %q", name)
			}
			return "", false
		})
		if client != nil || err == nil || !strings.Contains(err.Error(), "is unset") {
			t.Fatalf("client=%v err=%v, want missing-token refusal", client, err)
		}
	})

	t.Run("invalid URL is rejected", func(t *testing.T) {
		opts := &Options{}
		client, err := configureStockProviderRoute(config.LLMProviderRouteConfig{
			ResolverURL: "://invalid", AuthTokenEnv: "TOKEN",
		}, opts, func(string) (string, bool) { return "secret", true })
		if client != nil || err == nil || !strings.Contains(err.Error(), "construct stock resolver") {
			t.Fatalf("client=%v err=%v, want construction failure", client, err)
		}
	})

	t.Run("valid stock resolver owns the configured runtime route", func(t *testing.T) {
		opts := &Options{}
		client, err := configureStockProviderRoute(config.LLMProviderRouteConfig{
			ResolverURL: "https://resolver.example", AuthTokenEnv: "TOKEN", RuntimeID: "runtime-a", Timeout: time.Second,
		}, opts, func(string) (string, bool) { return "secret", true })
		if err != nil {
			t.Fatalf("configureStockProviderRoute: %v", err)
		}
		if client == nil || opts.ProviderRoute.Resolver != client || opts.ProviderRoute.RuntimeID != "runtime-a" {
			t.Fatalf("client=%v route=%+v", client, opts.ProviderRoute)
		}
		if err := client.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
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
