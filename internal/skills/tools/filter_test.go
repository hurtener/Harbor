package tools

import (
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestFilter_EmptyRequiredPasses(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "no-reqs", Origin: skills.OriginPack, Scope: skills.ScopeProject},
	}
	got := Filter(in, CapabilityContext{})
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1 (empty required = unconstrained)", len(got))
	}
}

func TestFilter_RequiredToolsSubset(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "uses-fetch-only", RequiredTools: []string{"http_fetch"}},
		{Name: "uses-fetch-and-write", RequiredTools: []string{"http_fetch", "fs_write"}},
		{Name: "uses-write-only", RequiredTools: []string{"fs_write"}},
	}
	cap := CapabilityContext{AllowedTools: []string{"http_fetch", "tool_search"}}
	got := Filter(in, cap)
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1 — only the fetch-only skill should pass", len(got))
	}
	if got[0].Name != "uses-fetch-only" {
		t.Fatalf("got[0].Name=%q, want uses-fetch-only", got[0].Name)
	}
}

func TestFilter_RequiredNamespacesSubset(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "ops-only", RequiredNS: []string{"ops"}},
		{Name: "ops-and-finance", RequiredNS: []string{"ops", "finance"}},
	}
	cap := CapabilityContext{AllowedNamespaces: []string{"ops"}}
	got := Filter(in, cap)
	if len(got) != 1 || got[0].Name != "ops-only" {
		t.Fatalf("got=%v, want [ops-only]", names(got))
	}
}

func TestFilter_RequiredTagsSubset(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "browser-skill", RequiredTags: []string{"browser"}},
		{Name: "api-skill", RequiredTags: []string{"api"}},
	}
	cap := CapabilityContext{AllowedTags: []string{"browser"}}
	got := Filter(in, cap)
	if len(got) != 1 || got[0].Name != "browser-skill" {
		t.Fatalf("got=%v, want [browser-skill]", names(got))
	}
}

func TestFilter_DefaultDeny_EmptyAllowedRejectsRequired(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "needs-tool", RequiredTools: []string{"http_fetch"}},
	}
	// Empty AllowedTools — default-deny rejects.
	got := Filter(in, CapabilityContext{})
	if len(got) != 0 {
		t.Fatalf("len(got)=%d, want 0 — empty AllowedTools must reject non-empty RequiredTools", len(got))
	}
}

func TestFilter_PreservesOrder(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"},
	}
	got := Filter(in, CapabilityContext{})
	for i, s := range got {
		if s.Name != in[i].Name {
			t.Fatalf("got[%d]=%q, want %q (order must be preserved)", i, s.Name, in[i].Name)
		}
	}
}

func TestFilter_AllAxesMustMatch(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{
			Name:          "multi-axis",
			RequiredTools: []string{"http_fetch"},
			RequiredNS:    []string{"ops"},
			RequiredTags:  []string{"browser"},
		},
	}
	cases := []struct {
		name string
		cap  CapabilityContext
		want bool
	}{
		{
			name: "all matched",
			cap:  CapabilityContext{AllowedTools: []string{"http_fetch"}, AllowedNamespaces: []string{"ops"}, AllowedTags: []string{"browser"}},
			want: true,
		},
		{
			name: "tools missing",
			cap:  CapabilityContext{AllowedNamespaces: []string{"ops"}, AllowedTags: []string{"browser"}},
			want: false,
		},
		{
			name: "namespaces missing",
			cap:  CapabilityContext{AllowedTools: []string{"http_fetch"}, AllowedTags: []string{"browser"}},
			want: false,
		},
		{
			name: "tags missing",
			cap:  CapabilityContext{AllowedTools: []string{"http_fetch"}, AllowedNamespaces: []string{"ops"}},
			want: false,
		},
	}
	for _, c := range cases {

		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := Filter(in, c.cap)
			if (len(got) == 1) != c.want {
				t.Fatalf("got=%d skills, want pass=%v", len(got), c.want)
			}
		})
	}
}

func TestFilter_EmptyInput(t *testing.T) {
	t.Parallel()

	got := Filter(nil, CapabilityContext{AllowedTools: []string{"x"}})
	if got != nil {
		t.Fatalf("got=%v, want nil for empty input", got)
	}
}

// names is a test helper — returns a slice of skill names for
// readable failure messages.
func names(in []skills.Skill) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Name
	}
	return out
}
