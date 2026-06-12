package pauseresume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// This file ships the pause sweeper — the
// exported maintenance loop that gives the pause lifecycle an END.
// Previously, Resume was the ONLY checkpoint-deletion path: a
// cancel-while-paused run (or an operator who simply never answered an
// approval) orphaned its pause record + checkpoint forever. The
// sweeper is the backstop: it walks the Coordinator's registry and,
// for every pause past its max-park deadline (WithMaxParkDuration),
// calls Coordinator.Resume with DecisionTimeout — that enum value's
// FIRST producer (RFC §3.3; the not-yet-emitted note that
// previously sat in decision.go is closed by this file).
//
// # Why the scan is registry-internal, not Coordinator.List
//
// The phase plan's first sketch put the sweeper "over the existing
// List surface". List is deliberately identity-scoped (CLAUDE.md §6):
// an empty TenantIDs filter projects ONLY the caller's own tenant, and
// a cross-tenant filter must NAME its tenants under AdminScoped —
// there is no "all tenants" wildcard, by design. A maintenance actor
// cannot enumerate tenants it has never seen, so a List-shaped scan
// would require widening the §6 isolation surface with a wildcard.
// The plan's Risks section anticipated exactly this and the recorded
// resolution (plan §"Ship-time deviations") is: the sweeper
// lives in THIS package and snapshots the registry directly
// (value-copies under the mutex, same shape List itself uses), while
// every MUTATION goes through the public Coordinator.Resume under the
// pause's OWN identity triple — the scope check, checkpoint delete,
// and pause.resumed emit all run unmodified. No storage-level identity
// filter is bypassed and no elevated List shape is minted.
//
// One recorded V1 boundary on this design: checkpoints orphaned
// by a PROCESS CRASH were invisible (the registry scan only sees
// in-process pauses; rehydrate-on-demand needs someone to ask by
// Token). A follow-up closes it: the StateStore gained its one explicitly-
// elevated maintenance scan (state.StateStore.ListKind, RFC §6.11),
// and every sweep pass first rescues `pauseresume.checkpoint:` rows
// with no live registry entry back into the registry
// (rescanCrashOrphans) — after which the unchanged expired scan +
// public Resume path reaps them when their re-stamped deadline
// passes.
//
// # Timeout is terminal
//
// A timed-out pause is resumed with DecisionTimeout and is TERMINAL
// for the waiting run: the steering RunLoop observes the timeout and
// finishes the run with Finish{ConstraintsConflict} — a deadline the
// human missed is a constraint the planner cannot resolve (mirrors
// the REJECT posture). Never a silent unpark-and-continue.

// DefaultSweepInterval is the sweep cadence applied when no
// WithSweepInterval option is given. Mirrors the documented
// `pauseresume.sweep_interval` config default.
const DefaultSweepInterval = time.Minute

// sweeperConfig is the option-applied construction config for
// RunSweeper.
type sweeperConfig struct {
	interval time.Duration
	logger   *slog.Logger
}

// SweeperOption configures RunSweeper. Options are applied in order;
// later options override earlier ones for the same field.
type SweeperOption func(*sweeperConfig)

// WithSweepInterval overrides the sweep cadence. Non-positive values
// fall back to DefaultSweepInterval.
func WithSweepInterval(d time.Duration) SweeperOption {
	return func(c *sweeperConfig) {
		if d > 0 {
			c.interval = d
		}
	}
}

