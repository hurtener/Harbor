package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// fakeProjectionQuality is a controllable ProjectionQuality double: it
// returns exactly the Quality the test wires, so freshness scenarios
// (current / catching_up / unavailable / a quality-read failure / a
// retention horizon) are exercised deterministically.
type fakeProjectionQuality struct {
	q   rollups.Quality
	err error
}

func (f fakeProjectionQuality) Quality(context.Context) (rollups.Quality, error) { return f.q, f.err }

// countingFallbackEnricher records every invocation the adapter makes of the
// raw bounded scan and returns the wired result — a shared `result`, or a
// per-session result when `bySession` is set — so a test can assert both
// "the raw scan was invoked" and "its result was merged / returned
// verbatim".
type countingFallbackEnricher struct {
	mu          sync.Mutex
	calls       int
	lastID      identity.Identity
	lastSession string
	result      SessionCounters
	bySession   map[string]SessionCounters // optional per-session raw results
}

func (f *countingFallbackEnricher) Counters(_ context.Context, id identity.Identity, sessionID string) SessionCounters {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastID = id
	f.lastSession = sessionID
	if r, ok := f.bySession[sessionID]; ok {
		return r
	}
	return f.result
}

func (f *countingFallbackEnricher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// applyRollupBatch applies one delta batch to the store. The checkpoint
// must advance across calls in a test (ApplyBatch no-ops on a
// non-advancing checkpoint — the replay-idempotency invariant); each
// test applies exactly one batch to a fresh store, so the first
// checkpoint is always 1.
func applyRollupBatch(t *testing.T, store *memstore.Store, deltas ...rollups.Delta) {
	t.Helper()
	if err := store.ApplyBatch(context.Background(), rollups.Batch{Checkpoint: 1, Deltas: deltas}); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
}

// costDelta builds the rollup delta of one successful `llm.cost.recorded`
// event: one completion, the exact micro cost, and the total tokens.
func costDelta(id identity.Identity, at time.Time, usd float64, tokens int) rollups.Delta {
	return rollups.Delta{
		Key: rollups.Key{
			BucketStart: rollups.BucketStart(at, rollups.BucketMinute),
			TenantID:    id.TenantID,
			UserID:      id.UserID,
			SessionID:   id.SessionID,
			Model:       "test-model",
		},
		Add: rollups.MeasureSet{
			LLMCompletions:  1,
			LLMCostMicros:   int64(math.Round(usd * float64(rollups.CostScaleMicros))),
			LLMTokensTotal:  int64(tokens),
			LLMLatencyCount: 1,
		},
	}
}

// taskOutcomeDelta builds the rollup delta of one task outcome event.
func taskOutcomeDelta(id identity.Identity, at time.Time, outcome string) rollups.Delta {
	var add rollups.MeasureSet
	switch outcome {
	case "completed":
		add.TasksCompleted = 1
	case "failed":
		add.TasksFailed = 1
	case "cancelled":
		add.TasksCancelled = 1
	default:
		panic("taskOutcomeDelta: unknown outcome " + outcome)
	}
	return rollups.Delta{
		Key: rollups.Key{
			BucketStart: rollups.BucketStart(at, rollups.BucketMinute),
			TenantID:    id.TenantID,
			UserID:      id.UserID,
			SessionID:   id.SessionID,
		},
		Add: add,
	}
}

// clockAt returns a fixed "now" for the adapter's window end.
func clockAt(now time.Time) func() time.Time { return func() time.Time { return now } }

// sessionWindow resolves the given window for any session.
func sessionWindow(openedAt, lastActivityAt time.Time) SessionWindowFunc {
	return func(context.Context, identity.Identity, string) (time.Time, time.Time, bool, error) {
		return openedAt, lastActivityAt, true, nil
	}
}

// newProjectionEnricher assembles a ProjectionEnricher over the given
// doubles with a silent logger (assertions, not diagnostics, carry the
// test's verdicts). The raw bounded scan (Fallback) is the canonical source
// of events / tasks / pending on the projection-backed path, so every
// assembled adapter requires one.
func newProjectionEnricher(
	t *testing.T,
	store *memstore.Store,
	quality ProjectionQuality,
	fallback Enricher,
	window SessionWindowFunc,
	clock func() time.Time,
) *ProjectionEnricher {
	t.Helper()
	enr, err := NewProjectionEnricher(ProjectionEnricherDeps{
		Store:    store,
		Quality:  quality,
		Fallback: fallback,
		Window:   window,
		Clock:    clock,
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewProjectionEnricher: %v", err)
	}
	return enr
}

// testLogger returns a discard slog.Logger so fallback warnings never spam
// test output (each test asserts the fallback behavior itself).
func testLogger(t *testing.T) *slog.Logger { return slog.New(slog.DiscardHandler) }

// currentQuality builds a `current` projection quality whose retained
// horizon starts at the session's opening bucket (full coverage).
func currentQuality(openedAt time.Time) fakeProjectionQuality {
	return fakeProjectionQuality{q: rollups.Quality{
		State:          rollups.StateCurrent,
		Watermark:      100,
		RetentionStart: rollups.BucketStart(openedAt, rollups.BucketMinute),
		RetentionEnd:   rollups.BucketStart(openedAt, rollups.BucketMinute).Add(24 * time.Hour),
	}}
}

func TestProjectionEnricher_NilDeps_FailLoud(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	base := ProjectionEnricherDeps{
		Store:    memstore.New(),
		Quality:  currentQuality(opened),
		Fallback: &countingFallbackEnricher{},
		Window:   sessionWindow(opened, opened),
		Clock:    clockAt(opened.Add(time.Hour)),
	}
	for name, mut := range map[string]func(*ProjectionEnricherDeps){
		"nil-store":    func(d *ProjectionEnricherDeps) { d.Store = nil },
		"nil-quality":  func(d *ProjectionEnricherDeps) { d.Quality = nil },
		"nil-fallback": func(d *ProjectionEnricherDeps) { d.Fallback = nil },
		"nil-window":   func(d *ProjectionEnricherDeps) { d.Window = nil },
	} {
		deps := base
		mut(&deps)
		if _, err := NewProjectionEnricher(deps); err == nil {
			t.Errorf("%s: NewProjectionEnricher must fail loud on a missing dep", name)
		}
	}
}

// TestProjectionEnricher_Current_MergedDivergentCounters is the primary
// corrected contract: when the projection is CURRENT and its retained
// horizon COVERS the session, the rollup is the DETERMINISTIC MERGE of the
// two authoritative sources, proven with intentionally DIVERGENT numbers —
// neither source may leak into the other's dimensions:
//
//   - The projection backs EXACTLY cost (micros→cents), total tokens, and
//     HasFailedTask (failed terminal outcomes). The session's 2
//     llm_completions are a SUBSET of its emitted events and its 1
//     terminal outcome is NOT its spawned-task count, so neither may
//     present as events_count / tasks_count.
//   - The raw bounded scan backs the canonical totals: 11 events emitted
//     and 7 spawned tasks, plus has_pending_intervention.
//   - The raw scan's divergent cost / tokens / failed-task lower bounds
//     never overwrite the projection's exact values, and the projection's
//     subset counts never overwrite the raw scan's canonical events /
//     tasks.
//   - Partial stays false: every raw read succeeded, so the aggregate is
//     current.
func TestProjectionEnricher_Current_MergedDivergentCounters(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(60 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store,
		costDelta(target, opened, 1.00, 300),                          // 100c / 300 tok / 1 completion
		costDelta(target, opened.Add(5*time.Minute), 2.00, 200),       // 200c / 200 tok / 2nd completion
		taskOutcomeDelta(target, opened.Add(6*time.Minute), "failed"), // 1 terminal outcome — a FAILED one
	)

	raw := SessionCounters{
		EventsCount:            11, // 11 events emitted — NOT the 2 completions
		TasksCount:             7,  // 7 tasks spawned — NOT the 1 terminal outcome
		HasPendingIntervention: true,
		// Deliberately divergent from the projection's exact values: the
		// raw scan's lower bounds must NEVER overwrite the projection's
		// exact cost / tokens / failed-task.
		TotalCostCents: 999,
		TotalTokens:    9999,
		HasFailedTask:  false,
	}

	fallback := &countingFallbackEnricher{result: raw}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	// Projection-backed (exact) — never replaced by the raw lower bounds.
	if c.TotalCostCents != 300 {
		t.Errorf("TotalCostCents = %d, want 300 (projection-backed; the raw scan's 999 must not leak in)", c.TotalCostCents)
	}
	if c.TotalTokens != 500 {
		t.Errorf("TotalTokens = %d, want 500 (projection-backed; the raw scan's 9999 must not leak in)", c.TotalTokens)
	}
	if !c.HasFailedTask {
		t.Error("HasFailedTask = false, want true — the projection's failed terminal outcome is authoritative, not the raw scan's false")
	}
	// Raw-backed (canonical) — never replaced by the projection subsets.
	if c.EventsCount != 11 {
		t.Errorf("EventsCount = %d, want 11 (canonical events emitted from the raw scan, NOT the 2 completions)", c.EventsCount)
	}
	if c.TasksCount != 7 {
		t.Errorf("TasksCount = %d, want 7 (canonical spawned tasks from the raw scan, NOT the 1 terminal outcome)", c.TasksCount)
	}
	if !c.HasPendingIntervention {
		t.Error("HasPendingIntervention = false, want true (from the raw scan)")
	}
	if c.Partial {
		t.Error("Partial = true, want false — a current, covering projection with a complete raw read is exact")
	}
	if got := fallback.called(); got != 1 {
		t.Errorf("fallback delegations = %d, want exactly 1 — the projection-backed path reads events/tasks/pending from the raw scan", got)
	}
}

// TestProjectionEnricher_Current_RetentionBoundaryIsCovered pins the
// coverage boundary: a retention horizon that starts EXACTLY at the
// session's opening bucket covers the session (BucketStart(openedAt) is
// NOT before RetentionStart) — only a horizon that starts AFTER the
// opening bucket is a gap. The merge path still consults the raw scan for
// the unmodelled dimensions; a raw result of measured zeros keeps the
// rollup exact.
func TestProjectionEnricher_Current_RetentionBoundaryIsCovered(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store, costDelta(target, opened, 0.25, 50))

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 25 {
		t.Errorf("TotalCostCents = %d, want 25 (boundary-covered session reads exactly)", c.TotalCostCents)
	}
	if c.Partial {
		t.Error("Partial = true, want false — a complete raw read next to an exactly-covered projection is exact")
	}
	if got := fallback.called(); got != 1 {
		t.Errorf("fallback delegations = %d, want exactly 1 (the projection-backed merge)", got)
	}
}

// TestProjectionEnricher_Current_NoRows_MeasuredExactZeros pins that a
// current, covering projection with NO rows for the session returns
// MEASURED zeros for the projection-backed dimensions — Partial=false when
// the raw scan also read in full — never a fabricated or degraded value:
// the projection observed none of the session's supported measures, which
// is a real measurement, not missing data. The raw scan still supplies the
// canonical events / tasks / pending (its zeros here are exact because it
// is not partial).
func TestProjectionEnricher_Current_NoRows_MeasuredExactZeros(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")
	other := sid("t1", "u1", "s-other")

	store := memstore.New()
	applyRollupBatch(t, store, costDelta(other, opened, 5.00, 500)) // rows exist, but not for target

	fallback := &countingFallbackEnricher{} // raw scan: measured zeros, not partial
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.Partial {
		t.Error("Partial = true, want false — zero projection rows for a covered session is a measured zero and the raw scan read in full")
	}
	if c.TotalCostCents != 0 || c.TotalTokens != 0 || c.HasFailedTask {
		t.Errorf("measured zeros expected for the projection-backed dimensions, got cost=%d tokens=%d failed=%v",
			c.TotalCostCents, c.TotalTokens, c.HasFailedTask)
	}
	if c.EventsCount != 0 || c.TasksCount != 0 || c.HasPendingIntervention {
		t.Errorf("raw-backed zeros expected for a session with no events / tasks / pauses, got events=%d tasks=%d pending=%v",
			c.EventsCount, c.TasksCount, c.HasPendingIntervention)
	}
	if got := fallback.called(); got != 1 {
		t.Errorf("fallback delegations = %d, want exactly 1 — the merge path still reads events/tasks/pending from the raw scan", got)
	}
}

// TestProjectionEnricher_SubCentCosts_ExactDeterministicCents is the
// cost-conversion honesty guard: the rollup sums exact integer micro-units
// and converts to cents ONCE at the end (round-half-up), so a session of
// sub-cent per-call costs sums to its true cent total instead of flooring
// every call to zero — the same false-absence the raw enricher closes.
// The raw scan's canonical events / tasks are deliberately divergent from
// the 50 completions, proving events_count never reads as the completion
// count.
func TestProjectionEnricher_SubCentCosts_ExactDeterministicCents(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(60 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	deltas := make([]rollups.Delta, 0, 50)
	for i := range 50 {
		deltas = append(deltas, costDelta(target, opened.Add(time.Duration(i)*time.Minute), 0.004, 3))
	}
	applyRollupBatch(t, store, deltas...)

	fallback := &countingFallbackEnricher{result: SessionCounters{EventsCount: 11, TasksCount: 7}}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	// 50 × $0.004 = $0.20 = 20 whole cents — never 0.
	if c.TotalCostCents != 20 {
		t.Errorf("TotalCostCents = %d, want 20 (50 sub-cent calls must sum to their true cent total, not floor to 0)", c.TotalCostCents)
	}
	if c.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150 (50 × 3)", c.TotalTokens)
	}
	if c.EventsCount != 11 {
		t.Errorf("EventsCount = %d, want 11 — events_count is the raw scan's canonical total, never the 50-completion subset", c.EventsCount)
	}
	if c.TasksCount != 7 {
		t.Errorf("TasksCount = %d, want 7 (the raw scan's spawned-task total)", c.TasksCount)
	}
}

// TestProjectionEnricher_FallsBackToRawScan pins the honest-fallback
// contract: every projection state that cannot prove current-and-covering
// delegations to the raw bounded scan, returns its result VERBATIM (its own
// Partial marking rides along — never replaced by fabricated zeros), and is
// observable through the fallback's call count. The rows each case reports
// are the raw result, including a raw Partial.
func TestProjectionEnricher_FallsBackToRawScan(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")
	store := memstore.New()
	applyRollupBatch(t, store, costDelta(target, opened, 1.00, 100))

	raw := SessionCounters{
		TasksCount:             7,
		EventsCount:            8,
		TotalCostCents:         900,
		TotalTokens:            1000,
		HasPendingIntervention: true,
		HasFailedTask:          true,
		Partial:                true, // the raw scan's own honesty marker rides along
	}

	cases := map[string]struct {
		quality ProjectionQuality
		window  SessionWindowFunc
		close   bool
	}{
		"catching_up": {
			quality: fakeProjectionQuality{q: rollups.Quality{
				State: rollups.StateCatchingUp, RetentionStart: rollups.BucketStart(opened, rollups.BucketMinute),
			}},
			window: sessionWindow(opened, now),
		},
		"unavailable": {
			quality: fakeProjectionQuality{q: rollups.Quality{
				State: rollups.StateUnavailable, Err: context.DeadlineExceeded,
				RetentionStart: rollups.BucketStart(opened, rollups.BucketMinute),
			}},
			window: sessionWindow(opened, now),
		},
		"quality-read-error": {
			quality: fakeProjectionQuality{err: context.Canceled},
			window:  sessionWindow(opened, now),
		},
		"retention-gap": {
			quality: fakeProjectionQuality{q: rollups.Quality{
				State: rollups.StateCurrent, RetentionStart: rollups.BucketStart(opened, rollups.BucketMinute).Add(time.Hour),
			}},
			window: sessionWindow(opened, now),
		},
		"window-unresolvable": {
			quality: currentQuality(opened),
			window: func(context.Context, identity.Identity, string) (time.Time, time.Time, bool, error) {
				return time.Time{}, time.Time{}, false, nil
			},
		},
		"window-error": {
			quality: currentQuality(opened),
			window: func(context.Context, identity.Identity, string) (time.Time, time.Time, bool, error) {
				return time.Time{}, time.Time{}, false, context.Canceled
			},
		},
		"projection-query-failure": {
			quality: currentQuality(opened),
			window:  sessionWindow(opened, now),
			close:   true, // a closed store makes the rollup query fail loud
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := memstore.New()
			applyRollupBatch(t, s, costDelta(target, opened, 1.00, 100))
			if tc.close {
				if err := s.Close(context.Background()); err != nil {
					t.Fatalf("close store: %v", err)
				}
			}
			fallback := &countingFallbackEnricher{result: raw}
			enr := newProjectionEnricher(t, s, tc.quality, fallback, tc.window, clockAt(now))

			c := enr.Counters(context.Background(), target, "s1")
			if got := fallback.called(); got != 1 {
				t.Fatalf("fallback delegations = %d, want exactly 1 — %s must delegate to the raw bounded scan", got, name)
			}
			// The raw result is returned verbatim — its Partial included.
			if c != raw {
				t.Errorf("fallback result not returned verbatim:\n got %+v\nwant %+v", c, raw)
			}
			if fallback.lastSession != "s1" {
				t.Errorf("fallback received session %q, want s1 (the folded row identity)", fallback.lastSession)
			}
		})
	}
}

