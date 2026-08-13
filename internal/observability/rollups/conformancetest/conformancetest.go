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
	"math"
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
	t.Run("PrecisionExactIntegers", func(t *testing.T) { precisionExactIntegers(t, factory) })
	t.Run("CostExactnessMicros", func(t *testing.T) { costExactnessMicros(t, factory) })
	t.Run("UsageFieldsComplete", func(t *testing.T) { usageFieldsComplete(t, factory) })
	t.Run("LatencyMinMaxMerge", func(t *testing.T) { latencyMinMaxMerge(t, factory) })
	t.Run("LargeCounterExactness", func(t *testing.T) { largeCounterExactness(t, factory) })
	t.Run("FixedUTCBucketBoundaries", func(t *testing.T) { fixedUTCBucketBoundaries(t, factory) })
	t.Run("MinuteSeparationAndCoarsening", func(t *testing.T) { minuteSeparationAndCoarsening(t, factory) })
	t.Run("DimensionIsolation", func(t *testing.T) { dimensionIsolation(t, factory) })
	t.Run("QueryValidation", func(t *testing.T) { queryValidation(t, factory) })
	t.Run("AgentAndUnsupportedRejected", func(t *testing.T) { agentAndUnsupportedRejected(t, factory) })
	t.Run("DeterministicPagination", func(t *testing.T) { deterministicPagination(t, factory) })
	t.Run("CursorShapeBinding", func(t *testing.T) { cursorShapeBinding(t, factory) })
	t.Run("GroupBy", func(t *testing.T) { groupBy(t, factory) })
	t.Run("ErasureFencePermanent", func(t *testing.T) { erasureFencePermanent(t, factory) })
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

// costEvent builds a successfully-persisted `llm.cost.recorded` event with
// prompt/completion tokens (total = prompt+completion), a latency, and no
// reasoning/cache fields.
func costEvent(seq uint64, at time.Time, quad identity.Quadruple, model string, costUSD float64, prompt, completion, latencyMS int) events.Event {
	return costEventUsage(seq, at, quad, model, costUSD, llm.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		LatencyMS:        int64(latencyMS),
	})
}

// costEventUsage builds a `llm.cost.recorded` event carrying the full Usage
// shape (prompt/completion/reasoning/cache-read/cache-write/total + latency).
func costEventUsage(seq uint64, at time.Time, quad identity.Quadruple, model string, costUSD float64, usage llm.Usage) events.Event {
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity:   quad,
			Model:      model,
			Cost:       llm.Cost{TotalCost: costUSD, Currency: "USD"},
			Usage:      usage,
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
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions, rollups.MeasureTasksCompleted},
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

	h1 := rollups.BucketStart(anchor.Add(-3*time.Hour), rollups.StoreGranularity)
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
	// The window is hour-aligned: events sit in the hour bucket 09:00 on
	// Aug 13, so the query bounds are the hour floors around them.
	res := mustQuery(ctx, t, s,
		rollups.BucketStart(anchor.Add(-24*time.Hour), rollups.BucketHour),
		rollups.BucketStart(anchor, rollups.BucketHour).Add(time.Hour),
		rollups.BucketHour)
	if got := totalCost(res); got != 2_000_000 {
		t.Fatalf("cost after idempotent replay = %d micros; want 2_000_000 (1.25+0.75)", got)
	}
	if got := totalCompletions(res); got != 2 {
		t.Fatalf("completions after idempotent replay = %d; want 2", got)
	}
	if got := totalTasks(res, rollups.MeasureTasksCompleted); got != 1 {
		t.Fatalf("tasks_completed after idempotent replay = %d; want 1", got)
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

// precisionExactIntegers pins exact integer accumulation: costs that sum
// exactly in float64 must sum exactly in micro-units, and token/latency
// sums must be exact integers — no float64 anywhere.
func precisionExactIntegers(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	// Costs that sum exactly: 0.25+0.25+0.5+1.0+2.0 = 4.00 USD.
	var evs []events.Event
	for i, c := range []float64{0.25, 0.25, 0.5, 1.0, 2.0} {
		evs = append(evs, costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Minute), quadT1U1S1, "model-a", c, 10, 20, 100))
	}
	apply(ctx, t, s, evs...)

	// The window must be hour-aligned (h is on the minute grid; the hour
	// floor is the aligned edge).
	from := rollups.BucketStart(h, rollups.BucketHour)
	q := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions, rollups.MeasureLLMTokensPrompt, rollups.MeasureLLMLatencySumMS},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := totalCost(res); got != 4_000_000 {
		t.Fatalf("cost sum = %d micros; want 4_000_000 (exact)", got)
	}
	if got := totalCompletions(res); got != 5 {
		t.Fatalf("completions = %d; want 5", got)
	}
	// Prompt 5×10=50, completion 5×20=100, latency 5×100=500.
	row := res.Rows[0]
	if got := row.Measures[rollups.MeasureLLMTokensPrompt].N; got != 50 {
		t.Fatalf("prompt tokens = %d; want 50", got)
	}
	if got := row.Measures[rollups.MeasureLLMCompletions].N; got != 5 {
		t.Fatalf("completions measure = %d; want 5", got)
	}
	if got := row.Measures[rollups.MeasureLLMLatencySumMS].N; got != 500 {
		t.Fatalf("latency ms = %d; want 500", got)
	}
}