// WithSweeperLogger hands the sweeper a logger for reap / failure
// lines. Defaults to slog.Default().
func WithSweeperLogger(l *slog.Logger) SweeperOption {
	return func(c *sweeperConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// RunSweeper runs the pause sweeper until ctx is cancelled. It is the
// ONE exported entry into the pause-lifecycle maintenance loop (
// ): on every tick it scans the Coordinator's pause
// registry and resumes each pause past its max-park deadline with
// DecisionTimeout, under the pause's own identity scope.
//
// RunSweeper blocks; callers run it on a goroutine they cancel + join
// on shutdown (CLAUDE.md §5 concurrency rules — the assembly registers
// it on the closer chain). It returns ctx.Err() when cancelled — the
// caller treats context.Canceled as a clean shutdown.
//
// Fail-loud preconditions (ErrSweeperMisconfigured):
//
//   - coord must be this package's Coordinator (pauseresume.New) — the
//     maintenance scan reads the concrete registry (see the file doc
//     for why List cannot serve it).
//   - the Coordinator must carry a max-park duration
//     (WithMaxParkDuration > 0) — a sweeper over never-expiring pauses
//     would silently reap nothing forever.
//
// Per-record reap failures (a store error on the checkpoint delete)
// are logged at Error and do NOT stop the loop: one wedged record must
// not shield every other expired pause from being reaped. A PRE-flip
// failure leaves the entry paused, so the next pass's expired scan
// retries it (a lost tool-context handle is no longer in this class —
// timeout resumes skip the handle re-attach); a POST-flip
// failure (the checkpoint delete after Resume already flipped the
// entry) marks the entry delete-pending and the next pass's
// retryPendingDeletes phase re-attempts the delete + the skipped
// pause.resumed emit — a resumed-but-undeleted checkpoint must never
// orphan silently. Losing a reap race to a
// legitimate concurrent Resume (ErrAlreadyResumed / ErrPauseNotFound)
// is benign — the pause resolved exactly once — and is not logged as a
// failure.
//
// Each pass also rescues CRASH-ORPHANED checkpoints — store rows whose
// pause was never rehydrated because the process that parked it died —
// into the registry via the StateStore's maintenance scan
// (rescanCrashOrphans), so the max-park ceiling applies to them
// like any live pause.
func RunSweeper(ctx context.Context, coord Coordinator, opts ...SweeperOption) error {
	c, ok := coord.(*coordinator)
	if !ok {
		return fmt.Errorf("%w: RunSweeper requires the pauseresume.New Coordinator (got %T)", ErrSweeperMisconfigured, coord)
	}
	if c.maxPark <= 0 {
		return fmt.Errorf("%w: Coordinator has no max-park duration (construct it with WithMaxParkDuration; see pauseresume.max_park_duration in examples/harbor.yaml)", ErrSweeperMisconfigured)
	}

	cfg := sweeperConfig{
		interval: DefaultSweepInterval,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := sweepOnce(ctx, c, cfg.logger); err != nil {
				// sweepOnce returns an error ONLY for ctx cancellation
				// mid-sweep (per-record failures are logged inside and
				// the sweep continues). Exit the loop cleanly.
				return err
			}
		}
	}
}

// sweepOnce performs one sweep pass: rescue crash-orphaned checkpoints
// into the registry (rescanCrashOrphans), then snapshot the
// expired pauses at the Coordinator's clock and Resume each with
// DecisionTimeout under the pause's own identity. Returns the count of
// successfully reaped pauses. The only error return is ctx
// cancellation mid-pass; per-record failures are logged loud (Error
// level, with the identity-attributed fields CLAUDE.md §5 requires)
// and skipped so one wedged record cannot block the rest of the pass.
func sweepOnce(ctx context.Context, c *coordinator, logger *slog.Logger) (int, error) {
	if err := c.rescanCrashOrphans(ctx, logger); err != nil {
		return 0, err
	}
	now := c.now()
	expired := c.expiredEntries(now)
	reaped := 0
	for _, e := range expired {
		if err := ctx.Err(); err != nil {
			return reaped, fmt.Errorf("pauseresume: sweep cancelled mid-pass: %w", err)
		}

		resumeCtx, err := sweeperResumeContext(ctx, e)
		if err != nil {
			// An entry with an invalid identity cannot exist (Request
			// fails closed on an incomplete triple) — defence-in-depth.
			logger.ErrorContext(ctx, "pauseresume: sweeper could not scope resume context",
				"token", string(e.token), "error", err)
			continue
		}

		// The audit-safe timeout facts: runtime bookkeeping only, no
		// caller-controlled bytes (the pause's own Payload is already
		// on the record; this map is merged in by Resume).
		payload := map[string]any{
			"timed_out":         true,
			"max_park_duration": c.maxPark.String(),
			"paused_at":         e.pausedAt.UTC().Format(time.RFC3339Nano),
			"expired_at":        e.expiresAt.UTC().Format(time.RFC3339Nano),
		}

		switch err := c.Resume(resumeCtx, e.token, DecisionTimeout, payload); {
		case err == nil:
			reaped++
			logger.InfoContext(ctx, "pauseresume: sweeper reaped expired pause",
				"token", string(e.token),
				"reason", string(e.reason),
				"tenant_id", e.identity.TenantID,
				"user_id", e.identity.UserID,
				"session_id", e.identity.SessionID,
				"run_id", e.runID,
				"paused_at", e.pausedAt,
				"max_park_duration", c.maxPark)
		case errors.Is(err, ErrAlreadyResumed), errors.Is(err, ErrPauseNotFound):
			// Benign race: a legitimate Resume won between the snapshot
			// and this call. The pause resolved exactly once — the
			// loser's error is the documented contract (plan §"Test
			// plan"), not a failure.
		default:
			// A substantive reap failure. Loud, then continue. Two
			// classes, both retried by the next pass: a PRE-flip
			// failure leaves the entry paused and the expired scan
			// re-selects it (timeout resumes skip the tool-context
			// re-attach — so a lost handle is no longer in
			// this class); a POST-flip failure (checkpoint-delete
			// store error after Resume already flipped the entry) is
			// marked delete-pending by Resume itself and re-attempted
			// by retryPendingDeletes below — the expired scan alone
			// could never see it again (it skips non-paused entries).
			logger.ErrorContext(ctx, "pauseresume: sweeper failed to reap expired pause",
				"token", string(e.token),
				"tenant_id", e.identity.TenantID,
				"user_id", e.identity.UserID,
				"session_id", e.identity.SessionID,
				"run_id", e.runID,
				"error", err)
		}
	}
	if err := c.retryPendingDeletes(ctx, logger); err != nil {
		return reaped, err
	}
	return reaped, nil
}

// rescanCrashOrphans rescues checkpoint rows that have no live pause
// record into the registry (the recorded V1 boundary, closed
// by). A pause checkpointed by a process that then CRASHED is
// invisible to the registry scan: rehydrate-on-demand (Status /
// Resume) recovers it only if someone asks by Token, so an unasked-for
// checkpoint leaked forever. The rescan walks the store's
// `pauseresume.checkpoint:` kinds via the StateStore's maintenance
// scan (state.StateStore.ListKind — the RFC §6.11 amendment;
// MaintenanceScoped is the explicit elevation claim, and every
// MUTATION still goes through the public Resume under the pause's own
// identity, exactly like the registry scan above) and installs every
// orphan as a registry entry with the expiry re-stamped from THIS
// Coordinator's maxPark — so the same pass's expired scan reaps it
// when its deadline has already passed, and a not-yet-expired orphan
// becomes legitimately resumable until then.
//
// Per-record failures (a corrupt checkpoint, an unsupported format
// version) are logged loud and skipped — they stay in the store for
// the operator rather than being silently deleted. A scan failure is
// logged loud and the pass continues against the live registry. The
// only error return is ctx cancellation.
func (c *coordinator) rescanCrashOrphans(ctx context.Context, logger *slog.Logger) error {
	if c.store == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pauseresume: orphan rescan cancelled: %w", err)
	}
	recs, err := c.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, checkpointKindPrefix)
	if err != nil {
		// Loud, then continue the pass: a wedged store must not shield
		// the live registry's expired pauses from being reaped.
		logger.ErrorContext(ctx, "pauseresume: sweeper checkpoint rescan failed", "error", err)
		return nil
	}
	for _, sr := range recs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pauseresume: orphan rescan cancelled mid-pass: %w", err)
		}
		token := Token(strings.TrimPrefix(sr.Kind, checkpointKindPrefix))

		c.mu.Lock()
		_, live := c.pauses[token]
		c.mu.Unlock()
		if live {
			continue // not an orphan — the registry already tracks it.
		}

		rec, err := DeserializeRecord(sr.Bytes)
		if err == nil && rec.Token != token {
			err = fmt.Errorf("%w: checkpoint kind names token %q but envelope carries %q", ErrCheckpointCorrupt, token, rec.Token)
		}
		var entry *pauseEntry
		if err == nil {
			entry, err = entryFromCheckpoint(rec)
		}
		if err != nil {
			// Corrupt / unreadable checkpoint: loud, skipped, left in
			// the store for the operator — never silently deleted.
			logger.ErrorContext(ctx, "pauseresume: sweeper found unreadable orphaned checkpoint",
				"token", string(token), "error", err)
			continue
		}
		// Re-stamp the expiry from THIS Coordinator's knob — same
		// discipline as rehydrate (the deadline is derived, never
		// persisted).
		if c.maxPark > 0 && entry.state == StatusPaused {
			entry.expiresAt = entry.pausedAt.Add(c.maxPark)
		}

		c.mu.Lock()
		if _, raced := c.pauses[token]; !raced {
			c.pauses[token] = entry
		}
		c.mu.Unlock()
		logger.InfoContext(ctx, "pauseresume: sweeper rescued crash-orphaned pause checkpoint",
			"token", string(token),
			"reason", string(entry.reason),
			"tenant_id", entry.identity.TenantID,
			"user_id", entry.identity.UserID,
			"session_id", entry.identity.SessionID,
			"run_id", entry.runID,
			"paused_at", entry.pausedAt,
			"expires_at", entry.expiresAt)
	}
	return nil
}

