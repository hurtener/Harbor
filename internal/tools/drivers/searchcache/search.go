package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hurtener/Harbor/internal/tools"
)

var ftsTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

func (d *driver) searchFTS5(ctx context.Context, query string, filterTags []string, limit int) ([]tools.Tool, error) {
	tokens := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	if len(tokens) == 0 {
		return nil, nil
	}

	hits, err := d.tryFTSQuery(ctx, buildANDExpr(tokens), limit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && len(tokens) > 1 {
		hits, err = d.tryFTSQuery(ctx, buildORExpr(tokens), limit)
		if err != nil {
			return nil, err
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}
	return d.materializeRows(ctx, hits, filterTags)
}

func (d *driver) tryFTSQuery(ctx context.Context, matchExpr string, limit int) ([]int64, error) {
	const sel = `SELECT rowid FROM tool_cache_fts WHERE tool_cache_fts MATCH ? LIMIT ?`
	rows, err := d.db.QueryContext(ctx, sel, matchExpr, limit)
	if err != nil {
		return nil, fmt.Errorf("searchcache: fts5 query: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, err
		}
		ids = append(ids, rid)
	}
	return ids, rows.Err()
}

func (d *driver) materializeRows(ctx context.Context, rowIDs []int64, filterTags []string) ([]tools.Tool, error) {
	ids := make([]any, len(rowIDs))
	placeholders := make([]string, len(rowIDs))
	for i, rid := range rowIDs {
		ids[i] = rid
		placeholders[i] = "?"
	}
	// gosec G202: the `placeholders` slice is constructed from a fixed
	// `?` token per rowID; the row IDs themselves are bound via
	// parameter substitution (ids...). There is no caller-controlled
	// string ever concatenated into the SQL.
	sel := `SELECT name, description, tags, args_schema FROM tool_cache WHERE rowid IN (` + strings.Join(placeholders, ",") + `)` //nolint:gosec // see comment above; placeholders are literal `?` tokens, values bind through ids...
	rows, err := d.db.QueryContext(ctx, sel, ids...)
	if err != nil {
		return nil, fmt.Errorf("searchcache: materialize: %w", err)
	}
	defer rows.Close()
	return scanTools(rows, filterTags)
}

func (d *driver) searchRegex(ctx context.Context, query string, filterTags []string, limit int) ([]tools.Tool, error) {
	re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, `SELECT name, description, tags, args_schema FROM tool_cache ORDER BY updated_at DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("searchcache: regex scan: %w", err)
	}
	defer rows.Close()

	toolList, err := scanTools(rows, filterTags)
	if err != nil {
		return nil, err
	}
	var out []tools.Tool
	for _, t := range toolList {
		if re.MatchString(t.Name) || re.MatchString(t.Description) || re.MatchString(strings.Join(t.Tags, " ")) {
			out = append(out, t)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (d *driver) searchExact(ctx context.Context, query string, filterTags []string, limit int) ([]tools.Tool, error) {
	lower := strings.ToLower(query)
	rows, err := d.db.QueryContext(ctx, `SELECT name, description, tags, args_schema FROM tool_cache WHERE LOWER(name) = ? LIMIT ?`, lower, limit)
	if err != nil {
		return nil, fmt.Errorf("searchcache: exact: %w", err)
	}
	defer rows.Close()
	exact, err := scanTools(rows, filterTags)
	if err != nil {
		return nil, err
	}
	if len(exact) > 0 {
		return exact, nil
	}
	// Fallthrough: scan all
	rows2, err := d.db.QueryContext(ctx, `SELECT name, description, tags, args_schema FROM tool_cache`)
	if err != nil {
		return nil, fmt.Errorf("searchcache: exact fallback: %w", err)
	}
	defer rows2.Close()
	all, err := scanTools(rows2, filterTags)
	if err != nil {
		return nil, err
	}
	var out []tools.Tool
	for _, t := range all {
		if strings.Contains(strings.ToLower(t.Name), lower) ||
			strings.Contains(strings.ToLower(t.Description), lower) ||
			strings.Contains(strings.ToLower(strings.Join(t.Tags, " ")), lower) {
			out = append(out, t)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (d *driver) listAll(ctx context.Context, limit int) ([]tools.Tool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT name, description, tags, args_schema FROM tool_cache ORDER BY updated_at DESC, name ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTools(rows, nil)
}

func scanTools(rows *sql.Rows, filterTags []string) ([]tools.Tool, error) {
	var out []tools.Tool
	for rows.Next() {
		var name, desc, tagsJSON, schemaJSON string
		if err := rows.Scan(&name, &desc, &tagsJSON, &schemaJSON); err != nil {
			return nil, err
		}
		tags := unmarshalStrings(tagsJSON)
		if len(filterTags) > 0 && !matchesTagsFilter(tags, filterTags) {
			continue
		}
		out = append(out, tools.Tool{
			Name:        name,
			Description: desc,
			Tags:        tags,
			ArgsSchema:  json.RawMessage(schemaJSON),
		})
	}
	return out, rows.Err()
}

func matchesTagsFilter(toolTags []string, filterTags []string) bool {
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
