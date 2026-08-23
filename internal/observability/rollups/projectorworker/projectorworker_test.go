package projectorworker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
	"github.com/hurtener/Harbor/internal/tasks"
)

// base is the fixed UTC hour window all fixtures use. Events land one
// minute apart inside it, so every query window [base, base+1h) at
// BucketHour is aligned and covers every row.
var base = rollups.BucketStart(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), rollups.StoreGranularity)

func tq(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
}

// costEvent builds a canonical llm.cost.recorded event for publishing
// (Sequence is bus-assigned; Publish rejects a pre-filled sequence).
// usage is optional — zero usage (all-zero tokens, zero latency) is the
// default, which is valid: the exact measures are the point of the
// callers that pass usage explicitly.
func costEvent(at time.Time, id identity.Quadruple, model string, cost float64, usage ...llm.Usage) events.Event {
	var u llm.Usage
	if len(usage) > 0 {
		u = usage[0]
	}
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   id,
		OccurredAt: at,
		Payload: llm.CostRecordedPayload{
			Identity: id,
			Model:    model,
			Cost:     llm.Cost{TotalCost: cost, Currency: "USD"},
			Usage:    u,
		},
	}
}

// taskEvent builds a canonical task outcome event (completed / failed /
// cancelled) for publishing.
func taskEvent(ty events.EventType, at time.Time, id identity.Quadruple) events.Event {
	var payload events.EventPayload
	switch ty {
	case tasks.EventTypeTaskCompleted:
		payload = tasks.TaskCompletedPayload{TaskID: "t-complete"}
	case tasks.EventTypeTaskFailed:
		payload = tasks.TaskFailedPayload{TaskID: "t-fail", ErrorCode: "boom"}
	case tasks.EventTypeTaskCancelled:
		payload = tasks.TaskCancelledPayload{TaskID: "t-cancel", Reason: "operator"}
	default:
		payload = tasks.TaskCompletedPayload{TaskID: "t"}
	}
	return events.Event{
		Type:       ty,
		Identity:   id,
		OccurredAt: at,
		Payload:    payload,
	}
}

// unsupportedEvent builds a canonical event type the rollup extractor
// deliberately maps to NO measure (it must advance the cursor, never a
// row).
func unsupportedEvent(at time.Time, id identity.Quadruple) events.Event {
	return events.Event{
		Type:       events.EventTypeRuntimeError,
		Identity:   id,
		OccurredAt: at,
		Payload:    events.SubscriptionIdleClosedPayload{SubscriberID: 1},
	}
}

// noticeEvent builds a bus-internal notice — the projection source must
// exclude it from pages (a sequence gap the worker must skip, not fail
// on).
func noticeEvent(at time.Time, id identity.Quadruple) events.Event {
	return events.Event{
		Type:       events.EventTypeBusDropped,
		Identity:   id,
		OccurredAt: at,
		Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1, DroppedCount: 1},
	}
}

func publish(t *testing.T, bus events.EventBus, evs ...events.Event) {
	t.Helper()
	for i, ev := range evs {
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish #%d (type=%s): %v", i, ev.Type, err)
		}
	}
}

// seq assigns explicit bus sequences 1..N to direct-construction event
// fixtures. The in-memory / durable buses assign sequences at Publish;
// stub-source tests construct events directly and must pin them.
func seq(evs ...events.Event) []events.Event {
	for i := range evs {
		evs[i].Sequence = uint64(i + 1)
	}
	return evs
}

// inmemCfg returns an in-memory bus config with the retention ring at
// the given capacity (0 disables the projection substrate).
func inmemCfg(ring int) config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         ring,
	}
}

