package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
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

func TestValidateLLMExternalGrant_CoordinatorTransport(t *testing.T) {
	valid := ExternalGrantCoordinatorConfig{
		ReceiptURL:        "https://coordinator.example.test/v1/receipts",
		TopUpURL:          "https://coordinator.example.test/v1/grants/top-up",
		AuthTokenEnv:      "HARBOR_COORDINATOR_TOKEN",
		Timeout:           5 * time.Second,
		MaxBatch:          100,
		ReconcileInterval: time.Minute,
	}
	tests := map[string]struct {
		cfg     ExternalGrantCoordinatorConfig
		wantErr string
	}{
		"valid https": {cfg: valid},
		"valid loopback": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.ReceiptURL = "http://127.0.0.1:8080/receipts"
			return c
		}()},
		"missing receipt":  {cfg: func() ExternalGrantCoordinatorConfig { c := valid; c.ReceiptURL = ""; return c }(), wantErr: "receipt_url"},
		"missing auth env": {cfg: func() ExternalGrantCoordinatorConfig { c := valid; c.AuthTokenEnv = ""; return c }(), wantErr: "auth_token_env"},
		"top-up remote plaintext": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.TopUpURL = "http://coordinator.example.test/grants/top-up"
			return c
		}(), wantErr: "loopback"},
		"top-up query": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.TopUpURL = "https://coordinator.example.test/grants/top-up?q=1"
			return c
		}(), wantErr: "query"},
		"remote plaintext": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.ReceiptURL = "http://coordinator.example.test/receipts"
			return c
		}(), wantErr: "loopback"},
		"userinfo": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.ReceiptURL = "https://user@coordinator.example.test/receipts"
			return c
		}(), wantErr: "user info"},
		"query": {cfg: func() ExternalGrantCoordinatorConfig {
			c := valid
			c.ReceiptURL = "https://coordinator.example.test/receipts?q=1"
			return c
		}(), wantErr: "query"},
		"negative timeout": {cfg: func() ExternalGrantCoordinatorConfig { c := valid; c.Timeout = -time.Second; return c }(), wantErr: "timeout"},
		"oversize batch":   {cfg: func() ExternalGrantCoordinatorConfig { c := valid; c.MaxBatch = 1001; return c }(), wantErr: "max_batch"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			grant := validExternalGrantConfig(t)
			grant.Coordinator = tc.cfg
			err := (&Config{LLM: LLMConfig{ExternalGrant: grant}}).validateLLMExternalGrant()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("valid config: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error=%v, want field %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateLLMExternalGrant_DisabledRejectsCoordinatorWork(t *testing.T) {
	c := &Config{}
	c.LLM.ExternalGrant.Mode = "disabled"
	c.LLM.ExternalGrant.Coordinator.ReceiptURL = "https://coordinator.example.test/receipts"
	if err := c.validateLLMExternalGrant(); err == nil || !strings.Contains(err.Error(), "coordinator") {
		t.Fatalf("error=%v, want disabled coordinator refusal", err)
	}
}