// costExactnessMicros pins the integer micro-unit cost representation: the
// classic 0.1+0.2 float artifact must NOT appear, and invalid source costs
// (NaN / ±Inf / negative / out-of-int64-range) must fail loudly at Extract.
func costExactnessMicros(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	// 0.1 + 0.2 + 0.30000000000000004 (the float64 of 0.1+0.2) must sum to
	// EXACTLY 600_000 micro-units — no accumulated float drift.
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "m", 0.1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S1, "m", 0.2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quadT1U1S1, "m", 0.1+0.2, 10, 10, 10),
	}
	apply(ctx, t, s, evs...)
	from := rollups.BucketStart(h, rollups.BucketHour)
	res := mustQuery(ctx, t, s, from, from.Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 600_000 {
		t.Fatalf("0.1+0.2+(0.1+0.2) = %d micros; want 600_000 (0.60 USD exactly)", got)
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].Scale; got != rollups.CostScaleMicros {
		t.Fatalf("cost scale = %d; want %d", got, rollups.CostScaleMicros)
	}

	// Invalid source costs fail loudly with ErrInvalidCost — a corrupted
	// log is never silently undercounted.
	bad := []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		-0.01,
		-1.0,
		// Beyond int64 micro range (MaxInt64 / 1e6 ≈ 9.22e12 USD).
		1e13,
	}
	for i, c := range bad {
		ev := costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Minute), quadT1U1S1, "m", c, 10, 10, 10)
		if _, err := rollups.Extract(ev); !errors.Is(err, rollups.ErrInvalidCost) {
			t.Fatalf("Extract(cost=%v) err = %v; want ErrInvalidCost", c, err)
		}
	}
}

// usageFieldsComplete pins the COMPLETE source-backed measure set from
// llm.cost.recorded: the successful-completion count, every token field
// (prompt/completion/reasoning/cache-read/cache-write/total), precise cost
// micro-units, and latency count/sum/min/max.
func usageFieldsComplete(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	ev := costEventUsage(1, h, quadT1U1S1, "model-x", 0.123456789,
		llm.Usage{
			PromptTokens:     1000,
			CompletionTokens: 500,
			ReasoningTokens:  50,
			CacheReadTokens:  800,
			CacheWriteTokens: 150,
			TotalTokens:      1550,
			LatencyMS:        812,
		})
	apply(ctx, t, s, ev)

	from := rollups.BucketStart(h, rollups.BucketHour)
	q := rollups.Query{
		From:   from,
		To:     from.Add(time.Hour),
		Bucket: rollups.BucketHour,
		Measures: []rollups.Measure{
			rollups.MeasureLLMCompletions,
			rollups.MeasureLLMTokensPrompt,
			rollups.MeasureLLMTokensCompletion,
			rollups.MeasureLLMTokensReasoning,
			rollups.MeasureLLMTokensCacheRead,
			rollups.MeasureLLMTokensCacheWrite,
			rollups.MeasureLLMTokensTotal,
			rollups.MeasureLLMCostMicros,
			rollups.MeasureLLMLatencyCount,
			rollups.MeasureLLMLatencySumMS,
			rollups.MeasureLLMLatencyMinMS,
			rollups.MeasureLLMLatencyMaxMS,
		},
		Sort:  rollups.SortKeyBucketAsc,
		Limit: 100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(res.Rows))
	}
	m := res.Rows[0].Measures
	want := map[rollups.Measure]int64{
		rollups.MeasureLLMCompletions:      1,
		rollups.MeasureLLMTokensPrompt:     1000,
		rollups.MeasureLLMTokensCompletion: 500,
		rollups.MeasureLLMTokensReasoning:  50,
		rollups.MeasureLLMTokensCacheRead:  800,
		rollups.MeasureLLMTokensCacheWrite: 150,
		rollups.MeasureLLMTokensTotal:      1550,
		rollups.MeasureLLMCostMicros:       123_457, // round(0.123456789 * 1e6)
		rollups.MeasureLLMLatencyCount:     1,
		rollups.MeasureLLMLatencySumMS:     812,
		rollups.MeasureLLMLatencyMinMS:     812,
		rollups.MeasureLLMLatencyMaxMS:     812,
	}
	for measure, wantN := range want {
		got := m[measure].N
		if got != wantN {
			t.Fatalf("measure %q = %d; want %d", measure, got, wantN)
		}
	}
}