// TestProjectionEnricher_Current_RawPartialMarksAggregatePartial pins the
// merge's honesty marker: when the raw bounded scan reports Partial (a
// truncated event scan, an unreadable registry read), the aggregate is
// Partial even though the projection is current and covering — the
// projection's exact cost / tokens / failed-task ride along, but the
// row's CounterStatus is partial (events / tasks / pending are honest
// lower bounds), never current. The raw scan's honest zero-plus-Partial
// result is preserved verbatim — availability is never fabricated.
func TestProjectionEnricher_Current_RawPartialMarksAggregatePartial(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store,
		costDelta(target, opened, 1.00, 100),
		taskOutcomeDelta(target, opened.Add(time.Minute), "failed"),
	)

	// The raw scan could not be taken in full: its events / tasks / pending
	// are honest lower bounds (a zero means "we could not look", and the
	// marker says so), and its divergent cost / tokens lower bounds must
	// not leak into the projection's exact values.
	raw := SessionCounters{
		EventsCount:    2,   // lower bound — the scan truncated
		TotalCostCents: 777, // divergent lower bound — must not overwrite
		Partial:        true,
	}

	fallback := &countingFallbackEnricher{result: raw}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if !c.Partial {
		t.Error("Partial = false, want true — a partial raw read makes the aggregate partial, never current")
	}
	if c.EventsCount != 2 {
		t.Errorf("EventsCount = %d, want 2 (the raw scan's honest lower bound rides along)", c.EventsCount)
	}
	// The projection's exact values are preserved next to the raw's
	// partial marker.
	if c.TotalCostCents != 100 {
		t.Errorf("TotalCostCents = %d, want 100 — the raw lower bound (777) must not overwrite the projection's exact cost", c.TotalCostCents)
	}
	if c.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100 (projection-backed, preserved under the partial marker)", c.TotalTokens)
	}
	if !c.HasFailedTask {
		t.Error("HasFailedTask = false, want true — the projection's failed terminal outcome is preserved under the partial marker")
	}
}

