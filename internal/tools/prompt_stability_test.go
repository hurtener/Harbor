package tools_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestPromptStabilityTierOf_ClassifiesBySourceOwnership(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool tools.Tool
		want tools.PromptStabilityTier
	}{
		{tools.Tool{Name: "tool_search", Transport: tools.TransportInProcess}, tools.PromptTierBuiltin},
		{tools.Tool{Name: "bare"}, tools.PromptTierBuiltin},
		{tools.Tool{Name: "docs_q", Transport: tools.TransportHTTP, Source: "docs"}, tools.PromptTierShared},
		{tools.Tool{Name: "boot_q", Transport: tools.TransportMCP, Source: "boot"}, tools.PromptTierShared},
		{tools.Tool{Name: "crm_q", Transport: tools.TransportMCP, Source: "crm" + tools.AgentSourceMarker + "00ff"}, tools.PromptTierAgent},
		{tools.Tool{Name: "mail_q", Transport: tools.TransportMCP, Source: "mail" + tools.UserSourceMarker + "00ff"}, tools.PromptTierUser},
	}
	for _, c := range cases {
		if got := tools.PromptStabilityTierOf(c.tool); got != c.want {
			t.Errorf("%s: tier %d, want %d", c.tool.Name, got, c.want)
		}
	}
}

func TestOrderForPromptStability_StableTierOrderWithoutMutatingInput(t *testing.T) {
	t.Parallel()
	in := []tools.Tool{
		{Name: "a_user", Transport: tools.TransportMCP, Source: "m" + tools.UserSourceMarker + "1"},
		{Name: "b_agent", Transport: tools.TransportMCP, Source: "c" + tools.AgentSourceMarker + "1"},
		{Name: "c_builtin", Transport: tools.TransportInProcess},
		{Name: "d_shared", Transport: tools.TransportHTTP, Source: "docs"},
		{Name: "e_builtin", Transport: tools.TransportInProcess},
	}
	got := tools.OrderForPromptStability(in)
	want := []string{"c_builtin", "e_builtin", "d_shared", "b_agent", "a_user"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("order = %v, want %v", toolNames(got), want)
		}
	}
	if in[0].Name != "a_user" {
		t.Error("input slice was reordered in place")
	}
}

func toolNames(in []tools.Tool) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, t.Name)
	}
	return out
}
