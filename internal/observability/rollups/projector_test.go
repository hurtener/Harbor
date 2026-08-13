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
// failure-injection knob. Next returns at most limit events strictly newer
// than after — a short non-empty batch does NOT mean the source is
// exhausted, only an empty read does (the corrected Source contract).
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
			Usage:    llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20, LatencyMS: 100},
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

// costMicros queries the store for the exact cost in micro-units.
func costMicros(ctx context.Context, t *testing.T, s rollups.Store, from, to time.Time, bucket rollups.BucketSize) int64 {
	t.Helper()
	res, err := s.Query(ctx, rollups.Query{
		From:     from,
		To:       to,
		Bucket:   bucket,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var total int64
	for _, r := range res.Rows {
		total += r.Measures[rollups.MeasureLLMCostMicros].N
	}
	return total
}

func TestProjector_RestartCatchUpAndReplayIdempotency(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
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

	// First run: catch up in batches of 2 (events 1..2, then 3, then the
	// empty read that proves current).
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
	// Retention is the row-level MINUTE grid: events land in buckets
	// 12:01, 12:02, 12:03.
	if !q.RetentionStart.Equal(h.Add(1*time.Minute)) || !q.RetentionEnd.Equal(h.Add(3*time.Minute)) {
		t.Fatalf("retention = %v..%v; want %v..%v", q.RetentionStart, q.RetentionEnd, h.Add(1*time.Minute), h.Add(3*time.Minute))
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
		t.Fatal("restart Advance must report caught up (an EMPTY read over a source with nothing newer than 3)")
	}

	// Replay idempotency: the restart must not have changed the sums.
	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 3_000_000 {
		t.Fatalf("cost after restart = %d micros; want 3_000_000 (no double-count)", got)
	}
	res, err := store.Query(ctx, rollups.Query{
		From:     h,
		To:       h.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureTasksCompleted},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureTasksCompleted].N != 1 {
		t.Fatalf("tasks_completed after restart = %+v; want 1", res.Rows)
	}
}

func TestProjector_QualityStates(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quad := eventID("tenant-a", "user-1", "session-1")

	// Fresh projector over a source with events: CatchingUp until the
	// empty read that drains it.
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

	// First Advance applies the single event but must NOT report caught
	// up: a short non-empty batch does not prove the source is exhausted.
	caughtUp, err := p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if caughtUp {
		t.Fatal("a short NON-EMPTY batch must not prove current — only a subsequent empty read may")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCatchingUp {
		t.Fatalf("state after non-empty batch = %q; want catching_up", q.State)
	}
	if q.Watermark != 1 {
		t.Fatalf("watermark = %d; want 1", q.Watermark)
	}
	if q.WatermarkAt.IsZero() {
		t.Fatal("WatermarkAt must be set after an advance")
	}

	// The subsequent EMPTY read marks current.
	caughtUp, err = p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance (empty): %v", err)
	}
	if !caughtUp {
		t.Fatal("an empty read must report caught up")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after empty read = %q; want current", q.State)
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

	// Recovery: the source heals; the next Advance (empty read) moves to
	// current.
	src.err = nil
	caughtUp, err = p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance after recovery: %v", err)
	}
	if !caughtUp {
		t.Fatal("source has nothing newer than 1; the empty read must catch up")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after recovery = %q; want current", q.State)
	}
}

// TestProjector_ShortBatchDoesNotProveCurrent pins the corrected Source
// contract: Source.Next promises at most limit, so a short non-empty batch
// (here the final event of a 3-event log with batch size 2) must leave the
// projector catching_up; only the SUBSEQUENT empty read marks current.
func TestProjector_ShortBatchDoesNotProveCurrent(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quad := eventID("tenant-a", "user-1", "session-1")

	src := &testSource{events: []events.Event{
		costRecord(1, h, quad, "m", 1),
		costRecord(2, h.Add(time.Minute), quad, "m", 2),
		costRecord(3, h.Add(2*time.Minute), quad, "m", 4),
	}}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store, rollups.WithProjectorBatchSize(2))
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}

	// Batch 1: two events — a FULL batch. Not current.
	caughtUp, err := p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance 1: %v", err)
	}
	if caughtUp {
		t.Fatal("full batch must not report caught up")
	}
	// Batch 2: one event — a SHORT NON-EMPTY batch. Still not current: the
	// source promised at most limit, not "the rest", so exhaustion is
	// unproven.
	caughtUp, err = p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance 2: %v", err)
	}
	if caughtUp {
		t.Fatal("short non-empty batch must NOT prove current — more events may exist beyond the returned prefix")
	}
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCatchingUp {
		t.Fatalf("state after short non-empty batch = %q; want catching_up", q.State)
	}
	if q.Watermark != 3 {
		t.Fatalf("watermark after short batch = %d; want 3 (the events were applied)", q.Watermark)
	}

	// Batch 3: the EMPTY read proves current.
	caughtUp, err = p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance 3: %v", err)
	}
	if !caughtUp {
		t.Fatal("empty read must report caught up")
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after empty read = %q; want current", q.State)
	}

	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 7_000_000 {
		t.Fatalf("cost = %d micros; want 7_000_000", got)
	}
}