// latencyMinMaxMerge pins the fold semantics of the latency min/max measures
// across multiple events and across a mixed cost/task group.
func latencyMinMaxMerge(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "m", 1, 10, 10, 100),
		costEvent(2, h.Add(time.Minute), quadT1U1S1, "m", 1, 10, 10, 50),
		costEvent(3, h.Add(2*time.Minute), quadT1U1S1, "m", 1, 10, 10, 200),
		costEvent(4, h.Add(3*time.Minute), quadT1U1S1, "m", 1, 10, 10, 50),
		taskEvent(5, h.Add(4*time.Minute), quadT1U1S1, tasks.EventTypeTaskCompleted),
	}
	apply(ctx, t, s, evs...)

	from := rollups.BucketStart(h, rollups.BucketHour)
	q := rollups.Query{
		From:   from,
		To:     from.Add(time.Hour),
		Bucket: rollups.BucketHour,
		Measures: []rollups.Measure{
			rollups.MeasureLLMLatencyCount,
			rollups.MeasureLLMLatencySumMS,
			rollups.MeasureLLMLatencyMinMS,
			rollups.MeasureLLMLatencyMaxMS,
			rollups.MeasureLLMCompletions,
			rollups.MeasureTasksCompleted,
		},
		Sort:  rollups.SortKeyBucketAsc,
		Limit: 100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(res.Rows))
	}
	m := res.Rows[0].Measures
	if got := m[rollups.MeasureLLMLatencyCount].N; got != 4 {
		t.Fatalf("latency count = %d; want 4 (the task event carries no latency)", got)
	}
	if got := m[rollups.MeasureLLMLatencySumMS].N; got != 400 {
		t.Fatalf("latency sum = %d; want 400 (100+50+200+50)", got)
	}
	if got := m[rollups.MeasureLLMLatencyMinMS].N; got != 50 {
		t.Fatalf("latency min = %d; want 50", got)
	}
	if got := m[rollups.MeasureLLMLatencyMaxMS].N; got != 200 {
		t.Fatalf("latency max = %d; want 200", got)
	}
	if got := m[rollups.MeasureLLMCompletions].N; got != 4 {
		t.Fatalf("completions = %d; want 4", got)
	}
	if got := m[rollups.MeasureTasksCompleted].N; got != 1 {
		t.Fatalf("tasks_completed = %d; want 1", got)
	}
}

// largeCounterExactness pins the >2^53 counter guarantee: result measure
// values are exact int64 — never normalised to float64, which would lose the
// low bits above 2^53.
func largeCounterExactness(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	big := int64(1<<53) + 1 // 9_007_199_254_740_993 — float64 cannot represent +1 here
	delta := rollups.Delta{
		Key: rollups.Key{BucketStart: h, TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1", Model: "model-a"},
		Add: rollups.MeasureSet{LLMTokensTotal: big},
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 1, Deltas: []rollups.Delta{delta}}); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	from := rollups.BucketStart(h, rollups.BucketHour)
	q := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMTokensTotal},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(res.Rows))
	}
	got := res.Rows[0].Measures[rollups.MeasureLLMTokensTotal].N
	if got != big {
		t.Fatalf("counter = %d; want %d (exact — float64 would lose the low bit)", got, big)
	}
	// The trap this guards against: a float64 round-trip cannot hold the +1.
	if int64(float64(big)) == big {
		t.Fatalf("test fixture broken: %d round-trips through float64", big)
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
		costEvent(1, hourEdge.Add(-time.Nanosecond), quadT1U1S1, "m", 1, 10, 10, 10), // 11:59:59.999… → minute 11:59
		costEvent(2, hourEdge, quadT1U1S1, "m", 2, 10, 10, 10),                       // 12:00:00 → minute 12:00
		costEvent(3, dayEdge.Add(-time.Nanosecond), quadT1U1S1, "m", 4, 10, 10, 10),  // 23:59:59.999… → minute 23:59
		costEvent(4, dayEdge, quadT1U1S1, "m", 8, 10, 10, 10),                        // 00:00:00 → minute 00:00 Aug 14
	}
	apply(ctx, t, s, evs...)

	// Hour granularity over Aug 13: buckets 11:00 (event 1), 12:00
	// (event 2), and 23:00 (event 3 — still Aug 13). Event 4 opens
	// Aug 14 and is excluded by the half-open window.
	res := mustQuery(ctx, t, s, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), rollups.BucketHour)
	if len(res.Rows) != 3 {
		t.Fatalf("hour rows = %d; want 3 (buckets 11:00, 12:00, 23:00)", len(res.Rows))
	}
	if got := totalCost(res); got != 7_000_000 {
		t.Fatalf("hour-window cost = %d micros; want 7_000_000 (events 1+2+3)", got)
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
	if got := totalCost(res); got != 15_000_000 {
		t.Fatalf("day-window cost = %d micros; want 15_000_000 (events 1+2+3+4)", got)
	}
	if res.Rows[0].BucketStart.Day() != 13 || res.Rows[1].BucketStart.Day() != 14 {
		t.Fatalf("day bucket starts = day %d, day %d; want 13, 14", res.Rows[0].BucketStart.Day(), res.Rows[1].BucketStart.Day())
	}
}

