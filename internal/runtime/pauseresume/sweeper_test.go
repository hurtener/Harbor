package pauseresume

// Sweeper unit tests (Phase 111c / D-200). Internal package: the
// deterministic single-pass tests drive sweepOnce / expiredEntries
// directly under the controllable clock (CLAUDE.md §11 — never
// time.Sleep for synchronisation); the lifecycle test drives the
// exported RunSweeper end-to-end and asserts the goroutine baseline is
// restored after shutdown.

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem" // sweeper tests exercise the real inmem StateStore on the checkpoint seam
)

var sweepID = identity.Identity{TenantID: "t-sweep", UserID: "u-sweep", SessionID: "s-sweep"}

// sweepClock is the package-internal controllable clock (the external
// test package keeps its own copy; Go test packages do not share
// identifiers).
type sweepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSweepClock() *sweepClock {
	return &sweepClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *sweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sweepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func sweepRunCtx(t *testing.T, id identity.Identity, runID string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func sweepStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func sweepBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func requestPaused(t *testing.T, c Coordinator, ctx context.Context, runID string) Token {
	t.Helper()
	p, err := c.Request(ctx, PauseRequest{
		Identity: sweepID,
		Reason:   ReasonApprovalRequired,
		Payload:  map[string]any{"gate": "approval"},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	_ = runID
	return p.Token
}

// TestWithMaxParkDuration_StampsExpiry asserts the expiry is derived
// from PausedAt + the construction-time knob at Request time.
func TestWithMaxParkDuration_StampsExpiry(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	c := New(WithClock(clk.Now), WithMaxParkDuration(time.Hour)).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-exp")
	tok := requestPaused(t, c, ctx, "run-exp")

	c.mu.Lock()
	entry := c.pauses[tok]
	c.mu.Unlock()
	want := entry.pausedAt.Add(time.Hour)
	if !entry.expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want pausedAt + 1h = %v", entry.expiresAt, want)
	}
}

// TestWithMaxParkDuration_ZeroNeverExpires pins the default: no knob ⇒
// zero expiresAt ⇒ expiredEntries never selects the pause, however far
// the clock advances.
func TestWithMaxParkDuration_ZeroNeverExpires(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	c := New(WithClock(clk.Now)).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-never")
	tok := requestPaused(t, c, ctx, "run-never")

	clk.advance(1000 * time.Hour)
	if got := c.expiredEntries(clk.Now()); len(got) != 0 {
		t.Fatalf("expiredEntries on a no-max-park coordinator = %d entries, want 0", len(got))
	}
	st, err := c.Status(ctx, tok)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StatusPaused {
		t.Fatalf("Status.State = %q, want paused (never-expire default violated)", st.State)
	}
}

// TestSweepOnce_ReapsOnlyExpired asserts the sweeper selects only
// pauses past their deadline, resumes them with DecisionTimeout, and
// leaves the rest parked — with the checkpoint deleted for the reaped
// pause (no orphan) and intact for the parked one.
func TestSweepOnce_ReapsOnlyExpired(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	store := sweepStore(t)
	c := New(
		WithClock(clk.Now),
		WithMaxParkDuration(time.Hour),
		WithCheckpointStore(store),
	).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-reap")

	early := requestPaused(t, c, ctx, "run-reap") // pausedAt = T0
	clk.advance(30 * time.Minute)
	late := requestPaused(t, c, ctx, "run-reap") // pausedAt = T0+30m

	// T0+61m: `early` is past its deadline, `late` is not.
	clk.advance(31 * time.Minute)
	reaped, err := sweepOnce(ctx, c, slog.Default())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("sweepOnce reaped = %d, want 1", reaped)
	}

	st, err := c.Status(ctx, early)
	if err != nil {
		t.Fatalf("Status(early): %v", err)
	}
	if st.State != StatusResumed || st.Decision != DecisionTimeout {
		t.Fatalf("early pause: state=%q decision=%q, want resumed/timeout", st.State, st.Decision)
	}

	st, err = c.Status(ctx, late)
	if err != nil {
		t.Fatalf("Status(late): %v", err)
	}
	if st.State != StatusPaused {
		t.Fatalf("late pause state = %q, want paused (sweeper over-reaped)", st.State)
	}

	// The reaped pause's checkpoint is gone — a fresh Coordinator over
	// the same store cannot find it (no orphan); the parked pause's
	// checkpoint survives.
	fresh := New(WithCheckpointStore(store))
	if _, err := fresh.Status(ctx, early); !errors.Is(err, ErrPauseNotFound) {
		t.Fatalf("reaped checkpoint: fresh Status err=%v, want ErrPauseNotFound", err)
	}
	if _, err := fresh.Status(ctx, late); err != nil {
		t.Fatalf("parked checkpoint must survive the sweep: %v", err)
	}
}

// TestSweepOnce_PayloadCarriesTimeoutFacts asserts the reap's Resume
// payload merges the audit-safe timeout facts onto the pause record.
func TestSweepOnce_PayloadCarriesTimeoutFacts(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	c := New(WithClock(clk.Now), WithMaxParkDuration(time.Minute)).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-facts")
	tok := requestPaused(t, c, ctx, "run-facts")

	clk.advance(2 * time.Minute)
	if _, err := sweepOnce(ctx, c, slog.Default()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	c.mu.Lock()
	entry := c.pauses[tok]
	payload := entry.payload
	c.mu.Unlock()
	if payload["timed_out"] != true {
		t.Fatalf("payload[timed_out] = %v, want true", payload["timed_out"])
	}
	if payload["max_park_duration"] != time.Minute.String() {
		t.Fatalf("payload[max_park_duration] = %v, want %q", payload["max_park_duration"], time.Minute.String())
	}
	for _, k := range []string{"paused_at", "expired_at"} {
		if s, ok := payload[k].(string); !ok || s == "" {
			t.Fatalf("payload[%s] = %v, want a non-empty RFC3339 string", k, payload[k])
		}
	}
	// The caller-supplied payload key survives the merge.
	if payload["gate"] != "approval" {
		t.Fatalf("payload[gate] = %v, want %q (caller payload lost in merge)", payload["gate"], "approval")
	}
}

// TestSweepOnce_EmitsPauseResumedWithTimeoutDecision asserts the
// canonical `pause.resumed` event carries the typed `timeout` marker
// (D-096 — DecisionTimeout's first producer) under the pause's own
// identity quadruple.
func TestSweepOnce_EmitsPauseResumedWithTimeoutDecision(t *testing.T) {
	t.Parallel()
	red := patternsAudit.New()
	bus := sweepBus(t, red)
	clk := newSweepClock()
	c := New(WithClock(clk.Now), WithMaxParkDuration(time.Minute), WithBus(bus)).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-emit")

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: sweepID.TenantID, User: sweepID.UserID, Session: sweepID.SessionID,
		Types: []events.EventType{EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	tok := requestPaused(t, c, ctx, "run-emit")
	clk.advance(2 * time.Minute)
	if _, err := sweepOnce(ctx, c, slog.Default()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(PauseResumedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want PauseResumedPayload", ev.Payload)
		}
		if payload.Token != string(tok) {
			t.Fatalf("event token = %q, want %q", payload.Token, tok)
		}
		if payload.Decision != DecisionTimeout {
			t.Fatalf("event decision = %q, want %q", payload.Decision, DecisionTimeout)
		}
		// Identity propagation: the event rides under the pause's own
		// quadruple (CLAUDE.md §6).
		if ev.Identity.Identity != sweepID || ev.Identity.RunID != "run-emit" {
			t.Fatalf("event identity = %+v, want %+v / run-emit", ev.Identity, sweepID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pause.resumed event within 2s of the sweep")
	}
}

// TestSweepOnce_RestampsExpiryOnRehydratedPause asserts the derived
// (never persisted) deadline is re-stamped by the rehydrating
// Coordinator's own knob, so a restarted Runtime applies its own
// ceiling to checkpoints written before the restart.
func TestSweepOnce_RestampsExpiryOnRehydratedPause(t *testing.T) {
	t.Parallel()
	store := sweepStore(t)
	clk := newSweepClock()
	ctx := sweepRunCtx(t, sweepID, "run-rehydrate")

	// Coordinator #1 (no max-park) checkpoints the pause.
	c1 := New(WithClock(clk.Now), WithCheckpointStore(store))
	tok := requestPaused(t, c1, ctx, "run-rehydrate")

	// Coordinator #2 — the "restarted" Runtime, WITH a max-park knob.
	// Resume's rehydrate path installs the entry; the re-stamped expiry
	// is what the install carries.
	c2 := New(WithClock(clk.Now), WithCheckpointStore(store), WithMaxParkDuration(time.Hour)).(*coordinator)
	st, err := c2.Status(ctx, tok)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.State != StatusPaused {
		t.Fatalf("Status.State = %q, want paused", st.State)
	}
	entry, err := c2.rehydrate(ctx, tok)
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	want := entry.pausedAt.Add(time.Hour)
	if !entry.expiresAt.Equal(want) {
		t.Fatalf("rehydrated expiresAt = %v, want pausedAt + 1h = %v", entry.expiresAt, want)
	}
}

// fakeForeignCoordinator is a non-package Coordinator implementation —
// RunSweeper must reject it loud rather than silently maintaining
// nothing.
type fakeForeignCoordinator struct{}

func (fakeForeignCoordinator) Request(context.Context, PauseRequest) (Pause, error) {
	return Pause{}, nil
}
func (fakeForeignCoordinator) Resume(context.Context, Token, Decision, map[string]any) error {
	return nil
}
func (fakeForeignCoordinator) Status(context.Context, Token) (Status, error) { return Status{}, nil }
func (fakeForeignCoordinator) List(context.Context, ListRequest) (ListResponse, error) {
	return ListResponse{}, nil
}

// TestRunSweeper_FailsLoudOnMisconfiguration pins the two fail-loud
// preconditions: a foreign Coordinator implementation and a
// Coordinator without a max-park duration.
func TestRunSweeper_FailsLoudOnMisconfiguration(t *testing.T) {
	t.Parallel()
	if err := RunSweeper(context.Background(), fakeForeignCoordinator{}); !errors.Is(err, ErrSweeperMisconfigured) {
		t.Fatalf("RunSweeper(foreign coordinator) err = %v, want ErrSweeperMisconfigured", err)
	}
	if err := RunSweeper(context.Background(), New()); !errors.Is(err, ErrSweeperMisconfigured) {
		t.Fatalf("RunSweeper(no max-park) err = %v, want ErrSweeperMisconfigured", err)
	}
}

// TestRunSweeper_LifecycleAndGoroutineBaseline drives the exported
// RunSweeper end-to-end (real ticker, tiny cadence): an expired pause
// is reaped with DecisionTimeout, and after ctx cancellation the
// goroutine count settles back to baseline (CLAUDE.md §11 —
// long-lived components restore the goroutine baseline on shutdown).
func TestRunSweeper_LifecycleAndGoroutineBaseline(t *testing.T) {
	baseline := runtime.NumGoroutine()

	c := New(WithMaxParkDuration(10 * time.Millisecond))
	ctx := sweepRunCtx(t, sweepID, "run-lifecycle")
	tok := requestPaused(t, c, ctx, "run-lifecycle")

	sweepCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSweeper(sweepCtx, c, WithSweepInterval(5*time.Millisecond))
	}()

	// Eventually-style bounded wait: the pause is reaped within the
	// deadline or the test fails. No bare sleep-for-sync — the loop
	// polls the public Status surface with a hard bound.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := c.Status(ctx, tok)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State == StatusResumed {
			if st.Decision != DecisionTimeout {
				t.Fatalf("Status.Decision = %q, want %q", st.Decision, DecisionTimeout)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause was not reaped within 5s of RunSweeper start")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunSweeper exit err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweeper did not exit within 2s of ctx cancellation")
	}

	// Goroutine baseline restored (bounded settle for runtime noise).
	deadline = time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines after sweeper shutdown = %d, baseline = %d (leak)", got, baseline)
	}
}

// TestSweeper_RaceWithLegitimateResume pins the exactly-once contract
// under contention: N expired pauses, a concurrent legitimate Resume
// per pause racing concurrent sweep passes. Every pause resolves
// exactly once — the loser observes the documented
// ErrAlreadyResumed / ErrPauseNotFound, and the final Decision is
// consistent with which side won. Run under -race (the gate).
func TestSweeper_RaceWithLegitimateResume(t *testing.T) {
	t.Parallel()
	const n = 100
	clk := newSweepClock()
	store := sweepStore(t)
	c := New(
		WithClock(clk.Now),
		WithMaxParkDuration(time.Minute),
		WithCheckpointStore(store),
	).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-race")

	tokens := make([]Token, n)
	for i := range n {
		tokens[i] = requestPaused(t, c, ctx, "run-race")
	}
	clk.advance(2 * time.Minute) // everything is now expired

	legitErrs := make([]error, n)
	var wg sync.WaitGroup
	// Concurrent sweep passes racing the legitimate resumes.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sweepOnce(ctx, c, slog.New(slog.DiscardHandler)); err != nil {
				t.Errorf("sweepOnce: %v", err)
			}
		}()
	}
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			legitErrs[i] = c.Resume(ctx, tokens[i], DecisionApprove, nil)
		}()
	}
	wg.Wait()

	for i, tok := range tokens {
		st, err := c.Status(ctx, tok)
		if err != nil {
			t.Fatalf("Status(%d): %v", i, err)
		}
		if st.State != StatusResumed {
			t.Fatalf("pause %d not resolved (state %q)", i, st.State)
		}
		switch {
		case legitErrs[i] == nil:
			if st.Decision != DecisionApprove {
				t.Fatalf("pause %d: legit Resume won but Decision = %q", i, st.Decision)
			}
		case errors.Is(legitErrs[i], ErrAlreadyResumed) || errors.Is(legitErrs[i], ErrPauseNotFound):
			if st.Decision != DecisionTimeout {
				t.Fatalf("pause %d: sweeper won but Decision = %q", i, st.Decision)
			}
		default:
			t.Fatalf("pause %d: legit Resume err = %v, want nil / ErrAlreadyResumed / ErrPauseNotFound", i, legitErrs[i])
		}
		// Exactly-once on the durable side too: the checkpoint is gone.
		fresh := New(WithCheckpointStore(store))
		if _, err := fresh.Status(ctx, tok); !errors.Is(err, ErrPauseNotFound) {
			t.Fatalf("pause %d: checkpoint survived resolution (err=%v)", i, err)
		}
	}
}

