package postgres

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// TestQuery_Explain_IndexedAccess pins the bounded indexed-query contract
// at the SQL level (the memstore parity pin): the driver's own query
// statement resolves the window + dimension filters through the rollup
// indexes — the planner must choose an index scan, never a full-table scan
// — and the mandatory window predicate is always present (a Query is
// bounded by construction: Validate requires From/To and caps the window
// at MaxBuckets).
func TestQuery_Explain_IndexedAccess(t *testing.T) {
	dsn := requireDSN(t)
	dsn = freshSchema(t, dsn)
	ctx := context.Background()

	s, err := New(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()
	d := s.(*driver)

	// Seed 400 minute buckets × 100 rows (4 tenants × 25) = 40_000 rows:
	// big enough that the planner's stats make a Seq Scan visibly more
	// expensive than the index range scan for a narrow window.
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	var deltas []rollups.Delta
	var seq uint64
	for b := range 400 {
		bstart := base.Add(time.Duration(b) * time.Minute)
		for i := range 100 {
			seq++
			deltas = append(deltas, rollups.Delta{
				Key: rollups.Key{
					BucketStart: bstart,
					TenantID:    fmt.Sprintf("tenant-%02d", i%4),
					UserID:      fmt.Sprintf("u%03d", i),
					SessionID:   fmt.Sprintf("s%03d", i),
					Model:       "model-a",
				},
				Add: rollups.MeasureSet{LLMCompletions: 1},
			})
		}
	}
	// One ApplyBatch over all 40_000 deltas would emit a bind parameter
	// per row column — 20 per upserted row (5 key + 15 measure) and 5 per
	// key in the read pass — blowing past PostgreSQL's 65_535-parameter
	// ceiling before the EXPLAIN assertions ever run. Seed in deterministic
	// bounded batches instead: seedBatchMax keeps the binding upsert
	// statement at 20 × 2_000 = 40_000 parameters. The batches are disjoint
	// and carry strictly monotonic checkpoints (the cumulative delta count,
	// final = 40_000), so every delta is applied exactly once and the final
	// table cardinality and tenant/window distribution are identical to the
	// single-batch seed.
	const seedBatchMax = 2000
	for start := 0; start < len(deltas); start += seedBatchMax {
		end := min(start+seedBatchMax, len(deltas))
		ckpt := uint64(end)
		if err := d.ApplyBatch(ctx, rollups.Batch{Checkpoint: ckpt, Deltas: deltas[start:end]}); err != nil {
			t.Fatalf("seed ApplyBatch (checkpoint=%d, deltas=%d): %v", ckpt, end-start, err)
		}
	}
	if _, err := d.db.ExecContext(ctx, "ANALYZE rollup_rows"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// A narrow window + tenant filter: the exact shape the projector and
	// the Console read through.
	q := rollups.Query{
		From:     base.Add(100 * time.Minute),
		To:       base.Add(110 * time.Minute),
		Bucket:   rollups.BucketMinute,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-00"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	spec, err := buildQuery(q)
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}

	// The statement is bounded by construction: the window predicate is
	// mandatory and always parameterized.
	if !strings.Contains(spec.sql, "rollup_rows.bucket_start >= $1") ||
		!strings.Contains(spec.sql, "rollup_rows.bucket_start < $2") {
		t.Fatalf("query is missing the mandatory window predicate:\n%s", spec.sql)
	}

	// The plan must resolve through the index — a Seq Scan on rollup_rows
	// is the parity violation this pin exists to catch.
	plan := explain(t, d, spec.sql, spec.args)
	if strings.Contains(plan, "Seq Scan on rollup_rows") {
		t.Fatalf("query plan full-scans rollup_rows; want an index scan:\n%s", plan)
	}
	if !indexedAccess(plan) {
		t.Fatalf("query plan does not use an index on rollup_rows; want an index scan:\n%s", plan)
	}

	// The same query at hour granularity (coarsening) must still resolve
	// through the minute-grid index.
	q.Bucket = rollups.BucketHour
	spec2, err := buildQuery(q)
	if err != nil {
		t.Fatalf("buildQuery(hour): %v", err)
	}
	plan2 := explain(t, d, spec2.sql, spec2.args)
	if strings.Contains(plan2, "Seq Scan on rollup_rows") {
		t.Fatalf("hour query plan full-scans rollup_rows; want an index scan:\n%s", plan2)
	}
	if !indexedAccess(plan2) {
		t.Fatalf("hour query plan does not use an index on rollup_rows:\n%s", plan2)
	}
}

// explain runs EXPLAIN on the driver's statement and returns the plan text.
func explain(t *testing.T, d *driver, sql string, args []any) string {
	t.Helper()
	rows, err := d.db.QueryContext(context.Background(), "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return out.String()
}

// indexedAccess reports whether an EXPLAIN plan resolves its rows through
// an index (Index Scan, Index Only Scan, or a bitmap index path). WHICH
// rollup index the planner picks (idx_rollup_bucket_tenant, the PK, or
// another) is a cost decision; the contract pin is that it never
// full-scans the row table.
func indexedAccess(plan string) bool {
	for _, marker := range []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"} {
		if strings.Contains(plan, marker) {
			return true
		}
	}
	return false
}

// TestQuery_BuildWindowBounded pins the window-bounding at the SQL level:
// buildQuery always emits the half-open [From, To) predicate with
// parameterized bounds, so a Query can never scan outside its bounded
// window even if a future caller smuggles an unbounded shape past
// Validate.
func TestQuery_BuildWindowBounded(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	q := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	spec, err := buildQuery(q)
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	if !strings.Contains(spec.sql, "rollup_rows.bucket_start >= $1") ||
		!strings.Contains(spec.sql, "rollup_rows.bucket_start < $2") {
		t.Fatalf("query is missing the mandatory bounded window:\n%s", spec.sql)
	}
	if len(spec.args) < 2 {
		t.Fatalf("query args = %v; want at least the window bounds", spec.args)
	}
}

// TestQuery_BuildShapeSanity pins the SQL generator's structural
// invariants for the EXACT query shapes the conformance suite drives
// (window variants, every bucket size, filters on every axis, group-bys,
// every sort key, measure sorts, and cursors from a paginated walk):
// every $N parameter reference is in range and the arg list has exactly
// one entry per reference, and the statement is paren-balanced outside
// single-quoted literals. This catches parameter-ordering and
// argument-count bugs that only a live Postgres would otherwise surface —
// the DSN-gated conformance suite is the real gate, this is the cheap
// pre-check that runs everywhere.
func TestQuery_BuildShapeSanity(t *testing.T) {
	h0 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	h1 := h0.Add(time.Hour)
	base := rollups.Query{
		From:     h0,
		To:       h1,
		Bucket:   rollups.BucketMinute,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}

	// A paginated walk's cursor: produced by the same shape with Limit 2.
	paged := base
	paged.GroupBy = []rollups.Dimension{rollups.DimensionSession}
	paged.Limit = 2
	pagedCursor := rollups.PageCursor{
		ShapeVersion: rollups.CursorShapeVersion,
		Fingerprint:  rollups.QueryShapeFingerprint(paged),
		BucketNano:   h0.UnixNano(),
		Group:        rollups.DimensionValues{rollups.DimensionSession: "session-2"},
	}
	cursorStr, err := rollups.EncodeCursor(pagedCursor)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*rollups.Query)
	}{
		{"minute window no filters", func(q *rollups.Query) {}},
		{"hour bucket", func(q *rollups.Query) { q.Bucket = rollups.BucketHour; q.From = h0; q.To = h1.Add(2 * time.Hour) }},
		{"day bucket", func(q *rollups.Query) {
			q.Bucket = rollups.BucketDay
			day := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
			q.From = day
			q.To = day.Add(24 * time.Hour)
		}},
		{"tenant filter", func(q *rollups.Query) { q.Filter = rollups.Filter{TenantIDs: []string{"tenant-a", "tenant-b"}} }},
		{"user filter", func(q *rollups.Query) { q.Filter = rollups.Filter{UserIDs: []string{"user-1"}} }},
		{"session filter", func(q *rollups.Query) { q.Filter = rollups.Filter{SessionIDs: []string{"session-1"}} }},
		{"model filter empty string", func(q *rollups.Query) { q.Filter = rollups.Filter{Models: []string{""}} }},
		{"all filters", func(q *rollups.Query) {
			q.Filter = rollups.Filter{TenantIDs: []string{"t"}, UserIDs: []string{"u"}, SessionIDs: []string{"s"}, Models: []string{"m"}}
		}},
		{"group by tenant model", func(q *rollups.Query) {
			q.GroupBy = []rollups.Dimension{rollups.DimensionTenant, rollups.DimensionModel}
		}},
		{"group by all dims", func(q *rollups.Query) {
			q.GroupBy = []rollups.Dimension{rollups.DimensionTenant, rollups.DimensionUser, rollups.DimensionSession, rollups.DimensionModel}
		}},
		{"bucket desc", func(q *rollups.Query) { q.Sort = rollups.SortKeyBucketDesc }},
		{"measure asc", func(q *rollups.Query) {
			q.Sort = rollups.SortKeyMeasureAsc
			q.SortMeasure = rollups.MeasureLLMCostMicros
		}},
		{"measure desc", func(q *rollups.Query) {
			q.Sort = rollups.SortKeyMeasureDesc
			q.SortMeasure = rollups.MeasureLLMCostMicros
		}},
		{"cursor page 2", func(q *rollups.Query) {
			q.GroupBy = []rollups.Dimension{rollups.DimensionSession}
			q.Cursor = cursorStr
		}},
		{"cursor measure desc", func(q *rollups.Query) {
			q.GroupBy = []rollups.Dimension{rollups.DimensionSession}
			q.Sort = rollups.SortKeyMeasureDesc
			q.SortMeasure = rollups.MeasureLLMCostMicros
			c := pagedCursor
			c.ShapeVersion = rollups.CursorShapeVersion
			c.Fingerprint = rollups.QueryShapeFingerprint(*q)
			c.MeasureVal = 42
			cs, err := rollups.EncodeCursor(c)
			if err != nil {
				t.Fatalf("EncodeCursor: %v", err)
			}
			q.Cursor = cs
		}},
		{"latency min max only", func(q *rollups.Query) {
			q.Measures = []rollups.Measure{rollups.MeasureLLMLatencyMinMS, rollups.MeasureLLMLatencyMaxMS}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			tc.mut(&q)
			if err := q.Validate(); err != nil {
				t.Fatalf("test query invalid: %v", err)
			}
			spec, err := buildQuery(q)
			if err != nil {
				t.Fatalf("buildQuery: %v", err)
			}
			if !strings.HasPrefix(spec.sql, "SELECT ") {
				t.Fatalf("statement does not start with SELECT:\n%s", spec.sql)
			}
			if !strings.Contains(spec.sql, " LIMIT $") {
				t.Fatalf("statement lacks a LIMIT:\n%s", spec.sql)
			}
			assertParenBalance(t, spec.sql)
			assertParamReferences(t, spec.sql, len(spec.args))
		})
	}
}

