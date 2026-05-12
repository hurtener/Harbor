package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// TestIntegration_PlannerTools_AgainstLocalDB exercises the seam
// every Phase 38 dependency declares: real `tools.Catalog` + real
// `skills.SkillStore` (`localdb` driver against `:memory:`) + real
// `events.EventBus` (inmem). Asserts:
//
//   - happy-path search → get → list round-trip.
//   - identity propagation (the localdb driver rejects on missing
//     identity AND the planner-tool layer wraps that into the
//     expected sentinel).
//   - capability filter excludes a skill whose RequiredTools is not
//     a subset of AllowedTools.
//   - PII redaction round-trip across a skill containing PII.
//
// CLAUDE.md §17: integration tests use real drivers everywhere on
// the seam — no mocks at the boundary.
func TestIntegration_PlannerTools_AgainstLocalDB(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Seed the store with three skills under a known identity.
	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-int", UserID: "u-int", SessionID: "s-int"},
		RunID:    "r-int",
	}
	seedCtx := context.Background()
	seed := []skills.Skill{
		{
			Name:          "fetch-only",
			Title:         "Fetch a URL",
			Trigger:       "fetch a URL via http_fetch",
			Steps:         []string{"call http_fetch with URL", "parse response"},
			Tags:          []string{"net", "ops"},
			Origin:        skills.OriginPack,
			Scope:         skills.ScopeProject,
			RequiredTools: []string{"http_fetch"},
		},
		{
			Name:          "write-only",
			Title:         "Write artifact via fs_write",
			Trigger:       "persist artifact",
			Steps:         []string{"call fs_write with path"},
			Tags:          []string{"fs", "ops"},
			Origin:        skills.OriginPack,
			Scope:         skills.ScopeProject,
			RequiredTools: []string{"fs_write"},
		},
		{
			Name:        "pii-skill",
			Title:       "Contact alice@example.com",
			Trigger:     "send mail",
			Description: "Use Authorization: Bearer abc.def.ghi",
			Steps:       []string{"draft", "send"},
			Origin:      skills.OriginPack,
			Scope:       skills.ScopeProject,
		},
	}
	for _, s := range seed {
		if err := store.Upsert(seedCtx, id, s); err != nil {
			t.Fatalf("Upsert(%q): %v", s.Name, err)
		}
	}

	// ctx for tool invocations — carries the identity.
	ctx, err := identity.WithRun(context.Background(), id.Identity, id.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// --- skill_search ---
	dSearch, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)
	searchArgs := mustMarshal(t, skilltools.SearchArgs{
		Query: "fetch",
		Limit: 10,
		Capability: skilltools.CapabilityContext{
			AllowedTools: []string{"http_fetch", "tool_search"},
		},
	})
	searchRes, err := dSearch.Invoke(ctx, searchArgs)
	if err != nil {
		t.Fatalf("search Invoke: %v", err)
	}
	sout := unmarshalSearchResult(t, searchRes)
	if len(sout.Skills) == 0 {
		t.Fatal("search returned 0 skills, want ≥1 fetch-only candidate")
	}
	var foundFetch bool
	for _, s := range sout.Skills {
		if s.Skill.Name == "fetch-only" {
			foundFetch = true
		}
		if s.Skill.Name == "write-only" {
			t.Fatalf("write-only leaked through capability filter")
		}
	}
	if !foundFetch {
		t.Fatalf("search did not return fetch-only: got %v", sliceNames(sout.Skills))
	}

	// --- skill_get ---
	dGet, _ := catalog.Resolve(skilltools.ToolNameSkillGet)
	getArgs := mustMarshal(t, skilltools.GetArgs{
		Names:     []string{"fetch-only", "write-only", "missing"},
		MaxTokens: 4096,
		Capability: skilltools.CapabilityContext{
			AllowedTools: []string{"http_fetch"},
		},
	})
	getRes, err := dGet.Invoke(ctx, getArgs)
	if err != nil {
		t.Fatalf("get Invoke: %v", err)
	}
	gout := unmarshalGetResult(t, getRes)
	if len(gout.Skills) != 1 || gout.Skills[0].Name != "fetch-only" {
		t.Fatalf("got %v, want only fetch-only (write-only filtered + missing skipped)", outNames(gout.Skills))
	}

	// --- skill_list ---
	dList, _ := catalog.Resolve(skilltools.ToolNameSkillList)
	listArgs := mustMarshal(t, skilltools.ListArgs{
		Limit:      100,
		Capability: skilltools.CapabilityContext{AllowedTools: []string{"http_fetch"}},
	})
	listRes, err := dList.Invoke(ctx, listArgs)
	if err != nil {
		t.Fatalf("list Invoke: %v", err)
	}
	lout := unmarshalListResult(t, listRes)
	// pii-skill has no RequiredTools — passes the filter; fetch-only
	// passes; write-only blocked. So we should see 2 skills.
	if len(lout.Skills) != 2 {
		t.Fatalf("list returned %d skills, want 2 (fetch-only + pii-skill)", len(lout.Skills))
	}

	// --- PII redaction round-trip via skill_get ---
	getPIIArgs := mustMarshal(t, skilltools.GetArgs{
		Names:     []string{"pii-skill"},
		MaxTokens: 4096,
		Capability: skilltools.CapabilityContext{
			RedactPII: true,
		},
	})
	piiRes, err := dGet.Invoke(ctx, getPIIArgs)
	if err != nil {
		t.Fatalf("get pii Invoke: %v", err)
	}
	piiOut := unmarshalGetResult(t, piiRes)
	if len(piiOut.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(piiOut.Skills))
	}
	got := piiOut.Skills[0]
	if strings.Contains(got.Title, "alice@example.com") {
		t.Fatalf("PII leaked in Title: %q", got.Title)
	}
	if strings.Contains(got.Description, "abc.def.ghi") {
		t.Fatalf("PII leaked in Description: %q", got.Description)
	}

	// --- Missing identity path: real driver rejects loudly ---
	bareArgs := mustMarshal(t, skilltools.SearchArgs{Query: "x"})
	_, err = dSearch.Invoke(context.Background(), bareArgs)
	if err == nil {
		t.Fatal("expected error on missing identity, got nil")
	}
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("err=%v, want wrapped ErrIdentityRequired", err)
	}
}

// sliceNames extracts names from a RankedSkill slice for failure
// messages.
func sliceNames(in []skills.RankedSkill) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Skill.Name
	}
	return out
}