// TestSweepOnce_ContinuesPastWedgedRecord asserts one failing reap (a
// lost tool-context handle) does not shield other expired pauses: the
// wedged record stays parked (loud-logged), the rest are reaped.
func TestSweepOnce_ContinuesPastWedgedRecord(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	c := New(WithClock(clk.Now), WithMaxParkDuration(time.Minute)).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-wedge")

	// A pause whose trajectory references a handle that is NOT in the
	// coordinator's registry — Resume (and therefore the reap) fails
	// loud with trajectory.ErrToolContextLost.
	wedged, err := c.Request(ctx, PauseRequest{
		Identity: sweepID,
		Reason:   ReasonExternalEvent,
		Trajectory: &trajectory.Trajectory{
			ToolContext: trajectory.ToolContext{
				Handles: []trajectory.HandleID{"handle-lost-in-sweep"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Request(wedged): %v", err)
	}
	healthy := requestPaused(t, c, ctx, "run-wedge")

	clk.advance(2 * time.Minute)
	reaped, err := sweepOnce(ctx, c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("sweepOnce reaped = %d, want 1 (the healthy pause)", reaped)
	}
	st, err := c.Status(ctx, healthy)
	if err != nil {
		t.Fatalf("Status(healthy): %v", err)
	}
	if st.State != StatusResumed || st.Decision != DecisionTimeout {
		t.Fatalf("healthy pause: state=%q decision=%q, want resumed/timeout", st.State, st.Decision)
	}
	st, err = c.Status(ctx, wedged.Token)
	if err != nil {
		t.Fatalf("Status(wedged): %v", err)
	}
	if st.State != StatusPaused {
		t.Fatalf("wedged pause state = %q, want paused (failed reap must not flip state)", st.State)
	}
}

// TestRunSweeper_DefaultIntervalMirrorsValidator pins the §4.4 mirror
// between DefaultSweepInterval and the config validator's
// `defaultPauseSweepInterval` (`internal/config/validate.go`). The
// validator cannot import this package, so it duplicates the 1m
// constant; this test asserts the duplicated value behaves
// identically: a `max_park_duration` equal to DefaultSweepInterval
// with a zero (defaulted) sweep_interval validates, while one second
// less is rejected on the one-sweep-overstay invariant.
func TestRunSweeper_DefaultIntervalMirrorsValidator(t *testing.T) {
	t.Parallel()
	build := func(maxPark time.Duration) error {
		cfg := config.Defaults()
		// Required-for-core LLM fields (documented dummy values — the
		// validator only checks non-emptiness here).
		cfg.LLM.Provider = "openrouter"
		cfg.LLM.Model = "test/model"
		cfg.LLM.APIKey = "env.HARBOR_TEST_DUMMY_KEY"
		cfg.PauseResume.MaxParkDuration = maxPark
		cfg.PauseResume.SweepInterval = 0
		return cfg.ValidateCore()
	}
	if err := build(DefaultSweepInterval); err != nil {
		t.Errorf("max_park_duration == DefaultSweepInterval (%s) with defaulted sweep_interval rejected: %v — the validator's mirrored default drifted",
			DefaultSweepInterval, err)
	}
	if err := build(DefaultSweepInterval - time.Second); err == nil {
		t.Errorf("max_park_duration just under DefaultSweepInterval (%s) with defaulted sweep_interval accepted — the validator's mirrored default drifted",
			DefaultSweepInterval-time.Second)
	}
}

// flakyDeleteStore wraps a real StateStore and fails Delete a
// configured number of times before passing through — the smallest
// fault injector that reproduces the resumed-but-undeleted checkpoint
// orphan (Wave C checkpoint audit). Unit-test-only; the seam-level
// behaviour runs on the real inmem driver underneath.
type flakyDeleteStore struct {
	state.StateStore
	mu        sync.Mutex
	failsLeft int
}

func (f *flakyDeleteStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	f.mu.Lock()
	if f.failsLeft > 0 {
		f.failsLeft--
		f.mu.Unlock()
		return errors.New("flaky store: injected delete failure")
	}
	f.mu.Unlock()
	return f.StateStore.Delete(ctx, id, kind)
}

// TestSweeper_RetriesOrphanedCheckpointDelete pins the orphan-retry
// guarantee: when a sweeper reap's Resume succeeds (state flipped,
// timeout recorded) but the checkpoint delete fails, the checkpoint
// is NOT orphaned — the next sweep pass's retryPendingDeletes clears
// it and emits the pause.resumed event the failed pass skipped.
func TestSweeper_RetriesOrphanedCheckpointDelete(t *testing.T) {
	t.Parallel()
	clk := newSweepClock()
	// failsLeft: 2 — the reap's own delete AND the same pass's retry
	// phase both fail, so the orphan survives a full sweep pass and
	// pass 2 proves the cross-tick retry.
	store := &flakyDeleteStore{StateStore: sweepStore(t), failsLeft: 2}
	red := patternsAudit.New()
	bus := sweepBus(t, red)
	c := New(
		WithClock(clk.Now),
		WithCheckpointStore(store),
		WithBus(bus),
		WithMaxParkDuration(time.Minute),
	).(*coordinator)
	ctx := sweepRunCtx(t, sweepID, "run-orphan")

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  sweepID.TenantID,
		User:    sweepID.UserID,
		Session: sweepID.SessionID,
		Types:   []events.EventType{EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	tok := requestPaused(t, c, ctx, "run-orphan")
	clk.advance(2 * time.Minute)

	// Pass 1: the reap's Resume flips the entry but the injected
	// delete failure orphans the checkpoint. Loud, not counted as
	// reaped, and the entry is flagged delete-pending.
	reaped, err := sweepOnce(ctx, c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("sweepOnce(pass 1): %v", err)
	}
	if reaped != 0 {
		t.Fatalf("pass 1 reaped = %d, want 0 (delete failed)", reaped)
	}
	st, err := c.Status(ctx, tok)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StatusResumed || st.Decision != DecisionTimeout {
		t.Fatalf("state=%q decision=%q, want resumed/timeout (the flip happened)", st.State, st.Decision)
	}
	if _, err := loadCheckpoint(ctx, store, tok); err != nil {
		t.Fatalf("checkpoint should still exist after the failed delete: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("pause.resumed emitted on the failed pass: %+v", ev)
	default:
	}

	// Pass 2: the store recovered; the retry phase clears the
	// checkpoint and completes the skipped emit.
	if _, err := sweepOnce(ctx, c, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("sweepOnce(pass 2): %v", err)
	}
	if _, err := loadCheckpoint(ctx, store, tok); !errors.Is(err, ErrPauseNotFound) {
		t.Fatalf("checkpoint after retry: err = %v, want ErrPauseNotFound (orphan cleared)", err)
	}
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(PauseResumedPayload)
		if !ok {
			t.Fatalf("pause.resumed payload type = %T", ev.Payload)
		}
		if payload.Token != string(tok) || payload.Decision != DecisionTimeout {
			t.Fatalf("pause.resumed payload = %+v, want token %q decision timeout", payload, tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry pass emitted no pause.resumed within bound")
	}

	// Idempotent: a third pass finds nothing pending.
	if pending := c.pendingDeleteEntries(); len(pending) != 0 {
		t.Fatalf("pendingDeleteEntries after successful retry = %d, want 0", len(pending))
	}
}
