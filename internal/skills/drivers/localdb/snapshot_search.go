package localdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// SearchSnapshot implements skills.SnapshotCandidateSearcher. It ranks only
// the frozen composed candidates supplied by the resolver while retaining this
// driver's actual FTS5 availability and porter/unicode61 ranking semantics.
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
	if d.ftsAvailable {
		result, err := d.searchSnapshotFTS5(ctx, query, candidates, limit)
		if err != nil {
			return nil, err
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	return skills.SearchSnapshotRegexExact(ctx, query, candidates, limit)
}

// searchSnapshotFTS5 builds a connection-local FTS5 index over exactly the
// frozen candidate bodies. A portable contains scorer must never claim the
// fts5 path; this invokes the same FTS5 tokenizer and bm25 function as the
// persistent catalog table. The localdb pool is intentionally one connection,
// so the temporary relation cannot race another call.
func (d *driver) searchSnapshotFTS5(ctx context.Context, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	query = strings.TrimSpace(query)
	tokens := skills.SnapshotSearchTokens(query)
	if query == "" || len(tokens) == 0 || len(candidates) == 0 {
		return nil, nil
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: snapshot fts5 connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	const table = "snapshot_skills_fts"
	if _, err := conn.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.`+table+` USING fts5(
        name, title, trigger, description, tags_text, updated_at UNINDEXED,
        tokenize='porter unicode61 remove_diacritics 1')`); err != nil {
		return nil, fmt.Errorf("skills/localdb: snapshot fts5 create: %w", err)
	}
	defer func() {
		// The request may be cancelled after a successful CREATE. Dropping the
		// connection-local relation still has to run before this sql.Conn goes
		// back to the pool. The short cleanup context prevents cancellation from
		// stranding the table without making teardown unbounded.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if _, dropErr := conn.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS temp."+table); dropErr != nil {
			return
		}
	}()
	for i, skill := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO temp.`+table+`(rowid, name, title, trigger, description, tags_text, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			i+1, skill.Name, skill.Title, skill.Trigger, skill.Description, strings.Join(skill.Tags, " "), skill.UpdatedAt.UTC().UnixNano()); err != nil {
			return nil, fmt.Errorf("skills/localdb: snapshot fts5 insert: %w", err)
		}
	}
	search := func(expr string) ([]snapshotFTS5Hit, error) {
		rows, err := conn.QueryContext(ctx, `SELECT rowid, bm25(`+table+`) FROM temp.`+table+` WHERE `+table+` MATCH ? ORDER BY bm25(`+table+`) ASC, updated_at DESC, name ASC LIMIT ?`, expr, limit)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: snapshot fts5 query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		result := make([]snapshotFTS5Hit, 0, limit)
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var hit snapshotFTS5Hit
			if err := rows.Scan(&hit.ordinal, &hit.raw); err != nil {
				return nil, fmt.Errorf("skills/localdb: snapshot fts5 scan: %w", err)
			}
			result = append(result, hit)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("skills/localdb: snapshot fts5 iterate: %w", err)
		}
		return result, nil
	}
	hits, err := search(snapshotFTS5Expression(tokens, " AND "))
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && len(tokens) > 1 {
		hits, err = search(snapshotFTS5Expression(tokens, " OR "))
		if err != nil {
			return nil, err
		}
	}
	return snapshotFTS5Results(candidates, hits)
}

type snapshotFTS5Hit struct {
	ordinal int
	raw     float64
}

func snapshotFTS5Expression(tokens []string, join string) string {
	parts := make([]string, len(tokens))
	for i, token := range tokens {
		parts[i] = `"` + token + `"`
	}
	return strings.Join(parts, join)
}

func snapshotFTS5Results(candidates []skills.Skill, hits []snapshotFTS5Hit) ([]skills.RankedSkill, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	minInv, maxInv := 0.0, 0.0
	inverted := make([]float64, len(hits))
	for i, hit := range hits {
		if hit.ordinal < 1 || hit.ordinal > len(candidates) {
			return nil, fmt.Errorf("skills/localdb: snapshot fts5 returned out-of-range ordinal %d", hit.ordinal)
		}
		inverted[i] = 1 / (1 + hit.raw)
		if i == 0 || inverted[i] < minInv {
			minInv = inverted[i]
		}
		if i == 0 || inverted[i] > maxInv {
			maxInv = inverted[i]
		}
	}
	result := make([]skills.RankedSkill, 0, len(hits))
	for i, hit := range hits {
		score := 1.0
		if maxInv != minInv {
			score = (inverted[i] - minInv) / (maxInv - minInv)
		}
		result = append(result, skills.RankedSkill{Skill: candidates[hit.ordinal-1], Score: score, Path: skills.PathFTS5})
	}
	skills.SortSnapshotResults(result)
	return result, nil
}

var _ skills.SnapshotCandidateSearcher = (*driver)(nil)
