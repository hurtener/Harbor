package searchcache

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/tools"
)

var ftsTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// bm25 column weights, in tool_cache_fts column order: name, description,
// tags, server_title, server_description. A hit on the tool's own name or
// its server's title outranks a passing mention in a description.
const ftsRankExpr = `bm25(tool_cache_fts, 4.0, 1.0, 1.0, 3.0, 0.5)`

// keyColumns are all a ranked or browse query reads: the name (the result
// identity the caller materializes from its own authoritative view) and the
// tags (the tag filter). Descriptions and schemas are never decoded for the
// process-wide match set.
const keyColumns = `c.name, c.tags`

// textColumns add the indexed text the substring fallback matches against.
const textColumns = `c.name, c.tags, c.description, c.server_title, c.server_description`

func (d *driver) searchFTS5(ctx context.Context, query string, limit int, admit func(tools.Tool) bool) ([]tools.Tool, error) {
	tokens := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	if len(tokens) == 0 {
		return nil, nil
	}
	hits, err := d.rankedFTS(ctx, buildANDExpr(tokens), limit, admit)
	if err != nil || len(hits) > 0 || len(tokens) == 1 {
		return hits, err
	}
	return d.rankedFTS(ctx, buildORExpr(tokens), limit, admit)
}

// rankedFTS streams matches in relevance order and keeps the first limit
// admitted rows. The visibility predicate runs before the limit, so foreign
// rows never consume result slots; memory stays bounded by limit.
func (d *driver) rankedFTS(ctx context.Context, matchExpr string, limit int, admit func(tools.Tool) bool) ([]tools.Tool, error) {
	const sel = `SELECT ` + keyColumns + `
        FROM tool_cache_fts JOIN tool_cache c ON c.rowid = tool_cache_fts.rowid
        WHERE tool_cache_fts MATCH ?
        ORDER BY ` + ftsRankExpr + `, c.name`
	rows, err := d.db.QueryContext(ctx, sel, matchExpr)
	if err != nil {
		return nil, fmt.Errorf("searchcache: fts5 query: %w", err)
	}
	defer rows.Close()
	return collect(rows, limit, false, nil, admit)
}

// searchSubstring is the fallback when FTS5 is unavailable or yields no
// admitted row: a case-insensitive substring match over every indexed field.
func (d *driver) searchSubstring(ctx context.Context, query string, limit int, admit func(tools.Tool) bool) ([]tools.Tool, error) {
	needle := strings.ToLower(query)
	rows, err := d.db.QueryContext(ctx, `SELECT `+textColumns+` FROM tool_cache c ORDER BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("searchcache: substring scan: %w", err)
	}
	defer rows.Close()
	return collect(rows, limit, true, func(t tools.Tool) bool { return matchesSubstring(t, needle) }, admit)
}

// scan returns admitted rows in name order (an empty query: tag-only or
// browse).
func (d *driver) scan(ctx context.Context, limit int, admit func(tools.Tool) bool) ([]tools.Tool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM tool_cache c ORDER BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("searchcache: scan: %w", err)
	}
	defer rows.Close()
	return collect(rows, limit, false, nil, admit)
}

func matchesSubstring(t tools.Tool, needle string) bool {
	for _, field := range []string{t.Name, t.Description, strings.Join(t.Tags, " "), t.SourceTitle, t.SourceDescription} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// collect streams rows, keeping the first limit that match and are
// admitted. Returned Tools carry only Name and Tags: callers materialize
// everything else from their own authoritative source. withText selects the
// textColumns layout (used for matching only, then dropped).
func collect(rows *sql.Rows, limit int, withText bool, match func(tools.Tool) bool, admit func(tools.Tool) bool) ([]tools.Tool, error) {
	var out []tools.Tool
	for rows.Next() {
		var t tools.Tool
		var tagsJSON string
		if withText {
			if err := rows.Scan(&t.Name, &tagsJSON, &t.Description, &t.SourceTitle, &t.SourceDescription); err != nil {
				return nil, err
			}
		} else if err := rows.Scan(&t.Name, &tagsJSON); err != nil {
			return nil, err
		}
		t.Tags = unmarshalStrings(tagsJSON)
		if match != nil && !match(t) {
			continue
		}
		if !admit(t) {
			continue
		}
		out = append(out, tools.Tool{Name: t.Name, Tags: t.Tags})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func matchesTagsFilter(toolTags []string, filterTags []string) bool {
	if len(filterTags) == 0 {
		return true
	}
	set := make(map[string]bool, len(toolTags))
	for _, t := range toolTags {
		set[strings.ToLower(t)] = true
	}
	for _, ft := range filterTags {
		if !set[strings.ToLower(ft)] {
			return false
		}
	}
	return true
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
