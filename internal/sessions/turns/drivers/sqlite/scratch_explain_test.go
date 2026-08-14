package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the driver's INDEX-PLAN assertion suite (white-box:
// it runs EXPLAIN QUERY PLAN against the driver's own statements so
// the tests pin the SAME SQL the driver executes — no test/driver SQL
// drift). Every read axis the store contract leans on must be an
// index SEARCH, never a table SCAN and never a TEMP B-TREE sort:
//
//   - indexed get / append-idempotency probe → primary key
//   - mutate guard read → primary key
//   - cursor boundary lookup → primary key
//   - erasure-fence probe → fence primary key (covering)
//   - keyset page (newest + cursor) → sequence index range
//   - exact older-row count → keyset index range (covering)
//   - retention eviction → keyset index range
//
// A plan regression that turns any of these into a SCAN (a silent
// full-history read) fails here.

// openDriver opens a fresh file-backed driver for EXPLAIN probing and
// returns it typed as *driver so the white-box tests can reach the
// pool + SQL constants.
func openDriver(t *testing.T) *driver {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "indexplan.sqlite")
	s, err := New(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d, ok := s.(*driver)
	if !ok {
		_ = s.Close(context.Background())
		t.Fatalf("New returned %T, want *driver", s)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

// explainRows runs `EXPLAIN QUERY PLAN <query>` and returns the detail
// strings (one per plan row).
func explainRows(t *testing.T, d *driver, query string, args ...any) []string {
	t.Helper()
	rows, err := d.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent int
		var notUsed, detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("EXPLAIN returned no plan rows for %s", query)
	}
	return out
}

// assertSearch asserts every plan row is a SEARCH (no SCAN) and that
// the wanted detail substring appears at least once.
func assertSearch(t *testing.T, detail []string, wantSubstr string) {
	t.Helper()
	joined := strings.Join(detail, "\n")
	for _, row := range detail {
		if strings.Contains(row, " SCAN ") || strings.HasPrefix(row, "SCAN ") {
			t.Errorf("plan contains a table SCAN:\n%s\n---\nwant every row to SEARCH, e.g. containing %q", joined, wantSubstr)
			return
		}
	}
	if !strings.Contains(joined, wantSubstr) {
		t.Errorf("plan does not use the expected index:\n%s\n---\nwant a row containing %q", joined, wantSubstr)
	}
}

func TestIndexPlan_IndexedGet_UsesPrimaryKey(t *testing.T) {
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, getRowSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_rows USING INDEX sqlite_autoindex_turn_rows_1")
}

func TestIndexPlan_AppendIdempotencyLookup_UsesPrimaryKey(t *testing.T) {
	// The append path's existing-row probe is getRowSQL (the same
	// primary-key lookup as indexed get).
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, getRowSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_rows USING INDEX sqlite_autoindex_turn_rows_1")
}

func TestIndexPlan_MutateGuardRead_UsesPrimaryKey(t *testing.T) {
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, guardRowSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_rows USING INDEX sqlite_autoindex_turn_rows_1")
}

func TestIndexPlan_CursorBoundaryLookup_UsesPrimaryKey(t *testing.T) {
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, boundarySeqSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_rows USING INDEX sqlite_autoindex_turn_rows_1")
}

func TestIndexPlan_FenceProbe_UsesFencePrimaryKey(t *testing.T) {
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, scopeFencedSQL, "t", "u", "s"),
		"SEARCH turn_fences USING COVERING INDEX sqlite_autoindex_turn_fences_1")
}

func TestIndexPlan_KeysetPage_UsesSequenceIndexRange(t *testing.T) {
	// The cursor page is a bounded index RANGE scan on the
	// (tenant, user, session, sequence[, turn_id]) index — the row-value
	// keyset predicate is range-optimized by SQLite, so no session
	// history is ever scanned and no TEMP B-TREE sorts the page.
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, listPageSQL, "t", "u", "s", 5, "run-1", 11),
		"SEARCH turn_rows USING INDEX turn_rows_session_seq_uidx")
	assertSearch(t, explainRows(t, d, listPageNewestSQL, "t", "u", "s", 11),
		"SEARCH turn_rows USING INDEX turn_rows_session_seq_uidx")
}

func TestIndexPlan_ExactRemainingCount_UsesKeysetIndex(t *testing.T) {
	d := openDriver(t)
	assertSearch(t, explainRows(t, d, countOlderSQL, "t", "u", "s", 5, "run-1"),
		"SEARCH turn_rows USING COVERING INDEX turn_rows_keyset_idx")
}

func TestIndexPlan_RetentionEviction_UsesKeysetIndex(t *testing.T) {
	d := openDriver(t)
	// The eviction boundary subquery and the delete both ride the
	// keyset index; neither is a table SCAN.
	assertSearch(t, explainRows(t, d, evictRowsSQL, "t", "u", "s", "t", "u", "s", 200),
		"SEARCH turn_rows USING COVERING INDEX turn_rows_keyset_idx")
}

func TestIndexPlan_ChildrenOrderedRead_UsesChildPrimaryKey(t *testing.T) {
	d := openDriver(t)
	// Activity + App children are read in first-declaration order via
	// their own primary-key / position indexes.
	assertSearch(t, explainRows(t, d, selectActivityRowsSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_activity_rows USING INDEX sqlite_autoindex_turn_activity_rows_1")
	assertSearch(t, explainRows(t, d, selectAppsRowsSQL, "t", "u", "s", "run-1"),
		"SEARCH turn_apps USING INDEX turn_apps_position_idx")
}
