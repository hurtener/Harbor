package localdb_test

// Phase 84d (D-191) — the semantic skill-retrieval mode: Search
// ranks by embedding similarity (path "semantic"), identity-scoped,
// with the fail-loud no-embedder guard at both the registry and the
// driver constructor. The deterministic embedder comes from
// internal/embeddings/embeddingstest.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
)

func semanticStore(t *testing.T) skills.SkillStore {
	t.Helper()
	store, err := localdb.New(skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       ":memory:",
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: newBus(t), Embedder: embeddingstest.New()})
	if err != nil {
		t.Fatalf("localdb.New(semantic): %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func quad(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	}}
}

func seedSkill(t *testing.T, store skills.SkillStore, id identity.Quadruple, name, title, trigger, desc string) {
	t.Helper()
	if err := store.Upsert(context.Background(), id, skills.Skill{
		Name:        name,
		Title:       title,
		Trigger:     trigger,
		Description: desc,
		Steps:       []string{"step one"},
		Origin:      skills.OriginGenerated,
		Scope:       skills.ScopeSession,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("Upsert %s: %v", name, err)
	}
}

func TestSearch_Semantic_RanksBySimilarity(t *testing.T) {
	store := semanticStore(t)
	id := quad("t1", "u1", "s1")
	seedSkill(t, store, id, "dock-boat", "Dock a boat", "docking a boat at the pier", "tie the boat to the harbor pier cleats")
	seedSkill(t, store, id, "bake-cake", "Bake a cake", "baking a chocolate cake", "mix flour sugar cocoa and bake the cake")
	seedSkill(t, store, id, "file-taxes", "File taxes", "filing yearly taxes", "gather receipts and file the tax forms")

	got, err := store.Search(context.Background(), id, "how to dock a boat at the pier", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search returned %d rows, want 2", len(got))
	}
	if got[0].Skill.Name != "dock-boat" {
		t.Errorf("top result = %q, want dock-boat", got[0].Skill.Name)
	}
	for i, r := range got {
		if r.Path != skills.PathSemantic {
			t.Errorf("row %d Path=%q, want %q", i, r.Path, skills.PathSemantic)
		}
		if r.Score < 0 || r.Score > 1 {
			t.Errorf("row %d Score=%v outside the canonical 0–1 scale", i, r.Score)
		}
	}
	if got[0].Score < got[1].Score {
		t.Errorf("rows not ranked: %v then %v", got[0].Score, got[1].Score)
	}
}

func TestSearch_Semantic_IdentityScoped(t *testing.T) {
	store := semanticStore(t)
	owner := quad("t1", "u1", "s1")
	seedSkill(t, store, owner, "secret-skill", "Secret", "the secret session skill", "only session one should retrieve this")

	otherSession := quad("t1", "u1", "s2")
	got, err := store.Search(context.Background(), otherSession, "the secret session skill", 5)
	if err != nil {
		t.Fatalf("Search other session: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("session 2 retrieved %d skill(s) scoped to session 1 — isolation breach", len(got))
	}
	otherTenant := quad("t2", "u1", "s1")
	got, err = store.Search(context.Background(), otherTenant, "the secret session skill", 5)
	if err != nil {
		t.Fatalf("Search other tenant: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tenant 2 retrieved %d skill(s) scoped to tenant 1 — isolation breach", len(got))
	}
	otherUser := quad("t1", "u2", "s1")
	got, err = store.Search(context.Background(), otherUser, "the secret session skill", 5)
	if err != nil {
		t.Fatalf("Search other user: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("user 2 retrieved %d skill(s) scoped to user 1 — isolation breach", len(got))
	}
}

func TestSearch_Semantic_EmptyCatalogAndEmptyQuery(t *testing.T) {
	store := semanticStore(t)
	id := quad("t1", "u1", "s1")
	got, err := store.Search(context.Background(), id, "anything at all", 5)
	if err != nil {
		t.Fatalf("Search empty catalog: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty catalog returned %d rows", len(got))
	}
	got, err = store.Search(context.Background(), id, "   ", 5)
	if err != nil {
		t.Fatalf("Search blank query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("blank query returned %d rows", len(got))
	}
}

func TestNew_Semantic_RequiresEmbedder(t *testing.T) {
	_, err := localdb.New(skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       ":memory:",
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: newBus(t)})
	if err == nil {
		t.Fatal("err=nil, want fail-loud no-embedder construction error")
	}
	if !strings.Contains(err.Error(), "Embedder") {
		t.Errorf("error must name the missing dependency, got %q", err.Error())
	}
}

func TestOpen_Semantic_RequiresEmbedder(t *testing.T) {
	_, err := skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       ":memory:",
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: newBus(t)})
	if err == nil {
		t.Fatal("err=nil, want the registry fail-loud guard")
	}
	if !strings.Contains(err.Error(), "Deps.Embedder") || !strings.Contains(err.Error(), "no stub fallback") {
		t.Errorf("guard error mismatch: %q", err.Error())
	}
}

func TestOpen_UnknownRetrievalMode_Rejected(t *testing.T) {
	_, err := skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       ":memory:",
		Retrieval: skills.RetrievalMode("vibes"),
	}, skills.Deps{Bus: newBus(t), Embedder: embeddingstest.New()})
	if err == nil || !strings.Contains(err.Error(), "unknown retrieval mode") {
		t.Fatalf("err=%v, want unknown-retrieval-mode rejection", err)
	}
}

func TestSearch_Semantic_EmbedFailureFailsLoudly(t *testing.T) {
	emb := embeddingstest.New()
	store, err := localdb.New(skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       ":memory:",
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: newBus(t), Embedder: emb})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	id := quad("t1", "u1", "s1")
	seedSkill(t, store, id, "any-skill", "Any", "any trigger", "any description")

	// Close the embedder: the next semantic Search must surface the
	// failure loudly — never silently fall back to the lexical
	// ladder.
	if err := emb.Close(context.Background()); err != nil {
		t.Fatalf("embedder Close: %v", err)
	}
	_, err = store.Search(context.Background(), id, "any trigger", 5)
	if err == nil {
		t.Fatal("Search succeeded with a dead embedder — silent degradation")
	}
	if !errors.Is(err, embeddings.ErrClientClosed) {
		t.Errorf("err=%v, want the wrapped embeddings.ErrClientClosed", err)
	}
	if !strings.Contains(err.Error(), "semantic embed") {
		t.Errorf("err=%v, want the semantic-embed wrap", err)
	}
}
