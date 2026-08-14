package projectorworker_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	rollsqlite "github.com/hurtener/Harbor/internal/observability/rollups/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// durableBusCfg is the durable event-bus config the restart tests use
// (mirrors the durable driver's own test config).
func durableBusCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         64,
	}
}

// openStateStore opens the file-backed SQLite StateStore (the durable
// event log substrate) at path.
func openStateStore(t *testing.T, path string) state.StateStore {
	t.Helper()
	s, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: path})
	if err != nil {
		t.Fatalf("statesqlite.New(%s): %v", path, err)
	}
	return s
}

// openRollupStore opens the file-backed SQLite rollup store at path.
func openRollupStore(t *testing.T, path string) *rollsqlite.Store {
	t.Helper()
	s, err := rollsqlite.New(path)
	if err != nil {
		t.Fatalf("rollsqlite.New(%s): %v", path, err)
	}
	return s
}

// openDurableBus opens a durable event bus over store (which it does
// NOT own — the caller closes it).
func openDurableBus(t *testing.T, store state.StateStore) (events.EventBus, events.ProjectionSource) {
	t.Helper()
	bus, err := durable.New(context.Background(), durableBusCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatalf("OpenProjectionSource: %v", err)
	}
	return bus, src
}

// durableCostEvents builds the canonical cost fixtures the durable-log
// restart tests publish: sub-cent costs, cache-token fields, and exact
// latencies that must survive the durable log's generic JSON round
// trip (the rehydrated RedactedMap payload shape Extract decodes).
func durableCostEvents(id identity.Quadruple, costs []float64) []events.Event {
	out := make([]events.Event, 0, len(costs))
	for i, c := range costs {
		out = append(out, costEvent(base.Add(time.Duration(i+1)*time.Minute), id, "model-r", c, llm.Usage{
			PromptTokens:     i + 1,
			CompletionTokens: i + 1,
			TotalTokens:      2 * (i + 1),
			CacheReadTokens:  i,
			CacheWriteTokens: i,
			LatencyMS:        int64(10 * (i + 1)),
		}))
	}
	return out
}

// TestWorker_DurableRestart_CrashBetweenPersistAndApply is the crash
// recovery pin: events persist into the durable log, the projection
// crashes BEFORE applying anything (checkpoint 0), and a fresh worker
// over the reopened durable stores catches up from the durable
// watermark — draining the persisted log exactly once, with the exact
// sub-cent / cache-token / latency measures surviving the durable log's
// generic JSON rehydration. New events published after the restart
// continue the rehydrated sequence.
func TestWorker_DurableRestart_CrashBetweenPersistAndApply(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "events.db")
	rollDB := filepath.Join(dir, "rollups.db")
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")

	// Run 1: persist 5 cost events and "crash" (close) WITHOUT any
	// projection apply — the rollup checkpoint stays 0.
	state1 := openStateStore(t, stateDB)
	bus1, _ := openDurableBus(t, state1)
	publish(t, bus1, durableCostEvents(a, []float64{0.000001, 0.000002, 0.10, 0.25, 1.0})...)
	if err := bus1.Close(ctx); err != nil {
		t.Fatalf("bus1.Close: %v", err)
	}
	if err := state1.Close(ctx); err != nil {
		t.Fatalf("state1.Close: %v", err)
	}

	// Run 2: restart — a NEW durable bus over the SAME event-log file
	// (sequences rehydrate from the persisted log), a NEW rollup store
	// over the SAME projection file (checkpoint 0 — nothing was applied).
	state2 := openStateStore(t, stateDB)
	bus2, src2 := openDurableBus(t, state2)
	roll2 := openRollupStore(t, rollDB)
	defer func() {
		_ = bus2.Close(ctx)
		_ = state2.Close(ctx)
		_ = roll2.Close(ctx)
	}()

	w, err := projectorworker.New(src2, roll2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after restart: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 5 {
		t.Fatalf("watermark = %d; want 5", q.Watermark)
	}
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q; want current", q.State)
	}

	// Exact totals through the durable RedactedMap rehydration: 1 + 2 +
	// 100_000 + 250_000 + 1_000_000 micros; tokens and latency exact.
	row := oneRow(t, roll2, sessionFilter("session-a", "model-r"),
		rollups.MeasureLLMCompletions, rollups.MeasureLLMCostMicros,
		rollups.MeasureLLMTokensPrompt, rollups.MeasureLLMTokensCompletion,
		rollups.MeasureLLMTokensCacheRead, rollups.MeasureLLMTokensCacheWrite,
		rollups.MeasureLLMTokensTotal, rollups.MeasureLLMLatencySumMS,
		rollups.MeasureLLMLatencyMinMS, rollups.MeasureLLMLatencyMaxMS)
	m := row.Measures
	for _, want := range []struct {
		m    rollups.Measure
		want int64
	}{
		{rollups.MeasureLLMCompletions, 5},
		{rollups.MeasureLLMCostMicros, 1_350_003},
		{rollups.MeasureLLMTokensPrompt, 15},
		{rollups.MeasureLLMTokensCompletion, 15},
		{rollups.MeasureLLMTokensCacheRead, 10},
		{rollups.MeasureLLMTokensCacheWrite, 10},
		{rollups.MeasureLLMTokensTotal, 30},
		{rollups.MeasureLLMLatencySumMS, 150},
		{rollups.MeasureLLMLatencyMinMS, 10},
		{rollups.MeasureLLMLatencyMaxMS, 50},
	} {
		if got := m[want.m].N; got != want.want {
			t.Fatalf("%s through durable rehydration = %d; want %d", want.m, got, want.want)
		}
	}

	// New events after the restart continue the rehydrated sequence
	// (6, 7) and the worker drains them on a further catch-up.
	publish(t, bus2, costEvent(base.Add(6*time.Minute), a, "model-r", 0.01),
		costEvent(base.Add(7*time.Minute), a, "model-r", 0.02))
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after new events: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 7 {
		t.Fatalf("watermark after new events = %d; want 7", q.Watermark)
	}
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCompletions, 7)
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCostMicros, 1_380_003)
}