// newInmemProjectionBus builds an in-memory bus with the retention ring
// enabled and opens its ProjectionSource.
func newInmemProjectionBus(t *testing.T, ring int) (events.EventBus, events.ProjectionSource) {
	t.Helper()
	bus, err := inmem.New(inmemCfg(ring), auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	return bus, src
}

func newMemStore(t *testing.T) *memstore.Store {
	t.Helper()
	s := memstore.New()
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// rowsFor queries one hour window (BucketHour) with the given filter
// and returns every result row.
func rowsFor(t *testing.T, s rollups.Store, f rollups.Filter, measures ...rollups.Measure) []rollups.Row {
	t.Helper()
	res, err := s.Query(context.Background(), rollups.Query{
		From:     base,
		To:       base.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Filter:   f,
		Measures: measures,
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    1000,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res.Rows
}

// measureSum totals a measure across the rows the filter resolves.
func measureSum(t *testing.T, s rollups.Store, f rollups.Filter, m rollups.Measure) int64 {
	t.Helper()
	var total int64
	for _, r := range rowsFor(t, s, f, m) {
		total += r.Measures[m].N
	}
	return total
}

// oneRow asserts the filter resolves exactly one row and returns it.
func oneRow(t *testing.T, s rollups.Store, f rollups.Filter, measures ...rollups.Measure) rollups.Row {
	t.Helper()
	rows := rowsFor(t, s, f, measures...)
	if len(rows) != 1 {
		t.Fatalf("filter %+v resolved %d rows, want exactly 1: %+v", f, len(rows), rows)
	}
	return rows[0]
}

// assertMeasure asserts the filter's single row carries exactly want
// for m.
func assertMeasure(t *testing.T, s rollups.Store, f rollups.Filter, m rollups.Measure, want int64) {
	t.Helper()
	row := oneRow(t, s, f, m)
	if got := row.Measures[m].N; got != want {
		t.Fatalf("measure %s under filter %+v = %d; want %d (row %+v)", m, f, got, want, row)
	}
}

func sessionFilter(session, model string) rollups.Filter {
	return rollups.Filter{SessionIDs: []string{session}, Models: []string{model}}
}

// TestWorker_InmemBus_ForwardProjection_ExactMeasures drives a real
// in-memory bus + memstore end to end: llm.cost.recorded and task
// outcome events project into exact integer measures (cost in micro-
// units, exact token/latency integers), attributed to exactly the
// (tenant, user, session, model) rows, with no identity bleed and no
// measures minted for unsupported event types.
func TestWorker_InmemBus_ForwardProjection_ExactMeasures(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	b := tq("tenant-a", "user-1", "session-b")
	c := tq("tenant-b", "user-2", "session-c")

	// seq 1: A/m1 — 0.10 USD = 100_000 micros, exact tokens + latency.
	// seq 2: A task.completed. seq 3: A/m2 — 0.25 USD = 250_000 micros.
	// seq 4: A task.failed. seq 5: B task.cancelled.
	// seq 6: C runtime.error (unsupported — cursor advance only).
	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "model-a", 0.10, llm.Usage{
			PromptTokens: 10, CompletionTokens: 20, ReasoningTokens: 5,
			CacheReadTokens: 3, CacheWriteTokens: 2, TotalTokens: 30, LatencyMS: 150,
		}),
		taskEvent(tasks.EventTypeTaskCompleted, base.Add(2*time.Minute), a),
		costEvent(base.Add(3*time.Minute), a, "model-b", 0.25, llm.Usage{
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, LatencyMS: 50,
		}),
		taskEvent(tasks.EventTypeTaskFailed, base.Add(4*time.Minute), a),
		taskEvent(tasks.EventTypeTaskCancelled, base.Add(5*time.Minute), b),
		unsupportedEvent(base.Add(6*time.Minute), c),
	)

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q; want current", q.State)
	}
	if q.Watermark != 6 {
		t.Fatalf("watermark = %d; want 6", q.Watermark)
	}
	if q.SourceWatermark != 6 {
		t.Fatalf("source watermark = %d; want 6", q.SourceWatermark)
	}

	// A/m1: exact cost, tokens, latency.
	row := oneRow(t, store, sessionFilter("session-a", "model-a"),
		rollups.MeasureLLMCompletions, rollups.MeasureLLMCostMicros,
		rollups.MeasureLLMTokensPrompt, rollups.MeasureLLMTokensCompletion,
		rollups.MeasureLLMTokensReasoning, rollups.MeasureLLMTokensCacheRead,
		rollups.MeasureLLMTokensCacheWrite, rollups.MeasureLLMTokensTotal,
		rollups.MeasureLLMLatencyCount, rollups.MeasureLLMLatencySumMS,
		rollups.MeasureLLMLatencyMinMS, rollups.MeasureLLMLatencyMaxMS)
	m := row.Measures
	for _, want := range []struct {
		m    rollups.Measure
		want int64
	}{
		{rollups.MeasureLLMCompletions, 1},
		{rollups.MeasureLLMCostMicros, 100_000},
		{rollups.MeasureLLMTokensPrompt, 10},
		{rollups.MeasureLLMTokensCompletion, 20},
		{rollups.MeasureLLMTokensReasoning, 5},
		{rollups.MeasureLLMTokensCacheRead, 3},
		{rollups.MeasureLLMTokensCacheWrite, 2},
		{rollups.MeasureLLMTokensTotal, 30},
		{rollups.MeasureLLMLatencyCount, 1},
		{rollups.MeasureLLMLatencySumMS, 150},
		{rollups.MeasureLLMLatencyMinMS, 150},
		{rollups.MeasureLLMLatencyMaxMS, 150},
	} {
		if got := m[want.m].N; got != want.want {
			t.Fatalf("A/m1 %s = %d; want %d", want.m, got, want.want)
		}
	}

	// A/m2: separate row, exact cost + latency (different model dim).
	assertMeasure(t, store, sessionFilter("session-a", "model-b"), rollups.MeasureLLMCompletions, 1)
	assertMeasure(t, store, sessionFilter("session-a", "model-b"), rollups.MeasureLLMCostMicros, 250_000)
	assertMeasure(t, store, sessionFilter("session-a", "model-b"), rollups.MeasureLLMLatencySumMS, 50)

	// Task outcomes attributed to their own sessions.
	assertMeasure(t, store, rollups.Filter{SessionIDs: []string{"session-a"}}, rollups.MeasureTasksCompleted, 1)
	assertMeasure(t, store, rollups.Filter{SessionIDs: []string{"session-a"}}, rollups.MeasureTasksFailed, 1)
	assertMeasure(t, store, rollups.Filter{SessionIDs: []string{"session-b"}}, rollups.MeasureTasksCancelled, 1)
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureTasksCompleted); got != 1 {
		t.Fatalf("total tasks_completed = %d; want 1 (no bleed)", got)
	}

	// The unsupported runtime.error contributed no measures anywhere.
	if got := measureSum(t, store, rollups.Filter{SessionIDs: []string{"session-c"}}, rollups.MeasureLLMCompletions); got != 0 {
		t.Fatalf("unsupported event minted completions = %d; want 0", got)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions); got != 2 {
		t.Fatalf("total completions = %d; want 2 (no synthesis)", got)
	}

	// Retention horizon reflects the stored minute rows.
	if !q.RetentionStart.Equal(base.Add(time.Minute)) || !q.RetentionEnd.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("retention = %v..%v; want %v..%v", q.RetentionStart, q.RetentionEnd, base.Add(time.Minute), base.Add(5*time.Minute))
	}
}

