package protocol

import (
	"context"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/sessions"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksinprocess "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// passRedactor is a no-op redactor — the cost payload is SafePayload
// (redaction is skipped for it), so a passthrough is sufficient and is NOT
// a subsystem re-implementation (§17.4).
type passRedactor struct{}

func (passRedactor) Redact(_ context.Context, p any) (any, error) { return p, nil }

// newReplayBus builds a real inmem event bus with a replay ring so
// HistoryReplayer.ListWindow serves the per-session scan. ringSize 0 yields
// a bus whose ListWindow returns ErrReplayUnavailable (the honest-partial
// path).
func newReplayBus(t *testing.T, ringSize int) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         ringSize,
	}
	bus, err := eventsinmem.New(cfg, passRedactor{})
	if err != nil {
		t.Fatalf("inmem bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newTaskRegistry(t *testing.T, bus events.EventBus) tasks.TaskRegistry {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	reg, err := tasksinprocess.New(tasks.Dependencies{
		Store:    st,
		Bus:      bus,
		Redactor: passRedactor{},
		Cfg:      config.TasksConfig{},
	})
	if err != nil {
		t.Fatalf("task registry: %v", err)
	}
	return reg
}

func sid(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

func publishCost(t *testing.T, bus events.EventBus, id identity.Identity, dollars float64, tokens int) {
	t.Helper()
	err := bus.Publish(context.Background(), events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: time.Now(),
		Payload: llm.CostRecordedPayload{
			Identity: identity.Quadruple{Identity: id},
			Model:    "test-model",
			Cost:     llm.Cost{TotalCost: dollars, Currency: "USD"},
			Usage:    llm.Usage{TotalTokens: tokens},
		},
	})
	if err != nil {
		t.Fatalf("publish cost: %v", err)
	}
}

func newEnricher(t *testing.T, bus events.EventBus, reg tasks.TaskRegistry, pauses pauseresume.Coordinator) *CounterEnricher {
	t.Helper()
	enr, err := NewCounterEnricher(CounterEnricherDeps{Bus: bus, Tasks: reg, Pauses: pauses})
	if err != nil {
		t.Fatalf("NewCounterEnricher: %v", err)
	}
	return enr
}

func TestCounterEnricher_NilDeps_FailLoud(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 32)
	reg := newTaskRegistry(t, bus)
	pauses := pauseresume.New()
	for name, deps := range map[string]CounterEnricherDeps{
		"nil-bus":    {Tasks: reg, Pauses: pauses},
		"nil-tasks":  {Bus: bus, Pauses: pauses},
		"nil-pauses": {Bus: bus, Tasks: reg},
	} {
		if _, err := NewCounterEnricher(deps); err == nil {
			t.Errorf("%s: NewCounterEnricher should fail loud on a missing dep", name)
		}
	}
}

func TestCounterEnricher_Cost_Tokens_Events_Summed(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	target := sid("t1", "u1", "s1")
	other := sid("t1", "u1", "s2")
	publishCost(t, bus, target, 1.00, 500) // 100c
	publishCost(t, bus, target, 2.50, 300) // 250c
	publishCost(t, bus, other, 9.99, 9999) // cross-session — must NOT bleed

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 350 {
		t.Errorf("TotalCostCents = %d, want 350 (1.00 + 2.50)", c.TotalCostCents)
	}
	if c.TotalTokens != 800 {
		t.Errorf("TotalTokens = %d, want 800", c.TotalTokens)
	}
	if c.EventsCount != 2 {
		t.Errorf("EventsCount = %d, want 2 (only s1's events)", c.EventsCount)
	}
	if c.Partial {
		t.Error("Partial = true, want false (scan did not truncate)")
	}
}

func TestCounterEnricher_Tasks_FailedTask_Scoped(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	target := sid("t1", "u1", "s1")
	other := sid("t2", "u9", "s9")
	// One failed task under the target session.
	spawnFailed(t, reg, target)
	// A task under a DIFFERENT session — must not bleed into the target.
	spawnFailed(t, reg, other)

	c := enr.Counters(context.Background(), target, "s1")
	if c.TasksCount != 1 {
		t.Errorf("TasksCount = %d, want 1 (session-scoped, no cross-session bleed)", c.TasksCount)
	}
	if !c.HasFailedTask {
		t.Error("HasFailedTask = false, want true")
	}
}

