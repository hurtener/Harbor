// Package conformancetest exposes the canonical correctness suite every
// rollups.Store implementation must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/observability/rollups` does not import the standard library
// `testing` package (precedent: `internal/state/conformancetest`,
// `internal/identity/conformancetest`).
//
// Downstream drivers (the shipped in-memory reference, future SQLite and
// Postgres implementations) consume it via:
//
//	import "github.com/hurtener/Harbor/internal/observability/rollups/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() (rollups.Store, func()) {
//	        s := mydriver.New()
//	        return s, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty Store plus a cleanup callback. The
// suite uses the factory once per top-level subtest; invocations are
// independent.
//
// The suite drives the Store exclusively through the public domain surface
// (rollups.Extract to derive deltas from canonical events, rollups.Query to
// read) — exactly how the projector and future callers use a Store — so a
// driver that passes it is proven correct against the domain contract, not
// against an implementation's private shape.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Factory returns a fresh, empty rollups.Store and its cleanup callback.
type Factory func() (rollups.Store, func())

// Run executes the conformance suite against the factory's Store. Each
// top-level subtest gets its own fresh store.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CheckpointAndIdempotentReplay", func(t *testing.T) { checkpointAndIdempotentReplay(t, factory) })
	t.Run("Precision", func(t *testing.T) { precision(t, factory) })
	t.Run("FixedUTCBucketBoundaries", func(t *testing.T) { fixedUTCBucketBoundaries(t, factory) })
	t.Run("DimensionIsolation", func(t *testing.T) { dimensionIsolation(t, factory) })
	t.Run("QueryValidation", func(t *testing.T) { queryValidation(t, factory) })
	t.Run("DeterministicPagination", func(t *testing.T) { deterministicPagination(t, factory) })
	t.Run("GroupBy", func(t *testing.T) { groupBy(t, factory) })
	t.Run("ErasureFenceNonResurrection", func(t *testing.T) { erasureFenceNonResurrection(t, factory) })
	t.Run("RetentionAndRebuild", func(t *testing.T) { retentionAndRebuild(t, factory) })
	t.Run("ConcurrentQueries", func(t *testing.T) { concurrentQueries(t, factory) })
}

// --- fixtures -----------------------------------------------------------

var (
	quadT1U1S1 = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}}
	quadT1U1S2 = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-2"}}
	quadT1U2S3 = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-2", SessionID: "session-3"}}
	quadT2U1S1 = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "user-1", SessionID: "session-1"}}
)

// anchor is a fixed UTC instant; all fixtures derive from it so bucket
// expectations are exact and deterministic.
var anchor = time.Date(2026, 8, 13, 12, 34, 56, 789_000_000, time.UTC)

// costEvent builds a successfully-persisted `llm.cost.recorded` event.
func costEvent(seq uint64, at time.Time, quad identity.Quadruple, model string, costUSD float64, prompt, completion, latencyMS int) events.Event {
	total := prompt + completion
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity: quad,
			Model:    model,
			Cost:     llm.Cost{TotalCost: costUSD, Currency: "USD"},
			Usage: llm.Usage{
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      total,
				LatencyMS:        int64(latencyMS),
			},
			OccurredAt: at,
		},
	}
}

// taskEvent builds a successfully-persisted task outcome event.
func taskEvent(seq uint64, at time.Time, quad identity.Quadruple, typ events.EventType) events.Event {
	var payload events.EventPayload
	switch typ {
	case tasks.EventTypeTaskCompleted:
		payload = tasks.TaskCompletedPayload{TaskID: "t-complete"}
	case tasks.EventTypeTaskFailed:
		payload = tasks.TaskFailedPayload{TaskID: "t-fail", ErrorCode: "ERR"}
	case tasks.EventTypeTaskCancelled:
		payload = tasks.TaskCancelledPayload{TaskID: "t-cancel"}
	default:
		panic("bad task type")
	}
	return events.Event{Type: typ, Identity: quad, OccurredAt: at, Sequence: seq, Payload: payload}
}