// minuteSeparationAndCoarsening pins the corrected storage contract: rows
// live on the fixed UTC MINUTE grid (never hour), so events in the same hour
// but different minutes produce separate minute rows, coarsen into one hour
// row at hour granularity, and one day row at day granularity — with the
// window filtering minute rows.
func minuteSeparationAndCoarsening(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	day := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	evs := []events.Event{
		costEvent(1, day.Add(12*time.Hour+1*time.Minute+30*time.Second), quadT1U1S1, "m", 1, 10, 10, 10),  // 12:01:30 → minute 12:01
		costEvent(2, day.Add(12*time.Hour+34*time.Minute+56*time.Second), quadT1U1S1, "m", 2, 10, 10, 10), // 12:34:56 → minute 12:34
		costEvent(3, day.Add(12*time.Hour+59*time.Minute+0*time.Second), quadT1U1S1, "m", 4, 10, 10, 10),  // 12:59:00 → minute 12:59
		costEvent(4, day.Add(13*time.Hour+2*time.Minute), quadT1U1S1, "m", 8, 10, 10, 10),                 // 13:02:00 → minute 13:02
	}
	apply(ctx, t, s, evs...)

	// Minute granularity over [12:00, 13:00): three SEPARATE minute rows —
	// the same hour does not collapse them.
	res := mustQuery(ctx, t, s, day.Add(12*time.Hour), day.Add(13*time.Hour), rollups.BucketMinute)
	if len(res.Rows) != 3 {
		t.Fatalf("minute rows = %d; want 3 (12:01, 12:34, 12:59)", len(res.Rows))
	}
	if res.Rows[0].BucketStart.Minute() != 1 || res.Rows[1].BucketStart.Minute() != 34 || res.Rows[2].BucketStart.Minute() != 59 {
		t.Fatalf("minute bucket starts = %d, %d, %d; want 1, 34, 59",
			res.Rows[0].BucketStart.Minute(), res.Rows[1].BucketStart.Minute(), res.Rows[2].BucketStart.Minute())
	}
	if got := totalCost(res); got != 7_000_000 {
		t.Fatalf("minute-window cost = %d micros; want 7_000_000 (1+2+4)", got)
	}

	// A sub-hour minute window filters the minute rows.
	res = mustQuery(ctx, t, s, day.Add(12*time.Hour), day.Add(12*time.Hour+30*time.Minute), rollups.BucketMinute)
	if len(res.Rows) != 1 {
		t.Fatalf("sub-hour minute rows = %d; want 1 (only minute 12:01 in [12:00, 12:30))", len(res.Rows))
	}
	if got := totalCost(res); got != 1_000_000 {
		t.Fatalf("sub-hour cost = %d micros; want 1_000_000", got)
	}

	// Hour granularity over [12:00, 14:00): the three 12:xx minutes coarsen
	// into ONE hour-12 row; 13:02 is its own hour-13 row.
	res = mustQuery(ctx, t, s, day.Add(12*time.Hour), day.Add(14*time.Hour), rollups.BucketHour)
	if len(res.Rows) != 2 {
		t.Fatalf("hour rows = %d; want 2 (12:00, 13:00)", len(res.Rows))
	}
	if res.Rows[0].BucketStart.Hour() != 12 || res.Rows[1].BucketStart.Hour() != 13 {
		t.Fatalf("hour bucket starts = %d, %d; want 12, 13", res.Rows[0].BucketStart.Hour(), res.Rows[1].BucketStart.Hour())
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N; got != 7_000_000 {
		t.Fatalf("hour-12 cost = %d micros; want 7_000_000 (1+2+4)", got)
	}
	if got := res.Rows[1].Measures[rollups.MeasureLLMCostMicros].N; got != 8_000_000 {
		t.Fatalf("hour-13 cost = %d micros; want 8_000_000", got)
	}

	// Day granularity over Aug 13: all four coarsen into one day row.
	res = mustQuery(ctx, t, s, day, day.Add(24*time.Hour), rollups.BucketDay)
	if len(res.Rows) != 1 {
		t.Fatalf("day rows = %d; want 1", len(res.Rows))
	}
	if got := totalCost(res); got != 15_000_000 {
		t.Fatalf("day cost = %d micros; want 15_000_000", got)
	}
}

func dimensionIsolation(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S2, "model-a", 2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quadT1U2S3, "model-b", 4, 10, 10, 10),
		costEvent(4, h.Add(3*time.Minute), quadT2U1S1, "model-b", 8, 10, 10, 10),
	}
	apply(ctx, t, s, evs...)

	from, to := rollups.BucketStart(h, rollups.BucketHour), rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour)

	// Whole window: all four.
	res := mustQuery(ctx, t, s, from, to, rollups.BucketHour)
	if got := totalCost(res); got != 15_000_000 {
		t.Fatalf("all-tenant cost = %d micros; want 15_000_000", got)
	}

	// Tenant isolation: tenant-a only sees its own rows.
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-a"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	res, err := s.Query(ctx, q)
	if err != nil {
		t.Fatalf("tenant-a query: %v", err)
	}
	if got := sumCost(res); got != 7_000_000 {
		t.Fatalf("tenant-a cost = %d micros; want 7_000_000 (1+2+4)", got)
	}

	// Session isolation within a tenant.
	q.Filter = rollups.Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"session-2"}}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("session-2 query: %v", err)
	}
	if got := sumCost(res); got != 2_000_000 {
		t.Fatalf("session-2 cost = %d micros; want 2_000_000", got)
	}

	// Model isolation: model-a only.
	q.Filter = rollups.Filter{Models: []string{"model-a"}}
	res, err = s.Query(ctx, q)
	if err != nil {
		t.Fatalf("model-a query: %v", err)
	}
	if got := sumCost(res); got != 3_000_000 {
		t.Fatalf("model-a cost = %d micros; want 3_000_000 (1+2)", got)
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
		t.Fatalf("un-attributed tasks = %d; want 1", got)
	}
}