func TestCounterEnricher_PendingIntervention_Scoped(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	pauses := pauseresume.New()
	enr := newEnricher(t, bus, reg, pauses)

	target := sid("t1", "u1", "s1")
	if _, err := pauses.Request(context.Background(), pauseresume.PauseRequest{
		Identity: target,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("pause request: %v", err)
	}

	c := enr.Counters(context.Background(), target, "s1")
	if !c.HasPendingIntervention {
		t.Error("HasPendingIntervention = false, want true")
	}
	// A different session sees no intervention.
	c2 := enr.Counters(context.Background(), sid("t1", "u1", "s-other"), "s-other")
	if c2.HasPendingIntervention {
		t.Error("cross-session intervention bleed — s-other must have no pending pause")
	}
}

func TestCounterEnricher_Truncation_SetsPartial_HonestLowerBound(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())
	enr.scanBound = 2 // force the per-session scan to truncate

	target := sid("t1", "u1", "s1")
	publishCost(t, bus, target, 1.00, 100)
	publishCost(t, bus, target, 1.00, 100)
	publishCost(t, bus, target, 1.00, 100) // 3 > bound 2

	c := enr.Counters(context.Background(), target, "s1")
	if !c.Partial {
		t.Fatal("Partial = false, want true — a truncated scan MUST surface an honest lower bound (D-309 WARN-1), not a silent undercount")
	}
	if c.EventsCount != 2 {
		t.Errorf("EventsCount = %d, want 2 (the bounded lower bound)", c.EventsCount)
	}
	// The counts are a lower bound, not the true total (300c) — the honest
	// signal is Partial, never a plausible-but-false exact number.
	if c.TotalCostCents >= 300 {
		t.Errorf("TotalCostCents = %d; a truncated scan must not report the exact total", c.TotalCostCents)
	}
}

func TestCounterEnricher_NoReplaySubstrate_HonestPartial(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 0) // ring disabled ⇒ ListWindow ErrReplayUnavailable
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	c := enr.Counters(context.Background(), sid("t1", "u1", "s1"), "s1")
	if !c.Partial {
		t.Error("Partial = false, want true — an unavailable windowed-read substrate is honest-partial, never a silent zero (D-311)")
	}
	if c.TotalCostCents != 0 || c.EventsCount != 0 {
		t.Errorf("bus-derived counts should be zero-with-Partial, got cost=%d events=%d", c.TotalCostCents, c.EventsCount)
	}
}

func TestCounterEnricher_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	// D-025: N≥100 concurrent Counters against ONE shared enricher.
	bus := newReplayBus(t, 512)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	a := sid("t1", "u1", "sA")
	b := sid("t2", "u2", "sB")
	publishCost(t, bus, a, 1.00, 100) // sA: 100c / 100 tok / 1 event
	publishCost(t, bus, b, 5.00, 500) // sB: 500c / 500 tok / 1 event

	// Baseline the goroutine count once it has STABILISED (an eventually-
	// style poll, not a fixed sleep-as-sync — §17.4): the enricher and its
	// drivers spawn no per-call goroutines, but setup goroutines may still
	// be settling. Publish is synchronous on the inmem bus, so the events
	// are already retained — no wait for an async event is needed.
	baseline := settledGoroutineCount()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan string, N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			want := a
			wantCost := int64(100)
			if n%2 == 1 {
				want = b
				wantCost = 500
			}
			c := enr.Counters(context.Background(), want, want.SessionID)
			if c.TotalCostCents != wantCost {
				errCh <- "context bleed: session " + want.SessionID + " got wrong cost"
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d above baseline after %d concurrent Counters", leaked, N)
	}
}

// TestProjectRow_FieldSetPin pins the real projector's output field-set
// (§17.8): projectRow produces ONLY the lifecycle fields; it NEVER
// populates the agent binding (nil) or any counter (zero). A fixture or a
// future change that makes projectRow richer than this fails the test.
func TestProjectRow_FieldSetPin(t *testing.T) {
	t.Parallel()
	snap := sessions.SessionSnapshot{
		Session: sessions.Session{
			ID:       "s1",
			Identity: sid("t1", "u1", "s1"),
			OpenedAt: time.Now().Add(-time.Hour),
			LastSeen: time.Now(),
			Title:    "My Session",
		},
		Running: true,
	}
	row := projectRow(snap)

	// Lifecycle fields ARE populated.
	if row.SessionID != "s1" || row.Title != "My Session" {
		t.Fatalf("lifecycle projection wrong: %+v", row)
	}
	// Agent binding is NIL — no single-valued binding exists (representable
	// absence, D-311). A fixture that set these would be richer than the
	// producer (the HA-20 class, polarity flipped).
	if row.AgentID != nil || row.AgentName != nil {
		t.Errorf("projectRow populated an agent field (%v/%v) — the producer cannot; a fixture richer than the runtime is the defect this pins (§17.8)", row.AgentID, row.AgentName)
	}
	// Counters are ZERO — the Enricher overlays them; projectRow never does.
	if row.TasksCount != 0 || row.EventsCount != 0 || row.TotalCostCents != 0 ||
		row.TotalTokens != 0 || row.HasPendingIntervention || row.HasFailedTask || row.CountersPartial {
		t.Errorf("projectRow populated a counter field — counters are the Enricher's job, not the projector's: %+v", row)
	}
}