// TestProjectionEnricher_UnwiredProjector_HonestUnavailable pins the
// no-enricher state of the projection: a ListerProjector with NO Enricher
// wired ships honest zeros marked CounterStatus=unavailable (the zeros mean
// "this build cannot provide them", never "measured as zero"), so an
// unwired build can never reproduce a false-empty counter page.
func TestProjectionEnricher_UnwiredProjector_HonestUnavailable(t *testing.T) {
	proj, err := NewListerProjector(catalogLister{snapshots: catalogSnapshots(2, "tenant-1", "user-1", "ne")})
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	if proj.CountersAvailable() {
		t.Error("CountersAvailable = true on an unwired projector, want false")
	}
	rows, err := proj.ListSessions(context.Background(), lifecycleCaller(), prototypes.SessionFilter{}, false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.CounterStatus != prototypes.CounterStatusUnavailable {
			t.Errorf("row %q CounterStatus = %q, want unavailable", r.SessionID, r.CounterStatus)
		}
		if r.TasksCount != 0 || r.EventsCount != 0 || r.TotalCostCents != 0 || r.TotalTokens != 0 ||
			r.HasPendingIntervention || r.HasFailedTask {
			t.Errorf("row %q carried counter data on an unwired projector: %+v", r.SessionID, r)
		}
	}
}