func queryValidation(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	apply(ctx, t, s, costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10))

	// The base window is hour- AND minute-aligned (the hour floor), so the
	// budget mutations below stay on their grid.
	from := rollups.BucketStart(h, rollups.BucketHour)
	base := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
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
			q.Measures = []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCostMicros}
		}, rollups.ErrQueryInvalid},
		{"unknown sort", func(q *rollups.Query) { q.Sort = "cost" }, rollups.ErrQueryInvalid},
		{"measure sort without measure", func(q *rollups.Query) { q.Sort = rollups.SortKeyMeasureDesc }, rollups.ErrQueryInvalid},
		{"zero limit", func(q *rollups.Query) { q.Limit = 0 }, rollups.ErrQueryInvalid},
		{"negative limit", func(q *rollups.Query) { q.Limit = -5 }, rollups.ErrQueryInvalid},
		{"limit over budget", func(q *rollups.Query) { q.Limit = rollups.MaxRowsPerQuery + 1 }, rollups.ErrQueryBudget},
		{"hour bucket budget", func(q *rollups.Query) { q.From = from.Add(-time.Duration(rollups.MaxBuckets+1) * time.Hour) }, rollups.ErrQueryBudget},
		{"minute bucket budget", func(q *rollups.Query) {
			q.Bucket = rollups.BucketMinute
			q.From = from.Add(-time.Duration(rollups.MaxBuckets+1) * time.Minute)
		}, rollups.ErrQueryBudget},
		{"unaligned second from", func(q *rollups.Query) { q.From = from.Add(time.Second) }, rollups.ErrQueryInvalid},
		{"unaligned nano from", func(q *rollups.Query) { q.From = from.Add(time.Nanosecond) }, rollups.ErrQueryInvalid},
		{"unaligned second to", func(q *rollups.Query) { q.To = from.Add(time.Hour).Add(-time.Second) }, rollups.ErrQueryInvalid},
		{"unaligned nano to", func(q *rollups.Query) { q.To = from.Add(time.Hour).Add(-time.Nanosecond) }, rollups.ErrQueryInvalid},
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

