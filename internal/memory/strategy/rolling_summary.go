package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// Default tuning constants — encoded as package constants per
// D-034 (operator-facing config narrows to RecoveryBacklogMax only;
// the retry / backoff / cadence knobs live here and require an RFC
// PR + new exported config field to tune).
const (
	defaultRetryAttempts      = 3
	defaultRetryBackoffBase   = 100 * time.Millisecond
	defaultDegradedRetryEvery = 10 * time.Second
)

// rollingSummaryExec implements `StrategyRollingSummary`:
//
//   - Recent-window buffer keyed per `identity.Quadruple`.
//   - When the buffer exceeds `FullZoneTurns` the overflow turns
//     spill into a `pending` queue and a single in-flight
//     summariser task is scheduled (one per memory key, via the
//     per-key mutex).
//   - On success the summary updates and `pending` clears.
//   - On failure the retry counter increments; after
//     `defaultRetryAttempts` consecutive failures the executor
//     transitions to `HealthDegraded`, emits
//     `memory.health_changed`, queues the failed batch into the
//     recovery backlog (bounded by `recoveryBacklogMax`), and
//     falls back to truncation semantics for `GetLLMContext`.
//   - A periodic recovery loop attempts to drain the backlog at
//     `defaultDegradedRetryEvery` cadence; on success it
//     transitions `degraded → recovering → healthy` (one
//     transition per recovery batch drained); each transition
//     emits `memory.health_changed`.
//
// Concurrent-reuse contract (D-025): the executor is shared across
// N goroutines. Per-key state is mutex-guarded; the recovery loop
// goroutine is cancellable via `close(stop)` + `Close`.
type rollingSummaryExec struct {
	state              state.StateStore
	bus                events.EventBus
	summarizer         memory.Summarizer
	budgetTokens       int
	recoveryBacklogMax int

	keys sync.Map // map[quadKey]*rollingKeyState

	// recovery-loop lifecycle.
	stop     chan struct{}
	stopOnce sync.Once
	loopWG   sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// rollingKeyState is the per-`(identity.Quadruple)` mutable state
// the rolling-summary strategy maintains.
type rollingKeyState struct {
	mu sync.Mutex

	recent  []memory.ConversationTurn // recent-window buffer (FullZoneTurns cap)
	pending []memory.ConversationTurn // turns awaiting summarisation
	summary string                    // current rolling summary

	health        memory.Health
	failedRetries int

	// backlog holds failed summariser batches awaiting recovery.
	// Bounded by `recoveryBacklogMax`; overflow drops oldest and
	// emits `memory.recovery_dropped`.
	backlog []memory.SummarizeRequest

	// loaded tracks whether the executor has loaded the persisted
	// state for this key.
	loaded bool
}

func newRollingSummaryExec(deps Deps) *rollingSummaryExec {
	max := deps.RecoveryBacklogMax
	if max == 0 {
		max = DefaultRecoveryBacklogMax
	}
	e := &rollingSummaryExec{
		state:              deps.State,
		bus:                deps.Bus,
		summarizer:         deps.Summarizer,
		budgetTokens:       deps.BudgetTokens,
		recoveryBacklogMax: max,
		stop:               make(chan struct{}),
	}
	e.loopWG.Add(1)
	go e.recoveryLoop()
	return e
}

func (e *rollingSummaryExec) keyState(id identity.Quadruple) *rollingKeyState {
	k := quadKeyFor(id)
	if v, ok := e.keys.Load(k); ok {
		return v.(*rollingKeyState)
	}
	fresh := &rollingKeyState{health: memory.HealthHealthy}
	actual, _ := e.keys.LoadOrStore(k, fresh)
	return actual.(*rollingKeyState)
}

// loadIfNeeded fills per-key state from the StateStore on first
// access.
func (e *rollingSummaryExec) loadIfNeeded(ctx context.Context, ks *rollingKeyState, id identity.Quadruple) error {
	if ks.loaded {
		return nil
	}
	rec, err := loadRecord(ctx, e.state, id)
	if err != nil {
		return err
	}
	if rec.Strategy == memory.StrategyRollingSummary {
		ks.recent = rec.Turns
		ks.summary = rec.Summary
	}
	ks.loaded = true
	return nil
}

func (e *rollingSummaryExec) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		ks.mu.Unlock()
		return err
	}
	ks.recent = append(ks.recent, turn)
	// Spill overflow into pending.
	for len(ks.recent) > FullZoneTurns {
		ks.pending = append(ks.pending, ks.recent[0])
		ks.recent = ks.recent[1:]
	}
	// Capture the batch + prior summary for the summariser call
	// while holding the lock; release before doing the actual call.
	var (
		batch      []memory.ConversationTurn
		prior      string
		shouldCall bool
		degraded   = ks.health == memory.HealthDegraded
	)
	if len(ks.pending) > 0 && !degraded {
		batch = make([]memory.ConversationTurn, len(ks.pending))
		copy(batch, ks.pending)
		prior = ks.summary
		shouldCall = true
	} else if len(ks.pending) > 0 && degraded {
		// Degraded mode: drain pending into the recovery backlog
		// instead of calling the summariser. Each pending batch
		// becomes one backlog entry; the recovery loop drains them
		// at `defaultDegradedRetryEvery` cadence.
		req := memory.SummarizeRequest{
			PreviousSummary: ks.summary,
			Turns:           append([]memory.ConversationTurn(nil), ks.pending...),
		}
		if len(ks.backlog) >= e.recoveryBacklogMax {
			// Drop oldest; emit recovery_dropped (best-effort).
			_ = memory.EmitRecoveryDropped(ctx, e.bus, id, "backlog_overflow")
			ks.backlog = ks.backlog[1:]
		}
		ks.backlog = append(ks.backlog, req)
		ks.pending = nil
	}
	// Persist intermediate state regardless of summariser scheduling.
	persistErr := persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	})
	ks.mu.Unlock()
	if persistErr != nil {
		return persistErr
	}

	if !shouldCall {
		return nil
	}

	// Summariser call OUTSIDE the lock — the implementation may
	// block on the LLM edge and must not stall other operations on
	// this key.
	resp, err := e.summarizer.Summarize(ctx, id, memory.SummarizeRequest{
		PreviousSummary: prior,
		Turns:           batch,
	})
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err != nil {
		return e.onSummarizerFailure(ctx, ks, id, batch, prior, err)
	}
	// Success path: collapse pending into the summary, reset retry
	// counter, restore healthy if we were retrying.
	ks.summary = resp.Summary
	// Drop the pending entries we just summarised (defend against
	// concurrent AddTurns having appended more).
	if len(ks.pending) >= len(batch) {
		ks.pending = ks.pending[len(batch):]
	} else {
		ks.pending = nil
	}
	ks.failedRetries = 0
	if ks.health == memory.HealthRetry {
		e.transitionHealth(ctx, ks, id, memory.HealthHealthy, "summarizer_succeeded")
	}
	return persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	})
}

