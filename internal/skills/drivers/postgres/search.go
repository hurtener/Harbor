package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// queryHashHexChars is the number of hex chars of sha256(query) the
// audit event carries. Matches the SQLite driver.
const queryHashHexChars = 16

// semanticCandidateCap bounds how many identity-scoped rows one
// semantic search embeds. Mirrors the SQLite driver: skill catalogs
// are small at this scale, so a brute-force cosine over the most-
// recently-updated candidate window is the deliberate design.
const semanticCandidateCap = 256

// Search implements skills.SkillStore. It runs the backend-agnostic
// ranking ladder (full-text -> regex -> exact) — or, when the opt-in
// semantic retrieval mode is configured, ranks by embedding
// similarity — and emits `skill.search_executed` with the path that
// produced the result. Behaviour matches the SQLite driver.
func (d *driver) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if d.closed.Load() {
		return nil, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.EmitIdentityRejected(ctx, d.bus, id, "Search")
	}
	if limit <= 0 {
		limit = defaultSearchN
	}
	if limit > maxSearchN {
		limit = maxSearchN
	}

	var (
		results []skills.RankedSkill
		path    string
		err     error
	)
	if d.retrieval == skills.RetrievalSemantic {
		// Opt-in semantic ranking. An embedding failure fails the
		// search loudly — the driver NEVER silently degrades to the
		// lexical ladder (AGENTS.md §13).
		results, err = d.searchSemantic(ctx, id, query, limit)
		path = skills.PathSemantic
	} else {
		results, path, err = d.search(ctx, id, query, limit)
	}
	if err != nil {
		return nil, err
	}
	// QueryHash hides the raw text from the audit pipeline; the search
	// input is hashed to keep correlation possible without leaking.
	if emitErr := d.bus.Publish(ctx, events.Event{
		Type:       skills.EventTypeSkillSearchExecuted,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: skills.SkillSearchExecutedPayload{
			QueryHash: hashQuery(query),
			Path:      path,
			Limit:     limit,
			ResultN:   len(results),
		},
	}); emitErr != nil {
		return nil, fmt.Errorf("skills/postgres: emit skill.search_executed: %w", emitErr)
	}
	return results, nil
}

// search executes the three-tier ranking ladder, matching the SQLite
// driver's ordering:
//
//  1. Full-text — strict AND first; if zero rows, OR-of-tokens
//     fallback. Score = ts_rank -> min-max normalised 0..1.
//  2. Regex — try compiling the query as-is; for multi-token queries
//     compile an OR-of-tokens regex. Scoring constants: name
//     fullmatch=0.95 / name match=0.90 / name search=0.85 / body
//     search=0.75.
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

	results, err := d.searchFTS(ctx, id, query, limit)
	if err != nil {
		return nil, "", err
	}
	if len(results) > 0 {
		return results, skills.PathFTS5, nil
	}

	results, err = d.searchRegex(ctx, id, query, limit)
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

