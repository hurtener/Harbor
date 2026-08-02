package skills_test

import (
	"context"
	"errors"
	"strings"
	"sync"
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

type fixedSnapshotEmbedder struct{ vectors [][]float32 }

func (e fixedSnapshotEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return e.vectors, nil
}

type countingSnapshotEmbedder struct {
	mu    sync.Mutex
	batch int
}

func (e *countingSnapshotEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.batch = len(texts)
	e.mu.Unlock()
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1, 0}
	}
	return result, nil
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

func TestSearchSnapshotRegexExact_UsesCanonicalTailWithoutClaimingFTS(t *testing.T) {
	t.Parallel()
	regex, err := skills.SearchSnapshotRegexExact(context.Background(), "^.$", []skills.Skill{snapshotCandidate("x", "single letter", time.Unix(1, 0))}, 1)
	if err != nil || len(regex) != 1 || regex[0].Path != skills.PathRegex || regex[0].Score != 0.95 {
		t.Fatalf("regex tail = (%+v, %v)", regex, err)
	}
	exact, err := skills.SearchSnapshotRegexExact(context.Background(), "[", []skills.Skill{snapshotCandidate("[", "punctuation name", time.Unix(1, 0))}, 1)
	if err != nil || len(exact) != 1 || exact[0].Path != skills.PathExact || exact[0].Score != 1 {
		t.Fatalf("exact tail = (%+v, %v)", exact, err)
	}
}

func TestSearchSnapshotRegexExact_OrderingLimitsTokensAndCancellation(t *testing.T) {
	t.Parallel()
	if got := skills.SnapshotSearchTokens(`  Harbor + DOCK's 42! `); len(got) != 4 || got[0] != "harbor" || got[1] != "dock" || got[2] != "s" || got[3] != "42" {
		t.Fatalf("SnapshotSearchTokens = %v", got)
	}
	newest := time.Unix(3, 0)
	candidates := []skills.Skill{
		snapshotCandidate("bravo", "dock", newest),
		snapshotCandidate("alpha", "dock", newest),
		snapshotCandidate("oldest", "dock", time.Unix(1, 0)),
	}
	got, err := skills.SearchSnapshotRegexExact(context.Background(), "dock", candidates, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Skill.Name != "alpha" || got[1].Skill.Name != "bravo" {
		t.Fatalf("ordered limited results = %+v", got)
	}
	if empty, err := skills.SearchSnapshotRegexExact(context.Background(), "  ", candidates, 2); err != nil || len(empty) != 0 {
		t.Fatalf("empty query = (%+v, %v)", empty, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := skills.SearchSnapshotRegexExact(cancelled, "dock", candidates, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled regex/exact search error = %v", err)
	}
}

func TestSnapshotResultHelpers_OrderAndTruncate(t *testing.T) {
	t.Parallel()
	newest := time.Unix(3, 0)
	result := []skills.RankedSkill{
		{Skill: snapshotCandidate("low", "", newest), Score: 0.2},
		{Skill: snapshotCandidate("bravo", "", newest), Score: 0.8},
		{Skill: snapshotCandidate("alpha", "", newest), Score: 0.8},
	}
	skills.SortSnapshotResults(result)
	if result[0].Skill.Name != "alpha" || result[1].Skill.Name != "bravo" || result[2].Skill.Name != "low" {
		t.Fatalf("sorted result = %+v", result)
	}
	if got := skills.TruncateSnapshotResults(result, 2); len(got) != 2 {
		t.Fatalf("truncated result count = %d, want 2", len(got))
	}
	if got := skills.TruncateSnapshotResults(result, 5); len(got) != 3 {
		t.Fatalf("untruncated result count = %d, want 3", len(got))
	}
}

func TestSearchSnapshotSemantic_RequiresEmbedderAndFailsLoud(t *testing.T) {
	t.Parallel()
	want := errors.New("embed offline")
	if _, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock boat", []skills.Skill{snapshotCandidate("dock", "dock a boat", time.Now())}, 1, failingSnapshotEmbedder{err: want}); !errors.Is(err, want) {
		t.Fatalf("semantic embed failure = %v, want %v", err, want)
	}
}

func TestSearchSnapshotSemantic_ValidatesInputsAndVectors(t *testing.T) {
	t.Parallel()
	candidate := []skills.Skill{snapshotCandidate("dock", "dock a boat", time.Now())}
	if empty, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "", candidate, 1, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty query = (%+v, %v)", empty, err)
	}
	if empty, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock", nil, 1, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty candidates = (%+v, %v)", empty, err)
	}
	if _, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock", candidate, 1, nil); err == nil || !strings.Contains(err.Error(), "missing Embedder") {
		t.Fatalf("missing embedder error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := skills.SearchSnapshotSemantic(cancelled, snapshotIdentity(), "dock", candidate, 1, fixedSnapshotEmbedder{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled semantic search error = %v", err)
	}
	if _, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock", candidate, 1, fixedSnapshotEmbedder{vectors: [][]float32{{1, 0}}}); err == nil || !strings.Contains(err.Error(), "1 vectors for 2 texts") {
		t.Fatalf("vector count error = %v", err)
	}
	badDimensions := fixedSnapshotEmbedder{vectors: [][]float32{{1, 0}, {1}}}
	if _, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock", candidate, 1, badDimensions); err == nil || !strings.Contains(err.Error(), `rank "dock"`) {
		t.Fatalf("vector dimension error = %v", err)
	}
}

func TestSearchSnapshotSemantic_UsesMostRecentCapAndOnlyFrozenCandidates(t *testing.T) {
	t.Parallel()
	candidates := make([]skills.Skill, 0, skills.SnapshotSemanticCandidateCap+10)
	for i := range skills.SnapshotSemanticCandidateCap + 10 {
		candidates = append(candidates, snapshotCandidate("candidate-"+time.Unix(int64(i), 0).UTC().Format("150405"), "candidate", time.Unix(int64(i), 0)))
	}
	embedder := &countingSnapshotEmbedder{}
	result, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock a boat", candidates, skills.SnapshotSemanticCandidateCap, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != skills.SnapshotSemanticCandidateCap {
		t.Fatalf("result count=%d want=%d", len(result), skills.SnapshotSemanticCandidateCap)
	}
	embedder.mu.Lock()
	batch := embedder.batch
	embedder.mu.Unlock()
	if batch != skills.SnapshotSemanticCandidateCap+1 {
		t.Fatalf("billed batch=%d want=%d", batch, skills.SnapshotSemanticCandidateCap+1)
	}
	if result[0].Skill.UpdatedAt.Before(result[len(result)-1].Skill.UpdatedAt) {
		t.Fatalf("most-recent candidate ordering was not retained: first=%s last=%s", result[0].Skill.UpdatedAt, result[len(result)-1].Skill.UpdatedAt)
	}
	if _, err := skills.SearchSnapshotSemantic(context.Background(), snapshotIdentity(), "dock a boat", []skills.Skill{snapshotCandidate("dock", "how to dock a boat", time.Now())}, 1, embeddingstest.New()); err != nil {
		t.Fatalf("frozen candidate semantic search: %v", err)
	}
}
