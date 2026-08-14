package postgres_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/conformancetest"
	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestPostgres_Conformance runs the canonical rollups.Store conformance
// suite against a Postgres connection — the same suite every V1 driver
// must pass. The test gates on HARBOR_PG_DSN: locally without Postgres
// available the test skips cleanly; CI provides a postgres:16 service
// container.
func TestPostgres_Conformance(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	conformancetest.Run(t, func() (rollups.Store, func()) {
		// Each conformance subtest gets its own driver instance so state
		// from one subtest can't bleed into another. We share a single
		// schema across subtests and reset between them so the per-test
		// setup cost stays bounded.
		s, err := postgres.New(postgres.Config{DSN: dsn})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		resetStore(t, dsn)
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestPostgres_RestartPrecisionErasure pins the driver-level durability
// and precision contract across a driver restart (the projector's
// restart catch-up surface): the checkpoint, the exact integer rows
// (including counters float64 cannot represent and micro-unit costs that
// must not accumulate float drift), and the PERMANENT erasure fences all
// survive a Close → New cycle on the same schema. The conformance suite
// covers each property within one lifetime; this test proves them across
// the restart boundary.
func TestPostgres_RestartPrecisionErasure(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()

	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	survivor := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-survivor"}
	erased := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-erased"}

	// --- first lifetime ---
	s1, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	// Cost records whose float64 sum is exactly 0.30 USD (0.1 + 0.2) plus
	// a large token counter above 2^53 and a task completion.
	var evs []events.Event
	for i, c := range []float64{0.1, 0.2} {
		evs = append(evs, costEvent(uint64(i+1), h.Add(time.Duration(i)*time.Minute), identity.Quadruple{Identity: survivor}, "model-a", c))
	}
	evs = append(evs, taskEvent(3, h.Add(2*time.Minute), identity.Quadruple{Identity: survivor}, tasks.EventTypeTaskCompleted))
	evs = append(evs, costEvent(4, h.Add(3*time.Minute), identity.Quadruple{Identity: erased}, "model-b", 1.5))
	// A hand-built large-counter delta (float64 cannot represent 1<<53+1).
	big := int64(1<<53) + 1
	evs = append(evs, bigCounterEvent(5, h.Add(4*time.Minute), survivor, big))
	applyEvents(t, ctx, s1, evs...)

	if err := s1.FenceSession(ctx, erased); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if ck, err := s1.Checkpoint(ctx); err != nil || ck != 5 {
		t.Fatalf("checkpoint before restart = %d, %v; want 5", ck, err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s1.Checkpoint(ctx); !errors.Is(err, rollups.ErrClosed) {
		t.Fatalf("post-close Checkpoint err = %v; want ErrClosed", err)
	}

	// --- second lifetime (restart) ---
	s2, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	if ck, err := s2.Checkpoint(ctx); err != nil || ck != 5 {
		t.Fatalf("checkpoint after restart = %d, %v; want 5 (durable across restart)", ck, err)
	}

	// Precision survives restart: the erased session's rows are gone, the
	// survivor's cost is exactly 300_000 micros (0.1 + 0.2, no float
	// drift), one task completion landed, and the big counter is exact.
	from := rollups.BucketStart(h, rollups.BucketHour)
	res, err := s2.Query(ctx, rollups.Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMTokensTotal, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("post-restart Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("post-restart rows = %d; want 1 (only the survivor, the erased rows are gone)", len(res.Rows))
	}
	m := res.Rows[0].Measures
	if got := m[rollups.MeasureLLMCostMicros].N; got != 300_000 {
		t.Fatalf("post-restart cost = %d micros; want 300_000 (0.1+0.2 exactly)", got)
	}
	if got := m[rollups.MeasureLLMTokensTotal].N; got != big {
		t.Fatalf("post-restart tokens_total = %d; want %d (exact above 2^53 — float64 would lose the low bit)", got, big)
	}
	if got := m[rollups.MeasureTasksCompleted].N; got != 1 {
		t.Fatalf("post-restart tasks_completed = %d; want 1", got)
	}
	if got := m[rollups.MeasureLLMCostMicros].Scale; got != rollups.CostScaleMicros {
		t.Fatalf("post-restart cost scale = %d; want %d", got, rollups.CostScaleMicros)
	}

	// The erasure fence survives restart and still prevents resurrection.
	if f, err := s2.IsFenced(ctx, erased); err != nil || !f {
		t.Fatalf("IsFenced(erased) after restart = %v, %v; want true", f, err)
	}
	late := costEvent(6, h.Add(5*time.Minute), identity.Quadruple{Identity: erased}, "model-b", 99)
	deltas, err := rollups.Extract(late)
	if err != nil {
		t.Fatalf("Extract(late): %v", err)
	}
	err = s2.ApplyBatch(ctx, rollups.Batch{Checkpoint: 6, Deltas: deltas})
	if !errors.Is(err, rollups.ErrSessionFenced) {
		t.Fatalf("late apply after restart err = %v; want ErrSessionFenced (the fence is permanent)", err)
	}
	if ck, err := s2.Checkpoint(ctx); err != nil || ck != 5 {
		t.Fatalf("checkpoint after refused late apply = %d, %v; want 5", ck, err)
	}

	// Rebuild clears rows + checkpoint but NEVER the fence.
	if err := s2.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if f, err := s2.IsFenced(ctx, erased); err != nil || !f {
		t.Fatalf("IsFenced(erased) after rebuild = %v, %v; want true (fences are permanent)", f, err)
	}
	if ck, err := s2.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("checkpoint after rebuild = %d, %v; want 0", ck, err)
	}
}

// TestPostgres_New_RequiresDSN pins the explicit-DSN-required contract.
// Empty DSN must surface a clear error rather than panic inside sql.Open.
func TestPostgres_New_RequiresDSN(t *testing.T) {
	_, err := postgres.New(postgres.Config{DSN: ""})
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}

// TestPostgres_ErrInvalidCost_FlowThrough pins that an invalid source cost
// fails loudly at Extract with the canonical sentinel before the store is
// ever touched (a corrupted log is never silently undercounted, on any
// driver).
func TestPostgres_ErrInvalidCost_FlowThrough(t *testing.T) {
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	bad := costEvent(1, h, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "m", math.NaN())
	if _, err := rollups.Extract(bad); !errors.Is(err, rollups.ErrInvalidCost) {
		t.Fatalf("Extract(NaN cost) err = %v; want ErrInvalidCost", err)
	}
}

// --- local fixtures ------------------------------------------------------

// costEvent builds a `llm.cost.recorded` event with fixed 10-token / 10ms
// usage — the fixture only varies the cost, which is what the driver-level
// tests assert on.
func costEvent(seq uint64, at time.Time, quad identity.Quadruple, model string, costUSD float64) events.Event {
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity:   quad,
			Model:      model,
			Cost:       llm.Cost{TotalCost: costUSD, Currency: "USD"},
			Usage:      llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20, LatencyMS: 10},
			OccurredAt: at,
		},
	}
}

func taskEvent(seq uint64, at time.Time, quad identity.Quadruple, typ events.EventType) events.Event {
	return events.Event{
		Type:       typ,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload:    tasks.TaskCompletedPayload{TaskID: tasks.TaskID("t-1")},
	}
}

func bigCounterEvent(seq uint64, at time.Time, id identity.Identity, tokens int64) events.Event {
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity:   identity.Quadruple{Identity: id},
			Model:      "model-c",
			Cost:       llm.Cost{TotalCost: 0.01, Currency: "USD"},
			Usage:      llm.Usage{PromptTokens: int(tokens), CompletionTokens: 0, TotalTokens: int(tokens), LatencyMS: 1},
			OccurredAt: at,
		},
	}
}

func applyEvents(t *testing.T, ctx context.Context, s rollups.Store, evs ...events.Event) {
	t.Helper()
	var deltas []rollups.Delta
	for _, ev := range evs {
		ds, err := rollups.Extract(ev)
		if err != nil {
			t.Fatalf("Extract(seq=%d): %v", ev.Sequence, err)
		}
		deltas = append(deltas, ds...)
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: evs[len(evs)-1].Sequence, Deltas: deltas}); err != nil {
		t.Fatalf("ApplyBatch(checkpoint=%d): %v", evs[len(evs)-1].Sequence, err)
	}
}