// TestWorker_OnlyEmptyReadProvesCurrent pins the strict catch-up
// contract: a non-empty page NEVER proves current — even one the
// source labels Current (the tail page of a drained log). Only a
// SUBSEQUENT read that returns no events flips the state.
func TestWorker_OnlyEmptyReadProvesCurrent(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "m", 0.01),
		costEvent(base.Add(2*time.Minute), a, "m", 0.02),
	)

	// Batch larger than the log: ONE non-empty page, source Current.
	w, err := projectorworker.New(src, store, projectorworker.WithBatchSize(100))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caughtUp, err := w.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if caughtUp {
		t.Fatal("a non-empty page must NOT prove current — only a subsequent EMPTY read may")
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCatchingUp {
		t.Fatalf("state after non-empty page = %q; want catching_up", q.State)
	}
	if q.Watermark != 2 {
		t.Fatalf("watermark after non-empty page = %d; want 2 (the events were applied)", q.Watermark)
	}
	if q.WatermarkAt.IsZero() {
		t.Fatal("WatermarkAt must be set after an applied page")
	}

	caughtUp, err = w.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance (empty): %v", err)
	}
	if !caughtUp {
		t.Fatal("an EMPTY read must report caught up")
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after empty read = %q; want current", q.State)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCostMicros); got != 30_000 {
		t.Fatalf("cost = %d micros; want 30_000", got)
	}
}

