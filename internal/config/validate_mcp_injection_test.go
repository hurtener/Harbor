package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidate_MCPInjection_Table covers the per-user credential-injection
// mapping validation on an MCP server connection: mutual exclusivity with the
// bearer mode / a static Authorization header, the downstream-sink allow-list,
// the reserved-`_meta`-key guard, and the redaction-covered-target-key guard.
func TestValidate_MCPInjection_Table(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantPath string
		wantText string
	}{
		{
			"injection + oauth_provider rejected (one auth mode)",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					OAuthProvider: "m365-broker",
					Injection:     &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormBasic},
				}}
			},
			"tools.mcp_servers[0].injection",
			"one auth mode",
		},
		{
			"injection + static Authorization header rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Headers:   map[string]string{"Authorization": "Basic xxx"},
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormBasic},
				}}
			},
			"tools.mcp_servers[0].headers",
			"one auth mode",
		},
		{
			"injection on stdio rejected (no http request)",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "local", TransportMode: "stdio", Command: []string{"/bin/echo"},
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormBasic},
				}}
			},
			"tools.mcp_servers[0].injection",
			"http(s) url",
		},
		{
			"injection unknown provider lists declared names",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "nope", Form: config.MCPInjectionFormBasic},
				}}
			},
			"tools.mcp_servers[0].injection.provider",
			"m365-broker",
		},
		{
			"injection host not in allow-list rejected",
			func(c *config.Config) {
				withOAuthProvider(c) // allows gcal.example.test only
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "other", TransportMode: "streamable_http", URL: "https://other.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormBasic},
				}}
			},
			"tools.mcp_servers[0].injection.provider",
			"allowed_downstream_hosts",
		},
		{
			"header form targeting Authorization rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormHeader, Header: "Authorization"},
				}}
			},
			"tools.mcp_servers[0].injection.header",
			"form=basic",
		},
		{
			"header form with non-redaction-covered key rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormHeader, Header: "x-acme-blob"},
				}}
			},
			"tools.mcp_servers[0].injection.header",
			"redaction-covered",
		},
		{
			"meta form with reserved segment rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormMeta, MetaKey: "tenant.api_key"},
				}}
			},
			"tools.mcp_servers[0].injection.meta_key",
			"reserved",
		},
		{
			"meta form with non-covered leaf rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.blob"},
				}}
			},
			"tools.mcp_servers[0].injection.meta_key",
			"redaction-covered",
		},
		{
			"unknown form rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					Injection: &config.MCPCredentialInjectionConfig{Provider: "m365-broker", Form: "bogus"},
				}}
			},
			"tools.mcp_servers[0].injection.form",
			"unknown form",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid config (mutation: %s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("err=%q missing path %q", err.Error(), tc.wantPath)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err=%q missing text %q", err.Error(), tc.wantText)
			}
		})
	}
}

// TestValidate_MCPInjection_Accepts proves the three well-formed injection
// forms pass validation.
func TestValidate_MCPInjection_Accepts(t *testing.T) {
	forms := []*config.MCPCredentialInjectionConfig{
		{Provider: "m365-broker", Form: config.MCPInjectionFormHeader, Header: "x-vendor-api-key"},
		{Provider: "m365-broker", Form: config.MCPInjectionFormBasic, BasicUsername: "svc"},
		{Provider: "m365-broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.api_key"},
	}
	for _, inj := range forms {
		cfg := mustLoadValid(t)
		withOAuthProvider(cfg)
		cfg.Tools.MCPServers = []config.MCPServerConfig{{
			Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
			Injection: inj,
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid injection form %q rejected: %v", inj.Form, err)
		}
	}
}

// TestIsReceiverInjectionCredentialKey_GoldenSet pins the redaction-coverage
// predicate — the SINGLE authority both the injection-mapping validation and the
// audit redactor consult. Trailing-segment matching keeps observability fields
// (token_type / token_url) out of the net while catching credential keys.
func TestIsReceiverInjectionCredentialKey_GoldenSet(t *testing.T) {
	covered := []string{"x-vendor-api-key", "x_github_token", "vendor.api_key", "user_password", "acme-secret", "X-API-Key", "credential"}
	for _, k := range covered {
		if !config.IsReceiverInjectionCredentialKey(k) {
			t.Errorf("IsReceiverInjectionCredentialKey(%q) = false, want true", k)
		}
	}
	notCovered := []string{"token_type", "token_url", "x-request-id", "content-type", "authorization", "monkey", ""}
	for _, k := range notCovered {
		if config.IsReceiverInjectionCredentialKey(k) {
			t.Errorf("IsReceiverInjectionCredentialKey(%q) = true, want false", k)
		}
	}
}