// apply applies a run of contiguous events (sequences 1..len in order) as
// one atomic batch, returning the batch checkpoint.
func apply(ctx context.Context, t *testing.T, s rollups.Store, evs ...events.Event) uint64 {
	t.Helper()
	if len(evs) == 0 {
		return 0
	}
	var deltas []rollups.Delta
	for _, ev := range evs {
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract(seq=%d): %v", ev.Sequence, err)
		}
		deltas = append(deltas, ds...)
	}
	ckpt := evs[len(evs)-1].Sequence
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: ckpt, Deltas: deltas}); err != nil {
		t.Fatalf("ApplyBatch(checkpoint=%d): %v", ckpt, err)
	}
	return ckpt
}

// query runs a read over the store with a mandatory window + closed fields.
func query(ctx context.Context, t *testing.T, s rollups.Store, from, to time.Time, bucket rollups.BucketSize) (rollups.Result, error) {
	t.Helper()
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   bucket,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD, rollups.MeasureLLMCompletions, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    1000,
	}
	return s.Query(ctx, q)
}

func mustQuery(ctx context.Context, t *testing.T, s rollups.Store, from, to time.Time, bucket rollups.BucketSize) rollups.Result {
	t.Helper()
	res, err := query(ctx, t, s, from, to, bucket)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res
}

// --- subtests ------------------------------------------------------------

func checkpointAndIdempotentReplay(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	if ck, err := s.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("fresh checkpoint = %d, %v; want 0", ck, err)
	}

	h1 := anchor.Add(-3 * time.Hour)
	evs := []events.Event{
		costEvent(1, h1, quadT1U1S1, "model-a", 1.25, 100, 50, 800),
		costEvent(2, h1.Add(time.Minute), quadT1U1S1, "model-a", 0.75, 200, 40, 400),
		taskEvent(3, h1.Add(2*time.Minute), quadT1U1S1, tasks.EventTypeTaskCompleted),
	}
	apply(ctx, t, s, evs...)

	if ck, err := s.Checkpoint(ctx); err != nil || ck != 3 {
		t.Fatalf("checkpoint after apply = %d, %v; want 3", ck, err)
	}

	// Idempotent replay: re-applying the SAME batch (same checkpoint) is a
	// no-op — sums must not double.
	apply(ctx, t, s, evs...)
	res := mustQuery(ctx, t, s, anchor.Add(-24*time.Hour), anchor, rollups.BucketHour)
	if got := totalCost(res); got != 2.00 {
		t.Fatalf("cost after idempotent replay = %v; want 2.00", got)
	}
	if got := totalCompletions(res); got != 2 {
		t.Fatalf("completions after idempotent replay = %v; want 2", got)
	}
	if got := totalTasks(res, rollups.MeasureTasksCompleted); got != 1 {
		t.Fatalf("tasks_completed after idempotent replay = %v; want 1", got)
	}

	// A non-advancing batch — whether equal or BELOW the stored
	// checkpoint — is the idempotent no-op: deltas + checkpoint are
	// atomic, so every event it covers was already applied. This is what
	// makes concurrent advances and restart replays safe.
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 3, Deltas: nil}); err != nil {
		t.Fatalf("equal checkpoint apply: %v", err)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 2, Deltas: nil}); err != nil {
		t.Fatalf("behind checkpoint apply must be a no-op, got: %v", err)
	}
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 3 {
		t.Fatalf("checkpoint after no-ops = %d, %v; want 3", ck, err)
	}
}