// agentAndUnsupportedRejected pins the corrected closed sets: agent is NOT a
// rollup dimension (even as an empty axis) and attempts / failed-call /
// user-message measures have no canonical source and are absent.
func agentAndUnsupportedRejected(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	apply(ctx, t, s, costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10))

	base := rollups.Query{
		From:     rollups.BucketStart(h, rollups.BucketHour),
		To:       rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}

	// An agent group_by — the axis does not exist in this release — must be
	// rejected loudly, never silently treated as an empty axis.
	for _, dim := range []rollups.Dimension{"agent", "agent_id", "run"} {
		q := base
		q.GroupBy = []rollups.Dimension{dim}
		if _, err := s.Query(ctx, q); !errors.Is(err, rollups.ErrQueryInvalid) {
			t.Fatalf("GroupBy=%q err = %v; want ErrQueryInvalid", dim, err)
		}
	}

	// Measures with no canonical payload backing are absent: attempts,
	// failed LLM calls, and user-message counts cannot be requested.
	for _, m := range []rollups.Measure{"llm_attempts", "llm_failed_calls", "user_messages", "llm_agent_cost"} {
		q := base
		q.Measures = []rollups.Measure{m}
		if _, err := s.Query(ctx, q); !errors.Is(err, rollups.ErrQueryInvalid) {
			t.Fatalf("Measures=%q err = %v; want ErrQueryInvalid", m, err)
		}
	}
}

func deterministicPagination(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	// 3 sessions × 3 hours, one cost event per (session, hour). h0 is the
	// HOUR floor so the events land exactly on hour boundaries and the
	// query window [h0, h0+3h) is hour-aligned.
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
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
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
		Measures:    []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:        rollups.SortKeyMeasureDesc,
		SortMeasure: rollups.MeasureLLMCostMicros,
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
		prev := mres.Rows[i-1].Measures[rollups.MeasureLLMCostMicros].N
		cur := mres.Rows[i].Measures[rollups.MeasureLLMCostMicros].N
		if prev < cur {
			t.Fatalf("measure-desc order broken at %d: %d then %d", i, prev, cur)
		}
	}

	// A cursor produced by a DIFFERENT query shape (different GroupBy) is
	// rejected loudly, never silently mis-paginated.
	tenantGrouped := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		GroupBy:  []rollups.Dimension{rollups.DimensionTenant},
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
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

