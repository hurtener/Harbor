package localdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// search executes the three-tier ranking ladder per brief 04 §4.4.
//
// Ladder:
//
//  1. FTS5 — strict AND first; if zero rows, OR-of-tokens fallback.
//     Score = bm25(raw) → 1/(1+raw) → min-max normalised 0..1.
//  2. Regex — try compiling the query as-is; for multi-token queries
//     compile an OR-of-tokens regex. Scoring constants per brief 04
//     §4.4: name fullmatch=0.95 / name match=0.90 / name search=0.85
//     / body search=0.75.
//  3. Exact — lowercase equality on `name | title | trigger | tags`.
//     Score=1.0.
//
// The first path that returns rows wins; subsequent paths run only
// when earlier ones produced nothing. Ties are broken by
// `(updated_at DESC, name ASC)`.
func (d *driver) search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, skills.PathFTS5, nil
	}

	if d.ftsAvailable {
		results, err := d.searchFTS5(ctx, id, query, limit)
		if err != nil {
			return nil, "", err
		}
		if len(results) > 0 {
			return results, skills.PathFTS5, nil
		}
	}

	results, err := d.searchRegex(ctx, id, query, limit)
	if err != nil {
		return nil, "", err
	}
	if len(results) > 0 {
		return results, skills.PathRegex, nil
	}

	results, err = d.searchExact(ctx, id, query, limit)
	if err != nil {
		return nil, "", err
	}
	return results, skills.PathExact, nil
}

