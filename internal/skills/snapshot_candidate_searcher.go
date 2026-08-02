package skills

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/identity"
)

// SnapshotCandidateSearcher is an immutable search policy over a caller-owned
// run snapshot. The zero value is not usable; construct it with
// NewSnapshotCandidateSearcher.
type snapshotCandidateSearcher struct {
	retrieval RetrievalMode
	embedder  Embedder
}

const defaultSnapshotSearchLimit = 20

// NewSnapshotCandidateSearcher captures the configured skill retrieval policy
// for an immutable candidate view. Default retrieval retains the lexical
// full-text -> regex -> exact ladder. Semantic retrieval requires an Embedder
// at construction and never falls back to lexical ranking when embedding fails.
func NewSnapshotCandidateSearcher(retrieval RetrievalMode, embedder Embedder) (SnapshotCandidateSearcher, error) {
	switch retrieval {
	case RetrievalDefault:
		return snapshotCandidateSearcher{retrieval: retrieval}, nil
	case RetrievalSemantic:
		if embedder == nil {
			return nil, fmt.Errorf("skills: Embedder is required for snapshot retrieval mode %q (no stub fallback)", RetrievalSemantic)
		}
		return snapshotCandidateSearcher{retrieval: retrieval, embedder: embedder}, nil
	default:
		return nil, fmt.Errorf("skills: unknown snapshot retrieval mode %q (expected \"\" or %q)", retrieval, RetrievalSemantic)
	}
}

// SearchSnapshot implements SnapshotCandidateSearcher. Candidates are never
// mutated or re-read from storage, so ranking observes exactly the run-start
// composed view supplied by the caller.
func (s snapshotCandidateSearcher) SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []Skill, limit int) ([]RankedSkill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateIdentity(id); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 1000 {
		return nil, fmt.Errorf("skills: invalid snapshot search limit %d", limit)
	}
	if limit == 0 {
		limit = defaultSnapshotSearchLimit
	}
	switch s.retrieval {
	case RetrievalDefault:
		return searchSnapshotLexical(ctx, query, candidates, limit)
	case RetrievalSemantic:
		if s.embedder == nil {
			return nil, fmt.Errorf("skills: snapshot semantic retrieval is missing Embedder (no stub fallback)")
		}
		return searchSnapshotSemantic(ctx, id, query, candidates, limit, s.embedder)
	default:
		return nil, fmt.Errorf("skills: unknown snapshot retrieval mode %q", s.retrieval)
	}
}

// searchSnapshotLexical preserves the public ladder's selection semantics:
// full-text first (strict-AND, then OR-of-tokens), regex only when full-text
// returned no rows, then exact only when regex returned no rows. All candidate
// ordering is stable at score DESC, UpdatedAt DESC, Name ASC.
func searchSnapshotLexical(ctx context.Context, query string, candidates []Skill, limit int) ([]RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []RankedSkill{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result := searchSnapshotFullText(query, candidates); len(result) > 0 {
		return truncateSnapshotResults(result, limit), nil
	}
	if result := searchSnapshotRegex(query, candidates); len(result) > 0 {
		return truncateSnapshotResults(result, limit), nil
	}
	return truncateSnapshotResults(searchSnapshotExact(query, candidates), limit), nil
}

var snapshotSearchTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

func searchSnapshotFullText(query string, candidates []Skill) []RankedSkill {
	tokens := snapshotSearchTokenRE.FindAllString(strings.ToLower(query), -1)
	if len(tokens) == 0 {
		return nil
	}
	result := snapshotFullTextMatches(tokens, candidates, true)
	if len(result) == 0 && len(tokens) > 1 {
		result = snapshotFullTextMatches(tokens, candidates, false)
	}
	if len(result) == 0 {
		return nil
	}

	// Match the configured full-text paths' normalised 0..1 scoring contract.
	minRaw, maxRaw := result[0].Score, result[0].Score
	for _, hit := range result[1:] {
		if hit.Score < minRaw {
			minRaw = hit.Score
		}
		if hit.Score > maxRaw {
			maxRaw = hit.Score
		}
	}
	for i := range result {
		if maxRaw == minRaw {
			result[i].Score = 1
		} else {
			result[i].Score = (result[i].Score - minRaw) / (maxRaw - minRaw)
		}
		result[i].Path = PathFTS5
	}
	sortSnapshotResults(result)
	return result
}

func snapshotFullTextMatches(tokens []string, candidates []Skill, requireAll bool) []RankedSkill {
	result := make([]RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		text := strings.ToLower(snapshotSearchText(skill))
		matched := 0
		for _, token := range tokens {
			if strings.Contains(text, token) {
				matched++
			}
		}
		if (requireAll && matched != len(tokens)) || (!requireAll && matched == 0) {
			continue
		}
		// A deterministic relevance proxy for the portable frozen candidate
		// view. It preserves the full-text tier's monotonic rank + normalised
		// score contract without consulting mutable backing storage.
		score := float64(matched) / float64(len(tokens))
		score += float64(strings.Count(text, tokens[0])) / float64(len(text)+1)
		result = append(result, RankedSkill{Skill: skill, Score: score})
	}
	return result
}

func searchSnapshotRegex(query string, candidates []Skill) []RankedSkill {
	re, err := snapshotSearchRegex(query)
	if err != nil {
		return nil
	}
	result := make([]RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		if score := snapshotRegexScore(re, skill); score > 0 {
			result = append(result, RankedSkill{Skill: skill, Score: score, Path: PathRegex})
		}
	}
	sortSnapshotResults(result)
	return result
}

func snapshotSearchRegex(query string) (*regexp.Regexp, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, errors.New("empty regex")
	}
	if !strings.ContainsAny(q, " \t\n") {
		if re, err := regexp.Compile("(?i)" + q); err == nil {
			return re, nil
		}
	}
	tokens := snapshotSearchTokenRE.FindAllString(q, -1)
	if len(tokens) == 0 {
		return nil, errors.New("no regex tokens")
	}
	escaped := make([]string, len(tokens))
	for i, token := range tokens {
		escaped[i] = regexp.QuoteMeta(token)
	}
	return regexp.Compile("(?i)(" + strings.Join(escaped, "|") + ")")
}

