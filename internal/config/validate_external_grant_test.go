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
		Mode:           "required",
		Audience:       "harbor-runtime",
		RuntimeID:      "runtime-1",
		OrganizationID: "org-a",
		PublicKeys: map[string]string{
			"key-1": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		},
	}
}

func TestValidateLLMExternalGrant_RequiresBootFenceAndKey(t *testing.T) {
	c := &Config{}
	c.LLM.ExternalGrant = validExternalGrantConfig(t)
	if err := c.validateLLMExternalGrant(); err != nil {
		t.Fatalf("valid external-grant config: %v", err)
	}

	for name, mutate := range map[string]func(*LLMExternalGrantConfig){
		"mode":         func(g *LLMExternalGrantConfig) { g.Mode = "mystery" },
		"audience":     func(g *LLMExternalGrantConfig) { g.Audience = "" },
		"runtime":      func(g *LLMExternalGrantConfig) { g.RuntimeID = "" },
		"organization": func(g *LLMExternalGrantConfig) { g.OrganizationID = "" },
		"keys":         func(g *LLMExternalGrantConfig) { g.PublicKeys = map[string]string{"key-1": "not-a-key"} },
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
}

func TestValidateLLMExternalGrant_DisabledNeedsNoKeys(t *testing.T) {
	c := &Config{}
	c.LLM.ExternalGrant.Mode = "disabled"
	if err := c.validateLLMExternalGrant(); err != nil {
		t.Fatalf("disabled external-grant config: %v", err)
	}
}
