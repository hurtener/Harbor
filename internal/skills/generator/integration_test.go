package generator_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/skills/generator"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// TestIntegration_GeneratorEndToEnd_AgainstLocalDB exercises the
// brief 04 §6 "generator end-to-end" pattern: real catalog + real
// localdb + real bus + real redactor. Propose with persist=true,
// then call skill_search through the Phase 38 catalog tool, and
// assert the new skill appears in the ranked results.
func TestIntegration_GeneratorEndToEnd_AgainstLocalDB(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	dsn := filepath.Join(t.TempDir(), "skills.sqlite")
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	deps := newTestDeps(t, bus)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Phase 38 Register: %v", err)
	}
	if err := generator.Register(catalog, store, deps); err != nil {
		t.Fatalf("Phase 41 Register: %v", err)
	}

	q := testIdentity()
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}

	// Propose via the catalog (exercises the full reflection-derived
	// JSON Schema validation path).
	proposeDesc, ok := catalog.Resolve(generator.ToolNameSkillPropose)
	if !ok {
		t.Fatal("Resolve skill_propose: not found")
	}
	proposeArgs, _ := json.Marshal(generator.ProposeArgs{
		Skill: generator.SkillDraft{
			Name:    "e2e-skill",
			Title:   "E2E generated skill",
			Trigger: "exercise the end-to-end path",
			Steps:   []string{"call thing"},
			Tags:    []string{"e2e", "generated"},
		},
		Persist: true,
	})
	proposeRes, err := proposeDesc.Invoke(ctx, proposeArgs)
	if err != nil {
		t.Fatalf("Invoke skill_propose: %v", err)
	}
	var receipt generator.SkillReceipt
	body, _ := json.Marshal(proposeRes.Value)
	_ = json.Unmarshal(body, &receipt)
	if !receipt.Persisted || receipt.Result != generator.ResultPersisted {
		t.Fatalf("receipt %+v: want Persisted=true Result=%q", receipt, generator.ResultPersisted)
	}

	// Call skill_search via the Phase 38 catalog tool and assert the
	// generated row is in the ranked results.
	searchDesc, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)
	searchArgs, _ := json.Marshal(skilltools.SearchArgs{
		Query: "exercise",
		Limit: 10,
	})
	searchRes, err := searchDesc.Invoke(ctx, searchArgs)
	if err != nil {
		t.Fatalf("Invoke skill_search: %v", err)
	}
	var sout skilltools.SearchResult
	sbody, _ := json.Marshal(searchRes.Value)
	_ = json.Unmarshal(sbody, &sout)
	var found bool
	for _, r := range sout.Skills {
		if r.Skill.Name == "e2e-skill" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skill_search did not return e2e-skill, got %d candidates", len(sout.Skills))
	}
}

// TestIntegration_CrossSessionPromotion_AgainstLocalDB is the load-
// bearing cross-session isolation test for Phase 41 (the spec's
// "MUST NOT find … then promote … MUST find" assertion).
//
// Identity A proposes a Scope=session skill. Identity B (different
// session, same tenant + user) calls skill_search → 0 rows. Identity
// A calls Promote(idA, name, []{idB}, ScopeProject). Identity B
// re-calls skill_search → MUST find.
//
// CLAUDE.md §6 rule 10: cross-session isolation tests are mandatory
// for any new code path touching identity. CLAUDE.md §17: integration
// tests use real drivers everywhere on the seam.
func TestIntegration_CrossSessionPromotion_AgainstLocalDB(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	dsn := filepath.Join(t.TempDir(), "skills.sqlite")
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	deps := newTestDeps(t, bus)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Phase 38 Register: %v", err)
	}
	if err := generator.Register(catalog, store, deps); err != nil {
		t.Fatalf("Phase 41 Register: %v", err)
	}

	idA := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-cross", UserID: "u-cross", SessionID: "s-A"},
		RunID:    "r-A",
	}
	idB := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-cross", UserID: "u-cross", SessionID: "s-B"},
		RunID:    "r-B",
	}

	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)
	ctxB, _ := identity.WithRun(context.Background(), idB.Identity, idB.RunID)

	// Identity A persists a Scope=session skill.
	draft := generator.SkillDraft{
		Name:    "cross-session",
		Title:   "Skill authored in session A",
		Trigger: "scoped to session",
		Steps:   []string{"do the thing"},
		Scope:   skills.ScopeSession,
	}
	receiptA, err := generator.Propose(ctxA, store, deps,
		generator.ProposeArgs{Skill: draft, Persist: true})
	if err != nil {
		t.Fatalf("Propose under A: %v", err)
	}
	if receiptA.Scope != skills.ScopeSession {
		t.Fatalf("receipt.Scope=%q want session", receiptA.Scope)
	}

	// Identity B's skill_search must NOT find it.
	searchDesc, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)
	searchArgs, _ := json.Marshal(skilltools.SearchArgs{Query: "scoped", Limit: 10})

	preRes, err := searchDesc.Invoke(ctxB, searchArgs)
	if err != nil {
		t.Fatalf("pre-promote search under B: %v", err)
	}
	var preOut skilltools.SearchResult
	body, _ := json.Marshal(preRes.Value)
	_ = json.Unmarshal(body, &preOut)
	for _, r := range preOut.Skills {
		if r.Skill.Name == "cross-session" {
			t.Fatalf("pre-promote: identity B leaked cross-session skill from identity A's session (got %s)", r.Skill.Name)
		}
	}
	// Also assert via direct Get under idB.
	if _, err := store.Get(ctxB, idB, "cross-session"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("pre-promote Get under B: got %v, want ErrSkillNotFound", err)
	}

	// Identity A promotes to identity B's session at Scope=project.
	if err := generator.Promote(ctxA, store, deps, idA, "cross-session",
		[]identity.Quadruple{idB}, skills.ScopeProject); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Identity B's skill_search MUST find it now.
	postRes, err := searchDesc.Invoke(ctxB, searchArgs)
	if err != nil {
		t.Fatalf("post-promote search under B: %v", err)
	}
	var postOut skilltools.SearchResult
	pbody, _ := json.Marshal(postRes.Value)
	_ = json.Unmarshal(pbody, &postOut)
	var found bool
	for _, r := range postOut.Skills {
		if r.Skill.Name == "cross-session" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-promote: identity B did not find cross-session via skill_search; got %d candidates", len(postOut.Skills))
	}

	// Direct Get under idB also succeeds.
	got, err := store.Get(ctxB, idB, "cross-session")
	if err != nil {
		t.Fatalf("post-promote direct Get under B: %v", err)
	}
	if got.Scope != skills.ScopeProject {
		t.Fatalf("post-promote Scope=%q want %q", got.Scope, skills.ScopeProject)
	}
	if got.OriginRef != "gen:s-A:r-A" {
		t.Fatalf("post-promote OriginRef=%q want gen:s-A:r-A (preserved from source)", got.OriginRef)
	}
}