// ftsTokenRE captures the alphanumeric tokens brief 04 §4.4 uses for
// FTS query construction. Punctuation / quoting is stripped so the
// FTS5 parser never sees user-controlled syntax.
var ftsTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// searchFTS5 runs the FTS5 path. Strict-AND first, then OR fallback.
// The MATCH query is built from tokenised input only — caller bytes
// never reach the FTS5 parser uncontrolled.
func (d *driver) searchFTS5(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	tokens := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	if len(tokens) == 0 {
		return nil, nil
	}

	tryQuery := func(matchExpr string) ([]ftsHit, error) {
		const sel = `
            SELECT s.rowid, bm25(skills_fts)
            FROM skills_fts
            JOIN skills s ON s.rowid = skills_fts.rowid
            WHERE skills_fts MATCH ?
              AND s.tenant = ? AND s.user = ? AND s.session = ?
            ORDER BY bm25(skills_fts) ASC, s.updated_at DESC, s.name ASC
            LIMIT ?`
		rows, err := d.db.QueryContext(ctx, sel,
			matchExpr, id.TenantID, id.UserID, id.SessionID, limit)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: fts5 query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var hits []ftsHit
		for rows.Next() {
			var h ftsHit
			if err := rows.Scan(&h.rowID, &h.rawScore); err != nil {
				return nil, fmt.Errorf("skills/localdb: fts5 scan: %w", err)
			}
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("skills/localdb: fts5 iterate: %w", err)
		}
		return hits, nil
	}

	hits, err := tryQuery(buildANDExpr(tokens))
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && len(tokens) > 1 {
		hits, err = tryQuery(buildORExpr(tokens))
		if err != nil {
			return nil, err
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}
	return d.materializeFTSHits(ctx, hits)
}

type ftsHit struct {
	rowID    int64
	rawScore float64
}

// materializeFTSHits fetches the skill rows for the matched rowids
// and applies the brief 04 §4.4 scoring: `1/(1+raw) → min-max
// normalised`. bm25 returns SMALLER values for stronger matches so
// `1/(1+raw)` is the brief's documented inversion.
func (d *driver) materializeFTSHits(ctx context.Context, hits []ftsHit) ([]skills.RankedSkill, error) {
	// Build the IN clause + arg list. Order preservation done in Go
	// after fetch using the per-hit score.
	ids := make([]any, len(hits))
	placeholders := make([]string, len(hits))
	rawByRowID := make(map[int64]float64, len(hits))
	for i, h := range hits {
		ids[i] = h.rowID
		placeholders[i] = "?"
		rawByRowID[h.rowID] = h.rawScore
	}
	// The concatenated parts are a compile-time column constant plus a
	// run of literal "?" placeholders — no user input reaches the SQL
	// text; the rowids flow in via the parameterised `ids...` args.
	//nolint:gosec // G202 false positive: only a const + literal "?" placeholders are concatenated
	sel := `SELECT ` + skillCols + `, rowid FROM skills WHERE rowid IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := d.db.QueryContext(ctx, sel, ids...)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: fts5 materialize: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rowWithRaw struct {
		s    skills.Skill
		raw  float64
		inv  float64
		name string
	}
	var rowsOut []rowWithRaw
	for rows.Next() {
		var s skills.Skill
		var rowID int64
		var tagsJSON, stepsJSON, preJSON, failJSON, rtJSON, rnsJSON, rtgJSON string
		var origin, scope, extraJSON string
		if err := rows.Scan(
			&s.Name, &s.Title, &s.Description, &s.Trigger, &s.TaskType,
			&tagsJSON, &stepsJSON, &preJSON, &failJSON,
			&rtJSON, &rnsJSON, &rtgJSON,
			&origin, &s.OriginRef, &scope, &s.ScopeTenantID, &s.ScopeProjectID,
			&s.ContentHash, &s.CreatedAt, &s.UpdatedAt, &s.LastUsed, &s.UseCount, &extraJSON,
			&rowID,
		); err != nil {
			return nil, fmt.Errorf("skills/localdb: fts5 row scan: %w", err)
		}
		s.Origin = skills.Origin(origin)
		s.Scope = skills.Scope(scope)
		s.Tags = unmarshalStrings(tagsJSON)
		s.Steps = unmarshalStrings(stepsJSON)
		s.Preconditions = unmarshalStrings(preJSON)
		s.FailureModes = unmarshalStrings(failJSON)
		s.RequiredTools = unmarshalStrings(rtJSON)
		s.RequiredNS = unmarshalStrings(rnsJSON)
		s.RequiredTags = unmarshalStrings(rtgJSON)
		s.Extra = unmarshalExtra(extraJSON)
		raw := rawByRowID[rowID]
		inv := 1.0 / (1.0 + raw)
		rowsOut = append(rowsOut, rowWithRaw{s: s, raw: raw, inv: inv, name: s.Name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: fts5 iterate materialize: %w", err)
	}

	// Min-max normalise inv → 0..1. Single-row results land at 1.0
	// (the canonical "best possible match") so the score is stable.
	if len(rowsOut) == 0 {
		return nil, nil
	}
	minInv := rowsOut[0].inv
	maxInv := rowsOut[0].inv
	for _, r := range rowsOut[1:] {
		if r.inv < minInv {
			minInv = r.inv
		}
		if r.inv > maxInv {
			maxInv = r.inv
		}
	}
	out := make([]skills.RankedSkill, 0, len(rowsOut))
	for _, r := range rowsOut {
		var score float64
		if maxInv == minInv {
			score = 1.0
		} else {
			score = (r.inv - minInv) / (maxInv - minInv)
		}
		out = append(out, skills.RankedSkill{Skill: r.s, Score: score, Path: skills.PathFTS5})
	}
	// Stable ordering: score DESC, updated_at DESC, name ASC.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if !out[i].Skill.UpdatedAt.Equal(out[j].Skill.UpdatedAt) {
			return out[i].Skill.UpdatedAt.After(out[j].Skill.UpdatedAt)
		}
		return out[i].Skill.Name < out[j].Skill.Name
	})
	return out, nil
}

// buildANDExpr returns the strict-AND FTS5 MATCH expression for
// tokens. Tokens are quoted to escape FTS5 syntax.
func buildANDExpr(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = `"` + t + `"`
	}
	return strings.Join(parts, " AND ")
}

// buildORExpr returns the OR-of-tokens FTS5 MATCH expression.
func buildORExpr(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = `"` + t + `"`
	}
	return strings.Join(parts, " OR ")
}

// searchRegex runs the regex fallback. Per brief 04 §4.4:
//
//   - Try compiling the query as-is; for multi-token queries, fall
//     back to OR-of-tokens regex (NL queries are rarely intentional
//     regex).
//   - Scoring: name fullmatch=0.95 / name match=0.90 /
//     name search=0.85 / body search=0.75.
//
// "fullmatch" = re matches the entire `name`; "match" = re matches
// somewhere in `name` from start; "search" = re finds the pattern
// anywhere in the field. We rank using the highest of those for
// each candidate row.
func (d *driver) searchRegex(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	re, err := buildRegex(query)
	if err != nil {
		return nil, nil // unparseable regex → empty, ladder falls through to exact
	}

	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant = ? AND user = ? AND session = ?
        ORDER BY updated_at DESC, name ASC`,
		id.TenantID, id.UserID, id.SessionID)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: regex query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []skills.RankedSkill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: regex scan: %w", err)
		}
		score := regexScore(re, s)
		if score == 0 {
			continue
		}
		out = append(out, skills.RankedSkill{Skill: s, Score: score, Path: skills.PathRegex})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: regex iterate: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if !out[i].Skill.UpdatedAt.Equal(out[j].Skill.UpdatedAt) {
			return out[i].Skill.UpdatedAt.After(out[j].Skill.UpdatedAt)
		}
		return out[i].Skill.Name < out[j].Skill.Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// buildRegex compiles `query` as a regex; if compilation fails (or