func snapshotRegexScore(re *regexp.Regexp, skill Skill) float64 {
	name := strings.ToLower(skill.Name)
	if loc := re.FindStringIndex(name); loc != nil && loc[0] == 0 && loc[1] == len(name) {
		return 0.95
	}
	if loc := re.FindStringIndex(name); loc != nil && loc[0] == 0 {
		return 0.90
	}
	if re.MatchString(name) {
		return 0.85
	}
	if re.MatchString(strings.ToLower(skill.Title + " " + skill.Description + " " + skill.Trigger + " " + strings.Join(skill.Tags, " "))) {
		return 0.75
	}
	return 0
}

func searchSnapshotExact(query string, candidates []Skill) []RankedSkill {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil
	}
	result := make([]RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		if strings.EqualFold(strings.TrimSpace(skill.Name), needle) || strings.EqualFold(strings.TrimSpace(skill.Title), needle) || strings.EqualFold(strings.TrimSpace(skill.Trigger), needle) || snapshotExactTag(skill.Tags, needle) {
			result = append(result, RankedSkill{Skill: skill, Score: 1, Path: PathExact})
		}
	}
	sortSnapshotResults(result)
	return result
}

func snapshotExactTag(tags []string, needle string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), needle) {
			return true
		}
	}
	return false
}

func sortSnapshotResults(result []RankedSkill) {
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		if !result[i].Skill.UpdatedAt.Equal(result[j].Skill.UpdatedAt) {
			return result[i].Skill.UpdatedAt.After(result[j].Skill.UpdatedAt)
		}
		return result[i].Skill.Name < result[j].Skill.Name
	})
}

func truncateSnapshotResults(result []RankedSkill, limit int) []RankedSkill {
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func searchSnapshotSemantic(ctx context.Context, id identity.Quadruple, query string, candidates []Skill, limit int, embedder Embedder) ([]RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(candidates) == 0 {
		return []RankedSkill{}, nil
	}
	texts := make([]string, 0, len(candidates)+1)
	texts = append(texts, query)
	for _, skill := range candidates {
		texts = append(texts, snapshotSearchText(skill))
	}
	embeddingCtx, err := snapshotEmbeddingIdentityContext(ctx, id)
	if err != nil {
		return nil, err
	}
	vectors, err := embedder.Embed(embeddingCtx, texts)
	if err != nil {
		return nil, fmt.Errorf("skills: snapshot semantic embed: %w", err)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("skills: snapshot embedder returned %d vectors for %d texts", len(vectors), len(texts))
	}
	result := make([]RankedSkill, 0, len(candidates))
	for i, skill := range candidates {
		cosine, err := embeddings.Cosine(vectors[0], vectors[i+1])
		if err != nil {
			return nil, fmt.Errorf("skills: snapshot semantic rank %q: %w", skill.Name, err)
		}
		result = append(result, RankedSkill{Skill: skill, Score: (cosine + 1) / 2, Path: PathSemantic})
	}
	sortSnapshotResults(result)
	return truncateSnapshotResults(result, limit), nil
}

func snapshotEmbeddingIdentityContext(ctx context.Context, id identity.Quadruple) (context.Context, error) {
	if verified, ok := identity.FromVerified(ctx); ok && id.TenantID != verified.TenantID {
		elevated, err := identity.WithElevated(ctx, id.Identity, "skills snapshot semantic search: attributing embedding to the resolved candidate identity")
		if err != nil {
			return nil, fmt.Errorf("skills: snapshot embedding identity: %w", err)
		}
		ctx = elevated
	}
	if id.RunID != "" {
		withRun, err := identity.WithRun(ctx, id.Identity, id.RunID)
		if err != nil {
			return nil, fmt.Errorf("skills: snapshot embedding identity: %w", err)
		}
		return withRun, nil
	}
	withIdentity, err := identity.With(ctx, id.Identity)
	if err != nil {
		return nil, fmt.Errorf("skills: snapshot embedding identity: %w", err)
	}
	return withIdentity, nil
}

func snapshotSearchText(skill Skill) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{skill.Name, skill.Title, skill.Trigger, skill.Description, strings.Join(skill.Tags, " ")} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

var _ SnapshotCandidateSearcher = snapshotCandidateSearcher{}
