package steering

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// EventTypeRunLimitReached — emitted once when a run reaches a configured
// run limit (maximum active duration or maximum cumulative tokens). The run
// then asks the planner for one bounded final answer without tools and
// completes with finish reason limit_reached. Payload is
// RunLimitReachedPayload (metadata only).
const EventTypeRunLimitReached events.EventType = "run.limit_reached"

func init() {
	events.RegisterEventType(EventTypeRunLimitReached)
}

// RunLimitReachedPayload is the typed payload for run.limit_reached.
// SafePayload by construction: the limit name and bounded counters only.
type RunLimitReachedPayload struct {
	events.SafeSealed
	// Limit is "max_duration" or "max_tokens".
	Limit string
	// MaxDurationMS is the configured active-duration limit; zero when unset.
	MaxDurationMS int64
	// ElapsedMS is the run's active duration when the limit was reached.
	ElapsedMS int64
	// MaxTokens is the configured cumulative token limit; zero when unset.
	MaxTokens int64
	// TokensUsed is the cumulative prompt plus completion tokens so far.
	TokensUsed int64
	// Steps is the number of trajectory steps recorded so far.
	Steps int
}

const (
	// limitFinalOutputTokens bounds the graceful final answer.
	limitFinalOutputTokens = 2048
	limitFinalInstructions = "This request has reached its %s limit (%s), so it stops now. " +
		"Do not call any tools. Write the final reply to the user in the language of the conversation: " +
		"briefly say the limit was reached, summarise what was done and found so far, " +
		"and list concretely what remains, so the user can ask to continue."
)

// runLimitState is per-run, stack-local limit bookkeeping.
type runLimitState struct {
	limits      planner.RunLimits
	startedAt   time.Time
	paused      time.Duration
	reached     string
	reachedAt   time.Duration
	tokensAtHit int64
}

func (s *runLimitState) active(now time.Time) time.Duration {
	return max(now.Sub(s.startedAt)-s.paused, 0)
}

func describeLimit(limit string, limits planner.RunLimits) string {
	if limit == planner.RunLimitMaxTokens {
		return fmt.Sprintf("%d tokens", limits.MaxTokens)
	}
	return limits.MaxDuration.String()
}

func limitName(limit string) string {
	if limit == planner.RunLimitMaxTokens {
		return "maximum tokens"
	}
	return "maximum duration"
}

func (rl *RunLoop) emitRunLimitReached(ctx context.Context, q identity.Quadruple, s *runLimitState, steps int) {
	rl.logger.InfoContext(ctx, "steering: run limit reached; finishing with a bounded final answer",
		slog.String("run_id", q.RunID), slog.String("limit", s.reached),
		slog.Int64("elapsed_ms", s.reachedAt.Milliseconds()), slog.Int64("tokens_used", s.tokensAtHit))
	if rl.bus == nil {
		return
	}
	ev := events.Event{
		Type: EventTypeRunLimitReached, Identity: q, OccurredAt: rl.clock.Now(),
		Payload: RunLimitReachedPayload{
			Limit: s.reached, MaxDurationMS: s.limits.MaxDuration.Milliseconds(), ElapsedMS: s.reachedAt.Milliseconds(),
			MaxTokens: s.limits.MaxTokens, TokensUsed: s.tokensAtHit, Steps: steps,
		},
	}
	if err := rl.bus.Publish(ctx, ev); err != nil {
		rl.logger.WarnContext(ctx, "steering: run.limit_reached publish failed",
			slog.String("run_id", q.RunID), slog.Any("error", err))
	}
}

// limitFinish stamps the terminal limit_reached outcome on the planner's
// final answer, or builds a deterministic partial answer when the planner
// could not provide one.
func limitFinish(q identity.Quadruple, s *runLimitState, decision planner.Decision, tr *planner.Trajectory) planner.Finish {
	answer, via := "", "runtime"
	if fin, ok := decision.(planner.Finish); ok {
		if text, isText := fin.Payload.(string); isText && strings.TrimSpace(text) != "" {
			answer, via = text, "planner"
		}
	}
	if answer == "" {
		answer = deterministicLimitAnswer(s, tr)
	}
	return planner.Finish{
		Reason:  planner.FinishLimitReached,
		Payload: answer,
		Metadata: map[string]any{
			"run_id":          q.RunID,
			"limit":           s.reached,
			"max_duration_ms": s.limits.MaxDuration.Milliseconds(),
			"elapsed_ms":      s.reachedAt.Milliseconds(),
			"max_tokens":      s.limits.MaxTokens,
			"tokens_used":     s.tokensAtHit,
			"via":             via,
		},
	}
}

// deterministicLimitAnswer states what ran and that work remains, without a
// model call.
func deterministicLimitAnswer(s *runLimitState, tr *planner.Trajectory) string {
	counts := map[string]int{}
	steps := 0
	if tr != nil {
		steps = len(tr.Steps)
		for _, step := range tr.Steps {
			switch a := step.Action.(type) {
			case planner.CallTool:
				counts[a.Tool]++
			case planner.CallParallel:
				for _, branch := range a.Branches {
					counts[branch.Tool]++
				}
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This request stopped because it reached its %s limit (%s) after %d steps.", limitName(s.reached), describeLimit(s.reached, s.limits), steps)
	if len(counts) > 0 {
		names := make([]string, 0, len(counts))
		for name := range counts {
			names = append(names, name)
		}
		sort.Strings(names)
		b.WriteString(" Tools used:")
		for i, name := range names {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, " %s (%d)", name, counts[name])
		}
		b.WriteString(".")
	}
	b.WriteString(" The work is not finished; ask to continue and it will resume from here.")
	return b.String()
}