func precision(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	// Costs that sum exactly in float64.
	var evs []events.Event
	for i, c := range []float64{0.25, 0.25, 0.5, 1.0, 2.0} {
		evs = append(evs, costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Minute), quadT1U1S1, "model-a", c, 10, 20, 100))
	}
	// Tokens and latency sums must be exact integers.
	apply(ctx, t, s, evs...)

	q := rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD, rollups.MeasureLLMCompletions, rollups.MeasureLLMTokensPrompt, rollups.MeasureLLMLatencyMS},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := totalCost(res); got != 4.00 {
		t.Fatalf("cost sum = %v; want 4.00 (exact float64)", got)
	}
	if got := totalCompletions(res); got != 5 {
		t.Fatalf("completions = %v; want 5", got)
	}
	// Prompt 5×10=50, completion 5×20=100, latency 5×100=500.
	row := res.Rows[0]
	if got := row.Measures[rollups.MeasureLLMTokensPrompt]; got != 50 {
		t.Fatalf("prompt tokens = %v; want 50", got)
	}
	if got := row.Measures[rollups.MeasureLLMCompletions]; got != 5 {
		t.Fatalf("completions measure = %v; want 5", got)
	}
	if got := row.Measures[rollups.MeasureLLMLatencyMS]; got != 500 {
		t.Fatalf("latency ms = %v; want 500", got)
	}
}

func fixedUTCBucketBoundaries(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	// Events exactly on hour and day boundaries must land in the NEXT
	// bucket, not the current one.
	hourEdge := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	dayEdge := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	evs := []events.Event{
		costEvent(1, hourEdge.Add(-time.Nanosecond), quadT1U1S1, "m", 1, 10, 10, 10), // 11:59:59.999… → hour 11
		costEvent(2, hourEdge, quadT1U1S1, "m", 2, 10, 10, 10),                       // 12:00:00 → hour 12
		costEvent(3, dayEdge.Add(-time.Nanosecond), quadT1U1S1, "m", 4, 10, 10, 10),  // 23:59:59.999… → day 13
		costEvent(4, dayEdge, quadT1U1S1, "m", 8, 10, 10, 10),                        // 00:00:00 → day 14
	}
	apply(ctx, t, s, evs...)

	// Hour granularity over Aug 13: buckets 11:00 (event 1), 12:00
	// (event 2), and 23:00 (event 3 — still Aug 13). Event 4 opens
	// Aug 14 and is excluded by the half-open window.
	res := mustQuery(ctx, t, s, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), rollups.BucketHour)
	if len(res.Rows) != 3 {
		t.Fatalf("hour rows = %d; want 3 (buckets 11:00, 12:00, 23:00)", len(res.Rows))
	}
	if got := totalCost(res); got != 7 {
		t.Fatalf("hour-window cost = %v; want 7 (events 1+2+3)", got)
	}
	if res.Rows[0].BucketStart.Hour() != 11 || res.Rows[1].BucketStart.Hour() != 12 || res.Rows[2].BucketStart.Hour() != 23 {
		t.Fatalf("hour bucket starts = %d, %d, %d; want 11, 12, 23",
			res.Rows[0].BucketStart.Hour(), res.Rows[1].BucketStart.Hour(), res.Rows[2].BucketStart.Hour())
	}

	// Day granularity over the two UTC days: buckets Aug 13 and Aug 14.
	res = mustQuery(ctx, t, s, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), rollups.BucketDay)
	if len(res.Rows) != 2 {
		t.Fatalf("day rows = %d; want 2", len(res.Rows))
	}
	if got := totalCost(res); got != 15 {
		t.Fatalf("day-window cost = %v; want 15 (events 1+2+3+4)", got)
	}
	if res.Rows[0].BucketStart.Day() != 13 || res.Rows[1].BucketStart.Day() != 14 {
		t.Fatalf("day bucket starts = day %d, day %d; want 13, 14", res.Rows[0].BucketStart.Day(), res.Rows[1].BucketStart.Day())
	}
}

