package pauseresume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// This file ships the pause sweeper (Phase 111c / D-200) — the
// exported maintenance loop that gives the pause lifecycle an END.
// Before 111c, Resume was the ONLY checkpoint-deletion path: a
// cancel-while-paused run (or an operator who simply never answered an
// approval) orphaned its pause record + checkpoint forever. The
// sweeper is the backstop: it walks the Coordinator's registry and,
// for every pause past its max-park deadline (WithMaxParkDuration),
// calls Coordinator.Resume with DecisionTimeout — that enum value's
// FIRST producer (RFC §3.3 / D-096; the "Phase 50 does not yet emit
// this" note in decision.go is closed by this file).
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
// resolution (D-200, plan §"Ship-time deviations") is: the sweeper
// lives in THIS package and snapshots the registry directly
// (value-copies under the mutex, same shape List itself uses), while
// every MUTATION goes through the public Coordinator.Resume under the
// pause's OWN identity triple — the scope check, handle re-attach,
// checkpoint delete, and pause.resumed emit all run unmodified. No
// storage-level identity filter is bypassed and no elevated List
// shape is minted.
//
// # Timeout is terminal
//
// A timed-out pause is resumed with DecisionTimeout and is TERMINAL
// for the waiting run: the steering RunLoop observes the timeout and
// finishes the run with Finish{ConstraintsConflict} — a deadline the
// human missed is a constraint the planner cannot resolve (mirrors
// D-071's REJECT posture). Never a silent unpark-and-continue.

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
// ONE exported entry into the pause-lifecycle maintenance loop (Phase
// 111c / D-200): on every tick it scans the Coordinator's pause
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
// Per-record reap failures (a lost tool-context handle, a store error
// on the checkpoint delete) are logged at Error and do NOT stop the
// loop: one wedged record must not shield every other expired pause
// from being reaped. Losing a reap race to a legitimate concurrent
// Resume (ErrAlreadyResumed / ErrPauseNotFound) is benign — the pause
// resolved exactly once — and is not logged as a failure.
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

// sweepOnce performs one sweep pass: snapshot the expired pauses at
// the Coordinator's clock, then Resume each with DecisionTimeout under
// the pause's own identity. Returns the count of successfully reaped
// pauses. The only error return is ctx cancellation mid-pass;
// per-record failures are logged loud (Error level, with the
// identity-attributed fields CLAUDE.md §5 requires) and skipped so one
// wedged record cannot block the rest of the pass.
func sweepOnce(ctx context.Context, c *coordinator, logger *slog.Logger) (int, error) {
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
			// plan" / D-200), not a failure.
		default:
			// A substantive reap failure (lost tool-context handle,
			// checkpoint-delete store error). Loud, then continue — the
			// record stays and the next pass retries it.
			logger.ErrorContext(ctx, "pauseresume: sweeper failed to reap expired pause",
				"token", string(e.token),
				"tenant_id", e.identity.TenantID,
				"user_id", e.identity.UserID,
				"session_id", e.identity.SessionID,
				"run_id", e.runID,
				"error", err)
		}
	}
	return reaped, nil
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
