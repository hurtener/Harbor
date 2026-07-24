package postgres_test

// Driver-level tests for the Postgres SkillStore. The behavioural
// surface is covered by the shared conformance suite
// (`internal/skills/conformancetest`); this file invokes that suite
// unchanged + adds driver-specific cases the suite cannot express
// (construction errors, the driver-registration side effect, the
// search ranking ladder, and the opt-in semantic retrieval leg).

import (
	"context"
	"errors"
	"strings"
	"testing"

	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/conformancetest"
	"github.com/hurtener/Harbor/internal/skills/drivers/postgres"
)

// TestConformance runs the shared SkillStore conformance suite against
// the Postgres driver. Each subtest gets its own fresh schema so state
// is isolated; the restart-survival subtest reopens a fresh driver
// against the SAME schema to prove durability. This is the §9 parity
// gate: the suite is inherited verbatim, no edits to make it pass.
func TestConformance(t *testing.T) {
	baseDSN := requireDSN(t)
	conformancetest.Run(t, func(t *testing.T) conformancetest.Harness {
		t.Helper()
		bus := buildBus(t)
		dsn := freshSchema(t, baseDSN)
		store, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn},
			skills.Deps{Bus: bus})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		return conformancetest.Harness{
			Store:   store,
			Bus:     bus,
			Cleanup: func() { _ = store.Close(context.Background()) },
			ReopenedStore: func() (skills.SkillStore, error) {
				return postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn},
					skills.Deps{Bus: bus})
			},
		}
	})
}

