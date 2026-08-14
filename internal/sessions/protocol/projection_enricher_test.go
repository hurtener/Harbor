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
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
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

// countingFallbackEnricher records every delegation the adapter makes to
// the raw bounded scan and returns a wired result, so a test can assert
// both "the fallback was invoked" and "its result was returned verbatim".
type countingFallbackEnricher struct {
	mu          sync.Mutex
	calls       int
	lastID      identity.Identity
	lastSession string
	result      SessionCounters
}

func (f *countingFallbackEnricher) Counters(_ context.Context, id identity.Identity, sessionID string) SessionCounters {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastID = id
	f.lastSession = sessionID
	return f.result
}

func (f *countingFallbackEnricher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// applyRollupBatch applies one delta batch to the store. The checkpoint
// must advance across calls in a test (ApplyBatch no-ops on a
// non-advancing checkpoint — the replay-idempotency invariant).
func applyRollupBatch(t *testing.T, store *memstore.Store, checkpoint uint64, deltas ...rollups.Delta) {
	t.Helper()
	if err := store.ApplyBatch(context.Background(), rollups.Batch{Checkpoint: checkpoint, Deltas: deltas}); err != nil {
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
// test's verdicts).
func newProjectionEnricher(
	t *testing.T,
	store *memstore.Store,
	quality ProjectionQuality,
	fallback Enricher,
	pauses pauseresume.Coordinator,
	window SessionWindowFunc,
	clock func() time.Time,
) *ProjectionEnricher {
	t.Helper()
	enr, err := NewProjectionEnricher(ProjectionEnricherDeps{
		Store:    store,
		Quality:  quality,
		Fallback: fallback,
		Pauses:   pauses,
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
		Pauses:   pauseresume.New(),
		Window:   sessionWindow(opened, opened),
		Clock:    clockAt(opened.Add(time.Hour)),
	}
	for name, mut := range map[string]func(*ProjectionEnricherDeps){
		"nil-store":    func(d *ProjectionEnricherDeps) { d.Store = nil },
		"nil-quality":  func(d *ProjectionEnricherDeps) { d.Quality = nil },
		"nil-fallback": func(d *ProjectionEnricherDeps) { d.Fallback = nil },
		"nil-pauses":   func(d *ProjectionEnricherDeps) { d.Pauses = nil },
		"nil-window":   func(d *ProjectionEnricherDeps) { d.Window = nil },
	} {
		deps := base
		mut(&deps)
		if _, err := NewProjectionEnricher(deps); err == nil {
			t.Errorf("%s: NewProjectionEnricher must fail loud on a missing dep", name)
		}
	}
}

// TestProjectionEnricher_Current_ExactProjectionBackedCounters is the
// primary projection-backed contract: when the projection is CURRENT and
// its retained horizon COVERS the session, the counters are EXACT
// projection-backed totals (no counters_partial, no raw scan, no fallback
// call), scoped to the session's own triple.
func TestProjectionEnricher_Current_ExactProjectionBackedCounters(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(90 * time.Minute)
	target := sid("t1", "u1", "s1")
	other := sid("t1", "u1", "s-other")

	store := memstore.New()
	applyRollupBatch(t, store, 1,
		costDelta(target, opened.Add(0*time.Minute), 0.50, 100), // 50c / 100 tok / 1 event
		costDelta(target, opened.Add(5*time.Minute), 1.00, 500), // 100c / 500 tok
		costDelta(target, opened.Add(6*time.Minute), 2.50, 300), // 250c / 300 tok
		costDelta(other, opened.Add(7*time.Minute), 9.99, 9999), // cross-session — must NOT bleed
		taskOutcomeDelta(target, opened.Add(8*time.Minute), "completed"),
		taskOutcomeDelta(target, opened.Add(9*time.Minute), "failed"),
		taskOutcomeDelta(target, opened.Add(10*time.Minute), "cancelled"),
	)

	pauses := pauseresume.New()
	if _, err := pauses.Request(context.Background(), pauseresume.PauseRequest{
		Identity: target,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("pause request: %v", err)
	}

	fallback := &countingFallbackEnricher{result: SessionCounters{TasksCount: 99}}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauses,
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 400 {
		t.Errorf("TotalCostCents = %d, want 400 (0.50 + 1.00 + 2.50)", c.TotalCostCents)
	}
	if c.TotalTokens != 900 {
		t.Errorf("TotalTokens = %d, want 900", c.TotalTokens)
	}
	if c.EventsCount != 3 {
		t.Errorf("EventsCount = %d, want 3 (the session's successful-completion events)", c.EventsCount)
	}
	if c.TasksCount != 3 {
		t.Errorf("TasksCount = %d, want 3 (completed + failed + cancelled)", c.TasksCount)
	}
	if !c.HasFailedTask {
		t.Error("HasFailedTask = false, want true (one failed task outcome)")
	}
	if !c.HasPendingIntervention {
		t.Error("HasPendingIntervention = false, want true (the pause read must still run on the projection path)")
	}
	if c.Partial {
		t.Error("Partial = true, want false — a current, covering projection returns EXACT counters, never counters_partial")
	}
	if got := fallback.called(); got != 0 {
		t.Errorf("fallback delegations = %d, want 0 — a current, covering projection must never touch the raw bounded scan", got)
	}
}

// TestProjectionEnricher_Current_RetentionBoundaryIsCovered pins the
// coverage boundary: a retention horizon that starts EXACTLY at the
// session's opening bucket covers the session (BucketStart(openedAt) is
// NOT before RetentionStart) — only a horizon that starts AFTER the
// opening bucket is a gap.
func TestProjectionEnricher_Current_RetentionBoundaryIsCovered(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store, 1, costDelta(target, opened, 0.25, 50))

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauseresume.New(),
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 25 {
		t.Errorf("TotalCostCents = %d, want 25 (boundary-covered session reads exactly)", c.TotalCostCents)
	}
	if got := fallback.called(); got != 0 {
		t.Errorf("fallback delegations = %d, want 0 — an exactly-covered session must not fall back", got)
	}
}

// TestProjectionEnricher_Current_NoRows_MeasuredExactZeros pins that a
// current, covering projection with NO rows for the session returns
// MEASURED zeros — Partial=false — never a fabricated or degraded value:
// the projection observed none of the session's supported measures, which
// is a real measurement, not missing data.
func TestProjectionEnricher_Current_NoRows_MeasuredExactZeros(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	target := sid("t1", "u1", "s1")
	other := sid("t1", "u1", "s-other")

	store := memstore.New()
	applyRollupBatch(t, store, 1, costDelta(other, opened, 5.00, 500)) // rows exist, but not for target

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauseresume.New(),
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.Partial {
		t.Error("Partial = true, want false — zero projection rows for a covered session is a measured zero")
	}
	if c.TotalCostCents != 0 || c.TotalTokens != 0 || c.EventsCount != 0 || c.TasksCount != 0 {
		t.Errorf("measured zeros expected, got cost=%d tokens=%d events=%d tasks=%d",
			c.TotalCostCents, c.TotalTokens, c.EventsCount, c.TasksCount)
	}
	if c.HasPendingIntervention || c.HasFailedTask {
		t.Error("intervention / failed-task must be false on a session with no rows and no pauses")
	}
	if got := fallback.called(); got != 0 {
		t.Errorf("fallback delegations = %d, want 0 — measured zeros never trigger the raw scan", got)
	}
}

// TestProjectionEnricher_SubCentCosts_ExactDeterministicCents is the
// cost-conversion honesty guard: the rollup sums exact integer micro-units
// and converts to cents ONCE at the end (round-half-up), so a session of
// sub-cent per-call costs sums to its true cent total instead of flooring
// every call to zero — the same false-absence the raw enricher closes.
func TestProjectionEnricher_SubCentCosts_ExactDeterministicCents(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(60 * time.Minute)
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	deltas := make([]rollups.Delta, 0, 50)
	for i := range 50 {
		deltas = append(deltas, costDelta(target, opened.Add(time.Duration(i)*time.Minute), 0.004, 3))
	}
	applyRollupBatch(t, store, 1, deltas...)

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauseresume.New(),
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	// 50 × $0.004 = $0.20 = 20 whole cents — never 0.
	if c.TotalCostCents != 20 {
		t.Errorf("TotalCostCents = %d, want 20 (50 sub-cent calls must sum to their true cent total, not floor to 0)", c.TotalCostCents)
	}
	if c.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150 (50 × 3)", c.TotalTokens)
	}
	if c.EventsCount != 50 {
		t.Errorf("EventsCount = %d, want 50", c.EventsCount)
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
	applyRollupBatch(t, store, 1, costDelta(target, opened, 1.00, 100))

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
			applyRollupBatch(t, s, 1, costDelta(target, opened, 1.00, 100))
			if tc.close {
				if err := s.Close(context.Background()); err != nil {
					t.Fatalf("close store: %v", err)
				}
			}
			fallback := &countingFallbackEnricher{result: raw}
			enr := newProjectionEnricher(t, s, tc.quality, fallback, pauseresume.New(), tc.window, clockAt(now))

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

// TestProjectionEnricher_UnreachableRow_PauseReadMarksPartial pins that
// the projection path still performs the pause read and that a read it
// cannot take marks the rollup Partial: the rollup query itself is
// filter-scoped (the store read needs no ctx elevation, so the
// projection-backed counts ARE read), but a row whose tenant the adapter
// may not reach leaves has_pending_intervention unmeasured — Partial says
// so, instead of a fabricated false.
func TestProjectionEnricher_UnreachableRow_PauseReadMarksPartial(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	foreign := sid("t-other", "u9", "s9")

	store := memstore.New()
	applyRollupBatch(t, store, 1, costDelta(foreign, opened, 5.00, 500))

	anchored, err := identity.WithVerified(context.Background(), sid("t1", "u1", "s1"))
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}

	enr := newProjectionEnricher(t, store, currentQuality(opened), &countingFallbackEnricher{},
		pauseresume.New(), sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(anchored, foreign, "s9")
	if !c.Partial {
		t.Error("Partial = false, want true — a pause read the adapter could not take must mark the rollup, never a fabricated false intervention")
	}
	// The projection counts are still read (the rollup query is scoped by
	// its filter, not by ctx elevation) — Partial marks them lower-bound.
	if c.TotalCostCents != 500 {
		t.Errorf("TotalCostCents = %d, want 500 — the filter-scoped projection read still serves the counts", c.TotalCostCents)
	}
	if c.HasPendingIntervention {
		t.Error("HasPendingIntervention = true for an unreadable row — the unmeasured value must read false WITH Partial")
	}
}

// TestProjectionEnricher_ForeignRowUnderFleetClaim_ReadInFull is the
// companion: under the admin-tier claim a fleet listing carries, the
// adapter's audited re-scope makes the pause read succeed, so the foreign
// row is read in full — Partial stays false. Without this pin the test
// above would pass for an adapter that never reads anything.
func TestProjectionEnricher_ForeignRowUnderFleetClaim_ReadInFull(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(30 * time.Minute)
	foreign := sid("t-other", "u9", "s9")

	store := memstore.New()
	applyRollupBatch(t, store, 1, costDelta(foreign, opened, 5.00, 500))

	enr := newProjectionEnricher(t, store, currentQuality(opened), &countingFallbackEnricher{},
		pauseresume.New(), sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(fleetCtx(t, sid("t1", "u1", "s1")), foreign, "s9")
	if c.Partial {
		t.Error("Partial = true for a complete foreign row under the fleet claim, want false")
	}
	if c.TotalCostCents != 500 {
		t.Errorf("TotalCostCents = %d, want 500", c.TotalCostCents)
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
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauseresume.New(),
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
// coarsening never changes the summed measures.
func TestProjectionEnricher_Current_LongLivedSession_CoarsenedExact(t *testing.T) {
	opened := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(200 * 24 * time.Hour) // 200 days: 288k minutes > MaxBuckets, 4800h > MaxBuckets → day grid
	target := sid("t1", "u1", "s1")

	store := memstore.New()
	applyRollupBatch(t, store, 1,
		costDelta(target, opened, 1.00, 100),                                // 100c / 100 tok / 1 event
		costDelta(target, opened.Add(199*24*time.Hour), 2.50, 300),          // 250c / 300 tok — near the window end
		taskOutcomeDelta(target, opened.Add(100*24*time.Hour), "completed"), // a task outcome mid-window
		taskOutcomeDelta(target, opened.Add(100*24*time.Hour), "failed"),    // 2 tasks total
	)

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauseresume.New(),
		sessionWindow(opened, now), clockAt(now))

	c := enr.Counters(context.Background(), target, "s1")
	if c.TotalCostCents != 350 {
		t.Errorf("TotalCostCents = %d, want 350 — coarsening must not change the summed cost", c.TotalCostCents)
	}
	if c.TotalTokens != 400 {
		t.Errorf("TotalTokens = %d, want 400", c.TotalTokens)
	}
	if c.EventsCount != 2 {
		t.Errorf("EventsCount = %d, want 2", c.EventsCount)
	}
	if c.TasksCount != 2 || !c.HasFailedTask {
		t.Errorf("TasksCount = %d, HasFailedTask = %v, want 2/true", c.TasksCount, c.HasFailedTask)
	}
	if c.Partial {
		t.Error("Partial = true, want false — a covered long-lived session returns exact counters")
	}
	if got := fallback.called(); got != 0 {
		t.Errorf("fallback delegations = %d, want 0", got)
	}
}

// TestProjectionEnricher_ConcurrentReuse_NoCrossTalk pins the D-025-style
// concurrent-reuse contract for the adapter: N≥100 concurrent Counters
// calls against ONE shared ProjectionEnricher under -race, with distinct
// sessions, must show no data races, no context bleed (each session reads
// exactly its own projection-backed counters), no fallback invocations,
// and no goroutine leak after the wave.
func TestProjectionEnricher_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	opened := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := opened.Add(2 * time.Hour)
	a := sid("t1", "u1", "sA")
	b := sid("t2", "u2", "sB")

	store := memstore.New()
	applyRollupBatch(t, store, 1,
		costDelta(a, opened, 1.00, 100), // sA: 100c / 100 tok / 1 event
		costDelta(b, opened, 5.00, 500), // sB: 500c / 500 tok / 1 event
		taskOutcomeDelta(a, opened.Add(time.Minute), "completed"),
		taskOutcomeDelta(a, opened.Add(2*time.Minute), "failed"),
	)

	pauses := pauseresume.New()
	if _, err := pauses.Request(context.Background(), pauseresume.PauseRequest{
		Identity: a,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("pause request: %v", err)
	}

	fallback := &countingFallbackEnricher{}
	enr := newProjectionEnricher(t, store, currentQuality(opened), fallback, pauses,
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
				if c.TotalCostCents != 100 || c.TotalTokens != 100 || c.EventsCount != 1 ||
					c.TasksCount != 2 || !c.HasFailedTask || !c.HasPendingIntervention || c.Partial {
					errCh <- "context bleed: session sA got " + fmt.Sprintf("%+v", c)
				}
			case b.SessionID:
				if c.TotalCostCents != 500 || c.TotalTokens != 500 || c.EventsCount != 1 ||
					c.TasksCount != 0 || c.HasFailedTask || c.HasPendingIntervention || c.Partial {
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
	if got := fallback.called(); got != 0 {
		t.Errorf("fallback delegations = %d, want 0 across %d concurrent projection-backed reads", got, N)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d above baseline after %d concurrent Counters", leaked, N)
	}
}
