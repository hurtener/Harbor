package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// withOAuthProvider seeds a valid config with a declared tokenexchange
// provider (+ the required KEK env name) so an MCP binding can resolve.
func withOAuthProvider(c *config.Config) {
	c.Tools.OAuthTokenKEKEnv = "HARBOR_TOOL_OAUTH_KEK"
	c.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{
		{
			Name:            "m365-broker",
			Driver:          "tokenexchange",
			ClientIDEnv:     "M365_CLIENT_ID",
			ClientSecretEnv: "M365_CLIENT_SECRET",
			TokenURL:        "https://broker.example.test/token",
		},
	}
}

// TestValidate_MCPSouthboundOAuth_Table covers the oauth_provider +
// meta_annotations validation on an MCP server connection.
func TestValidate_MCPSouthboundOAuth_Table(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantPath string
		wantText string // optional substring the error must carry
	}{
		{
			"unknown provider lists declared names",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					OAuthProvider: "not-declared",
				}}
			},
			"tools.mcp_servers[0].oauth_provider",
			"m365-broker",
		},
		{
			"binding on stdio rejected",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "local", TransportMode: "stdio", Command: []string{"/bin/echo"},
					OAuthProvider: "m365-broker",
				}}
			},
			"tools.mcp_servers[0].oauth_provider",
			"stdio",
		},
		{
			"static Authorization header conflicts with binding",
			func(c *config.Config) {
				withOAuthProvider(c)
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					OAuthProvider: "m365-broker",
					Headers:       map[string]string{"authorization": "Bearer static"},
				}}
			},
			"tools.mcp_servers[0].headers",
			"one auth mode",
		},
		{
			"reserved meta_annotations key rejected",
			func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					MetaAnnotations: map[string]string{"tenant": "x"},
				}}
			},
			"tools.mcp_servers[0].meta_annotations",
			"reserved",
		},
		{
			"spec-prefixed meta_annotations key rejected",
			func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					MetaAnnotations: map[string]string{"io.modelcontextprotocol/ui": "x"},
				}}
			},
			"tools.mcp_servers[0].meta_annotations",
			"reserved",
		},
		{
			"empty meta_annotations key rejected",
			func(c *config.Config) {
				c.Tools.MCPServers = []config.MCPServerConfig{{
					Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
					MetaAnnotations: map[string]string{"   ": "x"},
				}}
			},
			"tools.mcp_servers[0].meta_annotations",
			"empty",
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

// TestValidate_MCPSouthboundOAuth_Accepts proves a correct binding +
// annotations pass.
func TestValidate_MCPSouthboundOAuth_Accepts(t *testing.T) {
	cfg := mustLoadValid(t)
	withOAuthProvider(cfg)
	cfg.Tools.MCPServers = []config.MCPServerConfig{{
		Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
		OAuthProvider:   "m365-broker",
		MetaAnnotations: map[string]string{"deployment": "prod", "team": "search"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid southbound binding rejected: %v", err)
	}
}
