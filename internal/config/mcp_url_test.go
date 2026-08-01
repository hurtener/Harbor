package config_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestNormalizeMCPHTTPURL_StrictSharedBoundary(t *testing.T) {
	for _, raw := range []string{
		"", "mcp.example.test/path", "ftp://mcp.example.test/path",
		"https:///path", "https://user:pass@mcp.example.test/path",
		"https://mcp.example.test/path#client-only",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, err := config.NormalizeMCPHTTPURL(raw); err == nil {
				t.Fatalf("NormalizeMCPHTTPURL(%q) = %q, want error", raw, got)
			}
		})
	}
	got, err := config.NormalizeMCPHTTPURL(" HTTPS://mcp.example.test/path?tenant=t&mode=sse ")
	if err != nil {
		t.Fatalf("valid URL: %v", err)
	}
	if got != "https://mcp.example.test/path?tenant=t&mode=sse" {
		t.Fatalf("normalized URL = %q, query was not retained", got)
	}
}

func TestValidate_MCPServerURLUsesStrictSharedBoundary(t *testing.T) {
	cfg, err := config.Load(context.Background(), "testdata/valid_minimal.yaml")
	if err != nil {
		t.Fatalf("Load valid config: %v", err)
	}
	cfg.Tools.MCPServers = []config.MCPServerConfig{{Name: "bad-boot", TransportMode: "streamable_http", URL: "https://mcp.example.test/path#fragment"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("boot config accepted an MCP URL fragment")
	}
}