func TestProjector_GapFailsLoud(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
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
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
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

	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 4_000_000 {
		t.Fatalf("cost = %d micros; want 4_000_000 (events 1+3; fenced event 2 dropped)", got)
	}
}

func TestProjector_Rebuild(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quadA := eventID("tenant-a", "user-1", "session-a")
	quadB := eventID("tenant-a", "user-1", "session-b")

	src := &testSource{events: []events.Event{
		costRecord(1, h, quadA, "m", 1),
		costRecord(2, h.Add(time.Minute), quadB, "m", 2),
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
	if err := store.FenceSession(ctx, quadA.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if f, err := store.IsFenced(ctx, quadA.Identity); err != nil || !f {
		t.Fatalf("IsFenced = %v, %v; want true", f, err)
	}

	// Rebuild resets rows + checkpoint and returns to catching_up — but
	// the erasure fence is PERMANENT and must survive: reprojection cannot
	// resurrect the erased session.
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
	if f, err := store.IsFenced(ctx, quadA.Identity); err != nil || !f {
		t.Fatalf("IsFenced after rebuild = %v, %v; want TRUE (erasure fences are permanent)", f, err)
	}

	// Catch-up after rebuild: session-a's event is dropped at ingestion
	// (still fenced); session-b's row is reconstructed.
	if err := p.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after rebuild: %v", err)
	}
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 2 {
		t.Fatalf("watermark after rebuild catch-up = %d; want 2", q.Watermark)
	}
	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 2_000_000 {
		t.Fatalf("post-rebuild cost = %d micros; want 2_000_000 (only session-b; session-a never resurrected)", got)
	}
	if f, err := store.IsFenced(ctx, quadA.Identity); err != nil || !f {
		t.Fatalf("IsFenced after rebuild catch-up = %v, %v; want true", f, err)
	}
}

func TestProjector_CatchUpMultipleBatches(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
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
	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 25_000_000 {
		t.Fatalf("cost after multi-batch catch-up = %d micros; want 25_000_000", got)
	}
}

// gatedSource is a testSource whose Nth Next call blocks until the test
// releases it — the deterministic "delayed Advance" knob for the projector
// serialization regression. blocked is closed once the blocking call is
// inside Next, so the test can observe that the step is in-flight.
type gatedSource struct {
	mu      sync.Mutex
	events  []events.Event
	blockOn int // 1-based Next call index that blocks until gate is released
	calls   int
	gate    chan struct{}
	blocked chan struct{}
}

func (s *gatedSource) Next(ctx context.Context, after uint64, limit int) ([]events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.calls++
	block := s.calls == s.blockOn
	s.mu.Unlock()
	if block {
		close(s.blocked)
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// TestProjector_AdvanceRebuildSerialization pins the full-step advance
// serialization: the advance mutex is held for the ENTIRE Advance step, and
// Rebuild coordinates through the SAME mutex, so a delayed advance can never
// land AFTER a rebuild (which would jump the fresh checkpoint over the
// pre-rebuild events) and no advance can overwrite p.watermark backwards.
//
// Deterministic interleaving: drain 1..7, then start a SECOND Advance that
// blocks inside Source.Next holding the advance mutex; start Rebuild while
// it is blocked (on the fixed code Rebuild waits for the advance; on the
// unfixed code it runs immediately — the corruption window). Release the
// gate, join both, then drain to the head. The final watermark/checkpoint
// must be the newest sequence (10) with every event applied exactly once.
func TestProjector_AdvanceRebuildSerialization(t *testing.T) {
	ctx := context.Background()
	h := rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)
	quad := eventID("tenant-a", "user-1", "session-1")

	// A fixed 10-event log; event i costs i USD (i * 1e6 micros).
	var evs []events.Event
	for i := 1; i <= 10; i++ {
		evs = append(evs, costRecord(uint64(i), h.Add(time.Duration(i)*time.Minute), quad, "model-a", float64(i)))
	}
	src := &gatedSource{
		events:  evs,
		blockOn: 2, // the SECOND Advance (the delayed one) blocks
		gate:    make(chan struct{}),
		blocked: make(chan struct{}),
	}
	store := newStore(t)
	defer func() { _ = store.Close(ctx) }()

	p, err := rollups.NewProjector(src, store, rollups.WithProjectorBatchSize(7))
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}

	// Drain 1..7 (one full batch of 7).
	caughtUp, err := p.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance 1: %v", err)
	}
	if caughtUp {
		t.Fatal("full batch must not report caught up")
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 7 {
		t.Fatalf("checkpoint after drain 1..7 = %d, %v; want 7", ck, err)
	}

	// The DELAYED Advance: reads after=7, blocks inside Next (holding the
	// advance mutex on the fixed code) waiting for events 8..10.
	advDone := make(chan error, 1)
	go func() {
		_, err := p.Advance(ctx)
		advDone <- err
	}()
	<-src.blocked // the delayed Advance is now mid-step

	// Quality stays READABLE while the state-mutating step is in-flight
	// (the concurrent-read guarantee).
	q, err := p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality during in-flight advance: %v", err)
	}
	if q.Watermark != 7 {
		t.Fatalf("quality watermark during in-flight advance = %d; want 7", q.Watermark)
	}

	// The staggered Rebuild: launched while the advance is blocked. On the
	// fixed code it waits on the advance mutex until the advance finishes;
	// on the unfixed code it runs immediately and wipes the store under
	// the in-flight advance (the corruption window).
	rbDone := make(chan error, 1)
	go func() {
		rbDone <- p.Rebuild(ctx)
	}()

	// Release the delayed advance. Fixed code order: the advance completes
	// (applies 8..10 on top of 1..7), THEN the rebuild resets rows +
	// checkpoint to 0. Unfixed code order: rebuild already ran, then the
	// advance applies {checkpoint 10} over the emptied store — events
	// 1..7 are lost forever (the checkpoint claims they were applied).
	close(src.gate)
	if err := <-advDone; err != nil {
		t.Fatalf("delayed Advance: %v", err)
	}
	if err := <-rbDone; err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// After the rebuild completed LAST, the store is empty and the
	// projector must re-drain the WHOLE log: the watermark/checkpoint move
	// monotonically to the newest sequence (10) and stay coherent.
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality after rebuild: %v", err)
	}
	if q.Watermark != 0 || q.State != rollups.StateCatchingUp {
		t.Fatalf("post-rebuild quality = %+v; want watermark 0, catching_up", q)
	}

	var prev uint64
	iterations := 0
	for {
		caughtUp, err := p.Advance(ctx)
		if err != nil {
			t.Fatalf("drain Advance: %v", err)
		}
		q, err := p.Quality(ctx)
		if err != nil {
			t.Fatalf("Quality during drain: %v", err)
		}
		ck, err := store.Checkpoint(ctx)
		if err != nil {
			t.Fatalf("Checkpoint during drain: %v", err)
		}
		if q.Watermark != ck {
			t.Fatalf("coherence broken: projector watermark %d != store checkpoint %d", q.Watermark, ck)
		}
		if ck < prev {
			t.Fatalf("monotonicity violated: checkpoint regressed %d -> %d", prev, ck)
		}
		prev = ck
		iterations++
		if caughtUp {
			break
		}
		if iterations > 20 {
			t.Fatal("drain did not converge")
		}
	}

	// Final coherence: watermark == checkpoint == newest sequence (10),
	// state current, every event applied exactly once (sum 1..10 = 55 USD),
	// and the retained horizon matches the event buckets.
	q, err = p.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality final: %v", err)
	}
	if q.Watermark != 10 || q.State != rollups.StateCurrent {
		t.Fatalf("final quality = %+v; want watermark 10, current", q)
	}
	if ck, err := store.Checkpoint(ctx); err != nil || ck != 10 {
		t.Fatalf("final checkpoint = %d, %v; want 10", ck, err)
	}
	if got := costMicros(ctx, t, store, h, h.Add(time.Hour), rollups.BucketHour); got != 55_000_000 {
		t.Fatalf("final cost = %d micros; want 55_000_000 (events 1..10 applied exactly once)", got)
	}
	if !q.RetentionStart.Equal(h.Add(1*time.Minute)) || !q.RetentionEnd.Equal(h.Add(10*time.Minute)) {
		t.Fatalf("final retention = %v..%v; want %v..%v", q.RetentionStart, q.RetentionEnd, h.Add(1*time.Minute), h.Add(10*time.Minute))
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
