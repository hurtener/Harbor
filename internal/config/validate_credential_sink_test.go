package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidate_DownstreamAllowList pins the D-300 sink allow-list on the MCP
// southbound binding: a bound provider with an empty allow-list, or a
// connection whose host is not listed, is rejected at boot fail-closed.
func TestValidate_DownstreamAllowList(t *testing.T) {
	t.Run("empty allow-list on bound provider rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
		cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
			Name: "m365", Driver: "tokenexchange",
			ClientIDEnv: "CID", ClientSecretEnv: "CSEC",
			TokenURL: "https://broker.example.test/token",
			// no AllowedDownstreamHosts → fail-closed
		}}
		cfg.Tools.MCPServers = []config.MCPServerConfig{{
			Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
			OAuthProvider: "m365",
		}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "allowed_downstream_hosts") {
			t.Fatalf("want empty-allow-list rejection, got %v", err)
		}
	})

	t.Run("unlisted connection host rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
		cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
			Name: "m365", Driver: "tokenexchange",
			ClientIDEnv: "CID", ClientSecretEnv: "CSEC",
			TokenURL:               "https://broker.example.test/token",
			AllowedDownstreamHosts: []string{"gcal.example.test"},
		}}
		cfg.Tools.MCPServers = []config.MCPServerConfig{{
			Name: "evil", TransportMode: "streamable_http", URL: "https://evil.example.test",
			OAuthProvider: "m365",
		}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "allowed_downstream_hosts") {
			t.Fatalf("want unlisted-host rejection, got %v", err)
		}
	})

	t.Run("listed connection host (default-port equivalence) accepted", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
		cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
			Name: "m365", Driver: "tokenexchange",
			ClientIDEnv: "CID", ClientSecretEnv: "CSEC",
			TokenURL:               "https://broker.example.test/token",
			AllowedDownstreamHosts: []string{"gcal.example.test"},
		}}
		cfg.Tools.MCPServers = []config.MCPServerConfig{{
			Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test:443",
			OAuthProvider: "m365",
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("listed host with explicit :443 must pass (default-port equivalence): %v", err)
		}
	})
}

// TestValidate_CredentialBrokers pins the D-300 broker-list validation.
func TestValidate_CredentialBrokers(t *testing.T) {
	base := func() config.ToolOAuthCredentialBrokerConfig {
		return config.ToolOAuthCredentialBrokerConfig{
			Name:                   "m365-broker",
			TokenURL:               "https://broker.example.test/token",
			AllowedDownstreamHosts: []string{"graph.microsoft.com"},
			AuthTokenEnv:           "HARBOR_M365_BROKER_TOKEN",
		}
	}
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantText string
	}{
		{"valid broker accepted", func(c *config.Config) {
			// A Protocol-installed broker-pull provider shares the token store,
			// so a declared broker requires the KEK env (Phase 169).
			c.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{base()}
		}, ""},
		{"duplicate names rejected", func(c *config.Config) {
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{base(), base()}
		}, "duplicate broker name"},
		{"non-https token_url rejected", func(c *config.Config) {
			b := base()
			b.TokenURL = "http://broker.example.test/token"
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{b}
		}, "https"},
		{"loopback http token_url accepted", func(c *config.Config) {
			c.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
			b := base()
			b.TokenURL = "http://127.0.0.1:8080/token"
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{b}
		}, ""},
		{"empty allowed_downstream_hosts rejected", func(c *config.Config) {
			b := base()
			b.AllowedDownstreamHosts = nil
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{b}
		}, "allowed_downstream_hosts"},
		{"empty auth_token_env rejected", func(c *config.Config) {
			b := base()
			b.AuthTokenEnv = ""
			c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{b}
		}, "auth_token_env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantText == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("want rejection containing %q, got %v", tc.wantText, err)
			}
		})
	}
}

// TestValidate_CredentialBrokers_CoexistWithInlineRemote proves the broker
// list is additive — the inline oauth_providers[].remote block still loads
// alongside it (backward-compatible).
func TestValidate_CredentialBrokers_CoexistWithInlineRemote(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name: "m365", Driver: "tokenexchange", CredentialSource: "remote",
		TokenURL: "https://broker.example.test/token",
		Remote: &config.ToolOAuthRemoteConfig{
			URL:          "https://coordinator.example.test/cred",
			AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
		},
	}}
	cfg.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{{
		Name:                   "m365-broker",
		TokenURL:               "https://broker.example.test/token",
		AllowedDownstreamHosts: []string{"graph.microsoft.com"},
		AuthTokenEnv:           "HARBOR_M365_BROKER_TOKEN",
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("inline remote + broker list must coexist: %v", err)
	}
}
