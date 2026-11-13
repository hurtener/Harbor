package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidate_MCPDefaultLoadingMode — the deployment-wide MCP loading
// default accepts only the closed loading-mode set, and "deferred" needs an
// always-loaded tool_search built-in; every refusal names the field.
func TestValidate_MCPDefaultLoadingMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		builtIn []string
		entries []config.ToolEntryConfig
		wantErr string
	}{
		{name: "unset", builtIn: nil},
		{name: "always without search", mode: "always", builtIn: nil},
		{name: "deferred with search and get", mode: "deferred", builtIn: []string{"tool_search", "tool_get"}},
		{name: "unknown", mode: "lazy", builtIn: []string{"tool_search"}, wantErr: "must be one of"},
		{name: "deferred empty built-ins", mode: "deferred", builtIn: nil, wantErr: `requires "tool_search"`},
		{name: "deferred tool_get alone", mode: "deferred", builtIn: []string{"tool_get"}, wantErr: `requires "tool_search"`},
		{name: "deferred tool_search deferred", mode: "deferred", builtIn: []string{"tool_search", "tool_get"},
			entries: []config.ToolEntryConfig{{Name: "tool_search", LoadingMode: "deferred"}}, wantErr: "always-loaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustLoadValid(t)
			c.Tools.MCPDefaultLoadingMode = tc.mode
			c.Tools.BuiltIn = tc.builtIn
			c.Tools.Entries = tc.entries
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "tools.mcp_default_loading_mode") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want tools.mcp_default_loading_mode … %q", err, tc.wantErr)
			}
		})
	}
}