// TestWorker_DurableRestart_ResumeFromDurableWatermark pins the
// restart-resume contract: a worker that already applied events to a
// durable rollup store, then restarts over the reopened stores, resumes
// from the durable watermark — re-reading the log does not double-count
// (the atomic page apply makes replay idempotent).
func TestWorker_DurableRestart_ResumeFromDurableWatermark(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "events.db")
	rollDB := filepath.Join(dir, "rollups.db")
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")

	// Run 1: a full worker run applies all 5 events (checkpoint 5).
	state1 := openStateStore(t, stateDB)
	bus1, src1 := openDurableBus(t, state1)
	roll1 := openRollupStore(t, rollDB)
	publish(t, bus1, durableCostEvents(a, []float64{0.01, 0.02, 0.03, 0.04, 0.05})...)
	w1, err := projectorworker.New(src1, roll1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w1.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q1, err := w1.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q1.Watermark != 5 {
		t.Fatalf("run-1 watermark = %d; want 5", q1.Watermark)
	}
	if err := bus1.Close(ctx); err != nil {
		t.Fatalf("bus1.Close: %v", err)
	}
	if err := roll1.Close(ctx); err != nil {
		t.Fatalf("roll1.Close: %v", err)
	}
	if err := state1.Close(ctx); err != nil {
		t.Fatalf("state1.Close: %v", err)
	}

	// Run 2: restart. The fresh worker reads the durable checkpoint (5)
	// and must NOT re-apply events 1..5.
	state2 := openStateStore(t, stateDB)
	bus2, src2 := openDurableBus(t, state2)
	roll2 := openRollupStore(t, rollDB)
	defer func() {
		_ = bus2.Close(ctx)
		_ = state2.Close(ctx)
		_ = roll2.Close(ctx)
	}()
	w2, err := projectorworker.New(src2, roll2)
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	q2, err := w2.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality (restart): %v", err)
	}
	if q2.Watermark != 5 {
		t.Fatalf("restart watermark = %d; want 5 (durable checkpoint)", q2.Watermark)
	}
	if err := w2.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp (restart): %v", err)
	}
	q2, err = w2.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q2.State != rollups.StateCurrent {
		t.Fatalf("restart state = %q; want current", q2.State)
	}

	// No double-count across the restart: 5 events once.
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCompletions, 5)
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCostMicros, 150_000)

	// Events published after the restart (6, 7) drain on top.
	publish(t, bus2, costEvent(base.Add(6*time.Minute), a, "model-r", 0.06),
		costEvent(base.Add(7*time.Minute), a, "model-r", 0.07))
	if err := w2.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp after new events: %v", err)
	}
	q2, err = w2.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q2.Watermark != 7 {
		t.Fatalf("watermark after new events = %d; want 7", q2.Watermark)
	}
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCompletions, 7)
	assertMeasure(t, roll2, sessionFilter("session-a", "model-r"), rollups.MeasureLLMCostMicros, 280_000)
}