// TestDriverRegistered verifies the init() side-effect: the driver is
// reachable through the registry under "postgres". An empty DSN fails
// construction, but NOT with ErrUnknownDriver — proving registration
// happened. Runs without a live Postgres.
func TestDriverRegistered(t *testing.T) {
	bus := buildBus(t)
	_, err := skills.OpenDriver("postgres", skills.ConfigSnapshot{Driver: "postgres"},
		skills.Deps{Bus: bus})
	if err == nil {
		t.Fatal("OpenDriver(postgres, empty DSN): err=nil, want non-nil")
	}
	if errors.Is(err, skills.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// TestNew_RequiresDSN pins the explicit-DSN-required contract. Runs
// without a live Postgres.
func TestNew_RequiresDSN(t *testing.T) {
	bus := buildBus(t)
	_, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres"},
		skills.Deps{Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}

// TestNew_RequiresBus pins the fail-loud bus dep guard. Runs without a
// live Postgres.
func TestNew_RequiresBus(t *testing.T) {
	_, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: "postgres://x"},
		skills.Deps{Bus: nil})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

// TestNew_RejectsSemanticWithoutEmbedder pins the fail-loud contract:
// retrieval=semantic with no Embedder must error — never a stub
// fallback (AGENTS.md §13). The guard fires before any DB connection
// opens, so this runs without a live Postgres.
func TestNew_RejectsSemanticWithoutEmbedder(t *testing.T) {
	bus := buildBus(t)
	_, err := postgres.New(skills.ConfigSnapshot{
		Driver: "postgres", DSN: "postgres://x", Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil for semantic retrieval without embedder")
	}
	if !strings.Contains(err.Error(), "Embedder") {
		t.Errorf("error should mention Embedder; got: %v", err)
	}
}

// TestSearch_LadderParity exercises the full-text → regex → exact
// ladder against a real Postgres and asserts the normalised-score
// contract the SQLite driver also honours: every score in [0,1], the
// best full-text match at 1.0, and identity-scoped results only.
func TestSearch_LadderParity(t *testing.T) {
	baseDSN := requireDSN(t)
	bus := buildBus(t)
	dsn := freshSchema(t, baseDSN)
	store, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	corpus := []skills.Skill{
		{
			Name: "alpha", Title: "Alpha", Trigger: "trigger-alpha",
			Description: "harbor harbor harbor frequent",
			Steps:       []string{"step a"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject, Tags: []string{"go"},
		},
		{
			Name: "bravo", Title: "Bravo", Trigger: "trigger-bravo",
			Description: "harbor once",
			Steps:       []string{"step b"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject, Tags: []string{"go"},
		},
		{
			Name: "charlie", Title: "Charlie", Trigger: "trigger-charlie",
			Description: "unrelated content entirely",
			Steps:       []string{"step c"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject, Tags: []string{"rust"},
		},
	}
	for _, s := range corpus {
		if err := store.Upsert(ctx, fixtureID, s); err != nil {
			t.Fatalf("Upsert(%q): %v", s.Name, err)
		}
	}

	// Full-text path: "harbor" hits alpha + bravo (not charlie).
	got, err := store.Search(ctx, fixtureID, "harbor", 10)
	if err != nil {
		t.Fatalf("Search(harbor): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search(harbor) returned %d rows; want 2 (%v)", len(got), names(got))
	}
	for _, r := range got {
		if r.Score < 0 || r.Score > 1 {
			t.Errorf("score out of [0,1]: %q=%v", r.Skill.Name, r.Score)
		}
		if r.Path != skills.PathFTS5 {
			t.Errorf("path=%q, want %q", r.Path, skills.PathFTS5)
		}
	}
	// alpha has the higher token frequency → best match → score 1.0
	// after min-max normalisation; it must sort first.
	if got[0].Skill.Name != "alpha" {
		t.Errorf("top full-text hit=%q, want alpha", got[0].Skill.Name)
	}
	if got[0].Score != 1.0 {
		t.Errorf("top full-text score=%v, want 1.0", got[0].Score)
	}

	// Exact path: a tag value that is not a full-text lexeme hit but
	// matches lower(tags_text) exactly falls through to exact (1.0).
	gotExact, err := store.Search(ctx, fixtureID, "rust", 10)
	if err != nil {
		t.Fatalf("Search(rust): %v", err)
	}
	if len(gotExact) == 0 {
		t.Fatalf("Search(rust) returned 0 rows; want ≥1")
	}

	// Identity scoping: a different session sees nothing.
	other := fixtureID
	other.SessionID = "s-other"
	gotOther, err := store.Search(ctx, other, "harbor", 10)
	if err != nil {
		t.Fatalf("Search(other): %v", err)
	}
	if len(gotOther) != 0 {
		t.Fatalf("cross-session leak: other session saw %d rows", len(gotOther))
	}
}

// TestSearch_Semantic exercises the opt-in semantic retrieval leg
// against a real Postgres with the deterministic test embedder. This
// is the §9 conformance-parity leg for the semantic mode.
func TestSearch_Semantic(t *testing.T) {
	baseDSN := requireDSN(t)
	bus := buildBus(t)
	dsn := freshSchema(t, baseDSN)
	store, err := postgres.New(skills.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus, Embedder: embeddingstest.New()})
	if err != nil {
		t.Fatalf("postgres.New(semantic): %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		s := skills.Skill{
			Name: name, Title: name, Trigger: "trigger-" + name,
			Description: "description for " + name,
			Steps:       []string{"step"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject,
		}
		if err := store.Upsert(ctx, fixtureID, s); err != nil {
			t.Fatalf("Upsert(%q): %v", name, err)
		}
	}
	got, err := store.Search(ctx, fixtureID, "alpha", 10)
	if err != nil {
		t.Fatalf("Search(semantic): %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("semantic Search returned 0 rows; want ≥1")
	}
	for _, r := range got {
		if r.Path != skills.PathSemantic {
			t.Errorf("path=%q, want %q", r.Path, skills.PathSemantic)
		}
		if r.Score < 0 || r.Score > 1 {
			t.Errorf("semantic score out of [0,1]: %q=%v", r.Skill.Name, r.Score)
		}
	}
}

// TestPostgres_ListGet_CrossIdentityIsolation is the explicit
// security gate for the identity-scoped WHERE filters on the Get + List
// read paths (§6). Skill A is upserted under identity A; identity B
// (same tenant + user, different session) must NOT be able to Get it or
// see it in List — and vice versa. The concurrent test covers this
// implicitly; this asserts it directly so a dropped `session_id`
// predicate on Get/List fails loudly here.
func TestPostgres_ListGet_CrossIdentityIsolation(t *testing.T) {
	baseDSN := requireDSN(t)
	bus := buildBus(t)
	dsn := freshSchema(t, baseDSN)
	store, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	idA := fixtureID
	idA.SessionID = "s-A"
	idB := fixtureID
	idB.SessionID = "s-B"

	mkSkill := func(name string) skills.Skill {
		return skills.Skill{
			Name: name, Title: name, Trigger: "trigger-" + name,
			Description: "desc " + name, Steps: []string{"step"},
			Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		}
	}
	if err := store.Upsert(ctx, idA, mkSkill("skill-a")); err != nil {
		t.Fatalf("Upsert(A): %v", err)
	}
	if err := store.Upsert(ctx, idB, mkSkill("skill-b")); err != nil {
		t.Fatalf("Upsert(B): %v", err)
	}

	// Get isolation: B cannot read A's skill, and vice versa.
	if _, err := store.Get(ctx, idB, "skill-a"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Errorf("Get(B, skill-a): want ErrSkillNotFound, got %v", err)
	}
	if _, err := store.Get(ctx, idA, "skill-b"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Errorf("Get(A, skill-b): want ErrSkillNotFound, got %v", err)
	}
	// Each identity CAN read its own.
	if _, err := store.Get(ctx, idA, "skill-a"); err != nil {
		t.Errorf("Get(A, skill-a): own skill unreadable: %v", err)
	}
	if _, err := store.Get(ctx, idB, "skill-b"); err != nil {
		t.Errorf("Get(B, skill-b): own skill unreadable: %v", err)
	}

	// List isolation: A's List holds only skill-a; B's only skill-b.
	listA, err := store.List(ctx, idA, skills.ListFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List(A): %v", err)
	}
	if len(listA) != 1 || listA[0].Name != "skill-a" {
		t.Errorf("List(A) = %v, want exactly [skill-a]", listNames(listA))
	}
	listB, err := store.List(ctx, idB, skills.ListFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if len(listB) != 1 || listB[0].Name != "skill-b" {
		t.Errorf("List(B) = %v, want exactly [skill-b]", listNames(listB))
	}
}

func listNames(ss []skills.Skill) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func names(rs []skills.RankedSkill) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Skill.Name
	}
	return out
}