// paramRefRE matches every $N parameter reference in a statement.
var paramRefRE = regexp.MustCompile(`\$(\d+)`)

// assertParamReferences verifies that the statement references exactly the
// parameter slots 1..n, each at least once, and nothing beyond — the arg
// list must line up 1:1 with the references (a slot referenced twice has
// one arg, so the reference COUNT may exceed n; the referenced MAX must
// equal n and every slot 1..n must be referenced).
func assertParamReferences(t *testing.T, sql string, n int) {
	t.Helper()
	seen := make(map[int]bool)
	for _, m := range paramRefRE.FindAllStringSubmatch(sql, -1) {
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad $ref %q in:\n%s", m[0], sql)
		}
		if idx < 1 || idx > n {
			t.Fatalf("$%d out of range (args=%d) in:\n%s", idx, n, sql)
		}
		seen[idx] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Fatalf("argument slot $%d is never referenced in:\n%s", i, sql)
		}
	}
}

// assertParenBalance verifies balanced parentheses outside single-quoted
// string literals (the date_trunc units 'hour' / 'day' are the only
// literals the generator emits).
func assertParenBalance(t *testing.T, sql string) {
	t.Helper()
	depth := 0
	inQuote := false
	for i := range len(sql) {
		c := sql[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				t.Fatalf("unbalanced ')' at offset %d in:\n%s", i, sql)
			}
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced parens (depth %d) in:\n%s", depth, sql)
	}
}