// cursorShapeBinding pins the complete canonical-shape binding of a page
// cursor: a cursor produced by one query is rejected with ErrBadCursor when
// reused under a query that differs in ANY shape field — window, bucket,
// every filter axis, measures, group-by, sort, or sort-measure — and is
// accepted when the shape is identical (including a different page Limit,
// which is deliberately not part of the shape).
func cursorShapeBinding(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	// 3 sessions × 3 hour buckets, one cost event per (session, bucket):
	// 9 rows so Limit 2 forces real pages and a non-empty NextCursor.
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

	base := rollups.Query{
		From:     h0,
		To:       h0.Add(3 * time.Hour),
		Bucket:   rollups.BucketHour,
		GroupBy:  []rollups.Dimension{rollups.DimensionSession},
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    2,
	}
	first, err := s.Query(ctx, base)
	if err != nil {
		t.Fatalf("base page 1: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("base query with 9 rows and Limit 2 must produce a next cursor")
	}

	// Reusing the base cursor under a query that differs in ONE shape
	// field must fail typed — the cursor is bound to the shape that
	// produced it and is never silently re-purposed. Every mutation is
	// itself a VALID query, so the failure is the shape binding, not
	// validation.
	cases := []struct {
		name string
		mut  func(*rollups.Query)
	}{
		{"window", func(q *rollups.Query) { q.From = q.From.Add(time.Hour); q.To = q.To.Add(time.Hour) }},
		{"bucket", func(q *rollups.Query) { q.Bucket = rollups.BucketMinute }},
		{"filter tenant", func(q *rollups.Query) { q.Filter = rollups.Filter{TenantIDs: []string{"tenant-a"}} }},
		{"filter user", func(q *rollups.Query) { q.Filter = rollups.Filter{UserIDs: []string{"user-1"}} }},
		{"filter session", func(q *rollups.Query) { q.Filter = rollups.Filter{SessionIDs: []string{"session-1"}} }},
		{"filter model", func(q *rollups.Query) { q.Filter = rollups.Filter{Models: []string{"model-b"}} }},
		{"measures", func(q *rollups.Query) { q.Measures = append(q.Measures, rollups.MeasureLLMCompletions) }},
		{"group by", func(q *rollups.Query) { q.GroupBy = []rollups.Dimension{rollups.DimensionTenant} }},
		{"sort", func(q *rollups.Query) { q.Sort = rollups.SortKeyBucketDesc }},
		{"sort measure", func(q *rollups.Query) {
			q.Sort = rollups.SortKeyMeasureAsc
			q.SortMeasure = rollups.MeasureLLMCompletions
		}},
	}
	for _, tc := range cases {
		t.Run("mismatch "+tc.name, func(t *testing.T) {
			q := base
			q.Cursor = first.NextCursor
			tc.mut(&q)
			if _, err := s.Query(ctx, q); !errors.Is(err, rollups.ErrBadCursor) {
				t.Fatalf("cursor reused across %s: err=%v; want ErrBadCursor", tc.name, err)
			}
		})
	}

	// The declared continuation contract: the SAME shape with a different
	// page Limit may continue — Limit is deliberately not part of the
	// shape.
	continued := base
	continued.Cursor = first.NextCursor
	continued.Limit = 5
	res, err := s.Query(ctx, continued)
	if err != nil {
		t.Fatalf("same-shape different-limit continuation: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("same-shape continuation returned no rows")
	}

	// The effective-sort normalisation: a cursor produced under the empty
	// default continues under an explicitly-set SortKeyBucketAsc, and a
	// cursor produced under the explicit sort continues under the default.
	explicit := base
	explicit.Sort = rollups.SortKeyBucketAsc
	p2, err := s.Query(ctx, explicit)
	if err != nil {
		t.Fatalf("explicit-sort page: %v", err)
	}
	if p2.NextCursor == "" {
		t.Fatal("explicit-sort query must produce a next cursor")
	}
	viaDefault := explicit
	viaDefault.Sort = "" // the effective default
	viaDefault.Cursor = p2.NextCursor
	if _, err := s.Query(ctx, viaDefault); err != nil {
		t.Fatalf("cursor from explicit sort must continue under the empty-sort default: %v", err)
	}
}

func groupBy(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	evs := []events.Event{
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S1, "model-b", 2, 10, 10, 10),
		costEvent(3, h.Add(2*time.Minute), quadT1U1S2, "model-a", 4, 10, 10, 10),
	}
	apply(ctx, t, s, evs...)

	from, to := rollups.BucketStart(h, rollups.BucketHour), rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour)

	// No GroupBy: one row per bucket over the whole window.
	q := rollups.Query{
		From:     from,
		To:       to,
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
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
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N; got != 7_000_000 {
		t.Fatalf("no-groupby cost = %d micros; want 7_000_000", got)
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
			if got := r.Measures[rollups.MeasureLLMCostMicros].N; got != 5_000_000 {
				t.Fatalf("model-a group cost = %d micros; want 5_000_000", got)
			}
		case "model-b":
			if got := r.Measures[rollups.MeasureLLMCostMicros].N; got != 2_000_000 {
				t.Fatalf("model-b group cost = %d micros; want 2_000_000", got)
			}
		default:
			t.Fatalf("unexpected group model %q", r.Dimensions[rollups.DimensionModel])
		}
	}
}