// retryPendingDeletes re-attempts the checkpoint delete (and the
// skipped pause.resumed emit) for every resumed entry whose original
// delete failed (checkpoint audit — the orphan class Resume's
// state-flip-before-delete ordering creates). Runs on every sweep
// pass, not only when something expired, so a wedged store that
// recovers drains its backlog on the next tick. The only error return
// is ctx cancellation; per-record failures stay flagged, are logged
// loud, and are retried next pass.
func (c *coordinator) retryPendingDeletes(ctx context.Context, logger *slog.Logger) error {
	if c.store == nil {
		return nil
	}
	for _, e := range c.pendingDeleteEntries() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pauseresume: delete-retry cancelled mid-pass: %w", err)
		}
		rec, err := e.toCheckpoint()
		if err == nil {
			err = deleteCheckpoint(ctx, c.store, rec)
		}
		if err != nil {
			logger.ErrorContext(ctx, "pauseresume: sweeper failed to clear resumed pause's checkpoint (will retry next pass)",
				"token", string(e.token),
				"tenant_id", e.identity.TenantID,
				"user_id", e.identity.UserID,
				"session_id", e.identity.SessionID,
				"run_id", e.runID,
				"error", err)
			continue
		}
		c.clearDeletePending(e.token)
		// Complete the choreography: the original Resume returned
		// before its pause.resumed emit, so observers (including a
		// parked RunLoop waiting on the bus wake) never saw the
		// terminal event. Emit it now that the record is fully cleaned.
		c.emit(ctx, EventTypePauseResumed, &e, PauseResumedPayload{
			Token:    string(e.token),
			Reason:   string(e.reason),
			Decision: e.decision,
		})
		logger.InfoContext(ctx, "pauseresume: sweeper cleared resumed pause's orphaned checkpoint",
			"token", string(e.token),
			"tenant_id", e.identity.TenantID,
			"user_id", e.identity.UserID,
			"session_id", e.identity.SessionID,
			"run_id", e.runID)
	}
	return nil
}