// the query has whitespace, signalling an NL query), falls back to
// `(?i)(tok1|tok2|...)`.
func buildRegex(query string) (*regexp.Regexp, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("empty regex")
	}
	if !strings.ContainsAny(q, " \t\n") {
		if re, err := regexp.Compile("(?i)" + q); err == nil {
			return re, nil
		}
	}
	tokens := ftsTokenRE.FindAllString(q, -1)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no regex tokens")
	}
	escaped := make([]string, len(tokens))
	for i, t := range tokens {
		escaped[i] = regexp.QuoteMeta(t)
	}
	return regexp.Compile("(?i)(" + strings.Join(escaped, "|") + ")")
}

// regexScore applies brief 04 §4.4's per-field scoring constants.
// Returns the maximum applicable score for `s` against `re`, or 0
// if no field matched.
func regexScore(re *regexp.Regexp, s skills.Skill) float64 {
	lowerName := strings.ToLower(s.Name)
	// 0.95 — re matches the ENTIRE lowercased name.
	if loc := re.FindStringIndex(lowerName); loc != nil && loc[0] == 0 && loc[1] == len(lowerName) {
		return 0.95
	}
	// 0.90 — anchored prefix match on name.
	if loc := re.FindStringIndex(lowerName); loc != nil && loc[0] == 0 {
		return 0.90
	}
	// 0.85 — name search (re finds anywhere in name).
	if re.MatchString(lowerName) {
		return 0.85
	}
	// 0.75 — body search.
	body := strings.ToLower(s.Title + " " + s.Description + " " + s.Trigger + " " + strings.Join(s.Tags, " "))
	if re.MatchString(body) {
		return 0.75
	}
	return 0
}

// searchExact runs the lowercase-equality fallback. Score=1.0 on
// every row that matches.
func (d *driver) searchExact(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant = ? AND user = ? AND session = ?
          AND (
              lower(name) = ?
              OR lower(title) = ?
              OR lower(trigger) = ?
              OR lower(tags_text) LIKE ?
          )
        ORDER BY updated_at DESC, name ASC
        LIMIT ?`,
		id.TenantID, id.UserID, id.SessionID,
		q, q, q, "%"+q+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: exact query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []skills.RankedSkill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: exact scan: %w", err)
		}
		out = append(out, skills.RankedSkill{Skill: s, Score: 1.0, Path: skills.PathExact})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: exact iterate: %w", err)
	}
	return out, nil
}

// hashQuery returns the canonical query-hash for audit emission.
// First 16 hex chars of sha256(lowercased trimmed query).
func hashQuery(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])[:queryHashHexChars]
}
