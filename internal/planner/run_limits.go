package planner

import (
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// Run limit names, carried on FinalizeRequest.Limit, the run.limit_reached
// event and the terminal Finish metadata.
const (
	RunLimitMaxDuration = "max_duration"
	RunLimitMaxTokens   = "max_tokens"
)

// MaxRunLimitDuration bounds a configured maximum duration.
const MaxRunLimitDuration = config.MaxRunLimitDuration

// RunLimits bounds one run. A zero field means no limit for that dimension;
// either, both or neither may be set. MaxDuration is active wall-clock time,
// excluding time spent waiting on a pause. MaxTokens is the cumulative prompt
// plus completion tokens of every model call the run makes, including context
// maintenance. Limits are checked at each step boundary, so a single in-flight
// model or tool call is never interrupted; the graceful final answer is one
// further bounded call.
type RunLimits struct {
	MaxDuration time.Duration
	MaxTokens   int64
}

// IsZero reports that no limit is set.
func (l RunLimits) IsZero() bool { return l.MaxDuration <= 0 && l.MaxTokens <= 0 }

// Validate rejects negative or out-of-range limits.
func (l RunLimits) Validate() error {
	if l.MaxDuration < 0 || l.MaxDuration > MaxRunLimitDuration {
		return fmt.Errorf("run limits: max_duration must be between 0 and %s", MaxRunLimitDuration)
	}
	if l.MaxTokens < 0 {
		return fmt.Errorf("run limits: max_tokens must not be negative")
	}
	return nil
}

// Stricter narrows l by a per-run request: for each dimension the smaller
// positive value wins, so a request can tighten a limit but never loosen or
// remove one.
func (l RunLimits) Stricter(request RunLimits) RunLimits {
	out := l
	if request.MaxDuration > 0 && (out.MaxDuration <= 0 || request.MaxDuration < out.MaxDuration) {
		out.MaxDuration = request.MaxDuration
	}
	if request.MaxTokens > 0 && (out.MaxTokens <= 0 || request.MaxTokens < out.MaxTokens) {
		out.MaxTokens = request.MaxTokens
	}
	return out
}

// Reached reports which limit, if any, the run has reached.
func (l RunLimits) Reached(active time.Duration, tokens int64) string {
	switch {
	case l.MaxDuration > 0 && active >= l.MaxDuration:
		return RunLimitMaxDuration
	case l.MaxTokens > 0 && tokens >= l.MaxTokens:
		return RunLimitMaxTokens
	}
	return ""
}