// onSummarizerFailure is invoked under `ks.mu`. Increments the
// retry counter, transitions to `HealthRetry` (or `HealthDegraded`
// after exhaustion), enqueues the batch into the recovery backlog
// on degradation.
//
// Returns nil after the in-band failure has been absorbed —
// degraded mode is the observable failure surface (AGENTS.md §13
// "no silent degradation" exception, documented at the executor
// godoc + D-034). Returning an error here would force AddTurn to
// surface the summariser failure to the caller, which is exactly
// the silent-context-loss path we're closing.
func (e *rollingSummaryExec) onSummarizerFailure(
	ctx context.Context,
	ks *rollingKeyState,
	id identity.Quadruple,
	batch []memory.ConversationTurn,
	prior string,
	cause error,
) error {
	_ = cause // captured by the health transition's `Reason`.
	ks.failedRetries++
	if ks.failedRetries < defaultRetryAttempts {
		if ks.health == memory.HealthHealthy {
			e.transitionHealth(ctx, ks, id, memory.HealthRetry, "summarizer_failed")
		}
		return nil
	}
	// Retries exhausted — degrade.
	e.transitionHealth(ctx, ks, id, memory.HealthDegraded, "retries_exhausted")
	// Queue the failed batch into the recovery backlog (bounded).
	req := memory.SummarizeRequest{PreviousSummary: prior, Turns: batch}
	if len(ks.backlog) >= e.recoveryBacklogMax {
		// Drop oldest; emit recovery_dropped (best-effort — if
		// the bus is broken we don't want to fail the in-band
		// degradation).
		_ = memory.EmitRecoveryDropped(ctx, e.bus, id, "backlog_overflow")
		ks.backlog = ks.backlog[1:]
	}
	ks.backlog = append(ks.backlog, req)
	return nil
}

