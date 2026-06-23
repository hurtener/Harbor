package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// Default tuning constants — encoded as package constants per
// a settled scope decision (operator-facing config narrows to RecoveryBacklogMax only;
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
// Concurrent-reuse contract: the executor is shared across
// N goroutines. Per-key state is mutex-guarded; the recovery loop
// goroutine is cancellable via `close(stop)` + `Close`.
type rollingSummaryExec struct {
	state              state.StateStore
	bus                events.EventBus
	summarizer         memory.Summarizer
	budgetTokens       int
	recentTurns        int
	recoveryBacklogMax int

	keys sync.Map // map[quadKey]*rollingKeyState

	// recovery-loop lifecycle.
	stop     chan struct{}
	stopOnce sync.Once
	loopWG   sync.WaitGroup
	// recoveryCtx is the context the recovery loop passes to the
	// summariser. Close cancels it so an in-flight Summarize that
	// honours cancellation unblocks promptly, bounding loopWG.Wait
	// rather than letting a hung summariser pin shutdown forever.
	recoveryCtx    context.Context
	recoveryCancel context.CancelFunc

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
	recent := deps.RecentTurns
	if recent <= 0 {
		recent = FullZoneTurns
	}
	e := &rollingSummaryExec{
		state:              deps.State,
		bus:                deps.Bus,
		summarizer:         deps.Summarizer,
		budgetTokens:       deps.BudgetTokens,
		recentTurns:        recent,
		recoveryBacklogMax: max,
		stop:               make(chan struct{}),
	}
	e.recoveryCtx, e.recoveryCancel = context.WithCancel(context.Background())
	e.loopWG.Add(1)
	go e.recoveryLoop()
	return e
}

func (e *rollingSummaryExec) keyState(id identity.Quadruple) *rollingKeyState {
	k := quadKeyFor(id)
	if v, ok := e.keys.Load(k); ok {
		return v.(*rollingKeyState) //nolint:errcheck // e.keys only ever stores *rollingKeyState — the assertion cannot fail by construction.
	}
	fresh := &rollingKeyState{health: memory.HealthHealthy}
	actual, _ := e.keys.LoadOrStore(k, fresh)
	return actual.(*rollingKeyState) //nolint:errcheck // e.keys only ever stores *rollingKeyState — the assertion cannot fail by construction.
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
	for len(ks.recent) > e.recentTurns {
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
			_ = memory.EmitRecoveryDropped(ctx, e.bus, id, "backlog_overflow") //nolint:errcheck // best-effort emit — a broken bus must not fail in-band degradation.
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
		// No spill batch to summarise (or degraded mode drained it to
		// the recovery backlog). Still enforce the token budget: even
		// without a spill, the recent window plus the summary can
		// exceed the configured cap.
		return e.enforceBudgetCompaction(ctx, id)
	}

	// Summariser call OUTSIDE the lock — the implementation may
	// block on the LLM edge and must not stall other operations on
	// this key.
	resp, err := e.summarizer.Summarize(ctx, id, memory.SummarizeRequest{
		PreviousSummary: prior,
		Turns:           batch,
	})
	ks.mu.Lock()
	if err != nil {
		ferr := e.onSummarizerFailure(ctx, ks, id, batch, prior, err)
		ks.mu.Unlock()
		return ferr
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
	persistErr2 := persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyRollingSummary,
		Turns:    ks.recent,
		Summary:  ks.summary,
	})
	ks.mu.Unlock()
	if persistErr2 != nil {
		return persistErr2
	}
	// After folding the spilled batch, enforce the token budget: the
	// recent window plus the (now larger) summary may still exceed the
	// cap, in which case oldest recent turns are folded in too.
	return e.enforceBudgetCompaction(ctx, id)
}

// maxCompactionChunksPerAddTurn bounds the number of LLM-backed
// compaction calls a single AddTurn issues when the assembled context
// (recent window + summary) exceeds the configured token budget.
// Compaction folds the oldest recent turn into the summary one turn at
// a time; this cap keeps the per-turn summariser cost bounded. If a key
// is far over budget the remaining oldest turns fold on subsequent
// AddTurns, and the read-path guarantee (GetLLMContext) always clamps
// the emitted patch to the budget regardless of how far compaction got.
const maxCompactionChunksPerAddTurn = 4

