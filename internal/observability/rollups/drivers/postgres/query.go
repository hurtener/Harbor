package postgres

import (
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// querySpec is the fully-resolved SQL for one validated rollups.Query: the
// statement, its parameter args, the canonical-ordered group columns (for
// scanning + cursor build), and the requested measures in query order.
type querySpec struct {
	sql       string
	args      []any
	groupCols []string
	measures  []rollups.Measure
}

// buildQuery resolves a VALIDATED query into a single bounded, indexed SQL
// statement. The mandatory window predicate (WHERE bucket_start >= $1 AND
// bucket_start < $2) plus the closed-dimension filters drive the index;
// the minute rows are coarsened to the query's Bucket with fixed-UTC
// date_trunc; the requested measures aggregate as exact BIGINT sums / folds;
// the page is ordered by the query's total order and, when a cursor rides
// along, the keyset "strictly after the cursor" predicate is added as a
// HAVING clause (it references the aggregated measure sums).
func buildQuery(q rollups.Query) (querySpec, error) {
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = rollups.SortKeyBucketAsc
	}
	bucketExpr, err := bucketExprFor(q.Bucket)
	if err != nil {
		return querySpec{}, err
	}

	// Group columns in canonical (AllDimensions) order — the reference
	// driver's total-order tie-break, independent of the query's GroupBy
	// order.
	groupCols := make([]string, 0, len(q.GroupBy))
	for _, d := range rollups.AllDimensions {
		if containsDim(q.GroupBy, d) {
			groupCols = append(groupCols, dimColumn(d))
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(bucketExpr)
	sb.WriteString(" AS bk")
	for _, c := range groupCols {
		sb.WriteString(", ")
		sb.WriteString(c)
	}
	for _, m := range q.Measures {
		sb.WriteString(", ")
		sb.WriteString(measureExpr(m))
		sb.WriteString(" AS ")
		sb.WriteString(string(m))
	}
	sb.WriteString(" FROM rollup_rows WHERE rollup_rows.bucket_start >= $1 AND rollup_rows.bucket_start < $2")
	args := []any{q.From, q.To}
	argIdx := 3

	appendFilter := func(col string, vals []string) {
		vals = distinctStrings(vals)
		if len(vals) == 0 {
			return
		}
		fmt.Fprintf(&sb, " AND %s = ANY($%d)", col, argIdx)
		args = append(args, vals)
		argIdx++
	}
	appendFilter("tenant_id", q.Filter.TenantIDs)
	appendFilter("user_id", q.Filter.UserIDs)
	appendFilter("session_id", q.Filter.SessionIDs)
	appendFilter("model", q.Filter.Models)

	sb.WriteString(" GROUP BY bk")
	for _, c := range groupCols {
		sb.WriteString(", ")
		sb.WriteString(c)
	}

	if q.Cursor != "" {
		c, err := rollups.DecodeCursor(q.Cursor)
		if err != nil {
			return querySpec{}, err
		}
		// Structural defence beyond the fingerprint: the cursor's group
		// values must carry exactly the query's GroupBy dimensions.
		if !cursorShapeMatches(c.Group, q.GroupBy) {
			return querySpec{}, fmt.Errorf("%w: cursor group shape does not match the query's GroupBy", rollups.ErrBadCursor)
		}
		pred, predArgs := cursorPredicate(sortKey, q, c, groupCols, argIdx)
		sb.WriteString(" HAVING ")
		sb.WriteString(pred)
		args = append(args, predArgs...)
		argIdx += len(predArgs)
	}

	sb.WriteString(" ORDER BY ")
	sb.WriteString(strings.Join(orderByClause(sortKey, q, groupCols), ", "))
	fmt.Fprintf(&sb, " LIMIT $%d", argIdx)
	args = append(args, q.Limit+1)

	return querySpec{
		sql:       sb.String(),
		args:      args,
		groupCols: groupCols,
		measures:  append([]rollups.Measure(nil), q.Measures...),
	}, nil
}

// bucketExprFor is the fixed-UTC coarsening expression for the query bucket.
// The double AT TIME ZONE 'UTC' dance makes the truncation session-timezone
// independent: bucket_start AT TIME ZONE 'UTC' yields the naive UTC wall
// time, date_trunc truncates it, and the second AT TIME ZONE 'UTC'
// re-interprets the boundary as a UTC instant. Minute rows are already on
// the storage grid, so minute queries use the column directly.
func bucketExprFor(b rollups.BucketSize) (string, error) {
	switch b {
	case rollups.BucketMinute:
		return "rollup_rows.bucket_start", nil
	case rollups.BucketHour:
		return "date_trunc('hour', rollup_rows.bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'", nil
	case rollups.BucketDay:
		return "date_trunc('day', rollup_rows.bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'", nil
	default:
		return "", fmt.Errorf("rollups/postgres: internal: unsupported bucket %q", b)
	}
}

// cursorPredicate builds the keyset "the next page starts strictly after
// the cursor" predicate, mirroring the reference driver's rowAfter exactly:
//
//   - bucket_asc:     bk > cb OR (bk = cb AND groupAfter)
//   - bucket_desc:    bk < cb OR (bk = cb AND groupAfter)
//   - measure_asc:    m > cv OR (m = cv AND (bk > cb OR (bk = cb AND groupAfter)))
//   - measure_desc:   m < cv OR (m = cv AND (bk > cb OR (bk = cb AND groupAfter)))
//
// The measure comparisons use the aggregated expression (the same one in
// SELECT / ORDER BY), so they are exact integer comparisons — never float.
func cursorPredicate(sortKey rollups.SortKey, q rollups.Query, c rollups.PageCursor, groupCols []string, base int) (string, []any) {
	var parts []string
	args := make([]any, 0, len(groupCols)+4)
	idx := base

	switch sortKey {
	case rollups.SortKeyMeasureAsc, rollups.SortKeyMeasureDesc:
		m := measureExpr(q.SortMeasure)
		op := ">"
		if sortKey == rollups.SortKeyMeasureDesc {
			op = "<"
		}
		primary := fmt.Sprintf("%s %s $%d", m, op, idx)
		args = append(args, c.MeasureVal)
		idx++
		eq := fmt.Sprintf("%s = $%d", m, idx)
		args = append(args, c.MeasureVal)
		idx++
		bucketAfter, moreArgs := bucketTiePredicate(sortKey, c, groupCols, idx)
		args = append(args, moreArgs...)
		parts = append(parts, primary, fmt.Sprintf("(%s AND %s)", eq, bucketAfter))
	case rollups.SortKeyBucketDesc:
		bucketAfter, moreArgs := bucketTiePredicate(sortKey, c, groupCols, idx)
		args = append(args, moreArgs...)
		parts = append(parts, bucketAfter)
	default: // SortKeyBucketAsc
		bucketAfter, moreArgs := bucketTiePredicate(sortKey, c, groupCols, idx)
		args = append(args, moreArgs...)
		parts = append(parts, bucketAfter)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// bucketTiePredicate is the bucket component of the keyset predicate:
// (bk OP $cb OR (bk = $cb AND groupAfter)). The tie-break bucket direction
// is descending ONLY for the bucket_desc primary sort; measure sorts tie on
// the bucket ASCENDING (the reference driver's rowAfter).
func bucketTiePredicate(sortKey rollups.SortKey, c rollups.PageCursor, groupCols []string, base int) (string, []any) {
	op := ">"
	if sortKey == rollups.SortKeyBucketDesc {
		op = "<"
	}
	cb := time.Unix(0, c.BucketNano).UTC()
	groupPred, groupArgs := groupAfterPredicate(groupCols, c.Group, base+2)
	args := make([]any, 2, 2+len(groupArgs))
	args[0], args[1] = cb, cb
	args = append(args, groupArgs...)
	return fmt.Sprintf("(bk %s $%d OR (bk = $%d AND %s))", op, base, base+1, groupPred), args
}

// groupAfterPredicate is the canonical-order lexicographic group chain: the
// cursor's group values compared against the row's group columns, dimension
// by dimension in AllDimensions order. An empty group (no GroupBy) yields
// FALSE — with one row per bucket, the bucket predicate alone is total.
func groupAfterPredicate(cols []string, group rollups.DimensionValues, base int) (string, []any) {
	if len(cols) == 0 {
		return "FALSE", nil
	}
	vals := make([]string, len(cols))
	for i, c := range cols {
		vals[i] = group[dimOfColumn(c)]
	}
	var parts []string
	args := make([]any, 0, len(cols))
	idx := base
	for i := range cols {
		var conds []string
		for j := range i {
			conds = append(conds, fmt.Sprintf("%s = $%d", cols[j], idx))
			args = append(args, vals[j])
			idx++
		}
		conds = append(conds, fmt.Sprintf("%s > $%d", cols[i], idx))
		args = append(args, vals[i])
		idx++
		parts = append(parts, "("+strings.Join(conds, " AND ")+")")
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// orderByClause is the query's total order: primary sort key, then the
// bucket (ascending unless the primary IS bucket_desc), then the grouped
// dimension columns in canonical order. Every output row has a unique
// (bucket, group) key — grouping merged its duplicates — so the order is
// total and pagination never skips or repeats a row.
func orderByClause(sortKey rollups.SortKey, q rollups.Query, groupCols []string) []string {
	out := make([]string, 0, len(groupCols)+2)
	switch sortKey {
	case rollups.SortKeyBucketDesc:
		out = append(out, "bk DESC")
	case rollups.SortKeyMeasureAsc:
		out = append(out, measureExpr(q.SortMeasure)+" ASC", "bk ASC")
	case rollups.SortKeyMeasureDesc:
		out = append(out, measureExpr(q.SortMeasure)+" DESC", "bk ASC")
	default: // SortKeyBucketAsc
		out = append(out, "bk ASC")
	}
	for _, c := range groupCols {
		out = append(out, c+" ASC")
	}
	return out
}

// cursorShapeMatches reports whether the cursor's group values carry
// exactly the query's GroupBy dimensions — a cursor produced by a query
// with a different GroupBy (or hand-crafted) is rejected loudly rather than
// silently mis-paginating. The full shape binding (version + fingerprint)
// is enforced by Query.Validate; this is the structural defence against a
// fabricated position.
func cursorShapeMatches(group rollups.DimensionValues, groupBy []rollups.Dimension) bool {
	if len(group) != len(groupBy) {
		return false
	}
	for _, d := range groupBy {
		if _, ok := group[d]; !ok {
			return false
		}
	}
	return true
}

// containsDim reports whether dims contains d.
func containsDim(dims []rollups.Dimension, d rollups.Dimension) bool {
	for _, x := range dims {
		if x == d {
			return true
		}
	}
	return false
}

// distinctStrings returns vals with first-seen deduplication (set
// semantics for the filter axes).
func distinctStrings(vals []string) []string {
	if len(vals) < 2 {
		return vals
	}
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// dimColumn maps a closed dimension to its row column.
func dimColumn(d rollups.Dimension) string {
	switch d {
	case rollups.DimensionTenant:
		return "tenant_id"
	case rollups.DimensionUser:
		return "user_id"
	case rollups.DimensionSession:
		return "session_id"
	case rollups.DimensionModel:
		return "model"
	default:
		panic("rollups/postgres: unvalidated dimension " + string(d))
	}
}

// dimOfColumn is the inverse of dimColumn.
func dimOfColumn(c string) rollups.Dimension {
	switch c {
	case "tenant_id":
		return rollups.DimensionTenant
	case "user_id":
		return rollups.DimensionUser
	case "session_id":
		return rollups.DimensionSession
	case "model":
		return rollups.DimensionModel
	default:
		panic("rollups/postgres: unknown group column " + c)
	}
}

// measureColumn maps a closed measure to its row column. The measure is
// validated by the time buildQuery runs (Query.Validate), so the switch is
// total; the panics are "impossible by construction".
func measureColumn(m rollups.Measure) string {
	switch m {
	case rollups.MeasureLLMCompletions:
		return "llm_completions"
	case rollups.MeasureLLMTokensPrompt:
		return "llm_tokens_prompt"
	case rollups.MeasureLLMTokensCompletion:
		return "llm_tokens_completion"
	case rollups.MeasureLLMTokensReasoning:
		return "llm_tokens_reasoning"
	case rollups.MeasureLLMTokensCacheRead:
		return "llm_tokens_cache_read"
	case rollups.MeasureLLMTokensCacheWrite:
		return "llm_tokens_cache_write"
	case rollups.MeasureLLMTokensTotal:
		return "llm_tokens_total"
	case rollups.MeasureLLMCostMicros:
		return "llm_cost_micros"
	case rollups.MeasureLLMLatencyCount:
		return "llm_latency_count"
	case rollups.MeasureLLMLatencySumMS:
		return "llm_latency_sum_ms"
	case rollups.MeasureLLMLatencyMinMS:
		return "llm_latency_min_ms"
	case rollups.MeasureLLMLatencyMaxMS:
		return "llm_latency_max_ms"
	case rollups.MeasureTasksCompleted:
		return "tasks_completed"
	case rollups.MeasureTasksFailed:
		return "tasks_failed"
	case rollups.MeasureTasksCancelled:
		return "tasks_cancelled"
	default:
		panic("rollups/postgres: unvalidated measure " + string(m))
	}
}

// measureExpr is the aggregate expression for one measure: an exact BIGINT
// SUM (never float) for additive measures, MIN / MAX for the latency folds,
// with COALESCE to 0 for empty groups.
func measureExpr(m rollups.Measure) string {
	col := measureColumn(m)
	switch m {
	case rollups.MeasureLLMLatencyMinMS:
		return "COALESCE(MIN(" + col + "), 0)"
	case rollups.MeasureLLMLatencyMaxMS:
		return "COALESCE(MAX(" + col + "), 0)"
	default:
		return "COALESCE(SUM(" + col + "), 0)"
	}
}

// measureScale is the measure's fixed decimal denominator (CostScaleMicros
// for cost, 1 otherwise) — the same mapping the domain's MeasureSet.Get
// exposes.
func measureScale(m rollups.Measure) uint32 {
	if m == rollups.MeasureLLMCostMicros {
		return rollups.CostScaleMicros
	}
	return 1
}