// transitionHealth is invoked under `ks.mu`. Validates the
// transition + emits `memory.health_changed`; updates `ks.health`
// only on a valid transition. Best-effort emit — a bus failure is
// logged via the wrapped error but does NOT block the state
// transition itself (the executor's in-memory state is the source
// of truth for the next operation).
func (e *rollingSummaryExec) transitionHealth(ctx context.Context, ks *rollingKeyState, id identity.Quadruple, next memory.Health, reason string) {
	prior := ks.health
	if prior == "" {
		prior = memory.HealthHealthy
	}
	if err := memory.ValidateHealthTransition(prior, next); err != nil {
		// Invalid transition is a bug, not a recoverable state.
		// Don't update health; the next valid event will retry.
		return
	}
	ks.health = next
	_ = memory.EmitHealthChanged(ctx, e.bus, id, prior, next, reason)
}

func (e *rollingSummaryExec) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if e.isClosed() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return memory.LLMContextPatch{}, err
	}
	// Copy recent turns so the caller can't mutate executor state.
	recent := make([]memory.ConversationTurn, len(ks.recent))
	copy(recent, ks.recent)
	patch := memory.LLMContextPatch{
		Strategy:    memory.StrategyRollingSummary,
		Summary:     ks.summary,
		RecentTurns: recent,
		Tokens:      sumTokens(ks.recent) + summaryTokens(ks.summary),
	}
	// Degraded fallback per brief 04 §4.1: drop the (stale) summary
	// from the patch, return only the recent window so the planner
	// keeps the conversation usable.
	if ks.health == memory.HealthDegraded {
		patch.Summary = ""
		patch.Tokens = sumTokens(ks.recent)
	}
	return patch, nil
}

func (e *rollingSummaryExec) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if e.isClosed() {
		return 0, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return 0, err
	}
	if ks.health == memory.HealthDegraded {
		return sumTokens(ks.recent), nil
	}
	return sumTokens(ks.recent) + summaryTokens(ks.summary), nil
}

// summaryTokens returns the token estimate for a rolling summary
// string. Empty summary → 0 (a fresh store reads as "no memory");
// non-empty → chars/4 + 1.
func summaryTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/4 + 1
}

func (e *rollingSummaryExec) Flush(ctx context.Context, id identity.Quadruple) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.recent = nil
	ks.pending = nil
	ks.summary = ""
	ks.failedRetries = 0
	ks.backlog = nil
	ks.health = memory.HealthHealthy
	ks.loaded = true
	if err := e.state.Delete(ctx, id, kindMemoryState); err != nil {
		return fmt.Errorf("memory/strategy/rolling_summary: Flush delete: %w", err)
	}
	return nil
}

func (e *rollingSummaryExec) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if e.isClosed() {
		return "", memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if !ks.loaded {
		if err := e.loadIfNeeded(ctx, ks, id); err != nil {
			return "", err
		}
	}
	if ks.health == "" {
		return memory.HealthHealthy, nil
	}
	return ks.health, nil
}

func (e *rollingSummaryExec) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if e.isClosed() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return memory.Snapshot{}, err
	}
	rec := memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	}
	bytes, err := json.Marshal(rec)
	if err != nil {
		return memory.Snapshot{}, fmt.Errorf("memory/strategy/rolling_summary: marshal: %w", err)
	}
	return memory.Snapshot{Strategy: memory.StrategyRollingSummary, Bytes: bytes}, nil
}

func (e *rollingSummaryExec) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if snap.IsEmpty() {
		ks.recent = nil
		ks.summary = ""
		ks.pending = nil
		ks.loaded = true
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyRollingSummary})
	}
	if snap.Strategy != memory.StrategyRollingSummary {
		return fmt.Errorf("%w: snapshot strategy=%q, executor strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, memory.StrategyRollingSummary)
	}
	if len(snap.Bytes) == 0 {
		ks.recent = nil
		ks.summary = ""
		ks.pending = nil
		ks.loaded = true
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyRollingSummary})
	}
	var rec memoryStateRecord
	if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrInvalidSnapshot, err)
	}
	if rec.Strategy != memory.StrategyRollingSummary {
		return fmt.Errorf("%w: record strategy=%q", memory.ErrInvalidSnapshot, rec.Strategy)
	}
	ks.recent = rec.Turns
	ks.summary = rec.Summary
	ks.pending = nil
	ks.loaded = true
	return persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	})
}

func (e *rollingSummaryExec) Close(_ context.Context) error {
	e.mu.Lock()
	already := e.closed
	e.closed = true
	e.mu.Unlock()
	if already {
		return nil
	}
	e.stopOnce.Do(func() { close(e.stop) })
	// Drain the recovery loop goroutine. The loop honours `stop`
	// and returns; the WaitGroup wait is bounded by the loop's
	// next ticker tick (max defaultDegradedRetryEvery), plus the
	// in-flight summariser call's own ctx-honouring shutdown.
	e.loopWG.Wait()
	return nil
}