// enforceBudgetCompaction folds oldest recent turns into the rolling
// summary, oldest-first, until the assembled context (recent +
// summary) fits `e.budgetTokens` or the per-call chunk cap is reached.
// The prior summary is always threaded through `PreviousSummary` and
// never discarded. Bounded work per call: at most
// `maxCompactionChunksPerAddTurn` summariser calls.
//
// Lock discipline mirrors AddTurn's spill path: state is captured under
// the per-key lock, the summariser is called unlocked, and the result
// is re-applied under the lock. A zero/negative budget is a no-op
// (unbounded back-compat). When the key is degraded the loop bails —
// the summariser is unavailable, and the read-path drop-oldest
// guarantee keeps the emitted patch within budget.
//
// Same-key serialisation: AddTurn (and the compaction it triggers) is
// serialised per memory key by contract — a session processes its turns
// serially, so two concurrent AddTurns for the SAME key are not a
// supported scenario. The concurrent-reuse guarantee is per identity key
// (distinct sessions run fully concurrently, as the concurrent-reuse
// test exercises); it does not promise atomicity for a read-modify-write
// window on a single key. The summariser-is-unlocked gap below therefore
// captures `prior`/`oldest` under the lock and re-validates the
// recent-window length on re-acquire as defence in depth, not as a
// same-key concurrency guarantee.
//
// Persisted-summary bound: the final persist defensively clamps the
// stored summary to the budget so the StateStore row stays bounded
// regardless of how well (or poorly) the summariser compresses — the
// read-path clamp only bounds the EMITTED patch.
//
// Cost note: when a key is far over budget, a single AddTurn can issue
// up to `maxCompactionChunksPerAddTurn` summariser (LLM) calls here, on
// top of the one spill-path call AddTurn may already have made.
func (e *rollingSummaryExec) enforceBudgetCompaction(ctx context.Context, id identity.Quadruple) error {
	if e.budgetTokens <= 0 {
		return nil
	}
	ks := e.keyState(id)
	mutated := false
	for range maxCompactionChunksPerAddTurn {
		ks.mu.Lock()
		if ks.health == memory.HealthDegraded {
			ks.mu.Unlock()
			break
		}
		// Fit, or nothing left to fold (the newest turn stays verbatim).
		if len(ks.recent) <= 1 || sumTokens(ks.recent)+summaryTokens(ks.summary) <= e.budgetTokens {
			ks.mu.Unlock()
			break
		}
		oldest := ks.recent[0]
		prior := ks.summary
		ks.mu.Unlock()

		resp, err := e.summarizer.Summarize(ctx, id, memory.SummarizeRequest{
			PreviousSummary: prior,
			Turns:           []memory.ConversationTurn{oldest},
		})
		ks.mu.Lock()
		if err != nil {
			// Fold the failure into the health FSM exactly like the
			// spill path; the read-path guarantee still keeps the patch
			// in budget while degraded.
			_ = e.onSummarizerFailure(ctx, ks, id, []memory.ConversationTurn{oldest}, prior, err) //nolint:errcheck // onSummarizerFailure absorbs the failure into the health FSM (always-nil return, see its godoc).
			ks.mu.Unlock()
			break
		}
		// Drop the oldest recent turn we just folded. Re-check len under
		// the re-acquired lock to defend against a concurrent same-key
		// AddTurn having mutated the window.
		if len(ks.recent) > 1 {
			ks.recent = ks.recent[1:]
		}
		ks.summary = resp.Summary
		mutated = true
		ks.mu.Unlock()
	}
	if !mutated {
		return nil
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	// Defensively bound the PERSISTED summary to the budget. The read
	// path clamps only the EMITTED patch; without this floor a non- or
	// poorly-compressing summariser could grow the StateStore row
	// unbounded across turns. Real LLM summarisers compress and degraded
	// mode bails before reaching here, so this is robustness, not the
	// common path.
	if e.budgetTokens > 0 && summaryTokens(ks.summary) > e.budgetTokens {
		ks.summary = truncateSummaryToTokens(ks.summary, e.budgetTokens)
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
// godoc). Returning an error here would force AddTurn to
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
		_ = memory.EmitRecoveryDropped(ctx, e.bus, id, "backlog_overflow") //nolint:errcheck // best-effort emit — a broken bus must not fail in-band degradation.
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
	_ = memory.EmitHealthChanged(ctx, e.bus, id, prior, next, reason) //nolint:errcheck // best-effort emit — a bus failure must not block the in-memory health transition (see func doc).
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
	// Copy recent turns so the caller can't mutate executor state. The
	// budget clamp below operates entirely on these copies — the
	// read path NEVER mutates executor state.
	recent := make([]memory.ConversationTurn, len(ks.recent))
	copy(recent, ks.recent)
	summary := ks.summary
	// Degraded fallback: drop the (stale) summary so the planner keeps
	// the conversation usable on the recent window alone.
	if ks.health == memory.HealthDegraded {
		summary = ""
	}
	// Cheap, no-LLM final budget clamp. Even if the AddTurn-side
	// compaction is behind (chunk cap) or the summariser is degraded /
	// non-compressing, the emitted patch fits the budget: first drop
	// oldest recent turns (keep the newest), then — if a single turn
	// plus the summary still overflows — deterministically truncate the
	// summary string to the remaining token budget. The one exception:
	// when a SINGLE recent turn alone exceeds the budget, the freshest
	// turn is preserved verbatim (its text is never mangled), the summary
	// truncates to "", and the patch carries that one turn over budget.
	// The always-on token-window guard (ErrContextWindowExceeded) is the
	// backstop for that residual case.
	if e.budgetTokens > 0 {
		for len(recent) > 1 && summaryTokens(summary)+sumTokens(recent) > e.budgetTokens {
			recent = recent[1:]
		}
		if summaryTokens(summary)+sumTokens(recent) > e.budgetTokens {
			summary = truncateSummaryToTokens(summary, e.budgetTokens-sumTokens(recent))
		}
	}
	return memory.LLMContextPatch{
		Strategy:    memory.StrategyRollingSummary,
		Summary:     summary,
		RecentTurns: recent,
		Tokens:      sumTokens(recent) + summaryTokens(summary),
	}, nil
}

// compactionMarker is appended to a summary truncated by the read-path
// budget guarantee so the planner can see that older context was
// dropped rather than silently lost.
const compactionMarker = " …[older context compacted]"

// truncateSummaryToTokens deterministically shortens a rolling-summary
// string so its token estimate (chars/4+1) fits within `maxTokens`.
// Returns "" when `maxTokens <= 0` (no room for any summary). The
// truncation lands on a UTF-8 rune boundary and, when there is room,
// appends `compactionMarker`. The result always satisfies
// `summaryTokens(result) <= maxTokens`.
func truncateSummaryToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if summaryTokens(s) <= maxTokens {
		return s
	}
	// summaryTokens(x) = len(x)/4 + 1, so len(x) <= 4*(maxTokens-1)
	// keeps the estimate within maxTokens.
	maxBytes := 4 * (maxTokens - 1)
	if maxBytes <= len(compactionMarker) {
		// Not enough room for the marker — emit a bare rune-boundary
		// truncation that still fits the byte budget.
		return truncateOnRuneBoundary(s, maxBytes)
	}
	body := truncateOnRuneBoundary(s, maxBytes-len(compactionMarker))
	return body + compactionMarker
}

// truncateOnRuneBoundary returns the longest prefix of `s` whose byte
// length is `<= maxBytes` and which ends on a UTF-8 rune boundary.
func truncateOnRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b]
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

