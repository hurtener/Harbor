package registry

import (
	"testing"
)

// TestVersionHash_Deterministic_SameContentSameHash pins the core
// version_hash contract (RFC §6.16, D-068): byte-identical
// configuration content always produces the same hash.
func TestVersionHash_Deterministic_SameContentSameHash(t *testing.T) {
	cfg := AgentConfig{
		Prompts: []string{"sys prompt", "few-shot 1"},
		Tools: []ToolDescriptor{
			{Name: "search", SchemaDigest: "d1"},
			{Name: "fetch", SchemaDigest: "d2"},
		},
		PlannerConfig: map[string]string{"mode": "react", "max_steps": "8"},
		ModelPolicy:   map[string]string{"model": "haiku", "temp": "0"},
	}
	h1, err := VersionHash(cfg)
	if err != nil {
		t.Fatalf("VersionHash: %v", err)
	}
	h2, err := VersionHash(cfg)
	if err != nil {
		t.Fatalf("VersionHash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("version_hash not deterministic: %q != %q", h1, h2)
	}
	if h1 == "" {
		t.Fatal("version_hash empty")
	}
}

// TestVersionHash_OrderIndependent verifies the canonicaliser: prompt
// order, tool order, and map iteration order do not affect the hash.
func TestVersionHash_OrderIndependent(t *testing.T) {
	a := AgentConfig{
		Prompts: []string{"alpha", "beta", "gamma"},
		Tools: []ToolDescriptor{
			{Name: "z", SchemaDigest: "1"},
			{Name: "a", SchemaDigest: "2"},
		},
		PlannerConfig: map[string]string{"k1": "v1", "k2": "v2"},
		ModelPolicy:   map[string]string{"m": "x"},
	}
	b := AgentConfig{
		// Same content, different slice / construction order.
		Prompts: []string{"gamma", "alpha", "beta"},
		Tools: []ToolDescriptor{
			{Name: "a", SchemaDigest: "2"},
			{Name: "z", SchemaDigest: "1"},
		},
		PlannerConfig: map[string]string{"k2": "v2", "k1": "v1"},
		ModelPolicy:   map[string]string{"m": "x"},
	}
	ha, err := VersionHash(a)
	if err != nil {
		t.Fatalf("VersionHash(a): %v", err)
	}
	hb, err := VersionHash(b)
	if err != nil {
		t.Fatalf("VersionHash(b): %v", err)
	}
	if ha != hb {
		t.Fatalf("version_hash is order-dependent: %q != %q", ha, hb)
	}
}

// TestVersionHash_ChangesOnContentChange verifies the hash bumps when
// ANY of the four configuration dimensions changes.
func TestVersionHash_ChangesOnContentChange(t *testing.T) {
	base := AgentConfig{
		Prompts:       []string{"p1"},
		Tools:         []ToolDescriptor{{Name: "t1", SchemaDigest: "d1"}},
		PlannerConfig: map[string]string{"mode": "react"},
		ModelPolicy:   map[string]string{"model": "haiku"},
	}
	baseHash, err := VersionHash(base)
	if err != nil {
		t.Fatalf("VersionHash(base): %v", err)
	}

	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		name string
		mut  func(c *AgentConfig)
	}{
		{"prompt changed", func(c *AgentConfig) { c.Prompts = []string{"p1-edited"} }},
		{"prompt added", func(c *AgentConfig) { c.Prompts = []string{"p1", "p2"} }},
		{"tool name changed", func(c *AgentConfig) { c.Tools = []ToolDescriptor{{Name: "t2", SchemaDigest: "d1"}} }},
		{"tool schema changed", func(c *AgentConfig) { c.Tools = []ToolDescriptor{{Name: "t1", SchemaDigest: "d2"}} }},
		{"planner config changed", func(c *AgentConfig) { c.PlannerConfig = map[string]string{"mode": "planexecute"} }},
		{"model policy changed", func(c *AgentConfig) { c.ModelPolicy = map[string]string{"model": "sonnet"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := AgentConfig{
				Prompts:       append([]string(nil), base.Prompts...),
				Tools:         append([]ToolDescriptor(nil), base.Tools...),
				PlannerConfig: map[string]string{"mode": "react"},
				ModelPolicy:   map[string]string{"model": "haiku"},
			}
			tc.mut(&c)
			h, err := VersionHash(c)
			if err != nil {
				t.Fatalf("VersionHash: %v", err)
			}
			if h == baseHash {
				t.Fatalf("version_hash did not change after %s", tc.name)
			}
		})
	}
}

// TestVersionHash_EmptyConfig verifies the zero-value config hashes
// cleanly (no panic, deterministic).
func TestVersionHash_EmptyConfig(t *testing.T) {
	h1, err := VersionHash(AgentConfig{})
	if err != nil {
		t.Fatalf("VersionHash(empty): %v", err)
	}
	h2, err := VersionHash(AgentConfig{})
	if err != nil {
		t.Fatalf("VersionHash(empty): %v", err)
	}
	if h1 != h2 || h1 == "" {
		t.Fatalf("empty-config hash unstable or empty: %q %q", h1, h2)
	}
}
