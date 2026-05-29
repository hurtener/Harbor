package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// augmentDSN layers WAL + busy_timeout pragmas onto the DSN.
//
// modernc.org/sqlite reads repeated `_pragma=` query parameters as a
// sequence of `PRAGMA` statements run at connection open. Multi-pragma
// support requires multiple distinct `_pragma=` parameters in the URL,
// not a single value with `&_pragma=` embedded inside it — the URL
// encoder would escape the embedded `&` and the driver would see one
// pragma with a malformed value. We construct the DSN with `Add` so
// each pragma is its own param.
func augmentDSN(raw string) (string, error) {
	if raw == ":memory:" {
		return fmt.Sprintf(
			":memory:?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)",
			busyTimeoutMs), nil
	}
	if strings.HasPrefix(raw, "file:") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parse URI DSN: %w", err)
		}
		q := u.Query()
		hasJournal := false
		hasBusy := false
		for _, p := range q["_pragma"] {
			if strings.HasPrefix(p, "journal_mode") {
				hasJournal = true
			}
			if strings.HasPrefix(p, "busy_timeout") {
				hasBusy = true
			}
		}
		if !hasJournal {
			q.Add("_pragma", "journal_mode(WAL)")
		}
		if !hasBusy {
			q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs))
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return fmt.Sprintf(
		"%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)",
		raw, busyTimeoutMs), nil
}

func detectFTS5(ctx context.Context, db *sql.DB) bool {
	_, err := db.ExecContext(ctx, `SELECT count(*) FROM tool_cache_fts`)
	return err == nil
}

func marshalStrings(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ss) //nolint:errcheck // []string is always serialisable; a Marshal failure is unreachable
	return string(b)
}

func unmarshalStrings(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func buildANDExpr(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = `"` + t + `"`
	}
	return strings.Join(parts, " AND ")
}

func buildORExpr(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = `"` + t + `"`
	}
	return strings.Join(parts, " OR ")
}