func dimensionIsolation(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S2, "model-a", 2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quadT1U2S3, "model-b", 4, 10, 10, 10),
		costEvent(4, h.Add(3*time.Minute), quadT2U1S1, "model-b", 8, 10, 10, 10),
	}
	apply(ctx, t, s, evs...)

	from, to := h, h.Add(time.Hour)

	// Whole window: all four.
	res := mustQuery(ctx, t, s, from, to, rollups.BucketHour)
	if got := totalCost(res); got != 15 {
		t.Fatalf("all-tenant cost = %v; want 15", got)
	}

	// Tenant isolation: tenant-a only sees its own rows.
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-a"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("tenant-a query: %v", err)
	}
	if got := sumCost(res); got != 7 {
		t.Fatalf("tenant-a cost = %v; want 7 (1+2+4)", got)
	}

	// Session isolation within a tenant.
	q.Filter = rollups.Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"session-2"}}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("session-2 query: %v", err)
	}
	if got := sumCost(res); got != 2 {
		t.Fatalf("session-2 cost = %v; want 2", got)
	}

	// Model isolation: model-a only.
	q.Filter = rollups.Filter{Models: []string{"model-a"}}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("model-a query: %v", err)
	}
	if got := sumCost(res); got != 3 {
		t.Fatalf("model-a cost = %v; want 3 (1+2)", got)
	}

	// An empty-model filter means "un-attributed rows only" — task
	// outcomes have no model. cost.recorded rows carry a model, so a
	// Models filter of {""} must NOT match them.
	task := taskEvent(5, h.Add(4*time.Minute), quadT1U1S1, tasks.EventTypeTaskCompleted)
	apply(ctx, t, s, task)
	q.Filter = rollups.Filter{Models: []string{""}}
	q.Measures = []rollups.Measure{rollups.MeasureTasksCompleted}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("un-attributed model query: %v", err)
	}
	if got := sumTasks(res, rollups.MeasureTasksCompleted); got != 1 {
		t.Fatalf("un-attributed tasks = %v; want 1", got)
	}
}

func queryValidation(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	apply(ctx, t, s, costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10))

	base := rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}

	cases := []struct {
		name string
		mut  func(*rollups.Query)
		want error
	}{
		{"empty window", func(q *rollups.Query) { q.From, q.To = time.Time{}, time.Time{} }, rollups.ErrQueryInvalid},
		{"reversed window", func(q *rollups.Query) { q.From, q.To = q.To, q.From }, rollups.ErrQueryInvalid},
		{"unknown bucket", func(q *rollups.Query) { q.Bucket = "week" }, rollups.ErrQueryInvalid},
		{"unknown dimension", func(q *rollups.Query) { q.GroupBy = []rollups.Dimension{"run"} }, rollups.ErrQueryInvalid},
		{"repeated dimension", func(q *rollups.Query) {
			q.GroupBy = []rollups.Dimension{rollups.DimensionTenant, rollups.DimensionTenant}
		}, rollups.ErrQueryInvalid},
		{"empty measures", func(q *rollups.Query) { q.Measures = nil }, rollups.ErrQueryInvalid},
		{"unknown measure", func(q *rollups.Query) { q.Measures = []rollups.Measure{"tokens_cached"} }, rollups.ErrQueryInvalid},
		{"repeated measure", func(q *rollups.Query) {
			q.Measures = []rollups.Measure{rollups.MeasureLLMCostUSD, rollups.MeasureLLMCostUSD}
		}, rollups.ErrQueryInvalid},
		{"unknown sort", func(q *rollups.Query) { q.Sort = "cost" }, rollups.ErrQueryInvalid},
		{"measure sort without measure", func(q *rollups.Query) { q.Sort = rollups.SortKeyMeasureDesc }, rollups.ErrQueryInvalid},
		{"zero limit", func(q *rollups.Query) { q.Limit = 0 }, rollups.ErrQueryInvalid},
		{"negative limit", func(q *rollups.Query) { q.Limit = -5 }, rollups.ErrQueryInvalid},
		{"limit over budget", func(q *rollups.Query) { q.Limit = rollups.MaxRowsPerQuery + 1 }, rollups.ErrQueryBudget},
		{"bucket budget", func(q *rollups.Query) { q.From = h.Add(-time.Duration(rollups.MaxBuckets+1) * time.Hour) }, rollups.ErrQueryBudget},
		{"malformed cursor", func(q *rollups.Query) { q.Cursor = "not-a-cursor" }, rollups.ErrBadCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			tc.mut(&q)
			_, err := s.Query(ctx, q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Query err = %v; want %v", err, tc.want)
			}
		})
	}
}

