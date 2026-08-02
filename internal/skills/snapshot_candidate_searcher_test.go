package skills_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

type failingSnapshotEmbedder struct{ err error }

func (e failingSnapshotEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, e.err
}

func snapshotCandidate(name, description string, updated time.Time) skills.Skill {
	return skills.Skill{
		Name: name, Description: description, Trigger: "when " + name,
		Steps: []string{"do"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
		UpdatedAt: updated,
	}
}

func snapshotIdentity() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
}

func TestSnapshotCandidateSearcher_LexicalUsesFullTextFirstAndKeepsCandidateView(t *testing.T) {
	t.Parallel()
	searcher, err := skills.NewSnapshotCandidateSearcher(skills.RetrievalDefault, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []skills.Skill{
		snapshotCandidate("deploy-harbor", "deploy Harbor safely", time.Unix(10, 0)),
		snapshotCandidate("harbor-only", "Harbor diagnostics", time.Unix(20, 0)),
	}
	result, err := searcher.SearchSnapshot(context.Background(), snapshotIdentity(), "harbor deploy", candidates, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Skill.Name != "deploy-harbor" || result[0].Path != skills.PathFTS5 || result[0].Score != 1 {
		t.Fatalf("full-text first result = %+v", result)
	}
	if result[0].Skill.Name != candidates[0].Name {
		t.Fatalf("search returned a candidate outside frozen view: %+v", result[0])
	}

	regex, err := searcher.SearchSnapshot(context.Background(), snapshotIdentity(), "^.$", []skills.Skill{snapshotCandidate("x", "single letter", time.Unix(1, 0))}, 1)
	if err != nil || len(regex) != 1 || regex[0].Path != skills.PathRegex || regex[0].Score != 0.95 {
		t.Fatalf("regex fallback = (%+v, %v)", regex, err)
	}
	exact, err := searcher.SearchSnapshot(context.Background(), snapshotIdentity(), "[", []skills.Skill{snapshotCandidate("[", "punctuation name", time.Unix(1, 0))}, 1)
	if err != nil || len(exact) != 1 || exact[0].Path != skills.PathExact || exact[0].Score != 1 {
		t.Fatalf("exact fallback = (%+v, %v)", exact, err)
	}
}

func TestSnapshotCandidateSearcher_SemanticRequiresEmbedderAndFailsLoud(t *testing.T) {
	t.Parallel()
	if _, err := skills.NewSnapshotCandidateSearcher(skills.RetrievalSemantic, nil); err == nil {
		t.Fatal("semantic construction without Embedder succeeded")
	}
	want := errors.New("embed offline")
	searcher, err := skills.NewSnapshotCandidateSearcher(skills.RetrievalSemantic, failingSnapshotEmbedder{err: want})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searcher.SearchSnapshot(context.Background(), snapshotIdentity(), "dock boat", []skills.Skill{snapshotCandidate("dock", "dock a boat", time.Now())}, 1); !errors.Is(err, want) {
		t.Fatalf("semantic embed failure = %v, want %v", err, want)
	}
}

func TestSnapshotCandidateSearcher_SemanticRanksOnlyFrozenCandidates(t *testing.T) {
	t.Parallel()
	searcher, err := skills.NewSnapshotCandidateSearcher(skills.RetrievalSemantic, embeddingstest.New())
	if err != nil {
		t.Fatal(err)
	}
	candidates := []skills.Skill{
		snapshotCandidate("dock", "how to dock a boat at a pier", time.Unix(10, 0)),
		snapshotCandidate("invoice", "how to issue an invoice", time.Unix(20, 0)),
	}
	result, err := searcher.SearchSnapshot(context.Background(), snapshotIdentity(), "dock a boat", candidates, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].Path != skills.PathSemantic || result[0].Skill.Name != "dock" {
		t.Fatalf("semantic result = %+v", result)
	}
}
