package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func validExternalGrantConfig(t *testing.T) LLMExternalGrantConfig {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return LLMExternalGrantConfig{
		Mode:                    "required",
		Audience:                "harbor-runtime",
		RuntimeID:               "runtime-1",
		AuthorizedOrganizations: []string{"org-a", "org-b"},
		PublicKeys: map[string]string{
			"key-1": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		},
	}
}

func TestValidateLLMExternalGrant_RequiresIssuerFenceAndKey(t *testing.T) {
	c := &Config{}
	c.LLM.ExternalGrant = validExternalGrantConfig(t)
	if err := c.validateLLMExternalGrant(); err != nil {
		t.Fatalf("valid external-grant config: %v", err)
	}

	for name, mutate := range map[string]func(*LLMExternalGrantConfig){
		"mode":                    func(g *LLMExternalGrantConfig) { g.Mode = "mystery" },
		"route_mode":              func(g *LLMExternalGrantConfig) { g.RouteMode = "caller_selected" },
		"audience":                func(g *LLMExternalGrantConfig) { g.Audience = "" },
		"runtime":                 func(g *LLMExternalGrantConfig) { g.RuntimeID = "" },
		"authorized_organization": func(g *LLMExternalGrantConfig) { g.AuthorizedOrganizations = []string{""} },
		"keys":                    func(g *LLMExternalGrantConfig) { g.PublicKeys = map[string]string{"key-1": "not-a-key"} },
	} {
		t.Run(name, func(t *testing.T) {
			got := validExternalGrantConfig(t)
			mutate(&got)
			c.LLM.ExternalGrant = got
			if err := c.validateLLMExternalGrant(); err == nil {
				t.Fatal("invalid external-grant config accepted")
			}
		})
	}
	for _, routeMode := range []string{"", "runtime_default", "coordinator_bound"} {
		t.Run("route_mode_"+routeMode, func(t *testing.T) {
			got := validExternalGrantConfig(t)
			got.RouteMode = routeMode
			c.LLM.ExternalGrant = got
			if err := c.validateLLMExternalGrant(); err != nil {
				t.Fatalf("valid route mode %q: %v", routeMode, err)
			}
		})
	}
}

func TestValidateLLMExternalGrant_DisabledNeedsNoKeys(t *testing.T) {
	c := &Config{}
	c.LLM.ExternalGrant.Mode = "disabled"
	if err := c.validateLLMExternalGrant(); err != nil {
		t.Fatalf("disabled external-grant config: %v", err)
	}
}