func deterministicPagination(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	// 3 sessions × 3 hours, one cost event per (session, hour).
	h0 := rollups.BucketStart(anchor, rollups.BucketHour)
	var evs []events.Event
	seq := uint64(1)
	for hour := 0; hour < 3; hour++ {
		for _, quad := range []identity.Quadruple{quadT1U1S1, quadT1U1S2, quadT1U2S3} {
			evs = append(evs, costEvent(seq, h0.Add(time.Duration(hour)*time.Hour), quad, "model-a", float64(seq), 10, 10, 10))
			seq++
		}
	}
	apply(ctx, t, s, evs...)

	from, to := h0, h0.Add(3*time.Hour)
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		GroupBy:  []rollups.Dimension{rollups.DimensionSession},
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    2, // page through 9 rows in pages of 2
	}

	// First page; walk to the end; collect rows.
	var collected []rollups.Row
	cursor := ""
	for {
		q.Cursor = cursor
		res, err := s.Query(ctx, q)
		if err != nil {
			t.Fatalf("Query page: %v", err)
		}
		if len(res.Rows) > 2 {
			t.Fatalf("page exceeds Limit: got %d rows", len(res.Rows))
		}
		collected = append(collected, res.Rows...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	if len(collected) != 9 {
		t.Fatalf("paginated rows = %d; want 9 (3 hours × 3 sessions)", len(collected))
	}

	// Determinism: the same full query (no cursor) yields the same rows.
	full := q
	full.Cursor = ""
	full.Limit = 100
	res1, err := s.Query(ctx, full)
	if err != nil {
		t.Fatalf("full query 1: %v", err)
	}
	res2, err := s.Query(ctx, full)
	if err != nil {
		t.Fatalf("full query 2: %v", err)
	}
	if len(res1.Rows) != len(res2.Rows) {
		t.Fatalf("full query row counts differ: %d vs %d", len(res1.Rows), len(res2.Rows))
	}
	for i := range res1.Rows {
		if res1.Rows[i].BucketStart != res2.Rows[i].BucketStart ||
			res1.Rows[i].Dimensions[rollups.DimensionSession] != res2.Rows[i].Dimensions[rollups.DimensionSession] {
			t.Fatalf("full query row %d differs between identical queries", i)
		}
	}

	// Pages do not skip or repeat: the concatenated page walk must equal
	// the full result row-for-row.
	if len(collected) != len(res1.Rows) {
		t.Fatalf("page walk length %d != full result length %d", len(collected), len(res1.Rows))
	}
	for i := range collected {
		if collected[i].BucketStart != res1.Rows[i].BucketStart ||
			collected[i].Dimensions[rollups.DimensionSession] != res1.Rows[i].Dimensions[rollups.DimensionSession] {
			t.Fatalf("page walk diverges at index %d", i)
		}
	}

	// Measure sort (desc) is total and paginates the same way.
	mq := rollups.Query{
		From:        from,
		To:          to,
		Bucket:      rollups.BucketHour,
		GroupBy:     []rollups.Dimension{rollups.DimensionSession},
		Measures:    []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:        rollups.SortKeyMeasureDesc,
		SortMeasure: rollups.MeasureLLMCostUSD,
		Limit:       100,
	}
	mres, err := s.Query(ctx, mq)
	if err != nil {
		t.Fatalf("measure-sort query: %v", err)
	}
	if len(mres.Rows) != 9 {
		t.Fatalf("measure-sort rows = %d; want 9", len(mres.Rows))
	}
	for i := 1; i < len(mres.Rows); i++ {
		prev := mres.Rows[i-1].Measures[rollups.MeasureLLMCostUSD]
		cur := mres.Rows[i].Measures[rollups.MeasureLLMCostUSD]
		if prev < cur {
			t.Fatalf("measure-desc order broken at %d: %v then %v", i, prev, cur)
		}
	}

	// A cursor produced by a DIFFERENT query shape (different GroupBy) is
	// rejected loudly, never silently mis-paginated.
	tenantGrouped := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		GroupBy:  []rollups.Dimension{rollups.DimensionTenant},
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    1, // page through 3 tenants → a non-empty NextCursor
	}
	tg, err := s.Query(ctx, tenantGrouped)
	if err != nil {
		t.Fatalf("tenant-grouped query: %v", err)
	}
	if tg.NextCursor == "" {
		t.Fatal("tenant-grouped page must carry a next cursor")
	}
	mismatched := q
	mismatched.Cursor = tg.NextCursor
	mismatched.Limit = 100
	if _, err := s.Query(ctx, mismatched); !errors.Is(err, rollups.ErrBadCursor) {
		t.Fatalf("mismatched-shape cursor err = %v; want ErrBadCursor", err)
	}
}