// ftsTokenRE captures the alphanumeric tokens the search design uses
// for full-text query construction. Punctuation / quoting is stripped
// so the tsquery parser never sees user-controlled syntax.
var ftsTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// searchFTS runs the Postgres full-text path. Strict-AND first, then
// OR fallback. The tsquery is built from tokenised input only —
// caller bytes never reach the parser uncontrolled (the tokens are
// alphanumeric-only and still flow in as a bound parameter). Scoring
// mirrors the SQLite driver: raw relevance normalised to 0..1 with a
// single hit landing at 1.0.
func (d *driver) searchFTS(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	tokens := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	if len(tokens) == 0 {
		return nil, nil
	}

	hits, err := d.runFTS(ctx, id, strings.Join(tokens, " & "), limit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && len(tokens) > 1 {
		hits, err = d.runFTS(ctx, id, strings.Join(tokens, " | "), limit)
		if err != nil {
			return nil, err
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Min-max normalise the raw ts_rank into 0..1. ts_rank returns
	// LARGER values for stronger matches, so it is normalised
	// directly (the SQLite driver inverts bm25 first because bm25 is
	// smaller-is-better; both land on the same 0..1 scale with the
	// best match at 1.0 and a single row at 1.0).
	minRaw, maxRaw := hits[0].raw, hits[0].raw
	for _, h := range hits[1:] {
		if h.raw < minRaw {
			minRaw = h.raw
		}
		if h.raw > maxRaw {
			maxRaw = h.raw
		}
	}
	out := make([]skills.RankedSkill, 0, len(hits))
	for _, h := range hits {
		var score float64
		if maxRaw == minRaw {
			score = 1.0
		} else {
			score = (h.raw - minRaw) / (maxRaw - minRaw)
		}
		out = append(out, skills.RankedSkill{Skill: h.skill, Score: score, Path: skills.PathFTS5})
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

// ftsHit pairs a matched skill with its raw ts_rank.
type ftsHit struct {
	skill skills.Skill
	raw   float64
}

// runFTS executes one tsquery against the identity-scoped rows. The
// query expression is a bound parameter, never concatenated into the
// SQL text.
func (d *driver) runFTS(ctx context.Context, id identity.Quadruple, tsqueryExpr string, limit int) ([]ftsHit, error) {
	// skillCols is a compile-time column constant; the tsquery +
	// identity + limit all flow in as bound $N params.
	sel := `SELECT ` + skillCols + `, ts_rank(search_tsv, q) AS rank
        FROM skills, to_tsquery('english', $1) q
        WHERE search_tsv @@ q
          AND tenant_id = $2 AND user_id = $3 AND (session_id = $4 OR scope = $5)
        ORDER BY rank DESC, updated_at DESC, name ASC
        LIMIT $6`
	rows, err := d.db.QueryContext(ctx, sel,
		tsqueryExpr, id.TenantID, id.UserID, id.SessionID, string(skills.ScopeUser), limit)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: fts query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []ftsHit
	for rows.Next() {
		s, raw, err := scanSkillWithRank(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: fts scan: %w", err)
		}
		hits = append(hits, ftsHit{skill: s, raw: raw})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/postgres: fts iterate: %w", err)
	}
	return hits, nil
}

// scanSkillWithRank scans the skillCols columns plus a trailing
// ts_rank float.
func scanSkillWithRank(rows scannable) (skills.Skill, float64, error) {
	var (
		s         skills.Skill
		tagsJSON  string
		stepsJSON string
		preJSON   string
		failJSON  string
		rtJSON    string
		rnsJSON   string
		rtgJSON   string
		origin    string
		scope     string
		extraJSON string
		rank      float64
	)
	if err := rows.Scan(
		&s.Name, &s.Title, &s.Description, &s.Trigger, &s.TaskType,
		&tagsJSON, &stepsJSON, &preJSON, &failJSON,
		&rtJSON, &rnsJSON, &rtgJSON,
		&origin, &s.OriginRef, &scope, &s.ScopeTenantID, &s.ScopeProjectID,
		&s.ContentHash, &s.CreatedAt, &s.UpdatedAt, &s.LastUsed, &s.UseCount, &extraJSON,
		&rank,
	); err != nil {
		return skills.Skill{}, 0, err
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
	s.CreatedAt = s.CreatedAt.UTC()
	s.UpdatedAt = s.UpdatedAt.UTC()
	s.LastUsed = s.LastUsed.UTC()
	return s, rank, nil
}

// searchRegex runs the regex fallback with the SQLite driver's scoring
// constants (name fullmatch=0.95 / name match=0.90 / name search=0.85
// / body search=0.75).
func (d *driver) searchRegex(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	re, err := buildRegex(query)
	if err != nil {
		return nil, nil // unparseable regex → empty, ladder falls through to exact
	}

	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant_id = $1 AND user_id = $2 AND (session_id = $3 OR scope = $4)
        ORDER BY updated_at DESC, name ASC`,
		id.TenantID, id.UserID, id.SessionID, string(skills.ScopeUser))
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: regex query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []skills.RankedSkill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: regex scan: %w", err)
		}
		score := regexScore(re, s)
		if score == 0 {
			continue
		}
		out = append(out, skills.RankedSkill{Skill: s, Score: score, Path: skills.PathRegex})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/postgres: regex iterate: %w", err)
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

// buildRegex compiles `query` as a regex; if compilation fails (or the
// query has whitespace, signalling an NL query), falls back to
// `(?i)(tok1|tok2|...)`. Byte-identical to the SQLite driver.
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

// regexScore applies the per-field scoring constants. Returns the
// maximum applicable score for `s` against `re`, or 0 if no field
// matched. Byte-identical to the SQLite driver.
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

// searchExact runs the lowercase-equality fallback. Score=1.0 on every
// row that matches. Mirrors the SQLite driver.
func (d *driver) searchExact(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant_id = $1 AND user_id = $2 AND (session_id = $3 OR scope = $4)
          AND (
              lower(name) = $5
              OR lower(title) = $5
              OR lower(trigger_text) = $5
              OR lower(tags_text) LIKE $6
          )
        ORDER BY updated_at DESC, name ASC
        LIMIT $7`,
		id.TenantID, id.UserID, id.SessionID, string(skills.ScopeUser),
		q, "%"+q+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: exact query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []skills.RankedSkill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: exact scan: %w", err)
		}
		out = append(out, skills.RankedSkill{Skill: s, Score: 1.0, Path: skills.PathExact})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/postgres: exact iterate: %w", err)
	}
	return out, nil
}

// searchSemantic ranks the identity-scoped catalog by embedding
// similarity to `query`. One batched `Embed` call carries the query
// plus every candidate's searchable text; results are ranked by cosine
// and mapped onto the 0–1 score scale the other paths use
// (`(cosine+1)/2`). Mirrors the SQLite driver.
func (d *driver) searchSemantic(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant_id = $1 AND user_id = $2 AND (session_id = $3 OR scope = $4)
        ORDER BY updated_at DESC, name ASC
        LIMIT $5`,
		id.TenantID, id.UserID, id.SessionID, string(skills.ScopeUser), semanticCandidateCap)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: semantic candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []skills.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: semantic scan: %w", err)
		}
		candidates = append(candidates, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/postgres: semantic iterate: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	texts := make([]string, 0, len(candidates)+1)
	texts = append(texts, query)
	for _, s := range candidates {
		texts = append(texts, skillText(s))
	}

	ectx, err := embedIdentityCtx(ctx, id)
	if err != nil {
		return nil, err
	}
	vecs, err := d.embedder.Embed(ectx, texts)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: semantic embed: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("skills/postgres: embedder returned %d vectors for %d texts", len(vecs), len(texts))
	}

	qv := vecs[0]
	out := make([]skills.RankedSkill, 0, len(candidates))
	for i, s := range candidates {
		cos, cerr := embeddings.Cosine(qv, vecs[i+1])
		if cerr != nil {
			return nil, fmt.Errorf("skills/postgres: semantic rank %q: %w", s.Name, cerr)
		}
		out = append(out, skills.RankedSkill{
			Skill: s,
			Score: (cos + 1) / 2, // map cosine [-1,1] onto the canonical 0–1 score scale
			Path:  skills.PathSemantic,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// skillText is the embedded representation of a skill: the planner-
// facing match surface (name, title, trigger, description, tags) — the
// same fields the lexical ladder indexes. Mirrors the SQLite driver.
func skillText(s skills.Skill) string {
	parts := make([]string, 0, 5)
	for _, p := range []string{s.Name, s.Title, s.Trigger, s.Description, strings.Join(s.Tags, " ")} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return s.Name
	}
	return strings.Join(parts, "\n")
}

// embedIdentityCtx stamps the store-validated identity onto the context
// the embedder sees so the billable embedding traffic is attributed to
// the identity the rows are scoped under. Mirrors the SQLite driver.
func embedIdentityCtx(ctx context.Context, id identity.Quadruple) (context.Context, error) {
	if id.RunID != "" {
		ectx, err := identity.WithRun(ctx, id.Identity, id.RunID)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: identity: %w", err)
		}
		return ectx, nil
	}
	ectx, err := identity.With(ctx, id.Identity)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: identity: %w", err)
	}
	return ectx, nil
}

// hashQuery returns the canonical query-hash for audit emission. First
// 16 hex chars of sha256(lowercased trimmed query). Mirrors the SQLite
// driver.
func hashQuery(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])[:queryHashHexChars]
}