// pendingDeleteEntries snapshots (value copies, under the mutex)
// every resumed registry entry still awaiting its checkpoint delete.
func (c *coordinator) pendingDeleteEntries() []pauseEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []pauseEntry
	for _, e := range c.pauses {
		if e.state == StatusResumed && e.deletePending {
			out = append(out, *e)
		}
	}
	return out
}

// clearDeletePending unflags an entry after its retried checkpoint
// delete landed.
func (c *coordinator) clearDeletePending(token Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.pauses[token]; ok {
		entry.deletePending = false
	}
}

// expiredEntries snapshots (value copies, under the mutex — the same
// discipline as List's snapshotEntries) every registry entry that is
// still paused and past its max-park deadline at now. Entries with a
// zero expiresAt never expire.
func (c *coordinator) expiredEntries(now time.Time) []pauseEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []pauseEntry
	for _, e := range c.pauses {
		if e.state != StatusPaused || e.expiresAt.IsZero() {
			continue
		}
		if now.Before(e.expiresAt) {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// sweeperResumeContext scopes ctx to the expired pause's OWN identity
// so Coordinator.Resume's scope check (sameScope) sees the triple the
// pause was recorded under. The sweeper never widens an identity
// boundary: each reap is performed AS the pause's own scope, one pause
// at a time (CLAUDE.md §6; plan §"Risks" — not a storage-filter
// bypass).
func sweeperResumeContext(ctx context.Context, e pauseEntry) (context.Context, error) {
	if e.runID != "" {
		return identity.WithRun(ctx, e.identity, e.runID)
	}
	return identity.With(ctx, e.identity)
}
