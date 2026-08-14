package sqlite

import (
	"strings"
	"testing"
)

// This file pins the paging-SQL HYGIENE invariants at the statement
// level (white-box, so it reads the driver's own SQL constants): the
// stable tail/keyset/snapshot paging requirement is "without OFFSET /
// history scans", and a future edit that sneaks an OFFSET-based page
// into the read path fails here before it can silently degrade the
// driver.
func TestPagingSQL_NoOffsetInReadPath(t *testing.T) {
	for name, sql := range map[string]string{
		"listPageSQL":       listPageSQL,
		"listPageNewestSQL": listPageNewestSQL,
		"countOlderSQL":     countOlderSQL,
		"boundarySeqSQL":    boundarySeqSQL,
		"getRowSQL":         getRowSQL,
		"guardRowSQL":       guardRowSQL,
	} {
		if strings.Contains(strings.ToUpper(sql), " OFFSET ") {
			t.Errorf("%s must page by keyset, never OFFSET:\n%s", name, sql)
		}
	}
}

func TestPagingSQL_PageQueriesBoundWithLimit(t *testing.T) {
	for name, sql := range map[string]string{
		"listPageSQL":       listPageSQL,
		"listPageNewestSQL": listPageNewestSQL,
	} {
		if !strings.Contains(strings.ToUpper(sql), " LIMIT ") {
			t.Errorf("%s must bound the page with LIMIT:\n%s", name, sql)
		}
	}
}

func TestPagingSQL_ScopeIsExactIdentityPrefix(t *testing.T) {
	// Every read filters by the EXACT identity triple (tenant, user,
	// session) — a page can never widen into another session's rows.
	for name, sql := range map[string]string{
		"listPageSQL":       listPageSQL,
		"listPageNewestSQL": listPageNewestSQL,
		"countOlderSQL":     countOlderSQL,
		"boundarySeqSQL":    boundarySeqSQL,
		"getRowSQL":         getRowSQL,
		"guardRowSQL":       guardRowSQL,
	} {
		if !strings.Contains(sql, "tenant = ?") || !strings.Contains(sql, "user = ?") || !strings.Contains(sql, "session = ?") {
			t.Errorf("%s must filter by the exact identity triple:\n%s", name, sql)
		}
		if strings.Contains(strings.ToUpper(sql), " LIKE ") || strings.Contains(sql, "SUBSTR(") {
			t.Errorf("%s must not use wildcard / prefix matching on identity:\n%s", name, sql)
		}
	}
}

func TestWriteSQL_ChildrenReplacedWholesale(t *testing.T) {
	// The dynamic bounded collections are replaced wholesale on every
	// accepted write — a stale child can never survive a row rewrite.
	if !strings.Contains(deleteActivityRowsSQL, "turn_id = ?") {
		t.Errorf("deleteActivityRowsSQL must scope by turn_id:\n%s", deleteActivityRowsSQL)
	}
	if !strings.Contains(deleteAppsRowsSQL, "turn_id = ?") {
		t.Errorf("deleteAppsRowsSQL must scope by turn_id:\n%s", deleteAppsRowsSQL)
	}
}
