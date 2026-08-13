package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// TestQuery_IndexResolvedPlan pins the indexed-query contract at the SQL
// layer: a bounded query resolves its candidates through the bucket +
// dimension indexes — the bucket_start window range plus exact IN
// filters — never a full-table scan of the projection rows and never the
// canonical event log. `EXPLAIN QUERY PLAN` is the honest evidence: each
// representative read shows a SEARCH through an index, and no statement
// shows a SCAN of `rollup_rows`.
func TestQuery_IndexResolvedPlan(t *testing.T) {
	s := newTestStore(t, ":memory:")
	ctx := context.Background()
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	// 200 minute buckets × 40 rows (4 tenants × 10 users) = 8_000 rows.
	// Enough data that SQLite's planner prefers the indexes, and a mix of
	// tenants/users/models so every axis has real selectivity.
	var deltas []rollups.Delta
	var seq uint64
	for b := range 200 {
		bstart := base.Add(time.Duration(b) * time.Minute)
		for i := range 40 {
			seq++
			deltas = append(deltas, rollups.Delta{
				Key: rollups.Key{
					BucketStart: bstart,
					TenantID:    fmt.Sprintf("tenant-%02d", i%4),
					UserID:      fmt.Sprintf("user-%03d", i%10),
					SessionID:   fmt.Sprintf("session-%03d", i),
					Model:       fmt.Sprintf("model-%c", 'a'+rune(i%3)),
				},
				Add: rollups.MeasureSet{LLMCompletions: 1},
			})
		}
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: seq, Deltas: deltas}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	from := base.Add(50 * time.Minute)
	to := base.Add(55 * time.Minute)
	window := []any{from.UnixNano(), to.UnixNano()}

	cases := []struct {
		name string
		stmt string
		args []any
	}{
		{"window-only", `SELECT bucket_start FROM rollup_rows WHERE bucket_start >= ? AND bucket_start < ?`, window},
		{"window+tenant", `SELECT bucket_start FROM rollup_rows WHERE bucket_start >= ? AND bucket_start < ? AND tenant_id = ?`,
			append(window, "tenant-00")},
		{"window+tenant+user", `SELECT bucket_start FROM rollup_rows WHERE bucket_start >= ? AND bucket_start < ? AND tenant_id = ? AND user_id = ?`,
			append(window, "tenant-00", "user-000")},
		{"user-only", `SELECT bucket_start FROM rollup_rows WHERE user_id = ? AND bucket_start >= ? AND bucket_start < ?`,
			[]any{"user-003", from.UnixNano(), to.UnixNano()}},
		{"session-only", `SELECT bucket_start FROM rollup_rows WHERE session_id = ? AND bucket_start >= ? AND bucket_start < ?`,
			[]any{"session-005", from.UnixNano(), to.UnixNano()}},
		{"model-only", `SELECT bucket_start FROM rollup_rows WHERE model = ? AND bucket_start >= ? AND bucket_start < ?`,
			[]any{"model-a", from.UnixNano(), to.UnixNano()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s, tc.stmt, tc.args...)
			if strings.Contains(plan, "SCAN rollup_rows") {
				t.Fatalf("query plan raw-scans rollup_rows:\n%s", plan)
			}
			if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "USING COVERING INDEX") {
				t.Fatalf("query plan does not resolve through an index:\n%s", plan)
			}
		})
	}

	// The erasure-fence delete must also resolve through the identity
	// triple index — a session-scoped erasure is an index range, not a
	// table scan.
	t.Run("fence delete", func(t *testing.T) {
		plan := explainQueryPlan(t, s,
			`DELETE FROM rollup_rows WHERE tenant_id = ? AND user_id = ? AND session_id = ?`,
			"tenant-00", "user-000", "session-000")
		if strings.Contains(plan, "SCAN rollup_rows") {
			t.Fatalf("fence delete raw-scans rollup_rows:\n%s", plan)
		}
		if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "USING COVERING INDEX") {
			t.Fatalf("fence delete does not resolve through an index:\n%s", plan)
		}
	})
}

// TestMigrations_AppliedOnceIdempotent pins the migration contract: the
// embedded schema applies on a clean database, and re-opening the same
// file is a no-op (the shared runner gates on schema_migrations). A
// second open must not error and must not reset the checkpoint.
func TestMigrations_AppliedOnceIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "migrated.sqlite")

	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 7}); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open the same file: the migration must not re-run (no error, no
	// schema reset), and the checkpoint must survive.
	s2, err := New(dsn)
	if err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()
	ck, err := s2.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint (reopen): %v", err)
	}
	if ck != 7 {
		t.Fatalf("checkpoint after reopen = %d; want 7", ck)
	}
}

// --- helpers -------------------------------------------------------------

// newTestStore opens a Store for the test and registers its Close.
func newTestStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// explainQueryPlan runs `EXPLAIN QUERY PLAN` on stmt with args and
// returns the concatenated detail column.
func explainQueryPlan(t *testing.T, s *Store, stmt string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	return strings.Join(details, "\n")
}
