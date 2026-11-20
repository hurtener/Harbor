package pauseresume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hurtener/Harbor/internal/state"
)

// ResolveInterrupted is the boot-time pass that closes pauses a runtime
// restart left without a waiter. Some pauses can only be continued by
// the process that parked them: an approval-gated tool call keeps its
// original arguments in memory and blocks on an in-process channel, and
// the steering run loop that would route the decision is gone with the
// process. Their checkpoints outlive the restart, so without this pass
// they would look actionable in pause.list while every approve returns
// not-found and the run's task is already failed by task recovery.
//
// The pass scans the checkpoint store (the StateStore maintenance scan
// the sweeper uses) and resolves every still-paused checkpoint that
//
//   - carries no trajectory and no continuation (nothing durable could
//     continue it), and
//   - satisfies match (the owning subsystem names its process-bound
//     pauses),
//
// with DecisionInterrupted under the pause's own identity, through the
// public Resume path: the checkpoint is deleted and pause.resumed is
// emitted with the typed marker. Pauses already tracked by this
// process's registry are skipped. The resolved pauses are returned so
// the owning subsystem can emit its cause-specific companion event.
//
// Call it once at boot, before the runtime serves requests. It assumes
// the store is not shared with another live runtime process (the same
// single-writer assumption the task-recovery sweep makes at open).
//
// A Coordinator without a checkpoint store resolves nothing. A store
// scan failure returns the error; a per-record failure (unreadable
// checkpoint, resume error) is logged and skipped so one bad record
// does not hide the rest.
func ResolveInterrupted(ctx context.Context, coord Coordinator, match func(Pause) bool, logger *slog.Logger) ([]Pause, error) {
	c, ok := coord.(*coordinator)
	if !ok {
		return nil, fmt.Errorf("%w: ResolveInterrupted requires the pauseresume.New Coordinator (got %T)", ErrSweeperMisconfigured, coord)
	}
	if c.store == nil || match == nil {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	recs, err := c.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, checkpointKindPrefix)
	if err != nil {
		return nil, fmt.Errorf("pauseresume: interrupted-pause scan: %w", err)
	}
	var resolved []Pause
	for _, sr := range recs {
		if err := ctx.Err(); err != nil {
			return resolved, fmt.Errorf("pauseresume: interrupted-pause scan cancelled: %w", err)
		}
		token := Token(strings.TrimPrefix(sr.Kind, checkpointKindPrefix))
		c.mu.Lock()
		_, live := c.pauses[token]
		c.mu.Unlock()
		if live {
			continue
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
			logger.ErrorContext(ctx, "pauseresume: unreadable checkpoint skipped by interrupted-pause pass",
				"token", string(token), "error", err)
			continue
		}
		if entry.state != StatusPaused || entry.trajectory != nil || entry.continuation != nil {
			continue
		}
		p := entry.pause()
		if !match(p) {
			continue
		}
		c.mu.Lock()
		if _, raced := c.pauses[token]; !raced {
			c.pauses[token] = entry
		}
		c.mu.Unlock()
		resumeCtx, err := sweeperResumeContext(ctx, *entry)
		if err != nil {
			logger.ErrorContext(ctx, "pauseresume: interrupted pause has no valid identity", "token", string(token), "error", err)
			continue
		}
		switch err := c.Resume(resumeCtx, token, DecisionInterrupted, map[string]any{"interrupted": true}); {
		case err == nil:
			resolved = append(resolved, p)
			logger.InfoContext(ctx, "pauseresume: resolved pause interrupted by a runtime restart",
				"token", string(token),
				"reason", string(entry.reason),
				"tenant_id", entry.identity.TenantID,
				"user_id", entry.identity.UserID,
				"session_id", entry.identity.SessionID,
				"run_id", entry.runID)
		case errors.Is(err, ErrAlreadyResumed), errors.Is(err, ErrPauseNotFound):
			// Resolved concurrently; it resolved exactly once.
		default:
			logger.ErrorContext(ctx, "pauseresume: failed to resolve interrupted pause",
				"token", string(token), "error", err)
		}
	}
	return resolved, nil
}
