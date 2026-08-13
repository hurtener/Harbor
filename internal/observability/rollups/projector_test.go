package rollups_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	"github.com/hurtener/Harbor/internal/tasks"
)

// testSource is a faithful stand-in for the durable-log source: a fixed,
// ascending, gap-free run of successfully-persisted canonical events with a
// failure-injection knob. Next returns events strictly newer than after.
type testSource struct {
	mu     sync.Mutex
	events []events.Event
	err    error // when set, Next fails loudly (StateUnavailable)
}

func (s *testSource) Next(ctx context.Context, after uint64, limit int) ([]events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make([]events.Event, 0, limit)
	for _, ev := range s.events {
		if ev.Sequence <= after {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// eventID is a small identity fixture helper.
func eventID(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
}

// costRecord builds a cost.recorded event at the given sequence/time.
func costRecord(seq uint64, at time.Time, quad identity.Quadruple, model string, cost float64) events.Event {
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Identity: quad,
			Model:    model,
			Cost:     llm.Cost{TotalCost: cost, Currency: "USD"},
			Usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
		},
	}
}

// completeRecord builds a task.completed event.
func completeRecord(seq uint64, at time.Time, quad identity.Quadruple) events.Event {
	return events.Event{
		Type:       tasks.EventTypeTaskCompleted,
		Identity:   quad,
		OccurredAt: at,
		Sequence:   seq,
		Payload:    tasks.TaskCompletedPayload{TaskID: "t"},
	}
}

func newStore(t *testing.T) *memstore.Store {
	t.Helper()
	return memstore.New()
}

func TestProjector_RestartCatchUpAndReplayIdempotency(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quad := eventID("tenant-a", "user-1", "session-1")

	evs := []events.Event{
		costRecord(1, h.Add(1*time.Minute), quad, "model-a", 1.0),
		costRecord(2, h.Add(2*time.Minute), quad, "model-a", 2.0),
		completeRecord(3, h.Add(3*time.Minute), quad),
	}
	src := &testSource{events: evs}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store, rollups.WithProjectorBatchSize(2))
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}

	// First run: catch up in two batches (batch size 2).
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 3 {
		t.Fatalf("watermark = %d; want 3", q.Watermark)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q; want current", q.State)
	}
	if !q.RetentionStart.Equal(h) || !q.RetentionEnd.Equal(h) {
		t.Fatalf("retention = %v..%v; want %v..%v", q.RetentionStart, q.RetentionEnd, h, h)
	}

	// "Restart": a fresh projector over the SAME store + source must
	// resume at the durable checkpoint (3) — not double-apply events 1..3.
	p2, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector (restart): %v", err)
	}
	q2, err := p2.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality (restart): %v", err)
	}
	if q2.Watermark != 3 {
		t.Fatalf("restart watermark = %d; want 3 (durable checkpoint)", q2.Watermark)
	}
	caughtUp, err := p2.Advance(ctx)
	if err != nil {
		t.Fatalf("restart Advance: %v", err)
	}
	if !caughtUp {
		t.Fatal("restart Advance must report caught up (source has nothing newer than 3)")
	}

	// Replay idempotency: the restart must not have changed the sums.
	res, err := store.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(res.Rows))
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostUSD]; got != 3.0 {
		t.Fatalf("cost after restart = %v; want 3.0 (no double-count)", got)
	}
	if got := res.Rows[0].Measures[rollups.MeasureTasksCompleted]; got != 1 {
		t.Fatalf("tasks_completed after restart = %v; want 1", got)
	}
}

func TestProjector_QualityStates(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quad := eventID("tenant-a", "user-1", "session-1")

	// Fresh projector over a source with events: CatchingUp until drained.
	src := &testSource{events: []events.Event{costRecord(1, h, quad, "m", 1)}}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCatchingUp {
		t.Fatalf("fresh state = %q; want catching_up (head not yet verified)", q.State)
	}
	if q.Watermark != 0 || !q.WatermarkAt.IsZero() {
		t.Fatalf("fresh watermark = %d at %v; want 0 at zero", q.Watermark, q.WatermarkAt)
	}

	caughtUp, err := p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !caughtUp {
		t.Fatal("single-event source must catch up in one batch")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after drain = %q; want current", q.State)
	}
	if q.Watermark != 1 {
		t.Fatalf("watermark = %d; want 1", q.Watermark)
	}
	if q.WatermarkAt.IsZero() {
		t.Fatal("WatermarkAt must be set after an advance")
	}

	// Failure: the source breaks; state becomes Unavailable with the error.
	src.err = errors.New("durable log scan failed")
	if _, err := p.Advance(ctx); err == nil {
		t.Fatal("Advance over a broken source must error")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable {
		t.Fatalf("state after failure = %q; want unavailable", q.State)
	}
	if q.Err == nil {
		t.Fatal("Quality must carry the failure")
	}
	if q.Watermark != 1 {
		t.Fatalf("watermark after failure = %d; want 1 (checkpoint untouched)", q.Watermark)
	}

	// Recovery: the source heals; the next Advance moves forward.
	src.err = nil
	caughtUp, err = p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance after recovery: %v", err)
	}
	if !caughtUp {
		t.Fatal("source has nothing newer than 1; must catch up")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after recovery = %q; want current", q.State)
	}
}

