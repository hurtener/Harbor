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

// SnapshotSemanticCandidateCap is the maximum number of frozen skills a
// semantic snapshot search may embed. One billed request therefore contains at
// most this many candidate texts plus the query text.
const SnapshotSemanticCandidateCap = 256

// SearchSnapshotRegexExact applies the canonical non-full-text tail of the
// lexical ladder to a frozen candidate view. Driver-owned SearchSnapshot
// implementations call it only after their configured full-text tier was
// unavailable or returned no candidate rows; it never represents this result
// as FTS5.
func SearchSnapshotRegexExact(ctx context.Context, query string, candidates []Skill, limit int) ([]RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []RankedSkill{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result, err := searchSnapshotRegex(ctx, query, candidates); err != nil {
		return nil, err
	} else if len(result) > 0 {
		return truncateSnapshotResults(result, limit), nil
	}
	result, err := searchSnapshotExact(ctx, query, candidates)
	if err != nil {
		return nil, err
	}
	return truncateSnapshotResults(result, limit), nil
}

var snapshotSearchTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// SnapshotSearchTokens returns the safe full-text tokens shared by the
// backends' normal Search paths. User bytes never reach an FTS parser as raw
// syntax.
func SnapshotSearchTokens(query string) []string {
	return snapshotSearchTokenRE.FindAllString(strings.ToLower(query), -1)
}

func searchSnapshotRegex(ctx context.Context, query string, candidates []Skill) ([]RankedSkill, error) {
	re := snapshotSearchRegex(query)
	if re == nil {
		return []RankedSkill{}, nil // invalid regex is the canonical exact-tail fallthrough.
	}
	result := make([]RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if score := snapshotRegexScore(re, skill); score > 0 {
			result = append(result, RankedSkill{Skill: skill, Score: score, Path: PathRegex})
		}
	}
	SortSnapshotResults(result)
	return result, nil
}

func snapshotSearchRegex(query string) *regexp.Regexp {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if !strings.ContainsAny(q, " \t\n") {
		if re, err := regexp.Compile("(?i)" + q); err == nil {
			return re
		}
	}
	tokens := SnapshotSearchTokens(q)
	if len(tokens) == 0 {
		return nil
	}
	escaped := make([]string, len(tokens))
	for i, token := range tokens {
		escaped[i] = regexp.QuoteMeta(token)
	}
	re, err := regexp.Compile("(?i)(" + strings.Join(escaped, "|") + ")")
	if err != nil {
		return nil
	}
	return re
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

func searchSnapshotExact(ctx context.Context, query string, candidates []Skill) ([]RankedSkill, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, nil
	}
	result := make([]RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(skill.Name), needle) || strings.EqualFold(strings.TrimSpace(skill.Title), needle) || strings.EqualFold(strings.TrimSpace(skill.Trigger), needle) || snapshotExactTag(skill.Tags, needle) {
			result = append(result, RankedSkill{Skill: skill, Score: 1, Path: PathExact})
		}
	}
	SortSnapshotResults(result)
	return result, nil
}

func snapshotExactTag(tags []string, needle string) bool {
	for _, tag := range tags {
		// The configured SQLite and PostgreSQL drivers both use their
		// tags_text LIKE fallback, so the frozen-candidate tail retains that
		// established matching behavior rather than narrowing it to tag
		// equality.
		if strings.Contains(strings.ToLower(tag), needle) {
			return true
		}
	}
	return false
}

// SortSnapshotResults applies the canonical result ordering shared by the
// driver-owned frozen-candidate paths.
func SortSnapshotResults(result []RankedSkill) {
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

// TruncateSnapshotResults returns at most limit ranked rows. A zero limit is
// already normalized by the caller's public Search contract.
func TruncateSnapshotResults(result []RankedSkill, limit int) []RankedSkill {
	if len(result) > limit {
		return result[:limit]
	}
	return result
}

func truncateSnapshotResults(result []RankedSkill, limit int) []RankedSkill {
	return TruncateSnapshotResults(result, limit)
}

// SearchSnapshotSemantic ranks the most-recent 256 frozen candidates using
// one query-plus-candidates embedding batch. It is shared by driver-owned
// semantic paths so semantic snapshot behavior preserves the configured
// backend policy while never exceeding the established billed-call bound.
func SearchSnapshotSemantic(ctx context.Context, id identity.Quadruple, query string, candidates []Skill, limit int, embedder Embedder) ([]RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(candidates) == 0 {
		return []RankedSkill{}, nil
	}
	if embedder == nil {
		return nil, errors.New("skills: snapshot semantic retrieval is missing Embedder")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ordered := append([]Skill(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
		}
		return ordered[i].Name < ordered[j].Name
	})
	if len(ordered) > SnapshotSemanticCandidateCap {
		ordered = ordered[:SnapshotSemanticCandidateCap]
	}
	texts := make([]string, 0, len(ordered)+1)
	texts = append(texts, query)
	for _, skill := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		texts = append(texts, SnapshotSearchText(skill))
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
	result := make([]RankedSkill, 0, len(ordered))
	for i, skill := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cosine, err := embeddings.Cosine(vectors[0], vectors[i+1])
		if err != nil {
			return nil, fmt.Errorf("skills: snapshot semantic rank %q: %w", skill.Name, err)
		}
		result = append(result, RankedSkill{Skill: skill, Score: (cosine + 1) / 2, Path: PathSemantic})
	}
	SortSnapshotResults(result)
	return TruncateSnapshotResults(result, limit), nil
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

// SnapshotSearchText is the same planner-visible text surface all current
// driver full-text and semantic search paths index.
func SnapshotSearchText(skill Skill) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{skill.Name, skill.Title, skill.Trigger, skill.Description, strings.Join(skill.Tags, " ")} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}