// SearchTurns fails loudly: similarity search requires the semantic
// retrieval wrapper, which composes around this executor when the
// mode is enabled.
func (e *rollingSummaryExec) SearchTurns(_ context.Context, _ identity.Quadruple, _ string, _ int) ([]memory.ScoredTurn, error) {
	if e.isClosed() {
		return nil, memory.ErrStoreClosed
	}
	return nil, memory.ErrSemanticDisabled
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
		return fmt.Errorf("%w: %w", memory.ErrInvalidSnapshot, err)
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
	// Cancel the recovery ctx first so an in-flight summariser call
	// that honours cancellation returns promptly, THEN signal the loop
	// to stop, THEN join. The wait is bounded by: a summariser already
	// running returns on ctx cancel; one not yet running is gated by
	// the next `stop` check before the call. A summariser that ignores
	// ctx can still block its current batch — bounded only by that
	// summariser's own behaviour, not by this loop.
	e.recoveryCancel()
	e.stopOnce.Do(func() { close(e.stop) })
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
		key := k.(quadKey) //nolint:errcheck // e.keys only ever uses quadKey keys — the assertion cannot fail by construction.
		work = append(work, entry{
			id: identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  key.Tenant,
					UserID:    key.User,
					SessionID: key.Session,
				},
				RunID: key.Run,
			},
			ks: v.(*rollingKeyState), //nolint:errcheck // e.keys only ever stores *rollingKeyState — the assertion cannot fail by construction.
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
	// Use the executor's recovery context (not a caller's ctx, and not
	// context.Background()): Close cancels it, so an in-flight summariser
	// call that honours cancellation unblocks and `Close`'s loopWG.Wait
	// is bounded. We also re-check `isClosed` inside the lock and bail
	// before the summariser call if the executor closed between ticks.
	ctx := e.recoveryCtx
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
	_ = persistRecord(ctx, e.state, id, memoryStateRecord{ //nolint:errcheck // best-effort persist — the in-memory state is authoritative; the next AddTurn re-syncs (see comment above).
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