// TestWorker_NoticesAndFencedGapsAreSkipped pins the source's sequence
// gaps: bus-internal notices and erased-session events are excluded by
// the projection source, so the worker's pages legitimately skip
// sequences. The worker must apply the survivors and advance its
// watermark across the gap — never fail on it.
func TestWorker_NoticesAndFencedGapsAreSkipped(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	b := tq("tenant-a", "user-1", "session-b")

	// seq 1 A cost; seq 2 B cost; seq 3 notice; Fence(B); seq 4 A cost;
	// seq 5 notice at the tail. The tail notice is the important case:
	// a current page can have Watermark > Next even after all canonical
	// events have been applied.
	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "m", 0.01),
		costEvent(base.Add(2*time.Minute), b, "m", 0.50),
		noticeEvent(base.Add(3*time.Minute), a),
	)
	if f, ok := bus.(events.Fencer); ok {
		if err := f.Fence(ctx, b.Identity); err != nil {
			t.Fatalf("bus Fence: %v", err)
		}
	} else {
		t.Fatal("inmem bus does not implement events.Fencer")
	}
	publish(t, bus,
		costEvent(base.Add(4*time.Minute), a, "m", 0.02),
		noticeEvent(base.Add(5*time.Minute), a),
	)

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp (gapped source must not fail): %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	// The page covered seq 1..5; the notices (3, 5) and the fenced B
	// event (2) were excluded. The durable projection checkpoint still
	// advances through the excluded tail so an idle poll does not repeat
	// the same global source page forever.
	if q.Watermark != 5 {
		t.Fatalf("watermark = %d; want 5 (canonical cursor across the gap and excluded tail)", q.Watermark)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q; want current", q.State)
	}
	// Only A's events landed (B was fenced before the page; the notice
	// is not a rollup measure).
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 30_000)
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCompletions, 2)
	if got := measureSum(t, store, rollups.Filter{SessionIDs: []string{"session-b"}}, rollups.MeasureLLMCompletions); got != 0 {
		t.Fatalf("fenced session B minted completions = %d; want 0", got)
	}
}

// TestWorker_RebuildHonorsPermanentFences verifies the rebuild path:
// rows and watermark reset, but the erasure fence is PERMANENT — the
// worker drops the fenced session's late events at ingestion, so
// reprojection never resurrects an erased session. The store's fence
// (not the source's) is the authority here: the source keeps serving
// the fenced event and the worker must drop it.
func TestWorker_RebuildHonorsPermanentFences(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")
	b := tq("tenant-a", "user-1", "session-b")

	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "m", 0.10),
		costEvent(base.Add(2*time.Minute), b, "m", 0.50),
		taskEvent(tasks.EventTypeTaskCompleted, base.Add(3*time.Minute), a),
	)
	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCostMicros); got != 600_000 {
		t.Fatalf("cost before rebuild = %d micros; want 600_000", got)
	}

	// Erase session B via the STORE fence (the durable erasure
	// authority the worker honours at ingestion). The source still
	// serves B's event; the worker must drop it.
	if err := store.FenceSession(ctx, b.Identity); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}

	if err := w.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality after rebuild: %v", err)
	}
	if q.Watermark != 0 || q.State != rollups.StateCatchingUp {
		t.Fatalf("post-rebuild quality = %+v; want watermark 0, catching_up", q)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCostMicros); got != 0 {
		t.Fatalf("rows after rebuild = %d micros; want 0 (reset)", got)
	}
	if f, err := store.IsFenced(ctx, b.Identity); err != nil || !f {
		t.Fatalf("IsFenced(b) after rebuild = %v, %v; want true (erasure fences are permanent)", f, err)
	}

	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after rebuild: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality after rebuild catch-up: %v", err)
	}
	if q.Watermark != 3 || q.State != rollups.StateCurrent {
		t.Fatalf("post-rebuild-catch-up quality = %+v; want watermark 3, current", q)
	}
	// A's rows reconstructed; B's event dropped at ingestion (the fence
	// is permanent and never cleared by rebuild).
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 100_000)
	assertMeasure(t, store, rollups.Filter{SessionIDs: []string{"session-a"}}, rollups.MeasureTasksCompleted, 1)
	if got := measureSum(t, store, rollups.Filter{SessionIDs: []string{"session-b"}}, rollups.MeasureLLMCostMicros); got != 0 {
		t.Fatalf("fenced session B resurrected = %d micros; want 0", got)
	}
}

