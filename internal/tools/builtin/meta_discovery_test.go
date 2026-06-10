// meta_discovery_test.go — coverage for the tool_search / tool_get
// discovery meta-tool bodies (Phase 107c / D-167; tests added with
// Phase 111d's coverage gate on the package) plus the skill_*
// nil-store invoke posture (registered without a configured skills
// subsystem → loud operator-readable error at invoke time).

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// discoveryCatalog registers tool_search + tool_get plus one fixture
// tool. The in-memory catalog has no SearchCache attached, so Search
// honestly returns empty — toolSearch's "discovery unavailable" shape.
func discoveryCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	type ea struct{}
	type eo struct{}
	if err := inproc.RegisterFunc[ea, eo](cat, "kb_search",
		func(context.Context, ea) (eo, error) { return eo{}, nil },
		tools.WithDescription("fixture knowledge-base search")); err != nil {
		t.Fatalf("register fixture: %v", err)
	}
	if err := Register(cat, []string{"tool_search", "tool_get"}); err != nil {
		t.Fatalf("Register(tool_search, tool_get): %v", err)
	}
	return cat
}

func TestToolGet_FoundAndMissing(t *testing.T) {
	t.Parallel()
	cat := discoveryCatalog(t)
	ctx := skillTestCtx(t)

	out := invoke[ToolGetOut](t, cat, ctx, "tool_get", ToolGetArgs{Name: "kb_search"})
	if !out.Found || out.Name != "kb_search" || out.Description == "" {
		t.Errorf("tool_get(kb_search) = %+v, want found with description", out)
	}

	miss := invoke[ToolGetOut](t, cat, ctx, "tool_get", ToolGetArgs{Name: "nope"})
	if miss.Found || !strings.Contains(miss.Error, "not found") {
		t.Errorf("tool_get(nope) = %+v, want Found=false with in-band error", miss)
	}
}

func TestToolSearch_NoCacheReturnsEmpty(t *testing.T) {
	t.Parallel()
	cat := discoveryCatalog(t)
	ctx := skillTestCtx(t)

	out := invoke[ToolSearchOut](t, cat, ctx, "tool_search", ToolSearchArgs{Query: "knowledge"})
	if out.Count != 0 || len(out.Tools) != 0 {
		t.Errorf("tool_search without a SearchCache = %+v, want honest empty", out)
	}
	// Limit clamping branches.
	if out := invoke[ToolSearchOut](t, cat, ctx, "tool_search", ToolSearchArgs{Query: "x", Limit: 999}); out.Count != 0 {
		t.Errorf("tool_search clamp-high = %+v", out)
	}
}

func TestDiscoveryMetaTools_IdentityMandatory(t *testing.T) {
	t.Parallel()
	cat := discoveryCatalog(t)
	argsFor := map[string]any{
		"tool_search": ToolSearchArgs{Query: "x"},
		"tool_get":    ToolGetArgs{Name: "x"},
	}
	for name, args := range argsFor {
		desc, _ := cat.Resolve(name)
		raw, _ := json.Marshal(args)
		if _, err := desc.Invoke(context.Background(), raw); !errors.Is(err, ErrIdentityRequired) {
			t.Errorf("%s without identity: err = %v, want ErrIdentityRequired", name, err)
		}
	}
}

// TestSkillBuiltins_NilStoreFailsLoudAtInvoke pins the store-shaped
// dep posture (Phase 111d): registration with a nil SkillStore is
// structurally valid (the subsystem is simply not configured), and
// every skill_* invoke fails with an operator-readable message naming
// the misconfiguration.
func TestSkillBuiltins_NilStoreFailsLoudAtInvoke(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	rc := skillTestRegistryContext(t)
	rc.Catalog = cat
	rc.SkillStore = nil
	if err := RegisterWith(rc, []string{"skill_search", "skill_get", "skill_list", "skill_propose"}); err != nil {
		t.Fatalf("RegisterWith with nil store: %v", err)
	}
	ctx := skillTestCtx(t)
	cases := map[string]any{
		"skill_search":  SkillSearchArgs{Query: "x"},
		"skill_get":     SkillGetArgs{Names: []string{"x"}},
		"skill_list":    SkillListArgs{},
		"skill_propose": map[string]any{"skill": map[string]any{"name": "n", "trigger": "t", "steps": []string{"s"}}},
	}
	for name, args := range cases {
		desc, ok := cat.Resolve(name)
		if !ok {
			t.Fatalf("Resolve(%q): not found", name)
		}
		raw, _ := json.Marshal(args)
		_, err := desc.Invoke(ctx, raw)
		if err == nil || !strings.Contains(err.Error(), "SkillStore is nil") {
			t.Errorf("%s with nil store: err = %v, want loud operator-readable misconfiguration error", name, err)
		}
	}
}