func TestProjector_GapFailsLoud(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quad := eventID("tenant-a", "user-1", "session-1")

	// A gapped source violates the durable-log contract — the projector
	// must fail loudly rather than jump the checkpoint over the missing
	// sequence (a permanent undercount).
	src := &testSource{events: []events.Event{
		costRecord(1, h, quad, "m", 1),
		costRecord(2, h.Add(time.Minute), quad, "m", 2),
		costRecord(4, h.Add(2*time.Minute), quad, "m", 4), // seq 3 missing
	}}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	if _, err := p.Advance(ctx); err == nil {
		t.Fatal("Advance over a gapped source must fail loudly")
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable {
		t.Fatalf("state = %q; want unavailable", q.State)
	}
	ck, err := store.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if ck != 0 {
		t.Fatalf("checkpoint after gap = %d; want 0 (nothing applied)", ck)
	}
}

func TestProjector_FencedSessionDrop(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quadA := eventID("tenant-a", "user-1", "session-a")
	quadB := eventID("tenant-a", "user-1", "session-b")

	evs := []events.Event{
		costRecord(1, h, quadA, "m", 1),
		costRecord(2, h.Add(time.Minute), quadB, "m", 2),
		costRecord(3, h.Add(2*time.Minute), quadA, "m", 3),
	}
	src := &testSource{events: evs}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	// Session-b is erased while the projector is behind: its events must
	// be dropped at ingestion, never resurrected.
	if err := store.FenceSession(ctx, quadB.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	p, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 3 {
		t.Fatalf("watermark = %d; want 3 (the fenced event is dropped, not fatal)", q.Watermark)
	}

	res, err := store.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-a"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(res.Rows))
	}
	if got := res.Rows[0].Measures[rollups.MeasureLLMCostUSD]; got != 4 {
		t.Fatalf("cost = %v; want 4 (events 1+3; fenced event 2 dropped)", got)
	}
}

func TestProjector_Rebuild(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quad := eventID("tenant-a", "user-1", "session-1")

	src := &testSource{events: []events.Event{
		costRecord(1, h, quad, "m", 1),
		costRecord(2, h.Add(time.Minute), quad, "m", 2),
	}}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if err := store.FenceSession(ctx, quad.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if f, err := store.IsFenced(ctx, quad.Identity); err != nil || !f {
		t.Fatalf("IsFenced = %v, %v; want true", f, err)
	}

	// Rebuild clears rows, fences, checkpoint; reprocessing from the log
	// head reconstructs the projection.
	if err := p.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 0 || q.State != rollups.StateCatchingUp {
		t.Fatalf("post-rebuild quality = %+v; want watermark 0, catching_up", q)
	}
	if f, err := store.IsFenced(ctx, quad.Identity); err != nil || f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want false", f, err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after rebuild: %v", err)
	}
	res, err := store.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCostUSD] != 3 {
		t.Fatalf("post-rebuild cost = %+v; want 3", res.Rows)
	}
}

func TestProjector_CatchUpMultipleBatches(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.BucketHour)
	quad := eventID("tenant-a", "user-1", "session-1")

	var evs []events.Event
	for i := 0; i < 25; i++ {
		evs = append(evs, costRecord(uint64(i+1), h.Add(time.Duration(i)*time.Second), quad, "m", 1))
	}
	src := &testSource{events: evs}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store, rollups.WithProjectorBatchSize(7))
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 25 || q.State != rollups.StateCurrent {
		t.Fatalf("quality = %+v; want watermark 25, current", q)
	}
	res, err := store.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostUSD},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCostUSD] != 25 {
		t.Fatalf("cost after multi-batch catch-up = %+v; want 25", res.Rows)
	}
}

func TestProjector_NilDependencies(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	if _, err := rollups.NewProjector(nil, store); err == nil {
		t.Fatal("nil source must fail construction")
	}
	if _, err := rollups.NewProjector(&testSource{}, nil); err == nil {
		t.Fatal("nil store must fail construction")
	}
}