// TestProjectionEnricher_LifecycleProjection_ZeroEnricherCalls pins the
// lifecycle contract against THIS adapter: a projection=lifecycle list AND
// inspect serve from the catalog path before ANY Enricher call, so a wired
// ProjectionEnricher (and its raw fallback) is never invoked — the rows
// carry CounterStatus=not_requested, never a projection-backed value.
func TestProjectionEnricher_LifecycleProjection_ZeroEnricherCalls(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	store := memstore.New()
	fallback := &countingFallbackEnricher{result: SessionCounters{TasksCount: 7}}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	projector, err := NewListerProjector(catalogLister{snapshots: catalogSnapshots(20, "tenant-1", "user-1", "lc-proj")},
		WithEnricher(enr))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	scope := prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID}

	listResp, err := svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity:   scope,
		Projection: prototypes.SessionProjectionLifecycle,
		Filter:     prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusRunning}},
		Sort:       prototypes.SessionSortLastActivityDesc,
		Limit:      10,
	}, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := fallback.called(); got != 0 {
		t.Fatalf("fallback delegations = %d, want 0 — a lifecycle-only list must never touch the Enricher", got)
	}
	if len(listResp.Rows) != 10 {
		t.Fatalf("lifecycle page returned %d rows, want 10", len(listResp.Rows))
	}
	for _, r := range listResp.Rows {
		if r.CounterStatus != prototypes.CounterStatusNotRequested {
			t.Errorf("row %q CounterStatus = %q, want not_requested", r.SessionID, r.CounterStatus)
		}
	}

	inspResp, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity:   scope,
		SessionID:  "lc-proj-000",
		Projection: prototypes.SessionProjectionLifecycle,
	}, false)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := fallback.called(); got != 0 {
		t.Fatalf("fallback delegations = %d, want 0 — a lifecycle-only inspect must never touch the Enricher", got)
	}
	if inspResp.Row.CounterStatus != prototypes.CounterStatusNotRequested {
		t.Errorf("inspect row CounterStatus = %q, want not_requested", inspResp.Row.CounterStatus)
	}
	if inspResp.Row.TasksCount != 0 || inspResp.Row.TotalCostCents != 0 {
		t.Errorf("inspect row carried counter data on a lifecycle projection: %+v", inspResp.Row)
	}
}

