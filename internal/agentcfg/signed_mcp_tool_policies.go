package agentcfg

import (
	"fmt"
	"strings"
)

const (
	// MaxSignedMCPToolPolicies bounds the signed descriptor's policy map.
	MaxSignedMCPToolPolicies = 32
	// MaxSignedMCPToolNameBytes bounds one server-local MCP tool name.
	MaxSignedMCPToolNameBytes = 128
	// MaxSignedMCPToolAttempts is the existing default total attempt count;
	// signed descriptors may restrict it, but cannot enlarge it.
	MaxSignedMCPToolAttempts = 4
)

// NormalizeSignedMCPToolPolicies validates and copies the closed retry policy.
// Keys are exact server-local names, never Harbor's source-prefixed catalog names.
func NormalizeSignedMCPToolPolicies(in map[string]SignedMCPToolRetryPolicy) (map[string]SignedMCPToolRetryPolicy, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxSignedMCPToolPolicies {
		return nil, fmt.Errorf("too many tool policies: %d > %d", len(in), MaxSignedMCPToolPolicies)
	}
	out := make(map[string]SignedMCPToolRetryPolicy, len(in))
	for name, policy := range in {
		if name == "" || strings.TrimSpace(name) != name || len(name) > MaxSignedMCPToolNameBytes || strings.ContainsAny(name, " \t\r\n") {
			return nil, fmt.Errorf("noncanonical server-local tool name %q", name)
		}
		if policy.MaxAttempts < 1 || policy.MaxAttempts > MaxSignedMCPToolAttempts {
			return nil, fmt.Errorf("tool %q max_attempts must be 1..%d", name, MaxSignedMCPToolAttempts)
		}
		out[name] = policy
	}
	return out, nil
}
