package config_test

import (
	"strings"
	"testing"
)

// TestValidate_MCPDefaultLoadingMode — the deployment-wide MCP loading
// default accepts only the closed loading-mode set; anything else fails
// pre-boot naming the field.
func TestValidate_MCPDefaultLoadingMode(t *testing.T) {
	c := mustLoadValid(t)
	for _, ok := range []string{"", "always", "deferred"} {
		c.Tools.MCPDefaultLoadingMode = ok
		if err := c.Validate(); err != nil {
			t.Fatalf("mcp_default_loading_mode=%q refused: %v", ok, err)
		}
	}
	c.Tools.MCPDefaultLoadingMode = "lazy"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "tools.mcp_default_loading_mode") {
		t.Fatalf("unknown mode err = %v, want it to name tools.mcp_default_loading_mode", err)
	}
}