// TestProjectionEnricher_Current_LongLivedSession_CoarsenedExact pins the
// adaptive bucket-size path: a session whose lifetime exceeds the rollup
// query's minute-bucket budget must still return EXACT projection-backed
// totals — the adapter coarsens the query to the hour / day grid, and
// coarsening never changes the summed measures. The raw scan's canonical
// events / tasks are deliberately divergent from the projection's 2
// completions / 2 terminal outcomes, proving the public counters never read
// as projection subsets.
func TestProjectionEnricher_Current_LongLivedSession_CoarsenedExact(t *testing.T) {
	opened := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(200 * 24 * time.Hour) // 200 days: 288k minutes > MaxBuckets, 4800h > MaxBuckets → day grid
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store,
		costDelta(target, opened, 1.00, 100),                                // 100c / 100 tok — at the window start
		costDelta(target, opened.Add(199*24*time.Hour), 2.50, 300),          // 250c / 300 tok — near the window end
		taskOutcomeDelta(target, opened.Add(100*24*time.Hour), "completed"), // a terminal outcome mid-window
		taskOutcomeDelta(target, opened.Add(100*24*time.Hour), "failed"),    // a failed terminal outcome
	)

	fallback := &countingFallbackEnricher{result: SessionCounters{EventsCount: 11, TasksCount: 7}}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 350 {
		t.Errorf("TotalCostCents = %d, want 350 — coarsening must not change the summed cost", c.TotalCostCents)
	}
	if c.TotalTokens != 400 {
		t.Errorf("TotalTokens = %d, want 400", c.TotalTokens)
	}
	if !c.HasFailedTask {
		t.Error("HasFailedTask = false, want true (the projection's failed terminal outcome)")
	}
	if c.EventsCount != 11 || c.TasksCount != 7 {
		t.Errorf("EventsCount = %d, TasksCount = %d, want 11/7 (raw-backed canonical totals on a coarsened query)", c.EventsCount, c.TasksCount)
	}
	if c.Partial {
		t.Error("Partial = true, want false — a covered long-lived session returns exact counters")
	}
	if got := fallback.called(); got != 1 {
		t.Errorf("fallback delegations = %d, want exactly 1", got)
	}
}

