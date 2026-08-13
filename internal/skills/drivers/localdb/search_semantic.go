package localdb

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// semanticCandidateCap bounds how many identity-scoped rows one
// semantic search embeds. Skill catalogs are small at this scale;
// brute-force cosine over the candidate window is the deliberate
// design — a persistent skill-vector index is a later concern if
// catalog cardinality ever demands it. Candidates are the most
// recently updated rows, mirroring the List ordering.
const semanticCandidateCap = 256

// searchSemantic ranks the identity-scoped catalog by embedding
// similarity to `query`. One batched `Embed` call carries the query
// plus every candidate's searchable text; results are ranked by
// cosine and mapped onto the 0–1 score scale the other paths use
// (`(cosine+1)/2`).
//
// Identity scoping rides the SQL `WHERE` (never post-filtering),
// and the embedder sees a ctx stamped with the store-validated
// identity so the billable embedding traffic is attributed to the
// identity the rows are scoped under.
func (d *driver) searchSemantic(ctx context.Context, id identity.Quadruple, agentID, query string, limit int) ([]skills.RankedSkill, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	rows, err := d.db.QueryContext(ctx, selectSkillsSQL+`
        WHERE tenant = ? AND user = ? AND (session = ? OR scope = ?) AND agent_id = ?
        ORDER BY updated_at DESC, name ASC
        LIMIT ?`,
		id.TenantID, id.UserID, id.SessionID, string(skills.ScopeUser), agentID, semanticCandidateCap)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: semantic candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []skills.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: semantic scan: %w", err)
		}
		candidates = append(candidates, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: semantic iterate: %w", err)
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
		return nil, fmt.Errorf("skills/localdb: semantic embed: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("skills/localdb: embedder returned %d vectors for %d texts", len(vecs), len(texts))
	}

	qv := vecs[0]
	out := make([]skills.RankedSkill, 0, len(candidates))
	for i, s := range candidates {
		cos, cerr := embeddings.Cosine(qv, vecs[i+1])
		if cerr != nil {
			return nil, fmt.Errorf("skills/localdb: semantic rank %q: %w", s.Name, cerr)
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

// skillText is the embedded representation of a skill: the
// planner-facing match surface (name, title, trigger, description,
// tags) — the same fields the lexical ladder indexes.
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

// embedIdentityCtx stamps the store-validated identity onto the
// context the embedder sees, mirroring the memory subsystem's
// bridge: the skills surface carries identity as an explicit
// argument, the embedding edge is identity-in-ctx (fail-closed).
func embedIdentityCtx(ctx context.Context, id identity.Quadruple) (context.Context, error) {
	// A record from another tenant reaches here only behind a listing the
	// caller was already authorized for; seat that as an audited crossing
	// so the attribution names the tenant it reads and why.
	if verified, ok := identity.FromVerified(ctx); ok && id.TenantID != verified.TenantID {
		ectx, err := identity.WithElevated(ctx, id.Identity,
			"skills semantic search: attributing an embedding call to the identity the stored skill is scoped under")
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: identity: %w", err)
		}
		if id.RunID == "" {
			return ectx, nil
		}
		ectx, err = identity.WithRun(ectx, id.Identity, id.RunID)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: identity: %w", err)
		}
		return ectx, nil
	}
	if id.RunID != "" {
		ectx, err := identity.WithRun(ctx, id.Identity, id.RunID)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: identity: %w", err)
		}
		return ectx, nil
	}
	ectx, err := identity.With(ctx, id.Identity)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: identity: %w", err)
	}
	return ectx, nil
}