func groupBy(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S1, "model-b", 2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quadT1U1S2, "model-a", 4, 10, 10, 10),
	}
	apply(ctx, t, s, evs...)

	from, to := h, h.Add(time.Hour)

	// No GroupBy: one row per bucket over the whole window.
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("no-groupby query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("no-groupby rows = %d; want 1", len(res.Rows))
	}
	if len(res.Rows[0].Dimensions) != 0 {
		t.Fatalf("no-groupby Dimensions = %v; want empty", res.Rows[0].Dimensions)
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostUSD]; got != 7 {
		t.Fatalf("no-groupby cost = %v; want 7", got)
	}

	// GroupBy (tenant, model): rows = (tenant-a, model-a) 5 and
	// (tenant-a, model-b) 2.
	q.GroupBy = []rollups.Dimension{rollups.DimensionTenant, rollups.DimensionModel}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("groupby query: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("groupby rows = %d; want 2", len(res.Rows))
	}
	for _, r := range res.Rows {
		if r.Dimensions[rollups.DimensionTenant] != "tenant-a" {
			t.Fatalf("groupby tenant = %q; want tenant-a", r.Dimensions[rollups.DimensionTenant])
		}
		switch r.Dimensions[rollups.DimensionModel] {
		case "model-a":
			if got := r.Measures[rollups.MeasureLLMCostUSD]; got != 5 {
				t.Fatalf("model-a group cost = %v; want 5", got)
			}
		case "model-b":
			if got := r.Measures[rollups.MeasureLLMCostUSD]; got != 2 {
				t.Fatalf("model-b group cost = %v; want 2", got)
			}
		default:
			t.Fatalf("unexpected group model %q", r.Dimensions[rollups.DimensionModel])
		}
	}
}

