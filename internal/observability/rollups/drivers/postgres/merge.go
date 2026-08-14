package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// rowAcc is one rollup row's exact integer measure values during a batch
// merge. It mirrors rollups.MeasureSet field-for-field (never float64),
// plus a local latency-fold flag.
//
// The latency-fold identity: a delta or row "has latency" exactly when its
// latency count is > 0. The domain guarantees this — rollups.Extract sets
// hasLatency = true only together with LLMLatencyCount = 1, and the flag is
// unexported, so every MeasureSet the driver can receive satisfies
// (hasLatency ⟺ LLMLatencyCount > 0) — including hand-built deltas, whose
// hasLatency is false with count 0 (a hand-built count > 0 with no min/max
// folds zeros, which matches the reference driver's output for that shape).
// The driver therefore never needs the unexported flag: it derives it from
// the latency count and reproduces MeasureSet.Add's fold semantics exactly.
type rowAcc struct {
	completions      int64
	tokensPrompt     int64
	tokensCompletion int64
	tokensReasoning  int64
	tokensCacheRead  int64
	tokensCacheWrite int64
	tokensTotal      int64
	costMicros       int64
	latencyCount     int64
	latencySumMS     int64
	latencyMinMS     int64
	latencyMaxMS     int64
	tasksCompleted   int64
	tasksFailed      int64
	tasksCancelled   int64

	hasLatency bool
}

// mergeDelta accumulates one delta into the accumulator with the same
// CHECKED semantics as rollups.MeasureSet.Add: a negative additive delta
// fails loudly with wrapped rollups.ErrNegativeMeasure (a counter never
// shrinks), an int64 sum overflow fails loudly with wrapped
// rollups.ErrMeasureOverflow, and the latency min/max fold as the
// group-wise minimum / maximum. The accumulator is left EXACTLY as it was
// on a refused merge.
func (a *rowAcc) mergeDelta(d rollups.Delta) error {
	add := d.Add
	var (
		completions      int64
		tokensPrompt     int64
		tokensCompletion int64
		tokensReasoning  int64
		tokensCacheRead  int64
		tokensCacheWrite int64
		tokensTotal      int64
		costMicros       int64
		latencyCount     int64
		latencySumMS     int64
		tasksCompleted   int64
		tasksFailed      int64
		tasksCancelled   int64
	)
	var err error
	if completions, err = checkedAdd(a.completions, add.LLMCompletions, rollups.MeasureLLMCompletions); err != nil {
		return err
	}
	if tokensPrompt, err = checkedAdd(a.tokensPrompt, add.LLMTokensPrompt, rollups.MeasureLLMTokensPrompt); err != nil {
		return err
	}
	if tokensCompletion, err = checkedAdd(a.tokensCompletion, add.LLMTokensCompletion, rollups.MeasureLLMTokensCompletion); err != nil {
		return err
	}
	if tokensReasoning, err = checkedAdd(a.tokensReasoning, add.LLMTokensReasoning, rollups.MeasureLLMTokensReasoning); err != nil {
		return err
	}
	if tokensCacheRead, err = checkedAdd(a.tokensCacheRead, add.LLMTokensCacheRead, rollups.MeasureLLMTokensCacheRead); err != nil {
		return err
	}
	if tokensCacheWrite, err = checkedAdd(a.tokensCacheWrite, add.LLMTokensCacheWrite, rollups.MeasureLLMTokensCacheWrite); err != nil {
		return err
	}
	if tokensTotal, err = checkedAdd(a.tokensTotal, add.LLMTokensTotal, rollups.MeasureLLMTokensTotal); err != nil {
		return err
	}
	if costMicros, err = checkedAdd(a.costMicros, add.LLMCostMicros, rollups.MeasureLLMCostMicros); err != nil {
		return err
	}
	if latencyCount, err = checkedAdd(a.latencyCount, add.LLMLatencyCount, rollups.MeasureLLMLatencyCount); err != nil {
		return err
	}
	if latencySumMS, err = checkedAdd(a.latencySumMS, add.LLMLatencySumMS, rollups.MeasureLLMLatencySumMS); err != nil {
		return err
	}
	if tasksCompleted, err = checkedAdd(a.tasksCompleted, add.TasksCompleted, rollups.MeasureTasksCompleted); err != nil {
		return err
	}
	if tasksFailed, err = checkedAdd(a.tasksFailed, add.TasksFailed, rollups.MeasureTasksFailed); err != nil {
		return err
	}
	if tasksCancelled, err = checkedAdd(a.tasksCancelled, add.TasksCancelled, rollups.MeasureTasksCancelled); err != nil {
		return err
	}

	// Latency min/max are folds — exact comparisons, never sums, so they
	// cannot overflow. The delta carries latency exactly when its latency
	// count is > 0 (the domain invariant above).
	minMS, maxMS, hasLatency := a.latencyMinMS, a.latencyMaxMS, a.hasLatency
	if add.LLMLatencyCount > 0 {
		if !hasLatency || add.LLMLatencyMinMS < minMS {
			minMS = add.LLMLatencyMinMS
		}
		if !hasLatency || add.LLMLatencyMaxMS > maxMS {
			maxMS = add.LLMLatencyMaxMS
		}
		hasLatency = true
	}

	a.completions = completions
	a.tokensPrompt = tokensPrompt
	a.tokensCompletion = tokensCompletion
	a.tokensReasoning = tokensReasoning
	a.tokensCacheRead = tokensCacheRead
	a.tokensCacheWrite = tokensCacheWrite
	a.tokensTotal = tokensTotal
	a.costMicros = costMicros
	a.latencyCount = latencyCount
	a.latencySumMS = latencySumMS
	a.latencyMinMS = minMS
	a.latencyMaxMS = maxMS
	a.hasLatency = hasLatency
	a.tasksCompleted = tasksCompleted
	a.tasksFailed = tasksFailed
	a.tasksCancelled = tasksCancelled
	return nil
}