// TestWorker_RetentionGapLatched pins the retention-quality surface: a
// wrapped in-memory ring reports its eviction honestly, the worker
// latches the gap signal, and a rebuild re-evaluates it from the fresh
// read. A truncated substrate is never presented as a complete stream.
func TestWorker_RetentionGapLatched(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 8) // tiny ring: 20 events evict 12
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")

	var evs []events.Event
	for i := 1; i <= 20; i++ {
		evs = append(evs, costEvent(base.Add(time.Duration(i)*time.Minute), a, "m", 0.01))
	}
	publish(t, bus, evs...)

	// A FRESH worker from cursor 0 reads the retained tail (seq 13..20)
	// with RetentionGap=true.
	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Advance(ctx); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if !q.SourceRetentionGap {
		t.Fatal("retention gap must be latched after a page from before the retained head")
	}
	if q.SourceWatermark != 20 {
		t.Fatalf("source watermark = %d; want 20", q.SourceWatermark)
	}
	// The retained tail (13..20 → 8 completions) projected; the evicted
	// head (1..12) is honestly absent.
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions); got != 8 {
		t.Fatalf("completions from retained tail = %d; want 8", got)
	}

	// The empty read flips to current, but the gap latch stays: the
	// projection's history is still potentially incomplete.
	if caughtUp, err := w.Advance(ctx); err != nil || !caughtUp {
		t.Fatalf("empty Advance = %v, %v; want caught up", caughtUp, err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent || !q.SourceRetentionGap {
		t.Fatalf("quality after empty read = %+v; want current with the gap still latched", q)
	}

	// Rebuild clears the latch; the fresh read re-latches it (the ring
	// is still evicted before cursor 0).
	if err := w.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality after rebuild: %v", err)
	}
	if q.SourceRetentionGap {
		t.Fatal("rebuild must clear the retention-gap latch (re-evaluated on the next page)")
	}
	if _, err := w.Advance(ctx); err != nil {
		t.Fatalf("Advance after rebuild: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if !q.SourceRetentionGap {
		t.Fatal("the fresh read must re-latch the retention gap (history is still incomplete)")
	}
}

// stubSource is a scripted events.ProjectionSource for failure
// injection and deterministic interleaving. It serves a fixed ascending
// run of canonical events like a durable log, with knobs: fail (Page
// errors), unavail (Page reports ProjectionUnavailable), badOrder (Page
// returns events out of ascending order), badNext (Page reports a Next
// that is not the last returned event's sequence), and a gate that
// blocks the Nth Page call until the test releases it.
type stubSource struct {
	mu       sync.Mutex
	events   []events.Event
	fail     error
	unavail  bool
	badOrder bool
	badNext  *uint64
	gate     chan struct{}
	gateCall int
	calls    int
	blocked  chan struct{}
}

func (s *stubSource) Page(ctx context.Context, after uint64, limit int) (events.ProjectionPage, error) {
	if err := ctx.Err(); err != nil {
		return events.ProjectionPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return events.ProjectionPage{}, s.fail
	}
	if s.unavail {
		return events.ProjectionPage{Next: after, Quality: events.ProjectionUnavailable}, nil
	}
	s.calls++
	if s.gate != nil && s.calls == s.gateCall {
		close(s.blocked)
		g := s.gate
		select {
		case <-g:
		case <-ctx.Done():
			return events.ProjectionPage{}, ctx.Err()
		}
	}

	var wm uint64
	for _, ev := range s.events {
		if ev.Sequence > wm {
			wm = ev.Sequence
		}
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
	next := after
	if len(out) > 0 {
		next = out[len(out)-1].Sequence
	}
	if s.badNext != nil {
		next = *s.badNext
	}
	if s.badOrder && len(out) > 2 {
		out[0], out[len(out)-1] = out[len(out)-1], out[0]
	}
	more := false
	if len(out) > 0 {
		last := out[len(out)-1].Sequence
		for _, ev := range s.events {
			if ev.Sequence > last {
				more = true
				break
			}
		}
	}
	quality := events.ProjectionCurrent
	if more {
		quality = events.ProjectionCatchingUp
	}
	return events.ProjectionPage{Events: out, Next: next, Watermark: wm, Quality: quality}, nil
}

func (s *stubSource) Watermark(_ context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavail {
		return 0, events.ErrProjectionUnavailable
	}
	var wm uint64
	for _, ev := range s.events {
		if ev.Sequence > wm {
			wm = ev.Sequence
		}
	}
	return wm, nil
}

func (s *stubSource) Watch(_ context.Context, _ chan<- uint64) (events.ProjectionWatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavail {
		return nil, events.ErrProjectionUnavailable
	}
	return events.ProjectionWatchFunc(func() {}), nil
}

// setFail flips the failure-injection knob under the stub's mutex so a
// test mutating it while the worker's Run loop is concurrently reading
// it stays race-free.
func (s *stubSource) setFail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// TestWorker_SourceUnavailableAndFailureHeal pins the fail-loud
// posture: a source without a retained substrate is NEVER an empty
// stream, a source error marks StateUnavailable without touching the
// durable watermark, and the next healthy advance retries and heals.
func TestWorker_SourceUnavailableAndFailureHeal(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")
	store := newMemStore(t)
	src := &stubSource{events: seq(costEvent(base.Add(time.Minute), a, "m", 0.01))}

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Substrate unavailable → loud failure, never an empty stream.
	src.unavail = true
	if _, err := w.Advance(ctx); !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("Advance over an unavailable source: err=%v; want ErrProjectionUnavailable", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable || q.Err == nil {
		t.Fatalf("quality after unavailable = %+v; want unavailable with Err", q)
	}
	if q.Watermark != 0 {
		t.Fatalf("watermark after unavailable = %d; want 0 (checkpoint untouched)", q.Watermark)
	}

	// Source error → unavailable, checkpoint untouched.
	src.unavail = false
	src.fail = errors.New("durable log scan failed")
	if _, err := w.Advance(ctx); err == nil {
		t.Fatal("Advance over a failing source must error")
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateUnavailable || q.Err == nil {
		t.Fatalf("quality after failure = %+v; want unavailable with Err", q)
	}

	// Heal: the next advance applies the event, then the empty read
	// proves current.
	src.fail = nil
	caughtUp, err := w.Advance(ctx)
	if err != nil {
		t.Fatalf("Advance after heal: %v", err)
	}
	if caughtUp {
		t.Fatal("the healing page is non-empty; it must not prove current")
	}
	if _, err := w.Advance(ctx); err != nil {
		t.Fatalf("Advance (empty): %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state after heal = %q; want current", q.State)
	}
	assertMeasure(t, store, sessionFilter("session-a", "m"), rollups.MeasureLLMCostMicros, 10_000)
}

// TestWorker_CursorContractViolationsFailLoud pins the source-cursor
// guards: a page whose Next does not advance past the cursor, or whose
// events are not strictly ascending, would jump or double-apply the
// watermark — the worker must refuse loudly and leave the checkpoint
// untouched rather than silently under- or double-count.
func TestWorker_CursorContractViolationsFailLoud(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")

	cases := []struct {
		name string
		evs  []events.Event
		knob func(*stubSource)
	}{
		{
			name: "next-not-advancing",
			evs:  seq(costEvent(base.Add(time.Minute), a, "m", 0.01)),
			knob: func(s *stubSource) { n := uint64(0); s.badNext = &n },
		},
		{
			name: "non-ascending",
			evs: seq(
				costEvent(base.Add(time.Minute), a, "m", 0.01),
				costEvent(base.Add(2*time.Minute), a, "m", 0.02),
				costEvent(base.Add(3*time.Minute), a, "m", 0.03),
			),
			knob: func(s *stubSource) { s.badOrder = true },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(t)
			src := &stubSource{events: tc.evs}
			tc.knob(src)
			w, err := projectorworker.New(src, store)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := w.Advance(ctx); err == nil {
				t.Fatal("Advance over a cursor-violating page must fail loudly")
			}
			q, err := w.Quality(ctx)
			if err != nil {
				t.Fatalf("Quality: %v", err)
			}
			if q.State != rollups.StateUnavailable {
				t.Fatalf("state = %q; want unavailable", q.State)
			}
			if ck, err := store.Checkpoint(ctx); err != nil || ck != 0 {
				t.Fatalf("checkpoint after violation = %d, %v; want 0 (nothing applied)", ck, err)
			}
		})
	}
}

// TestWorker_NilDependencies pins the construction contract: a nil
// source or a nil store fails loudly.
func TestWorker_NilDependencies(t *testing.T) {
	store := newMemStore(t)
	if _, err := projectorworker.New(nil, store); err == nil {
		t.Fatal("nil source must fail construction")
	}
	src := &stubSource{}
	if _, err := projectorworker.New(src, nil); err == nil {
		t.Fatal("nil store must fail construction")
	}
}

// TestWorker_SubCentAndCacheTokenFidelity pins the exact-integer
// precision model through a real bus: sub-cent costs convert to exact
// micro-units and cache-token fields survive the round trip without
// float drift or truncation.
func TestWorker_SubCentAndCacheTokenFidelity(t *testing.T) {
	ctx := context.Background()
	bus, src := newInmemProjectionBus(t, 64)
	store := newMemStore(t)
	a := tq("tenant-a", "user-1", "session-a")

	publish(t, bus,
		costEvent(base.Add(time.Minute), a, "m", 0.000001, llm.Usage{
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, LatencyMS: 7,
		}),
		costEvent(base.Add(2*time.Minute), a, "m", 0.000002, llm.Usage{
			PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4, LatencyMS: 11,
		}),
		costEvent(base.Add(3*time.Minute), a, "m", 1.0, llm.Usage{
			PromptTokens: 100, CompletionTokens: 100, ReasoningTokens: 50,
			CacheReadTokens: 40, CacheWriteTokens: 30, TotalTokens: 250, LatencyMS: 1000,
		}),
	)
	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	row := oneRow(t, store, sessionFilter("session-a", "m"),
		rollups.MeasureLLMCostMicros, rollups.MeasureLLMTokensPrompt,
		rollups.MeasureLLMTokensCompletion, rollups.MeasureLLMTokensReasoning,
		rollups.MeasureLLMTokensCacheRead, rollups.MeasureLLMTokensCacheWrite,
		rollups.MeasureLLMTokensTotal, rollups.MeasureLLMLatencySumMS,
		rollups.MeasureLLMLatencyMinMS, rollups.MeasureLLMLatencyMaxMS)
	m := row.Measures
	for _, want := range []struct {
		m    rollups.Measure
		want int64
	}{
		{rollups.MeasureLLMCostMicros, 1_000_003}, // 1 + 2 + 1_000_000, exact
		{rollups.MeasureLLMTokensPrompt, 103},
		{rollups.MeasureLLMTokensCompletion, 103},
		{rollups.MeasureLLMTokensReasoning, 50},
		{rollups.MeasureLLMTokensCacheRead, 40},
		{rollups.MeasureLLMTokensCacheWrite, 30},
		{rollups.MeasureLLMTokensTotal, 256},
		{rollups.MeasureLLMLatencySumMS, 1018},
		{rollups.MeasureLLMLatencyMinMS, 7},
		{rollups.MeasureLLMLatencyMaxMS, 1000},
	} {
		if got := m[want.m].N; got != want.want {
			t.Fatalf("%s = %d; want %d", want.m, got, want.want)
		}
	}
}