// TestProjectionEnricher_ConcurrentReuse_NoCrossTalk pins the D-025-style
// concurrent-reuse contract for the adapter: N≥100 concurrent Counters
// calls against ONE shared ProjectionEnricher under -race, with distinct
// sessions, must show no data races, no context bleed across EITHER seam
// (each session reads exactly its own projection-backed cost / tokens /
// failed-task AND its own raw-backed events / tasks / pending), no goroutine
// leak after the wave, and exactly one raw-scan invocation per read.
func TestProjectionEnricher_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(2 * time.Hour)
	a := sid("t1", "u1", "sA")
	b := sid("t2", "u2", "sB")

	store := memstore.New()
	applyRollupBatch(t, store,
		costDelta(a, opened, 1.00, 100), // sA projection: 100c / 100 tok
		costDelta(b, opened, 5.00, 500), // sB projection: 500c / 500 tok
		taskOutcomeDelta(a, opened.Add(time.Minute), "completed"),
		taskOutcomeDelta(a, opened.Add(2*time.Minute), "failed"),
	)

	// Each session's raw scan supplies ITS OWN canonical events / tasks /
	// pending — deliberately divergent from the projection-backed values so
	// cross-talk on either seam is caught.
	fallback := &countingFallbackEnricher{bySession: map[string]SessionCounters{
		a.SessionID: {EventsCount: 11, TasksCount: 7, HasPendingIntervention: true},
		b.SessionID: {EventsCount: 5, TasksCount: 3, HasPendingIntervention: false},
	}}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback,
		sessionWindow(opened, now), clockAt(now))

	baseline := settledGoroutineCount()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan string, N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			want := a
			if n%2 == 1 {
				want = b
			}
			c := enr.Counters(context.Background(), want, want.SessionID)
			switch want.SessionID {
			case a.SessionID:
				if c.TotalCostCents != 100 || c.TotalTokens != 100 || !c.HasFailedTask ||
					c.EventsCount != 11 || c.TasksCount != 7 || !c.HasPendingIntervention || c.Partial {
					errCh <- "context bleed: session sA got " + fmt.Sprintf("%+v", c)
				}
			case b.SessionID:
				if c.TotalCostCents != 500 || c.TotalTokens != 500 || c.HasFailedTask ||
					c.EventsCount != 5 || c.TasksCount != 3 || c.HasPendingIntervention || c.Partial {
					errCh <- "context bleed: session sB got " + fmt.Sprintf("%+v", c)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
	if got := fallback.called(); got != N {
		t.Errorf("fallback delegations = %d, want %d — every projection-backed read consults the raw scan for events/tasks/pending", got, N)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d above baseline after %d concurrent Counters", leaked, N)
	}
}