func erasureFenceNonResurrection(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	triple := quadT1U1S1.Identity
	apply(ctx, t, s,
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S2, "model-a", 2, 10, 10, 10),
	)

	// Fence session-1: its rows are erased, session-2's survive.
	if err := s.FenceSession(ctx, triple); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if f, err := s.IsFenced(ctx, triple); err != nil || !f {
		t.Fatalf("IsFenced after fence = %v, %v; want true", f, err)
	}
	res := mustQuery(ctx, t, s, h, h.Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 2 {
		t.Fatalf("cost after fence = %v; want 2 (session-2 only)", got)
	}

	// A late event for the fenced triple is REFUSED — the erasure is never
	// resurrected by an asynchronous tail event.
	late := costEvent(3, h.Add(2*time.Minute), quadT1U1S1, "model-a", 100, 10, 10, 10)
	deltas, err := rollups.Extract(late)
	if err != nil {
		t.Fatalf("Extract(late): %v", err)
	}
	err = s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 3, Deltas: deltas})
	if !errors.Is(err, rollups.ErrSessionFenced) {
		t.Fatalf("late apply err = %v; want ErrSessionFenced", err)
	}
	// The checkpoint must NOT have advanced past the refused batch.
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 2 {
		t.Fatalf("checkpoint after refused batch = %d, %v; want 2", ck, err)
	}
	res = mustQuery(ctx, t, s, h, h.Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 2 {
		t.Fatalf("cost after refused late event = %v; want 2 (no resurrection)", got)
	}

	// Unfence: the triple may be reused afresh; new events flow again, and
	// the old rows stay gone.
	if err := s.UnfenceSession(ctx, triple); err != nil {
		t.Fatalf("UnfenceSession: %v", err)
	}
	if f, err := s.IsFenced(ctx, triple); err != nil || f {
		t.Fatalf("IsFenced after unfence = %v, %v; want false", f, err)
	}
	apply(ctx, t, s, costEvent(4, h.Add(3*time.Minute), quadT1U1S1, "model-a", 4, 10, 10, 10))
	res = mustQuery(ctx, t, s, h, h.Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 6 {
		t.Fatalf("cost after unfence+new event = %v; want 6 (2 + 4)", got)
	}
}

func retentionAndRebuild(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	// Empty store: zero retention.
	old, new, err := s.Retention(ctx)
	if err != nil || !old.IsZero() || !new.IsZero() {
		t.Fatalf("empty retention = %v..%v, %v; want zero..zero", old, new, err)
	}

	h1 := rollups.BucketStart(anchor.Add(-2*time.Hour), rollups.BucketHour)
	h2 := rollups.BucketStart(anchor, rollups.BucketHour)
	apply(ctx, t, s,
		costEvent(1, h1.Add(5*time.Minute), quadT1U1S1, "m", 1, 10, 10, 10),
		costEvent(2, h2.Add(5*time.Minute), quadT1U1S1, "m", 2, 10, 10, 10),
	)
	old, new, err = s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if !old.Equal(h1) || !new.Equal(h2) {
		t.Fatalf("retention = %v..%v; want %v..%v", old, new, h1, h2)
	}

	// Rebuild: everything clears, checkpoint resets to 0.
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("checkpoint after rebuild = %d, %v; want 0", ck, err)
	}
	res := mustQuery(ctx, t, s, h1.Add(-time.Hour), h2.Add(time.Hour), rollups.BucketHour)
	if len(res.Rows) != 0 {
		t.Fatalf("rows after rebuild = %d; want 0", len(res.Rows))
	}
	old, new, err = s.Retention(ctx)
	if err != nil || !old.IsZero() || !new.IsZero() {
		t.Fatalf("retention after rebuild = %v..%v, %v; want zero..zero", old, new, err)
	}

	// Fences clear too: a previously-fenced triple applies afresh.
	if err := s.FenceSession(ctx, quadT1U1S1.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if f, err := s.IsFenced(ctx, quadT1U1S1.Identity); err != nil || f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want false", f, err)
	}
}

func concurrentQueries(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.BucketHour)
	var evs []events.Event
	for i := 0; i < 40; i++ {
		evs = append(evs, costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Second), quadT1U1S1, "model-a", float64(i+1), 10, 10, 10))
	}
	apply(ctx, t, s, evs...)

	q := rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	expect := 820.0 // sum 1..40

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := s.Query(ctx, q)
			if err != nil {
				errs[idx] = err
				return
			}
			if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCostUSD] != expect {
				errs[idx] = fmt.Errorf("concurrent query: got rows=%d cost=%v want 1 row cost=%v",
					len(res.Rows), res.Rows[0].Measures[rollups.MeasureLLMCostUSD], expect)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent query %d: %v", i, err)
		}
	}
}

// --- helpers --------------------------------------------------------------

func sumCost(res rollups.Result) float64 {
	var total float64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCostUSD]
	}
	return total
}

func totalCost(res rollups.Result) float64 { return sumCost(res) }

func totalCompletions(res rollups.Result) float64 {
	var total float64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCompletions]
	}
	return total
}

func totalTasks(res rollups.Result, m rollups.Measure) float64 {
	var total float64
	for _, r := range res.Rows {
		total += r.Measures[m]
	}
	return total
}

func sumTasks(res rollups.Result, m rollups.Measure) float64 { return totalTasks(res, m) }