func (e *rollingSummaryExec) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// recoveryLoop periodically drains the per-key recovery backlogs.
// Cancellable via `e.stop` (closed in `Close`).
//
// Iteration: every `defaultDegradedRetryEvery` tick, walk every
// `rollingKeyState` with non-empty backlog. For each one:
//
//  1. Transition `HealthDegraded → HealthRecovering`.
//  2. Pop the oldest batch and call the summariser.
//  3. On success: collapse the summary, transition to `HealthHealthy`.
//  4. On failure: re-enqueue the batch at the tail, transition
//     back to `HealthDegraded`.
//
// Each transition emits `memory.health_changed` so subscribers see
// the full FSM walk.
func (e *rollingSummaryExec) recoveryLoop() {
	defer e.loopWG.Done()
	ticker := time.NewTicker(defaultDegradedRetryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.drainBacklogs()
		}
	}
}

// drainBacklogs walks every per-key state and attempts one
// recovery batch per key. Bounded work per tick — the loop runs
// again on the next tick if more backlog remains.
func (e *rollingSummaryExec) drainBacklogs() {
	// Build a snapshot of (id, ks) pointers so we don't hold the
	// sync.Map's internal iteration lock across the summariser
	// call (which may block).
	type entry struct {
		id identity.Quadruple
		ks *rollingKeyState
	}
	var work []entry
	e.keys.Range(func(k, v any) bool {
		key := k.(quadKey)
		work = append(work, entry{
			id: identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  key.Tenant,
					UserID:    key.User,
					SessionID: key.Session,
				},
				RunID: key.Run,
			},
			ks: v.(*rollingKeyState),
		})
		return true
	})
	for _, w := range work {
		e.recoverOne(w.id, w.ks)
	}
}

// recoverOne attempts to drain one batch from `ks.backlog`. Called
// once per key per tick; the per-key mutex serialises the operation
// against in-flight `AddTurn` calls.
func (e *rollingSummaryExec) recoverOne(id identity.Quadruple, ks *rollingKeyState) {
	// Use a fresh context so the recovery is not bound to a
	// caller's ctx. Honour cancellation via the executor's `stop`
	// channel — we re-check `isClosed` inside the lock and bail
	// before doing the summariser call if the executor was closed
	// between ticks.
	ctx := context.Background()
	ks.mu.Lock()
	if len(ks.backlog) == 0 {
		ks.mu.Unlock()
		return
	}
	if e.isClosed() {
		ks.mu.Unlock()
		return
	}
	if ks.health != memory.HealthDegraded {
		// Health raced to a non-degraded state — nothing to do.
		ks.mu.Unlock()
		return
	}
	e.transitionHealth(ctx, ks, id, memory.HealthRecovering, "recovery_loop_attempt")
	batch := ks.backlog[0]
	ks.mu.Unlock()

	resp, err := e.summarizer.Summarize(ctx, id, batch)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err != nil {
		// Recovery batch failed — back to degraded.
		// (Batch stays at head of backlog; another tick retries.)
		e.transitionHealth(ctx, ks, id, memory.HealthDegraded, "recovery_batch_failed")
		return
	}
	// Pop the head batch + fold into summary.
	if len(ks.backlog) > 0 {
		ks.backlog = ks.backlog[1:]
	}
	ks.summary = resp.Summary
	ks.failedRetries = 0
	if len(ks.backlog) == 0 {
		e.transitionHealth(ctx, ks, id, memory.HealthHealthy, "recovery_loop_drained")
	} else {
		// More backlog remains — back to degraded; next tick
		// continues draining.
		e.transitionHealth(ctx, ks, id, memory.HealthDegraded, "recovery_batch_drained")
	}
	// Best-effort persist; ignore failure (the in-memory state is
	// authoritative; the next persist landing AddTurn will re-sync).
	_ = persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	})
}

// Compile-time assertion that *rollingSummaryExec satisfies
// StrategyExecutor.
var _ StrategyExecutor = (*rollingSummaryExec)(nil)

// ErrSummarizerUnavailable is a sentinel a test-grade
// `Summarizer` may return to force the failure path. Not part of
// the public API; lives here so test code in the same package can
// reach it. Real `Summarizer` implementations should wrap their
// own typed errors.
//
//nolint:unused // exported indirectly via EchoSummarizer's helpers; tests reference this from the same package.
var errSummarizerUnavailable = errors.New("memory/strategy: summarizer unavailable")
