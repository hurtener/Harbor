package config

import (
	"strings"
	"testing"
)

func TestValidateLLMProviderRoute_EmptyIsOptionalAndConfiguredIsPinned(t *testing.T) {
	if err := validateLLMProviderRoute(LLMProviderRouteConfig{}); err != nil {
		t.Fatalf("empty optional route config: %v", err)
	}
	valid := LLMProviderRouteConfig{ResolverURL: "https://resolver.example.test/provider-route", AuthTokenEnv: "HARBOR_PROVIDER_ROUTE_TOKEN", RuntimeID: "runtime-a"}
	if err := validateLLMProviderRoute(valid); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, mutate := range map[string]func(*LLMProviderRouteConfig){
		"plaintext egress":  func(c *LLMProviderRouteConfig) { c.ResolverURL = "http://resolver.example.test/provider-route" },
		"missing token env": func(c *LLMProviderRouteConfig) { c.AuthTokenEnv = "" },
		"missing runtime":   func(c *LLMProviderRouteConfig) { c.RuntimeID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			got := valid
			mutate(&got)
			if err := validateLLMProviderRoute(got); err == nil || !strings.Contains(err.Error(), "llm.provider_route") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