// checkedAdd returns a+b when the exact int64 sum is representable,
// failing loudly otherwise — the exact mirror of the domain's checkedSum:
// a NEGATIVE delta is refused up front with wrapped
// rollups.ErrNegativeMeasure (a counter never shrinks, even when the sum
// would stay in range), and a non-negative delta that would overflow the
// int64 range fails with wrapped rollups.ErrMeasureOverflow naming the
// measure.
func checkedAdd(a, b int64, measure rollups.Measure) (int64, error) {
	if b < 0 {
		return 0, fmt.Errorf("%w: %s delta %d is negative", rollups.ErrNegativeMeasure, measure, b)
	}
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %s sum would overflow the int64 range", rollups.ErrMeasureOverflow, measure)
	}
	return a + b, nil
}

// mergeBatchRows folds the batch's deltas into their rows: one read of the
// existing rows for the batch's distinct keys, one checked merge per key
// (same-key deltas fold together, starting from the stored row), and a
// map key → merged values. A refused merge (negative delta or overflow)
// returns the wrapped sentinel and applies NOTHING — the caller's
// transaction rolls back.
func mergeBatchRows(ctx context.Context, tx *sql.Tx, deltas []rollups.Delta) (map[rollups.Key]*rowAcc, error) {
	if len(deltas) == 0 {
		return nil, nil
	}
	keys := distinctKeys(deltas)
	existing, err := readRows(ctx, tx, keys)
	if err != nil {
		return nil, fmt.Errorf("rollups/postgres: read existing rows: %w", err)
	}
	merged := make(map[rollups.Key]*rowAcc, len(keys))
	for _, k := range keys {
		acc := existing[k]
		if acc == nil {
			acc = &rowAcc{}
		}
		merged[k] = acc
	}
	for _, d := range deltas {
		if err := merged[d.Key].mergeDelta(d); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// distinctKeys returns the batch's row keys with set semantics, in
// first-seen order.
func distinctKeys(deltas []rollups.Delta) []rollups.Key {
	seen := make(map[rollups.Key]struct{}, len(deltas))
	out := make([]rollups.Key, 0, len(deltas))
	for _, d := range deltas {
		if _, dup := seen[d.Key]; dup {
			continue
		}
		seen[d.Key] = struct{}{}
		out = append(out, d.Key)
	}
	return out
}

// rowColumns is the full stored-row column list (key + measures).
const rowColumns = `bucket_start, tenant_id, user_id, session_id, model,
	llm_completions, llm_tokens_prompt, llm_tokens_completion, llm_tokens_reasoning,
	llm_tokens_cache_read, llm_tokens_cache_write, llm_tokens_total, llm_cost_micros,
	llm_latency_count, llm_latency_sum_ms, llm_latency_min_ms, llm_latency_max_ms,
	tasks_completed, tasks_failed, tasks_cancelled`

// readRows loads the stored rows for the batch's distinct keys in one
// parameterized query, returning a map key → accumulator.
func readRows(ctx context.Context, tx *sql.Tx, keys []rollups.Key) (map[rollups.Key]*rowAcc, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(rowColumns)
	sb.WriteString(" FROM rollup_rows WHERE ")
	args := make([]any, 0, len(keys)*5)
	n := 1
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		fmt.Fprintf(&sb, "(bucket_start = $%d AND tenant_id = $%d AND user_id = $%d AND session_id = $%d AND model = $%d)",
			n, n+1, n+2, n+3, n+4)
		n += 5
		args = append(args, k.BucketStart.UTC(), k.TenantID, k.UserID, k.SessionID, k.Model)
	}
	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[rollups.Key]*rowAcc, len(keys))
	for rows.Next() {
		var (
			bucketStart      time.Time
			tenantID, userID string
			sessionID, model string
			acc              rowAcc
		)
		if err := rows.Scan(&bucketStart, &tenantID, &userID, &sessionID, &model,
			&acc.completions, &acc.tokensPrompt, &acc.tokensCompletion, &acc.tokensReasoning,
			&acc.tokensCacheRead, &acc.tokensCacheWrite, &acc.tokensTotal, &acc.costMicros,
			&acc.latencyCount, &acc.latencySumMS, &acc.latencyMinMS, &acc.latencyMaxMS,
			&acc.tasksCompleted, &acc.tasksFailed, &acc.tasksCancelled,
		); err != nil {
			return nil, err
		}
		acc.hasLatency = acc.latencyCount > 0
		out[rollups.Key{
			BucketStart: bucketStart.UTC(),
			TenantID:    tenantID,
			UserID:      userID,
			SessionID:   sessionID,
			Model:       model,
		}] = &acc
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// upsertRows writes the merged rows in ONE parameterized multi-row
// INSERT ... ON CONFLICT DO UPDATE. The merged values already include the
// stored row's values (readRows + mergeDelta), so the DO UPDATE branch
// writes EXCLUDED values wholesale — the write is idempotent and never
// additive on top of a racing write (writes are serialized on the
// checkpoint row anyway).
func upsertRows(ctx context.Context, tx *sql.Tx, merged map[rollups.Key]*rowAcc) error {
	if len(merged) == 0 {
		return nil
	}
	// Deterministic write order (sorted keys) keeps the statement stable
	// and the params deterministic across calls.
	keys := make([]rollups.Key, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })

	var sb strings.Builder
	sb.WriteString("INSERT INTO rollup_rows (")
	sb.WriteString(rowColumns)
	sb.WriteString(") VALUES ")
	args := make([]any, 0, len(keys)*20)
	n := 1
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(")
		for j := range 20 {
			if j > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "$%d", n)
			n++
		}
		sb.WriteString(")")
		a := merged[k]
		args = append(args, k.BucketStart.UTC(), k.TenantID, k.UserID, k.SessionID, k.Model,
			a.completions, a.tokensPrompt, a.tokensCompletion, a.tokensReasoning,
			a.tokensCacheRead, a.tokensCacheWrite, a.tokensTotal, a.costMicros,
			a.latencyCount, a.latencySumMS, a.latencyMinMS, a.latencyMaxMS,
			a.tasksCompleted, a.tasksFailed, a.tasksCancelled)
	}
	sb.WriteString(` ON CONFLICT (bucket_start, tenant_id, user_id, session_id, model) DO UPDATE SET
		llm_completions = EXCLUDED.llm_completions,
		llm_tokens_prompt = EXCLUDED.llm_tokens_prompt,
		llm_tokens_completion = EXCLUDED.llm_tokens_completion,
		llm_tokens_reasoning = EXCLUDED.llm_tokens_reasoning,
		llm_tokens_cache_read = EXCLUDED.llm_tokens_cache_read,
		llm_tokens_cache_write = EXCLUDED.llm_tokens_cache_write,
		llm_tokens_total = EXCLUDED.llm_tokens_total,
		llm_cost_micros = EXCLUDED.llm_cost_micros,
		llm_latency_count = EXCLUDED.llm_latency_count,
		llm_latency_sum_ms = EXCLUDED.llm_latency_sum_ms,
		llm_latency_min_ms = EXCLUDED.llm_latency_min_ms,
		llm_latency_max_ms = EXCLUDED.llm_latency_max_ms,
		tasks_completed = EXCLUDED.tasks_completed,
		tasks_failed = EXCLUDED.tasks_failed,
		tasks_cancelled = EXCLUDED.tasks_cancelled`)
	_, err := tx.ExecContext(ctx, sb.String(), args...)
	return err
}

// keyLess is a total order over row keys for the deterministic write pass.
func keyLess(a, b rollups.Key) bool {
	if !a.BucketStart.Equal(b.BucketStart) {
		return a.BucketStart.Before(b.BucketStart)
	}
	if a.TenantID != b.TenantID {
		return a.TenantID < b.TenantID
	}
	if a.UserID != b.UserID {
		return a.UserID < b.UserID
	}
	if a.SessionID != b.SessionID {
		return a.SessionID < b.SessionID
	}
	return a.Model < b.Model
}
