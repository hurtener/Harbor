package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// SearchSnapshot implements skills.SnapshotCandidateSearcher. It uses this
// configured driver's PostgreSQL full-text implementation over exactly the
// frozen composed candidates; it never consults mutable catalog rows.
func (d *driver) SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if d.closed.Load() {
		return nil, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.EmitIdentityRejected(ctx, d.bus, id, "SearchSnapshot")
	}
	if limit <= 0 {
		limit = defaultSearchN
	}
	if limit > maxSearchN {
		limit = maxSearchN
	}
	if d.retrieval == skills.RetrievalSemantic {
		return skills.SearchSnapshotSemantic(ctx, id, query, candidates, limit, d.embedder)
	}
	result, err := d.searchSnapshotFTS(ctx, query, candidates, limit)
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		return result, nil
	}
	return skills.SearchSnapshotRegexExact(ctx, query, candidates, limit)
}

// searchSnapshotFTS sends the frozen candidate values through PostgreSQL's
// real to_tsvector/to_tsquery/ts_rank path. The CTE is per-query data, not a
// shared temporary table, so concurrent runs cannot see or mutate each other.
func (d *driver) searchSnapshotFTS(ctx context.Context, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	query = strings.TrimSpace(query)
	tokens := skills.SnapshotSearchTokens(query)
	if query == "" || len(tokens) == 0 || len(candidates) == 0 {
		return nil, nil
	}
	run := func(expression string) ([]snapshotFTSHit, error) {
		args := make([]any, 0, len(candidates)*4+2)
		var statement strings.Builder
		statement.WriteString("WITH snapshot(ordinal, search_text, updated_at, name) AS (VALUES ")
		for i, skill := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if i > 0 {
				statement.WriteString(",")
			}
			start := len(args) + 1
			fmt.Fprintf(&statement, "($%d::integer,$%d::text,$%d::timestamptz,$%d::text)", start, start+1, start+2, start+3)
			args = append(args, i, skills.SnapshotSearchText(skill), skill.UpdatedAt.UTC(), skill.Name)
		}
		queryArg := len(args) + 1
		limitArg := queryArg + 1
		statement.WriteString(") SELECT ordinal, ts_rank(to_tsvector('english', search_text), q) AS rank FROM snapshot, to_tsquery('english', $")
		fmt.Fprint(&statement, queryArg)
		statement.WriteString(") q WHERE to_tsvector('english', search_text) @@ q ORDER BY rank DESC, updated_at DESC, name ASC LIMIT $")
		fmt.Fprint(&statement, limitArg)
		args = append(args, expression, limit)
		rows, err := d.db.QueryContext(ctx, statement.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: snapshot fts query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		result := make([]snapshotFTSHit, 0, limit)
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var hit snapshotFTSHit
			if err := rows.Scan(&hit.ordinal, &hit.raw); err != nil {
				return nil, fmt.Errorf("skills/postgres: snapshot fts scan: %w", err)
			}
			result = append(result, hit)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("skills/postgres: snapshot fts iterate: %w", err)
		}
		return result, nil
	}
	hits, err := run(strings.Join(tokens, " & "))
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && len(tokens) > 1 {
		hits, err = run(strings.Join(tokens, " | "))
		if err != nil {
			return nil, err
		}
	}
	return snapshotFTSResults(candidates, hits)
}

type snapshotFTSHit struct {
	ordinal int
	raw     float64
}

func snapshotFTSResults(candidates []skills.Skill, hits []snapshotFTSHit) ([]skills.RankedSkill, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	minRaw, maxRaw := hits[0].raw, hits[0].raw
	for _, hit := range hits[1:] {
		if hit.raw < minRaw {
			minRaw = hit.raw
		}
		if hit.raw > maxRaw {
			maxRaw = hit.raw
		}
	}
	result := make([]skills.RankedSkill, 0, len(hits))
	for _, hit := range hits {
		if hit.ordinal < 0 || hit.ordinal >= len(candidates) {
			return nil, fmt.Errorf("skills/postgres: snapshot fts returned out-of-range ordinal %d", hit.ordinal)
		}
		score := 1.0
		if maxRaw != minRaw {
			score = (hit.raw - minRaw) / (maxRaw - minRaw)
		}
		result = append(result, skills.RankedSkill{Skill: candidates[hit.ordinal], Score: score, Path: skills.PathFullText})
	}
	skills.SortSnapshotResults(result)
	return result, nil
}

var _ skills.SnapshotCandidateSearcher = (*driver)(nil)