func TestCostFromEvent_TypedAndRedactedMap(t *testing.T) {
	t.Parallel()
	// costFromEvent returns DOLLARS (unrounded) so the caller can sum before
	// the single dollars→cents rounding (a per-event round would floor
	// sub-cent per-call costs to 0 — FAIL-1).
	// Typed SafePayload path (inmem live).
	typed := events.Event{Payload: llm.CostRecordedPayload{
		Cost:  llm.Cost{TotalCost: 1.23},
		Usage: llm.Usage{TotalTokens: 42},
	}}
	if d, tokens := costFromEvent(typed); math.Abs(d-1.23) > 1e-9 || tokens != 42 {
		t.Errorf("typed path: dollars=%v tokens=%d, want 1.23/42", d, tokens)
	}
	// RedactedMap path (durable replay stores a generic JSON object).
	redacted := events.Event{Payload: events.RedactedMap{Data: map[string]any{
		"Cost":  map[string]any{"TotalCost": float64(2.50)},
		"Usage": map[string]any{"TotalTokens": float64(300)},
	}}}
	if d, tokens := costFromEvent(redacted); math.Abs(d-2.50) > 1e-9 || tokens != 300 {
		t.Errorf("redacted-map path: dollars=%v tokens=%d, want 2.50/300", d, tokens)
	}
	// Unrecognised payload contributes zero (no fabricated cost).
	if d, tokens := costFromEvent(events.Event{}); d != 0 || tokens != 0 {
		t.Errorf("nil payload: dollars=%v tokens=%d, want 0/0", d, tokens)
	}
}

// TestCounterEnricher_SubCentCosts_NotFlooredToZero is the FAIL-1 guard: a
// session with many sub-cent per-call costs must sum to its true total, not
// a believable-but-false EXACT zero (which cost_above / cost_desc would
// then silently exclude / mis-rank — the false-absence this phase closes).
func TestCounterEnricher_SubCentCosts_NotFlooredToZero(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 128)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	target := sid("t1", "u1", "s1")
	// 50 calls at $0.004 each = $0.20 = 20¢. A per-event round would floor
	// each to 0 and report 0¢; the page-level sum must report 20¢.
	for range 50 {
		publishCost(t, bus, target, 0.004, 3)
	}
	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 20 {
		t.Fatalf("TotalCostCents = %d, want 20 (50 × $0.004 summed before rounding) — sub-cent per-event rounding floored a real cost to a false zero (FAIL-1)", c.TotalCostCents)
	}
	if c.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", c.TotalTokens)
	}
}