// erasureFencePermanent pins the corrected erasure contract: the fence is
// PERMANENT — there is no unfence, and Rebuild (reprojection) never clears
// it, so an erased session cannot be resurrected by a late event or by a
// rebuild-then-replay cycle.
func erasureFencePermanent(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
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
	res := mustQuery(ctx, t, s, rollups.BucketStart(h, rollups.BucketHour), rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 2_000_000 {
		t.Fatalf("cost after fence = %d micros; want 2_000_000 (session-2 only)", got)
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
	res = mustQuery(ctx, t, s, rollups.BucketStart(h, rollups.BucketHour), rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 2_000_000 {
		t.Fatalf("cost after refused late event = %d micros; want 2_000_000 (no resurrection)", got)
	}

	// Rebuild clears rows + checkpoint but MUST NOT clear the fence: the
	// erased session stays erased through reprojection.
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if f, err := s.IsFenced(ctx, triple); err != nil || !f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want true (fences are permanent)", f, err)
	}

	// Replay the log with the projector discipline (fenced triples dropped
	// at ingestion): session-2's rows are reconstructed, session-1's must
	// stay absent forever.
	var survivorDeltas []rollups.Delta
	for _, ev := range []events.Event{
		costEvent(1, h, quadT1U1S1, "model-a", 1, 10, 10, 10),
		costEvent(2, h.Add(time.Minute), quadT1U1S2, "model-a", 2, 10, 10, 10),
	} {
		fenced, err := s.IsFenced(ctx, ev.Identity.Identity)
		if err != nil {
			t.Fatalf("IsFenced(replay): %v", err)
		}
		if fenced {
			continue
		}
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract(replay): %v", err)
		}
		survivorDeltas = append(survivorDeltas, ds...)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 2, Deltas: survivorDeltas}); err != nil {
		t.Fatalf("replay ApplyBatch: %v", err)
	}
	res = mustQuery(ctx, t, s, rollups.BucketStart(h, rollups.BucketHour), rollups.BucketStart(h, rollups.BucketHour).Add(time.Hour), rollups.BucketHour)
	if got := totalCost(res); got != 2_000_000 {
		t.Fatalf("cost after rebuild+replay = %d micros; want 2_000_000 (session-1 never resurrected)", got)
	}

	// A direct apply of a fenced triple's delta is STILL refused after the
	// rebuild+replay cycle.
	late2 := costEvent(3, h.Add(3*time.Minute), quadT1U1S1, "model-a", 100, 10, 10, 10)
	deltas2, err := rollups.Extract(late2)
	if err != nil {
		t.Fatalf("Extract(late2): %v", err)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: 3, Deltas: deltas2}); !errors.Is(err, rollups.ErrSessionFenced) {
		t.Fatalf("post-rebuild late apply err = %v; want ErrSessionFenced", err)
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

	// Retention reports the row-level (MINUTE grid) bucket starts of the
	// actual stored events — not a coarser floor.
	ev1At := anchor.Add(-2 * time.Hour).Add(5 * time.Minute)
	ev2At := anchor.Add(5 * time.Minute)
	h1 := rollups.BucketStart(ev1At, rollups.StoreGranularity)
	h2 := rollups.BucketStart(ev2At, rollups.StoreGranularity)
	apply(ctx, t, s,
		costEvent(1, ev1At, quadT1U1S1, "m", 1, 10, 10, 10),
		costEvent(2, ev2At, quadT1U1S1, "m", 2, 10, 10, 10),
	)
	old, new, err = s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if !old.Equal(h1) || !new.Equal(h2) {
		t.Fatalf("retention = %v..%v; want %v..%v", old, new, h1, h2)
	}

	// Fence a session BEFORE the rebuild; the fence must survive it.
	if err := s.FenceSession(ctx, quadT1U1S1.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}

	// Rebuild: rows clear, checkpoint resets to 0, fences stay.
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if ck, err := s.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("checkpoint after rebuild = %d, %v; want 0", ck, err)
	}
	// An hour-aligned window over the (now empty) store: no rows.
	res := mustQuery(ctx, t, s,
		rollups.BucketStart(h1, rollups.BucketHour),
		rollups.BucketStart(h2, rollups.BucketHour).Add(time.Hour),
		rollups.BucketHour)
	if len(res.Rows) != 0 {
		t.Fatalf("rows after rebuild = %d; want 0", len(res.Rows))
	}
	old, new, err = s.Retention(ctx)
	if err != nil || !old.IsZero() || !new.IsZero() {
		t.Fatalf("retention after rebuild = %v..%v, %v; want zero..zero", old, new, err)
	}
	if f, err := s.IsFenced(ctx, quadT1U1S1.Identity); err != nil || !f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want true (erasure fences are permanent)", f, err)
	}
}

func concurrentQueries(t *testing.T, factory Factory) {
	ctx := context.Background()
	s, cleanup := factory()
	defer cleanup()

	h := rollups.BucketStart(anchor, rollups.StoreGranularity)
	var evs []events.Event
	for i := 0; i < 40; i++ {
		evs = append(evs, costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Second), quadT1U1S1, "model-a", float64(i+1), 10, 10, 10))
	}
	apply(ctx, t, s, evs...)

	from := rollups.BucketStart(h, rollups.BucketHour)
	q := rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	}
	expect := int64(820) * 1_000_000 // sum 1..40 = 820 USD

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
			if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N != expect {
				errs[idx] = fmt.Errorf("concurrent query: got rows=%d cost=%d want 1 row cost=%d",
					len(res.Rows), res.Rows[0].Measures[rollups.MeasureLLMCostMicros].N, expect)
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

// sumCost returns the exact total cost in micro-units across the rows.
func sumCost(res rollups.Result) int64 {
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCostMicros].N
	}
	return total
}

func totalCost(res rollups.Result) int64 { return sumCost(res) }

func totalCompletions(res rollups.Result) int64 {
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCompletions].N
	}
	return total
}

func totalTasks(res rollups.Result, m rollups.Measure) int64 {
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[m].N
	}
	return total
}

func sumTasks(res rollups.Result, m rollups.Measure) int64 { return totalTasks(res, m) }