// TestCounterEnricher_DurableReplay_RealEncoder is the WARN-2 guard: the
// cost / tokens extraction on the DURABLE replay path (RedactedMap) is
// driven through the REAL durable encoder (Publish → durable record store →
// ListWindow → decode → costFromEvent), not a hand-authored map. A future
// json tag on CostRecordedPayload / Cost / Usage that silently broke
// costFromMap would surface here rather than shipping 0-cost on every
// durable / SQLite / Postgres runtime.
func TestCounterEnricher_DurableReplay_RealEncoder(t *testing.T) {
	t.Parallel()
	st, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	cfg := config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         0, // durable mode: ListWindow reads the log, not a ring
	}
	bus, err := durable.New(context.Background(), cfg, passRedactor{}, st)
	if err != nil {
		t.Fatalf("durable bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	target := sid("t1", "u1", "s1")
	publishCost(t, bus, target, 1.50, 600) // 150c
	publishCost(t, bus, target, 0.004, 4)  // sub-cent, exercises the sum on the durable path too

	c := enr.Counters(context.Background(), target, "s1")
	// $1.504 → 150¢ (0.4¢ rounds down). The point: the durable RedactedMap
	// path reads real cost/tokens, not zeros.
	if c.TotalCostCents != 150 {
		t.Fatalf("durable-replay TotalCostCents = %d, want 150 — the real durable encoder round-trip must read cost, not zero (WARN-2)", c.TotalCostCents)
	}
	if c.TotalTokens != 604 {
		t.Errorf("durable-replay TotalTokens = %d, want 604", c.TotalTokens)
	}
}

func TestDollarsToCents_RoundsAndClamps(t *testing.T) {
	t.Parallel()
	if dollarsToCents(0) != 0 || dollarsToCents(-5) != 0 {
		t.Error("non-positive dollars must clamp to 0 cents (a cost is never negative)")
	}
	if got := dollarsToCents(9.999); got != 1000 {
		t.Errorf("9.999 → %d cents, want 1000", got)
	}
	if got := dollarsToCents(0.01); got != 1 {
		t.Errorf("0.01 → %d cents, want 1", got)
	}
}

// settledGoroutineCount returns runtime.NumGoroutine once it has been
// stable across consecutive short reads (or a bounded deadline elapses) —
// an eventually-style stabilisation, NOT a fixed sleep used as a
// synchronisation primitive (§17.4). Used to baseline the leak assertion
// after setup goroutines have quiesced.
func settledGoroutineCount() int {
	deadline := time.Now().Add(2 * time.Second)
	prev := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

// spawnFailed spawns a task under id and transitions it to Failed.
func spawnFailed(t *testing.T, reg tasks.TaskRegistry, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        tasks.KindBackground,
		Description: "test task",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "boom", Message: "failed"}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
}

// TestCounterEnricher_UnreadableRegistryRead_SetsPartial — the partial
// marker covers the REGISTRY reads, not only the bounded event scan.
//
// A rollup that could not take a read leaves TasksCount / HasFailedTask /
// HasPendingIntervention at their zero values. A zero that means "we
// could not look" and a zero that means "we looked and there were none"
// are different answers, and a caller sorting or faceting on the counter
// cannot tell them apart unless the row says so.
//
// The unreadable case here is a row whose tenant the rollup may not
// reach: the ctx carries a verified anchor in one tenant, the row belongs
// to another, and no audited crossing is in force.
func TestCounterEnricher_UnreadableRegistryRead_SetsPartial(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	anchored, err := identity.WithVerified(context.Background(), sid("t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	// A row the rollup cannot re-scope to: the enricher's own audited
	// re-scope is what makes a foreign row readable, so a row it may not
	// reach at all must report itself unmeasured.
	unreachable := identity.Identity{TenantID: "t-other", UserID: "", SessionID: "s9"}

	c := enr.Counters(anchored, unreachable, "s9")
	if !c.Partial {
		t.Error("Partial = false, want true — a registry read the rollup could not take must be marked, never reported as an exact zero")
	}
	if c.TasksCount != 0 {
		t.Errorf("TasksCount = %d, want 0 — an unreadable count stays zero, but Partial is what says so", c.TasksCount)
	}
}

// TestCounterEnricher_ForeignRowUnderAnAnchorIsReadInFull — the
// companion: under the claim a fleet listing carries, the rollup's audited
// re-scope makes a foreign-tenant row readable, so it is NOT partial.
// Without this pin the test above would pass for a rollup that simply
// never reads anything.
func TestCounterEnricher_ForeignRowUnderAnAnchorIsReadInFull(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	c := enr.Counters(fleetCtx(t, sid("t1", "u1", "s1")), sid("t-other", "u9", "s9"), "s9")
	if c.Partial {
		t.Error("Partial = true for a complete foreign row, want false — the rollup re-scopes to the row's own identity and reads it in full")
	}
}

// TestCounterEnricher_ForeignRowWithoutTheClaimIsRefusedAndMarked — the
// rollup re-checks the admin-tier claim where it mints the crossing rather
// than inheriting the listing's check. A foreign row on a request carrying
// no claim is refused, and the row says its counters are unmeasured
// instead of reporting zeros.
func TestCounterEnricher_ForeignRowWithoutTheClaimIsRefusedAndMarked(t *testing.T) {
	t.Parallel()
	bus := newReplayBus(t, 64)
	reg := newTaskRegistry(t, bus)
	enr := newEnricher(t, bus, reg, pauseresume.New())

	anchored, err := identity.WithVerified(context.Background(), sid("t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	c := enr.Counters(anchored, sid("t-other", "u9", "s9"), "s9")
	if !c.Partial {
		t.Error("Partial = false for a foreign row the rollup may not read; an unentitled crossing must leave the counters unmeasured, not zeroed")
	}
	if c.TasksCount != 0 {
		t.Errorf("TasksCount = %d, want 0 — the refused read reports nothing", c.TasksCount)
	}
}

// fleetCtx seats the verified identity AND the admin-tier claim a fleet
// listing carries, which is the shape the rollup sees in production when
// it is handed a foreign row.
func fleetCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
}
